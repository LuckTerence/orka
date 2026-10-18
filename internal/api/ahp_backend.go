/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

// The ACP session backend serves Orka's built-in agent runtimes over the Agent
// Host Protocol. An AHP session is an Orka Session in the user's namespace;
// each turn is an agent Task in that Session, run and governed like any other.
const (
	ahpBackendName = "orka"

	ahpRecordProject = "project"
	ahpRecordSession = "session"
	ahpRecordChat    = "chat"
	ahpRecordContent = "content"
	ahpRecordFile    = "file"

	ahpApprovalManual   = "manual"
	ahpApprovalAllowAll = "allow-all"

	ahpConfigApprovalMode     = "approvalMode"
	ahpConfigAvailableModes   = "availableApprovalModes"
	ahpConfigEffectiveMode    = "effectiveApprovalMode"
	ahpSchemaString           = "string"
	ahpChangesetKindSession   = "session"
	ahpGitCredentialSecretKey = defaultWorkspaceCredentialKey
	ahpToolFallbackName       = "tool"

	// ahpProjectRoot is where projects appear on this host. Nothing is cloned
	// there: each turn's Task clones the repository itself.
	ahpProjectRoot = "/workspace/src"

	ahpDefaultTurnTimeout = 2 * time.Hour
	ahpMaxPromptBytes     = 256 << 10
	ahpMaxProjects        = 200
)

// The Copilot dialect: the extension keys and commands the GitHub Copilot app
// and copilotd use on the wire for project management.
const (
	ahpCopilotProjectManagementMeta = "copilot.projectManagement"
	ahpCopilotConfigKey             = "copilot"
	ahpExtensionListProjects        = "extensions/listProjects"
	ahpExtensionCloneProject        = "extensions/cloneProject"
	ahpExtensionGetPlan             = "extensions/getPlan"
)

var ahpBuiltInRuntimes = []struct {
	runtime     corev1alpha1.AgentRuntimeType
	displayName string
}{
	{corev1alpha1.AgentRuntimeCopilot, "Copilot CLI on Orka"},
	{corev1alpha1.AgentRuntimeClaude, "Claude Code on Orka"},
	{corev1alpha1.AgentRuntimeCodex, "Codex on Orka"},
	{corev1alpha1.AgentRuntimeOpencode, "OpenCode on Orka"},
}

// AHPBackendConfig configures the ACP session backend.
type AHPBackendConfig struct {
	// GitCredentialSecret names a Secret in each user's namespace whose "token"
	// key clones and publishes their repositories. When the Secret is absent,
	// sessions clone public repositories read-only.
	GitCredentialSecret string
	// WorkspaceClass selects an ExecutionWorkspaceClass for session-scoped
	// workspaces, so a session's files stay warm between turns. Empty uses
	// the default path: every write turn publishes, and the next starts from it.
	WorkspaceClass string
	// TurnTimeout bounds each turn's Task.
	TurnTimeout time.Duration
}

// ahpApprovalDecider answers a held permission request as its requester.
type ahpApprovalDecider func(ctx context.Context, ui *UserInfo, namespace, task, approvalID string, approve bool) error

type ahpBackend struct {
	h     *Handlers
	store store.AHPStore
	live  *liveevents.Broker
	cfg   AHPBackendConfig
	now   func() time.Time

	// applyApprovalMode asks a Task to hold policy-allowed permission
	// requests for its requester; decideApproval answers one.
	applyApprovalMode func(task *corev1alpha1.Task, manual bool)
	decideApproval    ahpApprovalDecider
	// emitterFor delivers actions for a user with no connection, to settle
	// turns a previous leader left running.
	emitterFor func(ahphost.Principal) ahphost.Emitter

	mu    sync.Mutex
	turns map[string]*ahpTurn
	// reserved holds a session's turn slot from the moment a message is
	// admitted until its turn starts, so a second message cannot slip in.
	reserved  map[string]time.Time
	recovered map[string]bool
}

// ahpReservationTTL frees a slot whose message the host never applied.
const ahpReservationTTL = 30 * time.Second

