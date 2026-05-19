# LLMProvider TLS Certificate Support

**Date:** 2026-05-19
**Status:** Approved
**Scope:** lightspeed-agentic-operator only (no sandbox changes)

## Problem

When an OpenShift cluster sits behind a corporate proxy that performs TLS
interception, HTTPS connections from sandbox pods to LLM provider endpoints
(Anthropic, Vertex AI, OpenAI, etc.) fail with certificate verification
errors. The proxy's CA is not in the container's system trust store.

Cluster admins need a way to supply an additional CA bundle per LLM provider
so the operator can inject it into sandbox pods.

## Decision

Add an optional `tlsCertificate` field to `LLMProviderSpec` that references a
ConfigMap containing PEM-encoded CA certificates. The operator mounts the
ConfigMap into the sandbox pod and uses an init container to concatenate the
custom CA with the system trust store, then sets `SSL_CERT_FILE` and
`NODE_EXTRA_CA_CERTS` so all HTTPS clients in the pod trust the additional CA.

## Design

### 1. CRD API

#### New type: `ConfigMapReference` (`reference_types.go`)

```go
type ConfigMapReference struct {
    // name of the ConfigMap. Must be a valid RFC 1123 DNS subdomain.
    // +required
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    // +kubebuilder:validation:XValidation:rule="!format.dns1123Subdomain().validate(self).hasValue()",message="must be a valid DNS subdomain: lowercase alphanumeric characters, hyphens, and dots"
    Name string `json:"name,omitempty"`
}
```

#### Modified: `LLMProviderSpec` (`llmprovider_types.go`)

Add a new optional field at the top level of the spec (not inside per-provider
configs), because the CA is about the network path to the provider, not the
provider type:

```go
type LLMProviderSpec struct {
    Type LLMProviderType `json:"type,omitempty"`

    // tlsCertificate optionally references a ConfigMap containing a custom
    // CA bundle for TLS connections to this provider's endpoint. Used when
    // the provider is behind a TLS-intercepting proxy or uses a private CA.
    //
    // The ConfigMap must exist in the operator namespace (openshift-lightspeed)
    // and contain the key "ca-bundle.crt" with PEM-encoded CA certificates.
    // The certificates are appended to the system trust store in the sandbox
    // pod so that all HTTPS clients (Python SDKs, Node.js Claude Code, curl,
    // oc) trust the additional CA.
    //
    // When omitted, the sandbox pod uses only the default system trust store.
    // +optional
    TLSCertificate *ConfigMapReference `json:"tlsCertificate,omitzero"`

    Anthropic         AnthropicConfig         `json:"anthropic,omitzero"`
    GoogleCloudVertex GoogleCloudVertexConfig  `json:"googleCloudVertex,omitzero"`
    OpenAI            OpenAIConfig             `json:"openAI,omitzero"`
    AzureOpenAI       AzureOpenAIConfig        `json:"azureOpenAI,omitzero"`
    AWSBedrock        AWSBedrockConfig         `json:"awsBedrock,omitzero"`
}
```

### 2. Operator Template Patching (`sandbox_templates.go`)

New function `patchTLSCertificate` called from `EnsureAgentTemplate` after
`patchLLMCredentials`, when `llm.Spec.TLSCertificate != nil`.

#### Volumes

| Volume name       | Type      | Source                                               |
|-------------------|-----------|------------------------------------------------------|
| `llm-ca-bundle`   | configMap | ConfigMap named by `tlsCertificate.name`, key `ca-bundle.crt` |
| `llm-ca-combined` | emptyDir  | Shared between init container and main container     |

#### Init container

- **Name:** `merge-ca-bundle`
- **Image:** Same as the first container in the base template (the sandbox image)
- **Command:**
  ```
  ["sh", "-c",
   "cat /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /var/run/secrets/llm-ca/ca-bundle.crt > /tmp/ca-bundle/combined-ca-bundle.crt"]
  ```
- **Volume mounts:**
  - `llm-ca-bundle` → `/var/run/secrets/llm-ca` (readOnly: true)
  - `llm-ca-combined` → `/tmp/ca-bundle`

The system CA path `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem` is the
standard location in UBI/RHEL-based images (which the sandbox uses).

#### Main container modifications

