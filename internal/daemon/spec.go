// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/discovery"

	"github.com/tagwright/beacon"
)

// AssembleSpec is the config-to-backend bridge: it turns the services discovery
// resolved plus the loaded config into the runtime-neutral backend.Spec the
// backend renders. It is pure (no I/O, no socket) so the whole spec assembly is
// unit-testable.
//
// Spec.Services is discovery's output verbatim (already sorted by ContainerID by
// Discover, which keeps Spec.Hash stable across passes). Spec.Destinations is the
// UNION of the destination names the services actually route to, each looked up
// in cfg.Destinations and flattened into a single backend.Destination. Only
// referenced destinations are carried, so an unused destination in bilgeline.yml
// never widens the spec or churns the hash.
//
// The reserved "debug" destination is intentionally omitted even when a service
// routes to it: the renderer synthesizes debug itself, and it carries no config
// entry to flatten. A service routed nowhere (the "none" sentinel) contributes no
// destination names, so it never pulls a sink into the union.
func AssembleSpec(services []backend.ServiceSpec, cfg *config.Config) backend.Spec {
	dests := map[string]backend.Destination{}
	for _, svc := range services {
		for _, name := range svc.DestinationNames() {
			if name == debugDestinationName {
				// The renderer synthesizes debug; there is no config entry.
				continue
			}
			if _, done := dests[name]; done {
				continue
			}
			d, ok := cfg.Destinations[name]
			if !ok {
				// Discovery validated every route against the config, so an
				// unknown non-debug destination cannot reach here. If one somehow
				// did, leaving it out lets the renderer surface the mismatch
				// rather than emitting a half-built exporter.
				continue
			}
			dests[name] = flattenDestination(name, d)
		}
	}

	return backend.Spec{
		Services:     services,
		Destinations: dests,
	}
}

// debugDestinationName is the reserved, always-valid debug sink the renderer
// synthesizes. Mirrors discovery's own constant; duplicated here to keep the
// packages decoupled.
const debugDestinationName = "debug"

// flattenDestination collapses a config.Destination into the runtime-neutral
// backend.Destination the renderer copies verbatim into an exporter. The typed
// config fields become fixed keys in the neutral Settings map:
//
//	endpoint  <- Destination.Endpoint  (network sinks)
//	path      <- Destination.Path      (file sink)
//	headers   <- Destination.Headers   (network sinks, when non-empty)
//
// and the inline Settings passthrough (compression, tls, retry tuning, and so
// on) is copied in underneath those keys, so a typed field wins over an inline
// key of the same name.
//
// S1: string values are copied byte-for-byte. Any ${env:VAR} reference passes
// through untouched, never resolved here. bilgeline never reads a secret; the
// user provisions the matching env var on the collector container.
func flattenDestination(name string, d config.Destination) backend.Destination {
	settings := make(map[string]any, len(d.Settings)+3)

	// Inline passthrough first so the typed fields below take precedence on a
	// key collision.
	for k, v := range d.Settings {
		settings[k] = v
	}

	if d.Endpoint != "" {
		settings["endpoint"] = d.Endpoint
	}
	if d.Path != "" {
		settings["path"] = d.Path
	}
	if len(d.Headers) > 0 {
		headers := make(map[string]string, len(d.Headers))
		for k, v := range d.Headers {
			headers[k] = v
		}
		settings["headers"] = headers
	}

	return backend.Destination{
		Name:     name,
		Type:     d.Type,
		Settings: settings,
	}
}

// diagLevel maps a discovery Severity onto a beacon Level: an error-severity
// diagnostic (a container skipped for a validation fault) alerts at LevelError, a
// warning at LevelWarning. It is the single place the two severity vocabularies
// meet, kept pure so the mapping is unit-testable.
func diagLevel(sev discovery.Severity) beacon.Level {
	switch sev {
	case discovery.SeverityError:
		return beacon.LevelError
	default:
		return beacon.LevelWarning
	}
}
