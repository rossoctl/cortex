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

package injector

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var builderLog = logf.Log.WithName("namespace-checker")

func BuildSpiffeHelperContainer() corev1.Container {
	builderLog.Info("building SpiffyHelper Container")

	return corev1.Container{
		Name:            SpiffeHelperContainerName,
		Image:           "ghcr.io/spiffe/spiffe-helper:nightly",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		Command: []string{
			"/spiffe-helper",
			"-config=/etc/spiffe-helper/helper.conf",
			"run",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "spiffe-helper-config",
				MountPath: "/etc/spiffe-helper",
			},
			{
				Name:      "spire-agent-socket",
				MountPath: "/spiffe-workload-api",
			},
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				Name:      "shared-data",
				MountPath: "/shared",
			},
		},
	}
}

func BuildClientRegistrationContainer(clientID, name, namespace string) corev1.Container {
	builderLog.Info("building ClientRegistration Container")

	clientId := namespace + "/" + name
	return corev1.Container{
		Name:            ClientRegistrationContainerName,
		Image:           "ghcr.io/kagenti/kagenti/client-registration:latest",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		Command: []string{
			"/bin/sh",
			"-c",
			"while [ ! -f /opt/jwt_svid.token ]; do echo waiting for SVID; sleep 1; done; python client_registration.py; tail -f /dev/null",
		},
		Env: []corev1.EnvVar{
			{
				Name: "KEYCLOAK_URL",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key:      "KEYCLOAK_URL",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name: "KEYCLOAK_REALM",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key: "KEYCLOAK_REALM",
					},
				},
			},
			{
				Name: "KEYCLOAK_ADMIN_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key: "KEYCLOAK_ADMIN_USERNAME",
					},
				},
			},
			{
				Name: "KEYCLOAK_ADMIN_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key: "KEYCLOAK_ADMIN_PASSWORD",
					},
				},
			},
			{
				Name: "KEYCLOAK_TOKEN_EXCHANGE_ENABLED",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key:      "KEYCLOAK_TOKEN_EXCHANGE_ENABLED",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name: "KEYCLOAK_CLIENT_REGISTRATION_ENABLED",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "environments",
						},
						Key:      "KEYCLOAK_CLIENT_REGISTRATION_ENABLED",
						Optional: ptr.To(true),
					},
				},
			},
			{
				Name:  "CLIENT_NAME",
				Value: clientId,
			},
			{
				Name:  "CLIENT_ID",
				Value: "spiffe://localtest.me/sa/" + name,
			},
			{
				Name:  "NAMESPACE",
				Value: namespace,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "svid-output",
				MountPath: "/opt",
			},
			{
				// This is how client registration accesses the SVID
				Name:      "shared-data",
				MountPath: "/shared",
			},
		},
	}
}
