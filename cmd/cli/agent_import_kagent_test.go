/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const kagentImportFixture = `apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  name: k8s-agent
  namespace: kagent
spec:
  template:
    ref:
      name: shared-context
  harnessRef:
    name: kagent
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata:
  name: default-model-config
  namespace: kagent
spec:
  provider: OpenAI
  model: gpt-4.1
---
apiVersion: v1
kind: List
items:
- apiVersion: kagent.dev/v1alpha2
  kind: Agent
  metadata:
    name: helm-agent
    namespace: kagent
  spec:
    type: Declarative
    declarative:
      modelConfig: default-model-config
      systemMessage: You manage Helm releases.
- apiVersion: kagent.dev/v1alpha2
  kind: RemoteMCPServer
  metadata:
    name: kagent-tools
    namespace: kagent
`

func decodeWrappedAgent(t *testing.T, manifest map[string]any) corev1alpha1.Agent {
	t.Helper()
	out, err := sigsyaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var agent corev1alpha1.Agent
	if err := sigsyaml.UnmarshalStrict(out, &agent); err != nil {
		t.Fatalf("manifest is not a strict Orka Agent: %v\n%s", err, out)
	}
	return agent
}

func TestWrapKagentAgentsConvertsSupportedVersions(t *testing.T) {
	wrapped, skipped, err := wrapKagentAgents([]byte(kagentImportFixture))
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	if want := "api.kagent.dev/v1alpha3 ModelConfig (1), kagent.dev/v1alpha2 RemoteMCPServer (1)"; skipped != want {
		t.Errorf("skipped = %q, want %q", skipped, want)
	}
	want := []struct{ name, runtime, source string }{
		{"k8s-agent", "k8s-agent-kagent-runtime", "api.kagent.dev/v1alpha3 kagent/k8s-agent"},
		{"helm-agent", "helm-agent-kagent-runtime", "kagent.dev/v1alpha2 kagent/helm-agent"},
	}
	if len(wrapped) != len(want) {
		t.Fatalf("wrapped %d agents, want %d", len(wrapped), len(want))
	}
	for i, w := range want {
		agent := decodeWrappedAgent(t, wrapped[i].Manifest)
		if agent.APIVersion != "core.orka.ai/v1alpha1" || agent.Kind != "Agent" {
			t.Errorf("agent %d type = %s %s", i, agent.APIVersion, agent.Kind)
		}
		if agent.Name != w.name || agent.Namespace != "" {
			t.Errorf("agent %d = %s/%s, want name %s and no namespace", i, agent.Namespace, agent.Name, w.name)
		}
		if agent.Annotations[kagentSourceAnnotation] != w.source {
			t.Errorf("agent %d source annotation = %q, want %q", i, agent.Annotations[kagentSourceAnnotation], w.source)
		}
		runtime := agent.Spec.Runtime
		if runtime == nil || runtime.RuntimeRef == nil || runtime.RuntimeRef.Name != w.runtime || runtime.Type != "" {
			t.Errorf("agent %d runtime = %+v, want only runtimeRef %s", i, runtime, w.runtime)
		}
		if wrapped[i].RuntimeName != w.runtime {
			t.Errorf("agent %d RuntimeName = %q, want %q", i, wrapped[i].RuntimeName, w.runtime)
		}
		if agent.Spec.SystemPrompt != nil || agent.Spec.Model != nil || agent.Spec.Tools != nil {
			t.Errorf("agent %d carries behavior that kagent owns: %+v", i, agent.Spec)
		}
	}
}

