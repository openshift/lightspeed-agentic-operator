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
- Read/write Proposals, ProposalApprovals, result CRs
- Create/delete SandboxTemplates, SandboxClaims
- Read Sandboxes (wait for ready)
- Create/delete Roles, RoleBindings, ClusterRoles, ClusterRoleBindings

## External prerequisites

These must be created by a **platform admin** before the operator and agents can function correctly. The operator does not create them — it assumes they exist.

### 1. Agent ServiceAccount and read access (all phases)

**Why:** The agent pod runs as the `lightspeed-agent` ServiceAccount (referenced in `SandboxTemplate.spec.podTemplate.spec.serviceAccountName`). This SA is the runtime identity for all k8s API calls the agent makes (e.g. `kubectl get pods`, `kubectl patch deployment`). It must exist (or pods fail to start) and must have read permissions bound to it (or agents can't inspect cluster state to diagnose problems).

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

**Scope decision:** Cluster-wide read is shown above. For tighter security, use per-namespace Roles binding only to `targetNamespaces` the Proposal references — but this requires dynamic admin action per namespace.

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


## Dynamic execution RBAC (per-Proposal)

Created by the operator during the execution phase, deleted on terminal state or Proposal deletion.

| Resource | Name pattern | Scope | Content |
|----------|-------------|-------|---------|
| Role | `ls-exec-<proposal>` | Per target namespace | Permissions from `analysisResult.options[selected].rbac.namespaceScoped` |
| RoleBinding | `ls-exec-<proposal>` | Per target namespace | Binds Role to sandbox SA |
| ClusterRole | `ls-exec-cluster-<proposal>` | Cluster | Permissions from `rbac.clusterScoped` |
| ClusterRoleBinding | `ls-exec-cluster-<proposal>` | Cluster | Binds ClusterRole to sandbox SA |

Subject: per-proposal `ServiceAccount ls-exec-{namespace}-{name}` in the operator namespace.

Lifecycle:
- **Created**: just before execution agent call (`ensureExecutionRBAC`)
- **Deleted**: immediately after execution completes (before verification starts). Retries on failure via requeue. Also cleaned up on Proposal deletion (finalizer), escalation, or system failure as fallback.

> **Resolved: per-proposal SA isolation.** Each Proposal in execution phase gets its own ServiceAccount (`ls-exec-{namespace}-{name}`) in the operator namespace. Execution RBAC binds to this per-proposal SA, not the shared `lightspeed-agent`. The per-proposal SA is explicitly deleted after execution completes (before verification). This eliminates cross-Proposal permission bleed — concurrent Proposals cannot share write RBAC. Analysis and verification continue using the shared `lightspeed-agent` SA (read-only). The operator's `cluster-admin` privilege (external prerequisite) allows it to create SAs and Roles with arbitrary content without escalation issues.

## Agent RBAC per phase

The sandbox SA is `lightspeed-agent` (from `SandboxTemplate.spec.podTemplate.spec.serviceAccountName`).

| Phase | SA | Read access | Write access | Notes |
|-------|-----|-------------|--------------|-------|
| Analysis | `lightspeed-agent` | Admin-created ClusterRole (pods, deployments, events, logs, etc.) | None | Agent inspects cluster to diagnose; no mutations |
| Execution | `ls-exec-{ns}-{name}` (per-proposal) | Inherited from bound Roles | `ls-exec-*` Roles (operator-created) | Agent mutates cluster per remediation plan; isolated SA per Proposal |
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