// reserveTurn claims a session's turn slot; false means a turn is running or starting.
func (b *ahpBackend) reserveTurn(userKey, session string) bool {
	key := ahpTurnKey(userKey, session)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turns[key] != nil {
		return false
	}
	if at, ok := b.reserved[key]; ok && b.now().Sub(at) < ahpReservationTTL {
		return false
	}
	b.reserved[key] = b.now()
	return true
}

func (b *ahpBackend) releaseTurn(userKey, session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.reserved, ahpTurnKey(userKey, session))
}

func newAHPBackend(h *Handlers, s store.AHPStore, live *liveevents.Broker, cfg AHPBackendConfig) *ahpBackend {
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = ahpDefaultTurnTimeout
	}
	return &ahpBackend{
		h: h, store: s, live: live, cfg: cfg, now: time.Now,
		applyApprovalMode: ahpApplyApprovalMode, decideApproval: h.DecideTaskApprovalAs,
		turns: map[string]*ahpTurn{}, reserved: map[string]time.Time{}, recovered: map[string]bool{},
	}
}

// ahpApplyApprovalMode makes a manual-approval turn hold every permission
// request Orka's policy would allow for the person to decide. Allow all
// leaves the decision to policy.
func ahpApplyApprovalMode(task *corev1alpha1.Task, manual bool) {
	if !manual {
		return
	}
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	task.Spec.AgentRuntime.PermissionDecisions = corev1alpha1.PermissionDecisionsRequester
}

func (b *ahpBackend) Name() string { return ahpBackendName }

