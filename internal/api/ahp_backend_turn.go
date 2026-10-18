/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

const (
	ahpTurnPollInterval = 2 * time.Second
	ahpTitleRunes       = 60
	ahpOptionApprove    = "approve"
	ahpOptionDeny       = "deny"
)

// ahpTurn follows one turn's Task and turns its events into chat actions.
type ahpTurn struct {
	b         *ahpBackend
	principal ahphost.Principal
	session   ahpSession
	id        string
	task      string
	startedAt time.Time
	emit      ahphost.Emitter
	ctx       context.Context
	cancel    context.CancelFunc

	mu              sync.Mutex
	clientCancelled bool
	tools           map[string]*ahpToolCall
	order           []string
	textPart        string
	thoughtPart     string
	streamedText    bool
}

type ahpToolCall struct {
	started, ready, completed bool
	title                     string
	// approval is the held permission request waiting for the person.
	approval string
}

func ahpTurnKey(userKey, session string) string { return userKey + "\x00" + session }

func (b *ahpBackend) activeTurn(userKey, session string) *ahpTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turns[ahpTurnKey(userKey, session)]
}

func ahpStatus(status ahptypes.SessionStatus) *ahptypes.SessionStatus { return new(status) }

func ahpNow(b *ahpBackend) string { return b.now().UTC().Format(time.RFC3339Nano) }

func ahpTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= ahpTitleRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:ahpTitleRunes-1]) + "…"
}

// startTurn creates the turn's Task as the person and follows it.
func (b *ahpBackend) startTurn(ctx context.Context, principal ahphost.Principal, record ahpSession, action *ahptypes.ChatTurnStartedAction, emit ahphost.Emitter) {
	started := b.now().UTC()
	// The slot reserved when the message was admitted passes to the running
	// turn, or is freed if the turn never starts.
	defer b.releaseTurn(principal.UserKey, record.Session)
	fail := func(message string) {
		b.emitTurnError(ctx, emit, record, action.TurnId, started, "TurnNotStarted", message)
	}
	choice, err := b.resolveModel(ctx, principal, record, action.Message.Model)
	if err != nil {
		if rejection, ok := errors.AsType[*ahphost.Rejection](err); ok {
			fail(rejection.Reason)
		} else {
			fail("the model could not be resolved")
		}
		return
	}
	number := record.Turns + 1
	name := fmt.Sprintf("%s-t%d", record.OrkaSession, number)
	task := b.turnTask(ctx, principal, record, choice, name, action.Message.Text, number == 1)
	ui := ahpPrincipalUserInfo(principal)

	// Subscribe before the Task exists so no event of the turn is missed.
	sub := b.live.Subscribe(record.Namespace, name)
	if err := authorizeAndStampToolTaskCreate(ctx, b.h.client, b.h.clientset, ui.ContextToken, b.h.contextTokenAuthorization, "ahpTurn", ui, task); err != nil {
		sub.Close()
		fail(ahpErrorMessage(err))
		return
	}
	if err := b.h.client.Create(ctx, task); err != nil {
		sub.Close()
		if invalid := invalidObjectError("task", err); invalid != nil {
			fail(ahpErrorMessage(invalid))
		} else {
			fail("the turn's Task could not be created")
			log.Error(err, "create AHP turn Task", "namespace", task.Namespace, "task", task.Name)
		}
		return
	}
	sealRequesterStamp(ctx, b.h.client, task)

	turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	turn := &ahpTurn{
		b: b, principal: principal, session: record, id: action.TurnId, task: name, startedAt: started, emit: emit,
		ctx: turnCtx, cancel: cancel, tools: map[string]*ahpToolCall{},
	}
	b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
		s.Agent, s.Model, s.Turns = choice.Agent, choice.Model, number
		s.ActiveTurn, s.ActiveTask, s.ActiveStartedAt = action.TurnId, name, started.Format(time.RFC3339Nano)
		turn.session = *s
	})
	b.mu.Lock()
	b.turns[ahpTurnKey(principal.UserKey, record.Session)] = turn
	b.mu.Unlock()

	now := ahpNow(b)
	_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: record.Chat,
		Changes: ahptypes.PartialChatSummary{Status: ahpStatus(ahptypes.SessionStatusInProgress), ModifiedAt: &now},
	}})
	if number == 1 {
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
			Type: ahptypes.ActionTypeSessionTitleChanged, Title: ahpTitle(action.Message.Text),
		}})
	}
	go turn.run(sub)
}

