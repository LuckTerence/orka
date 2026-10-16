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
	"maps"
	"net/http"
	"net/url"
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

// kagentV1APIVersion is kagent v1, the Substrate-based design the wrapper
// targets. Earlier kagent versions expose a different control plane.
const kagentV1APIVersion = "api.kagent.dev/v1alpha3"

// kagentWrappedAgent is one Orka Agent generated from a kagent Agent.
type kagentWrappedAgent struct {
	// Source is the kagent Agent's [namespace/]name.
	Source string
	// SourceNamespace is empty when the input sets no metadata.namespace.
	SourceNamespace string
	SourceName      string
	RuntimeName     string
	Manifest        map[string]any
	Mode            kagentMode
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
	var wrapper kagentWrapperOptions
	var relayURL, egressFile, sourcesFile string
	var acceptedEgress, toolMappings []string
	var translate bool
	var translateOptions kagentTranslateOptions
	cmd := &cobra.Command{
		Use:   "kagent -f <file>",
		Short: "Wrap kagent Agents as Orka Agents",
		Long: `Create or update one Orka Agent per kagent Agent. Each Orka Agent references
the AgentRuntime "<name>` + kagentRuntimeSuffix + `", which must be registered separately
and serve exactly that kagent Agent. Re-running the import is safe: Agents that
already wrap the same kagent Agent are left alone or updated, and any other
existing Agent is an error.

Each Agent is reported as governed or observed. A governed Agent runs on the
ADK harness with every model and tool endpoint pointing at the relay named by
--relay-url, so Orka executes its tools and serves its model. Its latest
successful kagent revision must be the one its current spec compiles to, and
must have been compiled from the Harness, AgentTemplate, ModelConfig, and
RemoteMCPServer objects in the input (--revision-sources). That revision's
compiled egress (--revision-egress) may reach only the relay and the origins
named by --accepted-egress. Any other Agent is observed: kagent runs its
tools, and Orka governs dispatch and lifecycle. Dispatch control requires the
kagent install to admit only kagent's controller to Substrate's actor router
(see the orka-runtime-kagent README).
Include the Agents' Harness, AgentTemplate, ModelConfig, and RemoteMCPServer
objects in the input so the import can check them.

Accepts kagent v1 (api.kagent.dev/v1alpha3) Agents, including lists from
"kubectl get agents -o yaml". Other objects are skipped.

With --wrapper-config-dir, also writes each Agent's wrapper configuration,
<name>.kagent.json, pinned to the Agent's latest successful revision.

With --translate, converts each kagent Agent into a native Orka Agent that Orka
runs itself, with no kagent at run time. Translation maps only these fields
and refuses everything else, listing every problem:

` + kagentTranslationTable,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			data, err := readFileOrStdin(file)
			if err != nil {
				return fmt.Errorf("reading file: %w", err)
			}
			var wrapped []kagentWrappedAgent
			var skipped string
			if translate {
				if wrapper.Dir != "" || relayURL != "" {
					return fmt.Errorf("--translate creates native Orka Agents; it takes no wrapper or relay flags")
				}
				if translateOptions.Tools, err = parseKagentToolMappings(toolMappings); err != nil {
					return err
				}
				wrapped, skipped, err = translateKagentAgents(data, translateOptions)
			} else {
				var evidence kagentRevisionEvidence
				if evidence, err = readKagentRevisionEvidence(egressFile, sourcesFile, acceptedEgress); err != nil {
					return err
				}
				wrapped, skipped, err = wrapKagentAgents(data, relayURL, evidence)
			}
			if err != nil {
				return err
			}
			if err := wrapper.validate(wrapped); err != nil {
				return err
			}
			if skipped != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Skipped objects that are not kagent Agents: %s\n", skipped) //nolint:errcheck
			}
			written, err := writeKagentWrapperConfigs(wrapper, wrapped)
			if err != nil {
				return fmt.Errorf("writing wrapper configuration: %w", err)
			}
			for _, path := range written {
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrapper configuration written: %s\n", path) //nolint:errcheck
			}
			if dryRun {
				for _, agent := range wrapped {
					fmt.Fprintf(cmd.ErrOrStderr(), "kagent Agent %s: %s\n", agent.Source, describeKagentMode(agent.Mode)) //nolint:errcheck
				}
				namespace, _ := cmd.Flags().GetString(cliNamespaceQuery)
				return printKagentWrappedAgents(cmd.OutOrStdout(), wrapped, strings.TrimSpace(namespace))
			}
			return reconcileKagentWrappedAgents(cmd, wrapped)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to kagent Agent YAML/JSON (use - for stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the Orka Agent manifests instead of creating them")
	cmd.Flags().StringVar(&relayURL, "relay-url", "", "Origin of each Agent's wrapper relay, such as http://{name}-kagent-relay.orka-system.svc:8092; required for governed Agents")
	cmd.Flags().StringVar(&egressFile, "revision-egress", "", `JSON object mapping each kagent revision to its compiled egress destinations, e.g. {"<revision>": ["http://relay:8092"]}`)
	cmd.Flags().StringVar(&sourcesFile, "revision-sources", "", "JSON object mapping each kagent revision to its source snapshot (kagent.agent_runtime_revision.source_snapshot)")
	cmd.Flags().StringArrayVar(&acceptedEgress, "accepted-egress", nil, "Origin a governed revision may reach besides its relay, such as the runtime control-plane filter (repeatable)")
	cmd.Flags().BoolVar(&translate, "translate", false, "Convert the kagent Agents into native Orka Agents instead of wrapping them")
	cmd.Flags().StringArrayVar(&toolMappings, "tool", nil, "With --translate, map a kagent MCP tool to an existing Orka Tool: <RemoteMCPServer>/<tool>=<Orka Tool> (repeatable)")
	cmd.Flags().StringVar(&translateOptions.ProviderRef, "provider-ref", "", "With --translate, the Orka Provider serving models that use a custom endpoint")
	cmd.Flags().StringVar(&wrapper.Dir, "wrapper-config-dir", "", "Write each Agent's wrapper configuration (<name>.kagent.json) to this directory")
	cmd.Flags().StringVar(&wrapper.ControlPlaneURL, "control-plane-url", "", "kagent controller base URL the wrapper calls (with --wrapper-config-dir)")
	cmd.Flags().StringVar(&wrapper.UserIDPrefix, "user-id-prefix", "orka", "Prefix of the per-session kagent user identity (with --wrapper-config-dir)")
	cmd.Flags().StringVar(&wrapper.UpstreamURL, "model-upstream-url", "", "OpenAI-compatible base URL the relay serves governed Agents from")
	cmd.Flags().StringVar(&wrapper.UpstreamModel, "model-upstream-model", "", "Model the relay calls for governed Agents")
	return cmd
}

// wrapKagentAgents converts every kagent Agent in a YAML/JSON stream into an
// Orka Agent whose runtime is a per-agent AgentRuntime. It returns the other
// object kinds it skipped as a summary.
// kagentSourceAgent is one kagent Agent read from the input.
type kagentSourceAgent struct {
	APIVersion string
	Object     map[string]any
	Wrapped    kagentWrappedAgent
}

// collectKagentAgents decodes the input, keeps kagent v1 Agents, and indexes
// the related kagent objects. It returns a summary of the kinds it skipped.
func collectKagentAgents(data []byte) ([]kagentSourceAgent, kagentObjects, string, error) {
	objects, err := decodeKubernetesObjects(data)
	if err != nil {
		return nil, nil, "", err
	}
	var agents []kagentSourceAgent
	skipped := map[string]int{}
	byName := map[string]string{}
	for _, obj := range objects {
		apiVersion, _ := obj["apiVersion"].(string)
		kind, _ := obj["kind"].(string)
		if kind != "Agent" || !isKagentAPIVersion(apiVersion) {
			skipped[strings.TrimSpace(apiVersion+" "+kind)]++
			continue
		}
		if apiVersion != kagentV1APIVersion {
			return nil, nil, "", fmt.Errorf("unsupported kagent Agent version %q: only kagent v1 (%s) Agents can be wrapped", apiVersion, kagentV1APIVersion)
		}
		agent, err := wrapKagentAgent(apiVersion, obj)
		if err != nil {
			return nil, nil, "", err
		}
		name := metadataName(agent.Manifest)
		if previous, ok := byName[name]; ok {
			return nil, nil, "", fmt.Errorf("kagent Agents %s and %s would both become Orka Agent %q; import them into different namespaces", previous, agent.Source, name)
		}
		byName[name] = agent.Source
		agents = append(agents, kagentSourceAgent{APIVersion: apiVersion, Object: obj, Wrapped: agent})
	}
	if len(agents) == 0 {
		return nil, nil, "", fmt.Errorf("no kagent Agents found (expected %s kind Agent)", kagentV1APIVersion)
	}
	return agents, indexKagentObjects(objects), summarizeSkipped(skipped), nil
}

// wrapKagentAgents wraps every kagent v1 Agent in the input as an Orka Agent
// whose runtime is a per-agent AgentRuntime, and classifies its mode.
func wrapKagentAgents(data []byte, relayURL string, evidence kagentRevisionEvidence) ([]kagentWrappedAgent, string, error) {
	agents, related, skipped, err := collectKagentAgents(data)
	if err != nil {
		return nil, "", err
	}
	wrapped := make([]kagentWrappedAgent, 0, len(agents))
	for _, source := range agents {
		agent := source.Wrapped
		relay, err := kagentRelayOrigin(relayURL, metadataName(agent.Manifest))
		if err != nil {
			return nil, "", err
		}
		agent.Mode = classifyKagentAgent(source.Object, related, relay, evidence)
		wrapped = append(wrapped, agent)
	}
	return wrapped, skipped, nil
}

// translateKagentAgents converts every kagent v1 Agent into a native Orka
// Agent. It refuses the whole input if any field of any Agent cannot be
// translated, listing every problem.
func translateKagentAgents(data []byte, options kagentTranslateOptions) ([]kagentWrappedAgent, string, error) {
	agents, related, skipped, err := collectKagentAgents(data)
	if err != nil {
		return nil, "", err
	}
	translated := make([]kagentWrappedAgent, 0, len(agents))
	var refusals []string
	for _, source := range agents {
		spec, problems := translateKagentAgent(source.Object, related, options)
		if len(problems) > 0 {
			refusals = append(refusals, fmt.Sprintf("kagent Agent %s: %s", source.Wrapped.Source, strings.Join(problems, "; ")))
			continue
		}
		agent := source.Wrapped
		agent.Manifest = translatedKagentAgent(source.APIVersion, agent.Source, metadataName(agent.Manifest), spec)
		agent.RuntimeName, agent.Mode = "", kagentMode{Mode: kagentModeTranslated}
		translated = append(translated, agent)
	}
	if len(refusals) > 0 {
		return nil, "", fmt.Errorf("cannot translate:\n  %s\n\nTranslation maps only these fields:\n%s", strings.Join(refusals, "\n  "), kagentTranslationTable)
	}
	return translated, skipped, nil
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
		Source:          source,
		SourceNamespace: namespace,
		SourceName:      name,
		RuntimeName:     runtimeName,
		Manifest: map[string]any{
			"apiVersion": corev1alpha1.GroupVersion.String(),
			"kind":       "Agent",
			"metadata": map[string]any{
				cliNameKey: name,
				"annotations": map[string]any{
					kagentSourceAnnotation: apiVersion + " " + source,
				},
			},
			kagentFieldSpec: map[string]any{
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

// reconcileKagentWrappedAgents makes each Orka Agent match its manifest. It
// creates missing Agents, updates the spec of Agents that already wrap the
// same kagent Agent, and refuses to change any other existing Agent. Re-runs
// resume where an earlier run stopped.
func reconcileKagentWrappedAgents(cmd *cobra.Command, wrapped []kagentWrappedAgent) error {
	c := newClientFromCmd(cmd)
	out := cmd.OutOrStdout()
	ctx := context.Background()
	query := map[string]string{cliNamespaceQuery: c.Namespace}
	for _, agent := range wrapped {
		ensureManifestNamespace(agent.Manifest, c.Namespace)
		name := metadataName(agent.Manifest)
		path := "/api/v1/agents/" + url.PathEscape(name)
		action := "created"
		existing, err := c.DoJSON(ctx, http.MethodGet, path, query, nil)
		switch {
		case err != nil && strings.Contains(err.Error(), "HTTP 404"):
			body, err := json.Marshal(agent.Manifest)
			if err != nil {
				return fmt.Errorf("marshaling Agent %q: %w", name, err)
			}
			if _, err := c.DoJSON(ctx, http.MethodPost, "/api/v1/agents", nil, body); err != nil {
				return fmt.Errorf("creating Agent %q for kagent Agent %s: %w", name, agent.Source, err)
			}
		case err != nil:
			return fmt.Errorf("reading Agent %q: %w", name, err)
		default:
			current, _ := existing.(map[string]any)
			want := kagentString(agent.Manifest, "metadata", "annotations", kagentSourceAnnotation)
			if got := kagentString(current, "metadata", "annotations", kagentSourceAnnotation); got != want {
				return fmt.Errorf("agent %q already exists and does not wrap kagent Agent %s; remove it or import into another namespace", name, agent.Source)
			}
			action = "unchanged"
			if kagentSpecDrifted(current[kagentFieldSpec], agent.Manifest[kagentFieldSpec]) {
				body, err := json.Marshal(map[string]any{kagentFieldSpec: agent.Manifest[kagentFieldSpec]})
				if err != nil {
					return fmt.Errorf("marshaling Agent %q: %w", name, err)
				}
				if _, err := c.DoJSON(ctx, http.MethodPut, path, query, body); err != nil {
					return fmt.Errorf("updating Agent %q for kagent Agent %s: %w", name, agent.Source, err)
				}
				action = "updated"
			}
		}
		if agent.RuntimeName == "" {
			fmt.Fprintf(out, "Agent %s: %s (from kagent Agent %s; %s)\n", action, name, agent.Source, describeKagentMode(agent.Mode)) //nolint:errcheck
			continue
		}
		fmt.Fprintf(out, "Agent %s: %s (wraps kagent Agent %s; %s; needs AgentRuntime %s)\n", action, name, agent.Source, describeKagentMode(agent.Mode), agent.RuntimeName) //nolint:errcheck
	}
	return nil
}

// kagentOwnedSpecFields are the Agent spec fields the import writes in either
// mode. A field the import no longer sets must be removed, so each one is
// compared whole; fields the import never writes are left to their owners.
var kagentOwnedSpecFields = []string{"runtime", "systemPrompt", "model", "providerRef", kagentFieldTools}

// kagentSpecDrifted reports whether any import-owned spec field differs. The
// server defaults tools[].enabled to true, which the import never sets.
func kagentSpecDrifted(got, want any) bool {
	gotSpec, _ := got.(map[string]any)
	wantSpec, _ := want.(map[string]any)
	for _, field := range kagentOwnedSpecFields {
		current := gotSpec[field]
		if tools, ok := current.([]any); ok && field == kagentFieldTools {
			normalized := make([]any, 0, len(tools))
			for _, raw := range tools {
				if tool, ok := raw.(map[string]any); ok && tool["enabled"] == true {
					tool = maps.Clone(tool)
					delete(tool, "enabled")
					raw = tool
				}
				normalized = append(normalized, raw)
			}
			current = normalized
		}
		gotJSON, _ := json.Marshal(current)
		wantJSON, _ := json.Marshal(wantSpec[field])
		if !bytes.Equal(gotJSON, wantJSON) {
			return true
		}
	}
	return false
}
