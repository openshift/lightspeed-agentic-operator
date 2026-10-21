package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

const ErrMetricsTLS = "failed to configure metrics TLS"

// metricsTLS keeps the effective handoff policy immutable and swaps it atomically
// for new connections. A nil policy fails closed without blocking manager startup.
type metricsTLS struct {
	roots  *x509.CertPool
	policy atomic.Pointer[tls.Config]
}

// loadMetricsClientCA reads the client CA certificate from the metrics-client-ca
// ConfigMap in the openshift-monitoring namespace and returns its trust pool.
func loadMetricsClientCA(ctx context.Context, reader client.Reader) (*x509.CertPool, error) {
	ca := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "openshift-monitoring", Name: "metrics-client-ca"}, ca); err != nil {
		return nil, fmt.Errorf("%s: read metrics-client-ca ConfigMap in openshift-monitoring namespace: %w", ErrMetricsTLS, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ca.Data["client-ca.crt"])) {
		return nil, fmt.Errorf("%s: metrics-client-ca ConfigMap in openshift-monitoring namespace requires valid PEM in client-ca.crt data key", ErrMetricsTLS)
	}
	return roots, nil
}

// Configure is called by the metrics server before it installs its certificate
// watcher. Clone the base at handshake time to retain that watcher's GetCertificate.
func (m *metricsTLS) Configure(base *tls.Config) {
	base.ClientAuth = tls.RequireAndVerifyClientCert
	base.ClientCAs = m.roots
	base.MinVersion = tls.VersionTLS12
	base.NextProtos = []string{"http/1.1"}
	base.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
		policy := m.policy.Load()
		if policy == nil {
			return nil, fmt.Errorf("%s: waiting for valid TLS policy in lightspeed-agentic-configuration ConfigMap", ErrMetricsTLS)
		}
		cfg := base.Clone()
		cfg.GetConfigForClient = nil
		cfg.MinVersion = policy.MinVersion
		cfg.CipherSuites = policy.CipherSuites
		return cfg, nil
	}
}

// OnConfigMapChange updates the metrics TLS policy from the
// lightspeed-agentic-configuration ConfigMap. A deleted ConfigMap or invalid TLS
// policy causes new metrics TLS connections to be rejected.
func (m *metricsTLS) OnConfigMapChange(_ context.Context, cm *corev1.ConfigMap) error {
	if cm == nil {
		m.policy.Store(nil)
		return nil
	}
	policy, err := metricsPolicyConfig(cm.Data)
	if err != nil {
		m.policy.Store(nil)
		return err
	}
	m.policy.Store(policy)
	return nil
}

func metricsPolicyConfig(data map[string]string) (*tls.Config, error) {
	version := metricsTLSVersions[data[configuration.KeyTLSMinVersion]]
	if data[configuration.KeyTLSProfile] == "" || version == 0 {
		return nil, fmt.Errorf("%s: missing or invalid handoff TLS profile/minimum version", ErrMetricsTLS)
	}
	var names []string
	if err := json.Unmarshal([]byte(data[configuration.KeyTLSCipherSuites]), &names); err != nil {
		return nil, fmt.Errorf("%s: decode handoff TLS cipher suites: %w", ErrMetricsTLS, err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: empty handoff TLS cipher suites", ErrMetricsTLS)
	}
	var ciphers []uint16
	recognized := false
	for _, name := range names {
		code, ok := metricsCipherCodes[name]
		if !ok {
			ctrl.Log.WithName("setup").Info("TLS profile cipher is unsupported by Go", "cipher", name)
			continue
		}
		recognized = true
		// TLS 1.3 cipher suites are managed by Go, not tls.Config.CipherSuites.
		if code != tls.TLS_AES_128_GCM_SHA256 && code != tls.TLS_AES_256_GCM_SHA384 && code != tls.TLS_CHACHA20_POLY1305_SHA256 {
			ciphers = append(ciphers, code)
		}
	}
	if !recognized || (version < tls.VersionTLS13 && len(ciphers) == 0) {
		return nil, fmt.Errorf("%s: TLS profile has no supported pre-TLS-1.3 ciphers", ErrMetricsTLS)
	}
	return &tls.Config{MinVersion: version, CipherSuites: ciphers}, nil
}

var metricsTLSVersions = map[string]uint16{
	"VersionTLS10": tls.VersionTLS10,
	"VersionTLS11": tls.VersionTLS11,
	"VersionTLS12": tls.VersionTLS12,
	"VersionTLS13": tls.VersionTLS13,
}

// OpenShift profiles use OpenSSL cipher names rather than Go's IANA names.
var metricsCipherCodes = map[string]uint16{
	"TLS_AES_128_GCM_SHA256":        tls.TLS_AES_128_GCM_SHA256,
	"TLS_AES_256_GCM_SHA384":        tls.TLS_AES_256_GCM_SHA384,
	"TLS_CHACHA20_POLY1305_SHA256":  tls.TLS_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-AES128-GCM-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-AES128-SHA256":     tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-RSA-AES128-SHA256":       tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	"ECDHE-ECDSA-AES128-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	"ECDHE-RSA-AES128-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	"ECDHE-ECDSA-AES256-SHA":        tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	"ECDHE-RSA-AES256-SHA":          tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	"AES128-GCM-SHA256":             tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"AES256-GCM-SHA384":             tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"AES128-SHA256":                 tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
	"AES128-SHA":                    tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	"AES256-SHA":                    tls.TLS_RSA_WITH_AES_256_CBC_SHA,
}
