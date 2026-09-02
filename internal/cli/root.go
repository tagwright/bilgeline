// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package cli builds bilgeline's Cobra command tree and is the CLI's only entry
// point: cmd/bilgeline/main.go calls Execute and does nothing else.
//
// Cobra is used for two properties: it generates shell completion for every
// command and flag it knows about (the auto-added "completion" subcommand), and
// it derives --help text straight from the command and flag definitions below,
// so help never drifts out of sync with the actual options.
//
// The command tree:
//
//	bilgeline daemon    run the event-driven control loop (the container's
//	                    default command)
//	bilgeline generate  dry-run: print the collector config bilgeline would
//	                    produce, without writing or signalling (alias: render)
//	bilgeline validate  check bilgeline.yml and report every label/config
//	                    diagnostic, with a nonzero exit on any error
//	bilgeline status    show the discovered services, the backend, and its
//	                    health
//	bilgeline version   print the build version
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// cfgFile and logLevel back the root command's persistent flags. Cobra commands
// are small, long-lived singletons, so package-level vars bound by pflag are the
// idiomatic way to thread persistent flags through to every subcommand's RunE.
var (
	cfgFile  string
	logLevel string
)

// DefaultConfigPath is where bilgeline looks for its config when --config is not
// given. The file is optional: env-only operation with no file works too.
const DefaultConfigPath = "/etc/bilgeline/bilgeline.yml"

// Execute builds the command tree and runs it against os.Args. version is the
// build-time version string (see cmd/bilgeline/main.go), reported by both
// "bilgeline version" and the auto-generated "bilgeline --version" flag.
func Execute(version string) error {
	return newRootCmd(version).Execute()
}

// newRootCmd builds the root "bilgeline" command and attaches every subcommand.
// Cobra adds its own "completion" and "help" subcommands, and (because Version is
// set) a "--version" flag, automatically.
func newRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "bilgeline",
		Short: "bilgeline generates OpenTelemetry Collector config from container labels.",
		Long: `bilgeline reads bilgeline.* labels off running containers and generates an
OpenTelemetry Collector configuration that routes each service's logs to a
named destination. It does not run the collector: you deploy your own, and
bilgeline keeps its config in sync and signals it to reload.

The daemon is the normal way to run it; generate and validate are the
debugging commands for "show me what it would produce" and "check my labels".`,
		Version:      version,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("bilgeline {{.Version}}\n")

	root.PersistentFlags().StringVar(&cfgFile, "config", DefaultConfigPath, "path to bilgeline.yml")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newGenerateCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newStatusCmd())

	return root
}

// newVersionCmd prints the build version. The auto-generated "bilgeline
// --version" flag is templated to match it.
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

// newLogger builds a slog.Logger from the --log-level persistent flag, writing
// to stderr so stdout stays free for command output meant to be captured or
// piped (the generated YAML from "generate", especially).
func newLogger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}
