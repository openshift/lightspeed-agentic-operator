package proposal

import (
	"encoding/json"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

// Default JSON Schemas sent to the agent for LLM structured output enforcement.
// Each phase has a known response shape. For analysis, spec.analysisOutput
// controls the base schema (Default or Minimal mode) and optionally injects
// a custom schema as a required "components" property in each option.

var AnalysisOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "options": {
      "type": "array",
      "description": "One or more remediation options, ordered by recommendation. Provide at least one.",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string", "description": "Short human-readable title for this option (e.g., 'Increase memory limit', 'Scale horizontally')" },
          "summary": { "type": "string", "description": "Brief one-paragraph summary of this remediation approach" },
          "diagnosis": {
            "type": "object",
            "properties": {
              "summary": { "type": "string", "description": "Markdown-formatted root cause analysis explaining the problem, symptoms, and findings" },
              "confidence": { "type": "string", "enum": ["Low", "Medium", "High"], "description": "Your confidence in this diagnosis. Low: ambiguous symptoms. Medium: likely cause identified. High: clear, deterministic root cause" },
              "rootCause": { "type": "string", "description": "Concise one-line root cause (e.g., 'OOMKilled due to memory limit of 256Mi')" }
            },
            "required": ["summary", "confidence", "rootCause"]
          },
          "proposal": {
            "type": "object",
            "properties": {
              "description": { "type": "string", "description": "Markdown-formatted summary of the overall remediation approach" },
              "actions": {
                "type": "array",
                "description": "Ordered list of discrete actions to perform",
                "items": {
                  "type": "object",
                  "properties": {
                    "type": { "type": "string", "description": "Action category (e.g., 'patch', 'scale', 'restart', 'create', 'delete', 'rollout')" },
                    "description": { "type": "string", "description": "What this action does (e.g., 'Increase memory limit from 256Mi to 512Mi')" }
                  },
                  "required": ["type", "description"]
                }
              },
              "risk": { "type": "string", "enum": ["Low", "Medium", "High", "Critical"], "description": "Risk assessment. Low: safe to apply. Medium: review recommended. High: careful review required. Critical: manual approval strongly recommended" },
              "reversible": { "type": "string", "enum": ["Reversible", "Irreversible", "Partial"], "description": "Whether this remediation can be rolled back. Reversible: fully undoable. Irreversible: cannot undo. Partial: some actions undoable" },
              "estimatedImpact": { "type": "string", "description": "Expected impact on the system (e.g., 'Brief pod restart, ~30s downtime')" },
              "rollbackPlan": {
                "type": "object",
                "description": "How to undo the remediation if it fails or causes issues. Required when reversible is Reversible or Partial.",
                "properties": {
                  "description": { "type": "string", "description": "How to undo the remediation if it fails or causes issues" },
                  "command": { "type": "string", "description": "The rollback command or steps to execute" }
                },
                "required": ["description", "command"]
              }
            },
            "required": ["description", "actions", "risk", "reversible"]
          },
          "verification": {
            "type": "object",
            "properties": {
              "description": { "type": "string", "description": "Summary of how to verify the remediation worked" },
              "steps": {
                "type": "array",
                "description": "Ordered verification checks for the verification agent to run after execution",
                "items": {
                  "type": "object",
                  "properties": {
                    "name": { "type": "string", "description": "Short check identifier (e.g., 'pod-running', 'memory-usage-normal')" },
                    "command": { "type": "string", "description": "Command or API call to run (e.g., 'oc get pod -n production -l app=web -o jsonpath={.status.phase}')" },
                    "expected": { "type": "string", "description": "Expected output or condition (e.g., 'Running', 'ready=true')" },
                    "type": { "type": "string", "description": "Check category (e.g., 'command', 'metric', 'condition')" }
                  }
                }
              }
            }
          },
          "rbac": {
            "type": "object",
            "properties": {
              "namespaceScoped": {
                "type": "array",
                "description": "RBAC rules scoped to the proposal's target namespaces",
                "items": {
                  "type": "object",
                  "properties": {
                    "namespace": { "type": "string", "description": "Target namespace for this rule. Must match one of the proposal's targetNamespaces" },
                    "apiGroups": { "type": "array", "items": { "type": "string" }, "description": "API groups (e.g., '', 'apps', 'batch'). Use empty string '' for the core API group (pods, services, configmaps, etc.)" },
                    "resources": { "type": "array", "items": { "type": "string" }, "description": "Resource types (e.g., 'pods', 'deployments', 'configmaps')" },
                    "resourceNames": { "type": "array", "items": { "type": "string" }, "description": "Restrict to specific named resources. Omit to allow all resources of the given type" },
                    "verbs": { "type": "array", "items": { "type": "string" }, "description": "Allowed operations (e.g., 'get', 'list', 'patch', 'delete')" },
                    "justification": { "type": "string", "description": "Why this permission is needed (e.g., 'Need to patch deployment to increase memory limit')" }
                  },
                  "required": ["apiGroups", "resources", "verbs", "justification"]
                }
              },
              "clusterScoped": {
                "type": "array",
                "description": "RBAC rules for cluster-wide or non-namespaced resources (e.g., nodes, CRDs)",
                "items": {
                  "type": "object",
                  "properties": {
                    "apiGroups": { "type": "array", "items": { "type": "string" }, "description": "API groups (e.g., '', 'apps', 'batch'). Use empty string '' for the core API group (pods, services, configmaps, etc.)" },
                    "resources": { "type": "array", "items": { "type": "string" }, "description": "Resource types (e.g., 'nodes', 'clusterroles')" },
                    "resourceNames": { "type": "array", "items": { "type": "string" }, "description": "Restrict to specific named resources. Omit to allow all resources of the given type" },
                    "verbs": { "type": "array", "items": { "type": "string" }, "description": "Allowed operations (e.g., 'get', 'list', 'patch', 'delete')" },
                    "justification": { "type": "string", "description": "Why this permission is needed" }
                  },
                  "required": ["apiGroups", "resources", "verbs", "justification"]
                }
              }
            }
          }
        },
        "required": ["title", "diagnosis", "proposal", "verification"]
      }
    }
  },
  "required": ["options"]
}`)

// MinimalAnalysisOutputSchema is the base analysis output schema used
// when AnalysisOutputMode is Minimal. It contains only the options array
// with title per option. Built-in properties (diagnosis, proposal, rbac,
// verification, summary) are omitted. If execution or verification steps
// exist, those specific property definitions are added back dynamically.
var MinimalAnalysisOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "options": {
      "type": "array",
      "description": "One or more output options, ordered by recommendation. Provide at least one.",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string", "description": "Short human-readable title for this option" }
        },
        "required": ["title"]
      }
    }
  },
  "required": ["options"]
}`)

var ExecutionOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "success": { "type": "boolean", "description": "Whether all execution actions completed successfully" },
    "actionsTaken": {
      "type": "array",
      "description": "List of actions actually performed, in order",
      "items": {
        "type": "object",
        "properties": {
          "type": { "type": "string", "description": "Action category (e.g., 'patch', 'scale', 'restart')" },
          "description": { "type": "string", "description": "What was done (e.g., 'Patched deployment/web to set memory limit to 512Mi')" },
          "outcome": { "type": "string", "enum": ["Succeeded", "Failed"], "description": "Whether this individual action succeeded or failed" },
          "output": { "type": "string", "description": "Command output or API response from the action" },
          "error": { "type": "string", "description": "Error message if the action failed" }
        },
        "required": ["type", "description", "outcome"]
      }
    },
    "verification": {
      "type": "object",
      "description": "Lightweight inline verification performed immediately after execution",
      "properties": {
        "conditionOutcome": { "type": "string", "enum": ["Improved", "Unchanged", "Degraded"], "description": "Whether the target condition improved after remediation" },
        "summary": { "type": "string", "description": "Brief inline verification summary of what you observed after applying the fix" }
      }
    }
  },
  "required": ["success", "actionsTaken"]
}`)

var VerificationOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "success": { "type": "boolean", "description": "Whether all verification checks passed" },
    "checks": {
      "type": "array",
      "description": "Individual verification check results",
      "items": {
        "type": "object",
        "properties": {
          "name": { "type": "string", "description": "Check identifier matching the analysis verification plan (e.g., 'pod-running')" },
          "source": { "type": "string", "description": "The full command that was run (e.g., 'oc get pod -n production -o jsonpath={.status.phase}', 'promql: rate(container_cpu_usage_seconds_total[5m])')" },
          "value": { "type": "string", "description": "Actual observed value (e.g., 'Running', '3 replicas')" },
          "result": { "type": "string", "enum": ["Passed", "Failed"], "description": "Whether the observed value matches expectations" }
        },
        "required": ["name", "result"]
      }
    },
    "summary": { "type": "string", "description": "Overall verification summary in Markdown" }
  },
  "required": ["success", "checks", "summary"]
}`)

var EscalationOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "success": { "type": "boolean", "description": "Whether the escalation analysis completed successfully" },
    "summary": { "type": "string", "description": "Markdown summary of the escalation analysis and recommendations" },
    "content": { "type": "string", "description": "Detailed escalation content: root cause analysis across all failed attempts, recommended next steps, and any information for the escalation target" }
  },
  "required": ["success", "summary", "content"]
}`)

var defaultOutputSchemas = map[string]json.RawMessage{
	"analysis":     AnalysisOutputSchema,
	"execution":    ExecutionOutputSchema,
	"verification": VerificationOutputSchema,
	"escalation":   EscalationOutputSchema,
}

// builtInPropertyJSON holds pre-serialized property definitions from
// AnalysisOutputSchema. Stored as raw JSON to avoid shared map aliasing
// when injected into Minimal mode schemas.
var builtInPropertyJSON map[string]json.RawMessage

func init() {
	var schema map[string]any
	_ = json.Unmarshal(AnalysisOutputSchema, &schema)
	options := schema["properties"].(map[string]any)["options"].(map[string]any)
	items := options["items"].(map[string]any)
	allProps := items["properties"].(map[string]any)

	builtInPropertyJSON = make(map[string]json.RawMessage, len(allProps))
	for k, v := range allProps {
		b, _ := json.Marshal(v)
		builtInPropertyJSON[k] = b
	}
}

func injectBuiltInProperty(props map[string]any, name string) {
	var v any
	_ = json.Unmarshal(builtInPropertyJSON[name], &v)
	props[name] = v
}

func outputSchemaForStep(stepName string, proposal *agenticv1alpha1.Proposal) json.RawMessage {
	if stepName != "analysis" {
		return defaultOutputSchemas[stepName]
	}

	mode := proposal.Spec.AnalysisOutput.Mode
	if mode == "" {
		mode = agenticv1alpha1.AnalysisOutputModeDefault
	}
	customSchema := proposal.Spec.AnalysisOutput.Schema

	var baseSchema json.RawMessage
	if mode == agenticv1alpha1.AnalysisOutputModeMinimal {
		baseSchema = MinimalAnalysisOutputSchema
	} else {
		baseSchema = AnalysisOutputSchema
	}

	var schema map[string]any
	_ = json.Unmarshal(baseSchema, &schema)
	options := schema["properties"].(map[string]any)["options"].(map[string]any)
	items := options["items"].(map[string]any)
	props := items["properties"].(map[string]any)

	required := []any{"title"}
	if mode == agenticv1alpha1.AnalysisOutputModeDefault {
		required = append(required, "diagnosis", "proposal")
	}

	if !proposal.Spec.Execution.IsZero() {
		if mode == agenticv1alpha1.AnalysisOutputModeMinimal {
			injectBuiltInProperty(props, "proposal")
			injectBuiltInProperty(props, "rbac")
			required = append(required, "proposal")
		}
		required = append(required, "rbac")
	}

	if !proposal.Spec.Verification.IsZero() {
		if mode == agenticv1alpha1.AnalysisOutputModeMinimal {
			injectBuiltInProperty(props, "verification")
		}
		required = append(required, "verification")
	}

	if customSchema != nil {
		var components any
		if b, err := json.Marshal(customSchema); err == nil {
			_ = json.Unmarshal(b, &components)
			props["components"] = components
			required = append(required, "components")
		}
	}

	items["required"] = required
	result, _ := json.Marshal(schema)
	return result
}
