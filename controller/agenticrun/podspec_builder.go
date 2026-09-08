package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

const (
	ErrBuildBasePodSpec       = "base PodSpec with at least one container is required"
	ErrBuildAgentRequired     = "agent is required"
	ErrBuildLLMRequired       = "LLMProvider is required"
	ErrBuildSARequired        = "serviceAccount is required"
	ErrBuildInputCMRequired   = "inputConfigMapName is required"
	ErrBuildMCPServers        = "build MCP servers"
	ErrMarshalMCPServerConfig = "marshal MCP server config"

	llmCredsMountPath        = "/var/run/secrets/llm-credentials"
	llmCredsVolumeName       = "llm-credentials"
	inputConfigMapVolumeName = "input"
	mcpHeadersMountRoot      = "/var/secrets/mcp"
	mcpServersEnvVar         = "LIGHTSPEED_MCP_SERVERS"

	LabelStep      = "agentic.openshift.io/step"
	LabelRun       = "agentic.openshift.io/run"
	LabelComponent = "agentic.openshift.io/component"

	AnnotationRunName = "agentic.openshift.io/run-name"
)

type mcpServerEnvEntry struct {
	Name    string              `json:"name"`
	URL     string              `json:"url"`
	Timeout int32               `json:"timeout,omitempty"`
	Headers []mcpHeaderEnvEntry `json:"headers,omitempty"`
}

type mcpHeaderEnvEntry struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	SecretName string `json:"secretName,omitempty"`
}

// PodSpecBuilder overlays agent-specific configuration (env vars, volumes)
// onto a base PodSpec provided by the configuration cache.
type PodSpecBuilder struct{}

// Build takes a base PodSpec (from the configuration cache) and overlays
// agent, LLM, tools, and OTEL configuration for the given step.
// inputConfigMapName is mounted read-only at /input/ (OLS-3794 batch model).
// The base PodSpec must contain at least one container (the agent container).
// HTTP readiness/liveness probes are not set — batch sandboxes have no HTTP server.
func (b *PodSpecBuilder) Build(
	base *corev1.PodSpec,
	agent *agenticv1alpha1.Agent,
	llm *agenticv1alpha1.LLMProvider,
	tools *agenticv1alpha1.ToolsSpec,
	otelCfg *configuration.OTELConfig,
	rhokpCfg *configuration.RHOKPConfig,
	step string,
	runUID string,
	serviceAccount string,
	inputConfigMapName string,
	traceparent string,
	agentTimeoutSeconds int64,
) (*corev1.PodSpec, error) {
	if base == nil || len(base.Containers) == 0 {
		return nil, fmt.Errorf("%s", ErrBuildBasePodSpec)
	}
	if agent == nil {
		return nil, fmt.Errorf("%s", ErrBuildAgentRequired)
	}
	if llm == nil {
		return nil, fmt.Errorf("%s", ErrBuildLLMRequired)
	}
	if serviceAccount == "" {
		return nil, fmt.Errorf("%s", ErrBuildSARequired)
	}
	if inputConfigMapName == "" {
		return nil, fmt.Errorf("%s", ErrBuildInputCMRequired)
	}

	podSpec := base.DeepCopy()
	podSpec.ServiceAccountName = serviceAccount
	podSpec.AutomountServiceAccountToken = ptr.To(true)
	podSpec.RestartPolicy = corev1.RestartPolicyNever

	container := &podSpec.Containers[0]
	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	var volumes []corev1.Volume

	container.Env = append(container.Env,
		corev1.EnvVar{Name: "LIGHTSPEED_PROVIDER", Value: providerTypeString(llm.Spec.Type)},
		corev1.EnvVar{Name: "LIGHTSPEED_MODEL", Value: agent.Spec.Model},
	)
	b.addProviderSpecificEnv(container, llm)

	if len(agent.Spec.ReasoningConfig) > 0 {
		rcJSON, err := json.Marshal(agent.Spec.ReasoningConfig)
		if err != nil {
			return nil, fmt.Errorf("marshal reasoningConfig: %w", err)
		}
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "LIGHTSPEED_REASONING_CONFIG",
			Value: string(rcJSON),
		})
	}

	if agentTimeoutSeconds > 0 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "LIGHTSPEED_AGENT_TIMEOUT_SECONDS",
			Value: fmt.Sprintf("%d", agentTimeoutSeconds),
		})
	}
	if agent.Spec.MaxTurns > 0 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "LIGHTSPEED_AGENT_MAX_TURNS",
			Value: fmt.Sprintf("%d", agent.Spec.MaxTurns),
		})
	}

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

	// Batch input ConfigMap (sandbox-execution.md rule 7). No HTTP probes —
	// the sandbox is a batch executor, not an HTTP service (rule 6).
	volumes = append(volumes, corev1.Volume{
		Name: inputConfigMapVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: inputConfigMapName},
			},
		},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      inputConfigMapVolumeName,
		MountPath: inputConfigMapMountPath,
		ReadOnly:  true,
	})

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

	appendOTELEnvVars(container, &volumes, otelCfg, runUID)
	appendRHOKPEnvVars(container, &volumes, rhokpCfg)

	podSpec.Volumes = mergeVolumes(podSpec.Volumes, volumes)
	container.VolumeMounts = mergeVolumeMounts(container.VolumeMounts)

	appendAuditEnvVars(container)
	appendTraceEnvVars(container, traceparent)

	return podSpec, nil
}

