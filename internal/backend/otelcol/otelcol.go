// SPDX-License-Identifier: GPL-3.0-or-later

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
	"context"
	"errors"

	"github.com/tagwright/bilgeline/internal/backend"
)

// Backend is the otelcol implementation of backend.Backend. It is safe to reuse
// across renders and holds only deployment-level knobs (checkpoint directory,
// health endpoint) that are constant for a given collector deployment, never
// per-Spec state.
type Backend struct {
	// fileStorageDir is the on-disk directory the file_storage extension keeps
	// filelog read offsets in. It must be a durable path on a volume the
	// collector deployment owns, so a reload or restart resumes tailing from the
	// right offset rather than re-reading or skipping.
	fileStorageDir string

	// healthCheckEndpoint is where the health_check extension listens, used by the
	// apply chunk to probe the collector after a reload.
	healthCheckEndpoint string
}

// Defaults for a Backend's deployment knobs.
const (
	// DefaultFileStorageDir is the file_storage checkpoint directory when
	// unset. The extension is configured to create it if absent.
	DefaultFileStorageDir = "/var/lib/otelcol/file_storage"

	// DefaultHealthCheckEndpoint is where health_check listens when unset. The
	// wildcard host lets the apply chunk probe it from another container.
	DefaultHealthCheckEndpoint = "0.0.0.0:13133"
)

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

// New constructs an otelcol Backend with the given options applied over the
// deployment defaults.
func New(opts ...Option) *Backend {
	b := &Backend{
		fileStorageDir:      DefaultFileStorageDir,
		healthCheckEndpoint: DefaultHealthCheckEndpoint,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Name identifies this backend.
func (b *Backend) Name() string { return "otelcol" }

// errNotImplemented marks the methods this chunk deliberately leaves for the
// daemon/apply chunk.
var errNotImplemented = errors.New("otelcol: not implemented in this chunk")

// Apply is implemented in the daemon/apply chunk. It will write the rendered
// config to the shared path and SIGHUP (escalating to restart) the collector.
func (b *Backend) Apply(ctx context.Context, cfg backend.RenderedConfig) (backend.ApplyResult, error) {
	return backend.ApplyResult{}, errNotImplemented
}

// Healthy is implemented in the daemon/apply chunk. It will probe the collector's
// health_check extension.
func (b *Backend) Healthy(ctx context.Context) error {
	return errNotImplemented
}

// Compile-time assertion that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)
