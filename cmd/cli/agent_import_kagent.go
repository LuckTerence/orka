/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// kagentSourceAnnotation records the kagent Agent an imported Orka Agent wraps.
	kagentSourceAnnotation = "orka.ai/kagent-source"
	// kagentRuntimeSuffix names the AgentRuntime that must serve one wrapped kagent Agent.
	// An external harness-v2 profile pins exactly one remote agent, so each needs its own.
	kagentRuntimeSuffix = "-kagent-runtime"
)

// kagentAgentAPIVersions are the kagent Agent versions that expose A2A and can be wrapped.
var kagentAgentAPIVersions = map[string]bool{
	"api.kagent.dev/v1alpha3": true,
	"kagent.dev/v1alpha2":     true,
}

// kagentWrappedAgent is one Orka Agent generated from a kagent Agent.
type kagentWrappedAgent struct {
	Source      string
	RuntimeName string
	Manifest    map[string]any
}

func newAgentImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import agents defined on another platform",
	}
	cmd.AddCommand(newAgentImportKagentCmd())
	return cmd
}

func newAgentImportKagentCmd() *cobra.Command {
	var file string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "kagent -f <file>",
		Short: "Wrap kagent Agents as Orka Agents",
		Long: `Create one Orka Agent per kagent Agent. Each Orka Agent references the
AgentRuntime "<name>` + kagentRuntimeSuffix + `", which must be registered separately and
serve exactly that kagent Agent. kagent keeps executing the agent and its tools
with its own credentials; Orka governs who can dispatch work to it.

Accepts api.kagent.dev/v1alpha3 and kagent.dev/v1alpha2 Agents, including lists
from "kubectl get agents -o yaml". Other objects are skipped.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			data, err := readFileOrStdin(file)
			if err != nil {
				return fmt.Errorf("reading file: %w", err)
			}
			wrapped, skipped, err := wrapKagentAgents(data)
			if err != nil {
				return err
			}
			if skipped != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Skipped objects that are not kagent Agents: %s\n", skipped) //nolint:errcheck
			}
			if dryRun {
				namespace, _ := cmd.Flags().GetString(cliNamespaceQuery)
				return printKagentWrappedAgents(cmd.OutOrStdout(), wrapped, strings.TrimSpace(namespace))
			}
			return createKagentWrappedAgents(cmd, wrapped)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to kagent Agent YAML/JSON (use - for stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the Orka Agent manifests instead of creating them")
	return cmd
}

// wrapKagentAgents converts every kagent Agent in a YAML/JSON stream into an
// Orka Agent whose runtime is a per-agent AgentRuntime. It returns the other
// object kinds it skipped as a summary.
func wrapKagentAgents(data []byte) ([]kagentWrappedAgent, string, error) {
	objects, err := decodeKubernetesObjects(data)
	if err != nil {
		return nil, "", err
	}
	var wrapped []kagentWrappedAgent
	skipped := map[string]int{}
	byName := map[string]string{}
	for _, obj := range objects {
		apiVersion, _ := obj["apiVersion"].(string)
		kind, _ := obj["kind"].(string)
		if kind != "Agent" || !isKagentAPIVersion(apiVersion) {
			skipped[strings.TrimSpace(apiVersion+" "+kind)]++
			continue
		}
		if !kagentAgentAPIVersions[apiVersion] {
			return nil, "", fmt.Errorf("unsupported kagent Agent version %q: supported versions are api.kagent.dev/v1alpha3 and kagent.dev/v1alpha2", apiVersion)
		}
		agent, err := wrapKagentAgent(apiVersion, obj)
		if err != nil {
			return nil, "", err
		}
		name := metadataName(agent.Manifest)
		if previous, ok := byName[name]; ok {
			return nil, "", fmt.Errorf("kagent Agents %s and %s would both become Orka Agent %q; import them into different namespaces", previous, agent.Source, name)
		}
		byName[name] = agent.Source
		wrapped = append(wrapped, agent)
	}
	if len(wrapped) == 0 {
		return nil, "", fmt.Errorf("no kagent Agents found (expected api.kagent.dev/v1alpha3 or kagent.dev/v1alpha2 kind Agent)")
	}
	return wrapped, summarizeSkipped(skipped), nil
}

func wrapKagentAgent(apiVersion string, obj map[string]any) (kagentWrappedAgent, error) {
	metadata, _ := obj["metadata"].(map[string]any)
	name, _ := metadata[cliNameKey].(string)
	namespace, _ := metadata[cliNamespaceQuery].(string)
	name, namespace = strings.TrimSpace(name), strings.TrimSpace(namespace)
	if name == "" {
		return kagentWrappedAgent{}, fmt.Errorf("%s Agent is missing metadata.name", apiVersion)
	}
	source := name
	if namespace != "" {
		source = namespace + "/" + name
	}
	runtimeName := name + kagentRuntimeSuffix
	for _, candidate := range []string{name, runtimeName} {
		if problems := utilvalidation.IsDNS1123Subdomain(candidate); len(problems) > 0 {
			return kagentWrappedAgent{}, fmt.Errorf("kagent Agent %s: %q is not a valid Orka resource name: %s", source, candidate, strings.Join(problems, "; "))
		}
	}
	return kagentWrappedAgent{
		Source:      source,
		RuntimeName: runtimeName,
		Manifest: map[string]any{
			"apiVersion": corev1alpha1.GroupVersion.String(),
			"kind":       "Agent",
			"metadata": map[string]any{
				cliNameKey: name,
				"annotations": map[string]any{
					kagentSourceAnnotation: apiVersion + " " + source,
				},
			},
			"spec": map[string]any{
				"runtime": map[string]any{
					"runtimeRef": map[string]any{cliNameKey: runtimeName},
				},
			},
		},
	}, nil
}

func isKagentAPIVersion(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	return group == "kagent.dev" || group == "api.kagent.dev"
}

// manifestDocument is one YAML document or one value of a JSON stream.
type manifestDocument struct {
	raw    []byte
	isJSON bool
}

// decodeKubernetesObjects reads a multi-document YAML or JSON stream and
// flattens List objects into their items. Duplicate keys and non-object
// documents or List items are rejected so no source object is silently lost.
func decodeKubernetesObjects(data []byte) ([]map[string]any, error) {
	docs, err := splitManifestDocuments(data)
	if err != nil {
		return nil, err
	}
	var objects []map[string]any
	for i, doc := range docs {
		obj, err := parseManifestDocument(doc)
		if err != nil {
			return nil, fmt.Errorf("parsing manifest document %d: %w", i+1, err)
		}
		if len(obj) == 0 {
			continue
		}
		if objects, err = appendManifestObject(objects, obj); err != nil {
			return nil, fmt.Errorf("parsing manifest document %d: %w", i+1, err)
		}
	}
	return objects, nil
}

// appendManifestObject appends obj, recursively expanding Lists into their items.
func appendManifestObject(objects []map[string]any, obj map[string]any) ([]map[string]any, error) {
	kind, _ := obj["kind"].(string)
	items, isList := obj["items"].([]any)
	if !isList || !strings.HasSuffix(kind, "List") {
		return append(objects, obj), nil
	}
	listAPIVersion, _ := obj["apiVersion"].(string)
	for i, item := range items {
		itemObj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s item %d is not an object", kind, i)
		}
		// Typed lists such as AgentList may omit item type metadata; Kubernetes
		// infers it from the list. A generic List carries it on every item.
		if kind != "List" {
			if _, ok := itemObj["apiVersion"]; !ok {
				itemObj["apiVersion"] = listAPIVersion
			}
			if _, ok := itemObj["kind"]; !ok {
				itemObj["kind"] = strings.TrimSuffix(kind, "List")
			}
		}
		var err error
		if objects, err = appendManifestObject(objects, itemObj); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// parseManifestDocument decodes one document strictly, rejecting duplicate
// keys. JSON stays on the JSON decoder, which accepts every valid JSON escape
// (the YAML scanner rejects UTF-16 surrogate pairs).
func parseManifestDocument(doc manifestDocument) (map[string]any, error) {
	jsonDoc := doc.raw
	if doc.isJSON {
		if err := rejectDuplicateJSONKeys(json.NewDecoder(bytes.NewReader(doc.raw)), 0); err != nil {
			return nil, err
		}
	} else {
		converted, err := sigsyaml.YAMLToJSONStrict(doc.raw)
		if err != nil {
			return nil, err
		}
		jsonDoc = converted
	}
	var obj map[string]any
	if err := json.Unmarshal(jsonDoc, &obj); err != nil {
		return nil, errors.New("not an object")
	}
	return obj, nil
}

// maxManifestJSONDepth matches encoding/json's own nesting limit.
const maxManifestJSONDepth = 10000

// rejectDuplicateJSONKeys walks one JSON value and fails on any object that
// repeats a key, which encoding/json would otherwise resolve silently.
func rejectDuplicateJSONKeys(decoder *json.Decoder, depth int) error {
	if depth > maxManifestJSONDepth {
		return errors.New("JSON nesting exceeds the maximum depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, _ := keyToken.(string)
			if keys[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			keys[key] = true
		}
		if err := rejectDuplicateJSONKeys(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// splitManifestDocuments splits YAML at "---" separators and a JSON stream into
// its individual values, so each document is parsed and validated on its own.
func splitManifestDocuments(data []byte) ([]manifestDocument, error) {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs []manifestDocument
	for {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading manifest document %d: %w", len(docs)+1, err)
		}
		if values, ok := splitJSONValues(raw); ok {
			for _, value := range values {
				docs = append(docs, manifestDocument{raw: value, isJSON: true})
			}
			continue
		}
		// The strict YAML converter reads only the first document, so anything
		// after it (a "..." marker or concatenated JSON) must fail here instead.
		if err := requireSingleYAMLDocument(raw); err != nil {
			return nil, fmt.Errorf("parsing manifest document %d: %w", len(docs)+1, err)
		}
		docs = append(docs, manifestDocument{raw: raw})
	}
}

// splitJSONValues returns each value of a chunk that is entirely a JSON stream.
// It reports false for anything else, including flow-style YAML.
func splitJSONValues(raw []byte) ([][]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var values [][]byte
	for {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				return values, true
			}
			return nil, false
		}
		values = append(values, value)
	}
}

func requireSingleYAMLDocument(raw []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	for count := 0; ; count++ {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if count > 0 {
			return fmt.Errorf("found more than one YAML document; separate documents with ---")
		}
	}
}

func summarizeSkipped(skipped map[string]int) string {
	kinds := make([]string, 0, len(skipped))
	for kind, count := range skipped {
		if kind == "" {
			kind = "<no kind>"
		}
		kinds = append(kinds, fmt.Sprintf("%s (%d)", kind, count))
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}

func printKagentWrappedAgents(w io.Writer, wrapped []kagentWrappedAgent, namespace string) error {
	for i, agent := range wrapped {
		if namespace != "" {
			ensureManifestNamespace(agent.Manifest, namespace)
		}
		out, err := sigsyaml.Marshal(agent.Manifest)
		if err != nil {
			return fmt.Errorf("marshaling Agent %q: %w", metadataName(agent.Manifest), err)
		}
		if i > 0 {
			fmt.Fprintln(w, "---") //nolint:errcheck
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
	}
	return nil
}

func createKagentWrappedAgents(cmd *cobra.Command, wrapped []kagentWrappedAgent) error {
	c := newClientFromCmd(cmd)
	out := cmd.OutOrStdout()
	var created []string
	for _, agent := range wrapped {
		ensureManifestNamespace(agent.Manifest, c.Namespace)
		name := metadataName(agent.Manifest)
		body, err := json.Marshal(agent.Manifest)
		if err != nil {
			return fmt.Errorf("marshaling Agent %q: %w", name, err)
		}
		if _, err := c.DoJSON(context.Background(), http.MethodPost, "/api/v1/agents", nil, body); err != nil {
			if len(created) > 0 {
				return fmt.Errorf("creating Agent %q for kagent Agent %s: %w (already created: %s)", name, agent.Source, err, strings.Join(created, ", "))
			}
			return fmt.Errorf("creating Agent %q for kagent Agent %s: %w", name, agent.Source, err)
		}
		created = append(created, name)
		fmt.Fprintf(out, "Agent created: %s (wraps kagent Agent %s; needs AgentRuntime %s)\n", name, agent.Source, agent.RuntimeName) //nolint:errcheck
	}
	return nil
}
