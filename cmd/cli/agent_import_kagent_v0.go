/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// kagent v0 (kagent.dev/v1alpha2, the v0.10 releases) runs each Declarative
// Agent as one Deployment shared by all its sessions, and compiles no
// revisions. The wrapper pins instead the kagent.dev/config-hash annotation
// on the Deployment's pod template: kagent derives it from the compiled
// agent configuration, agent card, and secrets, and rolls the Deployment
// whenever it changes. A governed verdict reads that compiled configuration
// from the Agent's config Secret and recomputes the hash, so the verdict is
// about exactly the configuration the wrapper pins, not about source objects
// kagent may not have recompiled yet.
const (
	kagentV0APIVersion         = "kagent.dev/v1alpha2"
	kagentVersionV0            = "v0"
	kagentConfigHashAnnotation = "kagent.dev/config-hash"
	kagentV0RuntimeGo          = "go"
	kagentFieldDeclarative     = "declarative"
	kagentFieldMCPServer       = "mcpServer"
	kagentFieldToolNames       = "toolNames"
	kagentFieldAnnotations     = "annotations"
	kagentFieldRuntime         = "runtime"
	kagentFieldKind            = "kind"
	kagentFieldInline          = "inline"
	kagentFieldMemory          = "memory"
	kagentFieldModelConfig     = "modelConfig"
	kagentToolTypeMCPServer    = "McpServer"
	// kagentV0Container and kagentV0GoImage name the agent container and the
	// Go ADK image repository kagent runs it from (both image variants).
	kagentV0Container = "kagent"
	kagentV0GoImage   = "golang-adk"
)

// kagentConfigHashPattern matches the decimal uint64 kagent writes.
var kagentConfigHashPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// kagentV0SingleReplica explains why a v0 Agent must run one replica.
const kagentV0SingleReplica = "kagent v0 cancels a task only in the pod that runs it, so with more replicas a cancel can land on a pod that is not running the turn"

// kagentV0WrapRefusals lists why the wrapper cannot run a v0 Agent at all,
// in either mode.
func kagentV0WrapRefusals(agent, deployment map[string]any) []string {
	var problems []string
	switch kind := kagentString(agent, kagentFieldSpec, "type"); kind {
	case "Declarative":
	case "":
		problems = append(problems, "it sets no spec.type; only Declarative Agents run on a runtime the wrapper knows")
	default:
		problems = append(problems, fmt.Sprintf("it is a %s Agent; only Declarative Agents run on a runtime the wrapper knows", kind))
	}
	switch runtime := kagentString(agent, kagentFieldSpec, kagentFieldDeclarative, kagentFieldRuntime); runtime {
	case kagentV0RuntimeGo:
	case "":
		problems = append(problems, "it sets no declarative.runtime, so kagent runs it on the Python runtime; set runtime: go")
	default:
		problems = append(problems, fmt.Sprintf("it runs on the %q runtime; only the Go runtime can be wrapped: the Python runtime cannot cancel a task and saves request headers, including the turn's bearer, to kagent's database", runtime))
	}
	for i, tool := range kagentV0Tools(agent) {
		if approval, _ := kagentMap(tool, kagentFieldMCPServer)["requireApproval"].([]any); len(approval) > 0 {
			// A wrapped Agent cannot answer kagent's approval prompt; Orka's
			// approval policy gates relayed tools instead.
			problems = append(problems, fmt.Sprintf("tool %d requires kagent approval (requireApproval), which a wrapped Agent cannot answer", i))
		}
	}
	if replicas, set := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative, "deployment")["replicas"].(float64); set && replicas != 1 {
		problems = append(problems, fmt.Sprintf("it asks for %d replicas (declarative.deployment.replicas); %s", int64(replicas), kagentV0SingleReplica))
	}
	if deployment == nil {
		return append(problems, "its Deployment is not in the input, so the import cannot confirm it runs exactly one replica; "+kagentV0SingleReplica)
	}
	specReplicas := int64(1) // the Deployment default
	if value, ok := kagentMap(deployment, kagentFieldSpec)["replicas"].(float64); ok {
		specReplicas = int64(value)
	}
	if running := kagentInt(deployment, "status", "replicas"); specReplicas != 1 || running != 1 {
		problems = append(problems, fmt.Sprintf("its Deployment runs %d replicas of %d (status.replicas, spec.replicas), not exactly one; %s", running, specReplicas, kagentV0SingleReplica))
	}
	return problems
}