// turnTask builds one turn's Task. The Session carries the conversation; the
// repository comes from the project, cloned by Orka with Orka's credentials.
func (b *ahpBackend) turnTask(ctx context.Context, principal ahphost.Principal, record ahpSession, choice ahpAgentChoice, name, prompt string, first bool) *corev1alpha1.Task {
	workspace := &corev1alpha1.WorkspaceConfig{GitRepo: record.RepoURL, Branch: record.BaseBranch, Intent: corev1alpha1.WorkspaceIntentRead}
	if credential := b.gitCredential(ctx, principal.Namespace); credential != nil {
		workspace.Intent = corev1alpha1.WorkspaceIntentWrite
		workspace.ReadCredentialRef = credential
		publication := *credential
		workspace.PublicationCredentialRef = &publication
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: principal.Namespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: choice.Agent},
			Prompt:     prompt,
			SessionRef: &corev1alpha1.SessionReference{Name: record.OrkaSession, Create: first, Append: true},
			Workspace:  workspace,
			Timeout:    &metav1.Duration{Duration: b.cfg.TurnTimeout},
		},
	}
	if b.cfg.WorkspaceClass != "" {
		task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
			ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: b.cfg.WorkspaceClass},
			ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
		}}
	}
	b.applyApprovalMode(task, record.ApprovalMode != ahpApprovalAllowAll)
	return task
}

func ahpErrorMessage(err error) string {
	if rejection, ok := errors.AsType[*ahphost.Rejection](err); ok {
		return rejection.Reason
	}
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
		return fiberErr.Message
	}
	return "the turn could not start"
}

// emitTurnError ends a turn that never got a Task.
func (b *ahpBackend) emitTurnError(ctx context.Context, emit ahphost.Emitter, record ahpSession, turnID string, started time.Time, kind, message string) {
	_ = emit.Emit(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
		Type: ahptypes.ActionTypeChatError, TurnId: turnID, Duration: b.now().Sub(started).Milliseconds(),
		Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: kind, Message: message}},
	}})
	now := ahpNow(b)
	_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: record.Chat,
		Changes: ahptypes.PartialChatSummary{Status: ahpStatus(ahptypes.SessionStatusIdle), ModifiedAt: &now},
	}})
}

func (t *ahpTurn) stop() { t.cancel() }

func (t *ahpTurn) markCancelledByClient() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clientCancelled = true
}

func (t *ahpTurn) chat(actions ...ahptypes.StateAction) {
	_ = t.emit.Emit(t.ctx, t.session.Chat, actions...)
}

// run follows the turn's live events and its Task until the Task settles.
func (t *ahpTurn) run(sub *liveevents.Subscription) {
	defer sub.Close()
	defer t.b.forgetTurn(t)
	events := sub.C()
	ticker := time.NewTicker(ahpTurnPollInterval)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				// The subscription fell behind: the turn ends from the Task's
				// stored result instead of live updates.
				events = nil
				continue
			}
			t.handle(event)
		case <-ticker.C:
			task := &corev1alpha1.Task{}
			err := t.b.h.client.Get(t.ctx, types.NamespacedName{Namespace: t.session.Namespace, Name: t.task}, task)
			if apierrors.IsNotFound(err) {
				t.settleWithError("TaskDeleted", "the turn's Task was deleted")
				return
			}
			if err == nil && ahpTaskTerminal(task) {
				// Deliver anything still queued for this turn first.
				t.drain(events)
				t.settle(task)
				return
			}
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *ahpTurn) drain(events <-chan liveevents.Event) {
	for events != nil {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			t.handle(event)
		default:
			return
		}
	}
}

