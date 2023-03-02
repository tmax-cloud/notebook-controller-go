/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/utils/pointer"
	reconcilehelper "github.com/tmax-cloud/notebook-controller-go/pkg/reconcilehelper"
	"github.com/tmax-cloud/notebook-controller-go/api/v1"	
	"github.com/tmax-cloud/notebook-controller-go/pkg/culler"
	"github.com/tmax-cloud/notebook-controller-go/pkg/metrics"
	"k8s.io/apimachinery/pkg/api/resource"
	netv1 "k8s.io/api/networking/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const DefaultContainerPort = 8888
const DefaultServingPort = 80
const HttpsServingPort = 443
const AnnotationRewriteURI = "notebooks.kubeflow.org/http-rewrite-uri"
const AnnotationHeadersRequestSet = "notebooks.kubeflow.org/http-headers-request-set"

const PrefixEnvVar = "NB_PREFIX"

// The default fsGroup of PodSecurityContext.
// https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.11/#podsecuritycontext-v1-core
const DefaultFSGroup = int64(100)

/*
We generally want to ignore (not requeue) NotFound errors, since we'll get a
reconciliation request once the object exists, and requeuing in the meantime
won't help.
*/
func ignoreNotFound(err error) error {
	if apierrs.IsNotFound(err) {
		return nil
	}
	return err
}

// NotebookReconciler reconciles a Notebook object
type NotebookReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	Metrics       *metrics.Metrics
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=services,verbs="*"
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs="*"
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks;notebooks/status;notebooks/finalizers,verbs="*"
// +kubebuilder:rbac:groups="networking.istio.io",resources=virtualservices,verbs="*"