- **Volume mount:** `llm-ca-combined` → `/tmp/ca-bundle` (readOnly: true)
- **Environment variables:**
  - `SSL_CERT_FILE=/tmp/ca-bundle/combined-ca-bundle.crt` — honored by
    Python `ssl`, `requests`, `httpx`, `curl`, `oc`, and most C-based TLS
    libraries
  - `NODE_EXTRA_CA_CERTS=/tmp/ca-bundle/combined-ca-bundle.crt` — honored by
    Node.js (Claude Code). Node appends these to its compiled-in CA store.

#### Template hash

Add the ConfigMap name (or empty string when absent) to `templateHashInput`
so a CA configuration change produces a new derived template and triggers
garbage collection of the old one.

```go
type templateHashInput struct {
    LLM                 agenticv1alpha1.LLMProviderSpec     `json:"llm"`
    Model               string                              `json:"model"`
    Skills              []agenticv1alpha1.SkillsSource      `json:"skills"`
    MCPServers          []agenticv1alpha1.MCPServerConfig   `json:"mcpServers,omitempty"`
    RequiredSecrets     []agenticv1alpha1.SecretRequirement `json:"requiredSecrets,omitempty"`
    Step                string                              `json:"step"`
    BaseResourceVersion string                              `json:"baseRV"`
}
```

No change needed — `LLM` already serializes the full `LLMProviderSpec`, so
adding `TLSCertificate` to the spec struct automatically includes it in the
hash input.

### 3. Example YAML

Admin creates a ConfigMap with the proxy CA:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: corporate-proxy-ca
  namespace: openshift-lightspeed
data:
  ca-bundle.crt: |
    -----BEGIN CERTIFICATE-----
    MIIDxTCCAq2gAwIBAgI...
    -----END CERTIFICATE-----
```

LLMProvider references it:

```yaml
apiVersion: agentic.openshift.io/v1alpha1
kind: LLMProvider
metadata:
  name: anthropic-via-proxy
spec:
  type: Anthropic
  tlsCertificate:
    name: corporate-proxy-ca
  anthropic:
    credentialsSecret:
      name: llm-credentials
    url: https://api.anthropic.com
```

### 4. Testing

| Test                                    | What it verifies                                                     |
|-----------------------------------------|----------------------------------------------------------------------|
| `TestPatchTLSCertificate_AddsVolumes`   | ConfigMap volume + emptyDir volume added to pod spec                |
| `TestPatchTLSCertificate_AddsInitContainer` | Init container present with correct command and mounts           |
| `TestPatchTLSCertificate_SetsEnvVars`   | `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS` set on main container     |
| `TestPatchTLSCertificate_NoOp`          | No TLS certificate → no volumes, no init container, no env vars     |
| `TestTemplateHash_ChangesWithCA`        | Hash differs when `tlsCertificate` is added/changed/removed         |
| `TestEnsureAgentTemplate_WithCA`        | Full integration: base template + CA → derived template is correct  |

## Out of Scope

- **No sandbox-side changes.** CA injection is purely pod-template-level.
- **No cluster-wide CA.** Per-LLMProvider only. A global default can be added
  later as a field on a cluster-scoped config CRD.
- **No cert rotation for running pods.** Sandbox pods are ephemeral. New pods
  pick up ConfigMap changes automatically.
- **No mTLS.** Server CA trust only, not client certificate authentication.
- **No ConfigMap existence validation.** The operator does not verify the
  ConfigMap exists at reconcile time. A missing ConfigMap causes a pod
  scheduling failure, which is the standard Kubernetes behavior and is
  surfaced via pod events.

## Files Changed

| File                                          | Change                                                  |
|-----------------------------------------------|--------------------------------------------------------|
| `api/v1alpha1/reference_types.go`             | Add `ConfigMapReference` type                          |
| `api/v1alpha1/llmprovider_types.go`           | Add `TLSCertificate *ConfigMapReference` to spec       |
| `controller/proposal/sandbox_templates.go`    | Add `patchTLSCertificate`, call from `EnsureAgentTemplate` |
| `controller/proposal/sandbox_templates_test.go` | Add CA patching tests                               |
| `config/crd/bases/` (generated)               | Regenerated via `make manifests`                       |
| `api/v1alpha1/zz_generated.deepcopy.go` (generated) | Regenerated via `make generate`                 |
| `examples/setup/00-llm-providers.yaml`        | Add example with `tlsCertificate`                      |
