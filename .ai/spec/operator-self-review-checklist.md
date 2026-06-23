# OpenShift Operator Self-Review Checklist

An exhaustive pre-PR checklist for OpenShift operators built with controller-runtime / kubebuilder in Go. Use this before pushing code to catch issues that reviewers (human or AI) would flag as Critical or High.

**Severity levels:**
- **[Critical]** — Will cause bugs, data loss, security holes, or infinite loops in production. PR-blocking.
- **[High]** — Significant reliability/quality issue. Experienced reviewers will call it out.
- **[Medium]** — Best practice for maintainability, performance, or UX. Worth doing but won't break things if missed.

---

## 1. CRD / API Design

### Type Definitions

- [ ] **[Critical]** Every CRD type has `+kubebuilder:subresource:status`. *Why: without it, status updates increment `metadata.generation`, causing infinite reconciliation loops.*
- [ ] **[Critical]** `spec` and `status` are cleanly separated — controller never writes to `spec`, users never write to `status`. *Why: violating this breaks the desired-vs-observed contract and causes reconciliation fights.*
- [ ] **[High]** Optional numeric/boolean fields use pointers (`*int32`, `*bool`) to distinguish "not set" from zero-value. *Why: without pointers, you can't tell if a user set `replicas: 0` or omitted the field entirely.*
- [ ] **[High]** Required fields are marked `+required` or `+kubebuilder:validation:Required`. Optional fields are marked `+optional`. *Why: implicit contracts lead to nil-pointer panics and confusing user errors.*
- [ ] **[High]** Exactly one API version is marked `+kubebuilder:storageversion`. *Why: all objects persist in etcd using this version; missing it breaks upgrades.*
- [ ] **[Medium]** Field names follow Kubernetes conventions: camelCase in JSON, no abbreviations, plural for lists, singular for maps. *Why: consistency with the ecosystem makes the API intuitive.*
- [ ] **[Medium]** Every exported type and field has a godoc comment. *Why: these become the API reference docs and show up in `kubectl explain`.*

### Validation

- [ ] **[Critical]** All string-enum fields have `+kubebuilder:validation:Enum`. *Why: without it, typos and invalid values pass admission and reach your controller, causing silent failures.*
- [ ] **[High]** Numeric fields have `+kubebuilder:validation:Minimum` / `Maximum` where applicable. *Why: out-of-range values cause panics or undefined behavior in reconciliation.*
- [ ] **[High]** String fields have `+kubebuilder:validation:MaxLength` to prevent unbounded input. *Why: etcd has a 1.5MB object size limit; unbounded strings can hit it and cause cryptic errors.*
- [ ] **[High]** Cross-field validation uses `+kubebuilder:validation:XValidation` (CEL rules) rather than webhooks where possible. *Why: CEL runs in-process in the API server — no network hop, no availability dependency on your webhook.*
- [ ] **[High]** Union/discriminated types use `+kubebuilder:validation:XValidation` to enforce exactly-one-of semantics. *Why: without it, users can set conflicting fields simultaneously.*
- [ ] **[Medium]** List fields have `+kubebuilder:validation:MaxItems` to bound cardinality. *Why: unbounded lists cause memory issues during reconciliation and etcd size limits.*
- [ ] **[Medium]** URL fields validate scheme (HTTP/HTTPS only), reject fragments and userinfo. *Why: prevents injection of unexpected protocols or credentials in URLs.*

### Defaulting

- [ ] **[High]** Sensible defaults are set via `+kubebuilder:default` markers. *Why: reduces user configuration burden and prevents nil-pointer panics when optional fields are omitted.*
- [ ] **[Medium]** Controller initializes fields defensively even when defaults exist. *Why: webhooks may not be installed; the controller is the safety net.*

### Printer Columns & UX

- [ ] **[Medium]** Key status fields have `+kubebuilder:printcolumn` (at minimum: a status/ready column and age). *Why: `kubectl get` should show useful information without requiring `kubectl describe`.*
- [ ] **[Medium]** Wide-only columns use `priority: 1` for detailed info. *Why: keeps default `kubectl get` output clean while making detail available via `-o wide`.*

### Strategic Merge Patch

- [ ] **[High]** List fields with identity (items have a key like `name`) use `+listType=map` and `+listMapKey=<field>`. *Why: without this, JSON merge patch replaces entire lists, silently dropping entries on partial updates.*
- [ ] **[Medium]** Deduplicated value lists use `+listType=set`. Atomic lists use `+listType=atomic`. *Why: correct merge semantics prevent data loss during concurrent updates.*

### Immutability

- [ ] **[High]** Fields that must not change after creation are enforced via `+kubebuilder:validation:XValidation` with `rule: "self == oldSelf"` or `+k8s:immutable`. *Why: allowing mutation of identity/config fields after creation causes state corruption.*

---

## 2. Controller Reconciliation Logic

### Core Contract

