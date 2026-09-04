package agenticrun

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func TestBuildInputConfigMap(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-run",
			Namespace: "run-ns",
			UID:       types.UID("uid-aaaa-bbbb"),
		},
		Spec: agenticv1alpha1.AgenticRunSpec{
			Request:          "analyze this",
			TargetNamespaces: []string{"payments"},
		},
	}
	schema := json.RawMessage(`{"type":"object"}`)
	agentCtx := &agentContext{TargetNamespaces: []string{"payments"}}

	cm, err := buildInputConfigMap("op-ns", run, "analysis", nil, schema, agentCtx)
	if err != nil {
		t.Fatalf("buildInputConfigMap: %v", err)
	}
	if cm.Name != "ls-analysis-uid-aaaa-bbbb" {
		t.Errorf("name = %q, want ls-analysis-uid-aaaa-bbbb", cm.Name)
	}
	if cm.Namespace != "op-ns" {
		t.Errorf("namespace = %q, want op-ns", cm.Namespace)
	}
	if cm.Labels[LabelRun] != "uid-aaaa-bbbb" || cm.Labels[LabelStep] != "analysis" {
		t.Errorf("labels = %v", cm.Labels)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != run.UID {
		t.Errorf("ownerRefs = %+v", cm.OwnerReferences)
	}
	for _, key := range []string{inputConfigMapKeyQuery, inputConfigMapKeySchema, inputConfigMapKeyCtx, inputConfigMapKeyTmpl} {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("missing data key %q", key)
		}
	}
	if !strings.Contains(cm.Data[inputConfigMapKeyQuery], "analyze this") {
		t.Errorf("query should contain request text, got %q", cm.Data[inputConfigMapKeyQuery])
	}
	if cm.Data[inputConfigMapKeySchema] != string(schema) {
		t.Errorf("schema = %q", cm.Data[inputConfigMapKeySchema])
	}

	var tmpl map[string]any
	if err := json.Unmarshal([]byte(cm.Data[inputConfigMapKeyTmpl]), &tmpl); err != nil {
		t.Fatalf("result-template JSON: %v", err)
	}
	if tmpl["kind"] != "AnalysisResult" {
		t.Errorf("kind = %v", tmpl["kind"])
	}
	if _, hasStatus := tmpl["status"]; hasStatus {
		t.Error("result-template must not include status")
	}
	meta, _ := tmpl["metadata"].(map[string]any)
	if meta["name"] != resultCRName("my-run", "analysis", 1) {
		t.Errorf("template metadata.name = %v", meta["name"])
	}
	if meta["namespace"] != "op-ns" {
		t.Errorf("template metadata.namespace = %v", meta["namespace"])
	}
}

func TestResolvePrompts_DefaultBuiltinTemplates(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix the crash"},
	}
	agentCtx := &agentContext{}

	systemPrompt, query, err := resolvePrompts(nil, "analysis", "ns", run, agentCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "" {
		t.Errorf("expected empty system prompt for nil agent, got %q", systemPrompt)
	}
	if !strings.Contains(query, "fix the crash") {
		t.Errorf("query should contain request text, got %q", query)
	}
	if !strings.Contains(query, "analysis agent") {
		t.Errorf("query should contain built-in template text, got %q", query)
	}
}

func TestResolvePrompts_CustomSystemPromptWithCustomUser(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix it"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "You are a security auditor.",
					UserPrompt:   "Audit: {{.Request}}",
				},
			},
		},
	}

	systemPrompt, query, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "You are a security auditor." {
		t.Errorf("systemPrompt = %q, want custom", systemPrompt)
	}
	if query != "Audit: fix it" {
		t.Errorf("query = %q, want rendered custom template", query)
	}
}

func TestResolvePrompts_CustomUserPrompt(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "pod is crashing"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					UserPrompt: "Custom instructions.\n\n## Request\n\n{{.Request}}",
				},
			},
		},
	}

	systemPrompt, query, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "" {
		t.Errorf("expected empty system prompt, got %q", systemPrompt)
	}
	if !strings.Contains(query, "Custom instructions.") {
		t.Errorf("query should contain custom template, got %q", query)
	}
	if !strings.Contains(query, "pod is crashing") {
		t.Errorf("query should contain rendered request, got %q", query)
	}
}

func TestResolvePrompts_BothCustom(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "check certs"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "You are a cert auditor.",
					UserPrompt:   "Audit certs for: {{.Request}}",
				},
			},
		},
	}

	systemPrompt, query, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "You are a cert auditor." {
		t.Errorf("systemPrompt = %q", systemPrompt)
	}
	if query != "Audit certs for: check certs" {
		t.Errorf("query = %q", query)
	}
}

func TestResolvePrompts_SystemPromptReturnedWithBuiltinTemplate(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix it"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "custom system",
					// UserPrompt empty — uses built-in template
				},
			},
		},
	}

	systemPrompt, _, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "custom system" {
		t.Errorf("expected custom system prompt even with built-in user template, got %q", systemPrompt)
	}
}

func TestResolvePrompts_NoSystemPromptWhenNoneConfigured(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix it"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{},
	}

	systemPrompt, _, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if systemPrompt != "" {
		t.Errorf("expected empty system prompt when none configured, got %q", systemPrompt)
	}
}

func TestResolvePrompts_InvalidCustomTemplate(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix it"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					UserPrompt: "{{.InvalidField}}",
				},
			},
		},
	}

	_, _, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err == nil {
		t.Fatal("expected error for invalid custom template, got nil")
	}
	if !strings.Contains(err.Error(), "template exec") {
		t.Errorf("expected template exec error, got: %v", err)
	}
}

func TestResolvePrompts_MalformedCustomTemplate(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix it"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					UserPrompt: "{{.Unclosed",
				},
			},
		},
	}

	_, _, err := resolvePrompts(agent, "analysis", "ns", run, &agentContext{})
	if err == nil {
		t.Fatal("expected error for malformed template syntax, got nil")
	}
	if !strings.Contains(err.Error(), "template parse") {
		t.Errorf("expected template parse error, got: %v", err)
	}
}

func TestBuildInputConfigMap_SystemPromptConditional(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"},
		Spec:       agenticv1alpha1.AgenticRunSpec{Request: "fix"},
	}
	agent := &agenticv1alpha1.Agent{
		Spec: agenticv1alpha1.AgentSpec{
			Instructions: &agenticv1alpha1.AgentInstructions{
				Analysis: &agenticv1alpha1.StepInstructions{
					SystemPrompt: "custom",
					UserPrompt:   "Do: {{.Request}}",
				},
			},
		},
	}

	cm, err := buildInputConfigMap("ns", run, "analysis", agent, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("buildInputConfigMap: %v", err)
	}
	if cm.Data[inputConfigMapKeySystemPrompt] != "custom" {
		t.Errorf("system-prompt = %q, want 'custom'", cm.Data[inputConfigMapKeySystemPrompt])
	}

	// Without custom instructions — system-prompt key should be absent
	cm2, err := buildInputConfigMap("ns", run, "analysis", nil, json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("buildInputConfigMap: %v", err)
	}
	if _, ok := cm2.Data[inputConfigMapKeySystemPrompt]; ok {
		t.Errorf("system-prompt key should be absent when not configured, got %q", cm2.Data[inputConfigMapKeySystemPrompt])
	}
}

func TestBuildResultTemplate_UnknownStep(t *testing.T) {
	run := &agenticv1alpha1.AgenticRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", UID: "u"}}
	_, err := buildResultTemplate(run, "nope", run.Namespace)
	if err == nil {
		t.Fatal("expected error for unknown step")
	}
}
