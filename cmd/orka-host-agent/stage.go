/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// stageToken copies the connection token from its Secret mount into the
// memory-backed volume only the host mounts, as the uid it runs with. It runs
// as an init container from Orka's own image, so host images need no tools.
func stageToken(args []string) error {
	flags := flag.NewFlagSet("stage-token", flag.ContinueOnError)
	from := flags.String("from", "", "the connection token file to copy")
	to := flags.String("to", "", "where to write it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("--from and --to are required")
	}
	source, err := os.Open(*from)
	if err != nil {
		return fmt.Errorf("open %s: %w", *from, err)
	}
	defer func() { _ = source.Close() }()
	token, err := io.ReadAll(io.LimitReader(source, maxTokenBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", *from, err)
	}
	if len(token) > maxTokenBytes || strings.TrimSpace(string(token)) == "" {
		return fmt.Errorf("%s does not hold a connection token", *from)
	}
	// A token the host has not consumed yet is read-only; replace it.
	if err := os.Remove(*to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", *to, err)
	}
	target, err := os.OpenFile(*to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440)
	if err != nil {
		return fmt.Errorf("create %s: %w", *to, err)
	}
	if _, err := target.Write(token); err != nil {
		_ = target.Close()
		return fmt.Errorf("write %s: %w", *to, err)
	}
	return target.Close()
}
