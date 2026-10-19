/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/executionmode"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

// newAHPHost builds the AHP host behind --enable-ahp-api. The API server
// registers the session backend once its handlers exist. admit refuses the
// tokens of owners the API would no longer sign in.
func newAHPHost(s store.AHPStore, maxTTL time.Duration, admit func(ahphost.Principal) error) (*ahphost.Host, error) {
	backends, err := ahphost.NewRegistry()
	if err != nil {
		return nil, err
	}
	tokens := &ahphost.TokenService{Store: s, MaxTTL: maxTTL, Admit: admit}
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

var ahpWorkspaceImageDigest = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)

// validateAHPWorkspace checks the workspace settings before anything starts.
func validateAHPWorkspace(cfg api.AHPBackendConfig, watchNamespace, controlNamespace string) error {
	if cfg.WorkspaceImage == "" {
		return nil
	}
	if !ahpWorkspaceImageDigest.MatchString(cfg.WorkspaceImage) {
		return fmt.Errorf("--ahp-workspace-image must be pinned by digest")
	}
	switch strings.TrimSpace(cfg.WorkspaceNamespace) {
	case "", watchNamespace, controlNamespace:
		return fmt.Errorf("--ahp-workspace-namespace must name a namespace of its own")
	}
	return nil
}
