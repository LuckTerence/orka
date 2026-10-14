//go:build !windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/nativesession"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

type migrationState struct {
	Direction         string `json:"direction"`
	Server            string `json:"server"`
	Namespace         string `json:"namespace"`
	Session           string `json:"session"`
	Agent             string `json:"agent"`
	StateDir          string `json:"stateDir"`
	ProviderSessionID string `json:"providerSessionID,omitempty"`
	CWD               string `json:"cwd,omitempty"`
	OperationID       string `json:"operationID"`
	Data              []byte `json:"data"`
}

// migrationFormat returns the native session format of a local agent.
func migrationFormat(agent string) (nativesession.Format, error) {
	format, ok := nativesession.For(agent)
	if !ok {
		return nil, fmt.Errorf("--agent must be one of: %s", strings.Join(nativesession.Kinds(), ", "))
	}
	return format, nil
}

// Native migration requires an explicitly selected API endpoint. Kubernetes
// discovery identifies resources, not the application entrusted with history.
func newMigrationClient(cmd *cobra.Command) (*client.Client, string, func(), error) {
	server, _ := cmd.Flags().GetString("server")
	if server == "" {
		server = loadConfig().Server
	}
	if server == "" {
		return nil, "", nil, errors.New("native migration requires an explicit --server or configured API endpoint; automatic Service discovery cannot establish Orka identity")
	}
	c := newClientFromCmdWithServer(cmd, server)
	if strings.TrimSpace(c.Token) == "" && c.TxnToken == "" {
		return nil, "", nil, errors.New("native migration requires API authentication: provide an Orka bearer token or transaction token; Kubernetes exec and certificate credentials are not API credentials")
	}
	return c, server, func() {}, nil
}

func migrationDir(name string, create bool) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", err
		}
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if create {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("migration destination must be a private directory (0700)")
		}
	}
	return dir, nil
}

func privateMigrationJournal(name, home string) (string, error) {
	dir, err := migrationDir(name, true)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("migration journal must be a private directory (0700)")
	}
	for _, pair := range [][2]string{{home, dir}, {dir, home}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("migration journal and agent state directory must not overlap")
		}
	}
	return dir, nil
}

func readMigrationState(journal string, expected migrationState, maxBytes ...int) (*migrationState, error) {
	limit := harnessv2.DefaultMaxNativeSessionBytes
	if len(maxBytes) > 0 {
		var err error
		limit, err = harnessv2.NormalizeNativeSessionMaxBytes(maxBytes[0])
		if err != nil {
			return nil, err
		}
	}
	journalLimit := harnessv2.NativeSessionJSONLimit(limit)
	file, err := os.OpenFile(filepath.Join(journal, "request.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(journalLimit) {
		return nil, errors.New("invalid migration journal")
	}
	var saved migrationState
	decoder := json.NewDecoder(io.LimitReader(file, int64(journalLimit)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, errors.New("invalid migration journal")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid migration journal tail")
	}
	if !bytes.Equal(identityJSON(saved), identityJSON(expected)) {
		return nil, errors.New("migration journal belongs to another operation or target")
	}
	if saved.OperationID == "" || len(saved.Data) == 0 || len(saved.Data) > limit {
		return nil, errors.New("incomplete migration journal")
	}
	return &saved, nil
}

// Compare only fixed operation identity; Data is deliberately retained from the
// first request so an uncertain retry cannot capture a newer conversation.
func identityJSON(state migrationState) []byte {
	state.Data, state.OperationID = nil, ""
	data, _ := json.Marshal(state)
	return data
}

func persistMigrationState(journal string, state migrationState) error {
	return publishMigrationJSON(journal, "request.json", state)
}

func publishMigrationJSON(journal, target string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(journal, ".request-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	// Publish without replacing another invocation's immutable request.
	if err := os.Link(name, filepath.Join(journal, target)); err != nil {
		if errors.Is(err, os.ErrExist) {
			old, readErr := os.ReadFile(filepath.Join(journal, target))
			if readErr == nil && bytes.Equal(old, data) {
				return nil
			}
		}
		return err
	}
	dir, err := os.Open(journal)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func migrationOperationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "native-" + hex.EncodeToString(random[:]), nil
}

func newSessionMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Move agent conversations between local Codex or OpenCode and Orka Sessions"}
	cmd.PersistentFlags().Int("max-bundle-bytes", harnessv2.DefaultMaxNativeSessionBytes, "Maximum encoded native bundle bytes (1..67108864); ORKA_NATIVE_SESSION_MAX_BYTES supplies the default")
	cmd.AddCommand(newSessionMigrateImportCmd(), newSessionMigrateExportCmd())
	return cmd
}

func migrationMaxBundleBytes(cmd *cobra.Command) (int, error) {
	limit := harnessv2.DefaultMaxNativeSessionBytes
	if flag := cmd.Flags().Lookup("max-bundle-bytes"); flag != nil && flag.Changed {
		value, err := cmd.Flags().GetInt("max-bundle-bytes")
		if err != nil {
			return 0, err
		}
		limit = value
	} else if raw, present := os.LookupEnv("ORKA_NATIVE_SESSION_MAX_BYTES"); present && strings.TrimSpace(raw) != "" {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return 0, errors.New("ORKA_NATIVE_SESSION_MAX_BYTES must be an integer")
		}
		limit = value
	}
	if limit < 1 {
		return 0, errors.New("native bundle limit must be positive")
	}
	return harnessv2.NormalizeNativeSessionMaxBytes(limit)
}

// migrationFlagHelp describes the --agent and --state-dir values every
// native session format accepts.
func migrationFlagHelp() (agent, stateDir string) {
	kinds := nativesession.Kinds()
	dirs := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		format, _ := nativesession.For(kind)
		dirs = append(dirs, format.Local().StateVar+" for "+kind)
	}
	return "Local agent: " + strings.Join(kinds, " or "), "agent state directory: " + strings.Join(dirs, ", ")
}

