/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const governedKagentV0Fixture = `apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata: {name: relay-model, namespace: kagent}
spec:
  provider: OpenAI
  model: probe
  apiKeyPassthrough: true
  openAI: {baseUrl: "http://probe-gov-relay.orka-system.svc:8092/v1"}
---
apiVersion: kagent.dev/v1alpha2
kind: RemoteMCPServer
metadata: {name: orka-relay, namespace: kagent}
spec:
  description: Orka tools through the relay
  protocol: STREAMABLE_HTTP
  url: http://probe-gov-relay.orka-system.svc:8092/mcp
  timeout: 30m
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: probe-gov, namespace: kagent, generation: 4}
spec:
  replicas: 1
  template:
    metadata:
      annotations: {kagent.dev/config-hash: "12345678901234567890"}
status: {observedGeneration: 4, replicas: 1, updatedReplicas: 1, availableReplicas: 1}
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata: {name: probe-gov, namespace: kagent, generation: 2}
spec:
  type: Declarative
  description: Calls Orka tools.
  declarative:
    runtime: go
    systemMessage: You call Orka tools.
    modelConfig: relay-model
    tools:
    - type: McpServer
      mcpServer: {kind: RemoteMCPServer, apiGroup: kagent.dev, name: orka-relay, toolNames: [probe-orka], allowedHeaders: [Authorization]}
status:
  observedGeneration: 2
  conditions: [{type: Accepted, status: "True"}, {type: Ready, status: "True"}]
`

const governedV0Relay = "http://probe-gov-relay.orka-system.svc:8092"

