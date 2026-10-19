/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	ahpTerminalScheme     = "ahp-terminal:"
	ahpTerminalFlushEvery = 20 * time.Millisecond
	ahpTerminalChunkMax   = 32 << 10
	ahpTerminalInputMax   = 64 << 10
	ahpTerminalsPerUser   = 32
)

// ahpTerminal is one shell in a user's workspace.
type ahpTerminal struct {
	ctx     context.Context
	channel string
	title   string
	claim   ahptypes.TerminalClaim
	input   chan []byte
	sizes   chan remotecommand.TerminalSize
	cancel  context.CancelFunc
	// pending counts input admitted and not yet written, so admission can
	// refuse input instead of a full queue dropping it.
	pending atomic.Int64
	// pidFile records the shell's session in the workspace, so ending the
	// terminal can stop every process it started.
	pidFile string
}

const ahpTerminalInputQueue = 512

// Next implements remotecommand.TerminalSizeQueue. It returns nil once the
// terminal ends, which releases the resize worker.
func (t *ahpTerminal) Next() *remotecommand.TerminalSize {
	select {
	case size := <-t.sizes:
		return &size
	case <-t.ctx.Done():
		return nil
	}
}

func ahpTerminalKey(userKey, channel string) string { return userKey + "\x00" + channel }

// hasTerminals reports whether the user has a terminal running.
func (b *ahpBackend) hasTerminals(userKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.shells {
		if strings.HasPrefix(key, userKey+"\x00") {
			return true
		}
	}
	return false
}

func (b *ahpBackend) terminal(userKey, channel string) *ahpTerminal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shells[ahpTerminalKey(userKey, channel)]
}

// mayUseWorkspace refuses a delegated token that may not create Tasks: a
// workspace runs the person's commands.
func (b *ahpBackend) mayUseWorkspace(principal ahphost.Principal) error {
	cfg := b.h.contextTokenAuthorization
	if principal.Constraints.Delegated && cfg.Enabled() && !hasAnyScope(principal.Constraints.Scopes, cfg.TaskCreateScopes) {
		return &ahphost.Rejection{Reason: "this connection's token may not run commands"}
	}
	return nil
}

// CreateTerminal starts a shell in the user's workspace.
func (b *ahpBackend) CreateTerminal(ctx context.Context, principal ahphost.Principal, params ahptypes.CreateTerminalParams, emit ahphost.Emitter) (ahptypes.TerminalState, error) {
	if !b.workspaces.enabled() {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminals are not enabled on this host"}
	}
	if err := b.mayUseWorkspace(principal); err != nil {
		return ahptypes.TerminalState{}, err
	}
	if !strings.HasPrefix(params.Channel, ahpTerminalScheme) {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminal URIs use the ahp-terminal scheme"}
	}
	cwd := ahpWorkspaceRoot
	if params.Cwd != nil {
		dir := path.Clean(strings.TrimPrefix(*params.Cwd, "file://"))
		if dir == ahpWorkspaceRoot || strings.HasPrefix(dir, ahpWorkspaceRoot+"/") {
			cwd = dir
		}
	}
	title := "Terminal"
	if params.Name != nil && strings.TrimSpace(*params.Name) != "" {
		title = *params.Name
	}
	termCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	term := &ahpTerminal{
		ctx: termCtx, channel: params.Channel, title: title, claim: params.Claim, cancel: cancel,
		input: make(chan []byte, ahpTerminalInputQueue), sizes: make(chan remotecommand.TerminalSize, 4),
		pidFile: "/tmp/orka-terminal-" + ahpHash(principal.UserKey, params.Channel)[:16] + ".pid",
	}
	if params.Cols != nil && params.Rows != nil && *params.Cols > 0 && *params.Rows > 0 {
		term.sizes <- remotecommand.TerminalSize{Width: uint16(min(*params.Cols, 1000)), Height: uint16(min(*params.Rows, 1000))} //nolint:gosec // bounded above
	}
	// Claim the terminal's slot before anything starts, so concurrent creates
	// can neither replace a running shell nor pass the limit together, and
	// hold the user's lifecycle lock until the shell runs, so disposing the
	// last session cannot remove the workspace under it.
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	key := ahpTerminalKey(principal.UserKey, params.Channel)
	b.mu.Lock()
	count := 0
	for existing := range b.shells {
		if strings.HasPrefix(existing, principal.UserKey+"\x00") {
			count++
		}
	}
	_, taken := b.shells[key]
	if _, used := b.retired[key]; used {
		taken = true
	}
	if taken || count >= ahpTerminalsPerUser {
		b.mu.Unlock()
		cancel()
		if taken {
			return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "the terminal URI is already used; use a new one"}
		}
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "too many terminals are open"}
	}
	b.shells[key] = term
	b.mu.Unlock()
	pod, err := b.workspaces.ensure(ctx, principal, ahpWorkspaceStartWait)
	if err != nil {
		b.mu.Lock()
		if b.shells[key] == term {
			delete(b.shells, key)
		}
		b.mu.Unlock()
		cancel()
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: err.Error()}
	}

	stdin, stdinWriter := io.Pipe()
	// Ending the terminal unblocks a write the exec stream will never read.
	context.AfterFunc(termCtx, func() {
		_ = stdinWriter.CloseWithError(io.EOF)
		_ = stdin.Close()
	})
	go func() {
		defer func() { _ = stdinWriter.Close() }()
		for {
			select {
			case <-termCtx.Done():
				return
			case data := <-term.input:
				_, err := stdinWriter.Write(data)
				term.pending.Add(-1)
				if err != nil {
					return
				}
			}
		}
	}()
	output := newAHPTerminalOutput(termCtx, emit, params.Channel)
	go func() {
		// The shell leads its own session; its PID names that session, so
		// ending the terminal can stop everything started in it.
		err := b.workspaces.stream(termCtx, pod,
			[]string{"/bin/sh", "-c", `echo $$ > "$2"; cd "$1" 2>/dev/null || cd /workspace; exec /bin/sh -i`, "sh", cwd, term.pidFile},
			stdin, output, true, term)
		output.close()
		var exitCode *int64
		var exit interface{ ExitStatus() int }
		if errors.As(err, &exit) {
			code := int64(exit.ExitStatus())
			exitCode = &code
		} else if err == nil {
			zero := int64(0)
			exitCode = &zero
		}
		b.endTerminal(context.WithoutCancel(ctx), principal, term, emit, exitCode)
	}()
	cwdURI := "file://" + cwd
	isPty := true
	state := ahptypes.TerminalState{
		Title: title, Cwd: &cwdURI, Cols: params.Cols, Rows: params.Rows, Content: []ahptypes.TerminalContentPart{},
		Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}},
		Claim:     params.Claim, IsPty: &isPty,
	}
	return state, nil
}

