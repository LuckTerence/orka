/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Wrapped kagent Agents run in one of two modes. Observed: kagent runs the
// Agent's model and tools; Orka governs dispatch and lifecycle. Governed:
// every model and tool endpoint points at the wrapper's relay, so Orka runs
// the tools under its policy and serves the model. Only the ADK harness can
// be governed: it forwards the turn's bearer to models and MCP servers.
const (
	kagentModeObserved = "observed"
	kagentModeGoverned = "governed"

	kagentDiscoveryLabel = "kagent.dev/discovery"
	// The relay may hold a tool call for an Orka approval (up to 15 minutes)
	// and waits 20 minutes; kagent's MCP client must wait longer still.
	kagentMinRelayTimeout = 20 * time.Minute
)

var kagentRevisionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// kagentMode is the onboarding verdict for one kagent Agent.
type kagentMode struct {
	Mode string
	// Reasons explain why an Agent is observed rather than governed.
	Reasons []string
	// RelayOrigin is the relay every governed endpoint points at.
	RelayOrigin string
	// Revision is the Agent's latest successful kagent revision, if known.
	Revision string
}

// kagentEgressEvidence binds a verdict to what a revision can reach. The
// compiled egress allowlist, enforced by kagent's gateway, is the boundary
// that keeps a revision's model and tools behind the relay; the declared
// resources alone cannot prove what an immutable revision was compiled from.
type kagentEgressEvidence struct {
	// Revisions maps a kagent revision to its compiled egress destinations.
	Revisions map[string][]string
	// Accepted are origins a governed revision may reach besides its relay,
	// such as the runtime control-plane filter.
	Accepted []string
}

// kagentObjects indexes the kagent resources supplied next to the Agents.
type kagentObjects map[string]map[string]any

func kagentObjectKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

func indexKagentObjects(objects []map[string]any) kagentObjects {
	index := kagentObjects{}
	for _, obj := range objects {
		apiVersion, _ := obj["apiVersion"].(string)
		if apiVersion != kagentV1APIVersion {
			continue
		}
		kind, _ := obj["kind"].(string)
		index[kagentObjectKey(kind, kagentString(obj, "metadata", cliNamespaceQuery), kagentString(obj, "metadata", cliNameKey))] = obj
	}
	return index
}

func (o kagentObjects) lookup(kind, namespace, name string) map[string]any {
	return o[kagentObjectKey(kind, namespace, name)]
}

// classifyKagentAgent decides governed or observed from the Agent and the
// related objects in the same input. relay is the wrapper relay's origin for
// this Agent, established by the operator; endpoint shape alone never proves
// that a URL is Orka's relay. evidence binds the verdict to what the pinned
// revision can reach. Anything it cannot prove keeps the Agent observed.
func classifyKagentAgent(agent map[string]any, related kagentObjects, relay string, evidence kagentEgressEvidence) kagentMode {
	namespace := kagentString(agent, "metadata", cliNamespaceQuery)
	mode := kagentMode{Revision: kagentString(agent, "status", "latestSuccessfulRevision")}
	var reasons []string
	if relay == "" {
		reasons = append(reasons, "no relay was named (--relay-url)")
	}
	// The wrapper pins the latest successful revision, so the specs checked
	// here must be the ones that revision was compiled from.
	desired := kagentString(agent, "status", "desiredRevision")
	switch generation, observed := kagentInt(agent, "metadata", "generation"), kagentInt(agent, "status", "observedGeneration"); {
	case generation == 0 || observed != generation:
		reasons = append(reasons, "kagent has not observed the Agent's current generation")
	case mode.Revision == "" || desired != mode.Revision:
		reasons = append(reasons, "the latest successful revision is not the one the current spec compiles to")
	}
	harnessName := kagentString(agent, "spec", "harnessRef", cliNameKey)
	reasons = append(reasons, harnessProblems(related.lookup("Harness", namespace, harnessName), harnessName)...)
	templateReasons, origins := templateProblems(agent, related, namespace)
	reasons = append(reasons, templateReasons...)
	for _, origin := range origins {
		if relay != "" && origin != relay {
			reasons = append(reasons, fmt.Sprintf("an endpoint points at %s, not the relay %s", origin, relay))
		}
	}
	reasons = append(reasons, egressProblems(evidence, mode.Revision, relay)...)
	if len(reasons) > 0 {
		mode.Mode, mode.Reasons = kagentModeObserved, reasons
		return mode
	}
	mode.Mode, mode.RelayOrigin = kagentModeGoverned, relay
	return mode
}