func kagentV0Tools(agent map[string]any) []map[string]any {
	raw, _ := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative)[kagentFieldTools].([]any)
	tools := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		tool, _ := item.(map[string]any)
		tools = append(tools, tool)
	}
	return tools
}

// classifyKagentV0Agent decides governed or observed for a v0 Agent, and
// finds the config hash the wrapper pins. v0 compiles no egress allowlist:
// what keeps a governed Agent behind the relay is its pinned compiled
// configuration, with every endpoint on the relay and nothing that runs code
// or calls out on its own. Anything the import cannot see keeps the Agent
// observed.
func classifyKagentV0Agent(agent map[string]any, related kagentObjects, relay string) kagentMode {
	namespace := kagentString(agent, kagentFieldMetadata, cliNamespaceQuery)
	name := kagentString(agent, kagentFieldMetadata, cliNameKey)
	var mode kagentMode
	mode.Revision, mode.PinProblems = kagentV0Pin(agent, related.lookup("Deployment", namespace, name), name)
	var reasons []string
	if relay == "" {
		reasons = append(reasons, "no relay was named (--relay-url)")
	}
	reasons = append(reasons, mode.PinProblems...)
	reasons = append(reasons, kagentV0BehaviorProblems(agent)...)
	modelName := kagentString(agent, kagentFieldSpec, kagentFieldDeclarative, kagentFieldModelConfig)
	compiled, compiledProblems := kagentV0CompiledConfig(related.lookup("Secret", namespace, name),
		related.lookup("ModelConfig", namespace, modelName), name, modelName, mode.Revision)
	reasons = append(reasons, compiledProblems...)
	origins := map[string]bool{}
	if compiled != nil {
		configProblems, configOrigins := kagentV0ConfigProblems(compiled)
		reasons = append(reasons, configProblems...)
		for _, origin := range configOrigins {
			origins[origin] = true
		}
	}
	for _, origin := range slices.Sorted(maps.Keys(origins)) {
		if relay != "" && origin != relay {
			reasons = append(reasons, fmt.Sprintf("an endpoint points at %s, not the relay %s", origin, relay))
		}
	}
	if len(reasons) > 0 {
		mode.Mode, mode.Reasons = kagentModeObserved, reasons
		return mode
	}
	mode.Mode, mode.RelayOrigin = kagentModeGoverned, relay
	return mode
}

// kagentV0Pin returns the Deployment's config hash when the running Agent is
// exactly the one the input describes: kagent has accepted the Agent's
// current generation and it is Ready, and the Deployment runs the Go ADK and
// has finished rolling out to that hash. Ready alone is not enough: a failed
// reconcile leaves the previous Deployment Ready while Accepted turns False.
func kagentV0Pin(agent, deployment map[string]any, name string) (string, []string) {
	var problems []string
	generation := kagentInt(agent, kagentFieldMetadata, "generation")
	if generation == 0 || kagentInt(agent, "status", "observedGeneration") != generation {
		problems = append(problems, "kagent has not observed the Agent's current generation")
	}
	if !kagentConditionCurrent(agent, "Accepted", generation) {
		problems = append(problems, "kagent has not accepted the Agent's current generation")
	}
	if !kagentConditionCurrent(agent, "Ready", generation) {
		problems = append(problems, "the Agent is not Ready")
	}
	if deployment == nil {
		return "", append(problems, fmt.Sprintf("deployment %q is not in the input", name))
	}
	if image := kagentV0ContainerImage(deployment); kagentImageRepositoryName(image) != kagentV0GoImage {
		problems = append(problems, fmt.Sprintf("deployment %q runs %q, not kagent's Go runtime", name, image))
	}
	replicas := int64(1) // the Deployment default
	if value, ok := kagentMap(deployment, kagentFieldSpec)["replicas"].(float64); ok {
		replicas = int64(value)
	}
	rollout := kagentInt(deployment, kagentFieldMetadata, "generation")
	switch {
	case rollout == 0 || kagentInt(deployment, "status", "observedGeneration") != rollout:
		problems = append(problems, fmt.Sprintf("deployment %q has not been observed at its current generation", name))
	case replicas < 1:
		problems = append(problems, fmt.Sprintf("deployment %q has no replicas", name))
	case kagentInt(deployment, "status", "replicas") != replicas || kagentInt(deployment, "status", "updatedReplicas") != replicas ||
		kagentInt(deployment, "status", "availableReplicas") != replicas:
		problems = append(problems, fmt.Sprintf("deployment %q has not finished rolling out (status replicas, updatedReplicas, and availableReplicas must all equal spec.replicas)", name))
	}
	hash := kagentString(deployment, kagentFieldSpec, "template", kagentFieldMetadata, kagentFieldAnnotations, kagentConfigHashAnnotation)
	if _, err := strconv.ParseUint(hash, 10, 64); err != nil || !kagentConfigHashPattern.MatchString(hash) {
		problems = append(problems, fmt.Sprintf("deployment %q has no %s annotation on its pod template", name, kagentConfigHashAnnotation))
	}
	if len(problems) > 0 {
		return "", problems
	}
	return hash, nil
}

