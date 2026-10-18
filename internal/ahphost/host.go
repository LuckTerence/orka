/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/orka-agents/orka/internal/store"
)

const (
	rootChannel = ahptypes.RootResourceURI

	defaultJournalEntries = 1000
	defaultJournalBytes   = 8 << 20
	defaultSendQueue      = 1024
	defaultTokenRecheck   = 30 * time.Second
)

var errUnknownChannel = errors.New("unknown channel")

// Config configures a Host.
type Config struct {
	Store    store.AHPStore
	Tokens   *TokenService
	Backends *Registry
	// Limits bound each user's replay journal.
	Limits     store.AHPJournalLimits
	ServerInfo ahptypes.Implementation
	// SendQueue bounds the messages waiting for one slow client.
	SendQueue int
	// TokenRecheck is how often connected tokens are checked again.
	TokenRecheck time.Duration
	Logger       logr.Logger
	Now          func() time.Time
}

// Host serves the Agent Host Protocol for every user. Each user has their
// own sequence, journal, channel states and session catalog; the host keeps
// one hub per user that serializes changes and fans them out to that user's
// connections.
type Host struct {
	cfg Config

	mu      sync.Mutex
	hubs    map[string]*userHub
	byToken map[string]map[*conn]struct{}
	stopped bool
}

type userHub struct {
	mu     sync.Mutex
	key    string
	seq    int64
	loaded bool
	conns  map[*conn]struct{}
}

// storedChannel is how a channel's state is stored: the reduced state plus
// the session that owns the channel, so its actions reach the session's backend.
type storedChannel struct {
	Session string                 `json:"session"`
	State   ahptypes.SnapshotState `json:"state"`
}

// New builds a Host. Wire its token service's OnRevoke to Host.CloseToken.
func New(cfg Config) (*Host, error) {
	if cfg.Store == nil || cfg.Tokens == nil {
		return nil, fmt.Errorf("an AHP store and token service are required")
	}
	if cfg.Limits.MaxEntries <= 0 {
		cfg.Limits.MaxEntries = defaultJournalEntries
	}
	if cfg.Limits.MaxBytes <= 0 {
		cfg.Limits.MaxBytes = defaultJournalBytes
	}
	if cfg.SendQueue <= 0 {
		cfg.SendQueue = defaultSendQueue
	}
	if cfg.TokenRecheck <= 0 {
		cfg.TokenRecheck = defaultTokenRecheck
	}
	if cfg.ServerInfo.Name == "" {
		cfg.ServerInfo = ahptypes.Implementation{Name: "orka"}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Host{cfg: cfg, hubs: map[string]*userHub{}, byToken: map[string]map[*conn]struct{}{}}
	return h, nil
}

// Tokens returns the host's token service.
func (h *Host) Tokens() *TokenService { return h.cfg.Tokens }

// Start rechecks connected tokens until ctx ends, closing connections whose
// token was revoked or expired.
func (h *Host) Start(ctx context.Context) error {
	ticker := time.NewTicker(h.cfg.TokenRecheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.stopped = true
			h.mu.Unlock()
			h.closeAll("the server is shutting down")
			return nil
		case <-ticker.C:
			for _, c := range h.connections() {
				if err := h.cfg.Tokens.Recheck(ctx, c.principal); err != nil {
					c.closeWith(closeTokenInvalid, "the connection token is no longer valid")
				}
			}
		}
	}
}

// CloseToken closes every connection that uses a token.
func (h *Host) CloseToken(tokenID string) {
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.byToken[tokenID]))
	for c := range h.byToken[tokenID] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.closeWith(closeTokenInvalid, "the connection token was revoked")
	}
}

func (h *Host) connections() []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var conns []*conn
	for _, set := range h.byToken {
		for c := range set {
			conns = append(conns, c)
		}
	}
	return conns
}

func (h *Host) closeAll(reason string) {
	for _, c := range h.connections() {
		c.closeWith(closeGoingAway, reason)
	}
}

func (h *Host) hub(userKey string) *userHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	hub := h.hubs[userKey]
	if hub == nil {
		hub = &userHub{key: userKey, conns: map[*conn]struct{}{}}
		h.hubs[userKey] = hub
	}
	return hub
}

