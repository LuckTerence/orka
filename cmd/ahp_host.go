/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"time"

	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/executionmode"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

// newAHPHost builds the AHP host behind --enable-ahp-api. The API server
// registers the session backend once its handlers exist.
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

// ahpBackendFor returns the ACP session backend configuration when the AHP
// host is on. The backend runs turns through the harness-v2 ACP dispatcher.
func ahpBackendFor(host *ahphost.Host, mode executionmode.Mode, cfg api.AHPBackendConfig) *api.AHPBackendConfig {
	if host == nil || mode != executionmode.HarnessV2 {
		return nil
	}
	return &cfg
}