// kagentConditionCurrent reports whether the condition is True and, when it
// records one, was set for the given generation.
func kagentConditionCurrent(obj map[string]any, conditionType string, generation int64) bool {
	conditions, _ := kagentMap(obj, "status")["conditions"].([]any)
	return slices.ContainsFunc(conditions, func(raw any) bool {
		condition, _ := raw.(map[string]any)
		observed, recorded := condition["observedGeneration"].(float64)
		return condition["type"] == conditionType && condition["status"] == "True" && (!recorded || int64(observed) == generation)
	})
}

// kagentV0ContainerImage returns the image of the Deployment's agent container.
func kagentV0ContainerImage(deployment map[string]any) string {
	containers, _ := kagentMap(deployment, kagentFieldSpec, "template", kagentFieldSpec)["containers"].([]any)
	for _, raw := range containers {
		container, _ := raw.(map[string]any)
		if container[cliNameKey] == kagentV0Container {
			image, _ := container["image"].(string)
			return image
		}
	}
	return ""
}

// kagentImageRepositoryName returns the last path segment of an image's
// repository, without its tag or digest.
func kagentImageRepositoryName(image string) string {
	repository, _, _ := strings.Cut(image, "@")
	if slash := strings.LastIndex(repository, "/"); slash >= 0 {
		repository = repository[slash+1:]
	}
	name, _, _ := strings.Cut(repository, ":")
	return name
}

// kagentV0BehaviorProblems lists the Agent settings that run code or change
// the agent's runtime outside the compiled configuration, so the config hash
// does not show them.
func kagentV0BehaviorProblems(agent map[string]any) []string {
	var problems []string
	if _, set := kagentMap(agent, kagentFieldSpec)["skills"]; set {
		problems = append(problems, "the Agent loads skills, which run inside kagent")
	}
	// Environment can replace the forwarded bearer (STS) or reroute tools,
	// and an image registry or extra container runs code the hash does not cover.
	deployment := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative, "deployment")
	for _, field := range []string{"env", "envFrom", "volumes", "volumeMounts", "extraContainers", "imageRegistry"} {
		if _, set := deployment[field]; set {
			problems = append(problems, "the Agent's deployment sets "+field)
		}
	}
	return problems
}

