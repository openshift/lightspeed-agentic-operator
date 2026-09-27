package configuration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// testCACertPEM returns a minimal self-signed CA certificate PEM for tests.
func testCACertPEM(t *testing.T) []byte {
	t.Helper()
	certPEM, _ := testTLSCertKeyPEM(t, "test-ca", true)
	return certPEM
}

// testTLSCertKeyPEM returns a self-signed certificate and PKCS8 private key PEM.
func testTLSCertKeyPEM(t *testing.T, cn string, isCA bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         isCA,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestReadFromConfigMap_NotFound(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	cfg, err := ReadFromConfigMap(context.Background(), fc, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when ConfigMap absent")
	}
}

func TestReadFromConfigMap_ParsesAllFields(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			KeyOtelCollectorEndpoint: "otel-collector.ns.svc:4317",
			KeyOtelAdminEndpoint:     "https://otel-collector.ns.svc:8080",
			KeyOtelCASecret:          "my-ca-secret",
			KeyOtelCredentialsSecret: "my-creds-secret",
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cm).Build()

	cfg, err := ReadFromConfigMap(context.Background(), fc, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.CollectorEndpoint != "otel-collector.ns.svc:4317" {
		t.Errorf("CollectorEndpoint = %q", cfg.CollectorEndpoint)
	}
	if cfg.AdminEndpoint != "https://otel-collector.ns.svc:8080" {
		t.Errorf("AdminEndpoint = %q", cfg.AdminEndpoint)
	}
	if cfg.CASecretName != "my-ca-secret" {
		t.Errorf("CASecretName = %q", cfg.CASecretName)
	}
	if cfg.CredentialsSecret != "my-creds-secret" {
		t.Errorf("CredentialsSecret = %q", cfg.CredentialsSecret)
	}
}

func TestReadFromConfigMap_MissingOptionalFields(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			KeyOtelCollectorEndpoint: "collector:4317",
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cm).Build()

	cfg, err := ReadFromConfigMap(context.Background(), fc, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AdminEndpoint != "" {
		t.Errorf("AdminEndpoint should be empty, got %q", cfg.AdminEndpoint)
	}
	if cfg.CASecretName != "" {
		t.Errorf("CASecretName should be empty, got %q", cfg.CASecretName)
	}
	if cfg.CredentialsSecret != "" {
		t.Errorf("CredentialsSecret should be empty, got %q", cfg.CredentialsSecret)
	}
}

func TestParseConfigMap_ToolOutputInspectionEnabled(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		present bool
		want    bool
	}{
		{name: "true", value: "true", present: true, want: true},
		{name: "false", value: "false", present: true, want: false},
		{name: "trimmed mixed case false", value: " False ", present: true, want: false},
		{name: "missing defaults enabled", want: true},
		{name: "empty defaults enabled", value: "", present: true, want: true},
		{name: "malformed defaults enabled", value: "invalid", present: true, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]string{}
			if tc.present {
				data[KeyToolOutputInspectionEnabled] = tc.value
			}
			cfg, err := parseConfigMap(&corev1.ConfigMap{Data: data})
			if err != nil {
				t.Fatalf("parseConfigMap: %v", err)
			}
			if cfg.ToolOutputInspectionEnabled != tc.want {
				t.Fatalf("ToolOutputInspectionEnabled = %v, want %v", cfg.ToolOutputInspectionEnabled, tc.want)
			}
		})
	}
}

func TestParseConfigMap_TLSFields(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			KeyTLSProfile:            "IntermediateType",
			KeyTLSMinVersion:         "VersionTLS12",
			KeyTLSCipherSuites:       `["TLS_AES_128_GCM_SHA256"]`,
			KeyAdditionalCAConfigMap: "custom-ca",
		},
	}

	cfg, err := parseConfigMap(cm)
	if err != nil {
		t.Fatalf("parseConfigMap: %v", err)
	}
	if cfg.TLS.Profile != "IntermediateType" {
		t.Errorf("TLS.Profile = %q", cfg.TLS.Profile)
	}
	if cfg.TLS.MinVersion != "VersionTLS12" {
		t.Errorf("TLS.MinVersion = %q", cfg.TLS.MinVersion)
	}
	if cfg.TLS.CipherSuites != `["TLS_AES_128_GCM_SHA256"]` {
		t.Errorf("TLS.CipherSuites = %q", cfg.TLS.CipherSuites)
	}
	if cfg.AdditionalCAConfigMap != "custom-ca" {
		t.Errorf("AdditionalCAConfigMap = %q", cfg.AdditionalCAConfigMap)
	}
}

func TestParseConfigMap_TLSAdditionalCAConfigMapOmitted(t *testing.T) {
	cfg, err := parseConfigMap(&corev1.ConfigMap{})
	if err != nil {
		t.Fatalf("parseConfigMap: %v", err)
	}
	if cfg.AdditionalCAConfigMap != "" {
		t.Errorf("AdditionalCAConfigMap = %q, want empty", cfg.AdditionalCAConfigMap)
	}
}

func TestParseConfigMap_RHOKPFields(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			KeySandboxMode:   "bare-pod",
			KeyRHOKPEndpoint: "https://lightspeed-rhokp.ns.svc:8443",
			KeyRHOKPCASecret: "lightspeed-agentic-rhokp-ca",
		},
	}
	cfg, err := parseConfigMap(cm)
	if err != nil {
		t.Fatalf("parseConfigMap: %v", err)
	}
	if cfg.RHOKP.Endpoint != "https://lightspeed-rhokp.ns.svc:8443" {
		t.Errorf("RHOKP.Endpoint = %q", cfg.RHOKP.Endpoint)
	}
	if cfg.RHOKP.CASecretName != "lightspeed-agentic-rhokp-ca" {
		t.Errorf("RHOKP.CASecretName = %q", cfg.RHOKP.CASecretName)
	}
}

