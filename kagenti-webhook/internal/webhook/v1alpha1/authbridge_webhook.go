/*
Copyright 2025.

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

package v1alpha1

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/kagenti/kagenti-extensions/kagenti-webhook/internal/webhook/injector"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// authbridgelog is for logging in this package.
var authbridgelog = logf.Log.WithName("authbridge-webhook")

// AuthBridgeWebhook handles mutation of workload resources for AuthBridge injection
type AuthBridgeWebhook struct {
	Mutator *injector.PodMutator
	decoder admission.Decoder
}

// SetupAuthBridgeWebhookWithManager registers the authbridge webhook with the manager
func SetupAuthBridgeWebhookWithManager(mgr ctrl.Manager, mutator *injector.PodMutator) error {
	webhook := &AuthBridgeWebhook{
		Mutator: mutator,
		decoder: admission.NewDecoder(mgr.GetScheme()),
	}

	mgr.GetWebhookServer().Register("/mutate-workloads-authbridge", &admission.Webhook{
		Handler: webhook,
	})

	return nil
}

// Handle processes admission requests for workload resources
func (w *AuthBridgeWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	authbridgelog.Info("AuthBridge webhook called",
		"kind", req.Kind.Kind,
		"namespace", req.Namespace,
		"name", req.Name,
		"operation", req.Operation)

	var podSpec *corev1.PodSpec
	var resourceName string
	var mutatedObj interface{}
	var labels map[string]string
	var annotations map[string]string

	// Extract PodSpec based on resource type
	switch req.Kind.Kind {
	case "Deployment":
		var deployment appsv1.Deployment
		if err := w.decoder.Decode(req, &deployment); err != nil {
			authbridgelog.Error(err, "Failed to decode Deployment")
			return admission.Errored(http.StatusBadRequest, err)
		}
		podSpec = &deployment.Spec.Template.Spec
		resourceName = deployment.Name
		mutatedObj = &deployment
		labels = deployment.Spec.Template.Labels
		annotations = deployment.Spec.Template.Annotations

	case "StatefulSet":
		var statefulset appsv1.StatefulSet
		if err := w.decoder.Decode(req, &statefulset); err != nil {
			authbridgelog.Error(err, "Failed to decode StatefulSet")
			return admission.Errored(http.StatusBadRequest, err)
		}
		podSpec = &statefulset.Spec.Template.Spec
		resourceName = statefulset.Name
		mutatedObj = &statefulset
		labels = statefulset.Spec.Template.Labels
		annotations = statefulset.Spec.Template.Annotations

	case "DaemonSet":
		var daemonset appsv1.DaemonSet
		if err := w.decoder.Decode(req, &daemonset); err != nil {
			authbridgelog.Error(err, "Failed to decode DaemonSet")
			return admission.Errored(http.StatusBadRequest, err)
		}
		podSpec = &daemonset.Spec.Template.Spec
		resourceName = daemonset.Name
		mutatedObj = &daemonset
		labels = daemonset.Spec.Template.Labels
		annotations = daemonset.Spec.Template.Annotations

	case "Job":
		var job batchv1.Job
		if err := w.decoder.Decode(req, &job); err != nil {
			authbridgelog.Error(err, "Failed to decode Job")
			return admission.Errored(http.StatusBadRequest, err)
		}
		podSpec = &job.Spec.Template.Spec
		resourceName = job.Name
		mutatedObj = &job
		labels = job.Spec.Template.Labels
		annotations = job.Spec.Template.Annotations

	case "CronJob":
		var cronjob batchv1.CronJob
		if err := w.decoder.Decode(req, &cronjob); err != nil {
			authbridgelog.Error(err, "Failed to decode CronJob")
			return admission.Errored(http.StatusBadRequest, err)
		}
		podSpec = &cronjob.Spec.JobTemplate.Spec.Template.Spec
		resourceName = cronjob.Name
		mutatedObj = &cronjob
		labels = cronjob.Spec.JobTemplate.Spec.Template.Labels
		annotations = cronjob.Spec.JobTemplate.Spec.Template.Annotations

	default:
		authbridgelog.Info("Unsupported resource kind", "kind", req.Kind.Kind)
		return admission.Allowed("unsupported kind")
	}

	// Check if already injected (idempotency)
	if w.isAlreadyInjected(podSpec) {
		authbridgelog.Info("Skipping - sidecars already injected",
			"kind", req.Kind.Kind,
			"namespace", req.Namespace,
			"name", resourceName)
		return admission.Allowed("already injected")
	}

	if mutated, err := w.Mutator.InjectAuthBridge(ctx, podSpec, req.Namespace, resourceName, labels, annotations); err != nil {
		authbridgelog.Error(err, "Failed to mutate pod spec",
			"kind", req.Kind.Kind,
			"namespace", req.Namespace,
			"name", resourceName)
		return admission.Errored(http.StatusInternalServerError, err)
	} else if !mutated {
		authbridgelog.Info("Skipping mutation (injection not enabled)",
			"kind", req.Kind.Kind,
			"namespace", req.Namespace,
			"name", resourceName)
		return admission.Allowed("injection not enabled")
	}

	// Marshal the mutated object
	marshaledMutated, err := json.Marshal(mutatedObj)
	if err != nil {
		authbridgelog.Error(err, "Failed to marshal mutated resource")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	authbridgelog.Info("Successfully mutated resource",
		"kind", req.Kind.Kind,
		"namespace", req.Namespace,
		"name", resourceName)

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledMutated)
}

func (w *AuthBridgeWebhook) isAlreadyInjected(podSpec *corev1.PodSpec) bool {
	// Check sidecar containers (envoy-proxy is always injected by the AuthBridge path,
	// so it serves as a reliable marker even when spiffe-helper and client-registration
	// are both disabled via their respective flags)
	for _, container := range podSpec.Containers {
		if container.Name == injector.EnvoyProxyContainerName ||
			container.Name == injector.SpiffeHelperContainerName ||
			container.Name == injector.ClientRegistrationContainerName {
			return true
		}
	}
	// Also check init containers — proxy-init is always injected by InjectAuthBridge
	for _, container := range podSpec.InitContainers {
		if container.Name == injector.ProxyInitContainerName {
			return true
		}
	}
	return false
}

// +kubebuilder:webhook:path=/mutate-workloads-authbridge,mutating=true,failurePolicy=fail,sideEffects=None,groups=apps;batch,resources=deployments;statefulsets;daemonsets;jobs;cronjobs,verbs=create;update,versions=v1,name=inject.kagenti.io,admissionReviewVersions=v1
