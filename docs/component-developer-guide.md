# Component Developer Guide

How to integrate your product with the OpenShift Lightspeed agentic platform.

## Who is this for?

You are a **component team** (ACS, CVO, CMO, OSSM, or any product team) that wants Lightspeed to automatically analyze, remediate, or advise on issues your product detects. You ship:

1. A **skills image** (OCI) containing Claude Code Skills your agent should use.
2. An **adapter** (webhook, event source, controller) that creates `Proposal` CRs at runtime.

You interact with **one CRD**: `Proposal`. Everything else (LLM infrastructure, agent tiers, approval policy) is managed by the cluster admin.

## Architecture overview

```
Your Adapter                      Cluster Admin (Day 0)
    |                                  |
    | creates                          | creates
    v                                  v
 Proposal ----analysis.agent----> Agent ----> LLMProvider
 (namespaced)                  (cluster-scoped)  (cluster-scoped)
    |                                  |
    | spec.tools                       | creates
    v                                  v
 Skills Image                    ApprovalPolicy
 + Secrets                       (cluster-scoped singleton)
```

Your adapter creates a `Proposal` in your namespace. The Proposal defines the workflow shape inline (which steps run and which agent handles each step) and provides your domain-specific tools (skills image, secrets). The operator resolves the agent, picks the right LLM provider, and runs the workflow.

The operator also creates a `ProposalApproval` resource (1:1 with each Proposal) that tracks per-step approval state. Users approve or deny individual steps via the CLI, console, or direct API.

You do not need to know how `LLMProvider`, `Agent`, or `ApprovalPolicy` work internally. You reference agents by name string only.

## Quick start

### 1. Get RBAC access

Ask the cluster admin to bind `lightspeed-component-owner` to your adapter's ServiceAccount:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: my-product-lightspeed-access
  namespace: my-product-namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: lightspeed-component-owner
subjects:
  - kind: ServiceAccount
    name: my-product-adapter
    namespace: my-product-namespace
```

This grants your adapter permission to create and manage Proposals in `my-product-namespace`.

### 2. Ask the cluster admin to create runtime secrets

If your agent needs API tokens or credentials at runtime (e.g., an ACS API token, a GitHub PAT), the cluster admin creates these as Kubernetes Secrets in your namespace:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-api-token
  namespace: my-product-namespace
type: Opaque
stringData:
  token: "your-token-here"
```

Your Proposal references these by name. Proposals can only reference Secrets in their own namespace — standard Kubernetes RBAC enforces isolation.

### 3. Create a Proposal

```yaml
apiVersion: agentic.openshift.io/v1alpha1
kind: Proposal
metadata:
  name: fix-my-issue-123
  namespace: my-product-namespace
  labels:
    agentic.openshift.io/source: my-product
spec:
  request: |
    Describe the problem here. Include as much context as possible:
    what happened, what resources are affected, what namespace,
    any error messages or alert details.
  targetNamespaces:
    - affected-namespace
  analysis:
    agent: smart
  execution: {}
  verification:
    agent: fast
  maxAttempts: 3
  tools:
    skills:
      - image: registry.redhat.io/my-product/lightspeed-skills:latest
    requiredSecrets:
      - name: my-api-token
        mountAs:
          type: EnvVar
          envVar:
            name: MY_API_TOKEN
```

That's it. The operator picks it up and runs the workflow.

## Lifecycle

Every step (analysis, execution, verification) has a built-in approval gate. The step does not run until approval is present — either auto-approved via the cluster-wide `ApprovalPolicy` or explicitly approved by the user on the `ProposalApproval` resource.

```
                          +---------+
                          | Pending |
                          +----+----+
                               |
                          +----v------+
                          | Analyzing |
                          +----+------+
                               |
                    +----------+-----------+
                    |                      |
              +-----v------+       +------v----+
              | Executing  |       |  Denied   |
              +-----+------+       +-----------+
                    |
              +-----v------+
              | Verifying  |
              +-----+------+
                    |
          +---------+---------+
          |                   |
    +-----v------+    +------v----+    +-----------+
    | Completed  |    |  Failed   +--->| Escalated |
    +------------+    +-----------+    +-----------+
```

- **Pending** -- Proposal created, waiting for reconciliation.
- **Analyzing** -- Analysis step approved (or pending approval). Agent is running or waiting.
- **Executing** -- Analysis complete. Execution step approved (or pending approval). Agent is running or waiting.
- **Denied** -- User denied a step on the ProposalApproval. Terminal.
- **Verifying** -- Execution complete (or skipped). Verification step approved (or pending approval).
- **Completed** -- Verification passed. Terminal (success).
- **Failed** -- A step failed. May retry (up to `maxAttempts`) or escalate.
- **Escalated** -- Max retries exhausted. A child Proposal is created with failure history.