func (r *NotebookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("notebook", req.NamespacedName)

	// TODO(yanniszark): Can we avoid reconciling Events and Notebook in the same queue?
	event := &corev1.Event{}
	var getEventErr error
	getEventErr = r.Get(ctx, req.NamespacedName, event)
	if getEventErr == nil {
		log.Info("Found event for Notebook. Re-emitting...")

		// Find the Notebook that corresponds to the triggered event
		involvedNotebook := &v1.Notebook{}
		nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
		if err != nil {
			return ctrl.Result{}, err
		}

		involvedNotebookKey := types.NamespacedName{Name: nbName, Namespace: req.Namespace}
		if err := r.Get(ctx, involvedNotebookKey, involvedNotebook); err != nil {
			log.Error(err, "unable to fetch Notebook by looking at event")
			return ctrl.Result{}, ignoreNotFound(err)
		}

		// re-emit the event in the Notebook CR
		log.Info("Emitting Notebook Event.", "Event", event)
		r.EventRecorder.Eventf(involvedNotebook, event.Type, event.Reason,
			"Reissued from %s/%s: %s", strings.ToLower(event.InvolvedObject.Kind), event.InvolvedObject.Name, event.Message)
		return ctrl.Result{}, nil
	}

	if getEventErr != nil && !apierrs.IsNotFound(getEventErr) {
		return ctrl.Result{}, getEventErr
	}
	// If not found, continue. Is not an event.

	instance := &v1.Notebook{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		log.Error(err, "unable to fetch Notebook")
		return ctrl.Result{}, ignoreNotFound(err)
	}

	pvc := generatePersistentVolumeClaim(instance)

	// Check if the PersistentVolumeClaim already exists
	foundPvc := &corev1.PersistentVolumeClaim{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, foundPvc)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating PersistentVolumeClaim", "namespace", pvc.Namespace, "name", pvc.Name)
		err = r.Create(ctx, pvc)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create PersistentVolumeClaim")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting PersistentVolumeClaim")
		return ctrl.Result{}, err
	}

	// Reconcile StatefulSet
	ss := generateStatefulSet(instance)
	if err := ctrl.SetControllerReference(instance, ss, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Check if the StatefulSet already exists
	foundStateful := &appsv1.StatefulSet{}
	justCreated = false
	err = r.Get(ctx, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}, foundStateful)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		r.Metrics.NotebookCreation.WithLabelValues(ss.Namespace).Inc()
		err = r.Create(ctx, ss)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Statefulset")
			r.Metrics.NotebookFailCreation.WithLabelValues(ss.Namespace).Inc()
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting Statefulset")
		return ctrl.Result{}, err
	}
	// Update the foundStateful object and write the result back if there are any changes
	if !justCreated && reconcilehelper.CopyStatefulSetFields(ss, foundStateful) {
		log.Info("Updating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		err = r.Update(ctx, foundStateful)
		if err != nil {
			log.Error(err, "unable to update Statefulset")
			return ctrl.Result{}, err
		}
	}

	// Reconcile service
	service := generateService(instance)
	if err := ctrl.SetControllerReference(instance, service, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Check if the Service already exists
	foundService := &corev1.Service{}
	justCreated = false
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Service", "namespace", service.Namespace, "name", service.Name)
		err = r.Create(ctx, service)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Service")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting Statefulset")
		return ctrl.Result{}, err
	}
	// Update the foundService object and write the result back if there are any changes
	if !justCreated && reconcilehelper.CopyServiceFields(service, foundService) {
		log.Info("Updating Service\n", "namespace", service.Namespace, "name", service.Name)
		err = r.Update(ctx, foundService)
		if err != nil {
			log.Error(err, "unable to update Service")
			return ctrl.Result{}, err
		}
	}

	// Reconcile Ingress.
	err = r.reconcileIngress(instance)
		if err != nil {
			return ctrl.Result{}, err
		}

	// Reconcile Certificate.
	err = r.reconcileCertificate(instance)
	if err != nil {
		return ctrl.Result{}, err
	}	

	// Reconcile virtual service if we use ISTIO.
	if os.Getenv("USE_ISTIO") == "true" {
		err = r.reconcileVirtualService(instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Update the readyReplicas if the status is changed
	if foundStateful.Status.ReadyReplicas != instance.Status.ReadyReplicas {
		log.Info("Updating Status", "namespace", instance.Namespace, "name", instance.Name)
		instance.Status.ReadyReplicas = foundStateful.Status.ReadyReplicas
		err = r.Status().Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check the pod status
	pod := &corev1.Pod{}
	podFound := false
	err = r.Get(ctx, types.NamespacedName{Name: ss.Name + "-0", Namespace: ss.Namespace}, pod)
	if err != nil && apierrs.IsNotFound(err) {
		// This should be reconciled by the StatefulSet
		log.Info("Pod not found...")
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		// Got the pod
		podFound = true

		if len(pod.Status.ContainerStatuses) > 0 &&
			pod.Status.ContainerStatuses[0].State != instance.Status.ContainerState {
			log.Info("Updating container state: ", "namespace", instance.Namespace, "name", instance.Name)
			cs := pod.Status.ContainerStatuses[0].State
			instance.Status.ContainerState = cs
			oldConditions := instance.Status.Conditions
			newCondition := getNextCondition(cs)
			// Append new condition
			if len(oldConditions) == 0 || oldConditions[0].Type != newCondition.Type ||
				oldConditions[0].Reason != newCondition.Reason ||
				oldConditions[0].Message != newCondition.Message {
				log.Info("Appending to conditions: ", "namespace", instance.Namespace, "name", instance.Name, "type", newCondition.Type, "reason", newCondition.Reason, "message", newCondition.Message)
				instance.Status.Conditions = append([]v1.NotebookCondition{newCondition}, oldConditions...)

			}
			err = r.Status().Update(ctx, instance)
			if err != nil {
				return ctrl.Result{}, err			
			}
		}
	}

	if !podFound {
		// Delete LAST_ACTIVITY_ANNOTATION annotations for CR objects
		// that do not have a pod.
		log.Info("Notebook has not Pod running. Will remove last-activity annotation")
		meta := instance.ObjectMeta
		if meta.GetAnnotations() == nil {
			log.Info("No annotations found")
			return ctrl.Result{}, nil
		}

		if _, ok := meta.GetAnnotations()[culler.LAST_ACTIVITY_ANNOTATION]; !ok {
			log.Info("No last-activity annotations found")
			return ctrl.Result{}, nil
		}

		log.Info("Removing last-activity annotation")
		delete(meta.GetAnnotations(), culler.LAST_ACTIVITY_ANNOTATION)
		err = r.Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	}

	// Pod is found
	// Check if the Notebook needs to be stopped
	// Update the LAST_ACTIVIiANNOTATION
	

	// Check if the Notebook needs to be stopped
	if culler.NotebookNeedsCulling(instance.ObjectMeta) {
		log.Info(fmt.Sprintf(
			"Notebook %s/%s needs culling. Setting annotations",
			instance.Namespace, instance.Name))

		// Set annotations to the Notebook
		culler.SetStopAnnotation(&instance.ObjectMeta, r.Metrics)
		r.Metrics.NotebookCullingCount.WithLabelValues(instance.Namespace, instance.Name).Inc()
		err = r.Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if !culler.StopAnnotationIsSet(instance.ObjectMeta) {
		// The Pod is either too fresh, or the idle time has passed and it has
		// received traffic. In this case we will be periodically checking if
		// it needs culling.
		return ctrl.Result{RequeueAfter: culler.GetRequeueTime()}, nil
	}
	return ctrl.Result{RequeueAfter: culler.GetRequeueTime()}, nil
}

func getNextCondition(cs corev1.ContainerState) v1.NotebookCondition {
	var nbtype = ""
	var nbreason = ""
	var nbmsg = ""

	if cs.Running != nil {
		nbtype = "Running"
	} else if cs.Waiting != nil {
		nbtype = "Waiting"
		nbreason = cs.Waiting.Reason
		nbmsg = cs.Waiting.Message
	} else {
		nbtype = "Terminated"
		nbreason = cs.Terminated.Reason
		nbmsg = cs.Terminated.Reason
	}

	newCondition := v1.NotebookCondition{
		Type:          nbtype,
		LastProbeTime: metav1.Now(),
		Reason:        nbreason,
		Message:       nbmsg,
	}
	return newCondition
}

func setPrefixEnvVar(instance *v1.Notebook, container *corev1.Container) {
	prefix := "/notebook/" + instance.Namespace + "/" + instance.Name

	for _, envVar := range container.Env {
		if envVar.Name == PrefixEnvVar {
			envVar.Value = prefix
			return
		}
	}

	container.Env = append(container.Env, corev1.EnvVar{
		Name:  PrefixEnvVar,
		Value: prefix,
	})
}

func generatePersistentVolumeClaim(instance *v1.Notebook) *corev1.PersistentVolumeClaim {
	storageclass := instance.Spec.VolumeClaim[0].StorageClass
	pvc := &corev1.PersistentVolumeClaim{}

	if storageclass != "" {
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Spec.VolumeClaim[0].Name,
				Namespace: instance.Namespace,
				Labels: map[string]string{
					"notebook": instance.Name,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceStorage): resource.MustParse(instance.Spec.VolumeClaim[0].Size),
					},
				},
				StorageClassName: &storageclass,
			},
		}
	} else {
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Spec.VolumeClaim[0].Name,
				Namespace: instance.Namespace,
				Labels: map[string]string{
					"notebook": instance.Name,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceStorage): resource.MustParse(instance.Spec.VolumeClaim[0].Size),
					},
				},
			},
		}
	}

	return pvc
}