- [ ] **[Critical]** `Reconcile()` is idempotent — running it twice with no external changes produces the same result. *Why: this is the fundamental contract of the controller pattern; violating it causes duplicate resources, double-counting, or drift.*
- [ ] **[Critical]** Reconciliation is level-based (compares actual vs desired state), not edge-based (reacting to "what event happened"). *Why: events can be missed, duplicated, or reordered; level-based logic always converges.*
- [ ] **[Critical]** Controller handles the "object not found" case (deleted between enqueue and reconcile) by returning `(ctrl.Result{}, nil)` without error. *Why: not-found is normal during deletion; erroring on it causes infinite error loops.*
- [ ] **[High]** Reconciliation follows a consistent shape: (1) fetch resource, (2) handle deletion/finalizer, (3) initialize defaults, (4) validate, (5) business logic, (6) update status. *Why: consistent structure reduces cognitive load and bugs across controllers.*
- [ ] **[High]** No reconciliation path modifies the CR's `spec` — only `status` is updated by the controller. *Why: the controller is not the owner of desired state; writing to spec violates separation of concerns and can fight with user updates.*

### Idempotent Child Resource Creation

- [ ] **[Critical]** Child resource creation uses create-only-if-not-exists pattern (Create + ignore AlreadyExists) or get-then-create with conflict handling. *Why: without idempotent creation, duplicate child resources accumulate on every reconciliation.*
- [ ] **[High]** No read-modify-write (GET → modify → UPDATE) without conflict retry. *Why: concurrent reconciles can overwrite each other's changes, causing silent data loss.*
- [ ] **[High]** Uses `Patch` instead of `Update` for modifying existing resources where possible. *Why: Patch is less prone to conflict errors because it doesn't require the full object.*

### Concurrency & Reentrancy

- [ ] **[Critical]** No shared mutable state across reconcile invocations without synchronization. *Why: `MaxConcurrentReconciles > 1` means multiple goroutines run Reconcile simultaneously; shared state causes data races.*
- [ ] **[High]** Controller does not assume it is the only writer to a resource. *Why: users, other controllers, and admission webhooks can all modify resources concurrently.*

---

## 3. Status & Conditions

- [ ] **[Critical]** Status updates use `r.Status().Update()` or `r.Status().Patch()`, never `r.Update()`. *Why: `r.Update()` modifies the spec subresource, incrementing `generation` and causing infinite reconciliation loops.*
- [ ] **[Critical]** `ObservedGeneration` is set on every condition write to `metadata.generation`. *Why: without it, consumers cannot distinguish stale status (from a previous spec) from current status. A `Ready=True` condition without `ObservedGeneration` may reflect a previous generation's state.*
- [ ] **[High]** Conditions use `metav1.Condition` (standard Kubernetes condition type), not custom structs. *Why: standard conditions work with ecosystem tooling (kubectl wait, Argo, Flux) and follow consistent semantics.*
- [ ] **[High]** Condition types use positive polarity: `Ready=True` means good, `Ready=False` means bad. No `NotReady` or `Failed` type names. *Why: Kubernetes convention; double-negatives (`NotReady=False`) are confusing.*
- [ ] **[High]** `Reason` values are CamelCase (e.g., `DeploymentAvailable`, `ConfigInvalid`). *Why: Kubernetes convention; tools and dashboards parse these programmatically.*
- [ ] **[High]** Uses `meta.SetStatusCondition()` helper, not manual list manipulation. *Why: the helper only updates `LastTransitionTime` on actual status transitions, preventing unnecessary status churn that triggers downstream watches.*
- [ ] **[High]** A top-level `Ready` or aggregate condition exists that summarizes overall health. *Why: users should be able to check a single condition without understanding internal sub-conditions.*
- [ ] **[High]** Phase/state is derived from conditions, not stored as a separate field. *Why: a stored phase field is a second source of truth that can diverge from conditions, causing inconsistent reporting.*
- [ ] **[Medium]** Status update conflicts (optimistic concurrency) are handled gracefully — requeue and retry, don't fail permanently. *Why: conflicts are normal under concurrent access; crashing on them causes unnecessary restarts.*

---

## 4. Owner References & Garbage Collection

- [ ] **[Critical]** No cross-namespace owner references. Owner and owned resource are in the same namespace. *Why: cross-namespace owner references are silently ignored by the garbage collector, causing orphaned resources.*
- [ ] **[Critical]** Cluster-scoped resources do not set owner references to namespace-scoped resources. *Why: the garbage collector cannot resolve the reference, leading to unpredictable behavior.*
- [ ] **[High]** All controller-created child resources in the same namespace have `ctrl.SetControllerReference()` set. *Why: enables automatic garbage collection when the parent is deleted; without it, child resources are orphaned.*
- [ ] **[High]** Cross-namespace dependencies use labels + finalizer-based cleanup instead of owner references. *Why: owner references only work within a namespace; labels + explicit cleanup is the correct cross-namespace pattern.*
- [ ] **[Medium]** Owner references use `SetControllerReference` (not `SetOwnerReference`) to mark the controller as the managing owner. *Why: `SetControllerReference` also sets `controller: true` and `blockOwnerDeletion: true`, enabling proper GC behavior.*

