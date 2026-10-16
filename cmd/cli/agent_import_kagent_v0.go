/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
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
// whenever it changes.
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
	kagentToolTypeMCPServer    = "McpServer"
	kagentProviderOpenAIKind   = "OpenAI"
)

// kagentConfigHashPattern matches the decimal uint64 kagent writes.
var kagentConfigHashPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// kagentV0WrapRefusals lists why the wrapper cannot run a v0 Agent at all,
// in either mode.
func kagentV0WrapRefusals(agent map[string]any) []string {
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
// what keeps a governed Agent behind the relay is its pinned configuration,
// with every endpoint on the relay and nothing that runs code or calls out
// on its own. Anything the import cannot see keeps the Agent observed.
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
	origins := map[string]bool{}
	modelProblems, modelOrigin := kagentV0ModelProblems(agent, related, namespace)
	reasons = append(reasons, modelProblems...)
	if modelOrigin != "" {
		origins[modelOrigin] = true
	}
	for i, tool := range kagentV0Tools(agent) {
		toolProblems, origin := kagentV0ToolProblems(tool, i, related, namespace)
		reasons = append(reasons, toolProblems...)
		if origin != "" {
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
// exactly the one the input describes: kagent has observed the Agent, it is
// Ready, and the Deployment has finished rolling out to that hash.
func kagentV0Pin(agent, deployment map[string]any, name string) (string, []string) {
	var problems []string
	if generation := kagentInt(agent, kagentFieldMetadata, "generation"); generation == 0 || kagentInt(agent, "status", "observedGeneration") != generation {
		problems = append(problems, "kagent has not observed the Agent's current generation")
	}
	if !kagentConditionTrue(agent, "Ready") {
		problems = append(problems, "the Agent is not Ready")
	}
	if deployment == nil {
		return "", append(problems, fmt.Sprintf("deployment %q is not in the input", name))
	}
	replicas := int64(1) // the Deployment default
	if value, ok := kagentMap(deployment, kagentFieldSpec)["replicas"].(float64); ok {
		replicas = int64(value)
	}
	generation := kagentInt(deployment, kagentFieldMetadata, "generation")
	switch {
	case generation == 0 || kagentInt(deployment, "status", "observedGeneration") != generation:
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

func kagentConditionTrue(obj map[string]any, conditionType string) bool {
	conditions, _ := kagentMap(obj, "status")["conditions"].([]any)
	return slices.ContainsFunc(conditions, func(raw any) bool {
		condition, _ := raw.(map[string]any)
		return condition["type"] == conditionType && condition["status"] == "True"
	})
}

// kagentV0BehaviorProblems lists the Agent settings that run code, call out,
// or change the runtime outside the relay, or that the input cannot show.
func kagentV0BehaviorProblems(agent map[string]any) []string {
	var problems []string
	declarative := kagentMap(agent, kagentFieldSpec, kagentFieldDeclarative)
	if _, set := kagentMap(agent, kagentFieldSpec)["skills"]; set {
		problems = append(problems, "the Agent loads skills, which run inside kagent")
	}
	for field, reason := range map[string]string{
		"executeCodeBlocks": "the Agent executes code blocks inside kagent",
		"shareTools":        "the Agent has kagent's share-link tools",
	} {
		if kagentBool(declarative, field) {
			problems = append(problems, reason)
		}
	}
	for field, reason := range map[string]string{
		"memory":            "the Agent enables memory, which calls its own model",
		"context":           "the Agent compacts its context, which can call its own model",
		"systemMessageFrom": "the Agent reads its system message from an object the input does not show",
		"promptTemplate":    "the Agent templates its system message from objects the input does not show",
	} {
		if _, set := declarative[field]; set {
			problems = append(problems, reason)
		}
	}
	// Environment can replace the forwarded bearer (STS) or reroute tools,
	// and an image registry or extra container runs code the hash does not cover.
	for _, field := range []string{"env", "envFrom", "volumes", "volumeMounts", "extraContainers", "imageRegistry"} {
		if _, set := kagentMap(declarative, "deployment")[field]; set {
			problems = append(problems, "the Agent's deployment sets "+field)
		}
	}
	slices.Sort(problems)
	return problems
}

func kagentV0ModelProblems(agent map[string]any, related kagentObjects, namespace string) ([]string, string) {
	name := kagentString(agent, kagentFieldSpec, kagentFieldDeclarative, "modelConfig")
	model := related.lookup("ModelConfig", namespace, name)
	switch {
	case name == "":
		return []string{"the Agent names no modelConfig"}, ""
	case model == nil:
		return []string{fmt.Sprintf("model config %q is not in the input", name)}, ""
	case kagentString(model, kagentFieldSpec, cliProviderKey) != kagentProviderOpenAIKind:
		return []string{fmt.Sprintf("model config %q does not use the OpenAI provider", name)}, ""
	case !kagentBool(model, kagentFieldSpec, "apiKeyPassthrough"):
		return []string{fmt.Sprintf("model config %q does not pass the caller's bearer through", name)}, ""
	case !slices.Contains([]string{"", "chatCompletions"}, kagentString(model, kagentFieldSpec, "openAI", "apiFormat")):
		return []string{fmt.Sprintf("model config %q uses the %s API; the relay serves only chat completions", name, kagentString(model, kagentFieldSpec, "openAI", "apiFormat"))}, ""
	case len(kagentMap(model, kagentFieldSpec, "defaultHeaders")) > 0:
		return []string{fmt.Sprintf("model config %q sends static headers", name)}, ""
	}
	origin, ok := relayOrigin(kagentString(model, kagentFieldSpec, "openAI", "baseUrl"), "/v1")
	if !ok {
		return []string{fmt.Sprintf("model config %q does not point at a relay's /v1", name)}, ""
	}
	return nil, origin
}

// kagentV0ToolProblems checks one tool binding of a governed v0 Agent and
// returns the origin its server points at.
func kagentV0ToolProblems(tool map[string]any, index int, related kagentObjects, namespace string) ([]string, string) {
	if toolType := kagentString(tool, "type"); toolType != kagentToolTypeMCPServer {
		return []string{fmt.Sprintf("tool %d is a %q tool, not an MCP server", index, toolType)}, ""
	}
	server := kagentMap(tool, kagentFieldMCPServer)
	name := kagentString(server, cliNameKey)
	var problems []string
	if headers, _ := tool["headersFrom"].([]any); len(headers) > 0 {
		problems = append(problems, fmt.Sprintf("tool server %q binding sends static headers", name))
	}
	allowed, _ := server["allowedHeaders"].([]any)
	if !slices.ContainsFunc(allowed, func(raw any) bool {
		header, _ := raw.(string)
		return strings.EqualFold(strings.TrimSpace(header), "Authorization")
	}) {
		problems = append(problems, fmt.Sprintf("tool server %q binding does not forward Authorization (allowedHeaders)", name))
	}
	if kind, group := kagentString(server, kagentFieldKind), kagentString(server, "apiGroup"); kind != "RemoteMCPServer" || (group != "" && group != "kagent.dev") {
		return append(problems, fmt.Sprintf("tool server %q is not a kagent RemoteMCPServer", name)), ""
	}
	serverNamespace := kagentString(server, cliNamespaceQuery)
	if serverNamespace == "" {
		serverNamespace = namespace
	}
	remote := related.lookup("RemoteMCPServer", serverNamespace, name)
	if remote == nil {
		return append(problems, fmt.Sprintf("tool server %q is not in the input", name)), ""
	}
	for _, reason := range append(relayRemoteMCPServerProblems(remote), staticHeaderProblems(remote)...) {
		problems = append(problems, fmt.Sprintf("tool server %q %s", name, reason))
	}
	origin, _ := relayOrigin(kagentString(remote, kagentFieldSpec, "url"), "/mcp")
	return problems, origin
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
	modelName := kagentString(declarative, "modelConfig")
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
		case "systemMessage", "systemMessageFrom", "modelConfig", kagentFieldTools, "stream", kagentFieldRuntime:
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