func ahpTaskTerminal(task *corev1alpha1.Task) bool {
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func (t *ahpTurn) handle(event liveevents.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if event.Approval != nil {
		t.onApproval(event.Approval)
		return
	}
	runtime := event.Runtime
	if runtime == nil {
		return
	}
	switch runtime.Type {
	case harnessv2.EventUpdate:
		update := runtime.Update
		if update == nil {
			return
		}
		switch {
		case update.AssistantMessage != nil:
			t.text(update.AssistantMessage.Text)
		case update.AssistantThought != nil:
			t.thought(update.AssistantThought.Text)
		case update.ToolCall != nil:
			t.tool(update.ToolCall)
		}
	case harnessv2.EventCompleted:
		if runtime.Completed == nil {
			return
		}
		if !t.streamedText {
			for _, block := range runtime.Completed.Result.Content {
				if block.Type == harnessv2.ContentBlockText {
					t.text(block.Text)
				}
			}
		}
		usage := runtime.Completed.Result.Usage
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			info := ahptypes.UsageInfo{}
			input, output := int64(usage.InputTokens), int64(usage.OutputTokens) //nolint:gosec // token counts fit in int64
			info.InputTokens, info.OutputTokens = &input, &output
			if model := runtime.Completed.Result.Model; model != "" {
				info.Model = &model
			}
			t.chat(ahptypes.StateAction{Value: &ahptypes.ChatUsageAction{Type: ahptypes.ActionTypeChatUsage, TurnId: t.id, Usage: info}})
		}
	}
}

// text appends assistant text, opening a markdown part when needed.
func (t *ahpTurn) text(content string) {
	if content == "" {
		return
	}
	t.thoughtPart = ""
	if t.textPart == "" {
		t.textPart = ahpRandomHex(8)
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
			Type: ahptypes.ActionTypeChatResponsePart, TurnId: t.id,
			Part: ahptypes.ResponsePart{Value: &ahptypes.MarkdownResponsePart{Kind: ahptypes.ResponsePartKindMarkdown, Id: t.textPart}},
		}})
	}
	t.streamedText = true
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatDeltaAction{Type: ahptypes.ActionTypeChatDelta, TurnId: t.id, PartId: t.textPart, Content: content}})
}

func (t *ahpTurn) thought(content string) {
	if content == "" {
		return
	}
	t.textPart = ""
	if t.thoughtPart == "" {
		t.thoughtPart = ahpRandomHex(8)
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
			Type: ahptypes.ActionTypeChatResponsePart, TurnId: t.id,
			Part: ahptypes.ResponsePart{Value: &ahptypes.ReasoningResponsePart{Kind: ahptypes.ResponsePartKindReasoning, Id: t.thoughtPart}},
		}})
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatReasoningAction{Type: ahptypes.ActionTypeChatReasoning, TurnId: t.id, PartId: t.thoughtPart, Content: content}})
}

func ahpToolName(update *harnessv2.ToolCallUpdate) string {
	for _, name := range []string{update.Name, update.Kind} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return ahpToolFallbackName
}

func (t *ahpTurn) startTool(id, name, title string) *ahpToolCall {
	call := t.tools[id]
	if call == nil {
		call = &ahpToolCall{}
		t.tools[id] = call
		t.order = append(t.order, id)
	}
	if call.started {
		return call
	}
	if title == "" {
		title = name
	}
	t.textPart, t.thoughtPart = "", ""
	call.started, call.title = true, title
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallStartAction{
		Type: ahptypes.ActionTypeChatToolCallStart, TurnId: t.id, ToolCallId: id, ToolName: name, DisplayName: title,
	}})
	return call
}