func TestWrapKagentAgentsRejectsUnsafeInput(t *testing.T) {
	agentDoc := func(apiVersion, namespace, name string) string {
		return "apiVersion: " + apiVersion + "\nkind: Agent\nmetadata:\n  namespace: " + namespace + "\n  name: " + name + "\n"
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "unsupported kagent version",
			input: agentDoc("kagent.dev/v1alpha1", "kagent", "old-agent"),
			want:  `unsupported kagent Agent version "kagent.dev/v1alpha1"`,
		},
		{
			name:  "same name from two namespaces",
			input: agentDoc("api.kagent.dev/v1alpha3", "team-a", "triage") + "---\n" + agentDoc("kagent.dev/v1alpha2", "team-b", "triage"),
			want:  `kagent Agents team-a/triage and team-b/triage would both become Orka Agent "triage"`,
		},
		{
			name:  "missing name",
			input: "apiVersion: api.kagent.dev/v1alpha3\nkind: Agent\nmetadata:\n  namespace: kagent\n",
			want:  "missing metadata.name",
		},
		{
			name:  "runtime name too long",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", strings.Repeat("a", 250)),
			want:  "is not a valid Orka resource name",
		},
		{
			name:  "no kagent agents",
			input: "apiVersion: api.kagent.dev/v1alpha3\nkind: ModelConfig\nmetadata:\n  name: m\n",
			want:  "no kagent Agents found",
		},
		{
			name:  "orka agent is not imported",
			input: agentDoc("core.orka.ai/v1alpha1", "default", "native"),
			want:  "no kagent Agents found",
		},
		{
			name:  "malformed yaml",
			input: "apiVersion: [unterminated\n",
			want:  "parsing manifest document 1",
		},
		{
			name:  "duplicate yaml key",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "first") + "  name: second\n",
			want:  "parsing manifest document 1",
		},
		{
			name:  "duplicate json key",
			input: `{"apiVersion":"kagent.dev/v1alpha1","apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}`,
			want:  `parsing manifest document 1: duplicate key "apiVersion"`,
		},
		{
			name: "duplicate key in a later json value",
			input: `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}` + "\n" +
				`{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"b","name":"c"}}`,
			want: `parsing manifest document 2: duplicate key "name"`,
		},
		{
			name:  "trailing garbage after json",
			input: `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}} trailing`,
			want:  "parsing manifest document 1",
		},
		{
			name: "comment before concatenated json",
			input: "# export\n" + `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}` + "\n" +
				`{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"b"}}`,
			want: "parsing manifest document 1",
		},
		{
			name:  "document end marker hides a second document",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "a") + "...\n" + agentDoc("api.kagent.dev/v1alpha3", "kagent", "b"),
			want:  "parsing manifest document 1",
		},
		{
			name:  "inline document after separator",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "a") + "--- {apiVersion: api.kagent.dev/v1alpha3, kind: Agent, metadata: {name: b}}\n",
			want:  "reading manifest document 1",
		},
		{
			name:  "non-object list item",
			input: "apiVersion: v1\nkind: List\nitems:\n- " + strings.ReplaceAll(strings.TrimSuffix(agentDoc("api.kagent.dev/v1alpha3", "kagent", "ok"), "\n"), "\n", "\n  ") + "\n- not-an-object\n",
			want:  "List item 1 is not an object",
		},
		{
			name:  "scalar document",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "ok") + "---\njust-a-string\n",
			want:  "parsing manifest document 2: not an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := wrapKagentAgents([]byte(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRequireSingleYAMLDocument(t *testing.T) {
	if err := requireSingleYAMLDocument([]byte("# only a comment\n")); err != nil {
		t.Errorf("comment-only document: %v", err)
	}
	if err := requireSingleYAMLDocument([]byte("a: 1\n")); err != nil {
		t.Errorf("single document: %v", err)
	}
	if err := requireSingleYAMLDocument([]byte("a: 1\n---\nb: 2\n")); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("two documents: error = %v, want a multiple-document error", err)
	}
}

func TestWrapKagentAgentsReadsEveryJSONValue(t *testing.T) {
	input := `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"namespace":"kagent","name":"a"}}
{"apiVersion":"kagent.dev/v1alpha2","kind":"Agent","metadata":{"namespace":"kagent","name":"b"}}
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  namespace: kagent
  name: c
---
{apiVersion: api.kagent.dev/v1alpha3, kind: Agent, metadata: {namespace: kagent, name: d}}
`
	wrapped, _, err := wrapKagentAgents([]byte(input))
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	names := make([]string, 0, len(wrapped))
	for _, agent := range wrapped {
		names = append(names, metadataName(agent.Manifest))
	}
	if got := strings.Join(names, ","); got != "a,b,c,d" {
		t.Fatalf("wrapped Agents = %s, want a,b,c,d", got)
	}
}