---

## 5. Finalizers & Deletion

- [ ] **[Critical]** Finalizers are added during initialization (before the first successful reconciliation), not during deletion. *Why: adding finalizers during deletion is a race condition — the object may be deleted before you can add it.*
- [ ] **[Critical]** Finalizer cleanup logic runs only when `DeletionTimestamp` is set, and the finalizer is removed only after cleanup succeeds. *Why: removing the finalizer before cleanup completes means the object is deleted with dirty external state.*
- [ ] **[Critical]** Finalizer cleanup does not create new resources in a namespace that is being deleted (`namespace.Status.Phase != Terminating`). *Why: creating resources in a terminating namespace is rejected by the API server, causing the finalizer to stick forever.*
- [ ] **[High]** Each controller manages its own uniquely-named finalizer and ignores others. *Why: multiple controllers managing the same finalizer cause race conditions and skipped cleanup.*
- [ ] **[High]** Uses `controllerutil.AddFinalizer()` / `RemoveFinalizer()` helpers. *Why: these handle idempotency and list management correctly; manual list manipulation is error-prone.*
- [ ] **[High]** Finalizer names are scoped to the operator domain (e.g., `agentic.openshift.io/cleanup`). *Why: generic names like `finalizer` can collide with other operators.*
- [ ] **[High]** Cleanup has a bounded retry/timeout strategy. *Why: infinite retries on failed cleanup keep the finalizer forever, blocking namespace deletion and frustrating operators.*
- [ ] **[Medium]** Finalizer algorithm follows the standard pattern: (1) fetch CR, (2) if deleting and finalizer present → run cleanup → remove finalizer, (3) if not deleting and finalizer absent → add finalizer, (4) normal reconciliation. *Why: this ordering prevents races between deletion and reconciliation.*
- [ ] **[Medium]** Integration tests cover the full finalizer lifecycle: create → verify finalizer added → delete → verify cleanup → verify finalizer removed. *Why: finalizer bugs are hard to catch in production and cause stuck resources.*

---

## 6. RBAC

- [ ] **[Critical]** No `verbs=*` or `resources=*` in RBAC markers. Every verb and resource is listed explicitly. *Why: wildcard RBAC grants far more permission than needed; a compromised operator SA with `*` can do anything in the cluster.*
- [ ] **[Critical]** No `bind` or `escalate` verbs unless absolutely necessary and documented. *Why: these bypass RBAC protections entirely, allowing the operator to grant itself or others any permission.*
- [ ] **[High]** RBAC markers (`+kubebuilder:rbac`) are placed directly above `Reconcile()` and kept synchronized with code. *Why: stale markers cause either missing permissions (runtime errors) or excess permissions (security risk).*
- [ ] **[High]** Generated RBAC (`config/rbac/role.yaml`) is regenerated via `make manifests` after any RBAC marker changes, and the diff is committed. *Why: stale generated files cause deployment-time permission errors.*
- [ ] **[High]** Namespace-scoped operations use `Role` (not `ClusterRole`) when the operator only manages resources in its own namespace. *Why: reduces blast radius of compromise to a single namespace.*
- [ ] **[High]** Operator's service account is in a separate namespace from operands. *Why: co-locating with operands lets any pod in the namespace read the operator's token via projected volumes.*
- [ ] **[Medium]** `resourceNames` is used to restrict access to specific named resources where possible (e.g., a specific ConfigMap). *Why: limits scope even further within a namespace.*
- [ ] **[Medium]** RBAC is audited on each release — permissions for removed features are cleaned up. *Why: permissions accumulate over time; stale grants are unnecessary attack surface.*

---

## 7. Webhooks (Validating, Mutating, Conversion)

### General

- [ ] **[Critical]** Webhook handler is idempotent: `webhook(webhook(resource)) == webhook(resource)`. *Why: Kubernetes may re-invoke mutating webhooks; non-idempotent webhooks corrupt data on retry.*
- [ ] **[Critical]** Controller validates the CR even if webhooks exist — webhooks may not be installed or may fail. *Why: webhook installation is not guaranteed; the controller must gracefully handle invalid CRs.*
- [ ] **[High]** Webhook timeouts are short (5-10 seconds). *Why: webhooks add latency to every matching API request; long timeouts cause kubectl hangs and cascading slowdowns.*
- [ ] **[High]** System namespaces (`kube-system`, `openshift-*`) are excluded from webhook scope. *Why: matching core system resources can break cluster bootstrapping and updates.*
- [ ] **[High]** Webhook failure policy is explicitly set: `Fail` for validating (reject invalid objects), consider `Ignore` during initial rollout for mutating (prevent blocking all API ops if webhook crashes). *Why: default `Fail` on a broken webhook blocks all matching API operations cluster-wide.*