func (t *ahpTurn) readyTool(id string, call *ahpToolCall, title string, input json.RawMessage) {
	if call.ready {
		return
	}
	call.ready = true
	notNeeded := ahptypes.ToolCallConfirmationReasonNotNeeded
	ready := &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: t.id, ToolCallId: id,
		InvocationMessage: ahptypes.StringOrMarkdown{Plain: &title}, Confirmed: &notNeeded,
	}
	if len(input) > 0 {
		inline := string(input)
		ready.ToolInput = &ahptypes.ToolInput{Inline: &inline}
	}
	t.chat(ahptypes.StateAction{Value: ready})
}

// tool advances one tool call through start, ready and complete.
func (t *ahpTurn) tool(update *harnessv2.ToolCallUpdate) {
	title := update.Title
	if title == "" {
		title = ahpToolName(update)
	}
	call := t.startTool(update.ToolCallID, ahpToolName(update), update.Title)
	if len(update.Diffs) > 0 {
		t.recordDiffs(update.Diffs)
	}
	finished := update.Status == harnessv2.ToolCallStatusCompleted || update.Status == harnessv2.ToolCallStatusFailed
	if call.completed || (call.approval != "" && !finished) {
		// A held call moves on only after its decision.
		return
	}
	if call.approval != "" {
		// The call ended while its confirmation was open, for example when the
		// request expired: close the confirmation before the result.
		call.approval = ""
		reason := ahptypes.ToolCallConfirmationReasonUserAction
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: t.id, ToolCallId: update.ToolCallID,
			Approved: update.Status == harnessv2.ToolCallStatusCompleted, Confirmed: &reason,
		}})
	}
	switch update.Status {
	case harnessv2.ToolCallStatusInProgress:
		t.readyTool(update.ToolCallID, call, title, update.RawInput)
	case harnessv2.ToolCallStatusCompleted, harnessv2.ToolCallStatusFailed:
		t.readyTool(update.ToolCallID, call, title, update.RawInput)
		t.completeTool(update.ToolCallID, call, update.Status == harnessv2.ToolCallStatusCompleted, title, update.Content)
	}
}

func (t *ahpTurn) completeTool(id string, call *ahpToolCall, success bool, title string, content []harnessv2.ContentBlock) {
	if call.completed {
		return
	}
	call.completed = true
	result := ahptypes.ToolCallResult{Success: success, PastTenseMessage: ahptypes.StringOrMarkdown{Plain: &title}}
	for _, block := range content {
		if block.Type == harnessv2.ContentBlockText && block.Text != "" {
			result.Content = append(result.Content, ahptypes.ToolResultContent{Value: &ahptypes.ToolResultTextContent{
				Type: ahptypes.ToolResultContentTypeText, Text: block.Text,
			}})
		}
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallCompleteAction{
		Type: ahptypes.ActionTypeChatToolCallComplete, TurnId: t.id, ToolCallId: id, Result: result,
	}})
}

// onApproval shows a held permission request as a confirmation, and settles
// the confirmation when the request ends without the client's answer.
func (t *ahpTurn) onApproval(approval *liveevents.Approval) {
	id := approval.ToolCallID
	if id == "" {
		id = "approval-" + approval.ID
	}
	switch approval.State {
	case liveevents.ApprovalPending:
		name := approval.ToolName
		if name == "" {
			name = ahpToolFallbackName
		}
		call := t.startTool(id, name, approval.Title)
		// A running call may ask again; each hold reopens its confirmation.
		if call.completed || call.approval == approval.ID {
			return
		}
		call.ready, call.approval = true, approval.ID
		title := approval.Title
		if title == "" {
			title = call.title
		}
		t.askForConfirmation(id, title)
	default:
		call := t.tools[id]
		if call == nil || call.approval == "" {
			return
		}
		call.approval = ""
		approved := approval.State == liveevents.ApprovalResolved && ahpOptionAllows(approval)
		reason := ahptypes.ToolCallConfirmationReasonUserAction
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: t.id, ToolCallId: id, Approved: approved, Confirmed: &reason,
		}})
	}
}

