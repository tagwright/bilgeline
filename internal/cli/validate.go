// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/daemon"
	"github.com/tagwright/bilgeline/internal/discovery"
)

// newValidateCmd builds "bilgeline validate", the "check my labels and config"
// command. It loads and validates bilgeline.yml and, unless --config-only,
// discovers the current containers and reports every diagnostic (errors and
// warnings). It exits nonzero if config validation fails or any error-severity
// diagnostic is found, so it slots into CI or a pre-deploy check.
func newValidateCmd() *cobra.Command {
	var configOnly bool

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate bilgeline.yml and report label/config diagnostics",
		Long: `validate loads bilgeline.yml, checks it for internal consistency, and (unless
--config-only) discovers the containers currently opted in and reports every
diagnostic their bilgeline.* labels produce.

It exits nonzero if the config fails validation or any container fails with an
error-severity diagnostic (a bad enum, an unknown destination, an invalid
regex). Warnings are reported but do not fail the check.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newLogger()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			// config.Load already runs Validate; a config error surfaces here.
			cfg, err := config.Load(cfgFile)
			if err != nil {
				fmt.Fprintf(out, "config: INVALID\n%v\n", err)
				return errValidationFailed
			}
			fmt.Fprintln(out, "config: OK")

			if configOnly {
				return nil
			}

			rt, err := daemon.BuildRuntime()
			if err != nil {
				return err
			}
			defer func() { _ = rt.Close() }()

			selfID := daemon.ResolveSelfID(cmd.Context(), rt, logger)

			services, diags, err := discovery.Discover(cmd.Context(), rt, cfg, selfID)
			if err != nil {
				return fmt.Errorf("validate: discover: %w", err)
			}

			errCount := reportDiagnostics(cmd, diags)
			fmt.Fprintf(out, "services: %d routed, diagnostics: %d (%d error)\n",
				len(services), len(diags), errCount)

			if errCount > 0 {
				return errValidationFailed
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&configOnly, "config-only", false, "validate the config file only; do not discover containers")
	return cmd
}

// reportDiagnostics prints each diagnostic and returns how many were
// error-severity. Output goes to stdout so it is capturable; the caller decides
// the exit code from the returned count.
func reportDiagnostics(cmd *cobra.Command, diags []discovery.Diagnostic) int {
	out := cmd.OutOrStdout()
	errCount := 0
	for _, d := range diags {
		if d.Severity == discovery.SeverityError {
			errCount++
		}
		fmt.Fprintf(out, "%s: %s: %s\n", d.Severity, d.ContainerName, d.Message)
	}
	return errCount
}

// errValidationFailed is returned by "validate" to drive a nonzero process exit
// without Cobra reprinting a usage string (SilenceUsage is set on the root). Its
// message is terse because the specific findings were already printed.
var errValidationFailed = fmt.Errorf("validation failed")