// register adds a connection; it refuses once the host has stopped.
func (h *Host) register(c *conn) bool {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return false
	}
	if h.byToken[c.principal.TokenID] == nil {
		h.byToken[c.principal.TokenID] = map[*conn]struct{}{}
	}
	h.byToken[c.principal.TokenID][c] = struct{}{}
	h.mu.Unlock()
	hub := h.hub(c.principal.UserKey)
	hub.mu.Lock()
	hub.conns[c] = struct{}{}
	hub.mu.Unlock()
	return true
}

func (h *Host) unregister(c *conn) {
	h.mu.Lock()
	if set := h.byToken[c.principal.TokenID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byToken, c.principal.TokenID)
		}
	}
	h.mu.Unlock()
	hub := h.hub(c.principal.UserKey)
	hub.mu.Lock()
	delete(hub.conns, c)
	hub.mu.Unlock()
}

// loadLocked reads the user's sequence once. hub.mu must be held.
func (h *Host) loadLocked(ctx context.Context, hub *userHub) error {
	if hub.loaded {
		return nil
	}
	seq, err := h.cfg.Store.AHPServerSeq(ctx, hub.key)
	if err != nil {
		return err
	}
	hub.seq, hub.loaded = seq, true
	return nil
}

func (h *Host) serverSeq(ctx context.Context, userKey string) (int64, error) {
	var seq int64
	err := h.locked(ctx, userKey, func(current int64) error {
		seq = current
		return nil
	})
	return seq, err
}

// locked runs fn with the user's current sequence under the hub lock. Actions
// are applied and delivered under the same lock, so a snapshot taken, a
// subscription installed and a response queued inside fn are never overtaken
// by a concurrent action, nor miss one.
func (h *Host) locked(ctx context.Context, userKey string, fn func(seq int64) error) error {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	return fn(hub.seq)
}

func (h *Host) readChannel(ctx context.Context, userKey, channel string) (storedChannel, int64, error) {
	record, err := h.cfg.Store.GetAHPChannelState(ctx, userKey, channel)
	if errors.Is(err, store.ErrNotFound) {
		return storedChannel{}, 0, errUnknownChannel
	}
	if err != nil {
		return storedChannel{}, 0, err
	}
	var stored storedChannel
	if err := json.Unmarshal(record.State, &stored); err != nil {
		return storedChannel{}, 0, fmt.Errorf("decode channel state: %w", err)
	}
	return stored, record.FromSeq, nil
}