func harnessProblems(harness map[string]any, name string) []string {
	if harness == nil {
		return []string{fmt.Sprintf("harness %q is not in the input", name)}
	}
	if kagentMap(harness, "spec", "kagent") == nil {
		return []string{fmt.Sprintf("harness %q is not the ADK (kagent) harness", name)}
	}
	var problems []string
	// Only the default workload is known to run the compiled configuration.
	workload := kagentMap(harness, "spec", "workload")
	for _, field := range []string{"image", "command", "args"} {
		if _, set := workload[field]; set {
			problems = append(problems, fmt.Sprintf("harness %q overrides the workload %s", name, field))
		}
	}
	if kagentMap(harness, "spec", "kagent", "memory") != nil {
		problems = append(problems, fmt.Sprintf("harness %q enables memory", name))
	}
	if kagentMap(harness, "spec", "kagent", "compaction") != nil {
		problems = append(problems, fmt.Sprintf("harness %q compacts with its own model", name))
	}
	return problems
}

// templateProblems checks the Agent's model and tools, and returns the origins
// their endpoints point at.
func templateProblems(agent map[string]any, related kagentObjects, namespace string) ([]string, []string) {
	template := kagentMap(agent, "spec", "template", "inline")
	if template == nil {
		ref := kagentString(agent, "spec", "template", "ref", cliNameKey)
		found := related.lookup("AgentTemplate", namespace, ref)
		if found == nil {
			return []string{fmt.Sprintf("agent template %q is not in the input", ref)}, nil
		}
		template = kagentMap(found, "spec")
	}
	var problems []string
	origins := map[string]bool{}
	// These checks read the declared resources. The revision-bound boundary
	// is the compiled egress checked below: with bash disabled for skills, a
	// local skill or plugin that survives in the pinned revision can reach
	// only the relay and the accepted origins, so it cannot act outside Orka.
	for _, field := range []string{"skills", "plugins"} {
		if items, _ := template[field].([]any); len(items) > 0 {
			problems = append(problems, fmt.Sprintf("the template uses %s, which run inside kagent", field))
		}
	}
	modelName := kagentString(template, "modelConfig", cliNameKey)
	switch model := related.lookup("ModelConfig", namespace, modelName); {
	case model == nil:
		problems = append(problems, fmt.Sprintf("model config %q is not in the input", modelName))
	case kagentString(model, "spec", "provider") != "OpenAI":
		problems = append(problems, fmt.Sprintf("model config %q does not use the OpenAI provider", modelName))
	case !kagentBool(model, "spec", "apiKeyPassthrough"):
		problems = append(problems, fmt.Sprintf("model config %q does not pass the caller's bearer through", modelName))
	default:
		if origin, ok := relayOrigin(kagentString(model, "spec", "openAI", "baseUrl"), "/v1"); ok {
			origins[origin] = true
		} else {
			problems = append(problems, fmt.Sprintf("model config %q does not point at a relay's /v1", modelName))
		}
	}
	tools, _ := template["tools"].([]any)
	for i, raw := range tools {
		binding, _ := raw.(map[string]any)
		toolProblems, origin := toolBindingProblems(binding, i, related, namespace)
		problems = append(problems, toolProblems...)
		if origin != "" {
			origins[origin] = true
		}
	}
	sorted := make([]string, 0, len(origins))
	for origin := range origins {
		sorted = append(sorted, origin)
	}
	slices.Sort(sorted)
	return problems, sorted
}

