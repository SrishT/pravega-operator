/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package pravega

import (
	"strings"

	"fmt"

	api "github.com/pravega/pravega-operator/pkg/apis/pravega/v1alpha1"
	"github.com/pravega/pravega-operator/pkg/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func MakeControllerDeployment(p *api.PravegaCluster) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.DeploymentNameForController(p.Name),
			Namespace: p.Namespace,
			Labels:    util.LabelsForController(p),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &p.Spec.Pravega.ControllerReplicas,
			Template: MakeControllerPodTemplate(p),
			Selector: &metav1.LabelSelector{
				MatchLabels: util.LabelsForController(p),
			},
		},
	}
}

func MakeControllerPodTemplate(p *api.PravegaCluster) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      util.LabelsForController(p),
			Annotations: map[string]string{"pravega.version": p.Spec.Version},
		},
		Spec: *makeControllerPodSpec(p),
	}
}

func makeControllerPodSpec(p *api.PravegaCluster) *corev1.PodSpec {
	podSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:            "pravega-controller",
				Image:           util.PravegaImage(p),
				ImagePullPolicy: p.Spec.Pravega.Image.PullPolicy,
				Args: []string{
					"controller",
				},
				Ports: []corev1.ContainerPort{
					{
						Name:          "rest",
						ContainerPort: 10080,
					},
					{
						Name:          "grpc",
						ContainerPort: 9090,
					},
				},
				EnvFrom: []corev1.EnvFromSource{
					{
						ConfigMapRef: &corev1.ConfigMapEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: util.ConfigMapNameForController(p.Name),
							},
						},
					},
				},
				Resources: *p.Spec.Pravega.ControllerResources,
				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: util.HealthcheckCommand(9090),
						},
					},
					// Controller pods start fast. We give it up to 1 minute to become ready.
					PeriodSeconds:    5,
					FailureThreshold: 12,
				},
				LivenessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: util.HealthcheckCommand(9090),
						},
					},
					// We start the liveness probe from the maximum time the pod can take
					// before becoming ready.
					// If the pod fails the health check during 1 minute, Kubernetes
					// will restart it.
					InitialDelaySeconds: 60,
					PeriodSeconds:       15,
					FailureThreshold:    4,
				},
			},
		},
		Affinity: util.PodAntiAffinity("pravega-controller", p.Name),
	}

	if p.Spec.Pravega.ControllerServiceAccountName != "" {
		podSpec.ServiceAccountName = p.Spec.Pravega.ControllerServiceAccountName
	}

	configureControllerTLSSecrets(podSpec, p)

	return podSpec
}

func configureControllerTLSSecrets(podSpec *corev1.PodSpec, p *api.PravegaCluster) {
	if p.Spec.TLS.IsSecureController() {
		vol := corev1.Volume{
			Name: tlsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: p.Spec.TLS.Static.ControllerSecret,
				},
			},
		}
		podSpec.Volumes = append(podSpec.Volumes, vol)

		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      tlsVolumeName,
			MountPath: tlsMountDir,
		})
	}
}

func MakeControllerConfigMap(p *api.PravegaCluster) *corev1.ConfigMap {
	var javaOpts = []string{
		"-Xms512m",
		"-XX:+ExitOnOutOfMemoryError",
		"-XX:+CrashOnOutOfMemoryError",
		"-XX:+HeapDumpOnOutOfMemoryError",
		"-Dpravegaservice.clusterName=" + p.Name,
	}

	if match, _ := util.CompareVersions(p.Spec.Version, "0.4.0", ">="); match {
		// Pravega < 0.4 uses a Java version that does not support the options below
		javaOpts = append(javaOpts,
			"-XX:+UnlockExperimentalVMOptions",
			"-XX:+UseCGroupMemoryLimitForHeap",
			"-XX:MaxRAMFraction=2",
		)
	}

	for name, value := range p.Spec.Pravega.Options {
		javaOpts = append(javaOpts, fmt.Sprintf("-D%v=%v", name, value))
	}

	configData := map[string]string{
		"CLUSTER_NAME":           p.Name,
		"ZK_URL":                 p.Spec.ZookeeperUri,
		"JAVA_OPTS":              strings.Join(javaOpts, " "),
		"REST_SERVER_PORT":       "10080",
		"CONTROLLER_SERVER_PORT": "9090",
		"AUTHORIZATION_ENABLED":  "false",
		"TOKEN_SIGNING_KEY":      "secret",
		"USER_PASSWORD_FILE":     "/etc/pravega/conf/passwd",
		"TLS_ENABLED":            "false",
		"WAIT_FOR":               p.Spec.ZookeeperUri,
	}

	configMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.ConfigMapNameForController(p.Name),
			Labels:    util.LabelsForController(p),
			Namespace: p.Namespace,
		},
		Data: configData,
	}

	return configMap
}

func MakeControllerService(p *api.PravegaCluster) *corev1.Service {
	serviceType := corev1.ServiceTypeClusterIP
	if p.Spec.ExternalAccess.Enabled {
		serviceType = p.Spec.ExternalAccess.Type
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.ServiceNameForController(p.Name),
			Namespace: p.Namespace,
			Labels:    util.LabelsForController(p),
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
			Ports: []corev1.ServicePort{
				{
					Name: "rest",
					Port: 10080,
				},
				{
					Name: "grpc",
					Port: 9090,
				},
			},
			Selector: util.LabelsForController(p),
		},
	}
}

func MakeControllerPodDisruptionBudget(pravegaCluster *api.PravegaCluster) *policyv1beta1.PodDisruptionBudget {
	minAvailable := intstr.FromInt(1)
	return &policyv1beta1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodDisruptionBudget",
			APIVersion: "policy/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.PdbNameForController(pravegaCluster.Name),
			Namespace: pravegaCluster.Namespace,
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: util.LabelsForController(pravegaCluster),
			},
		},
	}
}