### Mutating Webhooks

- [ ] **[Critical]** Mutating webhooks do not overwrite entire arrays/lists — they patch specific fields. *Why: overwriting arrays causes data loss from other mutating webhooks or user-set values.*
- [ ] **[High]** No side effects in webhook handlers (no creating external resources, no API calls beyond reading). *Why: webhooks can be called speculatively (dry-run) or retried; side effects would be duplicated.*
- [ ] **[High]** Server-side injected fields (e.g., authenticated user identity) overwrite client-submitted values. *Why: trusting client-submitted identity fields enables impersonation attacks.*

### Conversion Webhooks

- [ ] **[Critical]** Conversion is lossless (round-trippable): `v1alpha1 → v1 → v1alpha1` does not lose data. *Why: lossy conversion silently drops fields during API version negotiation, corrupting stored objects.*
- [ ] **[Critical]** Conversion does not mutate `metadata.name`, `metadata.uid`, or `metadata.namespace`. *Why: the API server rejects these changes, causing all reads/writes of the converted resource to fail.*
- [ ] **[High]** Hub-and-spoke model: storage version is the "hub", non-hub versions implement `ConvertTo()` / `ConvertFrom()`. *Why: this is the standard pattern that scales to N versions without N² conversion functions.*
- [ ] **[Medium]** Fields that exist in one version but not another are preserved in annotations during conversion. *Why: without this, round-tripping through the hub drops version-specific fields.*

---

## 8. Error Handling & Requeueing

- [ ] **[Critical]** Returns `(ctrl.Result{}, err)` for transient errors — controller-runtime applies exponential backoff automatically. Does NOT return `Requeue: true` when an error is present. *Why: returning both Requeue and error causes double-queueing; the error path already requeues with backoff.*
- [ ] **[Critical]** Returns `(ctrl.Result{}, nil)` when reconciliation succeeded and no further action is needed. *Why: returning `Requeue: true` on success creates a hot loop that burns CPU and API server quota.*
- [ ] **[High]** Uses `reconcile.TerminalError(err)` for permanently invalid CRs that the user must fix. *Why: without terminal errors, the controller retries infinitely, filling logs and wasting resources on an error only a human can fix.*
- [ ] **[High]** Returns `(ctrl.Result{RequeueAfter: duration}, nil)` for wall-clock periodic checks (e.g., cert rotation, polling external state). *Why: `RequeueAfter` is deterministic; `Requeue: true` with backoff is unpredictable for time-sensitive operations.*
- [ ] **[High]** Returns `(ctrl.Result{Requeue: true}, nil)` only when something is in-progress but no error occurred (e.g., waiting for a pod to become ready). *Why: this triggers the rate limiter's backoff, which is appropriate for polling progress without overwhelming the API server.*
- [ ] **[Medium]** Error messages are wrapped with context: `fmt.Errorf("creating deployment for %s: %w", name, err)`. *Why: bare errors without context are impossible to diagnose in production logs.*
- [ ] **[Medium]** Does not assume permanent failure — images may appear, credentials may be added, networks recover. Retries with backoff for seemingly unrecoverable errors but reports the error in status. *Why: operators outlive transient failures; giving up permanently orphans the managed resource.*

---

## 9. Watch Predicates & Event Filtering

- [ ] **[High]** Uses `GenerationChangedPredicate` on the primary resource to skip reconciliation for status-only or metadata-only updates. *Why: without it, a status update triggers reconciliation, which updates status, which triggers reconciliation — an infinite loop.*
- [ ] **[High]** Does NOT use `GenerationChangedPredicate` for Secrets or ConfigMaps. *Why: Kubernetes does not increment `metadata.generation` on Secret/ConfigMap data changes — the predicate would filter out real data changes.*
- [ ] **[High]** Child-resource watches use `handler.EnqueueRequestForOwner` to reconcile the parent, not the child. *Why: the controller reconciles the parent CR, not individual child resources; enqueueing the child causes "resource not found" errors.*
- [ ] **[Medium]** Predicates are combined with `predicate.Or()` when multiple event types matter (e.g., generation changes OR annotation changes). *Why: `And()` is too restrictive; `Or()` ensures all relevant events trigger reconciliation.*
- [ ] **[Medium]** Cache label/field selectors limit what gets loaded into memory, in addition to predicates that filter what triggers reconciliation. *Why: predicates filter events but don't reduce memory usage; cache selectors do both.*

---

## 10. Operand Lifecycle Management