// askForConfirmation shows a tool call as waiting for the person's decision.
func (t *ahpTurn) askForConfirmation(id, title string) {
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: t.id, ToolCallId: id,
		InvocationMessage: ahptypes.StringOrMarkdown{Plain: &title}, ConfirmationTitle: &ahptypes.StringOrMarkdown{Plain: &title},
		Options: []ahptypes.ConfirmationOption{
			{Id: ahpOptionApprove, Label: "Allow", Kind: ahptypes.ConfirmationOptionKindApprove},
			{Id: ahpOptionDeny, Label: "Deny", Kind: ahptypes.ConfirmationOptionKindDeny},
		},
	}})
}

func ahpOptionAllows(approval *liveevents.Approval) bool {
	for _, option := range approval.Options {
		if option.OptionID == approval.OptionID {
			return option.Kind == harnessv2.PermissionOptionAllowOnce || option.Kind == harnessv2.PermissionOptionAllowAlways
		}
	}
	return false
}

func (t *ahpTurn) awaitingConfirmation(toolCallID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	call := t.tools[toolCallID]
	return call != nil && call.approval != ""
}

// confirm answers a held permission request with the decision of the
// connection that confirmed it, as that connection's own identity. A refused
// approval reopens the confirmation. A refused denial needs nothing: the
// request expires and is rejected, so the tool does not run either way.
func (t *ahpTurn) confirm(ctx context.Context, principal ahphost.Principal, toolCallID string, approved bool) {
	t.mu.Lock()
	call := t.tools[toolCallID]
	approval := ""
	if call != nil {
		approval, call.approval = call.approval, ""
	}
	t.mu.Unlock()
	if approval == "" || t.b.decideApproval == nil {
		return
	}
	err := t.b.decideApproval(ctx, ahpPrincipalUserInfo(principal), t.session.Namespace, t.task, approval, approved)
	if err == nil {
		return
	}
	log.Info("an AHP tool decision was refused", "namespace", t.session.Namespace, "task", t.task, "reason", ahpErrorMessage(err))
	if !approved {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if call.completed || call.approval != "" {
		return
	}
	call.approval = approval
	t.askForConfirmation(toolCallID, call.title)
}

// recordDiffs keeps the reported file edits and lists them in the session's
// changeset. The first reported content of a file is its "before".
func (t *ahpTurn) recordDiffs(diffs []harnessv2.ToolCallDiff) {
	ctx, b := t.ctx, t.b
	hash := ahpHash(t.principal.UserKey, t.session.Session)
	project := strings.TrimPrefix(t.sessionProjectDir(), "file://")
	for _, diff := range diffs {
		rel := path.Clean("/" + diff.Path)
		full := project + rel
		after := "sha256:" + ahpHash(diff.NewText)
		if b.store.PutAHPRecord(ctx, store.AHPRecord{
			UserKey: t.principal.UserKey, Kind: ahpRecordContent, Key: hash + "/" + after, Value: []byte(diff.NewText), UpdatedAt: b.now().UTC(),
		}) != nil {
			continue
		}
		var file ahpFileContent
		err := b.getRecord(ctx, t.principal.UserKey, ahpRecordFile, hash+"/"+full, &file)
		if errors.Is(err, store.ErrNotFound) && diff.OldText != nil {
			before := "sha256:" + ahpHash(*diff.OldText)
			if b.store.PutAHPRecord(ctx, store.AHPRecord{
				UserKey: t.principal.UserKey, Kind: ahpRecordContent, Key: hash + "/" + before, Value: []byte(*diff.OldText), UpdatedAt: b.now().UTC(),
			}) == nil {
				file.Before = before
			}
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			continue
		}
		file.Path, file.After = full, after
		if b.putRecord(ctx, t.principal.UserKey, ahpRecordFile, hash+"/"+full, file) != nil {
			continue
		}
		t.emitChangesetFile(hash, file)
	}
	t.emitChangeCounts(hash)
}

func (t *ahpTurn) sessionProjectDir() string {
	var project ahpProject
	if err := t.b.getRecord(t.ctx, t.principal.UserKey, ahpRecordProject, t.session.ProjectID, &project); err != nil {
		return "file://" + ahpProjectRoot
	}
	return ahpDirectoryURI(project.Path)
}

func (t *ahpTurn) content(hash, nonce string) string {
	return ahpContentScheme + hash[:24] + "/" + nonce
}

func (t *ahpTurn) emitChangesetFile(hash string, file ahpFileContent) {
	uri := "file://" + file.Path
	after := file.After
	edit := ahptypes.FileEdit{After: &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: t.content(hash, after), Nonce: &after}}}
	beforeText := ""
	if file.Before != "" {
		before := file.Before
		edit.Before = &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: t.content(hash, before), Nonce: &before}}
		if record, err := t.b.store.GetAHPRecord(t.ctx, t.principal.UserKey, ahpRecordContent, hash+"/"+before); err == nil {
			beforeText = string(record.Value)
		}
	}
	afterText := ""
	if record, err := t.b.store.GetAHPRecord(t.ctx, t.principal.UserKey, ahpRecordContent, hash+"/"+after); err == nil {
		afterText = string(record.Value)
	}
	added, removed := ahpLineChanges(beforeText, afterText)
	edit.Diff = &ahptypes.FileEditDiffStats{Added: &added, Removed: &removed}
	_ = t.emit.Emit(t.ctx, t.session.Changeset, ahptypes.StateAction{Value: &ahptypes.ChangesetFileSetAction{
		Type: ahptypes.ActionTypeChangesetFileSet, File: ahptypes.ChangesetFile{Id: ahpHash(file.Path)[:16], Edit: edit},
	}})
}

