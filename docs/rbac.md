# RBAC Model

## Overview

The agentic operator uses a layered RBAC model:
- **Operator RBAC** — what the operator itself can do (static, deployed with the operator)
- **External prerequisites** — admin-created permissions the operator depends on but does **not** create itself. These must be applied as a post-install step (see sections 1 and 2 below):
  - **Agent read RBAC** — what sandbox pods can read (admin prerequisite, all phases)
  - **Operator escalation privilege** — allows the operator to create Roles with arbitrary content

## Operator RBAC (static)

Deployed via `config/rbac/role.yaml` (`make deploy`).

| Resource | Name | Purpose |
|----------|------|---------|
| ServiceAccount | `controller-manager` | Operator identity |
| ClusterRole | `agentic-operator-manager-role` | Operator permissions (CRDs, sandboxes, RBAC management) |
| ClusterRoleBinding | `agentic-operator-manager-rolebinding` | Binds role to SA |

Key permissions:
- Read/write AgenticRuns, AgenticRunApprovals, result CRs
- Create/delete SandboxTemplates, SandboxClaims
- Read Sandboxes (wait for ready)
- Create/delete Roles, RoleBindings, ClusterRoles, ClusterRoleBindings

## External prerequisites

These must be created by a **platform admin** before the operator and agents can function correctly. The operator does not create them — it assumes they exist.

### 1. Agent read access ClusterRoleBindings (all phases)

**Why:** Every sandbox step gets its own per-step ServiceAccount (`ls-{step}-{namespace}-{runUID}`). The operator discovers all `ClusterRoleBinding`s where `lightspeed-agent` is a subject, then adds each per-step SA to those bindings. The `lightspeed-agent` SA serves as the **discovery seed** for reader bindings — no sandbox pod runs as `lightspeed-agent` directly.

**What:** A ServiceAccount + ClusterRole + ClusterRoleBinding granting read permissions. The `lightspeed-agent` SA must exist in the operator namespace and be the reference identity in the ClusterRoleBinding subjects.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: lightspeed-agent
  namespace: openshift-lightspeed