func generateStatefulSet(instance *v1.Notebook) *appsv1.StatefulSet {
	replicas := int32(1)
	if culler.StopAnnotationIsSet(instance.ObjectMeta) {
		replicas = 0
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"statefulset": instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					"sidecar.istio.io/inject": "false",
				},
				Labels: map[string]string{
					"statefulset":   instance.Name,
					"notebook-name": instance.Name,
				}},
				Spec: *instance.Spec.Template.Spec.DeepCopy(),
			},
		},
	}
	// copy all of the Notebook labels to the pod including poddefault related labels
	l := &ss.Spec.Template.ObjectMeta.Labels
	for k, v := range instance.ObjectMeta.Labels {
		(*l)[k] = v
	}

	podSpec := &ss.Spec.Template.Spec
	container := &podSpec.Containers[0]
	if container.WorkingDir == "" {
		container.WorkingDir = "/home/jovyan"
	}
	if container.Ports == nil {
		container.Ports = []corev1.ContainerPort{
			{
				ContainerPort: DefaultContainerPort,
				Name:          "notebook-port",
				Protocol:      "TCP",
			},
		}
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: "secret",
		MountPath: "/usr/local/share/ca-certificates",
	})	
	
	if container.Args == nil {
		container.Args = []string{"sh","-c", "update-ca-certificates && jupyter lab --notebook-dir=/home/${NB_USER} --ip=0.0.0.0 --no-browser --allow-root --port=8888 --NotebookApp.token='' --NotebookApp.password='' --NotebookApp.allow_origin='*' --NotebookApp.base_url=${NB_PREFIX}"}
	}

	
	
	/*
	if container.Command == nil {
		container.Command = []string{"sh","-c", "sudo", "update-ca-certificates"}
	}	
	
		
	
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: "bins",
		MountPath: "/home/jovyan/bin",
	})		
*/
	clientsecret := os.Getenv("CLIENT_SECRET")
	discoveryurl := os.Getenv("DISCOVERY_URL")
	gatekeeperVersion := os.Getenv("GATEKEEPER_VERSION")		
	logLevel := os.Getenv("LOG_LEVEL")
	isClosed := os.Getenv("IS_CLOSED")
	registryName := os.Getenv("REGISTRY_NAME")
	
	imageOpened := "docker.io/tmaxcloudck/gatekeeper:" + gatekeeperVersion
	imageClosed := registryName + "docker.io/tmaxcloudck/gatekeeper:" + gatekeeperVersion
	
	
	if isClosed == "true" {
		podSpec.Containers = append(podSpec.Containers, corev1.Container{
			Name:  "gatekeeper",		
			Image: imageClosed,
			Args: []string{
				"--client-id=notebook-gatekeeper",
				"--client-secret=" + clientsecret,
				"--listen=:3000",
				"--upstream-url=http://127.0.0.1:8888",
				"--discovery-url=" + discoveryurl,
				"--secure-cookie=false",
				"--upstream-keepalives=false",
				"--skip-openid-provider-tls-verify=true",
				"--skip-upstream-tls-verify=true",
				"--tls-cert=/etc/secrets/tls.crt",
				"--tls-private-key=/etc/secrets/tls.key",
				"--tls-ca-certificate=/etc/secrets/ca.crt",
				"--enable-self-signed-tls=false",
				"--enable-refresh-tokens=true",
				"--enable-default-deny=true",
				"--enable-metrics=true",
				"--encryption-key=AgXa7xRcoClDEU0ZDSH4X0XhL5Qy2Z2j",
				"--resources=uri=/*|roles=notebook-gatekeeper:notebook-gatekeeper-manager",
				"--log-level=" + logLevel,
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "service",
					ContainerPort: 3000,
				},
			},			
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "secret",
					MountPath: "/etc/secrets",
				},
			},
		})				
	} else {
		podSpec.Containers = append(podSpec.Containers, corev1.Container{
			Name:  "gatekeeper",		
			Image: imageOpened,
			Args: []string{
				"--client-id=notebook-gatekeeper",
				"--client-secret=" + clientsecret,
				"--listen=:3000",
				"--upstream-url=http://127.0.0.1:8888",
				"--discovery-url=" + discoveryurl,
				"--secure-cookie=false",
				"--upstream-keepalives=false",
				"--skip-openid-provider-tls-verify=true",
				"--skip-upstream-tls-verify=true",
				"--tls-cert=/etc/secrets/tls.crt",
				"--tls-private-key=/etc/secrets/tls.key",
				"--tls-ca-certificate=/etc/secrets/ca.crt",
				"--enable-self-signed-tls=false",
				"--enable-refresh-tokens=true",
				"--enable-default-deny=true",
				"--enable-metrics=true",
				"--encryption-key=AgXa7xRcoClDEU0ZDSH4X0XhL5Qy2Z2j",
				"--resources=uri=/*|roles=notebook-gatekeeper:notebook-gatekeeper-manager",
				"--log-level=" + logLevel,
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "service",
					ContainerPort: 3000,
				},
			},			
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "secret",
					MountPath: "/etc/secrets",
				},
			},
		})
	}

	

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: instance.Name + "-secret",
				DefaultMode: pointer.Int32(0777),
			},
		},
	})