// ahpProject is a repository a user added to the host.
type ahpProject struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Owner         string    `json:"owner"`
	URL           string    `json:"url"`
	Path          string    `json:"path"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// wire is a project as the Copilot dialect describes it.
func (p ahpProject) wire() map[string]any {
	project := map[string]any{
		"id": p.ID, apiFieldName: p.Name, "path": p.Path, "git": true, "origin": "cloned",
		"remoteUrl": p.URL, "status": "ready",
	}
	if p.DefaultBranch != "" {
		project["defaultBranch"] = p.DefaultBranch
	}
	return project
}

// ahpSession is what the backend keeps about one AHP session.
type ahpSession struct {
	Session      string `json:"session"`
	Chat         string `json:"chat"`
	Changeset    string `json:"changeset"`
	Namespace    string `json:"namespace"`
	OrkaSession  string `json:"orkaSession"`
	ProjectID    string `json:"projectId"`
	RepoURL      string `json:"repoUrl"`
	BaseBranch   string `json:"baseBranch,omitempty"`
	Runtime      string `json:"runtime"`
	Agent        string `json:"agent,omitempty"`
	Model        string `json:"model,omitempty"`
	ApprovalMode string `json:"approvalMode"`
	Turns        int    `json:"turns"`
	// The running turn, if any. It survives a controller restart, so the
	// turn can still be settled from its Task.
	ActiveTurn      string `json:"activeTurn,omitempty"`
	ActiveTask      string `json:"activeTask,omitempty"`
	ActiveStartedAt string `json:"activeStartedAt,omitempty"`
}

func ahpPrincipalUserInfo(p ahphost.Principal) *UserInfo {
	ui := &UserInfo{AuthType: AuthTypeOIDC, Issuer: p.Issuer, Subject: p.Subject, Username: p.Username, Namespace: p.Namespace}
	if p.Constraints.Delegated {
		ui.AuthType = AuthTypeContextToken
		ui.Roles = append([]string{}, p.Constraints.Scopes...)
		ui.ContextToken = &ContextToken{
			Issuer: p.Issuer, Subject: p.Subject, Scopes: append([]string{}, p.Constraints.Scopes...),
			TransactionContext: cloneMap(p.Constraints.TransactionContext),
		}
	}
	return ui
}

func ahpHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func ahpRandomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (b *ahpBackend) getRecord(ctx context.Context, userKey, kind, key string, out any) error {
	record, err := b.store.GetAHPRecord(ctx, userKey, kind, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(record.Value, out)
}

func (b *ahpBackend) putRecord(ctx context.Context, userKey, kind, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.store.PutAHPRecord(ctx, store.AHPRecord{UserKey: userKey, Kind: kind, Key: key, Value: raw, UpdatedAt: b.now().UTC()})
}

// ahpAgentChoice is an Orka Agent a user may run as one AHP model.
type ahpAgentChoice struct {
	Agent   string
	Runtime corev1alpha1.AgentRuntimeType
	Model   string
}

// agentChoices lists the built-in-runtime Agents in the user's namespace that
// the user's token allows, one per runtime and model.
func (b *ahpBackend) agentChoices(ctx context.Context, principal ahphost.Principal) ([]ahpAgentChoice, error) {
	var agents corev1alpha1.AgentList
	if err := b.h.client.List(ctx, &agents, client.InNamespace(principal.Namespace)); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	sort.Slice(agents.Items, func(i, j int) bool { return agents.Items[i].Name < agents.Items[j].Name })
	ui := ahpPrincipalUserInfo(principal)
	seen := map[string]bool{}
	var choices []ahpAgentChoice
	for i := range agents.Items {
		agent := &agents.Items[i]
		if !agent.DeletionTimestamp.IsZero() || agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef != nil ||
			agent.Spec.Model == nil || strings.TrimSpace(agent.Spec.Model.Name) == "" {
			continue
		}
		runtime := agent.Spec.Runtime.Type
		if !slices.ContainsFunc(ahpBuiltInRuntimes, func(r struct {
			runtime     corev1alpha1.AgentRuntimeType
			displayName string
		}) bool {
			return r.runtime == runtime
		}) {
			continue
		}
		key := string(runtime) + "\x00" + agent.Spec.Model.Name
		if seen[key] {
			continue
		}
		// Offer only Agents the user's token would let them run.
		probe := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}}}
		probe.Namespace = principal.Namespace
		if ui.ContextToken != nil {
			if err := authorizeContextTokenTaskCreateObject(ctx, b.h.client, ui.ContextToken, b.h.contextTokenAuthorization, "ahpListAgents", probe); err != nil {
				continue
			}
		}
		seen[key] = true
		choices = append(choices, ahpAgentChoice{Agent: agent.Name, Runtime: runtime, Model: agent.Spec.Model.Name})
	}
	return choices, nil
}

func ahpModelConfigSchema() *ahptypes.ConfigSchema {
	description := "Orka runs the model its Agent pins; the effort is passed to runtimes that take one."
	enum := []json.RawMessage{json.RawMessage(`"low"`), json.RawMessage(`"medium"`), json.RawMessage(`"high"`)}
	return &ahptypes.ConfigSchema{Type: "object", Properties: map[string]ahptypes.ConfigPropertySchema{
		"reasoningEffort": {Type: ahpSchemaString, Title: "Reasoning effort", Description: &description, Enum: enum},
	}}
}

// Agents lists one AHP agent per built-in runtime the user has Agents for.
func (b *ahpBackend) Agents(ctx context.Context, principal ahphost.Principal) ([]ahptypes.AgentInfo, error) {
	b.recoverTurns(ctx, principal)
	choices, err := b.agentChoices(ctx, principal)
	if err != nil {
		return nil, err
	}
	var agents []ahptypes.AgentInfo
	for _, runtime := range ahpBuiltInRuntimes {
		var models []ahptypes.SessionModelInfo
		for _, choice := range choices {
			if choice.Runtime != runtime.runtime {
				continue
			}
			models = append(models, ahptypes.SessionModelInfo{
				Id: choice.Model, Provider: string(runtime.runtime), Name: choice.Model, ConfigSchema: ahpModelConfigSchema(),
			})
		}
		if len(models) == 0 {
			continue
		}
		agents = append(agents, ahptypes.AgentInfo{
			Provider: string(runtime.runtime), DisplayName: runtime.displayName, Models: models,
			Description: "Runs " + string(runtime.runtime) + " as an Orka Task in your namespace, with Orka's policy and credentials.",
		})
	}
	return agents, nil
}

func (b *ahpBackend) projects(ctx context.Context, userKey string) ([]ahpProject, error) {
	records, err := b.store.ListAHPRecords(ctx, userKey, ahpRecordProject)
	if err != nil {
		return nil, err
	}
	projects := make([]ahpProject, 0, len(records))
	for _, record := range records {
		var project ahpProject
		if err := json.Unmarshal(record.Value, &project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].CreatedAt.Before(projects[j].CreatedAt) })
	return projects, nil
}

func ahpProjectsWire(projects []ahpProject) []map[string]any {
	wire := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		wire = append(wire, project.wire())
	}
	return wire
}

// RootExtensions advertises project management and lists the user's projects.
func (b *ahpBackend) RootExtensions(ctx context.Context, principal ahphost.Principal) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return nil, nil, err
	}
	config, err := ahpRootConfig(projects)
	if err != nil {
		return nil, nil, err
	}
	return map[string]json.RawMessage{ahpCopilotProjectManagementMeta: json.RawMessage(`{"available":true}`)}, config, nil
}

func ahpRootConfig(projects []ahpProject) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{"hostName": "orka", "projects": ahpProjectsWire(projects)})
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{ahpCopilotConfigKey: raw}, nil
}

// HandleExtension serves the project commands of the Copilot dialect.
func (b *ahpBackend) HandleExtension(ctx context.Context, principal ahphost.Principal, method string, params json.RawMessage, emit ahphost.Emitter) (any, error) {
	switch method {
	case ahpExtensionListProjects:
		projects, err := b.projects(ctx, principal.UserKey)
		if err != nil {
			return nil, err
		}
		return map[string]any{"projects": ahpProjectsWire(projects)}, nil
	case ahpExtensionCloneProject:
		var req struct {
			URL string `json:"url"`
		}
		if len(params) == 0 || json.Unmarshal(params, &req) != nil {
			return nil, &ahphost.Rejection{Reason: "a repository url is required"}
		}
		project, err := b.addProject(ctx, principal, req.URL)
		if err != nil {
			return nil, err
		}
		projects, err := b.projects(ctx, principal.UserKey)
		if err != nil {
			return nil, err
		}
		config, err := ahpRootConfig(projects)
		if err != nil {
			return nil, err
		}
		if err := emit.Emit(ctx, ahptypes.RootResourceURI, ahptypes.StateAction{Value: &ahptypes.RootConfigChangedAction{
			Type: ahptypes.ActionTypeRootConfigChanged, Config: config,
		}}); err != nil {
			return nil, err
		}
		return map[string]any{"project": project.wire()}, nil
	case ahpExtensionGetPlan:
		return map[string]any{"plan": map[string]any{"exists": false, "content": nil}, "todos": []any{}, "dependencies": []any{}}, nil
	default:
		return nil, ahphost.ErrUnknownExtension
	}
}

// addProject records a repository. Orka never clones it here: each turn's
// Task clones it in its own workspace, with Orka's credentials.
func (b *ahpBackend) addProject(ctx context.Context, principal ahphost.Principal, raw string) (ahpProject, error) {
	repo, owner, name, err := ahpCanonicalRepository(raw)
	if err != nil {
		return ahpProject{}, &ahphost.Rejection{Reason: err.Error()}
	}
	id := "project-" + ahpHash(repo)[:16]
	var existing ahpProject
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordProject, id, &existing); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return ahpProject{}, err
	}
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return ahpProject{}, err
	}
	if len(projects) >= ahpMaxProjects {
		return ahpProject{}, &ahphost.Rejection{Reason: fmt.Sprintf("at most %d projects are allowed", ahpMaxProjects)}
	}
	project := ahpProject{
		ID: id, Name: name, Owner: owner, URL: repo, Path: path.Join(ahpProjectRoot, owner, name), CreatedAt: b.now().UTC(),
	}
	return project, b.putRecord(ctx, principal.UserKey, ahpRecordProject, id, project)
}

// ahpCanonicalRepository accepts an https repository URL without credentials,
// query or fragment and returns it without a trailing .git.
func ahpCanonicalRepository(raw string) (repo, owner, name string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != connectorSchemeHTTPS || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("the repository must be an https URL without credentials, query or fragment")
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", "", "", errors.New("the repository URL must name an owner and a repository")
	}
	owner, name = segments[0], strings.TrimSuffix(segments[1], ".git")
	for _, part := range []string{owner, name} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\ `) {
			return "", "", "", errors.New("the repository URL is not valid")
		}
	}
	return fmt.Sprintf("https://%s/%s/%s", strings.ToLower(parsed.Host), owner, name), owner, name, nil
}

