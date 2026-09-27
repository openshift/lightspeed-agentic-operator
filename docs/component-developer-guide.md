# Component Developer Guide

How to integrate your product with the OpenShift Lightspeed agentic platform.

## Who is this for?

You are a **component team** (ACS, CVO, CMO, OSSM, or any product team) that wants Lightspeed to automatically analyze, remediate, or advise on issues your product detects. You ship:

1. A **skills image** (OCI) containing Claude Code Skills your agent should use.
2. An **adapter** (webhook, event source, controller) that creates `AgenticRun` CRs at runtime.

You interact with **one CRD**: `AgenticRun`. Everything else (LLM infrastructure, agent tiers, approval policy) is managed by the cluster admin.

## Architecture overview

```
Your Adapter                      Cluster Admin (Day 0)
    |                                  |
    | creates                          | creates
    v                                  v
 AgenticRun ----analysis.agent----> Agent ----> LLMProvider
 (namespaced)                  (cluster-scoped)  (cluster-scoped)
    |                                  |
    | spec.tools                       | creates
    v                                  v
 Skills Image                    ApprovalPolicy
 + Secrets                       (cluster-scoped singleton)
```

Your adapter creates an `AgenticRun` in your namespace. The AgenticRun defines the workflow shape inline (which steps run and which agent handles each step) and provides your domain-specific tools (skills image, secrets). The operator resolves the agent, picks the right LLM provider, and runs the workflow.

The operator also creates an `AgenticRunApproval` resource (1:1 with each AgenticRun) that tracks per-step approval state. Users approve or deny individual steps via the CLI, console, or direct API.

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

This grants your adapter permission to create and manage AgenticRuns in `my-product-namespace`.

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

Your AgenticRun references these by name. AgenticRuns can only reference Secrets in their own namespace — Secrets are namespace-scoped resources and the operator enforces same-namespace references.

### 3. Create an AgenticRun

```yaml
apiVersion: agentic.openshift.io/v1alpha1
kind: AgenticRun
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
  execution:
    agent: default
  verification:
    agent: fast
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

Every step (analysis, execution, verification) has a built-in approval gate. The step does not run until approval is present — either auto-approved via the cluster-wide `ApprovalPolicy` or explicitly approved by the user on the `AgenticRunApproval` resource.

```
                     +---------+
                     | Pending |
                     +----+----+
                          |
                     +----v------+
                     | Analyzing |
                     +----+------+
                          |
                     +----v-----+
                     | Proposed |
                     +----+-----+
                          |
                     +----v------+
                     | Executing |
                     +----+------+
                          |
                     +----v------+
                     | Verifying |
                     +----+------+
                          |
              +-----------+-----------+
              |                       |
        +-----v-----+          +------v------+
        | Completed |          | Escalating  |
        +-----------+          +------+------+
                                      |
                               +------v----+
                               | Escalated |
                               +-----------+

  Any step can also reach a terminal state off this spine:
    Denied  -- an approval was denied on the AgenticRunApproval.
    Failed  -- a step failed to run: an analysis/execution error, or a
               verification *system* failure (agent crash, timeout, or no
               result produced). An *objective* verification failure does
               NOT land here -- it escalates (Verifying -> Escalating ->
               Escalated), never re-executing.
```

- **Pending** -- AgenticRun created, waiting for reconciliation.
- **Analyzing** -- Analysis step approved (or pending approval). Agent is running or waiting.
- **Proposed** -- Analysis complete, awaiting execution approval (`Analyzed=True`, no `Executed` condition yet).
- **Executing** -- Execution step approved (or pending approval). Agent is running or waiting.
- **Denied** -- User denied a step on the AgenticRunApproval. Terminal.
- **Verifying** -- Execution complete (or skipped). Verification step approved (or pending approval).
- **Completed** -- Verification passed. Terminal (success).
- **Failed** -- A step failed. Terminal for analysis/execution failures and for verification *system* failures (crash/timeout/no result). An objective verification failure instead escalates immediately (no retry) -- see **Escalating** / **Escalated**.
- **Escalating** -- Verification failed and the operator is producing an `EscalationResult` (the `Escalated` condition is `Unknown`). In-flight, not terminal -- do not treat it as a final state.
- **Escalated** -- Escalation complete. An `EscalationResult` has been produced with the execution and verification history for a human operator to assess. Terminal.

### Approval flow

When an AgenticRun is created, the operator creates an `AgenticRunApproval` resource (same name, same namespace, owned by the AgenticRun). The `ApprovalPolicy` singleton determines which steps auto-approve.

Users approve steps via CLI:
```bash
# Approve analysis
oc agentic run approve fix-my-issue-123 --stage=analysis

# Approve execution with option selection and agent override
oc agentic run approve fix-my-issue-123 --stage=execution --option=0 --agent=fast

# Approve verification
oc agentic run approve fix-my-issue-123 --stage=verification

# Approve all remaining steps
oc agentic run approve fix-my-issue-123 --all