/*	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "secret-self",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "selfsigned-ca",				
			},
		},
	})

/*	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "bins",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "bins",
				},
			},	
		},
	})*/

	setPrefixEnvVar(instance, container)

	// For some platforms (like OpenShift), adding fsGroup: 100 is troublesome.
	// This allows for those platforms to bypass the automatic addition of the fsGroup
	// and will allow for the Pod Security Policy controller to make an appropriate choice
	// https://github.com/kubernetes-sigs/controller-runtime/issues/4617
	if value, exists := os.LookupEnv("ADD_FSGROUP"); !exists || value == "true" {
		if podSpec.SecurityContext == nil {
			fsGroup := DefaultFSGroup
			podSpec.SecurityContext = &corev1.PodSecurityContext{
				FSGroup: &fsGroup,
			}
		}
	}
	return ss
}

func generateService(instance *v1.Notebook) *corev1.Service {
	// Define the desired Service object
//	port := DefaultContainerPort
/*	containerPorts := instance.Spec.Template.Spec.Containers[0].Ports
	if containerPorts != nil {
		port = int(containerPorts[0].ContainerPort)
	}*/
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Annotations: map[string]string{
				"traefik.ingress.kubernetes.io/service.serverstransport": "insecure@file",				
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     "ClusterIP",
			Selector: map[string]string{"statefulset": instance.Name},
			Ports: []corev1.ServicePort{
				{
					// Make port name follow Istio pattern so it can be managed by istio rbac
					Name:       "https-" + instance.Name,
					Port:       int32(HttpsServingPort),
					TargetPort: intstr.FromInt(3000),
					Protocol:   "TCP",
				},
			},
		},
	}
	return svc
}