func ahpDirectoryURI(dir string) string { return "file://" + dir }

// projectForDirectory finds the project a session's working directory names.
func (b *ahpBackend) projectForDirectory(ctx context.Context, userKey string, dirs []string) (ahpProject, error) {
	if len(dirs) == 0 {
		return ahpProject{}, &ahphost.Rejection{Reason: "pick a project: sessions on Orka run in a repository"}
	}
	projects, err := b.projects(ctx, userKey)
	if err != nil {
		return ahpProject{}, err
	}
	for _, project := range projects {
		if dirs[0] == ahpDirectoryURI(project.Path) {
			return project, nil
		}
	}
	return ahpProject{}, &ahphost.Rejection{Reason: "the working directory is not a project on this host; add the repository first"}
}

func ahpSessionConfig(mode string) *ahptypes.SessionConfigState {
	modes := []json.RawMessage{json.RawMessage(`"manual"`), json.RawMessage(`"allow-all"`)}
	description := "Manual asks you before a tool runs that Orka's policy allows. Allow all lets Orka's policy decide."
	readOnly := true
	mutable := true
	notMutable := false
	values, _ := json.Marshal(mode)
	available, _ := json.Marshal([]string{ahpApprovalManual, ahpApprovalAllowAll})
	return &ahptypes.SessionConfigState{
		Schema: ahptypes.SessionConfigSchema{Type: "object", Properties: map[string]ahptypes.SessionConfigPropertySchema{
			ahpConfigApprovalMode: {Type: ahpSchemaString, Title: "Approvals", Description: &description, Enum: modes,
				EnumLabels: []string{"Manual approvals", "Allow all"}, SessionMutable: &mutable},
			ahpConfigAvailableModes: {Type: "array", Title: "Available approvals", ReadOnly: &readOnly, SessionMutable: &notMutable,
				Items: &ahptypes.ConfigPropertySchema{Type: ahpSchemaString, Title: "Approval mode", Enum: modes}},
			ahpConfigEffectiveMode: {Type: ahpSchemaString, Title: "Approvals in force", ReadOnly: &readOnly, SessionMutable: &notMutable, Enum: modes},
		}},
		Values: map[string]json.RawMessage{ahpConfigApprovalMode: values, ahpConfigAvailableModes: available, ahpConfigEffectiveMode: values},
	}
}

