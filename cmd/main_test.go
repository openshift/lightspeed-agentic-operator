package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestManagerOptions(t *testing.T) {
	for _, secure := range []bool{false, true} {
		opts := managerOptions(":8443", ":8081", "test-namespace", true, secure, "/etc/tls/private")
		if !opts.LeaderElection || opts.LeaderElectionNamespace != "test-namespace" || opts.LeaderElectionID != "lightspeed-agentic-operator.agentic.openshift.io" {
			t.Fatal("leader election must use an agentic-specific lease in the operator namespace")
		}
		if opts.LeaderElectionResourceLock != "leases" {
			t.Fatal("leader election must use Lease resources")
		}
		if opts.Metrics.BindAddress != ":8443" || opts.Metrics.SecureServing != secure || opts.Metrics.CertDir != "/etc/tls/private" || opts.Metrics.CertName != "tls.crt" || opts.Metrics.KeyName != "tls.key" {
			t.Fatalf("unexpected metrics options: %+v", opts.Metrics)
		}
		conf := &tls.Config{}
		for _, configure := range opts.Metrics.TLSOpts {
			configure(conf)
		}
		if secure && conf.MinVersion < tls.VersionTLS12 {
			t.Fatal("secure metrics must require TLS 1.2 or later")
		}
	}
	if managerOptions(":8080", ":8081", "local", false, false, "").LeaderElection {
		t.Fatal("local development must be able to disable leader election")
	}
}

func TestSandboxCRDInstalledBoundsStalledDiscovery(t *testing.T) {
	requestStarted := make(chan struct{})
	var firstRequest sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stall := false
		firstRequest.Do(func() {
			stall = true
			close(requestStarted)
		})
		if stall {
			// Release after the expected probe deadline so the old unbounded
			// implementation returns a successful response instead of hanging.
			safetyRelease := time.NewTimer(35 * time.Second)
			defer safetyRelease.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-safetyRelease.C:
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"sandboxclaims.extensions.agents.x-k8s.io"}}`); err != nil {
			t.Errorf("write CRD fixture response: %v", err)
		}
	}))
	defer server.Close()

	type probeResult struct {
		installed bool
		err       error
	}
	result := make(chan probeResult, 1)
	go func() {
		installed, err := sandboxCRDInstalled(&rest.Config{Host: server.URL})
		result <- probeResult{installed: installed, err: err}
	}()

	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("CRD discovery did not reach the HTTP server")
	}

	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	select {
	case got := <-result:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("sandboxCRDInstalled() error = %v, want context deadline exceeded", got.err)
		}
		if got.installed {
			t.Fatal("sandboxCRDInstalled() reported CRDs installed after discovery timed out")
		}
		if !strings.Contains(got.err.Error(), `get Sandbox API CRD "sandboxclaims.extensions.agents.x-k8s.io"`) {
			t.Fatalf("sandboxCRDInstalled() error lost CRD context: %v", got.err)
		}
	case <-deadline.C:
		t.Fatal("sandboxCRDInstalled() did not return after the stalled discovery request")
	}
}