func (b *ahpBackend) endTerminal(ctx context.Context, principal ahphost.Principal, term *ahpTerminal, emit ahphost.Emitter, exitCode *int64) {
	b.stopShell(ctx, principal, term)
	b.mu.Lock()
	key := ahpTerminalKey(principal.UserKey, term.channel)
	current := b.shells[key] == term
	if current {
		delete(b.shells, key)
		b.retireLocked(key)
	}
	b.mu.Unlock()
	if !current {
		return
	}
	_ = emit.Emit(ctx, term.channel, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{Type: ahptypes.ActionTypeTerminalExited, ExitCode: exitCode}})
	if b.updateTerminals != nil {
		_ = b.updateTerminals(ctx, principal)
	}
}

// DisposeTerminal stops a shell and every process it started, then frees its slot.
func (b *ahpBackend) DisposeTerminal(ctx context.Context, principal ahphost.Principal, channel string) error {
	if err := b.mayUseWorkspace(principal); err != nil {
		return err
	}
	term := b.terminal(principal.UserKey, channel)
	if term == nil {
		return nil
	}
	b.stopShell(ctx, principal, term)
	b.mu.Lock()
	key := ahpTerminalKey(principal.UserKey, channel)
	if b.shells[key] == term {
		delete(b.shells, key)
		b.retireLocked(key)
	}
	b.mu.Unlock()
	return nil
}

const (
	ahpRetiredTerminalsMax = 4096
	ahpRetiredTerminalTTL  = 24 * time.Hour
)

// retireLocked remembers a used terminal URI. Clients name terminals with
// fresh random URIs, so refusing reuse costs them nothing. b.mu must be held.
func (b *ahpBackend) retireLocked(key string) {
	if b.retired == nil {
		b.retired = map[string]time.Time{}
	}
	now := b.now()
	if len(b.retired) >= ahpRetiredTerminalsMax {
		for old, at := range b.retired {
			if now.Sub(at) > ahpRetiredTerminalTTL {
				delete(b.retired, old)
			}
		}
	}
	b.retired[key] = now
}

// stopShell ends a terminal's exec stream and kills its session in the
// workspace: closing the stream alone can leave the shell running there.
func (b *ahpBackend) stopShell(ctx context.Context, principal ahphost.Principal, term *ahpTerminal) {
	term.cancel()
	pod, ok := b.workspaces.running(ctx, principal)
	if !ok {
		return
	}
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = b.workspaces.run(killCtx, pod, "/bin/sh", "-c",
		`[ -f "$1" ] || exit 0; sid=$(cat "$1"); case "$sid" in ''|*[!0-9]*) exit 0;; esac; pkill -KILL -s "$sid"; rm -f "$1"`,
		"sh", term.pidFile)
}

