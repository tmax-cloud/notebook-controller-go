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
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	kubeflowv1 "github.com/tmax-cloud/notebook-controller-go/api/v1"
	"github.com/tmax-cloud/notebook-controller-go/pkg/culler"
	"github.com/tmax-cloud/notebook-controller-go/pkg/metrics"
	reconcilehelper "github.com/tmax-cloud/notebook-controller-go/pkg/reconcilehelper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/extensions/v1beta1"
//	certv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
//	cmmetav1 "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
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

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tmax.io,resources=notebooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tmax.io,resources=notebooks/status,verbs=get;update;patch

func (r *NotebookReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("notebook", req.NamespacedName)
	 
	// TODO(yanniszark): Can we avoid reconciling Events and Notebook in the same queue?
	event := &corev1.Event{}
	var getEventErr error
	getEventErr = r.Get(ctx, req.NamespacedName, event)
	if getEventErr == nil {
		involvedNotebook := &kubeflowv1.Notebook{}
		nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
		if err != nil {
			return ctrl.Result{}, err
		}
		involvedNotebookKey := types.NamespacedName{Name: nbName, Namespace: req.Namespace}
		if err := r.Get(ctx, involvedNotebookKey, involvedNotebook); err != nil {
			log.Error(err, "unable to fetch Notebook by looking at event")
			return ctrl.Result{}, ignoreNotFound(err)
		}
		r.EventRecorder.Eventf(involvedNotebook, event.Type, event.Reason,
			"Reissued from %s/%s: %s", strings.ToLower(event.InvolvedObject.Kind), event.InvolvedObject.Name, event.Message)
	}
	if getEventErr != nil && !apierrs.IsNotFound(getEventErr) {
		return ctrl.Result{}, getEventErr
	}
	// If not found, continue. Is not an event.

	instance := &kubeflowv1.Notebook{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		log.Error(err, "unable to fetch Notebook")
		return ctrl.Result{}, ignoreNotFound(err)
	}

	// Create PersistentVolumeClaim
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

	// Reconcile Ingress
	err = r.reconcileIngress(instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Reconcile Certificate
	err = r.reconcileCertificate(instance)
	if err != nil {
		return ctrl.Result{}, err
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
				instance.Status.Conditions = append([]kubeflowv1.NotebookCondition{newCondition}, oldConditions...)
			}
			err = r.Status().Update(ctx, instance)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Check if the Notebook needs to be stopped
	if podFound && culler.NotebookNeedsCulling(instance.ObjectMeta) {
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
	} else if podFound && !culler.StopAnnotationIsSet(instance.ObjectMeta) {
		// The Pod is either too fresh, or the idle time has passed and it has
		// received traffic. In this case we will be periodically checking if
		// it needs culling.
		return ctrl.Result{RequeueAfter: culler.GetRequeueTime()}, nil
	}

	return ctrl.Result{}, nil
}

func getNextCondition(cs corev1.ContainerState) kubeflowv1.NotebookCondition {
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

	newCondition := kubeflowv1.NotebookCondition{
		Type:          nbtype,
		LastProbeTime: metav1.Now(),
		Reason:        nbreason,
		Message:       nbmsg,
	}
	return newCondition
}

func generatePersistentVolumeClaim(instance *kubeflowv1.Notebook) *corev1.PersistentVolumeClaim {
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

func generateStatefulSet(instance *kubeflowv1.Notebook) *appsv1.StatefulSet {
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
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"sidecar.istio.io/inject": "false",
					},
					Labels: map[string]string{
						"statefulset":   instance.Name,
						"notebook-name": instance.Name,
					},
				},
				Spec: instance.Spec.Template.Spec,
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
//	volumes := &podSpec.Volumes[0]

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
	
	clientsecret := os.Getenv("CLIENT_SECRET")
    discoveryurl := os.Getenv("DISCOVERY_URL")
			
	podSpec.Containers = append(podSpec.Containers, corev1.Container{
		Name:  "gatekeeper",
		Image: "quay.io/keycloak/keycloak-gatekeeper:10.0.0",
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
			"--enable-self-signed-tls",
			"--enable-refresh-tokens=true",
			"--enable-default-deny=true",
			"--enable-metrics=true",
			"--encryption-key=AgXa7xRcoClDEU0ZDSH4X0XhL5Qy2Z2j",
			"--resources=uri=/*|roles=notebook-gatekeeper:notebook-gatekeeper-manager",
			"--verbose",
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

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: "secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: instance.Name + "-secret",
			},
		},
	})
	
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