- [ ] **[High]** Operand creation uses "ensure" pattern: check if exists, create if not, update if spec differs. Never blindly creates without checking. *Why: blind creation causes duplicate resources and conflict errors.*
- [ ] **[High]** Compares desired state to actual state before updating operands — skips the API call if nothing changed. *Why: unnecessary writes cause conflict errors, status churn, and wasted API server load.*
- [ ] **[High]** After updating an operand (e.g., Deployment), requeues to verify the rollout completed rather than assuming success. *Why: the update API call succeeding doesn't mean the rollout succeeded — pods may crash, images may be missing.*
- [ ] **[Medium]** Stores the hash of the desired operand spec in an annotation for cheap change detection. *Why: comparing hashes is O(1) and avoids expensive deep-equality checks on complex structs.*
- [ ] **[Medium]** Operand Deployments use rolling update strategy with explicit `maxSurge` and `maxUnavailable`. *Why: default rolling update values may not be appropriate for your workload's availability requirements.*

---

## 11. Leader Election & HA

- [ ] **[Critical]** Leader election is enabled in production (`LeaderElection: true` in Manager options). *Why: without it, multiple replicas reconcile simultaneously, causing duplicate child resources, conflicting writes, and race conditions.*
- [ ] **[High]** `LeaderElectionID` is unique per operator and uses a domain-scoped name (e.g., `agentic.openshift.io`). *Why: two different operators sharing a LeaderElectionID fight for the same lock, causing one to never reconcile.*
- [ ] **[High]** `LeaderElectionReleaseOnCancel: true` is set. *Why: on graceful shutdown, the leader releases the lease immediately instead of waiting for expiry, speeding up failover.*
- [ ] **[Medium]** `LeaseDuration`, `RenewDeadline`, and `RetryPeriod` are explicitly set or the defaults (15s, 10s, 2s) are acceptable. *Why: shorter values mean faster failover but more API server load; longer values mean slower failover but less overhead.*
- [ ] **[Medium]** Operator Deployment has `replicas: 2` (or more) for HA. *Why: a single replica means the operator is unavailable during pod restarts, node maintenance, and OOM kills.*

---

## 12. Health Probes

- [ ] **[Critical]** Liveness probe (`/healthz`) does NOT check external dependencies (database, API server, etc.). *Why: if the database is down, restarting the operator pod makes things worse; liveness means "is the process deadlocked," not "are all dependencies up."*
- [ ] **[High]** Liveness probe is configured using `healthz.Ping` (or equivalent simple check). *Why: complex liveness probes that call external services are the #1 cause of unnecessary operator pod restarts.*
- [ ] **[High]** Readiness probe (`/readyz`) checks informer cache sync status. *Why: an operator with unsynced caches will reconcile against stale data, causing incorrect decisions.*
- [ ] **[Medium]** If the operator has slow initialization (large cache warm-up), a startup probe is configured with sufficient `failureThreshold × periodSeconds`. *Why: without a startup probe, the liveness probe kills the pod during legitimate slow starts.*
- [ ] **[Medium]** `initialDelaySeconds` covers realistic startup time. *Why: too low = premature restarts; too high = slow failure detection.*

---

## 13. Metrics & Monitoring

### Default Metrics

- [ ] **[High]** Default controller-runtime metrics are not disabled (reconcile duration, reconcile errors, work queue depth/latency). *Why: these are critical for diagnosing operational issues; disabling them blinds the operations team.*

### Custom Metrics

- [ ] **[High]** Custom metrics use a consistent prefix (e.g., `myoperator_`) and follow Prometheus naming conventions. *Why: inconsistent naming makes dashboards and alerts fragile and hard to discover.*
- [ ] **[Medium]** Domain-specific metrics are exposed: managed instance count, operand health, configuration drift, error rates. *Why: generic controller metrics don't tell you if your operands are healthy; domain metrics do.*
- [ ] **[Medium]** Custom metrics are registered via `metrics.Registry.MustRegister()` in `init()`. *Why: late registration can cause panics if metrics are scraped before registration; `init()` ensures they're ready at startup.*

### Prometheus Integration

- [ ] **[High]** A `ServiceMonitor` resource exists in kustomize config for Prometheus to discover the operator's metrics endpoint. *Why: without a ServiceMonitor, Prometheus doesn't scrape the operator, and all metrics are invisible.*
- [ ] **[High]** Metrics endpoint is secured with TLS and authentication (not exposed as plain HTTP). *Why: metrics can leak sensitive operational data; unauthenticated access is a security risk.*
- [ ] **[Medium]** `PrometheusRule` resources define alerts for common failure modes (reconciliation errors, operand degraded, high queue depth). *Why: alerts turn passive metrics into actionable notifications.*
- [ ] **[Medium]** A `NetworkPolicy` restricts metrics access to only the Prometheus namespace. *Why: limits exposure of the metrics endpoint to authorized scrapers only.*

---

## 14. Security & Pod Hardening

### Operator Pod Security

