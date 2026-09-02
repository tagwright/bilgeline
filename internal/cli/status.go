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

// newStatusCmd builds "bilgeline status", a read-only snapshot: the services
// currently discovered, the resolved backend and collector identity, and the
// backend's health probe. It changes nothing.
func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show discovered services, the backend, and its health",
		Long: `status discovers the containers currently opted in, prints each routed
service and its destinations, names the backend and the configured collector,
and reports the backend's health probe. It is read-only: nothing is written and
the collector is not signalled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newLogger()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

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

			services, _, err := discovery.Discover(cmd.Context(), rt, cfg, selfID)
			if err != nil {
				return fmt.Errorf("status: discover: %w", err)
			}

			fmt.Fprintf(out, "backend:   %s\n", be.Name())
			collector := cfg.Collector
			if collector == "" {
				collector = "(marker-discovered: bilgeline.collector label)"
			}
			fmt.Fprintf(out, "collector: %s\n", collector)

			health := "healthy"
			if herr := be.Healthy(cmd.Context()); herr != nil {
				health = "unavailable: " + herr.Error()
			}
			fmt.Fprintf(out, "health:    %s\n", health)

			fmt.Fprintf(out, "services:  %d routed\n", len(services))
			for _, svc := range services {
				dests := svc.DestinationNames()
				where := "none (dropped at source)"
				if len(dests) > 0 {
					where = fmt.Sprintf("%v", dests)
				}
				fmt.Fprintf(out, "  - %s (%s) -> %s\n", svc.Name, shortID(svc.ContainerID), where)
			}

			return nil
		},
	}
}

// shortID trims a full container id to the conventional 12-char short form for
// display.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