automountServiceAccountToken: false
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: lightspeed-agent-reader
rules:
- apiGroups: ["", "apps", "batch"]
  resources: ["pods", "deployments", "replicasets", "statefulsets", "daemonsets", "events", "configmaps", "services", "jobs"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-reader-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: lightspeed-agent-reader
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: openshift-lightspeed
```

**Note:** The `lightspeed-agent` ServiceAccount is created by the operator at startup (idempotent). It is the discovery seed — all per-step SAs are dynamically added to (and removed from) these ClusterRoleBindings by `SandboxManager.Create` and `Release`.

**Note:** Replace `openshift-lightspeed` with your operator's namespace (`OPERATOR_NAMESPACE`, defaults to `openshift-lightspeed`) — discovery matches CRB subjects on namespace.

**Scope decision:** Cluster-wide read is shown above. Execution Roles are scoped to the namespaces declared by the approved remediation option's namespace-scoped RBAC rules.

**Custom resources are not covered by broad read roles.** Clusters commonly seed the reader binding from OpenShift's `cluster-reader`, an *aggregated* role holding the union of ClusterRoles labelled `aggregate-to-cluster-reader` or `aggregate-to-view`. A CRD's resources are in it only if its operator ships a labelled viewer role, and many do not — so an agent with apparent cluster-wide read still gets `Forbidden` on them.

Grant one with a read-only ClusterRole plus a binding naming `lightspeed-agent`; discovery selects on the subject, so it reaches every per-step SA and nothing else. Do not label the role for aggregation — that widens `cluster-reader` for every principal holding it. OLM v1, which is not aggregated:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: lightspeed-agent-olm-v1-reader
rules:
- apiGroups: ["olm.operatorframework.io"]
  resources: ["clusterextensions", "clustercatalogs"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: lightspeed-agent-olm-v1-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: lightspeed-agent-olm-v1-reader
subjects:
- kind: ServiceAccount
  name: lightspeed-agent
  namespace: openshift-lightspeed
```

### 2. Operator escalation privilege

**Why:** When the operator creates an execution Role granting (e.g.) `configmaps patch` in namespace `staging`, Kubernetes checks: "does the operator SA itself have `configmaps patch` in `staging`?" If not, it rejects the Role creation. This is Kubernetes' built-in escalation prevention — you can't grant permissions you don't hold.

**What:** The operator SA needs permissions **at least as broad** as what agents might ever request.

```yaml
# Development / e2e testing — broad permissions (not for production):
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: agentic-operator-escalation
rules:
- apiGroups: ["", "apps", "batch", "networking.k8s.io"]
  resources: ["*"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: agentic-operator-escalation-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: agentic-operator-escalation
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: openshift-lightspeed
```

**Production alternative:** Scope the ClusterRole to only the resources and verbs agents are expected to request (e.g. `deployments patch`, `configmaps get/update` in specific API groups), rather than `"*"` on everything.

**Include custom resource reads.** The analysis template has the agent derive read rules for custom resources (prerequisite #1), and those land in the execution Role — so the operator must hold them too. The example above covers only core, `apps`, `batch`, and `networking.k8s.io`; a plan reading a CRD outside them hits the escalation error below.


## Dynamic execution RBAC (per-AgenticRun)

Created by the operator during the execution phase, deleted on terminal state or AgenticRun deletion.

| Resource | Name pattern | Scope | Content |
|----------|-------------|-------|---------|
| Role | `ls-exec-<run>` | Per target namespace | Permissions from `analysisResult.options[selected].rbac.namespaceScoped` |
| RoleBinding | `ls-exec-<run>` | Per target namespace | Binds Role to sandbox SA |
| ClusterRole | `ls-exec-cluster-<run>` | Cluster | Permissions from `rbac.clusterScoped` |
| ClusterRoleBinding | `ls-exec-cluster-<run>` | Cluster | Binds ClusterRole to sandbox SA |

Subject: per-step `ServiceAccount ls-execution-{namespace}-{runUID}` in the operator namespace (same SA naming as all other steps, via `sandboxSAName`).

Lifecycle:
- **Created**: by `SandboxManager.Create` when the execution step launches. SA creation, reader CRB subject addition, and execution RBAC (Roles/ClusterRoles) are all encapsulated in `Create`.
- **Deleted**: by `SandboxManager.Release`. The SA and result RBAC are GC'd via owner references when the pod/claim is deleted. Cross-namespace Roles/ClusterRoles are explicitly deleted by `cleanupExecutionRBAC`. Reader CRB subjects are removed by `removeReaderSubject`. Also cleaned up on AgenticRun deletion (via `ReleaseSandboxes` in the finalizer).

> **Resolved: per-step SA isolation.** Every sandbox step gets its own ServiceAccount (`ls-{step}-{namespace}-{runUID}`) in the operator namespace, not just execution. No sandbox pod runs as the shared `lightspeed-agent` SA — it serves only as the discovery seed for reader ClusterRoleBindings. Execution RBAC binds to the execution step's per-step SA. Per-step SAs are cleaned up automatically via GC (owner reference to pod/claim) and reader CRB subjects are removed by `Release`. This eliminates cross-step and cross-run permission bleed. The operator's scoped escalation privilege (external prerequisite #2, `agentic-operator-escalation` ClusterRole) permits creating SAs and Roles within its granted scope — no cluster-admin required.

## Agent RBAC per phase

Every sandbox step gets a unique per-step ServiceAccount via `sandboxSAName(run, step)`.

| Phase | SA | Read access | Write access | Notes |
|-------|-----|-------------|--------------|-------|
| Analysis | `ls-analysis-{namespace}-{runUID}` | Via reader CRBs (inherited from `lightspeed-agent` discovery) | Result CR create/patch only | Agent inspects cluster to diagnose; no mutations |
| Execution | `ls-execution-{namespace}-{runUID}` | Via reader CRBs (inherited from `lightspeed-agent` discovery) | `ls-exec-*` Roles (operator-created) + Result CR create/patch | Agent mutates cluster per remediation plan; isolated SA per step |
| Verification | `ls-verification-{namespace}-{runUID}` | Via reader CRBs (inherited from `lightspeed-agent` discovery) | Result CR create/patch only | Separate SA from execution; read-only cluster access |
| Escalation | `ls-escalation-{namespace}-{runUID}` | Via reader CRBs (inherited from `lightspeed-agent` discovery) | Result CR create/patch only | Agent re-analyzes failure; no mutations |

## Stale reader subject cleanup

Reader CRB subjects are added by `SandboxManager.Create` and removed by `Release`. If `Release` is interrupted (e.g. operator crash), stale subjects may remain in the CRB. These are harmless — the referenced per-step SA is deleted via owner-reference GC, so the stale subject entry points to a non-existent SA with no permissions. The finalizer on `AgenticRun` deletion calls `ReleaseSandboxes`, which re-attempts removal for all steps, providing crash-recovery coverage. A periodic background sweep is not currently implemented; the finalizer path is considered sufficient.

## Troubleshooting

**Error:** `"is attempting to grant RBAC permissions not currently held"` during execution

**Cause:** The operator SA lacks sufficient permissions to create a Role with the content the analysis agent requested. Kubernetes RBAC escalation prevention blocks it.

**Fix:** Expand the operator's escalation privilege (external prerequisite #2) to include the missing permissions. The error message lists exactly which `{APIGroups, Resources, Verbs}` are needed.

**Error:** `cannot get resource "<custom-resource>" in API group "<group>"` — ending `at the cluster scope` or `in the namespace "<ns>"` — for a per-step SA (`ls-anl-…`, `ls-exe-…`, `ls-ver-…`)

**Cause:** The CRD's operator ships no aggregated viewer role, so the resource is absent from `cluster-reader` and from the reader bindings per-step SAs inherit. Broad read access does not imply every custom resource.

**Fix:** Grant it explicitly (prerequisite #1, "Custom resources are not covered by broad read roles"); on spokes this doesn't apply — reader bindings come only from the hub-provisioned `lightspeed-hub:cluster-reader` / `lightspeed-hub:cluster-monitoring-view` (`spokeReaderBindingNames`). For execution only, analysis can instead derive the read into the per-run Role (or ClusterRole for cluster-scope access); the operator must hold it too (prerequisite #2). This path works on spokes, checked against the spoke credentials.

## Security boundaries

| Boundary | Enforced by |
|----------|-------------|
| Agent cannot write during analysis | Per-step SA has only reader CRB access + Result CR create/patch |
| Agent write scope limited to what analysis proposed | Operator creates Roles from `rbac.namespaceScoped`/`clusterScoped` only |
| Write permissions revoked after execution | `SandboxManager.Release` cleans up execution RBAC; GC handles SA |
| Per-step SA isolation | Each step gets a unique SA, eliminating cross-step permission bleed |
| Operator cannot grant permissions it doesn't hold | Kubernetes RBAC escalation prevention (requires admin prerequisite #2) |