func generateService(instance *kubeflowv1.Notebook) *corev1.Service {
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
	return fmt.Sprintf("notebook-%s-%s", namespace, kfName)
}

func generateIngress(instance *kubeflowv1.Notebook) (*netv1.Ingress, error) {
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
		Hosts:      []string{instance.Name + "." + customDomain},
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
				"ingress.tmaxcloud.org/name":   instance.Name,				
			},
		},
		Spec: netv1.IngressSpec{
			TLS:              tls,
			IngressClassName: ingressclassname,
			Rules: []netv1.IngressRule{
				{
					Host: instance.Name + "." + customDomain,
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathTypePrefix,
									Backend: netv1.IngressBackend{
										ServiceName: instance.Name,
										ServicePort: intstr.FromInt(int(HttpsServingPort)),
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

func (r *NotebookReconciler) reconcileIngress(instance *kubeflowv1.Notebook) error {	
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

func generateCertificate(instance *kubeflowv1.Notebook) (*unstructured.Unstructured, error) {
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

func (r *NotebookReconciler) reconcileCertificate(instance *kubeflowv1.Notebook) error {	
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
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: nbName}, &kubeflowv1.Notebook{}); err != nil {
		// If error != NotFound, trigger the reconcile call anyway to avoid loosing a potential relevant event
		return !apierrs.IsNotFound(err)
	}
	return true
}

func (r *NotebookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&kubeflowv1.Notebook{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{})
	// watch Ingress
	ingress := &netv1.Ingress{}	
	builder.Owns(ingress)
	// watch Certificate
	certificate := &unstructured.Unstructured{}
	certificate.SetAPIVersion("cert-manager.io/v1")
	certificate.SetKind("Certificate")
	builder.Owns(certificate)
	
	c, err := builder.Build(r)
	if err != nil {
		return err
	}

	// watch underlying pod
	mapFn := handler.ToRequestsFunc(
		func(a handler.MapObject) []ctrl.Request {
			return []ctrl.Request{
				{NamespacedName: types.NamespacedName{
					Name:      a.Meta.GetLabels()["notebook-name"],
					Namespace: a.Meta.GetNamespace(),
				}},
			}
		})
	p := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if _, ok := e.MetaOld.GetLabels()["notebook-name"]; !ok {
				return false
			}
			return e.ObjectOld != e.ObjectNew
		},
		CreateFunc: func(e event.CreateEvent) bool {
			if _, ok := e.Meta.GetLabels()["notebook-name"]; !ok {
				return false
			}
			return true
		},
	}

	eventToRequest := handler.ToRequestsFunc(
		func(a handler.MapObject) []ctrl.Request {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Name:      a.Meta.GetName(),
					Namespace: a.Meta.GetNamespace(),
				}},
			}
		})

	eventsPredicates := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			event := e.ObjectNew.(*corev1.Event)
			nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
			if err != nil {
				return false
			}
			return e.ObjectOld != e.ObjectNew &&
				isStsOrPodEvent(event) &&
				nbNameExists(r.Client, nbName, e.MetaNew.GetNamespace())
		},
		CreateFunc: func(e event.CreateEvent) bool {
			event := e.Object.(*corev1.Event)
			nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
			if err != nil {
				return false
			}
			return isStsOrPodEvent(event) &&
				nbNameExists(r.Client, nbName, e.Meta.GetNamespace())
		},
	}

	if err = c.Watch(
		&source.Kind{Type: &corev1.Pod{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: mapFn,
		},
		p); err != nil {
		return err
	}

	if err = c.Watch(
		&source.Kind{Type: &corev1.Event{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: eventToRequest,
		},
		eventsPredicates); err != nil {
		return err
	}

	return nil
}
