/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// SessionBackend runs AHP sessions for the agents it offers. The host owns
// the protocol, the journal and fan-out; a backend owns what a session does.
// Every call carries the principal so the backend can authorize the Orka
// operation it causes with that principal's constraints.
type SessionBackend interface {
	// Name identifies the backend in the session catalog.
	Name() string
	// Agents lists the agents this backend offers the principal.
	Agents(ctx context.Context, principal Principal) ([]ahptypes.AgentInfo, error)
	// CreateSession starts a session. The host records the returned states and
	// marks the session ready.
	CreateSession(ctx context.Context, principal Principal, req CreateSessionRequest, emit Emitter) (*CreatedSession, error)
	// DisposeSession stops a session and releases what it holds.
	DisposeSession(ctx context.Context, principal Principal, session string) error
	// AuthorizeClientAction decides whether a client-dispatched action that the
	// host already validated may be applied, as the Orka operation it causes.
	// Returning a *Rejection rejects it with a reason the client sees.
	AuthorizeClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction) error
	// HandleClientAction runs the side effects of a client action after the
	// host applied and journaled it. It runs off the connection's read loop.
	HandleClientAction(ctx context.Context, principal Principal, channel string, action ahptypes.StateAction, emit Emitter)
}

// CreateSessionRequest is a validated createSession command.
type CreateSessionRequest struct {
	Session            string
	Provider           string
	WorkingDirectories []string
	Config             map[string]json.RawMessage
}

// CreatedSession is a new session's initial state and the states of the chats it lists.
type CreatedSession struct {
	State ahptypes.SessionState
	Chats map[string]ahptypes.ChatState
}

// Emitter delivers a backend's actions for one user through the host, which
// reduces, journals and fans them out.
type Emitter interface {
	Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error
}

// Rejection rejects a client action with a reason the client sees.
type Rejection struct {
	Reason string
}

func (r *Rejection) Error() string { return "action rejected: " + r.Reason }

// Registry holds the session backends in the order their agents are listed.
type Registry struct {
	backends []SessionBackend
}

// NewRegistry builds a registry. Backend names must be unique.
func NewRegistry(backends ...SessionBackend) (*Registry, error) {
	seen := map[string]bool{}
	for _, backend := range backends {
		if seen[backend.Name()] {
			return nil, fmt.Errorf("duplicate AHP session backend %q", backend.Name())
		}
		seen[backend.Name()] = true
	}
	return &Registry{backends: backends}, nil
}

// Agents lists every backend's agents for the principal.
func (r *Registry) Agents(ctx context.Context, principal Principal) ([]ahptypes.AgentInfo, error) {
	agents := []ahptypes.AgentInfo{}
	if r == nil {
		return agents, nil
	}
	for _, backend := range r.backends {
		offered, err := backend.Agents(ctx, principal)
		if err != nil {
			return nil, err
		}
		agents = append(agents, offered...)
	}
	return agents, nil
}

// ForProvider returns the backend offering the provider to the principal. An
// empty provider selects the first agent offered.
func (r *Registry) ForProvider(ctx context.Context, principal Principal, provider string) (SessionBackend, string, error) {
	if r == nil {
		return nil, "", fmt.Errorf("no session backends are configured")
	}
	for _, backend := range r.backends {
		offered, err := backend.Agents(ctx, principal)
		if err != nil {
			return nil, "", err
		}
		for _, agent := range offered {
			if provider == "" || agent.Provider == provider {
				return backend, agent.Provider, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no agent provider %q is available", provider)
}

// Named returns the backend with the given name.
func (r *Registry) Named(name string) (SessionBackend, bool) {
	if r == nil {
		return nil, false
	}
	for _, backend := range r.backends {
		if backend.Name() == name {
			return backend, true
		}
	}
	return nil, false
}