// kagentV0CompiledConfig reads the compiled configuration from the Agent's
// config Secret and requires it to be the one the Deployment runs. kagent
// hashes the compiled config.json, the agent card, the secret-hash bytes of
// what the configuration depends on, and any code-sandbox settings, in that
// order, and pins the first 8 bytes as a decimal. For a governed
// configuration the only secret-hash bytes are the ModelConfig's
// status.secretHash: the SHA-256 of the secrets the model uses, or of nothing
// when it uses none (a relay RemoteMCPServer is plain HTTP, so it pins no TLS
// Secret). Anything else (more secret material, a stale status, or a Secret
// kagent has not rolled out yet) fails the comparison and so fails closed.
func kagentV0CompiledConfig(secret, model map[string]any, name, modelName, pinned string) (map[string]any, []string) {
	if secret == nil {
		return nil, []string{fmt.Sprintf("the Agent's config Secret %q is not in the input; a governed verdict reads the compiled configuration it holds", name)}
	}
	var problems []string
	var secretHash []byte
	secretHashKnown := false
	if model == nil {
		problems = append(problems, fmt.Sprintf("model config %q is not in the input; kagent folds its status.secretHash into the config hash", modelName))
	} else if decoded, err := hex.DecodeString(kagentString(model, "status", "secretHash")); err != nil {
		problems = append(problems, fmt.Sprintf("model config %q has a status.secretHash the import cannot read", modelName))
	} else {
		secretHash, secretHashKnown = decoded, true
	}
	data := kagentMap(secret, "data")
	decode := func(key string) ([]byte, bool) {
		value, _ := data[key].(string)
		decoded, err := base64.StdEncoding.DecodeString(value)
		return decoded, value != "" && err == nil
	}
	cfg, ok := decode("config.json")
	if !ok {
		return nil, append(problems, fmt.Sprintf("config Secret %q has no readable config.json", name))
	}
	card, _ := decode("agent-card.json")
	sandbox, hasSandbox := decode("srt-settings.json")
	if hasSandbox {
		problems = append(problems, "the Agent runs a code sandbox (srt-settings.json)")
	}
	if pinned != "" && secretHashKnown {
		sum := sha256.New()
		sum.Write(cfg)
		sum.Write(card)
		sum.Write(secretHash)
		sum.Write(sandbox)
		if got := strconv.FormatUint(binary.BigEndian.Uint64(sum.Sum(nil)[:8]), 10); got != pinned {
			problems = append(problems, fmt.Sprintf("config Secret %q hashes to %s, not the pinned %s: kagent has not rolled it out yet, or it depends on secret material beyond its ModelConfig's", name, got, pinned))
		}
	}
	var config map[string]any
	if err := json.Unmarshal(cfg, &config); err != nil || config == nil {
		return nil, append(problems, fmt.Sprintf("config Secret %q holds a config.json that is not a JSON object", name))
	}
	return config, problems
}

// kagentV0ConfigProblems checks a compiled v0 configuration for a governed
// Agent and returns the origins its endpoints point at.
func kagentV0ConfigProblems(config map[string]any) ([]string, []string) {
	var problems, origins []string
	model := kagentMap(config, "model")
	switch {
	case kagentString(model, "type") != "openai":
		problems = append(problems, fmt.Sprintf("the compiled model is %q, not OpenAI", kagentString(model, "type")))
	case !kagentBool(model, "api_key_passthrough"):
		problems = append(problems, "the compiled model does not pass the caller's bearer through")
	case !slices.Contains([]string{"", "chatCompletions"}, kagentString(model, "api_format")):
		problems = append(problems, fmt.Sprintf("the compiled model uses the %s API; the relay serves only chat completions", kagentString(model, "api_format")))
	case len(kagentMap(model, "headers")) > 0:
		problems = append(problems, "the compiled model sends static headers")
	case kagentMap(model, "token_exchange") != nil:
		problems = append(problems, "the compiled model exchanges its own tokens")
	default:
		if origin, ok := relayOrigin(kagentString(model, "base_url"), "/v1"); ok {
			origins = append(origins, origin)
		} else {
			problems = append(problems, "the compiled model does not point at a relay's /v1")
		}
	}
	tools, _ := config["http_tools"].([]any)
	for i, raw := range tools {
		tool, _ := raw.(map[string]any)
		params := kagentMap(tool, "params")
		if origin, ok := relayOrigin(kagentString(params, "url"), "/mcp"); ok {
			origins = append(origins, origin)
		} else {
			problems = append(problems, fmt.Sprintf("compiled tool server %d (%s) does not point at a relay's /mcp", i, kagentString(params, "url")))
		}
		if len(kagentMap(params, "headers")) > 0 {
			problems = append(problems, fmt.Sprintf("compiled tool server %d sends static headers", i))
		}
		if timeout, _ := params["timeout"].(float64); timeout <= kagentRelayTimeout.Seconds() {
			problems = append(problems, fmt.Sprintf("compiled tool server %d needs a timeout longer than %s", i, kagentRelayTimeout))
		}
		allowed, _ := tool["allowed_headers"].([]any)
		if !slices.ContainsFunc(allowed, func(raw any) bool {
			header, _ := raw.(string)
			return strings.EqualFold(strings.TrimSpace(header), "Authorization")
		}) {
			problems = append(problems, fmt.Sprintf("compiled tool server %d does not forward Authorization (allowedHeaders)", i))
		}
		if approval, _ := tool["require_approval"].([]any); len(approval) > 0 {
			problems = append(problems, fmt.Sprintf("compiled tool server %d requires kagent approval", i))
		}
	}
	for field, reason := range map[string]string{
		"sse_tools":     "the compiled configuration uses SSE tool servers",
		"remote_agents": "the compiled configuration calls other Agents",
	} {
		if items, _ := config[field].([]any); len(items) > 0 {
			problems = append(problems, reason)
		}
	}
	for field, reason := range map[string]string{
		"execute_code": "the compiled configuration executes code blocks inside kagent",
		"share_tools":  "the compiled configuration has kagent's share-link tools",
	} {
		if kagentBool(config, field) {
			problems = append(problems, reason)
		}
	}
	for field, reason := range map[string]string{
		kagentFieldMemory: "the compiled configuration enables memory, which calls its own model",
		"context_config":  "the compiled configuration compacts its context, which can call its own model",
	} {
		if value, set := config[field]; set && value != nil {
			problems = append(problems, reason)
		}
	}
	slices.Sort(problems)
	return problems, origins
}

