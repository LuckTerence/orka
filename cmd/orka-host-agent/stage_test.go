/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageTokenCopiesAndReplacesTheToken(t *testing.T) {
	from := writeFile(t, "token", testToken)
	to := filepath.Join(t.TempDir(), "token")
	for range 2 {
		// The second run replaces a token the host has not consumed yet.
		if err := stageToken([]string{"--from", from, "--to", to}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(to)
		if err != nil || string(got) != testToken {
			t.Fatalf("staged token = %q, %v", got, err)
		}
		info, err := os.Stat(to)
		if err != nil || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("staged token mode = %v, %v; want read-only", info.Mode(), err)
		}
	}
}

func TestStageTokenRefusesAMissingOrEmptyToken(t *testing.T) {
	to := filepath.Join(t.TempDir(), "token")
	for name, args := range map[string][]string{
		"no flags":     nil,
		"missing file": {"--from", filepath.Join(t.TempDir(), "missing"), "--to", to},
		"empty token":  {"--from", writeFile(t, "token", " \n"), "--to", to},
		"oversized":    {"--from", writeFile(t, "token", strings.Repeat("a", maxTokenBytes+1)), "--to", to},
	} {
		err := stageToken(args)
		if err == nil {
			t.Errorf("%s: staged", name)
		} else if strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: error leaked the token: %v", name, err)
		}
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatalf("a refused stage wrote %s: %v", to, err)
	}
}
