package reconcilehelper

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/extensions/v1beta1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Deployment reconciles a k8s deployment object.
func Deployment(ctx context.Context, r client.Client, deployment *appsv1.Deployment, log logr.Logger) error {
	foundDeployment := &appsv1.Deployment{}
	justCreated := false
	if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, foundDeployment); err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Deployment", "namespace", deployment.Namespace, "name", deployment.Name)
			if err := r.Create(ctx, deployment); err != nil {
				log.Error(err, "unable to create deployment")
				return err
			}
			justCreated = true
		} else {
			log.Error(err, "error getting deployment")
			return err
		}
	}
	if !justCreated && CopyDeploymentSetFields(deployment, foundDeployment) {
		log.Info("Updating Deployment", "namespace", deployment.Namespace, "name", deployment.Name)
		if err := r.Update(ctx, foundDeployment); err != nil {
			log.Error(err, "unable to update deployment")
			return err
		}
	}

	return nil
}

// Service reconciles a k8s service object.
func Service(ctx context.Context, r client.Client, service *corev1.Service, log logr.Logger) error {
	foundService := &corev1.Service{}
	justCreated := false
	if err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService); err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating Service", "namespace", service.Namespace, "name", service.Name)
			if err = r.Create(ctx, service); err != nil {
				log.Error(err, "unable to create service")
				return err
			}
			justCreated = true
		} else {
			log.Error(err, "error getting service")
			return err
		}
	}
	if !justCreated && CopyServiceFields(service, foundService) {
		log.Info("Updating Service\n", "namespace", service.Namespace, "name", service.Name)
		if err := r.Update(ctx, foundService); err != nil {
			log.Error(err, "unable to update Service")
			return err
		}
	}

	return nil
}


func Ingress(ctx context.Context, r client.Client, ingressName, namespace string, ingress *netv1.Ingress, log logr.Logger) error {
	foundIngress := &netv1.Ingress{}
	justCreated := false	
	if err := r.Get(ctx, types.NamespacedName{Name: ingressName, Namespace: namespace}, foundIngress); err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating ingress", "namespace", namespace, "name", ingressName)
			if err := r.Create(ctx, ingress); err != nil {
				log.Error(err, "unable to create ingress")
				return err
			}
			justCreated = true
		} else {
			log.Error(err, "error getting ingress")
			return err
		}
	}
	if !justCreated && CopyIngress(ingress, foundIngress) {
		log.Info("Updating ingress", "namespace", namespace, "name", ingressName)
		if err := r.Update(ctx, foundIngress); err != nil {
			log.Error(err, "unable to update ingress")
			return err
		}
	}

	return nil
}

func Certificate(ctx context.Context, r client.Client, certificateName, namespace string, certificate *unstructured.Unstructured, log logr.Logger) error {
	foundCertificate := &unstructured.Unstructured{}
	foundCertificate.SetAPIVersion("cert-manager.io/v1")
	foundCertificate.SetKind("Certificate")
	justCreated := false	
	if err := r.Get(ctx, types.NamespacedName{Name: certificateName, Namespace: namespace}, foundCertificate); err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("Creating certificate", "namespace", namespace, "name", certificateName)
			if err := r.Create(ctx, certificate); err != nil {
				log.Error(err, "unable to create certificate")
				return err
			}
			justCreated = true
		} else {
			log.Error(err, "error getting certificate")
			return err
		}
	}
	if !justCreated && CopyCertificate(certificate, foundCertificate) {
		log.Info("Updating certificate", "namespace", namespace, "name", certificateName)
		if err := r.Update(ctx, foundCertificate); err != nil {
			log.Error(err, "unable to update certificate")
			return err
		}
	}

	return nil
}


// Reference: https://github.com/pwittrock/kubebuilder-workshop/blob/master/pkg/util/util.go

// CopyStatefulSetFields copies the owned fields from one StatefulSet to another
// Returns true if the fields copied from don't match to.
func CopyStatefulSetFields(from, to *appsv1.StatefulSet) bool {
	requireUpdate := false
	for k, v := range to.Labels {
		if from.Labels[k] != v {
			requireUpdate = true
		}
	}
	to.Labels = from.Labels

	for k, v := range to.Annotations {
		if from.Annotations[k] != v {
			requireUpdate = true
		}
	}
	to.Annotations = from.Annotations

	if from.Spec.Replicas != to.Spec.Replicas {
		to.Spec.Replicas = from.Spec.Replicas
		requireUpdate = true
	}

	if !reflect.DeepEqual(to.Spec.Template.Spec, from.Spec.Template.Spec) {
		requireUpdate = true
	}
	to.Spec.Template.Spec = from.Spec.Template.Spec

	return requireUpdate
}

func CopyDeploymentSetFields(from, to *appsv1.Deployment) bool {
	requireUpdate := false
	for k, v := range to.Labels {
		if from.Labels[k] != v {
			requireUpdate = true
		}
	}
	to.Labels = from.Labels

	for k, v := range to.Annotations {
		if from.Annotations[k] != v {
			requireUpdate = true
		}
	}
	to.Annotations = from.Annotations

	if from.Spec.Replicas != to.Spec.Replicas {
		to.Spec.Replicas = from.Spec.Replicas
		requireUpdate = true
	}

	if !reflect.DeepEqual(to.Spec.Template.Spec, from.Spec.Template.Spec) {
		requireUpdate = true
	}
	to.Spec.Template.Spec = from.Spec.Template.Spec

	return requireUpdate
}

// CopyServiceFields copies the owned fields from one Service to another
func CopyServiceFields(from, to *corev1.Service) bool {
	requireUpdate := false
	for k, v := range to.Labels {
		if from.Labels[k] != v {
			requireUpdate = true
		}
	}
	to.Labels = from.Labels

	for k, v := range to.Annotations {
		if from.Annotations[k] != v {
			requireUpdate = true
		}
	}
	to.Annotations = from.Annotations

	// Don't copy the entire Spec, because we can't overwrite the clusterIp field

	if !reflect.DeepEqual(to.Spec.Selector, from.Spec.Selector) {
		requireUpdate = true
	}
	to.Spec.Selector = from.Spec.Selector

	if !reflect.DeepEqual(to.Spec.Ports, from.Spec.Ports) {
		requireUpdate = true
	}
	to.Spec.Ports = from.Spec.Ports

	return requireUpdate
}

// Copy configuration related fields to another instance and returns true if there
// is a diff and thus needs to update.
func CopyIngress(from, to *netv1.Ingress) bool {
	requireUpdate := false

	// Don't copy the entire Spec, because we can't overwrite the clusterIp field

	if !reflect.DeepEqual(to.Spec.TLS, from.Spec.TLS) {
		requireUpdate = true
	}
	to.Spec.TLS = from.Spec.TLS

	if !reflect.DeepEqual(to.Spec.Rules, from.Spec.Rules) {
		requireUpdate = true
	}
	to.Spec.Rules = from.Spec.Rules

	return requireUpdate
}

func CopyCertificate(from, to *unstructured.Unstructured) bool {
	fromSpec, found, err := unstructured.NestedMap(from.Object, "spec")
	if !found {
		return false
	}
	if err != nil {
		return false
	}

	toSpec, found, err := unstructured.NestedMap(to.Object, "spec")
	if !found || err != nil {
		unstructured.SetNestedMap(to.Object, fromSpec, "spec")
		return true
	}

	requiresUpdate := !reflect.DeepEqual(fromSpec, toSpec)
	if requiresUpdate {
		unstructured.SetNestedMap(to.Object, fromSpec, "spec")
	}
	return requiresUpdate
}
