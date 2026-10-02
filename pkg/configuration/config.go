package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SandboxConfig holds sandbox mode and base PodSpec from the ConfigMap.
type SandboxConfig struct {
	Mode    string
	PodSpec *corev1.PodSpec
}

// OTELConfig holds OTEL collector connectivity from the ConfigMap.
type OTELConfig struct {
	CollectorEndpoint string
	AdminEndpoint     string
	CASecretName      string
	CredentialsSecret string
}

// MCPConfig holds MCP server connectivity from the ConfigMap.
// Present only when introspection is enabled on the classic operator.
type MCPConfig struct {
	Endpoint     string
	CASecretName string
}

// RHOKPConfig holds RHOKP (Red Hat Offline Knowledge Portal) connectivity
// from the ConfigMap. Present only when OKP is enabled (not ByokRAGOnly).
type RHOKPConfig struct {
	Endpoint     string
	CASecretName string
}

// TLSConfig holds the normalized TLS handoff values from the ConfigMap.
type TLSConfig struct {
	Profile      string
	MinVersion   string
	CipherSuites string
}

// Config holds the parsed contents of the lightspeed-agentic-configuration
// ConfigMap. Nil means the ConfigMap has not been seen yet.
type Config struct {
	Sandbox                     SandboxConfig
	TLS                         TLSConfig
	AdditionalCAConfigMap       string
	OTEL                        OTELConfig
	MCP                         MCPConfig
	RHOKP                       RHOKPConfig
	ToolOutputInspectionEnabled bool
	TerminalTTLDays             int32
}

// Cache is a thread-safe holder for the parsed ConfigMap contents.
// Starts nil (ConfigMap not yet available). Populated by the configwatch
// handler when the ConfigMap appears, updated on changes, reset to nil
// on deletion.
//
// Components that need to react to config changes (e.g. OTEL provider)
// are registered via SetOTELProvider and invoked from OnConfigMapChange.
type Cache struct {
	config                 atomic.Pointer[Config]
	otelProvider           *Provider
	ForceBareMode          bool
	sandboxClaimsAvailable atomic.Bool
}

// EnableSandboxClaims allows the mode selected by the handoff to take effect
// if Sandbox CRDs appeared after this controller started on OpenShift 4.x.
// ForceBareMode is immutable after construction; this override is safe to
// update alongside the ConfigMap watcher.
func (c *Cache) EnableSandboxClaims() {
	c.sandboxClaimsAvailable.Store(true)
}

// Get returns the current config, or nil if the ConfigMap has not been seen.
func (c *Cache) Get() *Config {
	return c.config.Load()
}

// Available reports whether the ConfigMap has been loaded.
func (c *Cache) Available() bool {
	return c.config.Load() != nil
}

// SetOTELProvider registers the OTEL provider so that OnConfigMapChange
// can reconfigure it when the ConfigMap changes.
func (c *Cache) SetOTELProvider(p *Provider) {
	c.otelProvider = p
}

// OnConfigMapChange is a configwatch.Handler-compatible callback.
// Parses the ConfigMap, updates the cache, and reconfigures registered
// components (OTEL provider, etc.).
func (c *Cache) OnConfigMapChange(ctx context.Context, cm *corev1.ConfigMap) error {
	log := logf.FromContext(ctx)

	if err := c.update(cm); err != nil {
		return err
	}

	if c.otelProvider != nil {
		if err := c.otelProvider.reconfigureFromConfigMap(ctx, cm); err != nil {
			log.Error(err, "OTEL provider reconfiguration failed")
			return err
		}
	}

	return nil
}

// update parses the ConfigMap and stores the result.
func (c *Cache) update(cm *corev1.ConfigMap) error {
	if cm == nil {
		c.config.Store(nil)
		return nil
	}
	cfg, err := parseConfigMap(cm)
	if err != nil {
		return err
	}
	if c.ForceBareMode && !c.sandboxClaimsAvailable.Load() && cfg.Sandbox.Mode != "bare-pod" {
		logf.Log.Info("Sandbox CRDs not installed, overriding sandbox-mode to bare-pod", "requested", cfg.Sandbox.Mode)
		cfg.Sandbox.Mode = "bare-pod"
	}
	c.config.Store(cfg)
	return nil
}

func parseToolOutputInspectionEnabled(data map[string]string) bool {
	raw, ok := data[KeyToolOutputInspectionEnabled]
	if !ok {
		return true
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return enabled
}

func parseTerminalTTLDays(data map[string]string) int32 {
	raw, ok := data[KeyTerminalTTLDays]
	if !ok {
		logf.Log.Info("terminal TTL not configured, using default", "days", DefaultTerminalTTLDays)
		return DefaultTerminalTTLDays
	}
	days, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || days <= 0 {
		logf.Log.Info("invalid terminal TTL, using default", "value", raw, "days", DefaultTerminalTTLDays)
		return DefaultTerminalTTLDays
	}
	return int32(days)
}

func parseConfigMap(cm *corev1.ConfigMap) (*Config, error) {
	cfg := &Config{
		TerminalTTLDays: parseTerminalTTLDays(cm.Data),
		Sandbox: SandboxConfig{
			Mode: cm.Data[KeySandboxMode],
		},
		TLS: TLSConfig{
			Profile:      cm.Data[KeyTLSProfile],
			MinVersion:   cm.Data[KeyTLSMinVersion],
			CipherSuites: cm.Data[KeyTLSCipherSuites],
		},
		AdditionalCAConfigMap: cm.Data[KeyAdditionalCAConfigMap],
		OTEL: OTELConfig{
			CollectorEndpoint: cm.Data[KeyOtelCollectorEndpoint],
			AdminEndpoint:     cm.Data[KeyOtelAdminEndpoint],
			CASecretName:      cm.Data[KeyOtelCASecret],
			CredentialsSecret: cm.Data[KeyOtelCredentialsSecret],
		},
		MCP: MCPConfig{
			Endpoint:     cm.Data[KeyMCPEndpoint],
			CASecretName: cm.Data[KeyMCPCASecret],
		},
		RHOKP: RHOKPConfig{
			Endpoint:     cm.Data[KeyRHOKPEndpoint],
			CASecretName: cm.Data[KeyRHOKPCASecret],
		},
		ToolOutputInspectionEnabled: parseToolOutputInspectionEnabled(cm.Data),
	}

	if podSpecJSON, ok := cm.Data[KeySandboxPodSpec]; ok && podSpecJSON != "" {
		var podSpec corev1.PodSpec
		if err := json.Unmarshal([]byte(podSpecJSON), &podSpec); err != nil {
			return nil, fmt.Errorf("%s: %w", ErrParseSandboxPodSpec, err)
		}
		cfg.Sandbox.PodSpec = &podSpec
	}

	if cfg.Sandbox.Mode == "" {
		cfg.Sandbox.Mode = "bare-pod"
	}

	return cfg, nil
}