func TestWrapKagentAgentsAcceptsListsAndJSONEscapes(t *testing.T) {
	input := `apiVersion: kagent.dev/v1alpha2
kind: AgentList
items:
- metadata:
    namespace: kagent
    name: typed
---
{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"namespace":"kagent","name":"emoji","annotations":{"icon":"😀"}}}
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: List
  items:
  - apiVersion: api.kagent.dev/v1alpha3
    kind: Agent
    metadata:
      namespace: kagent
      name: nested
`
	wrapped, skipped, err := wrapKagentAgents([]byte(input))
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want nothing skipped", skipped)
	}
	if len(wrapped) != 3 || wrapped[0].Source != "kagent/typed" || wrapped[1].Source != "kagent/emoji" || wrapped[2].Source != "kagent/nested" {
		t.Fatalf("wrapped = %+v, want kagent/typed, kagent/emoji and kagent/nested", wrapped)
	}
	if got := decodeWrappedAgent(t, wrapped[0].Manifest).Annotations[kagentSourceAnnotation]; got != "kagent.dev/v1alpha2 kagent/typed" {
		t.Errorf("typed list item source = %q, want the list's apiVersion", got)
	}
}

func writeKagentFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(kagentImportFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestAgentImportKagentCmdDryRunPrintsManifests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeKagentFixture(t)

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"agent", "import", "kagent", "-f", path, "--dry-run", "-n", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	objects, err := decodeKubernetesObjects(stdout.Bytes())
	if err != nil {
		t.Fatalf("dry-run output is not a manifest stream: %v\n%s", err, stdout.String())
	}
	if len(objects) != 2 {
		t.Fatalf("printed %d manifests, want 2:\n%s", len(objects), stdout.String())
	}
	for _, obj := range objects {
		agent := decodeWrappedAgent(t, obj)
		if agent.Namespace != "team" {
			t.Errorf("Agent %s namespace = %q, want team", agent.Name, agent.Namespace)
		}
	}
	if !strings.Contains(stderr.String(), "Skipped objects that are not kagent Agents") {
		t.Errorf("stderr = %q, want a skipped-objects note", stderr.String())
	}
}

func kagentImportAPIServer(t *testing.T, failOn string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != agentsAPIPath {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if metadataName(body) == failOn {
			http.Error(w, `{"error":"agent already exists"}`, http.StatusConflict)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestAgentImportKagentCmdCreatesAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, bodies := kagentImportAPIServer(t, "")

	var stdout bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"agent", "import", "kagent", "-f", writeKagentFixture(t), "--server", srv.URL, "-n", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if len(*bodies) != 2 {
		t.Fatalf("created %d Agents, want 2", len(*bodies))
	}
	for _, body := range *bodies {
		agent := decodeWrappedAgent(t, body)
		if agent.Namespace != "team" {
			t.Errorf("Agent %s namespace = %q, want team", agent.Name, agent.Namespace)
		}
	}
	if !strings.Contains(stdout.String(), "Agent created: k8s-agent (wraps kagent Agent kagent/k8s-agent; needs AgentRuntime k8s-agent-kagent-runtime)") {
		t.Errorf("stdout = %q, want the created Agent and its required AgentRuntime", stdout.String())
	}
}

func TestAgentImportKagentCmdReportsPartialFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, bodies := kagentImportAPIServer(t, "helm-agent")

	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"agent", "import", "kagent", "-f", writeKagentFixture(t), "--server", srv.URL, "-n", "team"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), `creating Agent "helm-agent"`) || !strings.Contains(err.Error(), "already created: k8s-agent") {
		t.Fatalf("error = %v, want the failed Agent and the ones already created", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("created %d Agents before the failure, want 1", len(*bodies))
	}
}