func ahpApprovalModeFrom(config map[string]json.RawMessage) (string, error) {
	raw, ok := config[ahpConfigApprovalMode]
	if !ok {
		return ahpApprovalManual, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return "", errors.New("approvalMode must be a string")
	}
	switch mode {
	case ahpApprovalManual, ahpApprovalAllowAll:
		return mode, nil
	case "assisted":
		// Orka has no model-reviewed approvals; ask the person instead.
		return ahpApprovalManual, nil
	default:
		return "", fmt.Errorf("approvalMode %q is not supported", mode)
	}
}

// CreateSession starts a session in one of the user's projects. Nothing runs
// until the first message: that turn's Task creates the Orka Session.
func (b *ahpBackend) CreateSession(ctx context.Context, principal ahphost.Principal, req ahphost.CreateSessionRequest, _ ahphost.Emitter) (*ahphost.CreatedSession, error) {
	project, err := b.projectForDirectory(ctx, principal.UserKey, req.WorkingDirectories)
	if err != nil {
		return nil, err
	}
	mode, err := ahpApprovalModeFrom(req.Config)
	if err != nil {
		return nil, &ahphost.Rejection{Reason: err.Error()}
	}
	baseBranch := ""
	if raw, ok := req.Config["baseBranch"]; ok {
		_ = json.Unmarshal(raw, &baseBranch)
	}
	if baseBranch == "" {
		baseBranch = project.DefaultBranch
	}
	chatID := ahpRandomHex(16)
	chat := "ahp-chat:/" + chatID
	changeset := "ahp-changeset:/" + chatID + "/session"
	record := ahpSession{
		Session: req.Session, Chat: chat, Changeset: changeset, Namespace: principal.Namespace,
		OrkaSession: "ahp-" + ahpHash(principal.UserKey, req.Session)[:24], ProjectID: project.ID, RepoURL: project.URL,
		BaseBranch: baseBranch, Runtime: req.Provider, ApprovalMode: mode,
	}
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordSession, req.Session, record); err != nil {
		return nil, err
	}
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordChat, chat, req.Session); err != nil {
		return nil, err
	}
	now := b.now().UTC().Format(time.RFC3339Nano)
	title := project.Owner + "/" + project.Name
	description := "Cumulative file edits the agent has made in this session."
	user := ahptypes.ChatOrigin{Value: &ahptypes.ChatUserOrigin{Kind: ahptypes.ChatOriginKindUser}}
	return &ahphost.CreatedSession{
		State: ahptypes.SessionState{
			Title: title, Status: ahptypes.SessionStatusIdle,
			Project:            &ahptypes.ProjectInfo{Uri: project.URL, DisplayName: project.Owner + "/" + project.Name},
			WorkingDirectories: []string{ahpDirectoryURI(project.Path)},
			ActiveClients:      []ahptypes.SessionActiveClient{},
			Chats:              []ahptypes.ChatSummary{{Resource: chat, Title: title, Status: ahptypes.SessionStatusIdle, ModifiedAt: now, Origin: &user}},
			DefaultChat:        &chat,
			Config:             ahpSessionConfig(mode),
			Changesets: []ahptypes.Changeset{{
				Label: "Session Changes", ChangeKind: ahpChangesetKindSession, UriTemplate: changeset, Description: &description,
			}},
		},
		Chats: map[string]ahptypes.ChatState{chat: {
			Resource: chat, Title: title, Status: ahptypes.SessionStatusIdle, ModifiedAt: now, Origin: &user, Turns: []ahptypes.Turn{},
		}},
		Changesets: map[string]ahptypes.ChangesetState{changeset: {Status: ahptypes.ChangesetStatusReady, Files: []ahptypes.ChangesetFile{}}},
	}, nil
}

