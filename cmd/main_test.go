package main

import (
	"crypto/tls"
	"testing"
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
