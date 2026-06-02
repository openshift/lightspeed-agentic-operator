# Sandbox execution and agent I/O

Behavioral specification for how workflow steps run inside ephemeral **sandboxes** and how the operator talks to the agent HTTP API. **Approval gates** are in `approval.md`. **CRD fields** for tools, secrets, and agents are in `crd-api.md`.

## Behavioral Rules

1. **Sandbox API objects**: For each step invocation, the controller MUST create a namespaced **SandboxClaim** (API group `extensions.agents.x-k8s.io`, `v1alpha1`) that references a **SandboxTemplate** by name and uses a shutdown policy that deletes the sandbox when released.
2. **Operator namespace**: Sandbox claims and sandbox workloads MUST reside in the **operator’s configured namespace** (process flag `namespace` or equivalent environment substitution), not necessarily the `Proposal` namespace.
3. **Template selection**: Before claiming, the controller MUST ensure a **derived** `SandboxTemplate` exists: it clones a **base** template identified by configured default template name, patches it with generic LLM configuration env vars (see rule 16a), credential mounts, tools (skills, MCP, required secrets), and step mode, and names it deterministically using a hash of relevant inputs so identical configurations reuse the same template. The operator MUST NOT set SDK-specific env vars (e.g. `ANTHROPIC_MODEL`, `CLAUDE_CODE_USE_VERTEX`, `OPENAI_BASE_URL`); all SDK-specific mapping is the sandbox's responsibility.
4. **Template immutability & GC**: If a derived template for the same agent + step + hash already exists, creation MUST be a no-op. Older derived templates for the same agent+step with different names SHOULD be garbage-collected after successful creation of the newest.
5. **Claim naming**: Claim names MUST be derived from proposal name and step label, truncated to valid Kubernetes name length limits.
6. **Readiness**: The controller MUST poll sandbox/claim status until the backing `Sandbox` reports `Ready=True` (standard condition pattern) and exposes a **service FQDN** for in-cluster HTTP, or until a configurable **sandbox wait budget** elapses (error path).
7. **Endpoint construction**: Agent HTTP URL MUST be formed from the readiness endpoint; if the endpoint is not already an absolute URL with HTTP scheme, the client MUST prefix standard cluster HTTP scheme and port expected for the agent container.
8. **HTTP contract**: Each step MUST call the agent **`POST /v1/agent/run`** with JSON body carrying at least `query`, `outputSchema`, and `context`; optional `systemPrompt` and `timeout_ms` exist in the wire shape but **system prompt MUST be sent empty** in the current implementation (prompt material lives in `query` and templates).
9. **Response handling**: HTTP success responses MUST be parsed as JSON matching the per-step schema (analysis/execution/verification/escalation). Non-success HTTP MUST fail the step with an error surfaced to proposal conditions.
10. **Output schema selection**: `outputSchema` MUST be the step-specific JSON schema: analysis schema depends on `spec.analysisOutput.mode`, whether execution/verification steps exist in the proposal, and optional injected `components` sub-schema from `spec.analysisOutput.schema`; other steps use fixed schemas for their response shapes.
11. **Analysis query payload**: The `query` string MUST encode the user request or revision-augmented request and encode workflow flags indicating whether execution/verification steps exist (template-rendered).
12. **Execution query payload**: The `query` MUST include JSON describing the approved remediation option.
13. **Verification query payload**: The `query` MUST include the approved option JSON and a JSON description of the latest execution output (actions and inline verification) when available.
14. **Context envelope**: The `context` object MUST include `targetNamespaces` from `spec.targetNamespaces`, synthesized `previousAttempts` from failed prior `status.steps.*.results` entries, `approvedOption` when executing/verifying, and `executionResult` when verifying. Note: the sandbox context prefix formatter (see sandbox `run-api.md`) only expands `targetNamespaces`, `attempt`, `previousAttempts`, and `approvedOption` into the model prompt; `executionResult` is carried in `context` for tracing but verification execution details are primarily conveyed to the model via the `query` body (rendered from the verification template).
15. **Secrets — proposal** `spec.tools.requiredSecrets` / per-step tools: Secret objects MUST live in the **same namespace as the `Proposal`**. Mounting into sandbox MUST honor `SecretMountSpec`: environment variable injection OR file mount at configured absolute path.
16. **Secrets — LLM credentials**: LLM provider credentials MUST be loaded from secret names declared on the `LLMProvider` and wired into the derived template via `envFrom` (all secret keys as env vars) AND a read-only volume mount at `/var/run/secrets/llm-credentials/`. Both mounts MUST be unconditional regardless of provider type — the sandbox uses whichever form its SDK requires. The operator MUST NOT set individual credential env vars by name.
16a. **LLM configuration env vars**: The operator MUST set the following generic env vars on the derived template. The operator MUST NOT set any SDK-specific env vars — the sandbox maps these generic vars to SDK-specific vars internally.

    | Env var | Required | Source |
    |---|---|---|
    | `LIGHTSPEED_PROVIDER` | Yes | `LLMProvider.spec.type` mapped to: `anthropic`, `vertex`, `openai`, `azure`, `bedrock` |
    | `LIGHTSPEED_MODEL` | Yes | `Agent.spec.model` |
    | `LIGHTSPEED_MODEL_PROVIDER` | When provider=`vertex` | `googleCloudVertex.modelProvider` (`Anthropic`, `Google`, `OpenAI`) |
    | `LIGHTSPEED_MODE` | Yes | Workflow step name |
    | `LIGHTSPEED_PROVIDER_URL` | When URL set on provider config | URL override from active provider config branch |
    | `LIGHTSPEED_PROVIDER_PROJECT` | When applicable | `googleCloudVertex.projectID` |
    | `LIGHTSPEED_PROVIDER_REGION` | When applicable | `googleCloudVertex.region` or `awsBedrock.region` |
    | `LIGHTSPEED_PROVIDER_API_VERSION` | When applicable | `azureOpenAI.apiVersion` |

    Provider type mapping from CRD `spec.type`:

    | CRD `spec.type` | `LIGHTSPEED_PROVIDER` value |
    |---|---|
    | `anthropic` | `anthropic` |
    | `googleCloudVertex` | `vertex` |
    | `openAI` | `openai` |
    | `azureOpenAI` | `azure` |
    | `awsBedrock` | `bedrock` |
