package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMetricsTLSHandshake(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "metrics test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "prometheus"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "metrics-client-ca", Namespace: "openshift-monitoring"}, Data: map[string]string{"client-ca.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}},
	).Build()
	clientCA, err := loadMetricsClientCA(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	metricsPolicy := &metricsTLS{roots: clientCA}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "metrics") }))
	servingCert := httptest.NewTLSServer(nil)
	defer servingCert.Close()
	server.TLS = &tls.Config{Certificates: servingCert.TLS.Certificates}
	metricsPolicy.Configure(server.TLS)
	if err := metricsPolicy.OnConfigMapChange(context.Background(), metricsPolicyConfigMap("VersionTLS12", `["ECDHE-RSA-AES128-GCM-SHA256"]`)); err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(server.Certificate())
	for _, tc := range []struct {
		name    string
		certs   []tls.Certificate
		success bool
	}{
		{name: "no client certificate"},
		{name: "untrusted client certificate", certs: server.TLS.Certificates},
		{name: "trusted client certificate", certs: []tls.Certificate{{Certificate: [][]byte{clientDER}, PrivateKey: key}}, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: serverRoots, Certificates: tc.certs, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			response, err := client.Get(server.URL)
			if err != nil {
				if tc.success {
					t.Fatal(err)
				}
				return
			}
			defer response.Body.Close()
			if !tc.success {
				t.Fatal("untrusted client reached metrics endpoint")
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("unexpected response: %s", response.Status)
			}
		})
	}
}

func metricsPolicyConfigMap(version, ciphers string) *corev1.ConfigMap {
	return &corev1.ConfigMap{Data: map[string]string{"tls-profile": "Custom", "tls-min-version": version, "tls-cipher-suites": ciphers}}
}

func TestMetricsTLSHandoff(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	ca := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "metrics-client-ca", Namespace: "openshift-monitoring"}, Data: map[string]string{"client-ca.crt": certPEM}}
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(ca).Build()
	clientCA, err := loadMetricsClientCA(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := &metricsTLS{roots: clientCA}
	base := &tls.Config{}
	policy.Configure(base)
	assertRejected := func() {
		t.Helper()
		if _, err := base.GetConfigForClient(nil); err == nil {
			t.Fatal("expected TLS handshakes to be rejected without valid handoff policy")
		}
	}
	assertRejected()
	for _, version := range []string{"VersionTLS12", "VersionTLS13"} {
		if err := policy.OnConfigMapChange(context.Background(), metricsPolicyConfigMap(version, `["ECDHE-RSA-AES128-GCM-SHA256"]`)); err != nil {
			t.Fatal(err)
		}
		cfg, err := base.GetConfigForClient(nil)
		if err != nil {
			t.Fatal(err)
		}
		want := uint16(tls.VersionTLS12)
		if version == "VersionTLS13" {
			want = tls.VersionTLS13
		}
		if cfg.MinVersion != want || cfg.ClientAuth != tls.RequireAndVerifyClientCert || cfg.ClientCAs == nil {
			t.Fatalf("policy not applied: %+v", cfg)
		}
		if len(cfg.CipherSuites) != 1 || cfg.CipherSuites[0] != tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 {
			t.Fatal("handoff cipher not applied")
		}
		if cfg.GetConfigForClient != nil {
			t.Fatal("returned TLS config must not recurse")
		}
	}
	for _, cm := range []*corev1.ConfigMap{nil, metricsPolicyConfigMap("", ""), metricsPolicyConfigMap("invalid", "[]"), metricsPolicyConfigMap("VersionTLS12", "invalid"), metricsPolicyConfigMap("VersionTLS12", `["unsupported"]`), metricsPolicyConfigMap("VersionTLS13", `["unsupported"]`)} {
		_ = policy.OnConfigMapChange(context.Background(), cm)
		assertRejected()
	}
	if err := policy.OnConfigMapChange(context.Background(), metricsPolicyConfigMap("VersionTLS13", `["TLS_AES_128_GCM_SHA256"]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := base.GetConfigForClient(nil); err != nil {
		t.Fatal("policy did not recover:", err)
	}
}

func TestMetricsTLSConcurrentUpdates(t *testing.T) {
	policy := &metricsTLS{roots: x509.NewCertPool()}
	base := &tls.Config{}
	policy.Configure(base)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(update bool) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if update {
					_ = policy.OnConfigMapChange(context.Background(), metricsPolicyConfigMap("VersionTLS13", `["TLS_AES_128_GCM_SHA256"]`))
					_ = policy.OnConfigMapChange(context.Background(), nil)
				} else {
					cfg, err := base.GetConfigForClient(nil)
					if err == nil && (cfg.MinVersion != tls.VersionTLS13 || cfg.ClientAuth != tls.RequireAndVerifyClientCert) {
						t.Error("inconsistent handshake policy")
					}
				}
			}
		}(i == 0)
	}
	wg.Wait()
}

func TestMetricsTLSInvalidCA(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	for _, data := range []map[string]string{nil, {"client-ca.crt": "invalid"}} {
		reader := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "metrics-client-ca", Namespace: "openshift-monitoring"}, Data: data}).Build()
		if _, err := loadMetricsClientCA(context.Background(), reader); err == nil {
			t.Fatal("expected invalid CA to fail")
		}
	}
}