- [ ] **[Critical]** Operator pod runs as non-root: `runAsNonRoot: true`, numeric `USER` in Containerfile (e.g., `USER 65532:65532`). *Why: root containers can escalate to host-level access; non-root is required for OpenShift restricted SCC and certification.*
- [ ] **[Critical]** `allowPrivilegeEscalation: false` and `capabilities: {drop: [ALL]}` are set. *Why: these prevent container breakout attacks; required by the `restricted` Pod Security Standard.*
- [ ] **[High]** `readOnlyRootFilesystem: true` is set. *Why: prevents attackers from writing malicious binaries; forces all writes to explicit volumes.*
- [ ] **[High]** `seccompProfile: {type: RuntimeDefault}` is set. *Why: restricts system calls the container can make; blocks many container escape techniques.*
- [ ] **[High]** `automountServiceAccountToken: false` on operand pods that don't need API access. *Why: projected SA tokens are a credential; not mounting them reduces attack surface of compromised operand pods.*

### Image Security

- [ ] **[High]** Base image is Red Hat UBI (Universal Base Image). *Why: required for OpenShift certification; provides RHEL-level security patches and FIPS compliance.*
- [ ] **[High]** Multi-stage Dockerfile: build stage compiles, runtime stage contains only the binary. *Why: build tools (compilers, package managers) in the runtime image are unnecessary attack surface.*
- [ ] **[Medium]** Image digests are pinned in deployment manifests, not mutable tags. *Why: tags can be overwritten; digests are immutable and ensure exactly the tested image runs.*
- [ ] **[Medium]** Image scanning (Trivy or similar) runs in CI and blocks on critical/high CVEs. *Why: shipping known vulnerabilities is a compliance and security risk.*
- [ ] **[Medium]** Go module dependencies are scanned for known vulnerabilities (Dependabot, Renovate, `govulncheck`). *Why: transitive dependency vulnerabilities are the most common source of CVEs in Go binaries.*

### Network Security

- [ ] **[Medium]** TLS is used for all internal communication (operator ↔ operand, operator ↔ webhook). *Why: unencrypted in-cluster traffic can be sniffed by compromised pods on the same node.*
- [ ] **[Medium]** A deny-all default NetworkPolicy exists in the operator namespace, with explicit allow rules for API server egress, Prometheus ingress, and operand egress. *Why: limits lateral movement if the operator pod is compromised.*

---

## 15. Testing

### Unit Tests

- [ ] **[High]** Reconciliation logic is tested in isolation with a fake client (`fake.NewClientBuilder()`). *Why: unit tests with fake clients are fast (~ms) and catch logic bugs without needing a cluster.*
- [ ] **[High]** Validation and defaulting logic is tested independently from the reconciler. *Why: these are pure functions; testing them separately keeps test focus narrow and failures easy to diagnose.*
- [ ] **[High]** Table-driven tests cover both happy paths and error cases for each reconciliation phase. *Why: table-driven tests make it easy to add edge cases and see all scenarios at a glance.*
- [ ] **[Medium]** Test helpers create minimal fixtures (e.g., `testProposal()`, `testLLM()`) with only the fields needed for each test. *Why: bloated fixtures obscure what the test actually validates.*

### Integration Tests (envtest)

- [ ] **[High]** Integration tests use `envtest` (real etcd + API server, no kubelet) for full reconciliation loop testing. *Why: envtest catches bugs that fake clients miss: admission webhooks, status subresource behavior, field validation.*
- [ ] **[High]** Integration tests verify end-to-end: create CR → wait for controller → verify child resources and status conditions. *Why: this is the closest to production behavior without a full cluster.*
- [ ] **[Medium]** `ReaderFailOnMissingInformer: true` is set in test configurations. *Why: catches accidental informer creation that could cause memory leaks in production.*

### E2E Tests

- [ ] **[High]** E2E tests run against a real cluster (Kind, OpenShift) and test the full operator lifecycle: deploy → create CRs → verify operands → update → delete → verify cleanup. *Why: e2e tests catch deployment, RBAC, and networking issues that unit/integration tests cannot.*
- [ ] **[Medium]** E2E tests use a build tag (e.g., `//go:build e2e`) to separate them from unit tests. *Why: e2e tests require a cluster and take minutes; mixing them with unit tests slows down the development loop.*
- [ ] **[Medium]** E2E tests cover upgrade scenarios: install v1, create CRs, upgrade to v2, verify CRs and operands are healthy. *Why: upgrade bugs are the most common source of production incidents for operators.*

### Test Coverage

- [ ] **[High]** Finalizer lifecycle is tested: create → verify finalizer added → delete → verify cleanup → verify finalizer removed. *Why: finalizer bugs cause stuck resources that block namespace deletion.*
- [ ] **[Medium]** Webhook logic (mutating, validating) has dedicated tests with various valid and invalid inputs. *Why: webhook bugs affect all API operations matching the webhook scope, not just your operator.*

---

## 16. Structured Logging & Events

### Logging