func classifyV0Fixture(t *testing.T, fixture, relay string) kagentMode {
	t.Helper()
	wrapped, _, err := wrapKagentAgents([]byte(fixture), relay, kagentRevisionEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 1 || wrapped[0].Version != kagentVersionV0 {
		t.Fatalf("wrapped = %+v, want one v0 Agent", wrapped)
	}
	return wrapped[0].Mode
}

func TestClassifyKagentV0AgentRequiresEveryEndpointOnTheRelay(t *testing.T) {
	mode := classifyV0Fixture(t, governedKagentV0Fixture, governedRelay)
	if mode.Mode != kagentModeGoverned || mode.RelayOrigin != governedV0Relay || mode.Revision != "12345678901234567890" {
		t.Fatalf("mode = %+v", mode)
	}
	if mode := classifyV0Fixture(t, governedKagentV0Fixture, ""); mode.Mode != kagentModeObserved ||
		!strings.Contains(strings.Join(mode.Reasons, ";"), "no relay was named") || mode.Revision == "" {
		t.Fatalf("an Agent was governed without a named relay, or lost its pin: %+v", mode)
	}
	for name, test := range map[string]struct{ old, new, reason string }{
		"no passthrough":       {"apiKeyPassthrough: true", "apiKeyPassthrough: false", "does not pass the caller's bearer through"},
		"model elsewhere":      {"8092/v1", "8092/other", "does not point at a relay's /v1"},
		"responses API":        {"8092/v1\"}", "8092/v1\", apiFormat: responses}", "uses the responses API"},
		"static model headers": {"apiKeyPassthrough: true", "apiKeyPassthrough: true\n  defaultHeaders: {X-Team: a}", `model config "relay-model" sends static headers`},
		"anthropic model":      {"provider: OpenAI", "provider: Anthropic", "does not use the OpenAI provider"},
		"missing model":        {"modelConfig: relay-model", "modelConfig: other-model", `model config "other-model" is not in the input`},
		"short timeout":        {"timeout: 30m", "timeout: 30s", "needs a timeout longer than 20m0s"},
		"relay timeout":        {"timeout: 30m", "timeout: 20m", "needs a timeout longer than 20m0s"},
		"sse":                  {"protocol: STREAMABLE_HTTP", "protocol: SSE", "does not use STREAMABLE_HTTP"},
		"server headers":       {"  timeout: 30m", "  timeout: 30m\n  headersFrom: [{name: X-Team, value: a}]", `tool server "orka-relay" sends static headers`},
		"no bearer forwarding": {"allowedHeaders: [Authorization]", "allowedHeaders: [x-user-email]", "does not forward Authorization"},
		"binding headers":      {"allowedHeaders: [Authorization]}", "allowedHeaders: [Authorization]}\n      headersFrom: [{name: Authorization, value: static}]", "binding sends static headers"},
		"kmcp server":          {"kind: RemoteMCPServer, apiGroup", "kind: MCPServer, apiGroup", "is not a kagent RemoteMCPServer"},
		"missing tool server":  {"name: orka-relay, toolNames", "name: other, toolNames", `tool server "other" is not in the input`},
		"two relays":           {"svc:8092/mcp", "svc:9000/mcp", "not the relay http://probe-gov-relay.orka-system.svc:8092"},
		"agent tool":           {"    - type: McpServer\n      mcpServer: {kind: RemoteMCPServer, apiGroup: kagent.dev, name: orka-relay, toolNames: [probe-orka], allowedHeaders: [Authorization]}", "    - type: Agent\n      agent: {name: helper}", `tool 0 is a "Agent" tool`},
		"skills":               {"  type: Declarative\n", "  type: Declarative\n  skills: {refs: [ghcr.io/x/skills:1]}\n", "loads skills"},
		"memory":               {"    runtime: go\n", "    runtime: go\n    memory: {modelConfig: embed}\n", "enables memory"},
		"code blocks":          {"    runtime: go\n", "    runtime: go\n    executeCodeBlocks: true\n", "executes code blocks"},
		"share tools":          {"    runtime: go\n", "    runtime: go\n    shareTools: true\n", "share-link tools"},
		"compaction":           {"    runtime: go\n", "    runtime: go\n    context: {compaction: {compactionInterval: 5}}\n", "compacts its context"},
		"prompt from secret":   {"systemMessage: You call Orka tools.", "systemMessageFrom: {type: Secret, name: p, key: k}", "reads its system message from an object the input does not show"},
		"prompt template":      {"    runtime: go\n", "    runtime: go\n    promptTemplate: {dataSources: [{name: p}]}\n", "templates its system message"},
		"deployment env":       {"    runtime: go\n", "    runtime: go\n    deployment: {env: [{name: STS_WELL_KNOWN_URI, value: http://sts}]}\n", "deployment sets env"},
		"image registry":       {"    runtime: go\n", "    runtime: go\n    deployment: {imageRegistry: example.com}\n", "deployment sets imageRegistry"},
		"unready Agent":        {`{type: Ready, status: "True"}`, `{type: Ready, status: "False"}`, "the Agent is not Ready"},
		"unobserved Agent":     {"  observedGeneration: 2\n", "  observedGeneration: 1\n", "has not observed the Agent's current generation"},
		"rolling out":          {"updatedReplicas: 1", "updatedReplicas: 0", "has not finished rolling out"},
		"old pods remain":      {"status: {observedGeneration: 4, replicas: 1", "status: {observedGeneration: 4, replicas: 2", "has not finished rolling out"},
		"unobserved rollout":   {"status: {observedGeneration: 4", "status: {observedGeneration: 3", "has not been observed at its current generation"},
		"no deployment":        {"metadata: {name: probe-gov, namespace: kagent, generation: 4}", "metadata: {name: other, namespace: kagent, generation: 4}", `deployment "probe-gov" is not in the input`},
		"no config hash":       {`annotations: {kagent.dev/config-hash: "12345678901234567890"}`, "annotations: {}", "has no kagent.dev/config-hash annotation"},
		"hash out of range":    {`"12345678901234567890"`, `"99999999999999999999"`, "has no kagent.dev/config-hash annotation"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
			if changed == governedKagentV0Fixture {
				t.Fatalf("fixture has no %q", test.old)
			}
			mode := classifyV0Fixture(t, changed, governedRelay)
			if mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, "; "), test.reason) {
				t.Fatalf("mode = %+v, want observed because %s", mode, test.reason)
			}
		})
	}
	for name, change := range map[string][2]string{
		"lowercase header":       {"allowedHeaders: [Authorization]", "allowedHeaders: [authorization]"},
		"explicit chat format":   {"8092/v1\"}", "8092/v1\", apiFormat: chatCompletions}"},
		"server in the core api": {"apiGroup: kagent.dev, ", ""},
		"placement settings":     {"    runtime: go\n", "    runtime: go\n    deployment: {replicas: 1, resources: {limits: {cpu: 1}}}\n"},
	} {
		changed := strings.Replace(governedKagentV0Fixture, change[0], change[1], 1)
		if changed == governedKagentV0Fixture {
			t.Fatalf("%s: fixture has no %q", name, change[0])
		}
		if mode := classifyV0Fixture(t, changed, governedRelay); mode.Mode != kagentModeGoverned {
			t.Errorf("%s: mode = %+v, want governed", name, mode)
		}
	}
}