17. **Secrets — MCP headers**: When an MCP header sources a Secret, the template MUST mount that secret on a dedicated read-only path suitable for header injection configuration.
18. **Skills volumes**: Skills MUST be conveyed as OCI image volume(s) on the sandbox pod template; when `SkillsSource.paths` is set, the controller MUST mount each path as a `subPath` under the configured skills mount root using stable mount naming derived from the path’s final segment. When multiple `skills` entries exist in `ToolsSpec`, template derivation MUST apply image/path patching based on the **first** non-empty skills source (current behavior).
19. **MCP servers**: MCP configuration MUST be serialized to an environment variable payload listing servers, URLs, timeouts, and header sources so the agent runtime can open MCP connections without CR-specific code in the agent.
20. **Sandbox observability patch**: Immediately after creating a claim, the controller MUST patch `Proposal.status.steps.<step>.sandbox` with claim name and operator namespace so consoles/CLIs can tail logs before the sandbox is ready.
21. **Execution RBAC materialization**: When the approved remediation option includes RBAC requests, before execution the controller MUST create namespace-scoped `Role`+`RoleBinding` pairs in each target namespace ClusterRole+ClusterRoleBinding for cluster-scoped rules, binding subjects to the **sandbox service account** used by the template (cluster-wide default name configured in operator). Idempotent create MUST tolerate existing objects.
22. **RBAC subjects namespace**: RoleBindings MUST reference the service account in the **operator namespace** (where sandbox pods run), even when roles live in target namespaces.
23. **RBAC tracking annotation**: The controller SHOULD persist the list of namespaces receiving namespace-scoped RBAC on the `Proposal` via a dedicated annotation so cleanup can run after retries or status resets.
24. **RBAC cleanup**: When the proposal reaches configured terminal outcomes, fails fatally, completes escalation successfully, or is deleted, the controller MUST delete execution RBAC objects it created, using the annotation or equivalent persisted scope information.
25. **Finalizers**: Non-deleted proposals MUST gain a cleanup finalizer before leaving non-terminal phases so deletion can run RBAC and sandbox release hooks safely.
26. **Result CR writes**: After each successful or failed agent invocation (per step), the controller MUST create/update an `AnalysisResult`, `ExecutionResult`, `VerificationResult`, or `EscalationResult` with immutable spec, owner reference to the `Proposal`, started/completed conditions, embedded outcome payload, sandbox reference, and optional `failureReason` for system errors.
27. **Retry index**: `ExecutionResult` and `VerificationResult` MUST record the current execution retry index in spec for correlation with `status.steps.execution.retryCount`.
28. **Sandbox release**: On proposal deletion and on terminal phases (`Completed`, `Denied`, `Escalated`), the controller MUST delete known sandbox claims recorded under `status.steps.*.sandbox` (best-effort aggregation; first error MAY be returned for visibility).
29. **Concurrency cap**: Maximum concurrent proposal reconciles SHOULD respect `ApprovalPolicy.spec.maxConcurrentProposals` when present (see `crd-api.md`).