// DisposeSession cancels a running turn and forgets the session. The Orka
// Session and its Tasks keep their history.
func (b *ahpBackend) DisposeSession(ctx context.Context, principal ahphost.Principal, session string) error {
	var record ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, session, &record); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if turn := b.activeTurn(principal.UserKey, session); turn != nil {
		turn.stop()
	}
	if record.ActiveTask != "" {
		b.cancelTask(ctx, principal, record.Namespace, record.ActiveTask)
	}
	hash := ahpHash(principal.UserKey, session)
	for _, kind := range []string{ahpRecordContent, ahpRecordFile} {
		if err := b.store.DeleteAHPRecords(ctx, principal.UserKey, kind, hash+"/"); err != nil {
			return err
		}
	}
	if err := b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordChat, record.Chat); err != nil {
		return err
	}
	return b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordSession, session)
}

func (b *ahpBackend) cancelTask(ctx context.Context, principal ahphost.Principal, namespace, name string) {
	task := &corev1alpha1.Task{}
	if err := b.h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		return
	}
	_, _ = b.h.requestTaskCancellation(ctx, ahpPrincipalUserInfo(principal), task)
}

func (b *ahpBackend) sessionForChannel(ctx context.Context, userKey, channel string) (ahpSession, error) {
	session := channel
	if strings.HasPrefix(channel, "ahp-chat:") {
		if err := b.getRecord(ctx, userKey, ahpRecordChat, channel, &session); err != nil {
			return ahpSession{}, err
		}
	}
	var record ahpSession
	err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record)
	return record, err
}

// resolveModel maps a turn's model to the Agent that runs it. A session keeps
// the Agent of its first turn: an Orka Session is bound to one runtime profile.
func (b *ahpBackend) resolveModel(ctx context.Context, principal ahphost.Principal, record ahpSession, model *ahptypes.ModelSelection) (ahpAgentChoice, error) {
	choices, err := b.agentChoices(ctx, principal)
	if err != nil {
		return ahpAgentChoice{}, err
	}
	wanted := record.Model
	if model != nil && model.Id != "" {
		wanted = model.Id
	}
	if record.Agent != "" && wanted != record.Model {
		return ahpAgentChoice{}, &ahphost.Rejection{Reason: fmt.Sprintf("this session runs %s; start a new session to use another model", record.Model)}
	}
	for _, choice := range choices {
		if string(choice.Runtime) != record.Runtime {
			continue
		}
		if record.Agent != "" && choice.Agent == record.Agent {
			return choice, nil
		}
		if record.Agent == "" && (wanted == "" || choice.Model == wanted) {
			return choice, nil
		}
	}
	return ahpAgentChoice{}, &ahphost.Rejection{Reason: "the selected model is not available to you"}
}

