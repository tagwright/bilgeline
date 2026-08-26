// SPDX-License-Identifier: GPL-3.0-or-later

// Command bilgeline is the label-driven log-routing companion for Docker and
// Podman: it generates OpenTelemetry Collector configuration from bilgeline.*
// labels and drives a user-deployed collector. This is a scaffold. The
// discovery, config generation, and reload logic are not implemented yet.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tagwright/beacon"
	"github.com/tagwright/core/runtime"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// It defaults to the tree's current beta.
var version = "00.01.00b1"

// defaultDockerSocket is the conventional Docker API socket path, used by the
// daemon stub until real config-driven socket resolution lands.
const defaultDockerSocket = "/var/run/docker.sock"

func main() {
	if err := newRootCmd(version).Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the root "bilgeline" command and attaches its
// subcommands. Cobra adds its own "completion" and "help" subcommands, and
// (because Version is set) a "--version" flag, automatically.
func newRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "bilgeline",
		Short: "bilgeline generates OpenTelemetry Collector config from container labels.",
		Long: `bilgeline reads bilgeline.* labels off running containers and generates an
OpenTelemetry Collector configuration that routes each service's logs to a
named destination. It does not run the collector: you deploy your own, and
bilgeline keeps its config in sync and signals it to reload.

This is a scaffold. The daemon is not implemented yet.`,
		Version:      version,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("bilgeline {{.Version}}\n")

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newDaemonCmd())

	return root
}

// newVersionCmd prints the build version.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bilgeline version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "bilgeline %s\n", version)
			return nil
		},
	}
}

// newDaemonCmd builds "bilgeline daemon", the long-running service. This is
// a stub: it constructs the collaborators the real daemon will drive (a
// container runtime client and a beacon notifier) so the module wiring is
// proven to build and link, then returns without running a control loop.
func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the bilgeline daemon (not yet implemented)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

			// Construct the container runtime client the daemon will use to
			// enumerate containers and watch lifecycle events. Not driven yet.
			rt := runtime.NewDocker(defaultDockerSocket)
			defer func() { _ = rt.Close() }()

			// Construct the notifier the daemon will report through. The "log"
			// channel is beacon's always-on floor when nothing else is
			// configured, matching how ballast wires its notifier.
			// A nil SecretResolver is fine here: the "log" channel resolves no
			// secrets. Real destination channels will pass a resolver, the way
			// ballast wires beacon.SecretResolver over its own secret store.
			notifier, err := beacon.New(beacon.Config{
				Channels: []beacon.ChannelConfig{{Type: "log"}},
			}, nil)
			if err != nil {
				return fmt.Errorf("build notifier: %w", err)
			}
			_ = notifier

			logger.Info("bilgeline daemon: not yet implemented")
			return nil
		},
	}
}