func toolBindingProblems(binding map[string]any, index int, related kagentObjects, namespace string) ([]string, string) {
	server := kagentMap(binding, "mcp", "server")
	if server == nil {
		return []string{fmt.Sprintf("tool %d is not an MCP binding", index)}, ""
	}
	serverName, _ := server[cliNameKey].(string)
	if kind, _ := server["kind"].(string); kind != "RemoteMCPServer" {
		return []string{fmt.Sprintf("tool server %q is a %s, not a RemoteMCPServer", serverName, kind)}, ""
	}
	remote := related.lookup("RemoteMCPServer", namespace, serverName)
	if remote == nil {
		return []string{fmt.Sprintf("tool server %q is not in the input", serverName)}, ""
	}
	var problems []string
	for _, reason := range governedRemoteMCPServerProblems(remote) {
		problems = append(problems, fmt.Sprintf("tool server %q %s", serverName, reason))
	}
	origin, _ := relayOrigin(kagentString(remote, "spec", "url"), "/mcp")
	return problems, origin
}

// egressProblems requires the pinned revision's compiled egress to reach only
// the relay and the accepted origins.
func egressProblems(evidence kagentEgressEvidence, revision, relay string) []string {
	egress, ok := evidence.Revisions[revision]
	if !ok {
		return []string{fmt.Sprintf("no compiled egress was supplied for revision %q (--revision-egress)", revision)}
	}
	var problems []string
	for _, destination := range egress {
		if origin, ok := relayOrigin(destination, ""); !ok || (origin != relay && !slices.Contains(evidence.Accepted, origin)) {
			problems = append(problems, "the pinned revision can reach "+destination)
		}
	}
	return problems
}

// kagentRelayOrigin expands the --relay-url template for one Agent.
func kagentRelayOrigin(template, name string) (string, error) {
	if template == "" {
		return "", nil
	}
	origin, ok := relayOrigin(strings.ReplaceAll(template, "{name}", name), "")
	if !ok {
		return "", fmt.Errorf("--relay-url must be an http(s) origin with no path, optionally using {name}")
	}
	return origin, nil
}

func governedRemoteMCPServerProblems(remote map[string]any) []string {
	var problems []string
	if kagentString(remote, "spec", "protocol") != "STREAMABLE_HTTP" {
		problems = append(problems, "does not use STREAMABLE_HTTP")
	}
	if _, ok := relayOrigin(kagentString(remote, "spec", "url"), "/mcp"); !ok {
		problems = append(problems, "does not point at a relay's /mcp")
	}
	if timeout, err := time.ParseDuration(kagentString(remote, "spec", "timeout")); err != nil || timeout < kagentMinRelayTimeout {
		problems = append(problems, fmt.Sprintf("needs a timeout of at least %s", kagentMinRelayTimeout))
	}
	if labels := kagentMap(remote, "metadata", "labels"); labels == nil || labels[kagentDiscoveryLabel] != "disabled" {
		problems = append(problems, "does not disable discovery with "+kagentDiscoveryLabel+"=disabled")
	}
	if headers, _ := kagentMap(remote, "spec")["headersFrom"].([]any); len(headers) > 0 {
		problems = append(problems, "sends static headers")
	}
	return problems
}

