/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

// newAHPHost builds the AHP host behind --enable-ahp-api. No session backend
// ships yet, so the host serves the protocol foundation with no agents.
func newAHPHost(s store.AHPStore, maxTTL time.Duration) (*ahphost.Host, error) {
	backends, err := ahphost.NewRegistry()
	if err != nil {
		return nil, err
	}
	tokens := &ahphost.TokenService{Store: s, MaxTTL: maxTTL}
	host, err := ahphost.New(ahphost.Config{
		Store:    s,
		Tokens:   tokens,
		Backends: backends,
		Logger:   ctrl.Log.WithName("ahp"),
	})
	if err != nil {
		return nil, err
	}
	// Revoking a token closes every connection that uses it.
	tokens.OnRevoke = host.CloseToken
	return host, nil
}