- [ ] **[High]** Uses `logr` interface with key-value pairs, not string interpolation or `fmt.Sprintf`. *Why: structured logs are searchable/parseable by log aggregators; string interpolation breaks field extraction.*
- [ ] **[High]** Kubernetes objects are logged via `klog.KObj()` or `klog.KRef()` (produces `namespace/name`). *Why: consistent formatting makes logs greppable across all controllers.*
- [ ] **[High]** Log levels are set appropriately: `V(0)` for errors/state changes, `V(1)` for flow, `V(2)` for debug. Production runs at `V(0)`. *Why: verbose logging in production causes log volume explosion and masks real errors.*
- [ ] **[Medium]** Significant state changes log the "before" and "after" (e.g., "scaling deployment from 3 to 5 replicas"). *Why: state-change diffs are the most useful information for diagnosing production issues.*
- [ ] **[Medium]** Sensitive data (secrets, tokens, credentials) is never logged, even at debug level. *Why: log aggregators store data long-term; leaked credentials in logs are a security incident.*

### Kubernetes Events

- [ ] **[High]** Meaningful state changes emit Kubernetes events via `mgr.GetEventRecorderFor()`. *Why: events are visible in `kubectl describe` and are the primary debugging tool for cluster operators.*
- [ ] **[High]** Event `reason` uses CamelCase (e.g., `ReconciliationSucceeded`, `OperandDeployFailed`). *Why: convention enables programmatic event handling by monitoring tools.*
- [ ] **[Medium]** Events use `EventTypeNormal` for success, `EventTypeWarning` for errors/degradation. *Why: warning events surface in monitoring dashboards; normal events are informational.*
- [ ] **[Medium]** Events are NOT emitted for every reconciliation — only for meaningful state transitions. *Why: noisy events bury important information and degrade `kubectl describe` UX.*

---

## 17. Performance & Scalability

### Informer Cache

- [ ] **[High]** `ReaderFailOnMissingInformer: true` is set in Manager options. *Why: without it, controller-runtime silently creates new informers on uncached `Get()`/`List()` calls, causing memory leaks and worker blocking.*
- [ ] **[High]** Infrequently accessed large resource types (Secrets, ConfigMaps) are excluded from cache or use label selectors. *Why: every cached type maintains a full in-memory copy of all matching objects cluster-wide; caching all Secrets can consume gigabytes.*
- [ ] **[Medium]** Field indexes (`mgr.GetFieldIndexer().IndexField()`) are used for frequent lookups. *Why: indexed `List()` calls resolve from cache in O(1); unindexed calls scan the full cache.*

### Rate Limiting & Concurrency

- [ ] **[High]** `MaxConcurrentReconciles` is tuned based on workload (not left at default 1 unless appropriate). *Why: the default of 1 serializes all reconciliation; for operators managing many objects, this creates backlogs.*
- [ ] **[Medium]** Work queue depth (`workqueue_depth` metric) is monitored. *Why: growing queue depth means the reconciler is falling behind, which delays status updates and slows user operations.*
- [ ] **[Medium]** Reconciliation of an up-to-date object is fast and offline (reads only from cache, makes zero API calls). *Why: during resyncs, every watched object is re-reconciled; slow reconciles create massive backlogs at scale.*

### API Call Efficiency

- [ ] **[High]** `status.observedGeneration` is compared to `metadata.generation` to skip reconciliation when nothing changed in the spec. *Why: avoids expensive API calls and external operations when the user hasn't changed anything.*
- [ ] **[Medium]** Paginated `List()` calls are used when listing large collections directly from the API server (not cache). *Why: unpaginated List on large collections can OOM the operator or timeout.*

---

## 18. OpenShift-Specific

### Security Context Constraints

- [ ] **[High]** Operator and operand pods target `restricted-v2` SCC. *Why: most restrictive SCC; required for OpenShift certification.*
- [ ] **[High]** Custom SCC requirements are documented with justification. *Why: reviewers and security teams need to audit why elevated privileges are needed.*
- [ ] **[Critical]** No use of `allowHostNetwork`, `allowHostPID`, `allowHostIPC`, or `allowHostPorts` unless the operator is a control-plane component. *Why: these effectively grant host access, bypassing container isolation.*

### Routes & Networking

- [ ] **[Medium]** Uses `Route` resources (not `Ingress`) for OpenShift-native HTTP endpoints. *Why: Routes support edge, passthrough, and re-encrypt TLS termination with OpenShift's router.*
- [ ] **[Medium]** Uses service-serving certificates (`service.beta.openshift.io/serving-cert-secret-name` annotation) for automatic TLS cert provisioning. *Why: OpenShift manages certificate rotation automatically; manual cert management is error-prone.*

### Deployment

- [ ] **[High]** Uses `apps/v1 Deployment`, not `DeploymentConfig`. *Why: DeploymentConfig is deprecated since OpenShift 4.14; Deployment is the Kubernetes standard.*

### API Integration

