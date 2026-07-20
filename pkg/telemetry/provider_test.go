package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type testIDGen struct{}

func (t *testIDGen) NewIDs(_ context.Context) (trace.TraceID, trace.SpanID) {
	return trace.TraceID{}, trace.SpanID{}
}
func (t *testIDGen) NewSpanID(_ context.Context, _ trace.TraceID) trace.SpanID {
	return trace.SpanID{}
}

var _ sdktrace.IDGenerator = (*testIDGen)(nil)

func validTestConfig(t *testing.T, endpoint string) *CollectorConfig {
	t.Helper()
	return &CollectorConfig{
		CollectorEndpoint: endpoint,
		AdminEndpoint:     "https://collector:8080",
		CACert:            testCACertPEM(t),
	}
}

func TestProvider_Configure_NilDisables(t *testing.T) {
	p := NewProvider(&testIDGen{})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure(nil): %v", err)
	}
	if p.Config() != nil {
		t.Error("config should be nil after Configure(nil)")
	}
	if err := p.DeleteLogs(context.Background(), "test"); err != nil {
		t.Error("DeleteLogs should no-op when unconfigured")
	}
}

func TestProvider_Configure_WithValidConfig(t *testing.T) {
	p := NewProvider(&testIDGen{})
	cfg := validTestConfig(t, "localhost:4317")
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.Config() == nil {
		t.Fatal("config should not be nil")
	}
	if p.Config().CollectorEndpoint != "localhost:4317" {
		t.Errorf("endpoint = %q", p.Config().CollectorEndpoint)
	}
}

func TestProvider_Configure_InvalidLeavesPrevious(t *testing.T) {
	p := NewProvider(&testIDGen{})
	good := validTestConfig(t, "host1:4317")
	if err := p.Configure(context.Background(), good); err != nil {
		t.Fatalf("Configure good: %v", err)
	}
	tpBefore := p.tp

	bad := &CollectorConfig{
		CollectorEndpoint: "host2:4317",
		AdminEndpoint:     "http://host2:8080", // not https
		CACert:            testCACertPEM(t),
	}
	if err := p.Configure(context.Background(), bad); err == nil {
		t.Fatal("expected validation error")
	}
	if p.tp != tpBefore {
		t.Error("invalid Configure should leave previous TracerProvider in place")
	}
	if p.Config().CollectorEndpoint != "host1:4317" {
		t.Errorf("config = %q, want host1:4317", p.Config().CollectorEndpoint)
	}
}

func TestProvider_Reconfigure(t *testing.T) {
	p := NewProvider(&testIDGen{})

	cfg1 := validTestConfig(t, "host1:4317")
	if err := p.Configure(context.Background(), cfg1); err != nil {
		t.Fatalf("Configure 1: %v", err)
	}

	cfg2 := validTestConfig(t, "host2:4317")
	if err := p.Configure(context.Background(), cfg2); err != nil {
		t.Fatalf("Configure 2: %v", err)
	}

	if p.Config().CollectorEndpoint != "host2:4317" {
		t.Errorf("after reconfigure, endpoint = %q, want host2:4317", p.Config().CollectorEndpoint)
	}
}

func TestProvider_OnConfigMapChange_Valid(t *testing.T) {
	p := NewProvider(&testIDGen{})
	ca := string(testCACertPEM(t))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName},
		Data: map[string]string{
			"collector-endpoint": "collector:4317",
			"admin-endpoint":     "https://collector:8080",
			"ca.crt":             ca,
		},
	}
	if err := p.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("OnConfigMapChange: %v", err)
	}

	if p.Config() == nil {
		t.Fatal("config should be set after OnConfigMapChange")
	}
	if p.Config().CollectorEndpoint != "collector:4317" {
		t.Errorf("endpoint = %q", p.Config().CollectorEndpoint)
	}
}

func TestProvider_OnConfigMapChange_EmptyDisables(t *testing.T) {
	p := NewProvider(&testIDGen{})
	if err := p.Configure(context.Background(), validTestConfig(t, "collector:4317")); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName},
		Data:       map[string]string{},
	}
	if err := p.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("empty ConfigMap should succeed: %v", err)
	}
	if p.Config() != nil {
		t.Error("empty ConfigMap should disable telemetry")
	}
}

