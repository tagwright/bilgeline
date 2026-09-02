// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/daemon"
	"github.com/tagwright/bilgeline/internal/discovery"
	"github.com/tagwright/core/runtime"
)

// newGenerateCmd builds "bilgeline generate" (alias "render"), the dry-run: it
// discovers the current containers, assembles the spec, renders the backend
// config, and prints it to stdout. It writes nothing and signals nothing. This is
// the "show me what bilgeline would produce" debugging command.
func newGenerateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "generate",
		Aliases: []string{"render"},
		Short:   "Print the collector config bilgeline would produce (dry run)",
		Long: `generate discovers the containers currently opted in via their bilgeline.*
labels, assembles the routing spec, renders the backend configuration, and
prints it to stdout. It is a pure dry run: no file is written and the collector
is not signalled.

Use it to preview exactly what the daemon would apply, and to debug labels and
destinations without touching a running collector.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newLogger()
			if err != nil {
				return err
			}

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}

			rt, err := daemon.BuildRuntime()
			if err != nil {
				return err
			}
			defer func() { _ = rt.Close() }()

			selfID := daemon.ResolveSelfID(cmd.Context(), rt, logger)
			be := daemon.BuildBackend(rt, cfg)

			data, err := generateConfig(cmd.Context(), rt, be, cfg, selfID)
			if err != nil {
				return err
			}

			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

// generateConfig is the testable core of "generate": it discovers, assembles the
// spec, and renders it, returning the backend config bytes. It takes the runtime
// and backend as parameters so a test can inject fakes and exercise the whole
// path without a socket or a live collector. Diagnostics are intentionally
// ignored here: generate is about the produced config, and "validate" is the
// command that reports diagnostics.
func generateConfig(ctx context.Context, rt runtime.Runtime, be backend.Backend, cfg *config.Config, selfID string) ([]byte, error) {
	services, _, err := discovery.Discover(ctx, rt, cfg, selfID)
	if err != nil {
		return nil, fmt.Errorf("generate: discover: %w", err)
	}

	spec := daemon.AssembleSpec(services, cfg)

	rendered, err := be.Render(spec)
	if err != nil {
		return nil, fmt.Errorf("generate: render: %w", err)
	}
	return ensureTrailingNewline(rendered.Data), nil
}

// ensureTrailingNewline makes the printed config end in exactly one newline, so
// the dry-run output sits cleanly in a terminal and in a piped file alike.
func ensureTrailingNewline(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	if data[len(data)-1] == '\n' {
		return data
	}
	return append(data, '\n')
}