func TestParseConfigMap_RHOKPOmitted(t *testing.T) {
	cm := &corev1.ConfigMap{
		Data: map[string]string{
			KeySandboxMode: "bare-pod",
		},
	}
	cfg, err := parseConfigMap(cm)
	if err != nil {
		t.Fatalf("parseConfigMap: %v", err)
	}
	if cfg.RHOKP.Endpoint != "" {
		t.Errorf("RHOKP.Endpoint should be empty, got %q", cfg.RHOKP.Endpoint)
	}
	if cfg.RHOKP.CASecretName != "" {
		t.Errorf("RHOKP.CASecretName should be empty, got %q", cfg.RHOKP.CASecretName)
	}
}

func TestCollectorConfig_IsValid(t *testing.T) {
	if (&CollectorConfig{}).IsValid() {
		t.Error("empty config should not request export")
	}
	if !(&CollectorConfig{CollectorEndpoint: "x:4317"}).IsValid() {
		t.Error("config with endpoint should request export")
	}
}

func TestCollectorConfig_Validate(t *testing.T) {
	ca := testCACertPEM(t)

	if err := (*CollectorConfig)(nil).Validate(); err != nil {
		t.Errorf("nil config should be valid (disabled): %v", err)
	}
	if err := (&CollectorConfig{}).Validate(); err != nil {
		t.Errorf("empty config should be valid (disabled): %v", err)
	}

	good := &CollectorConfig{
		CollectorEndpoint: "collector.ns.svc:4317",
		AdminEndpoint:     "https://collector.ns.svc:8080",
		CACert:            ca,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}

	cases := []struct {
		name string
		cfg  *CollectorConfig
	}{
		{
			name: "bad collector endpoint",
			cfg: &CollectorConfig{
				CollectorEndpoint: "not-a-host-port",
				AdminEndpoint:     "https://collector:8080",
				CACert:            ca,
			},
		},
		{
			name: "missing admin",
			cfg: &CollectorConfig{
				CollectorEndpoint: "collector:4317",
				CACert:            ca,
			},
		},
		{
			name: "http admin",
			cfg: &CollectorConfig{
				CollectorEndpoint: "collector:4317",
				AdminEndpoint:     "http://collector:8080",
				CACert:            ca,
			},
		},
		{
			name: "missing ca",
			cfg: &CollectorConfig{
				CollectorEndpoint: "collector:4317",
				AdminEndpoint:     "https://collector:8080",
			},
		},
		{
			name: "bad ca pem",
			cfg: &CollectorConfig{
				CollectorEndpoint: "collector:4317",
				AdminEndpoint:     "https://collector:8080",
				CACert:            []byte("not-a-cert"),
			},
		},
		{
			name: "credentials secret without client material",
			cfg: &CollectorConfig{
				CollectorEndpoint: "collector:4317",
				AdminEndpoint:     "https://collector:8080",
				CACert:            ca,
				CredentialsSecret: "mtls-secret",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	clientCert, clientKey := testTLSCertKeyPEM(t, "client", false)
	withMTLS := &CollectorConfig{
		CollectorEndpoint: "collector:4317",
		AdminEndpoint:     "https://collector:8080",
		CACert:            ca,
		CredentialsSecret: "mtls-secret",
		ClientCert:        clientCert,
		ClientKey:         clientKey,
	}
	if err := withMTLS.Validate(); err != nil {
		t.Fatalf("mTLS config should be valid: %v", err)
	}
}

func TestLoadClientCredentials(t *testing.T) {
	certPEM, keyPEM := testTLSCertKeyPEM(t, "client", false)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mtls-secret", Namespace: "test-ns"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(secret).Build()

	cert, key, err := loadClientCredentials(context.Background(), fc, "test-ns", "mtls-secret")
	if err != nil {
		t.Fatalf("loadClientCredentials: %v", err)
	}
	if !bytes.Equal(cert, certPEM) || !bytes.Equal(key, keyPEM) {
		t.Error("loaded material does not match Secret")
	}

	_, _, err = loadClientCredentials(context.Background(), fc, "test-ns", "missing")
	if err == nil {
		t.Fatal("expected error for missing Secret")
	}
}

func TestCollectorConfig_Equal(t *testing.T) {
	a := &CollectorConfig{
		CollectorEndpoint: "c:4317",
		AdminEndpoint:     "https://c:8080",
		CASecretName:      "ca-secret",
		CACert:            []byte("ca"),
		CredentialsSecret: "sec",
	}
	b := &CollectorConfig{
		CollectorEndpoint: "c:4317",
		AdminEndpoint:     "https://c:8080",
		CASecretName:      "ca-secret",
		CACert:            []byte("ca"),
		CredentialsSecret: "sec",
	}
	if !a.Equal(b) {
		t.Error("identical configs should be equal")
	}
	b.CollectorEndpoint = "other:4317"
	if a.Equal(b) {
		t.Error("different endpoints should not be equal")
	}
	if !(*CollectorConfig)(nil).Equal(nil) {
		t.Error("nil should equal nil")
	}
	if a.Equal(nil) {
		t.Error("non-nil should not equal nil")
	}
}
