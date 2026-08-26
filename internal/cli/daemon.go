// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tagwright/bilgeline/internal/daemon"
)

// newDaemonCmd builds "bilgeline daemon", the long-running service and the
// container's default command. daemon.Run does all of its own wiring (config,
// runtime, self-id, backend, notifier, control loop), so this command only builds
// the logger, installs a signal-driven context, and calls it.
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the bilgeline control loop",
		Long: `daemon runs bilgeline's long-running, event-driven service: it loads config,
discovers containers opted in via their bilgeline.* (or tagwright.log.*)
labels, watches the container runtime for lifecycle changes, and on each
debounced change regenerates the collector config and signals the collector to
reload it, until it receives SIGINT or SIGTERM.

This is the container's default command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newLogger()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return daemon.Run(ctx, cfgFile, logger)
		},
	}
}