// mergeVolumes combines base and overlay volumes. When both contain a volume
// with the same name, the overlay entry wins (e.g. a generated skills image
// volume overrides a placeholder in the base).
func mergeVolumes(base, overlay []corev1.Volume) []corev1.Volume {
	byName := make(map[string]int, len(base))
	merged := make([]corev1.Volume, len(base))
	copy(merged, base)
	for i, v := range merged {
		byName[v.Name] = i
	}
	for _, v := range overlay {
		if idx, exists := byName[v.Name]; exists {
			merged[idx] = v
		} else {
			byName[v.Name] = len(merged)
			merged = append(merged, v)
		}
	}
	return merged
}

// mergeVolumeMounts deduplicates mounts by mountPath, keeping the last entry
// so generated mounts override base entries.
func mergeVolumeMounts(mounts []corev1.VolumeMount) []corev1.VolumeMount {
	byPath := make(map[string]int, len(mounts))
	var out []corev1.VolumeMount
	for _, m := range mounts {
		if idx, exists := byPath[m.MountPath]; exists {
			out[idx] = m
		} else {
			byPath[m.MountPath] = len(out)
			out = append(out, m)
		}
	}
	return out
}

func appendAuditEnvVars(container *corev1.Container) {
	container.Env = append(container.Env, corev1.EnvVar{Name: "LIGHTSPEED_AUDIT_ENABLED", Value: "true"})
}

func appendTraceEnvVars(container *corev1.Container, traceparent string) {
	if traceparent == "" {
		return
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: traceparentEnvVar, Value: traceparent})
}

const (
	otelCAVolumeName = "otel-ca"
	otelCAMountPath  = "/var/run/secrets/otel-ca"
	otelCASecretKey  = "otel-ca.crt"
)

func appendOTELEnvVars(container *corev1.Container, volumes *[]corev1.Volume, otelCfg *configuration.OTELConfig, runUID string) {
	if otelCfg == nil || otelCfg.CollectorEndpoint == "" {
		return
	}
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: otelCfg.CollectorEndpoint},
		corev1.EnvVar{Name: "LIGHTSPEED_AGENTICRUN_UID", Value: runUID},
	)

	if otelCfg.CASecretName != "" {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_CERTIFICATE", Value: otelCAMountPath + "/" + otelCASecretKey},
		)
		*volumes = append(*volumes, corev1.Volume{
			Name: otelCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: otelCfg.CASecretName},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      otelCAVolumeName,
			MountPath: otelCAMountPath,
			ReadOnly:  true,
		})
	}
}

const (
	rhokpCAVolumeName = "rhokp-ca"
	rhokpCAMountPath  = "/var/run/secrets/rhokp-ca"
	rhokpCASecretKey  = "rhokp-ca.crt"
)

func appendRHOKPEnvVars(container *corev1.Container, volumes *[]corev1.Volume, rhokpCfg *configuration.RHOKPConfig) {
	if rhokpCfg == nil || rhokpCfg.Endpoint == "" {
		return
	}
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "LIGHTSPEED_RHOKP_ENDPOINT", Value: rhokpCfg.Endpoint},
	)

	if rhokpCfg.CASecretName != "" {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "LIGHTSPEED_RHOKP_CA_CERTIFICATE", Value: rhokpCAMountPath + "/" + rhokpCASecretKey},
		)
		*volumes = append(*volumes, corev1.Volume{
			Name: rhokpCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: rhokpCfg.CASecretName},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      rhokpCAVolumeName,
			MountPath: rhokpCAMountPath,
			ReadOnly:  true,
		})
	}
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
	workdirVol := corev1.Volume{
		Name:         "skills-workdir",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}

	mounts := []corev1.VolumeMount{{
		Name:      "skills-workdir",
		MountPath: "/app/skills/.agents",
	}}
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

	return []corev1.Volume{vol, workdirVol}, mounts
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

func credentialsSecretName(llm *agenticv1alpha1.LLMProvider) string {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		return llm.Spec.Anthropic.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return llm.Spec.GoogleCloudVertex.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderOpenAI:
		return llm.Spec.OpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return llm.Spec.AzureOpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return llm.Spec.AWSBedrock.CredentialsSecret.Name
	default:
		return ""
	}
}

func providerURL(llm *agenticv1alpha1.LLMProvider) string {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		return llm.Spec.Anthropic.URL
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return llm.Spec.GoogleCloudVertex.URL
	case agenticv1alpha1.LLMProviderOpenAI:
		return llm.Spec.OpenAI.URL
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return llm.Spec.AzureOpenAI.URL
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return llm.Spec.AWSBedrock.URL
	default:
		return ""
	}
}

const traceparentEnvVar = "TRACEPARENT"

// traceparentFromContext formats the active span in ctx as a W3C traceparent
// value for sandbox pod env injection. Returns empty when no valid span is present.
func traceparentFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID().String(), sc.SpanID().String(), byte(sc.TraceFlags()))
}

func providerTypeString(t agenticv1alpha1.LLMProviderType) string {
	switch t {
	case agenticv1alpha1.LLMProviderAnthropic:
		return "anthropic"
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return "vertex"
	case agenticv1alpha1.LLMProviderOpenAI:
		return "openai"
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return "azure"
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return "bedrock"
	default:
		return strings.ToLower(string(t))
	}
}
