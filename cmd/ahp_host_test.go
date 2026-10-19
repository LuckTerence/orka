/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"testing"

	"github.com/orka-agents/orka/internal/api"
)

func TestValidateAHPWorkspace(t *testing.T) {
	digest := "ghcr.io/example/git@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name string
		cfg  api.AHPBackendConfig
		ok   bool
	}{
		{"disabled", api.AHPBackendConfig{}, true},
		{"pinned", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-ahp-workspaces"}, true},
		{"tag", api.AHPBackendConfig{
			WorkspaceImage: "ghcr.io/example/git:latest", WorkspaceNamespace: "orka-ahp-workspaces",
		}, false},
		{"watch namespace", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-system"}, false},
		{"control namespace", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-control"}, false},
		{"empty namespace", api.AHPBackendConfig{WorkspaceImage: digest}, false},
	} {
		err := validateAHPWorkspace(test.cfg, "orka-system", "orka-control")
		if (err == nil) != test.ok {
			t.Errorf("%s: err = %v, want ok=%v", test.name, err, test.ok)
		}
	}
}