func newSessionMigrateImportCmd() *cobra.Command {
	var agent, stateDir, providerSessionID, cwd, journal string
	var stopped bool
	cmd := &cobra.Command{
		Use: "import <new-session-name>", Short: "Stage a stopped local agent conversation for its first Orka Task", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, err := migrationMaxBundleBytes(cmd)
			if err != nil {
				return err
			}
			format, err := migrationFormat(agent)
			if err != nil {
				return err
			}
			if !stopped {
				return fmt.Errorf("stop every local %s process using the state directory, then pass --source-stopped", agent)
			}
			resolvedState, err := migrationDir(stateDir, false)
			if err != nil {
				return err
			}
			resolvedCWD := ""
			if cwd != "" {
				if resolvedCWD, err = migrationDir(cwd, false); err != nil {
					return err
				}
			}
			resolvedJournal, err := privateMigrationJournal(journal, resolvedState)
			if err != nil {
				return err
			}
			c, target, cleanup, err := newMigrationClient(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			c.NativeSessionMaxBytes = limit
			expected := migrationState{
				Direction: "import", Server: target, Namespace: c.Namespace, Session: args[0],
				Agent: agent, StateDir: resolvedState, ProviderSessionID: providerSessionID, CWD: resolvedCWD,
			}
			saved, err := readMigrationState(resolvedJournal, expected, limit)
			if err != nil {
				return err
			}
			if saved == nil {
				expected.Data, err = format.Capture(cmd.Context(), nativesession.Dirs{State: resolvedState, Workspace: resolvedCWD}, providerSessionID, limit)
				if err != nil {
					return err
				}
				expected.OperationID, err = migrationOperationID()
				if err != nil {
					return err
				}
				if err := persistMigrationState(resolvedJournal, expected); err != nil {
					return fmt.Errorf("persist migration request before import: %w", err)
				}
				saved = &expected
			}
			summary, err := format.Inspect(cmd.Context(), saved.Data, limit)
			if err != nil {
				return err
			}
			receipt, err := c.ImportNativeSession(cmd.Context(), args[0], saved.OperationID, agent, saved.Data)
			if err != nil {
				return fmt.Errorf("%w; retry using the same --journal-dir", err)
			}
			if receipt.OperationID != saved.OperationID || receipt.DataDigest != summary.DataDigest || receipt.ProviderSessionID != providerSessionID ||
				receipt.SessionName != args[0] || receipt.Namespace != c.Namespace {
				return errors.New("native import receipt does not match the saved request")
			}
			if err := publishMigrationJSON(resolvedJournal, "import-receipt.json", receipt); err != nil {
				return fmt.Errorf("save import receipt: %w; retry using the same --journal-dir", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Staged %s session %s in Session %s. Submit a Task with this Session to continue; its agent must run %s.\n", agent, providerSessionID, args[0], agent)
			return err
		},
	}
	agentHelp, stateDirHelp := migrationFlagHelp()
	cmd.Flags().StringVar(&agent, "agent", "", agentHelp)
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Source "+stateDirHelp)
	cmd.Flags().StringVar(&providerSessionID, "session-id", "", "The agent's own session ID: a Codex thread UUID or an OpenCode ses_ ID")
	cmd.Flags().StringVar(&cwd, "cwd", "", "Directory the source session ran in (required for opencode)")
	cmd.Flags().StringVar(&journal, "journal-dir", "", "Private directory retained for exact retries")
	cmd.Flags().BoolVar(&stopped, "source-stopped", false, "Confirm every local agent process using the state directory has stopped")
	for _, flag := range []string{"agent", "state-dir", "session-id", "journal-dir"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newSessionMigrateExportCmd() *cobra.Command {
	var agent, stateDir, cwd, journal, binary string
	cmd := &cobra.Command{
		Use: "export <session-name>", Short: "Install a saved Orka checkpoint into a fresh local agent state directory", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, err := migrationMaxBundleBytes(cmd)
			if err != nil {
				return err
			}
			format, err := migrationFormat(agent)
			if err != nil {
				return err
			}
			local := format.Local()
			if binary == "" {
				binary = local.Binary
			}
			version, err := exec.CommandContext(cmd.Context(), binary, "--version").Output()
			if err != nil || strings.TrimSpace(string(version)) != local.Version {
				return fmt.Errorf("destination requires %s reporting version %q (--agent-bin)", local.Binary, local.Version)
			}
			resolvedState, err := migrationDir(stateDir, true)
			if err != nil {
				return err
			}
			if sqliteHome := os.Getenv("CODEX_SQLITE_HOME"); agent == "codex" && sqliteHome != "" {
				// Relative overrides depend on the resume CWD; fresh-home validation
				// covers databases in this exact absolute home only.
				resolvedSQLiteHome, sqliteErr := migrationDir(sqliteHome, false)
				if sqliteHome != strings.TrimSpace(sqliteHome) || !filepath.IsAbs(sqliteHome) || sqliteErr != nil || resolvedSQLiteHome != resolvedState {
					return errors.New("CODEX_SQLITE_HOME must be unset or an absolute path without surrounding whitespace resolving to the destination CODEX_HOME before native export")
				}
			}
			resolvedCWD, err := migrationDir(cwd, false)
			if err != nil {
				return err
			}
			resolvedJournal, err := privateMigrationJournal(journal, resolvedState)
			if err != nil {
				return err
			}
			c, target, cleanup, err := newMigrationClient(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			c.NativeSessionMaxBytes = limit
			expected := migrationState{
				Direction: "export", Server: target, Namespace: c.Namespace, Session: args[0],
				Agent: agent, StateDir: resolvedState, CWD: resolvedCWD,
			}
			saved, err := readMigrationState(resolvedJournal, expected, limit)
			if err != nil {
				return err
			}
			if saved == nil {
				response, err := c.ExportNativeSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if response.ProviderKind != agent {
					return fmt.Errorf("session %s holds a %s conversation, not %s", args[0], response.ProviderKind, agent)
				}
				summary, err := format.Inspect(cmd.Context(), response.Data, limit)
				if err != nil {
					return err
				}
				if summary.DataDigest != response.DataDigest || summary.ProviderSessionID != response.ProviderSessionID {
					return errors.New("native export identity mismatch")
				}
				expected.Data = response.Data
				expected.OperationID, err = migrationOperationID()
				if err != nil {
					return err
				}
				if err := persistMigrationState(resolvedJournal, expected); err != nil {
					return err
				}
				saved = &expected
			}
			summary, err := format.Inspect(cmd.Context(), saved.Data, limit)
			if err != nil {
				return err
			}
			// A retry after a completed install finds its receipt; formats need
			// not recognize their own earlier installation.
			if _, err := os.Lstat(filepath.Join(resolvedJournal, "export-receipt.json")); errors.Is(err, os.ErrNotExist) {
				if err := format.Install(cmd.Context(), saved.Data, nativesession.Dirs{State: resolvedState, Workspace: resolvedCWD}, resolvedJournal, limit); err != nil {
					return fmt.Errorf("install: %w; retain the journal and retry it", err)
				}
				if err := publishMigrationJSON(resolvedJournal, "export-receipt.json", summary); err != nil {
					return fmt.Errorf("save export receipt: %w; retry using the same --journal-dir", err)
				}
			} else if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Installed %s session %s in %s. Set %s to that directory, authenticate with current local credentials, then resume the session from %s.\n",
				agent, summary.ProviderSessionID, resolvedState, local.StateVar, resolvedCWD)
			return err
		},
	}
	agentHelp, stateDirHelp := migrationFlagHelp()
	cmd.Flags().StringVar(&agent, "agent", "", agentHelp)
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "Fresh private destination "+stateDirHelp)
	cmd.Flags().StringVar(&cwd, "cwd", "", "Existing destination working directory")
	cmd.Flags().StringVar(&journal, "journal-dir", "", "Private directory outside the state directory, retained for retries")
	cmd.Flags().StringVar(&binary, "agent-bin", "", "Local agent executable, which must report the release Orka carries (default: the agent's name)")
	for _, flag := range []string{"agent", "state-dir", "cwd", "journal-dir"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}
