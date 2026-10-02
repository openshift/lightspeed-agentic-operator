package configuration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSandboxClaimModeBecomesAvailableAfterActivation(t *testing.T) {
	cache := &Cache{ForceBareMode: true} // started on 4.x without Sandbox CRDs
	cm := &corev1.ConfigMap{Data: map[string]string{KeySandboxMode: "sandbox-claim"}}
	if err := cache.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if got := cache.Get().Sandbox.Mode; got != "bare-pod" {
		t.Fatalf("mode before Sandbox CRDs = %q, want bare-pod", got)
	}
	cache.EnableSandboxClaims() // CRDs appeared during the 5.0 upgrade
	if err := cache.OnConfigMapChange(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if got := cache.Get().Sandbox.Mode; got != "sandbox-claim" {
		t.Fatalf("mode after completed upgrade = %q, want sandbox-claim", got)
	}
}