// kagentV0TranslatedDeploymentFields are deployment settings that only place
// kagent's pod; a native Orka Agent has none, and dropping them changes no
// behavior. Every other deployment field is refused.
var kagentV0TranslatedDeploymentFields = []string{
	"replicas", "resources", "tolerations", "affinity", "nodeSelector", "labels", kagentFieldAnnotations,
	"deploymentAnnotations", "imagePullSecrets", "imagePullPolicy", "securityContext",
	"podSecurityContext", "serviceAccountName", "serviceAccountConfig",
}

// translateKagentV0Agent returns the native Orka Agent spec for a v0 Agent,
// or every reason it cannot be translated.
func translateKagentV0Agent(agent map[string]any, related kagentObjects, options kagentTranslateOptions) (map[string]any, []string) {
	namespace := kagentString(agent, kagentFieldMetadata, cliNamespaceQuery)
	problems := untranslatableKagentV0Fields(agent)
	declarative := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative)
	if declarative == nil {
		problems = append(problems, "it has no declarative spec")
		slices.Sort(problems)
		return nil, problems
	}
	spec := map[string]any{}
	prompt, promptProblem := translateKagentV0SystemMessage(declarative)
	if promptProblem != "" {
		problems = append(problems, promptProblem)
	} else if prompt != nil {
		spec["systemPrompt"] = prompt
	}
	modelName := kagentString(declarative, kagentFieldModelConfig)
	if model := related.lookup("ModelConfig", namespace, modelName); model == nil {
		problems = append(problems, fmt.Sprintf("model config %q is not in the input", modelName))
	} else {
		translatedModel, providerRef, modelProblems := translateKagentModel(model, options.ProviderRef)
		for _, problem := range modelProblems {
			problems = append(problems, fmt.Sprintf("model config %q %s", modelName, problem))
		}
		spec["model"] = translatedModel
		if providerRef != "" {
			spec["providerRef"] = map[string]any{cliNameKey: providerRef}
		}
	}
	refs, toolProblems := translateKagentV0Tools(agent, options)
	problems = append(problems, toolProblems...)
	if len(refs) > 0 {
		spec[kagentFieldTools] = refs
	}
	slices.Sort(problems)
	return spec, problems
}