### Approval flow

When a Proposal is created, the operator creates a `ProposalApproval` resource (same name, same namespace, owned by the Proposal). The `ApprovalPolicy` singleton determines which steps auto-approve.

Users approve steps via CLI:
```bash
# Approve analysis
oc agentic proposal approve fix-my-issue-123 --stage=analysis

# Approve execution with option selection and agent override
oc agentic proposal approve fix-my-issue-123 --stage=execution --option=0 --agent=fast

# Approve verification
oc agentic proposal approve fix-my-issue-123 --stage=verification

# Approve all remaining steps
oc agentic proposal approve fix-my-issue-123 --all

# Deny a step (terminal for the entire proposal)
oc agentic proposal deny fix-my-issue-123 --stage=execution
```

### Watching for completion

Use a standard Kubernetes watch on conditions. Phase is derived from conditions, not stored:

```go
// Go example
watch, _ := client.Watch(ctx, &v1alpha1.ProposalList{}, ...)
for event := range watch.ResultChan() {
    proposal := event.Object.(*v1alpha1.Proposal)
    phase := v1alpha1.DerivePhase(proposal.Status.Conditions)
    switch phase {
    case v1alpha1.ProposalPhaseCompleted:
        // Remediation succeeded
    case v1alpha1.ProposalPhaseFailed:
        // Check proposal.Status.PreviousAttempts for failure details
    case v1alpha1.ProposalPhaseEscalated:
        // Max retries exhausted, child proposal created
    }
}
```

## Tools

The `tools` block tells the operator what to mount into the agent's sandbox pod.

### Skills images

Skills are OCI images containing Claude Code Skills. The operator mounts them as Kubernetes image volumes (requires K8s 1.34+).

```yaml
tools:
  skills:
    # Mount the entire image
    - image: registry.redhat.io/my-product/lightspeed-skills:latest

    # Or selectively mount specific skill directories
    - image: registry.redhat.io/my-product/lightspeed-skills:latest
      paths:
        - /skills/my-skill-a
        - /skills/my-skill-b
```

When `paths` is omitted, the entire image is mounted. When specified, only those directories are mounted (each as a separate subPath). Use selective paths when your image has many skills but a particular proposal only needs a subset.

### Required secrets

Declare secrets the agent needs at runtime. The cluster admin creates the actual Secret objects in your namespace.

```yaml
tools:
  requiredSecrets:
    - name: acs-api-token
      description: "ACS Central API token for querying violations"
      mountAs:
        type: EnvVar
        envVar:
          name: ACS_API_TOKEN

    - name: tls-cert
      description: "Client TLS certificate for mTLS endpoints"
      mountAs:
        type: FilePath
        filePath:
          path: /etc/secrets/tls
```

`mountAs.type` determines how the secret is exposed:
- `EnvVar` — injects the secret value as an environment variable (`envVar.name`).
- `FilePath` — mounts the secret as a file at the given path (`filePath.path`).

### Analysis output

Configure the analysis step's structured output via `spec.analysisOutput`. The `mode` field controls which built-in properties the schema includes:
- **Default** (or omit entirely) — full schema with diagnosis, proposal, RBAC, verification plan
- **Minimal** — base structure only (options array with title); requires `schema` to be set

Optionally define a JSON Schema for adapter-specific structured data injected as a required "components" property:

```yaml
analysisOutput:
  mode: Default      # or "Minimal" for analysis-only with custom shape
  schema:
    type: object
    properties:
      affectedCVEs:
        type: array
        items:
          type: string
      patchedImage:
        type: string
```

### Per-step tools

When different steps need different skills from the same image, use per-step tools. Per-step tools **replace** (not merge with) the shared `spec.tools` for that step.

```yaml
spec:
  # Shared secrets available to all steps
  tools:
    requiredSecrets:
      - name: acs-api-token
        mountAs: ACS_API_TOKEN

  # Analysis gets remediation + compliance skills
  analysis:
    agent: smart
    tools:
      skills:
        - image: registry.redhat.io/acs/lightspeed-skills:latest
          paths: [/skills/acs-remediation, /skills/acs-compliance]

  # Execution gets only remediation skills
  execution:
    tools:
      skills:
        - image: registry.redhat.io/acs/lightspeed-skills:latest
          paths: [/skills/acs-remediation]

  # Verification gets only compliance skills
  verification:
    agent: fast
    tools:
      skills:
        - image: registry.redhat.io/acs/lightspeed-skills:latest
          paths: [/skills/acs-compliance]
```