// AuthorizeClientAction admits the client actions this backend acts on and
// the harmless view actions; anything else is rejected.
func (b *ahpBackend) AuthorizeClientAction(ctx context.Context, principal ahphost.Principal, channel string, action ahptypes.StateAction) error {
	record, err := b.sessionForChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return &ahphost.Rejection{Reason: "unknown session"}
	}
	switch a := action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		if channel != record.Chat {
			return &ahphost.Rejection{Reason: "messages go to the session's chat"}
		}
		if strings.TrimSpace(a.Message.Text) == "" || len(a.Message.Text) > ahpMaxPromptBytes || a.TurnId == "" {
			return &ahphost.Rejection{Reason: "a message needs text and a turn id"}
		}
		if _, err := b.resolveModel(ctx, principal, record, a.Message.Model); err != nil {
			return err
		}
		if record.ActiveTurn != "" || !b.reserveTurn(principal.UserKey, record.Session) {
			return &ahphost.Rejection{Reason: "a turn is already running"}
		}
		return nil
	case *ahptypes.ChatTurnCancelledAction:
		if record.ActiveTurn == "" || a.TurnId != record.ActiveTurn {
			return &ahphost.Rejection{Reason: "the turn is not running"}
		}
		return b.mayOperateTasks(principal)
	case *ahptypes.ChatToolCallConfirmedAction:
		if err := b.mayOperateTasks(principal); err != nil {
			return err
		}
		turn := b.activeTurn(principal.UserKey, record.Session)
		if turn == nil || a.TurnId != turn.id || !turn.awaitingConfirmation(a.ToolCallId) {
			return &ahphost.Rejection{Reason: "the tool call is not waiting for a decision"}
		}
		if b.decideApproval == nil {
			return &ahphost.Rejection{Reason: "approvals are not available on this host"}
		}
		return nil
	case *ahptypes.SessionConfigChangedAction:
		for key := range a.Config {
			switch key {
			case ahpConfigApprovalMode:
				if _, err := ahpApprovalModeFrom(a.Config); err != nil {
					return &ahphost.Rejection{Reason: err.Error()}
				}
			case ahpConfigAvailableModes, ahpConfigEffectiveMode:
				return &ahphost.Rejection{Reason: key + " is read-only"}
			}
		}
		return nil
	case *ahptypes.SessionTitleChangedAction, *ahptypes.SessionActiveClientSetAction, *ahptypes.SessionActiveClientRemovedAction,
		*ahptypes.SessionIsReadChangedAction, *ahptypes.SessionIsArchivedChangedAction,
		*ahptypes.ChatDraftChangedAction, *ahptypes.ChatIsReadChangedAction, *ahptypes.ChatIsArchivedChangedAction:
		// View state only. Client-provided tools in an active client are never offered to agents.
		return nil
	default:
		return &ahphost.Rejection{Reason: "not supported on this host"}
	}
}

// mayOperateTasks refuses a delegated token without the scopes that update
// Tasks, before its decision or cancel is shown as accepted. The decision and
// cancel paths check the same identity again.
func (b *ahpBackend) mayOperateTasks(principal ahphost.Principal) error {
	cfg := b.h.contextTokenAuthorization
	if principal.Constraints.Delegated && cfg.Enabled() && !hasAnyScope(principal.Constraints.Scopes, cfg.TaskUpdateScopes) {
		return &ahphost.Rejection{Reason: "this connection's token may not operate Tasks"}
	}
	return nil
}

