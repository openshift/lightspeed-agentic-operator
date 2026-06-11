# OLS-3166: Admission-Time Proposal Blocking via ValidatingAdmissionPolicy

Jira: [OLS-3166](https://redhat.atlassian.net/browse/OLS-3166)
Date: 2026-06-11
Status: Spike findings + recommended design

## Problem

When `AgenticOLSConfig.spec.suspended=true`, the current reconciler guard
(OLS-3162) accepts new `Proposal` CRs at the API server and then immediately
terminates them with `EmergencyStopped` on the next reconcile loop. This means
proposals briefly exist as `Pending` before being killed. Callers don't get
immediate rejection feedback — they must watch the proposal to discover it was
stopped.

**Requirement**: Proposals must never be created while the system is suspended.
Callers must receive an immediate admission rejection error.

## Approaches Evaluated

### A — Reconciler guard only (current, eliminated)

Already implemented (OLS-3162). Proposals are accepted then killed. Insufficient
because proposals briefly exist in `Pending` phase before termination.

### B — Validating admission webhook in operator

A `ValidatingWebhookConfiguration` served by the operator pod intercepts
`CREATE` on `Proposal` resources, fetches `AgenticOLSConfig`, and rejects if
suspended.

**Pros**: Maximum flexibility for arbitrary validation logic.

**Cons**:
- **Availability blast radius**: During operator restarts/upgrades the webhook
  server is unavailable. `failurePolicy: Fail` blocks all proposal creation
  when operator is down; `failurePolicy: Ignore` bypasses the check entirely.
- Certificate management via OLM (self-signed CA, mount paths, rotation).
- OLM packaging: CSV `webhookDefinitions`, `deploymentName` matching.
- Bootstrap deadlock risk: must exclude operator namespace via
  `namespaceSelector`.
- Greenfield: zero existing webhook infrastructure in this operator.
- ~5–8 days LOE for what amounts to checking a single boolean.

### C — CRD-level CEL (eliminated)

CRD `x-kubernetes-validations` rules are scoped to the current object only. They
cannot reference external resources like `AgenticOLSConfig`. Confirmed by
Kubernetes documentation: "no cross-object or stateful validation rules are
supported."

### D — ValidatingAdmissionPolicy with `paramRef` (recommended)

Kubernetes-native `ValidatingAdmissionPolicy` (GA since K8s 1.30, OpenShift
4.17+) blocks `Proposal` creation at admission time using a CEL expression that
reads `AgenticOLSConfig` via `paramRef`. No webhook server, no certificates, no
operator involvement at admission time.

**Pros**:
- Runs in the API server process — zero latency, zero blast radius from operator
  downtime.
- No TLS certificates, no webhook server, no OLM `webhookDefinitions`.
- Trivial to implement: two static YAML manifests.
- Already used by other OpenShift components (e.g., `aws-cluster-api-controllers`
  uses VAP for `AWSCluster` validation).
- Combined with existing reconciler guard = defense-in-depth.

**Cons**:
- Requires OpenShift 4.17+ (confirmed as minimum supported version).
- CEL can only read the `paramRef` resource — not suitable for arbitrary
  cross-cluster lookups (sufficient for this use case).

**LOE**: ~2 days.

### E — External policy engine (Kyverno/Gatekeeper) (eliminated)

Cannot assume any policy engine is installed on target clusters.

## Recommended Design: ValidatingAdmissionPolicy + Reconciler Guard

### VAP Manifests

Two static resources installed alongside CRDs:

**ValidatingAdmissionPolicy**:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: agentic.openshift.io-proposal-suspension
spec:
  paramKind:
    apiVersion: agentic.openshift.io/v1alpha1
    kind: AgenticOLSConfig
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["agentic.openshift.io"]
        apiVersions: ["v1alpha1"]
        operations: ["CREATE"]
        resources: ["proposals"]
  validations:
    - expression: "!has(params.spec.suspended) || !params.spec.suspended"
      messageExpression: "'Proposal creation is blocked: agentic system is suspended (AgenticOLSConfig.spec.suspended=true)'"
```

**ValidatingAdmissionPolicyBinding**:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: agentic.openshift.io-proposal-suspension
spec:
  policyName: agentic.openshift.io-proposal-suspension
  validationActions: [Deny]
  paramRef:
    name: cluster
    parameterNotFoundAction: Allow
```

Key decisions:

| Decision | Rationale |
|----------|-----------|
| `operations: ["CREATE"]` only | Updates to existing proposals must not be blocked; the reconciler manages lifecycle transitions. |
| `parameterNotFoundAction: Allow` | If no `AgenticOLSConfig` CR exists, proposals are allowed. Matches existing absence semantics (spec rule 3: absence = `suspended=false`). |
| `failurePolicy: Fail` | If the policy itself is misconfigured, err on the side of blocking. Safe direction for an emergency mechanism. |

### Defense-in-Depth

The existing reconciler guard (OLS-3162, spec rules 12–15) remains unchanged.
Two independent enforcement layers:

| Layer | When | What it catches |
|-------|------|-----------------|
| VAP (primary) | Admission time — before CR is persisted | New `Proposal` CREATE while suspended |
| Reconciler guard (fallback) | Reconcile loop — after CR exists | Race conditions during suspension toggle, proposals that existed before suspension, VAP removal/misconfiguration |

No code changes to the reconciler.

### File Layout

```
config/
├── admission/
│   ├── kustomization.yaml
│   ├── proposal-suspension-policy.yaml
│   └── proposal-suspension-binding.yaml
├── default/
│   └── kustomization.yaml                   # updated: includes ../admission
└── crd/bases/                               # unchanged
```

`config/admission/kustomization.yaml` lists the two YAML files as resources.
`config/default/kustomization.yaml` includes `../admission` alongside `../crd`
and `../rbac`. The quickstart `install.sh` script applies these manifests via
the existing kustomize path.

### RBAC

No additional RBAC required. The VAP and binding are cluster-level resources
installed by the cluster admin (or OLM) during operator installation. The
operator service account does not create or manage them at runtime.

### Spec Changes

New rules added to `.ai/spec/what/system-config.md` under a new
**Admission-Time Blocking** section:

- **Rule 23**: While `suspended=true`, the API server MUST reject `Proposal`
  CREATE requests at admission time with a clear error message. Enforced via a
  `ValidatingAdmissionPolicy` with `paramRef` to `AgenticOLSConfig`.
- **Rule 24**: If no `AgenticOLSConfig` CR exists, admission MUST allow proposal
  creation. Consistent with rule 3 (absence = `suspended=false`). Enforced via
  `parameterNotFoundAction: Allow`.
- **Rule 25**: The admission policy MUST only intercept `CREATE` operations on
  `proposals`. Updates to existing proposals MUST NOT be blocked.
- **Rule 26**: The reconciler guard (rules 12–15) MUST remain as
  defense-in-depth. The admission policy is primary enforcement; the reconciler
  guard is fallback.
- **Rule 27**: The admission policy and binding are static manifests installed
  alongside CRDs. They require no runtime management by the operator.

### E2E Test Coverage

New test in `test/e2e/suspension_test.go`:

**"proposal creation is rejected while suspended"**: With
`AgenticOLSConfig.spec.suspended=true` already active, attempt
`c.Create(ctx, proposal)` and assert it returns an admission rejection error
(status 403/422 with message containing "suspended"). Verify no `Proposal` CR
was created.

Complements existing tests which verify reconciler-based termination of
in-flight proposals.

## Spike Questions — Answers

| # | Question | Answer |
|---|----------|--------|
| 1 | Validating admission webhook blast radius? | Significant. `failurePolicy: Fail` blocks all proposal creation when operator is down (restarts, upgrades). `failurePolicy: Ignore` defeats the purpose. Certificate rotation adds operational surface. OLM packaging requires CSV `webhookDefinitions`. Not recommended. |
| 2 | Can CRD-level CEL reference another resource? | No. CRD `x-kubernetes-validations` rules are scoped to the current object only. Confirmed by K8s docs. |
| 3 | Other K8s-native patterns? | **ValidatingAdmissionPolicy** with `paramRef` (recommended). GA since K8s 1.30, OpenShift 4.17+. Runs in API server, references `AgenticOLSConfig` directly. No external dependencies. |
| 4 | Hybrid approach value? | Yes. VAP for admission-time blocking (primary) + reconciler guard for defense-in-depth (fallback). Both are independent, low-cost to maintain. |
| 5 | LOE for webhooks? | ~5–8 days (greenfield webhook infra, TLS, OLM CSV, upgrade testing). **VAP approach: ~2 days.** |

## LOE Estimate (Recommended Approach)

| Task | Effort |
|------|--------|
| Two YAML manifests + kustomization | 0.5 day |
| Kustomize wiring (`config/default/`, quickstart) | 0.5 day |
| Spec update (`system-config.md`) | 0.5 day |
| E2E test for admission rejection | 0.5 day |
| **Total** | **~2 days** |