func ingressName(kfName string, namespace string) string {
	return fmt.Sprintf("%s-%s", kfName, namespace)
}

func generateIngress(instance *v1.Notebook) (*netv1.Ingress, error) {
	name := instance.Name
	namespace := instance.Namespace
	var tls []netv1.IngressTLS
	var ingressclassname = new(string)
	*ingressclassname = "tmax-cloud"
/*	if redirect.Expose != nil && redirect.Expose.TLS.Enabled() {
		tls = []netv1.IngressTLS{{
			SecretName: redirect.Expose.TLS.CertificateRef,
			Hosts:      []string{redirect.Expose.Ingress.Host},
		}}
	}*/
	customDomain := os.Getenv("CUSTOM_DOMAIN")

	tls = []netv1.IngressTLS{{		
		Hosts:      []string{ingressName(name, namespace) + "." + customDomain},
	}}
	
	pathTypePrefix := netv1.PathTypePrefix
	
	ingress := &netv1.Ingress{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "networking.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressName(name, namespace),
			Namespace: namespace,
			Annotations: map[string]string{
				"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
				"cert-manager.io/cluster-issuer": "tmaxcloud-issuer",
			},
			Labels: map[string]string{
				"ingress.tmaxcloud.org/name":   ingressName(name, namespace),				
			},
		},
		Spec: netv1.IngressSpec{
			TLS:              tls,
			IngressClassName: ingressclassname,
			Rules: []netv1.IngressRule{
				{
					Host: ingressName(name, namespace) + "." + customDomain,
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathTypePrefix,
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: instance.Name,
											Port: netv1.ServiceBackendPort{
												Number: int32(443),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	return ingress, nil
}

func (r *NotebookReconciler) reconcileIngress(instance *v1.Notebook) error {	
	log := r.Log.WithValues("notebook", instance.Namespace)
	ingress, err := generateIngress(instance)
	if err := ctrl.SetControllerReference(instance, ingress, r.Scheme); err != nil {
		return err
	}
	// ingress 존재 체크
	foundIngress := &netv1.Ingress{}
	justCreated := false	
	err = r.Get(context.TODO(), types.NamespacedName{Name: ingressName(instance.Name,
		instance.Namespace), Namespace: instance.Namespace}, foundIngress)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Ingress", "namespace", ingress.Namespace, "name", ingressName(instance.Name, instance.Namespace))
		err = r.Create(context.TODO(), ingress)
		justCreated = true
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if !justCreated && reconcilehelper.CopyIngress(ingress, foundIngress) {
		log.Info("Updating Ingress\n", "namespace", ingress.Namespace, "name", ingressName(instance.Name, instance.Namespace))
		err = r.Update(context.TODO(), foundIngress)
		if err != nil {
			return err
		}
	}

	return nil
}

func certificateName(kfName string, namespace string) string {
	return fmt.Sprintf("cert-%s-%s", namespace, kfName)
}

func generateCertificate(instance *v1.Notebook) (*unstructured.Unstructured, error) {
	name := instance.Name
	namespace := instance.Namespace
	cert := &unstructured.Unstructured{}
	cert.SetAPIVersion("cert-manager.io/v1")
	cert.SetKind("Certificate")
	cert.SetName(certificateName(name, namespace))
	cert.SetNamespace(namespace)
	
	secretname := fmt.Sprintf("%s-secret", name)
	if err := unstructured.SetNestedField(cert.Object, secretname, "spec", "secretName"); err != nil {
		return nil, fmt.Errorf("Set .spec.secretName error: %v", err)
	}
	var isca bool = false
	if err := unstructured.SetNestedField(cert.Object, isca, "spec", "isCA"); err != nil {
		return nil, fmt.Errorf("Set .spec.isCA error: %v", err)
	}
	dnsnames := []string{
		"tmax-cloud",
	}
	if err := unstructured.SetNestedStringSlice(cert.Object, dnsnames, "spec", "dnsNames"); err != nil {
		return nil, fmt.Errorf("Set .spec.dnsNames error: %v", err)
	}
	keyusage := []string{
		"digital signature",
		"key encipherment",
		"server auth",
		"client auth",
	}
	if err := unstructured.SetNestedStringSlice(cert.Object, keyusage, "spec", "usages"); err != nil {
		return nil, fmt.Errorf("Set .spec.usages error: %v", err)
	}

	issuerref := map[string]string{
		"group": "cert-manager.io",
		"kind": "ClusterIssuer",
		"name": "tmaxcloud-issuer",
	}
	
	if err := unstructured.SetNestedStringMap(cert.Object, issuerref, "spec", "issuerRef"); err != nil {
		return nil, fmt.Errorf("Set .spec.issuerref error: %v", err)
	}	

	return cert, nil
}

func (r *NotebookReconciler) reconcileCertificate(instance *v1.Notebook) error {	
	log := r.Log.WithValues("notebook", instance.Namespace)
	certificate, err := generateCertificate(instance)
	if err := ctrl.SetControllerReference(instance, certificate, r.Scheme); err != nil {
		return err
	}
	// certificate 존재 체크
	foundCertificate := &unstructured.Unstructured{}
	justCreated := false
	foundCertificate.SetAPIVersion("cert-manager.io/v1")
	foundCertificate.SetKind("Certificate")	
	err = r.Get(context.TODO(), types.NamespacedName{Name: certificateName(instance.Name,
		instance.Namespace), Namespace: instance.Namespace}, foundCertificate)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Certificate", "namespace", instance.Namespace, "name", certificateName(instance.Name, instance.Namespace))
		err = r.Create(context.TODO(), certificate)
		justCreated = true
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if !justCreated && reconcilehelper.CopyCertificate(certificate, foundCertificate) {
		log.Info("Updating Certificate\n", "namespace", instance.Namespace, "name", certificateName(instance.Name, instance.Namespace))
		err = r.Update(context.TODO(), foundCertificate)
		if err != nil {
			return err
		}
	}

	return nil
}

func virtualServiceName(kfName string, namespace string) string {
	return fmt.Sprintf("notebook-%s-%s", namespace, kfName)
}

func generateVirtualService(instance *v1.Notebook) (*unstructured.Unstructured, error) {
	name := instance.Name
	namespace := instance.Namespace
	clusterDomain := "cluster.local"
	prefix := fmt.Sprintf("/notebook/%s/%s/", namespace, name)

	// unpack annotations from Notebook resource
	annotations := make(map[string]string)
	for k, v := range instance.ObjectMeta.Annotations {
		annotations[k] = v
	}

	rewrite := fmt.Sprintf("/notebook/%s/%s/", namespace, name)
	// If AnnotationRewriteURI is present, use this value for "rewrite"
	if _, ok := annotations[AnnotationRewriteURI]; ok && len(annotations[AnnotationRewriteURI]) > 0 {
		rewrite = annotations[AnnotationRewriteURI]
	}

	if clusterDomainFromEnv, ok := os.LookupEnv("CLUSTER_DOMAIN"); ok {
		clusterDomain = clusterDomainFromEnv
	}
	service := fmt.Sprintf("%s.%s.svc.%s", name, namespace, clusterDomain)

	vsvc := &unstructured.Unstructured{}
	vsvc.SetAPIVersion("networking.istio.io/v1alpha3")
	vsvc.SetKind("VirtualService")
	vsvc.SetName(virtualServiceName(name, namespace))
	vsvc.SetNamespace(namespace)
	if err := unstructured.SetNestedStringSlice(vsvc.Object, []string{"*"}, "spec", "hosts"); err != nil {
		return nil, fmt.Errorf("Set .spec.hosts error: %v", err)
	}

	istioGateway := os.Getenv("ISTIO_GATEWAY")
	if len(istioGateway) == 0 {
		istioGateway = "kubeflow/kubeflow-gateway"
	}
	if err := unstructured.SetNestedStringSlice(vsvc.Object, []string{istioGateway},
		"spec", "gateways"); err != nil {
		return nil, fmt.Errorf("Set .spec.gateways error: %v", err)
	}

	headersRequestSet := make(map[string]string)
	// If AnnotationHeadersRequestSet is present, use its values in "headers.request.set"
	if _, ok := annotations[AnnotationHeadersRequestSet]; ok && len(annotations[AnnotationHeadersRequestSet]) > 0 {
		requestHeadersBytes := []byte(annotations[AnnotationHeadersRequestSet])
		if err := json.Unmarshal(requestHeadersBytes, &headersRequestSet); err != nil {
			// if JSON decoding fails, set an empty map
			headersRequestSet = make(map[string]string)
		}
	}
	// cast from map[string]string, as SetNestedSlice needs map[string]interface{}
	headersRequestSetInterface := make(map[string]interface{})
	for key, element := range headersRequestSet {
		headersRequestSetInterface[key] = element
	}

	// the http section of the istio VirtualService spec
	http := []interface{}{
		map[string]interface{}{
			"headers": map[string]interface{}{
				"request": map[string]interface{}{
					"set": headersRequestSetInterface,
				},
			},
			"match": []interface{}{
				map[string]interface{}{
					"uri": map[string]interface{}{
						"prefix": prefix,
					},
				},
			},
			"rewrite": map[string]interface{}{
				"uri": rewrite,
			},
			"route": []interface{}{
				map[string]interface{}{
					"destination": map[string]interface{}{
						"host": service,
						"port": map[string]interface{}{
							"number": int64(DefaultServingPort),
						},
					},
				},
			},
		},
	}

	// add http section to istio VirtualService spec
	if err := unstructured.SetNestedSlice(vsvc.Object, http, "spec", "http"); err != nil {
		return nil, fmt.Errorf("Set .spec.http error: %v", err)
	}

	return vsvc, nil

}

func (r *NotebookReconciler) reconcileVirtualService(instance *v1.Notebook) error {
	log := r.Log.WithValues("notebook", instance.Namespace)
	virtualService, err := generateVirtualService(instance)
	if err := ctrl.SetControllerReference(instance, virtualService, r.Scheme); err != nil {
		return err
	}
	// Check if the virtual service already exists.
	foundVirtual := &unstructured.Unstructured{}
	justCreated := false
	foundVirtual.SetAPIVersion("networking.istio.io/v1alpha3")
	foundVirtual.SetKind("VirtualService")
	err = r.Get(context.TODO(), types.NamespacedName{Name: virtualServiceName(instance.Name,
		instance.Namespace), Namespace: instance.Namespace}, foundVirtual)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating virtual service", "namespace", instance.Namespace, "name",
			virtualServiceName(instance.Name, instance.Namespace))
		err = r.Create(context.TODO(), virtualService)
		justCreated = true
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if !justCreated && reconcilehelper.CopyVirtualService(virtualService, foundVirtual) {
		log.Info("Updating virtual service", "namespace", instance.Namespace, "name",
			virtualServiceName(instance.Name, instance.Namespace))
		err = r.Update(context.TODO(), foundVirtual)
		if err != nil {
			return err
		}
	}

	return nil
}

func isStsOrPodEvent(event *corev1.Event) bool {
	return event.InvolvedObject.Kind == "Pod" || event.InvolvedObject.Kind == "StatefulSet"
}

func nbNameFromInvolvedObject(c client.Client, object *corev1.ObjectReference) (string, error) {
	name, namespace := object.Name, object.Namespace

	if object.Kind == "StatefulSet" {
		return name, nil
	}
	if object.Kind == "Pod" {
		pod := &corev1.Pod{}
		err := c.Get(
			context.TODO(),
			types.NamespacedName{
				Namespace: namespace,
				Name:      name,
			},
			pod,
		)
		if err != nil {
			return "", err
		}
		if nbName, ok := pod.Labels["notebook-name"]; ok {
			return nbName, nil
		}
	}
	return "", fmt.Errorf("object isn't related to a Notebook")
}

func nbNameExists(client client.Client, nbName string, namespace string) bool {
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: nbName}, &v1.Notebook{}); err != nil {
		// If error != NotFound, trigger the reconcile call anyway to avoid loosing a potential relevant event
		return !apierrs.IsNotFound(err)
	}
	return true
}

// predNBPodIsLabeled filters pods not containing the "notebook-name" label key
func predNBPodIsLabeled() predicate.Funcs {
	// Documented at
	// https://github.com/kubernetes-sigs/controller-runtime/blob/ce8bdd3d81ab410ff23255e9ad3554f613c5183c/pkg/predicate/predicate_test.go#L884
	checkNBLabel := func() func(object client.Object) bool {
		return func(object client.Object) bool {
			_, labelExists := object.GetLabels()["notebook-name"]
			return labelExists
		}
	}

	return predicate.NewPredicateFuncs(checkNBLabel())
}

// predNBEvents filters events not coming from Pod or STS, and coming from
// unknown NBs
func predNBEvents(r *NotebookReconciler) predicate.Funcs {
	checkEvent := func() func(object client.Object) bool {
		return func(object client.Object) bool {
			event := object.(*corev1.Event)
			nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
			if err != nil {
				return false
			}
			return isStsOrPodEvent(event) && nbNameExists(r.Client, nbName, object.GetNamespace())
		}
	}

	predicates := predicate.NewPredicateFuncs(checkEvent())

	// Do not reconcile when an event gets deleted
	predicates.DeleteFunc = func(e event.DeleteEvent) bool {
		return false
	}

	return predicates
}

// SetupWithManager sets up the controller with the Manager.
func (r *NotebookReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Map function to convert pod events to reconciliation requests
	mapPodToRequest := func(object client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{
				Name:      object.GetLabels()["notebook-name"],
				Namespace: object.GetNamespace(),
			}},
		}
	}

	// Map function to convert namespace events to reconciliation requests
	mapEventToRequest := func(object client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{
				Name:      object.GetName(),
				Namespace: object.GetNamespace(),
			}},
		}
	}

	
	// watch Certificate
	certificate := &unstructured.Unstructured{}
	certificate.SetAPIVersion("cert-manager.io/v1")
	certificate.SetKind("Certificate")
	

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&v1.Notebook{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&netv1.Ingress{}).
		Owns(certificate).
		Watches(
			&source.Kind{Type: &corev1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(mapPodToRequest),
			builder.WithPredicates(predNBPodIsLabeled())).
		Watches(
			&source.Kind{Type: &corev1.Event{}},
			handler.EnqueueRequestsFromMapFunc(mapEventToRequest),
			builder.WithPredicates(predNBEvents(r)))
	// watch Istio virtual service
	if os.Getenv("USE_ISTIO") == "true" {
		virtualService := &unstructured.Unstructured{}
		virtualService.SetAPIVersion("networking.istio.io/v1alpha3")
		virtualService.SetKind("VirtualService")
		builder.Owns(virtualService)
	}
	
	

	err := builder.Complete(r)
	if err != nil {
		return err
	}

	return nil
}