// HandleClientAction runs what an admitted client action asks for.
func (b *ahpBackend) HandleClientAction(ctx context.Context, principal ahphost.Principal, channel string, action ahptypes.StateAction, emit ahphost.Emitter) {
	record, err := b.sessionForChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return
	}
	switch a := action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		b.startTurn(ctx, principal, record, a, emit)
	case *ahptypes.ChatTurnCancelledAction:
		if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil {
			turn.markCancelledByClient()
		}
		if record.ActiveTask != "" {
			b.cancelTask(ctx, principal, record.Namespace, record.ActiveTask)
		}
	case *ahptypes.ChatToolCallConfirmedAction:
		if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil {
			turn.confirm(ctx, principal, a.ToolCallId, a.Approved)
		}
	case *ahptypes.SessionConfigChangedAction:
		if _, ok := a.Config[ahpConfigApprovalMode]; !ok {
			return
		}
		mode, err := ahpApprovalModeFrom(a.Config)
		if err != nil {
			return
		}
		b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) { s.ApprovalMode = mode })
		effective, _ := json.Marshal(mode)
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionConfigChangedAction{
			Type: ahptypes.ActionTypeSessionConfigChanged, Config: map[string]json.RawMessage{ahpConfigEffectiveMode: effective},
		}})
	}
}

func (b *ahpBackend) updateSession(ctx context.Context, userKey, session string, mutate func(*ahpSession)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var record ahpSession
	if err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record); err != nil {
		return
	}
	mutate(&record)
	_ = b.putRecord(ctx, userKey, ahpRecordSession, session, record)
}

// ahpFileContent is what a changeset serves for one file of a session.
type ahpFileContent struct {
	Path   string `json:"path"`
	After  string `json:"after"`
	Before string `json:"before,omitempty"`
}

const ahpContentScheme = "ahp-content:/"

// ReadResource serves the before and after contents of the files in a
// session's changeset, as the agent reported them. Nothing is read from a
// workspace or from this server's disk. On the root channel the session is
// found from the resource: a content URI names it, and a file is served from
// the user's session that last changed it.
func (b *ahpBackend) ReadResource(ctx context.Context, principal ahphost.Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error) {
	var sessions []string
	if session == "" {
		records, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
		if err != nil {
			return ahptypes.ResourceReadResult{}, err
		}
		for _, record := range records {
			sessions = append(sessions, record.Key)
		}
	} else {
		var record ahpSession
		if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, session, &record); err != nil || channel != record.Changeset {
			return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
		}
		sessions = []string{session}
	}
	var found store.AHPRecord
	ok := false
	for _, candidate := range sessions {
		hash := ahpHash(principal.UserKey, candidate)
		nonce := ""
		var updated time.Time
		switch {
		case strings.HasPrefix(uri, ahpContentScheme+hash[:24]+"/"):
			nonce = strings.TrimPrefix(uri, ahpContentScheme+hash[:24]+"/")
		case strings.HasPrefix(uri, "file:///"):
			fileRecord, err := b.store.GetAHPRecord(ctx, principal.UserKey, ahpRecordFile, hash+"/"+strings.TrimPrefix(uri, "file://"))
			if err != nil {
				continue
			}
			var file ahpFileContent
			if json.Unmarshal(fileRecord.Value, &file) != nil {
				continue
			}
			nonce, updated = file.After, fileRecord.UpdatedAt
		}
		if nonce == "" {
			continue
		}
		content, err := b.store.GetAHPRecord(ctx, principal.UserKey, ahpRecordContent, hash+"/"+nonce)
		if err != nil {
			continue
		}
		if !updated.IsZero() {
			content.UpdatedAt = updated
		}
		if !ok || content.UpdatedAt.After(found.UpdatedAt) {
			found, ok = content, true
		}
	}
	if !ok {
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	return ahptypes.ResourceReadResult{Data: string(found.Value), Encoding: ahptypes.ContentEncodingUtf8}, nil
}

// gitCredential returns the user's repository credential, if configured.
func (b *ahpBackend) gitCredential(ctx context.Context, namespace string) *corev1alpha1.WorkspaceCredentialReference {
	if b.cfg.GitCredentialSecret == "" {
		return nil
	}
	// Read Secrets uncached: the manager's cache must not start watching
	// Secrets for this lookup.
	var reader client.Reader = b.h.client
	if b.h.apiReader != nil {
		reader = b.h.apiReader
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: b.cfg.GitCredentialSecret}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "read the AHP git credential Secret", "namespace", namespace)
		}
		return nil
	}
	if len(secret.Data[ahpGitCredentialSecretKey]) == 0 {
		return nil
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: b.cfg.GitCredentialSecret, Key: ahpGitCredentialSecretKey}
}
