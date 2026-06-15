# Audit Logging

Implementation spec for compliance audit logging in the agentic operator. Parent spec: `ols/.ai/spec/what/audit-logging.md` (authoritative for cross-repo requirements, event semantics, and correlation contract).

## Behavioral Rules

### Operator Audit Events

1. The operator MUST emit the following structured JSON audit events to stdout at each phase transition during Proposal reconciliation. Each event carries `trace_id` (Proposal's `metadata.uid` with hyphens stripped to 32 hex chars).

| Event | When | Payload |
|---|---|---|
| `audit.proposal.received` | New Proposal CR detected (finalizer added) | Proposal `.spec` + select metadata |
| `audit.analysis.completed` | AnalysisResult CR created | AnalysisResult serialization (all RemediationOptions) |
| `audit.approval.received` | ProposalApproval PATCH observed by webhook | Approver `uid`/`username` (webhook-injected), selected option, full text of selected option |
| `audit.execution.completed` | ExecutionResult CR created | ExecutionResult serialization (all ActionsTaken) |
| `audit.verification.completed` | VerificationResult CR created, checks passed | VerificationResult serialization |
| `audit.verification.retry` | Verification failed, retrying execution+verification | VerificationResult serialization, retry count |
| `audit.escalation.completed` | EscalationResult CR created | EscalationResult serialization |
| `audit.proposal.terminal` | Proposal reaches terminal phase (Completed, Failed, Denied, Escalated) | Final phase, terminal reason |

2. CR serialization MUST include `.spec` plus `metadata.name`, `metadata.namespace`, `metadata.creationTimestamp`, and `metadata.uid`. Not the full Kubernetes metadata.

3. Audit events MUST be emitted from the reconciliation loop where the operator already has the Proposal object in scope. The `trace_id` is read from the Proposal's `metadata.uid`.

### OTEL Spans

4. The operator MUST create a root span `proposal.lifecycle` when it first detects a new Proposal CR. The OTEL trace ID MUST be the Proposal's `metadata.uid` with hyphens stripped.

5. On operator restart, the operator MUST read the Proposal's `metadata.uid` from the CR and resume the trace by constructing a SpanContext with the same trace ID.

6. Child spans MUST be created for each phase: `proposal.analyze`, `proposal.human_approval`, `proposal.execute`, `proposal.verify`, `proposal.escalate`.

7. `proposal.human_approval` starts when the operator begins waiting for approval and ends when the ProposalApproval PATCH is observed. Duration = human decision time.

8. On retry (verification failure → re-execute), new `proposal.execute` and `proposal.verify` child spans are created under the same root. The retry index MUST be a span attribute.

### Trace Propagation

9. The operator MUST propagate trace context to the sandbox via W3C `traceparent` header on all `/v1/agent/run` HTTP calls. The trace ID in the header is the Proposal's `metadata.uid` (hyphens stripped).

### Mutating Admission Webhook

10. The operator MUST host a MutatingAdmissionWebhook for `PATCH` operations on `proposalapprovals.agentic.openshift.io/v1alpha1`.

11. The webhook MUST read `request.userInfo.username` and `request.userInfo.uid` from the AdmissionReview and write them into `spec.approver.uid`, `spec.approver.username`, and `spec.approver.timestamp` (server-side `time.Now()`) on the CR, overwriting any client-submitted values.

12. The webhook MUST emit the `audit.approval.received` log event with user identity and `trace_id` (Proposal's `metadata.uid`, read from the CR's owner reference UID field).

13. The webhook MUST be fail-closed — if the webhook is unavailable, the API server rejects the PATCH.

14. The webhook runs in the same controller-manager process — same binary, same logger, same OTEL tracer.

### CRD Changes

15. The ProposalApproval CRD MUST add `spec.approver` with fields:
    - `uid` (string) — from `userInfo.uid`, webhook-authoritative
    - `username` (string) — from `userInfo.username`, webhook-authoritative
    - `timestamp` (string, RFC3339) — server-side `time.Now()`, webhook-authoritative

### Configuration

16. The operator reads audit config from the `AgenticOLSConfig` CR at `spec.audit`. If `spec.audit` is absent, the default is `enabled: true` with no OTEL export.

17. When `spec.audit.enabled` is `true` (or absent — default), all audit events emit. When explicitly `false`, no audit events emit.

18. When `spec.audit.otel.endpoint` is set, the operator configures an OTLP exporter pointed at that endpoint. When empty or absent, a no-op exporter is used.

19. The operator MUST pass the OTEL endpoint to the sandbox via environment variable or config mount so the sandbox can configure its own exporter.

### Structured JSON Format

20. All audit events MUST be single JSON lines to stdout with at minimum: `timestamp`, `level`, `event`, `trace_id`. The existing logr+zap setup already produces structured JSON — audit events use the same logger.

## Cross-References

- `proposal-lifecycle.md` — phase transitions where audit events are emitted
- `approval.md` — approval flow and ProposalApproval CR
- `sandbox-execution.md` — sandbox HTTP calls where trace context is propagated
- `crd-api.md` — CRD definitions (ProposalApproval needs `spec.approver` addition)
