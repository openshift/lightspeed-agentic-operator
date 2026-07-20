package telemetry

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ConfigMapName is the well-known name of the ConfigMap created by the
	// lightspeed-operator with Collector connectivity details.
	ConfigMapName = "lightspeed-otel-collector-client"

	// ConfigMap keys
	keyCollectorEndpoint = "collector-endpoint"
	keyAdminEndpoint     = "admin-endpoint"
	keyCACert            = "ca.crt"
	keyCredentialsSecret = "credentials-secret"
)

// CollectorConfig holds the resolved Collector connectivity settings
// read from the lightspeed-operator-managed ConfigMap.
type CollectorConfig struct {
	// CollectorEndpoint is the OTLP gRPC endpoint (e.g., "otel-collector.openshift-lightspeed.svc:4317").
	CollectorEndpoint string

	// AdminEndpoint is the HTTPS admin API endpoint (e.g., "https://otel-collector.openshift-lightspeed.svc:8080").
	AdminEndpoint string

	// CACert is the PEM-encoded CA certificate for TLS connections to the Collector.
	CACert []byte

	// CredentialsSecret is the name of the Secret containing TLS client credentials
	// for authenticating to the Collector (if mTLS is required).
	// The Secret must be kubernetes.io/tls (or equivalent) with tls.crt and tls.key.
	CredentialsSecret string

	// ClientCert and ClientKey are PEM material loaded from CredentialsSecret.
	// Empty when credentials-secret is omitted (CA-only trust).
	ClientCert []byte
	ClientKey  []byte
}

// IsValid reports whether export is requested (collector-endpoint is set).
// An empty ConfigMap is intentionally valid as "telemetry disabled".
func (c *CollectorConfig) IsValid() bool {
	return c != nil && c.CollectorEndpoint != ""
}

// Validate checks CollectorConfig.
// Disabled configs (nil / no collector-endpoint) are valid.
// Enabled configs require host:port collector-endpoint, https admin-endpoint,
// and a parseable ca.crt PEM. When credentials-secret is set, ClientCert and
// ClientKey must already be loaded and form a valid TLS key pair.
func (c *CollectorConfig) Validate() error {
	if c == nil || !c.IsValid() {
		return nil
	}

	host, port, err := net.SplitHostPort(c.CollectorEndpoint)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("%s: collector-endpoint must be host:port", ErrInvalidConfig)
	}

	if c.AdminEndpoint == "" {
		return fmt.Errorf("%s: admin-endpoint is required", ErrInvalidConfig)
	}
	if !strings.HasPrefix(strings.ToLower(c.AdminEndpoint), "https://") {
		return fmt.Errorf("%s: admin-endpoint must use https", ErrInvalidConfig)
	}

	if len(c.CACert) == 0 {
		return fmt.Errorf("%s: ca.crt is required", ErrInvalidConfig)
	}
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(c.CACert) {
		return fmt.Errorf("%s", ErrParseCACert)
	}

	if c.CredentialsSecret != "" {
		if len(c.ClientCert) == 0 || len(c.ClientKey) == 0 {
			return fmt.Errorf("%s: credentials-secret %q requires tls.crt and tls.key", ErrInvalidConfig, c.CredentialsSecret)
		}
		if _, err := tls.X509KeyPair(c.ClientCert, c.ClientKey); err != nil {
			return fmt.Errorf("%s: %w", ErrParseClientCert, err)
		}
	}

	return nil
}

// Equal reports whether c and other describe the same Collector connectivity.
func (c *CollectorConfig) Equal(other *CollectorConfig) bool {
	if c == nil || other == nil {
		return c == other
	}
	return c.CollectorEndpoint == other.CollectorEndpoint &&
		c.AdminEndpoint == other.AdminEndpoint &&
		c.CredentialsSecret == other.CredentialsSecret &&
		bytes.Equal(c.CACert, other.CACert) &&
		bytes.Equal(c.ClientCert, other.ClientCert) &&
		bytes.Equal(c.ClientKey, other.ClientKey)
}

// loadClientCredentials reads tls.crt and tls.key from a Secret in namespace.
func loadClientCredentials(ctx context.Context, c client.Reader, namespace, secretName string) (cert, key []byte, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, secret); err != nil {
		return nil, nil, fmt.Errorf("%s %q: %w", ErrReadCredentialsSecret, secretName, err)
	}
	cert = secret.Data[corev1.TLSCertKey]
	key = secret.Data[corev1.TLSPrivateKeyKey]
	if len(cert) == 0 || len(key) == 0 {
		return nil, nil, fmt.Errorf("%s %q: missing %s or %s", ErrReadCredentialsSecret, secretName, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return cert, key, nil
}

// parseConfigMap extracts CollectorConfig fields from a ConfigMap.
func parseConfigMap(cm *corev1.ConfigMap) *CollectorConfig {
	cfg := &CollectorConfig{
		CollectorEndpoint: cm.Data[keyCollectorEndpoint],
		AdminEndpoint:     cm.Data[keyAdminEndpoint],
		CredentialsSecret: cm.Data[keyCredentialsSecret],
	}
	if ca, ok := cm.Data[keyCACert]; ok {
		cfg.CACert = []byte(ca)
	}
	return cfg
}

// ReadFromConfigMap reads the Collector config from the well-known ConfigMap.
// Returns nil (no error) when the ConfigMap does not exist — telemetry is optional.
func ReadFromConfigMap(ctx context.Context, c client.Reader, namespace string) (*CollectorConfig, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: ConfigMapName, Namespace: namespace}
	if err := c.Get(ctx, key, cm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%s %q: %w", ErrReadConfigMap, ConfigMapName, err)
	}
	return parseConfigMap(cm), nil
}