// apply reduces actions into a channel's state, journals them under the next
// sequence numbers, and fans them out to the user's subscribed connections.
func (h *Host) apply(
	ctx context.Context,
	principal Principal,
	channel string,
	actions []ahptypes.StateAction,
	origin *ahptypes.ActionOrigin,
) error {
	hub := h.hub(principal.UserKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	var states []store.AHPChannelState
	var stored storedChannel
	if channel != rootChannel {
		var err error
		if stored, _, err = h.readChannel(ctx, principal.UserKey, channel); err != nil {
			return err
		}
		for _, action := range actions {
			if reduce(&stored.State, action) == ahp.ReduceOutcomeOutOfScope {
				return fmt.Errorf("action does not apply to channel %s", channel)
			}
		}
	}
	envelopes := make([]ahptypes.ActionEnvelope, len(actions))
	entries := make([]store.AHPJournalEntry, len(actions))
	for i, action := range actions {
		envelopes[i] = ahptypes.ActionEnvelope{
			Channel: channel, Action: action, ServerSeq: hub.seq + int64(i) + 1, Origin: origin,
		}
		raw, err := json.Marshal(envelopes[i])
		if err != nil {
			return err
		}
		entries[i] = store.AHPJournalEntry{Seq: envelopes[i].ServerSeq, Channel: channel, Envelope: raw}
	}
	last := hub.seq + int64(len(actions))
	if channel != rootChannel {
		raw, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		states = append(states, store.AHPChannelState{Channel: channel, State: raw, FromSeq: last})
	}
	if err := h.cfg.Store.AppendAHPJournal(ctx, principal.UserKey, hub.seq, entries, states, h.cfg.Limits); err != nil {
		hub.loaded = false
		return err
	}
	hub.seq = last
	for c := range hub.conns {
		for _, envelope := range envelopes {
			c.deliverAction(envelope)
		}
	}
	return nil
}

// storeChannels records new channel states without journaling actions.
func (h *Host) storeChannels(ctx context.Context, userKey string, channels map[string]storedChannel) error {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	states := make([]store.AHPChannelState, 0, len(channels))
	for channel, state := range channels {
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		states = append(states, store.AHPChannelState{Channel: channel, State: raw, FromSeq: hub.seq})
	}
	if err := h.cfg.Store.AppendAHPJournal(ctx, userKey, hub.seq, nil, states, h.cfg.Limits); err != nil {
		hub.loaded = false
		return err
	}
	return nil
}

// notifyRoot sends a protocol notification to the user's connections
// subscribed to the root channel. Notifications are not journaled.
func (h *Host) notifyRoot(userKey, method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for c := range hub.conns {
		c.deliverNotification(rootChannel, method, raw)
	}
}

// snapshot returns a channel's current state for the principal, or
// errUnknownChannel if the user has no such channel.
func (h *Host) snapshot(ctx context.Context, principal Principal, channel string) (ahptypes.Snapshot, error) {
	var snapshot ahptypes.Snapshot
	err := h.locked(ctx, principal.UserKey, func(seq int64) error {
		var err error
		snapshot, err = h.snapshotLocked(ctx, principal, seq, channel)
		return err
	})
	return snapshot, err
}

// snapshotLocked is snapshot for a caller inside locked, at sequence seq.
func (h *Host) snapshotLocked(ctx context.Context, principal Principal, seq int64, channel string) (ahptypes.Snapshot, error) {
	if channel == rootChannel {
		root, err := h.rootState(ctx, principal)
		if err != nil {
			return ahptypes.Snapshot{}, err
		}
		return ahptypes.Snapshot{Resource: rootChannel, State: ahptypes.SnapshotState{Root: root}, FromSeq: seq}, nil
	}
	stored, fromSeq, err := h.readChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return ahptypes.Snapshot{}, err
	}
	return ahptypes.Snapshot{Resource: channel, State: stored.State, FromSeq: fromSeq}, nil
}

func (h *Host) rootState(ctx context.Context, principal Principal) (*ahptypes.RootState, error) {
	agents, err := h.cfg.Backends.Agents(ctx, principal)
	if err != nil {
		return nil, err
	}
	sessions, err := h.cfg.Store.ListAHPSessions(ctx, principal.UserKey)
	if err != nil {
		return nil, err
	}
	active := int64(len(sessions))
	return &ahptypes.RootState{Agents: agents, ActiveSessions: &active}, nil
}

// emitter delivers a backend's actions for one principal.
type emitter struct {
	host      *Host
	principal Principal
}

func (e emitter) Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error {
	if channel == rootChannel {
		return fmt.Errorf("session backends cannot change the root channel")
	}
	return e.host.apply(ctx, e.principal, channel, actions, nil)
}

func reduce(state *ahptypes.SnapshotState, action ahptypes.StateAction) ahp.ReduceOutcome {
	switch {
	case state.Session != nil:
		return ahp.ApplyActionToSession(state.Session, action)
	case state.Chat != nil:
		return ahp.ApplyActionToChat(state.Chat, action)
	case state.Changeset != nil:
		return ahp.ApplyActionToChangeset(state.Changeset, action)
	case state.Terminal != nil:
		return ahp.ApplyActionToTerminal(state.Terminal, action)
	case state.Annotations != nil:
		return ahp.ApplyActionToAnnotations(state.Annotations, action)
	case state.Canvas != nil:
		return ahp.ApplyActionToCanvas(state.Canvas, action)
	case state.ResourceWatch != nil:
		return ahp.ApplyActionToResourceWatch(state.ResourceWatch, action)
	case state.Automations != nil:
		return ahp.ApplyActionToAutomation(state.Automations, action)
	case state.AutomationRun != nil:
		return ahp.ApplyActionToAutomationRun(state.AutomationRun, action)
	default:
		return ahp.ReduceOutcomeOutOfScope
	}
}