// relayOrigin returns the http(s) origin of value when its path is exactly path.
func relayOrigin(value, path string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.TrimSuffix(u.Path, "/") != path {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// kagentWrapperOptions configure the per-Agent wrapper configuration files.
type kagentWrapperOptions struct {
	Dir             string
	ControlPlaneURL string
	UserIDPrefix    string
	UpstreamURL     string
	UpstreamModel   string
}

// kagentWrapperConfig mirrors the wrapper's /agent/kagent.json.
type kagentWrapperConfig struct {
	Model    string                 `json:"model"`
	Kagent   kagentWrapperTarget    `json:"kagent"`
	Governed *kagentWrapperGoverned `json:"governed,omitempty"`
}

type kagentWrapperTarget struct {
	ControlPlaneURL  string            `json:"controlPlaneURL"`
	Agent            map[string]string `json:"agent"`
	ExpectedRevision string            `json:"expectedRevision"`
	UserIDPrefix     string            `json:"userIDPrefix"`
}

type kagentWrapperGoverned struct {
	UpstreamBaseURL string `json:"upstreamBaseURL"`
	UpstreamModel   string `json:"upstreamModel"`
}

func (o kagentWrapperOptions) validate(wrapped []kagentWrappedAgent) error {
	if o.Dir == "" {
		return nil
	}
	if _, ok := relayOrigin(o.ControlPlaneURL, ""); !ok {
		return fmt.Errorf("--control-plane-url must be the kagent controller's http(s) base URL")
	}
	for _, agent := range wrapped {
		if !kagentRevisionPattern.MatchString(agent.Mode.Revision) {
			return fmt.Errorf("kagent Agent %s has no status.latestSuccessfulRevision to pin; export it with kubectl get -o yaml after it is Ready", agent.Source)
		}
		if agent.Mode.Mode == kagentModeGoverned && (o.UpstreamURL == "" || o.UpstreamModel == "") {
			return fmt.Errorf("kagent Agent %s is governed: --model-upstream-url and --model-upstream-model name the model Orka serves it", agent.Source)
		}
	}
	return nil
}

// writeKagentWrapperConfigs writes <name>.kagent.json for each Agent. The
// output is deterministic, so a re-run produces the same image digest.
func writeKagentWrapperConfigs(o kagentWrapperOptions, wrapped []kagentWrappedAgent) ([]string, error) {
	if o.Dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(o.Dir, 0o750); err != nil {
		return nil, err
	}
	var written []string
	for _, agent := range wrapped {
		namespace, name, _ := strings.Cut(agent.Source, "/")
		config := kagentWrapperConfig{
			Model: "kagent/" + namespace + "/" + name,
			Kagent: kagentWrapperTarget{
				ControlPlaneURL: strings.TrimSuffix(o.ControlPlaneURL, "/"), Agent: map[string]string{cliNamespaceQuery: namespace, cliNameKey: name},
				ExpectedRevision: agent.Mode.Revision, UserIDPrefix: o.UserIDPrefix,
			},
		}
		if agent.Mode.Mode == kagentModeGoverned {
			config.Governed = &kagentWrapperGoverned{UpstreamBaseURL: o.UpstreamURL, UpstreamModel: o.UpstreamModel}
		}
		data, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(o.Dir, metadataName(agent.Manifest)+".kagent.json")
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	slices.Sort(written)
	return written, nil
}

func describeKagentMode(mode kagentMode) string {
	if mode.Mode == kagentModeGoverned {
		return "governed via " + mode.RelayOrigin
	}
	return "observed: " + strings.Join(mode.Reasons, "; ")
}

func kagentMap(obj map[string]any, path ...string) map[string]any {
	current := obj
	for _, key := range path {
		next, _ := current[key].(map[string]any)
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

func kagentString(obj map[string]any, path ...string) string {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(string)
	return strings.TrimSpace(value)
}

func kagentInt(obj map[string]any, path ...string) int64 {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(float64)
	return int64(value)
}

func kagentBool(obj map[string]any, path ...string) bool {
	parent := kagentMap(obj, path[:len(path)-1]...)
	value, _ := parent[path[len(path)-1]].(bool)
	return value
}

// readKagentEgressEvidence loads --revision-egress and --accepted-egress.
func readKagentEgressEvidence(path string, accepted []string) (kagentEgressEvidence, error) {
	evidence := kagentEgressEvidence{Revisions: map[string][]string{}}
	for _, origin := range accepted {
		normalized, ok := relayOrigin(origin, "")
		if !ok {
			return evidence, fmt.Errorf("--accepted-egress %q must be an http(s) origin with no path", origin)
		}
		evidence.Accepted = append(evidence.Accepted, normalized)
	}
	if path == "" {
		return evidence, nil
	}
	data, err := readFileOrStdin(path)
	if err != nil {
		return evidence, fmt.Errorf("reading --revision-egress: %w", err)
	}
	if err := json.Unmarshal(data, &evidence.Revisions); err != nil {
		return evidence, fmt.Errorf("--revision-egress must be a JSON object of revision to destinations: %w", err)
	}
	return evidence, nil
}