# Deny a step (terminal for the entire run)
oc agentic run deny fix-my-issue-123 --stage=execution
```

### Watching for completion

Use a standard Kubernetes watch on conditions. Phase is derived from conditions, not stored:

```go
// Go example
watch, _ := client.Watch(ctx, &v1alpha1.AgenticRunList{}, ...)
for event := range watch.ResultChan() {
    run := event.Object.(*v1alpha1.AgenticRun)
    phase := v1alpha1.DerivePhase(run.Status.Conditions)
    switch phase {
    case v1alpha1.AgenticRunPhaseCompleted:
        // Remediation succeeded
    case v1alpha1.AgenticRunPhaseFailed:
        // Inspect run.Status.Steps.*.results (and the referenced Result CRs) for failure details
    case v1alpha1.AgenticRunPhaseEscalating:
        // Verification failed; escalation is in progress (not yet terminal) -- keep watching
    case v1alpha1.AgenticRunPhaseEscalated:
        // Verification failed; an EscalationResult was created for human review
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

When `paths` is omitted, the entire image is mounted. When specified, only those directories are mounted (each as a separate subPath). Use selective paths when your image has many skills but a particular run only needs a subset.

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
- **Default** (or omit entirely) — full schema with diagnosis, remediationPlan, RBAC, verification plan
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

### Run-level tools

Tools are defined once at `spec.tools` and are available to every configured step: analysis, execution, verification, and escalation. Skills, MCP servers, and required secrets are shared across the workflow.

```yaml
spec:
  tools:
    skills:
      - image: registry.redhat.io/acs/lightspeed-skills:latest
        paths: [/skills/acs-remediation, /skills/acs-compliance]
    requiredSecrets:
      - name: acs-api-token
        mountAs:
          type: EnvVar
          envVar:
            name: ACS_API_TOKEN

  analysis:
    agent: smart
  execution:
    agent: default
  verification:
    agent: fast
```

There are no per-step tool overrides. Every configured step receives the same run-level tools and required secrets.

## Workflow shapes

The workflow shape is defined directly on the AgenticRun by including or omitting steps. Analysis is always required. Common patterns:

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
  execution:
    agent: default
  verification:
    agent: fast
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

An adapter is any code that creates `AgenticRun` CRs in response to external events. Common patterns:

### Webhook adapter

Your product fires an HTTP webhook when it detects an issue. The adapter receives the webhook, translates the payload into an AgenticRun, and creates it via the Kubernetes API.

```go
func handleViolation(w http.ResponseWriter, r *http.Request) {
    var violation ACSViolation
    json.NewDecoder(r.Body).Decode(&violation)

    run := &v1alpha1.AgenticRun{
        ObjectMeta: metav1.ObjectMeta{
            GenerateName: "acs-fix-",
            Namespace:    "stackrox",
            Labels: map[string]string{
                "agentic.openshift.io/source": "acs",
            },
        },
        Spec: v1alpha1.AgenticRunSpec{
            Request: formatViolation(violation),
            TargetNamespaces: []string{violation.Namespace},
            Analysis: &v1alpha1.AgenticRunStep{
                Agent: "smart",
            },
            Execution:    &v1alpha1.AgenticRunStep{},
            Verification: &v1alpha1.AgenticRunStep{Agent: "fast"},
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

    client.Create(ctx, run)
}
```

### AlertManager adapter

An AlertManager adapter is an external webhook server that receives AlertManager notifications and creates AgenticRuns. Like any other adapter, it runs independently from the operator — the operator only reconciles the resulting AgenticRun CRs.

### Controller / watch adapter

Write a Kubernetes controller that watches your product's resources and creates AgenticRuns when conditions are met:

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var myResource MyResource
    r.Get(ctx, req.NamespacedName, &myResource)

    if myResource.NeedsRemediation() {
        run := buildAgenticRun(myResource)
        r.Create(ctx, run)
    }
    return ctrl.Result{}, nil
}
```

## Deduplication

The platform does not deduplicate AgenticRuns automatically. Your adapter should implement deduplication to avoid creating multiple runs for the same issue. Common strategies:

- **Name-based:** Use a deterministic name derived from the issue (e.g., `acs-fix-nginx-cve-2024-1234`). Kubernetes will reject the create if an AgenticRun with that name already exists.
- **Label-based:** Before creating, list AgenticRuns with matching labels and check for active (non-terminal) ones.
- **Cooldown:** Track when the last AgenticRun was created for a given issue and skip if within a cooldown window.

## Secret isolation

AgenticRuns can only reference Secrets in their own namespace. An AgenticRun in namespace `stackrox` cannot access a Secret in namespace `openshift-cluster-version`. Secrets are namespace-scoped resources and the operator enforces same-namespace references.

```
stackrox/                          openshift-cluster-version/
  Secret: acs-api-token              Secret: github-token
  AgenticRun: fix-nginx-cve            AgenticRun: upgrade-risk
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

### AgenticRunSpec

```go
type AgenticRunSpec struct {
    // Primary input to the analysis agent.
    // Immutable after creation. Max 32768 chars.
    Request string

    // Namespace(s) this run operates on.
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
    Analysis     *AgenticRunStep  // required
    Execution    *AgenticRunStep
    Verification *AgenticRunStep

    // Mutable fields — the designated mutation points.
    RevisionFeedback string  // Set to feedback text to trigger re-analysis.
    TTLAfterTerminal *int32   // Seconds to keep the run after it reaches a terminal
                              // state before the operator deletes it (0 disables).
                              // Overrides the cluster-wide default; may be pre-set
                              // before the run terminates.
}
```

### AgenticRunStep

```go
type AgenticRunStep struct {
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
    // Must exist in the same namespace as the AgenticRun.
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
    // Name of the Secret (same namespace as the AgenticRun). Required.
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