Note that per-step `tools` replaces the shared `spec.tools` entirely for that step. In the example above, `requiredSecrets` from `spec.tools` are **not** automatically inherited by steps that define their own tools. If a step needs the secret, include it in the step's tools block.

## Workflow shapes

The workflow shape is defined directly on the Proposal by including or omitting steps. Analysis is always required. Common patterns:

| Pattern | Steps | Use when |
|---------|-------|----------|
| Remediation | `analysis` + `execution` + `verification` | Agent should fix the issue directly. |
| Assisted | `analysis` + `verification` (no `execution`) | Agent analyzes and proposes a fix, user applies it (e.g., via GitOps), then approves verification. |
| Advisory | `analysis` only | Agent investigates and reports findings. No execution. |

### Examples

**Remediation** (full pipeline):
```yaml
spec:
  analysis:
    agent: smart
  execution: {}
  verification:
    agent: fast
  maxAttempts: 3
```

**Assisted** (analysis + verification, user applies):
```yaml
spec:
  analysis:
    agent: smart
  verification:
    agent: fast
```

**Advisory** (analysis only):
```yaml
spec:
  analysis:
    agent: smart
```

To discover available agent tiers:

```bash
oc get agents
```

## Writing a good request

The `request` field is the primary input to the analysis agent. The better the context you provide, the better the diagnosis and remediation will be.

**Include:**
- What happened (alert name, error message, violation type).
- Which resources are affected (deployment name, pod name, namespace).
- Relevant metrics or symptoms (error rate, latency, crash count).
- Any constraints ("this namespace is managed by ArgoCD", "do not restart the database").

**Example (ACS violation):**
```yaml
request: |
  ACS policy violation: Fixable CVSS >= 7 (severity: CRITICAL_SEVERITY)
  Policy description: Alert on deployments with fixable vulnerabilities
  Lifecycle stage: DEPLOY
  Affected deployment: staging/nginx-frontend
  Affected images: nginx:1.21
  Violations:
    - CVE-2023-44487 (CVSS 7.5) — HTTP/2 rapid reset attack
    - CVE-2024-24790 (CVSS 9.8) — path traversal
```

**Example (AlertManager):**
```yaml
request: |
  AlertManager alert fired: IstioHighErrorRate (critical)
  Service payment-service in namespace istio-system has 15% error rate
  (5xx responses) over the last 10 minutes.
  Source workload: checkout-frontend
  Labels: severity=critical, destination_service=payment-service
```

## Labels

Use labels to help with filtering and observability:

```yaml
metadata:
  labels:
    # Identify the source system
    agentic.openshift.io/source: acs

    # Your product-specific categorization
    agentic.openshift.io/policy: fixable-cve-critical
    agentic.openshift.io/component: ossm
```

## Building an adapter

An adapter is any code that creates `Proposal` CRs in response to external events. Common patterns:

### Webhook adapter

Your product fires an HTTP webhook when it detects an issue. The adapter receives the webhook, translates the payload into a Proposal, and creates it via the Kubernetes API.

```go
func handleViolation(w http.ResponseWriter, r *http.Request) {
    var violation ACSViolation
    json.NewDecoder(r.Body).Decode(&violation)

    proposal := &v1alpha1.Proposal{
        ObjectMeta: metav1.ObjectMeta{
            GenerateName: "acs-fix-",
            Namespace:    "stackrox",
            Labels: map[string]string{
                "agentic.openshift.io/source": "acs",
            },
        },
        Spec: v1alpha1.ProposalSpec{
            Request: formatViolation(violation),
            TargetNamespaces: []string{violation.Namespace},
            Analysis: &v1alpha1.ProposalStep{
                Agent: "smart",
            },
            Execution:    &v1alpha1.ProposalStep{},
            Verification: &v1alpha1.ProposalStep{Agent: "fast"},
            Tools: v1alpha1.ToolsSpec{
                Skills: []v1alpha1.SkillsSource{{
                    Image: "registry.redhat.io/acs/lightspeed-skills:latest",
                }},
                RequiredSecrets: []v1alpha1.SecretRequirement{{
                    Name: "acs-api-token",
                    MountAs: v1alpha1.SecretMountSpec{
                        Type:   v1alpha1.SecretMountEnvVar,
                        EnvVar: v1alpha1.SecretMountEnvVarConfig{Name: "ACS_API_TOKEN"},
                    },
                }},
            },
        },
    }

    client.Create(ctx, proposal)
}
```

### AlertManager adapter

An AlertManager adapter is an external webhook server that receives AlertManager notifications and creates Proposals. Like any other adapter, it runs independently from the operator — the operator only reconciles the resulting Proposal CRs.

### Controller / watch adapter

