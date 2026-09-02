// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package otelcol is the OpenTelemetry Collector backend: it turns bilgeline's
// runtime-neutral backend.Spec into a valid, static otelcol-contrib YAML config
// and (in a later chunk) makes that config live against a running collector.
//
// This chunk implements Render and Name fully. Apply and Healthy are stubs that
// report "not implemented" so the type satisfies backend.Backend and the tree
// builds; the daemon/apply chunk fills them in.
//
// The whole translation from bilgeline's nouns (services, destinations, routes)
// into collector vocabulary (receivers, operators, processors, connectors,
// exporters, pipelines) lives in this package and nowhere else. That is the seam
// discipline backend.Backend exists to keep: a Fluent Bit backend would emit its
// own format here without the core loop caring.
//
// Config shape, per the verified design (see the bilgeline arch research brief,
// section 5, and the frozen Phase 1 decisions):
//
//   - One filelog receiver per distinct processing signature. Its include list is
//     the EXPLICIT per-container json-file paths of that signature's members, so
//     the collector never opens a file for a container bilgeline did not route.
//   - A container operator (format docker) plus a regex_parser that recovers the
//     64-hex container id from the file path, then the group's parse, recombine,
//     timestamp, and severity operators, then any profile raw operators verbatim.
//   - A memory_limiter, a single transform that stamps each service's identity and
//     attributes keyed on the recovered container id (plus a routing-key resource
//     attribute holding the service's sorted destination set), and a filter that
//     realizes per-service stream, level-floor, and drop rules.
//   - A routing connector that fans the one ingest pipeline out to one pipeline per
//     DISTINCT destination set, each exporting to that set's exporters. A service
//     routed nowhere carries no routing key and is dropped at the connector.
//   - file_storage (filelog checkpointing) and health_check extensions, always on.
package otelcol

import (
	"time"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/core/runtime"
)

// Backend is the otelcol implementation of backend.Backend. It is safe to reuse
// across renders and holds only deployment-level knobs (checkpoint directory,
// health endpoint, and the apply-path wiring) that are constant for a given
// collector deployment, never per-Spec state.
//
// Render depends on none of the apply-path fields: a zero-value Backend (or one
// built with New and no apply options) renders correctly. The runtime, shared
// path, and collector identity are only consulted by Apply and Healthy.
type Backend struct {
	// fileStorageDir is the on-disk directory the file_storage extension keeps
	// filelog read offsets in. It must be a durable path on a volume the
	// collector deployment owns, so a reload or restart resumes tailing from the
	// right offset rather than re-reading or skipping.
	fileStorageDir string

	// healthCheckEndpoint is where the health_check extension listens, used by the
	// apply chunk to probe the collector after a reload.
	healthCheckEndpoint string

	// rt is the container runtime the apply path drives to find, signal, and
	// recover the collector. Nil in a render-only Backend; Apply and Healthy
	// return a descriptive error when it is unset.
	rt runtime.Runtime

	// sharedConfigPath is the on-disk path Apply writes the rendered config to,
	// on the volume shared read-only into the collector. It is also the path the
	// mount guard checks the collector actually mounts before any signal.
	sharedConfigPath string

	// collectorName is the fallback collector container identity (cfg.Collector),
	// used only when no container carries the bilgeline.collector marker.
	collectorName string

	// collectorHealthURL is an optional collector health endpoint. When set,
	// Healthy and the wedge-recovery path may additionally GET it as a secondary,
	// network-dependent confirmation. It is never the primary wedge signal and a
	// restart is never triggered solely because it was unreachable. Empty (off)
	// by default.
	collectorHealthURL string

	// reloadWait is the bounded window Apply polls the collector's container
	// state after a SIGHUP (and again after a restart) before concluding the
	// reload took. Defaults to DefaultReloadWait.
	reloadWait time.Duration
}

// Defaults for a Backend's deployment knobs.
const (
	// DefaultFileStorageDir is the file_storage checkpoint directory when
	// unset. The extension is configured to create it if absent.
	DefaultFileStorageDir = "/var/lib/otelcol/file_storage"

	// DefaultHealthCheckEndpoint is where health_check listens when unset. The
	// wildcard host lets the apply chunk probe it from another container.
	DefaultHealthCheckEndpoint = "0.0.0.0:13133"

	// DefaultReloadWait is the bounded window Apply polls the collector's
	// container state after a SIGHUP, and again after a restart, before deciding
	// the reload settled. A few seconds is enough to catch the 2025 reload crash
	// bugs, which manifest as the collector process dying (the container exits)
	// shortly after the SIGHUP.
	DefaultReloadWait = 5 * time.Second
)

// reloadPollInterval is how often the wedge-recovery loop re-inspects the
// collector's state within the reloadWait window.
const reloadPollInterval = 250 * time.Millisecond

// Name is the backend attribute name holding the routing key: a resource
// attribute whose value is a service's canonical, sorted destination set, matched
// by the routing connector to fan logs out to the right per-set pipeline.
const routingKeyAttr = "bilgeline.routing_key"

// Option configures a Backend at construction.
type Option func(*Backend)

// WithFileStorageDir overrides the file_storage checkpoint directory. Pass a
// durable path the collector deployment mounts and owns.
func WithFileStorageDir(dir string) Option {
	return func(b *Backend) {
		if dir != "" {
			b.fileStorageDir = dir
		}
	}
}

// WithHealthCheckEndpoint overrides the health_check listen endpoint.
func WithHealthCheckEndpoint(endpoint string) Option {
	return func(b *Backend) {
		if endpoint != "" {
			b.healthCheckEndpoint = endpoint
		}
	}
}

// WithRuntime injects the container runtime the apply path drives to find,
// SIGHUP, and recover the collector. Required for Apply and Healthy; Render does
// not use it.
func WithRuntime(rt runtime.Runtime) Option {
	return func(b *Backend) {
		b.rt = rt
	}
}

// WithSharedConfigPath sets the on-disk path Apply writes the rendered config
// to and that the mount guard requires the collector to mount.
func WithSharedConfigPath(path string) Option {
	return func(b *Backend) {
		if path != "" {
			b.sharedConfigPath = path
		}
	}
}

// WithCollectorName sets the fallback collector container identity
// (cfg.Collector), used only when no container carries the bilgeline.collector
// marker.
func WithCollectorName(name string) Option {
	return func(b *Backend) {
		b.collectorName = name
	}
}

// WithCollectorHealthURL sets an optional collector health URL used as a
// secondary, network-dependent confirmation by Healthy and the reload path. It
// is off (empty) by default and never drives a restart on its own.
func WithCollectorHealthURL(url string) Option {
	return func(b *Backend) {
		b.collectorHealthURL = url
	}
}

// WithReloadWait overrides the bounded window Apply polls the collector's
// container state after a SIGHUP (and after a restart). A non-positive value is
// ignored, leaving DefaultReloadWait in place.
func WithReloadWait(d time.Duration) Option {
	return func(b *Backend) {
		if d > 0 {
			b.reloadWait = d
		}
	}
}

// New constructs an otelcol Backend with the given options applied over the
// deployment defaults.
func New(opts ...Option) *Backend {
	b := &Backend{
		fileStorageDir:      DefaultFileStorageDir,
		healthCheckEndpoint: DefaultHealthCheckEndpoint,
		reloadWait:          DefaultReloadWait,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Name identifies this backend.
func (b *Backend) Name() string { return "otelcol" }

// Apply and Healthy are implemented in apply.go.

// Compile-time assertion that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)