// Terminals lists the user's running shells.
func (b *ahpBackend) Terminals(_ context.Context, principal ahphost.Principal) ([]ahptypes.TerminalInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var terminals []ahptypes.TerminalInfo
	for key, term := range b.shells {
		if !strings.HasPrefix(key, principal.UserKey+"\x00") {
			continue
		}
		terminals = append(terminals, ahptypes.TerminalInfo{
			Resource: term.channel, Title: term.title, Claim: term.claim,
			Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}},
		})
	}
	sort.Slice(terminals, func(i, j int) bool { return terminals[i].Resource < terminals[j].Resource })
	return terminals, nil
}

// authorizeTerminalAction admits input and resizes for the user's own shells.
func (b *ahpBackend) authorizeTerminalAction(principal ahphost.Principal, channel string, action ahptypes.StateAction) error {
	term := b.terminal(principal.UserKey, channel)
	if term == nil {
		return &ahphost.Rejection{Reason: "the terminal is not running"}
	}
	switch a := action.Value.(type) {
	case *ahptypes.TerminalInputAction:
		if len(a.Data) > ahpTerminalInputMax {
			return &ahphost.Rejection{Reason: "the input is too large"}
		}
		if err := b.mayUseWorkspace(principal); err != nil {
			return err
		}
		// Reserve room for the input now, on the terminal just checked: once
		// admitted it is always delivered.
		if term.pending.Add(1) > ahpTerminalInputQueue {
			term.pending.Add(-1)
			return &ahphost.Rejection{Reason: "the terminal is not reading input; try again"}
		}
		return nil
	case *ahptypes.TerminalResizedAction:
		if a.Cols < 1 || a.Rows < 1 || a.Cols > 1000 || a.Rows > 1000 {
			return &ahphost.Rejection{Reason: "the size is not valid"}
		}
		return nil
	case *ahptypes.TerminalTitleChangedAction:
		return nil
	default:
		return &ahphost.Rejection{Reason: "not supported on this host"}
	}
}

// handleTerminalAction delivers input and resizes in the order they came.
func (b *ahpBackend) handleTerminalAction(principal ahphost.Principal, channel string, action ahptypes.StateAction) {
	term := b.terminal(principal.UserKey, channel)
	if term == nil {
		return
	}
	switch a := action.Value.(type) {
	case *ahptypes.TerminalInputAction:
		// Admission reserved room, so this never blocks.
		select {
		case term.input <- []byte(a.Data):
		case <-term.ctx.Done():
		}
	case *ahptypes.TerminalResizedAction:
		select {
		case term.sizes <- remotecommand.TerminalSize{Width: uint16(a.Cols), Height: uint16(a.Rows)}: //nolint:gosec // validated at admission
		default:
		}
	}
}

// ahpTerminalOutput turns a shell's output into terminal/data actions,
// coalesced and cut on UTF-8 boundaries.
type ahpTerminalOutput struct {
	ctx     context.Context
	emit    ahphost.Emitter
	channel string

	mu      sync.Mutex
	pending []byte
	timer   *time.Timer
	closed  bool
}

func newAHPTerminalOutput(ctx context.Context, emit ahphost.Emitter, channel string) *ahpTerminalOutput {
	return &ahpTerminalOutput{ctx: ctx, emit: emit, channel: channel}
}

func (o *ahpTerminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, io.ErrClosedPipe
	}
	o.pending = append(o.pending, p...)
	if len(o.pending) >= ahpTerminalChunkMax {
		o.flushLocked()
	} else if o.timer == nil {
		o.timer = time.AfterFunc(ahpTerminalFlushEvery, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.timer = nil
			o.flushLocked()
		})
	}
	return len(p), nil
}

func (o *ahpTerminalOutput) flushLocked() {
	for len(o.pending) > 0 {
		cut := min(len(o.pending), ahpTerminalChunkMax)
		if cut < len(o.pending) {
			// Move the cut back to the start of a character, but never past
			// what one character spans: malformed bytes are passed on.
			at := cut
			for back := 0; back < utf8.UTFMax-1 && at > 0 && !utf8.RuneStart(o.pending[at]); back++ {
				at--
			}
			if at > 0 && utf8.RuneStart(o.pending[at]) {
				cut = at
			}
		} else if !o.closed {
			// Hold back a final character whose bytes have not all arrived:
			// at most UTFMax-1 bytes stay pending.
			for back := 1; back < utf8.UTFMax && back <= cut; back++ {
				if i := cut - back; utf8.RuneStart(o.pending[i]) {
					if !utf8.FullRune(o.pending[i:cut]) {
						cut = i
					}
					break
				}
			}
			if cut == 0 {
				return
			}
		}
		data := strings.ToValidUTF8(string(o.pending[:cut]), "\uFFFD")
		o.pending = o.pending[cut:]
		_ = o.emit.Emit(o.ctx, o.channel, ahptypes.StateAction{Value: &ahptypes.TerminalDataAction{Type: ahptypes.ActionTypeTerminalData, Data: data}})
	}
}

func (o *ahpTerminalOutput) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.closed = true
	o.flushLocked()
}
