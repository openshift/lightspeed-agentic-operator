# `oc-agentic` CLI — architecture (how)

Audience: AI agents. Command behavior and user-facing rules belong in **what/** specs where applicable (e.g. `what/approval.md` for approval semantics). This document describes **code layout, client wiring, and I/O paths**.

---

## Entry point: `cmd/oc-agentic/main.go`

- Builds `genericclioptions.IOStreams` from `os.Stdin` / `Stdout` / `Stderr`.
- `cli.NewRootCmd(streams).Execute()` — Cobra root.

---

## Module map: `cli/`

| File | Types | Key functions |
|------|-------|----------------|
| `root.go` | — | `NewRootCmd(streams)` — registers `proposal` subtree, `system` commands (`status`, `suspend`, `resume`), and `version` |
| `version.go` | Package var `Version` (default `dev`) | `NewVersionCmd(streams)` |

---

## Module map: `cli/system/`

| File | Types | Key functions |
|------|-------|----------------|
| `helpers.go` | — (`scheme`, `configName` constant) | `newClient`, `getConfig` |
| `status.go` | `StatusOptions` | `NewStatusCmd`, `Complete`, `Run` |
| `suspend.go` | `SuspendOptions` | `NewSuspendCmd`, `Complete`, `Run` |
| `resume.go` | `ResumeOptions` | `NewResumeCmd`, `Complete`, `Run` |

- **`status`:** Gets `AgenticOLSConfig` named `cluster`; prints `"Agentic System: SUSPENDED"` or `"Active"`. When the `Suspended` status condition is present, includes relative/absolute transition time and condition message (suspended) or resume time (`AdminDeactivated`). Falls back to spec-only lines while suspension is in progress. `NotFound` = Active.
- **`suspend`:** Prompts confirmation (skip with `--yes`); patches `spec.suspended=true` or creates the CR if absent. Idempotent (reports "already suspended" if already true).
- **`resume`:** Patches `spec.suspended=false`. Reports "not suspended" if already false or CR absent.

---

## Module map: `cli/proposal/`

| File | Types | Key functions |
|------|-------|----------------|
| `proposal.go` | — | `NewProposalCmd(streams)` — registers subcommands |
| `helpers.go` | Color constants; `scheme` (`runtime.Scheme` with client-go + `agenticv1alpha1`); output constants | `NewClient`, `NewClientFromConfig`, `ResolveNamespace`, `IsTerminalPhase`, `PhaseColor`, `ColoredPhase`, `HumanDuration`, `PrintTable`, `MarshalOutput`, `SortProposalsByAge`, `IsValidPhase`, `IsValidStep`, `NormalizeStep`, `ValidateOutputFormat`, `stepStatusFromConditions`, `valueOrDash`, `int32PtrStr` |
| `create.go` | `CreateOptions` (embeds `IOStreams`, holds `configFlags`, typed `client.Client`, namespace) | `NewCreateCmd`, `Complete`, `Validate`, `Run` |
| `list.go` | `ListOptions` | `NewListCmd`, `Complete`, `Validate`, `Run`, `printTable`, `printWideTable` |
| `get.go` | `GetOptions` | `NewGetCmd`, `Complete`, `Validate`, `Run`, `printDetail` |
| `approve.go` | `ApproveOptions` | `NewApproveCmd`, `Complete`, `Validate`, `Run`, `getOrCreateApproval`, `pendingStages`, `normalizeStageType` |
| `deny.go` | `DenyOptions` | `NewDenyCmd`, `Complete`, `Run`, `nextPendingStage` |
| `delete.go` | `DeleteOptions` | `NewDeleteCmd`, `Complete`, `Run` |
| `watch.go` | `WatchOptions`; package var `proposalGVR` | `NewWatchCmd`, `Complete`, `Run`, `doWatch`, `extractConditions` |
| `logs.go` | `LogsOptions` | `NewLogsCmd`, `Complete`, `Validate`, `Run`, `resolveSandbox` |

`*_test.go` files under `cli/` exercise commands (not fully enumerated here).

---

## Command tree

```
oc-agentic
├── proposal (aliases: proposals, prop)
│   ├── list (ls)
│   ├── get NAME
│   ├── create
│   ├── approve NAME
│   ├── deny NAME
│   ├── watch NAME
│   ├── logs NAME
│   └── delete NAME
├── status
├── suspend
├── resume
└── version
```

---

## Kubernetes client usage (not generic “dynamic” for CRUD)

- **Primary:** `sigs.k8s.io/controller-runtime/pkg/client.Client` constructed by `NewClient(configFlags)` → `configFlags.ToRESTConfig()` → `client.New(cfg, client.Options{Scheme: scheme})`.
- **Typed API:** `Create`/`Get`/`List`/`Patch`/`Delete` use `agenticv1alpha1` types (`Proposal`, `ProposalList`, `ProposalApproval`).
- **Watch:** `cli/proposal/watch.go` uses **`k8s.io/client-go/dynamic`** `Resource(proposalGVR).Namespace(ns).Watch` with `FieldSelector` on `metadata.name` — events arrive as `*unstructured.Unstructured`.
- **Logs:** `k8s.io/client-go/kubernetes.Clientset` `CoreV1().Pods(ns).GetLogs(name, opts).Stream` — pod name taken from proposal status sandbox info (see below).

There is **no** unstructured client for proposal CRUD in the main commands; only watch uses dynamic + unstructured.

---

## `genericclioptions` integration

- Every command embeds or holds `*genericclioptions.ConfigFlags` from `genericclioptions.NewConfigFlags(true)`.
- **`AddFlags`** on each cobra command wires kubeconfig/context/namespace flags consistent with `kubectl` / `oc`.
- **`IOStreams`** passed from root for all printing and JSON/YAML encoders.
- **Namespace resolution:** `ResolveNamespace` prefers `*ConfigFlags.Namespace`; else kubeconfig current context namespace (unless `"default"`); else `"openshift-lightspeed"` (the `DefaultNamespace` constant).

---

## Per-command API behavior (concise)

- **`create`:** Builds `Proposal` with `GenerateName: "ag-"`, `Spec.Request`, `TargetNamespaces`, `Analysis.Agent` from flag default `"default"`. `client.Create`. Output: line message or `-o json|yaml` via `MarshalOutput`.
- **`list`:** `client.List` `ProposalList`, optional `client.InNamespace`, filter by `--phase` using `agenticv1alpha1.DerivePhase` on each item. Table via `PrintTable` + `ColoredPhase` + `HumanDuration`; `-o wide` adds target namespaces column; `-A` lists cluster-wide.
- **`get`:** `client.Get` by name; human-readable sections from `Spec`, `Status.Steps`, and `Status.Conditions` (step summaries via `stepStatusFromConditions`).
- **`approve`:** Loads `Proposal`; `getOrCreateApproval` (get or create `ProposalApproval` with owner ref — create path omits controller flags present in controller’s `ensureProposalApproval`; operator reconciler may enrich). Builds `[]ApprovalStage` entries, `client.Patch(MergeFrom)` on approval. `--all` uses `pendingStages` derived from spec non-zero steps vs existing stage types. `--wait` delegates to `doWatch`.
- **`deny`:** Requires existing `ProposalApproval`; appends denied stage with `ApprovalDecisionDenied`. `--stage` defaults via `nextPendingStage` walk order analysis → execution → verification.
- **`delete`:** `client.Delete` minimal `Proposal` object keyed by name/namespace.
- **`watch`:** Dynamic watch; `extractConditions` pulls `status.conditions` into `[]metav1.Condition`; phase from `DerivePhase`; prints only on phase change; stops when `IsTerminalPhase` (Completed, Failed, Escalated, Denied — note helpers.go set).
- **`logs`:** Loads proposal via controller-runtime client; `resolveSandbox` picks explicit `--step` (normalized via `NormalizeStep`) or prefers verification, then execution, then analysis sandbox info. Uses **`SandboxInfo.ClaimName` as pod name** and `SandboxInfo.Namespace` (fallback proposal namespace). Streams with optional `-f`.

---

## Output formatting

- **Tables:** `text/tabwriter` via `PrintTable`.
- **Phases:** ANSI colors in `PhaseColor` / `ColoredPhase` (green complete, red failed/denied, yellow in-progress, magenta escalated/emergency-stopped).
- **Structured:** `-o json` uses `encoding/json` encoder with indent; `-o yaml` uses `sigs.k8s.io/yaml` `Marshal`.
- **Validation:** `ValidateOutputFormat` — list allows `wide` in addition to json/yaml; get/create allow json/yaml only.

---

## Shared helpers (`cli/proposal/helpers.go`)

- **Scheme:** Registers `clientgoscheme` + `agenticv1alpha1` for typed client.
- **Phase/step validation:** `validProposalPhases` slice must stay aligned with API constants (comment in file). `validSandboxSteps` for logs `--step`.
- **Sorting:** `SortProposalsByAge` descending by `CreationTimestamp`.
- **Duration:** `k8s.io/apimachinery/pkg/util/duration.HumanDuration`.

---

## Data Flow

```
User invokes oc-agentic proposal <cmd> [flags]
  │
  ├─ Cobra dispatches to subcommand Run()
  │    ├─ Complete(): resolve namespace, build K8s client
  │    ├─ Validate(): check required args, output format
  │    └─ Run():
  │         ├─ CRUD commands → controller-runtime typed client → API server
  │         ├─ watch → dynamic client Watch() → event loop → phase change detection → terminal check
  │         └─ logs → clientset Pods().GetLogs().Stream() → io.Copy to stdout
  │
  └─ Output: table (tabwriter), JSON (encoding/json), YAML (sigs.k8s.io/yaml), or colored text
```

## Key Abstractions

- **Typed client** (`controller-runtime`): Used for all CRUD operations on `Proposal`, `ProposalApproval`. Provides compile-time safety via `agenticv1alpha1` types.
- **Dynamic client** (`client-go`): Used only for watch operations. Returns `*unstructured.Unstructured` events decoded via `DerivePhase`.
- **Clientset** (`client-go`): Used only for pod log streaming. Direct CoreV1 API access.
- **`ConfigFlags`**: Standard kubeconfig/context/namespace flag bundle shared across all subcommands.
- **Phase derivation dependency**: CLI uses `agenticv1alpha1.DerivePhase` from the API package directly, ensuring phase logic stays in sync between operator and CLI.

## Cross-references

- Proposal phases and condition → phase mapping: **`api/v1alpha1` `DerivePhase`** — see **what/proposal-lifecycle.md** (not duplicated here).
- Approval stage types and policy interaction: reconciler and CLI both append to `ProposalApproval.Spec.Stages` — see **what/approval.md**.
- Sandbox claim/pod relationship for log streaming: **what/sandbox-execution.md** if documented; this how spec notes the CLI’s use of `Status.Steps.*.Sandbox` only.

---

## Implementation notes

- **`validProposalPhases` in `helpers.go`** includes all terminal phases including `EmergencyStopped`. Still missing `Proposed` and `Escalating` relative to `ProposalPhase` in `api/v1alpha1/proposal_types.go`. `list --phase` validation (`IsValidPhase`) can reject phase strings that `DerivePhase` still produces; align the slice with API constants when fixing UX.
- **`watch` terminal set:** `IsTerminalPhase` matches five phases (includes `Escalated` and `EmergencyStopped`); matches common completion paths; verify against **what/** if `Analyzing` as terminal edge cases matter.