func TestProvider_OnConfigMapChange_InvalidDisablesAndErrors(t *testing.T) {
	p := NewProvider(&testIDGen{})
	if err := p.Configure(context.Background(), validTestConfig(t, "collector:4317")); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName},
		Data: map[string]string{
			"collector-endpoint": "collector:4317",
			"admin-endpoint":     "http://collector:8080",
			"ca.crt":             string(testCACertPEM(t)),
		},
	}
	err := p.OnConfigMapChange(context.Background(), cm)
	if err == nil {
		t.Fatal("expected error for invalid ConfigMap")
	}
	if p.Config() != nil {
		t.Error("invalid ConfigMap should disable export (not keep old)")
	}
}

func TestProvider_OnConfigMapChange_Nil(t *testing.T) {
	p := NewProvider(&testIDGen{})

	if err := p.Configure(context.Background(), validTestConfig(t, "x:4317")); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if err := p.OnConfigMapChange(context.Background(), nil); err != nil {
		t.Fatalf("OnConfigMapChange(nil): %v", err)
	}

	if p.Config() != nil {
		t.Error("config should be nil after nil ConfigMap change")
	}
}

func TestProvider_OnConfigMapChange_UnchangedSkipsReconfigure(t *testing.T) {
	p := NewProvider(&testIDGen{})
	ca := string(testCACertPEM(t))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName},
		Data: map[string]string{
			"collector-endpoint": "collector:4317",
			"admin-endpoint":     "https://collector:8080",
			"ca.crt":             ca,
		},
	}
	if err := p.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("first change: %v", err)
	}
	tpAfterFirst := p.tp

	if err := p.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("second change: %v", err)
	}
	if p.tp != tpAfterFirst {
		t.Error("identical ConfigMap update should not rebuild TracerProvider")
	}
}

func TestProvider_DeleteLogs_NilConfig(t *testing.T) {
	p := NewProvider(&testIDGen{})
	err := p.DeleteLogs(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("DeleteLogs with nil config should return nil, got: %v", err)
	}
}

func TestProvider_OnConfigMapChange_LoadsClientCredentials(t *testing.T) {
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

	p := NewProvider(&testIDGen{})
	p.SetSecretSource(fc, "test-ns")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			"collector-endpoint": "collector:4317",
			"admin-endpoint":     "https://collector:8080",
			"ca.crt":             string(testCACertPEM(t)),
			"credentials-secret": "mtls-secret",
		},
	}
	if err := p.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatalf("OnConfigMapChange: %v", err)
	}
	cfg := p.Config()
	if cfg == nil {
		t.Fatal("expected config")
	}
	if len(cfg.ClientCert) == 0 || len(cfg.ClientKey) == 0 {
		t.Fatal("client cert/key should be loaded from Secret")
	}
}

func TestProvider_OnConfigMapChange_MissingCredentialsSecretDisables(t *testing.T) {
	p := NewProvider(&testIDGen{})
	p.SetSecretSource(fake.NewClientBuilder().WithScheme(testScheme()).Build(), "test-ns")
	if err := p.Configure(context.Background(), validTestConfig(t, "collector:4317")); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName},
		Data: map[string]string{
			"collector-endpoint": "collector:4317",
			"admin-endpoint":     "https://collector:8080",
			"ca.crt":             string(testCACertPEM(t)),
			"credentials-secret": "missing-secret",
		},
	}
	if err := p.OnConfigMapChange(context.Background(), cm); err == nil {
		t.Fatal("expected error when credentials Secret is missing")
	}
	if p.Config() != nil {
		t.Error("missing credentials Secret should disable export")
	}
}

func TestProvider_Configure_WithClientCert(t *testing.T) {
	certPEM, keyPEM := testTLSCertKeyPEM(t, "client", false)
	p := NewProvider(&testIDGen{})
	cfg := validTestConfig(t, "localhost:4317")
	cfg.CredentialsSecret = "mtls"
	cfg.ClientCert = certPEM
	cfg.ClientKey = keyPEM
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure with mTLS: %v", err)
	}
	if p.Config() == nil || len(p.Config().ClientCert) == 0 {
		t.Fatal("mTLS config should be active")
	}
}
