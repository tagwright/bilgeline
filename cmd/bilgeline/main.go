// SPDX-License-Identifier: GPL-3.0-or-later

// Command bilgeline is the label-driven log-routing companion for Docker and
// Podman: it generates OpenTelemetry Collector configuration from bilgeline.*
// labels and drives a user-deployed collector. This file is the thin main:
// it builds the version string and hands control to internal/cli, which owns the
// whole command tree.
package main

import (
	"os"

	"github.com/tagwright/bilgeline/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// It defaults to the tree's current beta.
var version = "00.01.00b1"

func main() {
	if err := cli.Execute(version); err != nil {
		os.Exit(1)
	}
}