- [ ] **[High]** OpenShift API types (e.g., `routev1`, `consolev1`) are registered in the scheme via `utilruntime.Must()`. *Why: unregistered types cause runtime panics when the controller tries to create or read them.*
- [ ] **[Medium]** OpenShift-specific code paths are guarded by API discovery (check if the API group exists). *Why: makes the operator portable to vanilla Kubernetes; prevents crashes on non-OpenShift clusters.*

---

## 19. OLM & Operator Lifecycle

### ClusterServiceVersion (CSV)

- [ ] **[High]** CSV defines all required RBAC, CRDs, and deployment specs. *Why: the CSV is the single source of truth for OLM installation; missing entries cause install failures.*
- [ ] **[High]** `installModes` are set correctly (typically `OwnNamespace` + `AllNamespaces`). *Why: incorrect install modes cause OLM to reject the operator or install it with wrong scope.*
- [ ] **[Medium]** CSV includes `description`, `icon`, `maintainers`, `links`, and `minKubeVersion`. *Why: catalog UX and compatibility checking depend on these fields.*

### Upgrade Strategy

- [ ] **[High]** `replaces` field defines a linear upgrade path between versions. *Why: without it, OLM cannot automatically upgrade from one version to the next.*
- [ ] **[Medium]** `skips` or `skipRange` allows skipping intermediate versions when safe. *Why: users on old versions shouldn't have to install every intermediate release.*
- [ ] **[Medium]** Upgrade path is tested in CI: install v(N), create CRs, upgrade to v(N+1), verify health. *Why: untested upgrades are the most common source of operator-related production incidents.*

### Bundle Validation

- [ ] **[High]** `operator-sdk bundle validate` runs in CI and passes. *Why: catches metadata issues (missing CRDs, invalid CSV fields) before they reach users.*
- [ ] **[Medium]** Scorecard checks (`operator-sdk scorecard`) run in CI. *Why: scorecard validates runtime behavior (CR creation, status updates, cleanup) against OLM expectations.*

---

## 20. Build, CI/CD & Config Management

### Build

- [ ] **[High]** `make manifests` and `make generate` output matches committed files (CI fails on diff). *Why: stale generated code causes runtime errors that are hard to diagnose; the CI check enforces freshness.*
- [ ] **[High]** All tool versions (controller-gen, kustomize, operator-sdk) are pinned. *Why: unpinned tools cause non-reproducible builds and "works on my machine" failures.*
- [ ] **[High]** `go vet`, `golangci-lint`, and API-specific linters (e.g., `golangci-kal`) run in CI. *Why: linters catch bugs, style violations, and API design issues that reviewers would flag.*
- [ ] **[Medium]** Dockerfile uses multi-stage build with a minimal runtime image (UBI-minimal). *Why: smaller images have less attack surface, pull faster, and use less registry storage.*

### CI Pipeline Order

- [ ] **[Medium]** CI runs in this order: lint → unit test → build → integration test → e2e test → bundle validate → image scan. *Why: fast checks first (lint, unit) fail early; expensive checks (e2e, scan) run only on code that passes basics.*

### Kustomize Config

- [ ] **[High]** No hardcoded namespaces in manifests — uses kustomize `namespace` transformer. *Why: hardcoded namespaces break deployment to non-default namespaces.*
- [ ] **[Medium]** Environment-specific values (image registry, resource limits, replica counts) are in patches/overlays, not in base manifests. *Why: keeps the base environment-neutral and prevents config drift between environments.*
- [ ] **[Medium]** No duplicated manifests across overlays — overlays patch the base. *Why: duplication causes manifests to diverge silently when one copy is updated but not the other.*

### Git & PR Hygiene

- [ ] **[High]** Commit messages and PR titles follow project conventions (e.g., `OLS-XXXX` prefix). *Why: Jira integration and automated changelog generation depend on consistent formatting.*
- [ ] **[Medium]** Generated files are committed separately from hand-written code changes. *Why: generated file diffs are noisy; separating them makes the hand-written changes reviewable.*

---

## Quick Reference: Pre-Push Sanity Checks

Run these commands before every PR push:

```bash
# Format and lint
make fmt
make fmt-check
make vet
make api-lint

# Regenerate and verify manifests are fresh
make manifests
make generate
git diff --exit-code

# Run tests
make test

# Build
make build
make docker-build
```

---

## How to Use This Checklist

### For Self-Review (Before Pushing a PR)
1. Scan the categories relevant to your change (API change? Check sections 1, 3, 7. Controller change? Check sections 2, 4, 5, 8, 9.)
2. Check every Critical item — these are PR-blockers.
3. Check High items — address them or document why they don't apply.
4. Skim Medium items — address what's easy, note what you'll defer.

### For AI Code Review
Include this file as context when asking an AI to review your PR. The AI should validate each applicable item and only flag issues not covered by this checklist as new findings.

### Maintaining This Checklist
- Add new items when a reviewer catches something not covered here.
- Remove items that become enforced by linters or admission webhooks (they're already caught).
- Update severity when experience shows an item causes more (or fewer) production issues than expected.