func TestWrapKagentV0AgentsRefusesRuntimesTheWrapperCannotRun(t *testing.T) {
	for name, test := range map[string]struct{ old, new, want string }{
		"python runtime": {"runtime: go", "runtime: python", "the Python runtime cannot cancel a task and saves request headers"},
		"no runtime":     {"    runtime: go\n", "", "sets no declarative.runtime, so kagent runs it on the Python runtime"},
		"byo agent":      {"type: Declarative", "type: BYO", "it is a BYO Agent; only Declarative Agents"},
		"kagent approval": {
			"allowedHeaders: [Authorization]}", "allowedHeaders: [Authorization], requireApproval: [probe-orka]}",
			"tool 0 requires kagent approval (requireApproval), which a wrapped Agent cannot answer",
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
			if changed == governedKagentV0Fixture {
				t.Fatalf("fixture has no %q", test.old)
			}
			// The refusal applies to observed Agents too: no relay is named.
			if _, _, err := wrapKagentAgents([]byte(changed), "", kagentRevisionEvidence{}); err == nil ||
				!strings.Contains(err.Error(), "kagent Agent kagent/probe-gov cannot be wrapped") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWrapKagentAgentsKeepsEachVersionsObjectsApart(t *testing.T) {
	// A v1 ModelConfig of the same name does not stand in for a v0 Agent's.
	mixed := strings.Replace(governedKagentV0Fixture, "apiVersion: kagent.dev/v1alpha2\nkind: ModelConfig", "apiVersion: api.kagent.dev/v1alpha3\nkind: ModelConfig", 1) +
		"---\napiVersion: api.kagent.dev/v1alpha3\nkind: Agent\nmetadata: {name: v1-agent, namespace: kagent}\n"
	wrapped, _, err := wrapKagentAgents([]byte(mixed), governedRelay, kagentRevisionEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 2 || wrapped[0].Version != kagentVersionV0 || wrapped[1].Version != "" {
		t.Fatalf("wrapped = %+v, want a v0 and a v1 Agent", wrapped)
	}
	if reasons := strings.Join(wrapped[0].Mode.Reasons, ";"); wrapped[0].Mode.Mode != kagentModeObserved || !strings.Contains(reasons, `model config "relay-model" is not in the input`) {
		t.Fatalf("v0 mode = %+v", wrapped[0].Mode)
	}
	if got := decodeWrappedAgent(t, wrapped[0].Manifest).Annotations[kagentSourceAnnotation]; got != "kagent.dev/v1alpha2 kagent/probe-gov" {
		t.Errorf("v0 source annotation = %q", got)
	}
}

func TestAgentImportKagentCmdWritesV0WrapperConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(governedKagentV0Fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", path, "--dry-run", "--wrapper-config-dir", dir, "--control-plane-url", "http://kagent-controller.kagent.svc:8083",
		"--relay-url", governedRelay, "--model-upstream-url", "http://vekil.vekil-system.svc:8080/v1", "--model-upstream-model", "gpt-4.1"}
	if _, err := runKagentImport(t, args...); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if golden := `{
  "model": "kagent/kagent/probe-gov",
  "kagent": {
    "version": "v0",
    "controlPlaneURL": "http://kagent-controller.kagent.svc:8083",
    "agent": {
      "name": "probe-gov",
      "namespace": "kagent"
    },
    "expectedRevision": "12345678901234567890",
    "userIDPrefix": "orka"
  },
  "governed": {
    "upstreamBaseURL": "http://vekil.vekil-system.svc:8080/v1",
    "upstreamModel": "gpt-4.1"
  }
}
`; string(written) != golden {
		t.Fatalf("v0 wrapper configuration = %s", written)
	}
	rolling := strings.Replace(governedKagentV0Fixture, "updatedReplicas: 1", "updatedReplicas: 0", 1)
	if err := os.WriteFile(path, []byte(rolling), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "has no config hash to pin (deployment \"probe-gov\" has not finished rolling out") {
		t.Fatalf("unpinned v0 Agent error = %v", err)
	}
	// An observed v0 Agent is pinned the same way.
	observed := strings.Replace(governedKagentV0Fixture, "apiKeyPassthrough: true", "apiKeyPassthrough: false", 1)
	if err := os.WriteFile(path, []byte(observed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args[:len(args)-4]...); err != nil {
		t.Fatal(err)
	}
	var config kagentWrapperConfig
	raw, _ := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if err := json.Unmarshal(raw, &config); err != nil || config.Governed != nil || config.Kagent.Version != kagentVersionV0 || config.Kagent.ExpectedRevision != "12345678901234567890" {
		t.Fatalf("observed v0 wrapper configuration = %s (%v)", raw, err)
	}
}

const translatableKagentV0Fixture = `apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata: {name: gpt, namespace: kagent}
spec:
  provider: OpenAI
  model: gpt-4.1
  apiKeySecret: openai-key
  apiKeySecretKey: key
  openAI: {temperature: "0.2", maxTokens: 1024}
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata: {name: k8s-agent, namespace: kagent}
spec:
  type: Declarative
  description: Answers Kubernetes questions.
  declarative:
    runtime: python
    stream: true
    systemMessage: You are a Kubernetes expert.
    modelConfig: gpt
    deployment: {replicas: 2, resources: {limits: {cpu: "1"}}}
    tools:
    - type: McpServer
      mcpServer: {kind: RemoteMCPServer, apiGroup: kagent.dev, name: k8s, toolNames: [list_pods]}
`

func TestTranslateKagentV0AgentsMapsOnlyKnownFields(t *testing.T) {
	tools := map[string]string{"k8s/list_pods": "k8s-list-pods"}
	translated, _, err := translateKagentAgents([]byte(translatableKagentV0Fixture), kagentTranslateOptions{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	agent := decodeWrappedAgent(t, translated[0].Manifest)
	if agent.Spec.Runtime != nil || agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.Inline != "You are a Kubernetes expert." ||
		agent.Spec.Model == nil || agent.Spec.Model.Provider != "openai" || agent.Spec.Model.Name != "gpt-4.1" ||
		agent.Spec.Model.Temperature == nil || *agent.Spec.Model.Temperature != 0.2 || agent.Spec.Model.MaxTokens == nil || *agent.Spec.Model.MaxTokens != 1024 ||
		len(agent.Spec.Tools) != 1 || agent.Spec.Tools[0].Name != "k8s-list-pods" || agent.Annotations[kagentSourceAnnotation] != "kagent.dev/v1alpha2 kagent/k8s-agent" {
		t.Fatalf("translated Agent = %+v", agent)
	}
	fromConfigMap := strings.Replace(translatableKagentV0Fixture, "systemMessage: You are a Kubernetes expert.", "systemMessageFrom: {type: ConfigMap, name: prompts, key: k8s}", 1)
	translated, _, err = translateKagentAgents([]byte(fromConfigMap), kagentTranslateOptions{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if prompt := decodeWrappedAgent(t, translated[0].Manifest).Spec.SystemPrompt; prompt == nil || prompt.ConfigMapRef == nil ||
		prompt.ConfigMapRef.Name != "prompts" || prompt.ConfigMapRef.Key != "k8s" {
		t.Fatalf("system prompt = %+v", prompt)
	}
}

func TestTranslateKagentV0AgentsRefusesEverythingItCannotMap(t *testing.T) {
	input := strings.NewReplacer(
		"    stream: true\n", "    stream: true\n    memory: {modelConfig: embed}\n    promptTemplate: {dataSources: [{name: p}]}\n",
		"systemMessage: You are a Kubernetes expert.", "systemMessageFrom: {type: Secret, name: prompts, key: k8s}",
		"resources: {limits: {cpu: \"1\"}}}", "env: [{name: KAGENT_PROPAGATE_TOKEN, value: \"true\"}]}",
		"toolNames: [list_pods]}", "toolNames: [list_pods], requireApproval: [list_pods], allowedHeaders: [Authorization]}\n      headersFrom: [{name: X, value: y}]\n    - type: Agent\n      agent: {name: helper}",
		"  apiKeySecretKey: key\n", "  apiKeySecretKey: key\n  apiKeyPassthrough: false\n",
		"  description: Answers Kubernetes questions.\n", "  description: Answers Kubernetes questions.\n  skills: {refs: [s]}\n",
	).Replace(translatableKagentV0Fixture)
	_, _, err := translateKagentAgents([]byte(input), kagentTranslateOptions{})
	if err == nil {
		t.Fatal("an untranslatable v0 Agent was accepted")
	}
	for _, want := range []string{
		"declarative.memory has no Orka equivalent", "declarative.promptTemplate has no Orka equivalent",
		"reads its system message from a Secret", "declarative.deployment.env changes the agent's runtime",
		"sets mcpServer.requireApproval", "sets mcpServer.allowedHeaders", "tool 0 sets headersFrom", `tool 1 is a "Agent" tool`,
		"sets apiKeyPassthrough, which has no Orka equivalent", "spec.skills has no Orka equivalent",
		"tool k8s/list_pods has no Orka Tool", "Translation maps only these fields",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if _, _, err := translateKagentAgents([]byte(strings.Replace(translatableKagentV0Fixture, "type: Declarative", "type: BYO", 1)), kagentTranslateOptions{}); err == nil ||
		!strings.Contains(err.Error(), "it is a \"BYO\" Agent; only Declarative Agents translate") {
		t.Fatalf("BYO Agent error = %v", err)
	}
}
