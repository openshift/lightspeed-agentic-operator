# RBAC Model

## Overview

The agentic operator uses a layered RBAC model:
- **Operator RBAC** — what the operator itself can do (static, deployed with the operator)
- **External prerequisites** — admin-created permissions the operator depends on but does **not** create itself. These must be applied as a post-install step (see sections 1 and 2 below):
  - **Shared agent read RBAC** — what verification and escalation sandboxes can read (admin prerequisite)
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

### 1. Shared agent ServiceAccount and read access

**Why:** Verification and escalation pods run as the `lightspeed-agent` ServiceAccount. It must exist and have read permissions. Analysis and execution use separate per-run identities described below.

**What:** A ServiceAccount + ClusterRole + ClusterRoleBinding granting read permissions.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: lightspeed-agent
  namespace: default
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
  namespace: default
```

**Note:** The ServiceAccount is typically included in the SandboxTemplate YAML (see `test/agent/sandboxtemplate/sandboxtemplate.yaml`).

**Scope decision:** Cluster-wide read remains a prerequisite only for shared verification and escalation. Analysis uses a dynamic namespace-scoped identity and never trusts `targetNamespaces` to widen access.

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
  namespace: default
```

**Production alternative:** Scope the ClusterRole to only the resources and verbs agents are expected to request (e.g. `deployments patch`, `configmaps get/update` in specific API groups), rather than `"*"` on everything.


## Dynamic analysis identity (per-AgenticRun)

Before analysis, the operator creates a UID-qualified ServiceAccount in its own namespace and binds it to the built-in `view` ClusterRole only in the AgenticRun namespace. A supplemental Role grants only `pods/log` get. The identity has no ClusterRoleBinding, Secret access, Result write permission, or access to other entries in `spec.targetNamespaces`. It is deleted after analysis and by terminal/finalizer cleanup.

## Dynamic execution RBAC (per-AgenticRun)

Created by the operator during the execution phase, deleted on terminal state or AgenticRun deletion.

| Resource | Name pattern | Scope | Content |
|----------|-------------|-------|---------|
| Role | `ls-exec-<run>` | Per target namespace | Permissions from `analysisResult.options[selected].rbac.namespaceScoped` |
| RoleBinding | `ls-exec-<run>` | Per target namespace | Binds Role to sandbox SA |
| ClusterRole | `ls-exec-cluster-<run>` | Cluster | Permissions from `rbac.clusterScoped` |
| ClusterRoleBinding | `ls-exec-cluster-<run>` | Cluster | Binds ClusterRole to sandbox SA |

Subject: per-run `ServiceAccount ls-exec-{namespace}-{name}` in the operator namespace.

Lifecycle:
- **Created**: just before execution agent call (`ensureExecutionRBAC`)
- **Deleted**: immediately after execution completes (before verification starts). Retries on failure via requeue. Also cleaned up on AgenticRun deletion (finalizer), escalation, or system failure as fallback.

> **Resolved: per-run SA isolation.** Each AgenticRun gets separate analysis and execution ServiceAccounts in the operator namespace. Analysis is read-only and namespace-scoped; execution receives only approved remediation RBAC. Both identities are explicitly deleted when their phase ends. Verification and escalation continue using the shared `lightspeed-agent` SA.

## Agent RBAC per phase

The sandbox ServiceAccount is selected per phase:

| Phase | SA | Read access | Write access | Notes |
|-------|-----|-------------|--------------|-------|
| Analysis | `ls-analysis-{ns}-{name}-{uid-hash}` (per-run) | Built-in `view` plus `pods/log`, only in the AgenticRun namespace | None | `targetNamespaces` cannot widen access |
| Execution | `ls-exec-{ns}-{name}` (per-run) | Inherited from bound Roles | `ls-exec-*` Roles (operator-created) | Agent mutates cluster per remediation plan; isolated SA per AgenticRun |
| Verification | `lightspeed-agent` | Admin-created read access | None | Per-proposal SA deleted after execution; verification has read only |
| Escalation | `lightspeed-agent` | Admin-created read access | None | Agent re-analyzes failure; no mutations |

## Troubleshooting

**Error:** `"is attempting to grant RBAC permissions not currently held"` during execution

**Cause:** The operator SA lacks sufficient permissions to create a Role with the content the analysis agent requested. Kubernetes RBAC escalation prevention blocks it.

**Fix:** Expand the operator's escalation privilege (external prerequisite #2) to include the missing permissions. The error message lists exactly which `{APIGroups, Resources, Verbs}` are needed.

## Security boundaries

| Boundary | Enforced by |
|----------|-------------|
| Agent cannot write during analysis | No write RBAC bound to SA until execution |
| Agent write scope limited to what analysis proposed | Operator creates Roles from `rbac.namespaceScoped`/`clusterScoped` only |
| Write permissions revoked after terminal state | Finalizer calls `cleanupExecutionRBAC` |
| Operator cannot grant permissions it doesn't hold | Kubernetes RBAC escalation prevention (requires admin prerequisite #2) |