## Configuration Surface

- Operator process: namespace (operator install namespace), base sandbox template name
- `SandboxTemplate` base object name in operator namespace (default from operator bootstrap)
- `Proposal.metadata.namespace` (secrets + result CRs)
- `spec.tools`, per-step `spec.*.tools` (`SkillsSource`, `MCPServerConfig`, `SecretRequirement`)
- `spec.targetNamespaces` and RBAC materialization targets
- `spec.analysisOutput` (analysis schema behavior)
- `Agent.spec.model` → `LIGHTSPEED_MODEL`, `LLMProvider.spec.type` → `LIGHTSPEED_PROVIDER`, `LLMProvider.spec.*.credentialsSecret` (envFrom + volume mount), optional `LIGHTSPEED_PROVIDER_URL`, `LIGHTSPEED_PROVIDER_PROJECT`, `LIGHTSPEED_PROVIDER_REGION`, `LIGHTSPEED_PROVIDER_API_VERSION`, `LIGHTSPEED_MODEL_PROVIDER` (see rule 16a)
- `Proposal` annotation `agentic.openshift.io/rbac-namespaces` (RBAC scope for cleanup)
- `Proposal` finalizer string `agentic.openshift.io/execution-rbac-cleanup`

## Constraints

- Sandbox features that depend on **OCI image volumes** require Kubernetes version support as documented on `ToolsSpec` / `SkillsSource` API comments.
- Required Secret **keys** for optional proposal-mounted secrets using `EnvVar` MUST match what the template expects (MCP header secrets and generic required secrets may differ — MCP env-from pattern in implementation may expect a specific key name for token-like secrets).
- Agent HTTP is cluster-internal; clients MUST NOT assume public internet TLS semantics.

## Planned Changes

- [PLANNED: OLS-2957] **Sandbox template management** UX and CRD ergonomics (base/derived lifecycle, versioning) may change operator/template coupling described in rules 2–4.
- [PLANNED: OLS-3038] **TLS verification and network policy** for agent traffic may replace permissive internal TLS client behavior.
- [OLS-3153] ~~IMPLEMENTED~~ Operator-sandbox env var contract: SDK-specific env vars removed from operator; replaced by generic `LIGHTSPEED_*` vars (rule 16a). Sandbox handles all SDK-specific mapping internally. Supersedes OLS-3044 and OLS-3051.
- [PLANNED: OLS-2894] Support **multiple concurrent skills images** in template derivation beyond the first `skills` entry if product requires composite skill bundles.