// emitChangeCounts updates the chat's change counts from every edited file.
func (t *ahpTurn) emitChangeCounts(hash string) {
	records, err := t.b.store.ListAHPRecords(t.ctx, t.principal.UserKey, ahpRecordFile)
	if err != nil {
		return
	}
	var additions, deletions, files int64
	for _, record := range records {
		if !strings.HasPrefix(record.Key, hash+"/") {
			continue
		}
		var file ahpFileContent
		if json.Unmarshal(record.Value, &file) != nil {
			continue
		}
		before, after := "", ""
		if file.Before != "" {
			if content, err := t.b.store.GetAHPRecord(t.ctx, t.principal.UserKey, ahpRecordContent, hash+"/"+file.Before); err == nil {
				before = string(content.Value)
			}
		}
		if content, err := t.b.store.GetAHPRecord(t.ctx, t.principal.UserKey, ahpRecordContent, hash+"/"+file.After); err == nil {
			after = string(content.Value)
		}
		added, removed := ahpLineChanges(before, after)
		additions, deletions, files = additions+added, deletions+removed, files+1
	}
	_ = t.emit.Emit(t.ctx, t.session.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: t.session.Chat,
		Changes: ahptypes.PartialChatSummary{Changes: &ahptypes.ChangesSummary{Additions: &additions, Deletions: &deletions, Files: &files}},
	}})
}

// ahpLineChanges estimates added and removed lines by matching lines as a
// multiset; it is a summary count, not a diff.
func ahpLineChanges(before, after string) (int64, int64) {
	counts := map[string]int{}
	oldLines := ahpLines(before)
	for _, line := range oldLines {
		counts[line]++
	}
	common := 0
	newLines := ahpLines(after)
	for _, line := range newLines {
		if counts[line] > 0 {
			counts[line]--
			common++
		}
	}
	return int64(len(newLines) - common), int64(len(oldLines) - common)
}

func ahpLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// settle ends the turn from its Task's terminal phase.
func (t *ahpTurn) settle(task *corev1alpha1.Task) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.streamedText && task.Status.ResultRef != nil && task.Status.ResultRef.Available && t.b.h.resultStore != nil {
		if data, err := t.b.h.resultStore.GetResult(t.ctx, task.Namespace, task.Name); err == nil {
			t.text(string(data))
		}
	}
	for _, id := range t.order {
		if call := t.tools[id]; call != nil && !call.completed {
			title := "interrupted"
			t.readyTool(id, call, title, nil)
			t.completeTool(id, call, false, title, nil)
		}
	}
	duration := t.b.now().Sub(t.startedAt).Milliseconds()
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded:
		if note := ahpDeliveryNote(task); note != "" {
			t.textPart = ""
			t.text(note)
		}
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCompleteAction{Type: ahptypes.ActionTypeChatTurnComplete, TurnId: t.id, Duration: duration}})
	case corev1alpha1.TaskPhaseCancelled:
		if !t.clientCancelled {
			t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: t.id, Duration: duration}})
		}
	default:
		reason, message := "TaskFailed", strings.TrimSpace(task.Status.Message)
		if task.Status.Execution != nil && task.Status.Execution.Reason != "" {
			reason = string(task.Status.Execution.Reason)
		}
		if task.Status.Delivery != nil && task.Status.Delivery.Message != "" {
			message = task.Status.Delivery.Message
		}
		if message == "" {
			message = "the turn failed"
		}
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
			Type: ahptypes.ActionTypeChatError, TurnId: t.id, Duration: duration,
			Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: reason, Message: message}},
		}})
	}
	t.finishSession()
}

func (t *ahpTurn) settleWithError(kind, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
		Type: ahptypes.ActionTypeChatError, TurnId: t.id, Duration: t.b.now().Sub(t.startedAt).Milliseconds(),
		Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: kind, Message: message}},
	}})
	t.finishSession()
}

// finishSession marks the chat idle and the session free for the next turn.
func (t *ahpTurn) finishSession() {
	now := ahpNow(t.b)
	_ = t.emit.Emit(t.ctx, t.session.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: t.session.Chat,
		Changes: ahptypes.PartialChatSummary{Status: ahpStatus(ahptypes.SessionStatusIdle), ModifiedAt: &now},
	}})
	t.b.updateSession(t.ctx, t.principal.UserKey, t.session.Session, func(s *ahpSession) {
		if s.ActiveTurn == t.id {
			s.ActiveTurn, s.ActiveTask, s.ActiveStartedAt = "", "", ""
		}
	})
}

// ahpDeliveryNote tells the person where a write turn's changes went.
func ahpDeliveryNote(task *corev1alpha1.Task) string {
	delivery := task.Status.Delivery
	if delivery == nil || delivery.Branch == "" {
		return ""
	}
	switch delivery.State {
	case corev1alpha1.TaskDeliveryStateVerifiedExact, corev1alpha1.TaskDeliveryStateDeliveredSuperseded:
		return fmt.Sprintf("\n\nOrka pushed this turn's changes to the branch `%s`.", delivery.Branch)
	default:
		return ""
	}
}

func (b *ahpBackend) forgetTurn(turn *ahpTurn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := ahpTurnKey(turn.principal.UserKey, turn.session.Session)
	if b.turns[key] == turn {
		delete(b.turns, key)
	}
	turn.cancel()
}

// recoverTurns settles turns a previous leader left running, once per user,
// from their Tasks.
func (b *ahpBackend) recoverTurns(ctx context.Context, principal ahphost.Principal) {
	b.mu.Lock()
	if b.recovered[principal.UserKey] || b.emitterFor == nil {
		b.mu.Unlock()
		return
	}
	b.recovered[principal.UserKey] = true
	b.mu.Unlock()
	records, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return
	}
	for _, raw := range records {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) != nil || record.ActiveTurn == "" || b.activeTurn(principal.UserKey, record.Session) != nil {
			continue
		}
		started, _ := time.Parse(time.RFC3339Nano, record.ActiveStartedAt)
		turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		turn := &ahpTurn{
			b: b, principal: principal, session: record, id: record.ActiveTurn,
			task: record.ActiveTask, startedAt: started, emit: b.emitterFor(principal), ctx: turnCtx, cancel: cancel,
			tools: map[string]*ahpToolCall{},
		}
		b.mu.Lock()
		b.turns[ahpTurnKey(principal.UserKey, record.Session)] = turn
		b.mu.Unlock()
		go turn.run(b.live.Subscribe(record.Namespace, record.ActiveTask))
	}
}
