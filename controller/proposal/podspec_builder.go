package proposal

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	ErrBuildMCPServers        = "build MCP servers"
	ErrMarshalMCPServerConfig = "marshal MCP server config"
)

type PodSpecBuilder struct {
	Image           string
	ImagePullPolicy string
}

func (b *PodSpecBuilder) Build(
	agent *agenticv1alpha1.Agent,
	llm *agenticv1alpha1.LLMProvider,
	tools *agenticv1alpha1.ToolsSpec,
	step string,
	serviceAccount string,
) (*corev1.PodSpec, error) {
	if agent == nil {
		return nil, fmt.Errorf("agent is required")
	}
	if llm == nil {
		return nil, fmt.Errorf("LLMProvider is required")
	}
	if serviceAccount == "" {
		return nil, fmt.Errorf("serviceAccount is required")
	}

	container := corev1.Container{
		Name:            "agent",
		Image:           b.Image,
		ImagePullPolicy: corev1.PullPolicy(b.ImagePullPolicy),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: 8080,
			Protocol:      corev1.ProtocolTCP,
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	var volumes []corev1.Volume

	container.Env = append(container.Env,
		corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER", Value: providerTypeString(llm.Spec.Type)},
		corev1.EnvVar{Name: "LIGHTSPEED_MODEL", Value: agent.Spec.Model},
	)
	b.addProviderSpecificEnv(&container, llm)

	secretName := credentialsSecretName(llm)
	container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
		},
	})
	volumes = append(volumes, corev1.Volume{
		Name: llmCredsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      llmCredsVolumeName,
		MountPath: llmCredsMountPath,
		ReadOnly:  true,
	})

	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
		},
		InitialDelaySeconds: 3,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
	container.LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       30,
		FailureThreshold:    3,
	}

	if tools != nil {
		skillVols, skillMounts := b.buildSkills(tools.Skills)
		volumes = append(volumes, skillVols...)
		container.VolumeMounts = append(container.VolumeMounts, skillMounts...)
	}

	if tools != nil && len(tools.MCPServers) > 0 {
		mcpVols, mcpMounts, mcpEnv, err := b.buildMCPServers(tools.MCPServers)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ErrBuildMCPServers, err)
		}
		volumes = append(volumes, mcpVols...)
		container.VolumeMounts = append(container.VolumeMounts, mcpMounts...)
		container.Env = append(container.Env, mcpEnv...)
	}

	if tools != nil && len(tools.RequiredSecrets) > 0 {
		secVols, secMounts, secEnv := b.buildRequiredSecrets(tools.RequiredSecrets)
		volumes = append(volumes, secVols...)
		container.VolumeMounts = append(container.VolumeMounts, secMounts...)
		container.Env = append(container.Env, secEnv...)
	}

	return &corev1.PodSpec{
		ServiceAccountName:           serviceAccount,
		AutomountServiceAccountToken: ptr.To(true),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes:    volumes,
	}, nil
}

func (b *PodSpecBuilder) addProviderSpecificEnv(container *corev1.Container, llm *agenticv1alpha1.LLMProvider) {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		if u := providerURL(llm); u != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_URL", Value: u})
		}
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		cfg := llm.Spec.GoogleCloudVertex
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "LIGHTSPEED_MODEL_PROVIDER", Value: strings.ToLower(string(cfg.ModelProvider))},
			corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_PROJECT", Value: cfg.ProjectID},
			corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_REGION", Value: cfg.Region},
		)
		if u := providerURL(llm); u != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_URL", Value: u})
		}
	case agenticv1alpha1.LLMProviderOpenAI:
		if u := providerURL(llm); u != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_URL", Value: u})
		}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		cfg := llm.Spec.AzureOpenAI
		providerURLValue := cfg.Endpoint
		if u := cfg.URL; u != "" {
			providerURLValue = u
		}
		container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_URL", Value: providerURLValue})
		if cfg.APIVersion != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_API_VERSION", Value: cfg.APIVersion})
		}
	case agenticv1alpha1.LLMProviderAWSBedrock:
		cfg := llm.Spec.AWSBedrock
		container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_REGION", Value: cfg.Region})
		if u := providerURL(llm); u != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER_URL", Value: u})
		}
	}
}

func (b *PodSpecBuilder) buildSkills(skills []agenticv1alpha1.SkillsSource) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(skills) == 0 || skills[0].Image == "" {
		return nil, nil
	}
	s := skills[0]

	vol := corev1.Volume{
		Name: "skills",
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  s.Image,
				PullPolicy: corev1.PullAlways,
			},
		},
	}

	var mounts []corev1.VolumeMount
	if len(s.Paths) > 0 {
		baseMountPath := "/app/skills"
		for _, p := range s.Paths {
			subPath := strings.TrimPrefix(p, "/")
			skillName := path.Base(p)
			mounts = append(mounts, corev1.VolumeMount{
				Name:      "skills",
				MountPath: path.Join(baseMountPath, skillName),
				SubPath:   subPath,
				ReadOnly:  true,
			})
		}
	}

	return []corev1.Volume{vol}, mounts
}

func (b *PodSpecBuilder) buildMCPServers(servers []agenticv1alpha1.MCPServerConfig) ([]corev1.Volume, []corev1.VolumeMount, []corev1.EnvVar, error) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	entries := make([]mcpServerEnvEntry, 0, len(servers))
	for _, s := range servers {
		entry := mcpServerEnvEntry{
			Name:    s.Name,
			URL:     s.URL,
			Timeout: s.TimeoutSeconds,
		}
		for _, h := range s.Headers {
			he := mcpHeaderEnvEntry{
				Name:   h.Name,
				Source: string(h.ValueFrom.Type),
			}
			if h.ValueFrom.Type == agenticv1alpha1.MCPHeaderSourceTypeSecret {
				he.SecretName = h.ValueFrom.Secret.Name
				volName := "mcp-header-" + h.ValueFrom.Secret.Name
				volumes = append(volumes, corev1.Volume{
					Name: volName,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: h.ValueFrom.Secret.Name},
					},
				})
				mounts = append(mounts, corev1.VolumeMount{
					Name:      volName,
					MountPath: mcpHeadersMountRoot + "/" + h.ValueFrom.Secret.Name,
					ReadOnly:  true,
				})
			}
			entry.Headers = append(entry.Headers, he)
		}
		entries = append(entries, entry)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", ErrMarshalMCPServerConfig, err)
	}

	envs := []corev1.EnvVar{{Name: mcpServersEnvVar, Value: string(data)}}
	return volumes, mounts, envs, nil
}

func (b *PodSpecBuilder) buildRequiredSecrets(secrets []agenticv1alpha1.SecretRequirement) ([]corev1.Volume, []corev1.VolumeMount, []corev1.EnvVar) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	var envs []corev1.EnvVar

	for _, s := range secrets {
		switch s.MountAs.Type {
		case agenticv1alpha1.SecretMountFilePath:
			volName := "req-" + s.Name
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: s.Name},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: s.MountAs.FilePath.Path,
				ReadOnly:  true,
			})
		case agenticv1alpha1.SecretMountEnvVar:
			optional := true
			envs = append(envs, corev1.EnvVar{
				Name: s.MountAs.EnvVar.Name,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: s.Name},
						Key:                  "token",
						Optional:             &optional,
					},
				},
			})
		}
	}
	return volumes, mounts, envs
}
