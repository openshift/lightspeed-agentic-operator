# Future: ConfigMap-based agent communication

## Current approach

The operator creates a sandbox pod running an HTTP server. It calls `POST /v1/agent/run` synchronously, blocking the reconcile loop until the agent responds (up to 5 minutes). The agent is a long-running HTTP server that must stay alive for the duration of the sandbox claim.

## Proposed alternative: run-to-completion with file I/O

Replace the HTTP server with a batch CLI program that reads input from a mounted file and writes output to stdout (or a known file path).

### Flow

1. **Operator creates input ConfigMap** with the request payload:
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: ls-analysis-<proposal>-input
   data:
     request.json: |
       {"query":"...","outputSchema":{...},"context":{...}}
   ```

2. **Operator creates sandbox** — pod spec mounts the ConfigMap as a volume at `/input/request.json`. Pod command is the agent CLI (not an HTTP server). Pod `restartPolicy: Never`.

3. **Operator pre-creates output ConfigMap** (empty):
   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: ls-analysis-<proposal>-output
     labels:
       agentic.openshift.io/proposal: <proposal>
       agentic.openshift.io/step: analysis
   data: {}
   ```

5. **Agent runs** — reads `/input/request.json`, calls LLM, writes result to the output ConfigMap:
   ```go
   cm.Data["response.json"] = string(resultJSON)
   client.Update(ctx, cm)
   ```
   Agent SA only needs `update` on this specific ConfigMap (narrow Role scoped by resource name).

6. **Pod completes** (exit 0 = success, non-zero = failure).

7. **Operator detects completion** — watches pod status or sandbox condition. Reads `data["response.json"]` from the output ConfigMap. Operator also runs periodic checks (e.g. on requeue timer) to detect runaway agents that exceed expected duration and deletes them (kills the pod, marks step as failed).

8. **Operator parses output** — same JSON contract as today, just read from ConfigMap instead of HTTP response body. Operator deletes both input and output ConfigMaps after processing.

### Advantages

- **No HTTP server** — agent is a simple CLI: read file → call LLM → write ConfigMap → exit
- **No network policies** — no service discovery, no port exposure, no DNS resolution issues
- **No long-running reconcile** — operator creates sandbox and returns. Reconciles again on pod completion event.
- **Simpler agent image** — no `net/http`, no healthz, no request parsing. Just a `main()` that processes one request.
- **Natural timeout** — pod `activeDeadlineSeconds` handles timeouts at the k8s level
- **Crash recovery** — if operator restarts, it checks pod status on next reconcile (no lost HTTP connection)
- **Operator controls lifecycle** — creates both input and output ConfigMaps, owns them, deletes them after use
- **Minimal agent RBAC** — only needs `update` on the single output ConfigMap (can be scoped by resourceName in the Role)

### Disadvantages

- **ConfigMap 1MB limit** — large outputs could hit this (unlikely in practice; current responses are <10KB)
- **No streaming** — can't stream partial results (current HTTP also doesn't stream, so no regression)
- **Slightly slower** — volume mount propagation adds ~1-2s vs direct HTTP
- **Agent needs k8s client** — must write to ConfigMap (lightweight: just one Update call, no watches)

### Agent code (simplified)

```go
func main() {
    // Read input from mounted ConfigMap volume
    input, _ := os.ReadFile("/input/request.json")
    
    var req Request
    json.Unmarshal(input, &req)
    
    result := callLLM(req)
    
    // Write output to the pre-created output ConfigMap
    resultJSON, _ := json.Marshal(result)
    cm := &corev1.ConfigMap{}
    client.Get(ctx, types.NamespacedName{
        Name:      os.Getenv("OUTPUT_CONFIGMAP"),
        Namespace: os.Getenv("POD_NAMESPACE"),
    }, cm)
    cm.Data["response.json"] = string(resultJSON)
    client.Update(ctx, cm)
}
```

### Operator changes

1. `callWithSandbox` → `dispatchToSandbox` (non-blocking: create ConfigMap + SandboxClaim, return)
2. New reconcile path: when pod completes → read output → continue to next phase
3. `SandboxManager.WaitReady` replaced by pod completion watch
4. `AgentHTTPClient` removed entirely

### Migration path

1. Support both modes behind a feature flag (HTTP vs file I/O)
2. Default to HTTP (current behavior)
3. Switch to file I/O once validated
4. Remove HTTP path

### RBAC for agent

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-output-writer
  namespace: default
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["ls-analysis-<proposal>-output"]
  verbs: ["get", "update"]
```

The operator creates this Role + RoleBinding per sandbox (scoped to the specific output ConfigMap name). Deleted on cleanup.

### Open questions

- Should the input ConfigMap be cleaned up by the operator or by the sandbox controller?
- How does the console stream agent logs during execution if the agent is a batch job? (Currently it reads sandbox pod logs — same approach works here)
- Should the output ConfigMap name be passed as env var (`OUTPUT_CONFIGMAP`) or derived from a convention?