// untranslatableKagentV0Fields lists the Agent fields translation refuses.
func untranslatableKagentV0Fields(agent map[string]any) []string {
	var problems []string
	for field := range kagentMap(agent, kagentFieldSpec) {
		switch field {
		case "type", kagentFieldDeclarative, kagentFieldDescription, "iconUrl", "documentationUrl", "version", cliProviderKey:
			// Agent card metadata; an Orka Agent has no card.
		default:
			problems = append(problems, fmt.Sprintf("spec.%s has no Orka equivalent", field))
		}
	}
	if kind := kagentString(agent, kagentFieldSpec, "type"); kind != "Declarative" {
		problems = append(problems, fmt.Sprintf("it is a %q Agent; only Declarative Agents translate", kind))
	}
	declarative := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative)
	for field := range declarative {
		switch field {
		case "systemMessage", "systemMessageFrom", kagentFieldModelConfig, kagentFieldTools, "stream", kagentFieldRuntime:
		case "executeCodeBlocks", "shareTools":
			if kagentBool(declarative, field) {
				problems = append(problems, fmt.Sprintf("declarative.%s is enabled, which Orka Agents do not have", field))
			}
		case "deployment":
			for setting := range kagentMap(declarative, "deployment") {
				if !slices.Contains(kagentV0TranslatedDeploymentFields, setting) {
					problems = append(problems, fmt.Sprintf("declarative.deployment.%s changes the agent's runtime, which Orka Agents do not have", setting))
				}
			}
		default:
			problems = append(problems, fmt.Sprintf("declarative.%s has no Orka equivalent", field))
		}
	}
	return problems
}

// translateKagentV0SystemMessage maps the system message onto
// spec.systemPrompt, or returns why it cannot.
func translateKagentV0SystemMessage(declarative map[string]any) (map[string]any, string) {
	from := kagentMap(declarative, "systemMessageFrom")
	switch {
	case kagentString(declarative, "systemMessage") != "" && from != nil:
		return nil, "the Agent sets both systemMessage and systemMessageFrom"
	case kagentString(declarative, "systemMessage") != "":
		return map[string]any{kagentFieldInline: declarative["systemMessage"]}, ""
	case from != nil && kagentString(from, "type") != "ConfigMap":
		return nil, fmt.Sprintf("the Agent reads its system message from a %s; Orka reads system prompts from ConfigMaps", kagentString(from, "type"))
	case from != nil:
		return map[string]any{"configMapRef": map[string]any{
			cliNameKey: kagentString(from, cliNameKey), "key": kagentString(from, "key"),
		}}, ""
	}
	return nil, ""
}

// translateKagentV0Tools maps each MCP tool to the Orka Tool named by --tool.
func translateKagentV0Tools(agent map[string]any, options kagentTranslateOptions) ([]any, []string) {
	var refs []any
	var problems []string
	refuse := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	for i, tool := range kagentV0Tools(agent) {
		if toolType := kagentString(tool, "type"); toolType != kagentToolTypeMCPServer {
			refuse("tool %d is a %q tool; only MCP tools translate", i, toolType)
			continue
		}
		for field := range tool {
			if field != "type" && field != kagentFieldMCPServer {
				refuse("tool %d sets %s, which has no Orka equivalent", i, field)
			}
		}
		server := kagentMap(tool, kagentFieldMCPServer)
		// The server reference and tool names map to Orka Tools; every other
		// binding field changes behavior. An approval requirement in particular
		// must never be dropped by a plain Orka Tool reference.
		for field := range server {
			if !slices.Contains([]string{kagentFieldKind, "apiGroup", cliNameKey, cliNamespaceQuery, kagentFieldToolNames}, field) {
				refuse("tool %d sets mcpServer.%s, which has no Orka equivalent; give the Orka Tool its own policy", i, field)
			}
		}
		serverName := kagentString(server, cliNameKey)
		names, _ := server[kagentFieldToolNames].([]any)
		if len(names) == 0 {
			refuse("tool server %q binds every tool; list them so each can be mapped", serverName)
		}
		for _, rawName := range names {
			toolName, _ := rawName.(string)
			target, ok := options.Tools[serverName+"/"+toolName]
			if !ok {
				refuse("tool %s/%s has no Orka Tool; pass --tool %s/%s=<Orka Tool>", serverName, toolName, serverName, toolName)
				continue
			}
			refs = append(refs, map[string]any{cliNameKey: target})
		}
	}
	return refs, problems
}
