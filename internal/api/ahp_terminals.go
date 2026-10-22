/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
	executionevents "github.com/orka-agents/orka/internal/events"
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

// CreateTerminal starts a shell in the user's workspace.
func (b *ahpBackend) CreateTerminal(ctx context.Context, principal ahphost.Principal, params ahptypes.CreateTerminalParams, emit ahphost.Emitter) (ahptypes.TerminalState, error) {
	if !b.workspaces.enabled() {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminals are not enabled on this host"}
	}
	if !strings.HasPrefix(params.Channel, ahpTerminalScheme) {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminal URIs use the ahp-terminal scheme"}
	}
	cwd := ahpWorkspaceRoot
	if params.Cwd != nil {
		if dir, ok := ahpWorkspacePath(*params.Cwd); ok {
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
	b.workspaces.prepare(ctx, principal, pod, cwd)

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
	output := newAHPTerminalOutput(termCtx, emit, params.Channel, func() []string { return b.workspaces.tokensFor(principal.UserKey) })
	go func() {
		// The shell leads its own session; its PID names that session, so
		// ending the terminal can stop everything started in it.
		err := b.workspaces.stream(termCtx, pod,
			[]string{ahpShell, "-c", `echo $$ > "$2"; cd "$1" 2>/dev/null || cd /workspace; exec /bin/sh -i`, "sh", cwd, term.pidFile},
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
	cwdURI := ahpDirectoryURI(cwd)
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
	_ = b.workspaces.run(killCtx, pod, ahpShell, "-c",
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
		if len(a.Title) > ahpTerminalTitleMax {
			return &ahphost.Rejection{Reason: "the title is too long"}
		}
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
		b.workspaces.refreshCredentialForTerminal(principal)
	case *ahptypes.TerminalResizedAction:
		select {
		case term.sizes <- remotecommand.TerminalSize{Width: uint16(a.Cols), Height: uint16(a.Rows)}: //nolint:gosec // validated at admission
		default:
		}
	case *ahptypes.TerminalTitleChangedAction:
		// The terminal list is built from the backend's terminals, so a
		// renamed terminal keeps its name there too.
		b.mu.Lock()
		term.title = a.Title
		b.mu.Unlock()
		if b.updateTerminals != nil {
			_ = b.updateTerminals(term.ctx, principal)
		}
	}
}

// ahpTerminalOutput turns a shell's output into terminal/data actions,
// coalesced and cut on UTF-8 boundaries. Terminal data is journaled, so the
// workspace's own git tokens are replaced before it leaves.
type ahpTerminalOutput struct {
	ctx     context.Context
	emit    ahphost.Emitter
	channel string
	// credentials returns the tokens the workspace holds.
	credentials func() []string

	mu      sync.Mutex
	pending []byte
	timer   *time.Timer
	closed  bool
	// heldAt is when pending started to end in what could begin a token. It is
	// held back until more output arrives, or ahpTerminalTokenHold passes.
	heldAt time.Time
}

func newAHPTerminalOutput(ctx context.Context, emit ahphost.Emitter, channel string, credentials func() []string) *ahpTerminalOutput {
	return &ahpTerminalOutput{ctx: ctx, emit: emit, channel: channel, credentials: credentials}
}

// ahpTerminalTokenHold is how long terminal output that is a token's public
// start, such as "gh", waits for more output.
const ahpTerminalTokenHold = 250 * time.Millisecond

// ahpTerminalTitleMax bounds a terminal's title.
const ahpTerminalTitleMax = 256

// ahpMinRedactedToken ignores values too short to be a token: replacing them
// would mangle ordinary output.
const ahpMinRedactedToken = 8

// ahpTokenPublicStart is the length of a token's type prefix through its
// first underscore, such as "ghu_", which reveals nothing about the token.
func ahpTokenPublicStart(token string) int {
	return strings.IndexByte(token, '_') + 1
}

// ahpTokenForms are what terminal output never carries for a token: the token,
// and its secret part, which output split right after the public start carries
// alone.
func ahpTokenForms(token string) []string {
	if len(token) < ahpMinRedactedToken {
		return nil
	}
	forms := []string{token}
	if secret := token[ahpTokenPublicStart(token):]; secret != token && len(secret) >= ahpMinRedactedToken {
		forms = append(forms, secret)
	}
	return forms
}

// ahpRedactTokens replaces every form of every token in data, longest first.
func ahpRedactTokens(data []byte, tokens []string) []byte {
	var forms []string
	for _, token := range tokens {
		forms = append(forms, ahpTokenForms(token)...)
	}
	slices.SortFunc(forms, func(a, b string) int { return len(b) - len(a) })
	for _, form := range forms {
		data = bytes.ReplaceAll(data, []byte(form), []byte(executionevents.ExecutionEventRedactedValue))
	}
	return data
}

// ahpTokenHeld says how much of data's end could be the start of a token
// form. long is past a token's public start, so a token is under way; short
// is a public start alone, such as "gh".
func ahpTokenHeld(data []byte, tokens []string) (long, short int) {
	for _, token := range tokens {
		public := ahpTokenPublicStart(token)
		for i, form := range ahpTokenForms(token) {
			for k := min(len(form)-1, len(data)); k > 0; k-- {
				if !bytes.HasSuffix(data, []byte(form[:k])) {
					continue
				}
				if i == 0 && k <= public {
					short = max(short, k)
				} else {
					long = max(long, k)
				}
				break
			}
		}
	}
	return long, short
}

func (o *ahpTerminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, io.ErrClosedPipe
	}
	o.pending = append(o.pending, p...)
	o.heldAt = time.Time{}
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
	ready := len(o.pending)
	if o.credentials != nil {
		if tokens := o.credentials(); len(tokens) > 0 {
			o.pending = ahpRedactTokens(o.pending, tokens)
			ready = len(o.pending)
			// A token may be split across writes. Text that is one under way
			// waits for more output, however long; a token's public start,
			// such as "gh", waits only ahpTerminalTokenHold, because what
			// follows it is redacted on its own.
			if long, short := ahpTokenHeld(o.pending, tokens); !o.closed && long > 0 {
				ready -= long
			} else if !o.closed && short > 0 {
				if o.heldAt.IsZero() {
					o.heldAt = time.Now()
				}
				if wait := ahpTerminalTokenHold - time.Since(o.heldAt); wait > 0 {
					ready -= short
					if o.timer == nil {
						o.timer = time.AfterFunc(wait, func() {
							o.mu.Lock()
							defer o.mu.Unlock()
							o.timer = nil
							o.flushLocked()
						})
					}
				}
			}
		}
	}
	for ready > 0 {
		cut := min(ready, ahpTerminalChunkMax)
		if cut < ready {
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
		ready -= cut
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