Write a Kubernetes controller that watches your product's resources and creates Proposals when conditions are met:

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var myResource MyResource
    r.Get(ctx, req.NamespacedName, &myResource)

    if myResource.NeedsRemediation() {
        proposal := buildProposal(myResource)
        r.Create(ctx, proposal)
    }
    return ctrl.Result{}, nil
}
```

## Deduplication

The platform does not deduplicate Proposals automatically. Your adapter should implement deduplication to avoid creating multiple proposals for the same issue. Common strategies:

- **Name-based:** Use a deterministic name derived from the issue (e.g., `acs-fix-nginx-cve-2024-1234`). Kubernetes will reject the create if a Proposal with that name already exists.
- **Label-based:** Before creating, list proposals with matching labels and check for active (non-terminal) ones.
- **Cooldown:** Track when the last proposal was created for a given issue and skip if within a cooldown window.

## Secret isolation

Proposals can only reference Secrets in their own namespace. A Proposal in namespace `stackrox` cannot access a Secret in namespace `openshift-cluster-version`. This is enforced by standard Kubernetes RBAC — no custom logic required.

```
stackrox/                          openshift-cluster-version/
  Secret: acs-api-token              Secret: github-token
  Proposal: fix-nginx-cve            Proposal: upgrade-risk
    tools.requiredSecrets:             tools.requiredSecrets:
      - acs-api-token    <-- OK          - github-token    <-- OK
      - github-token     <-- DENIED      - acs-api-token   <-- DENIED
```

## What your adapter does NOT do

- **Pick agent tiers.** Your adapter selects agent names (e.g., `smart`, `fast`, `default`) per step. The cluster admin configures what those agents mean (LLM provider, model, timeouts).
- **Manage LLM credentials.** The cluster admin configures LLMProvider and Agent CRs. Your adapter never touches these.
- **Manage approval policy.** The cluster admin configures the ApprovalPolicy singleton. Your adapter never touches it.
- **Create RBAC for execution.** The analysis agent requests RBAC permissions as part of its output. The operator's policy engine validates and creates the actual Kubernetes RBAC resources.
- **Manage the sandbox pod.** The operator creates and manages sandbox pods for each step.

## API reference

### ProposalSpec

```go
type ProposalSpec struct {
    // Primary input to the analysis agent.
    // Immutable after creation. Max 32768 chars.
    Request string

    // Namespace(s) this proposal operates on.
    // Immutable. Used for RBAC scoping. Max 50 namespaces.
    TargetNamespaces []string

    // Default tools for all steps.
    // Immutable. Per-step tools replace this for individual steps.
    Tools ToolsSpec

    // Analysis output configuration (mode + optional custom schema).
    // Mode: Default (full built-in schema) or Minimal (title only).
    // Immutable after creation.
    AnalysisOutput *AnalysisOutput

    // Per-step configuration. Analysis is required.
    // Omit execution to skip it (advisory/assisted).
    // Omit verification to skip it.
    // All immutable after creation.
    Analysis     *ProposalStep  // required
    Execution    *ProposalStep
    Verification *ProposalStep

    // Mutable fields — the designated mutation points.
    MaxAttempts *int32  // Retry limit (patched at approval).
    Revision    *int32  // Increment to trigger re-analysis with feedback.
}
```

### ProposalStep

```go
type ProposalStep struct {
    // Name of the cluster-scoped Agent CR to use for this step.
    // Defaults to "default" when omitted.
    Agent string

    // Per-step tools that replace spec.tools for this step.
    // Use when different steps need different skills.
    Tools *ToolsSpec
}
```

### ToolsSpec

```go
type ToolsSpec struct {
    // OCI images containing skills. Max 20 images.
    Skills []SkillsSource

    // Secrets the sandbox needs at runtime. Max 20 secrets.
    // Must exist in the same namespace as the Proposal.
    RequiredSecrets []SecretRequirement

    // External MCP servers the agent can connect to. Max 20 servers.
    MCPServers []MCPServerConfig
}
```

### SkillsSource

```go
type SkillsSource struct {
    // OCI image reference. Required. Max 512 chars.
    Image string

    // Restrict which directories to mount. When omitted, the
    // entire image is mounted. Max 50 paths.
    Paths []string
}
```

### SecretRequirement

```go
type SecretRequirement struct {
    // Name of the Secret (same namespace as the Proposal). Required.
    Name string

    // How the secret is exposed in the sandbox pod. Required.
    MountAs SecretMountSpec

    // Human-readable explanation for the cluster admin.
    Description string
}

type SecretMountSpec struct {
    // "EnvVar" or "FilePath". Required.
    Type SecretMountType

    // Required when Type is "EnvVar".
    EnvVar SecretMountEnvVarConfig

    // Required when Type is "FilePath".
    FilePath SecretMountFilePathConfig
}
```
