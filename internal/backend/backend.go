// SPDX-License-Identifier: GPL-3.0-or-later

// Package backend defines the pluggable seam bilgeline drives to turn a
// resolved routing description into a live logging backend, and the
// runtime-neutral types that cross it.
//
// The interface is deliberately phrased in bilgeline's own nouns (services,
// destinations, routes, a rendered config) and carries no OpenTelemetry
// Collector vocabulary in its method set: no receivers, exporters, pipelines,
// connectors, or OTLP terms. That discipline is what lets a second backend (a
// Fluent Bit generator, say) slot in later without a rewrite, the way ballast's
// engine.Engine hides restic behind a functional interface. The otelcol
// implementation lands in a later chunk.
//
// The types here are the FROZEN contract the discovery, renderer, and daemon
// chunks build on. Spec is what discovery produces and the daemon diffs;
// RenderedConfig is what a backend emits and bilgeline writes and diffs on
// disk; ApplyResult is what making that config live reports back for logging.
// Some fields are populated only once the renderer and daemon land; they are
// part of the contract now so those chunks need not change it.
package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Backend turns a runtime-neutral Spec into a backend-native configuration and
// makes it live. bilgeline owns discovery, debouncing, and the control loop;
// the backend owns the config format and its own reload story. In pure-generator
// mode bilgeline does NOT own the backing process lifecycle, so there is
// deliberately no Stop method: the user deploys and owns the collector, and the
// backend only nudges it.
type Backend interface {
	// Render turns a Spec into the backend-native configuration bytes. It is
	// pure: no side effects, no I/O, no process signalling. bilgeline calls
	// Render, writes the result to the shared config path, and diffs it against
	// what is already there to decide whether an Apply is even needed. The same
	// Spec must always render to byte-identical output so that diffing is
	// meaningful.
	Render(spec Spec) (RenderedConfig, error)

	// Apply makes an already-rendered config live: it writes the bytes to the
	// shared location the backing process reads and signals that process to pick
	// them up (for otelcol, a SIGHUP, escalating to a restart if the process
	// does not come back healthy). It reports through ApplyResult whether it
	// reloaded, restarted, or only wrote the file because the process was
	// absent. Implemented in a later chunk.
	Apply(ctx context.Context, cfg RenderedConfig) (ApplyResult, error)

	// Healthy reports whether the backing process is currently serving. A nil
	// error means healthy. bilgeline uses it after an Apply to decide whether a
	// reload wedged the process and a restart is warranted.
	Healthy(ctx context.Context) error

	// Name identifies the backend, e.g. "otelcol", for logging and diagnostics.
	Name() string
}

// Spec is the complete, runtime-neutral description of everything bilgeline has
// resolved to route: the enabled services and the named destinations they can
// target. It carries no backend vocabulary. Discovery produces it, the daemon
// diffs successive Specs by Hash to skip no-op churn, and the backend renders
// it.
type Spec struct {
	// Services are the routed containers, one entry per container. The daemon
	// presents them in a stable order (sorted by ContainerID) so that Hash is
	// reproducible; see Hash.
	Services []ServiceSpec

	// Destinations are the named sinks a service's routes can point at, keyed by
	// destination name. This is the runtime-neutral projection of the
	// destinations defined in bilgeline.yml: the daemon flattens each configured
	// destination (endpoint, path, headers, and any passthrough settings) into a
	// single Settings map so the backend needs no knowledge of the config schema.
	Destinations map[string]Destination
}

// Hash is a stable content hash of the Spec, used by the daemon to detect
// whether anything meaningful changed between two discovery passes and thus
// whether a re-render and Apply are needed at all.
//
// It is computed over a canonical JSON encoding of the Spec. Map keys are
// emitted in sorted order by the JSON encoder, so Destinations and the
// per-service attribute maps are order-stable on their own. Slice order is
// significant and is NOT normalized here: callers must present Services (and
// each service's Routes and other slices) in a deterministic order for the hash
// to be stable across passes. The daemon does this by sorting services by
// ContainerID and each service's destination set at build time.
func (s Spec) Hash() string {
	data, err := json.Marshal(s)
	if err != nil {
		// The Spec is composed entirely of JSON-encodable types, so a marshal
		// error is not reachable in practice; fall back to a fixed sentinel so
		// the caller still gets a usable (if constant) value rather than a panic.
		return "unhashable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Destination is the runtime-neutral form of one named sink. It is what the
// backend renders an exporter (or its own equivalent) from. Type selects the
// exporter kind; Settings carries the fully materialized, backend-ready
// settings for it (endpoint, path, headers, and anything else), already merged
// by the daemon from the bilgeline.yml destination definition.
//
// Under the S1 secrets model, Settings values may contain literal ${env:VAR}
// reference strings. bilgeline never resolves them: they are copied verbatim
// into the rendered config, and the user provisions the matching env vars on
// the backing process. The backend treats them as opaque strings.
type Destination struct {
	// Name is the destination's name, echoed for convenience so a Destination
	// value is self-describing away from its map key.
	Name string

	// Type is the sink kind, e.g. "otlphttp", "loki", "file", "debug". The
	// backend maps it to a concrete exporter.
	Type string

	// Settings is the backend-ready configuration block for this destination,
	// copied into the rendered exporter verbatim (including any ${env:VAR}
	// strings). Keys are backend-native.
	Settings map[string]any
}

// SourceKind names where a service's logs are read from. v1 routes
// SourceDockerJSONFile; SourcePodmanJournald is expressible now so the grammar
// and this contract never preclude the Podman path, even though the renderer
// does not emit it yet.
type SourceKind string

const (
	// SourceDockerJSONFile is the Docker json-file log driver, tailed by the
	// collector's filelog receiver over /var/lib/docker/containers.
	SourceDockerJSONFile SourceKind = "docker-json-file"

	// SourcePodmanJournald is the Podman journald path, read via the journald
	// receiver. Second priority, not emitted in v1.
	SourcePodmanJournald SourceKind = "podman-journald"
)

// ParseMode is how a service's log body is parsed into structured fields. The
// built-in enum values mirror the label grammar's reserved parser names; a
// profile may additionally resolve to ParseRegex with a ParsePattern.
type ParseMode string

const (
	// ParseNone passes the raw body through unparsed. Default.
	ParseNone ParseMode = "none"
	// ParseJSON parses a JSON body into attributes.
	ParseJSON ParseMode = "json"
	// ParseLogfmt parses a logfmt body into attributes.
	ParseLogfmt ParseMode = "logfmt"
	// ParseRegex parses the body with a named-group regex, supplied via
	// ParsePattern. Reached only through a profile, never a bare label.
	ParseRegex ParseMode = "regex"
)

// Stream selects which container output streams a service routes.
type Stream string

const (
	// StreamBoth routes stdout and stderr. Default.
	StreamBoth Stream = "both"
	// StreamStdout routes stdout only.
	StreamStdout Stream = "stdout"
	// StreamStderr routes stderr only.
	StreamStderr Stream = "stderr"
)

// ServiceSpec is one routed container, fully resolved: label values, profile
// contributions, and fleet-wide defaults have already been merged into flat
// data by the daemon. The renderer consumes it as plain data and turns it into
// backend operators; it performs no further resolution.
type ServiceSpec struct {
	// ContainerID is the full 64-hex container id. It keys enrichment and,
	// sorted, orders Services for a stable Spec.Hash.
	ContainerID string

	// Name is the stable service name (compose service, else container name,
	// else the bilgeline.name override). It becomes service.name on every
	// record and groups processing. Duplicates across containers are legal
	// (replicas), distinguished by ContainerID.
	Name string

	// Source is where this service's logs are read from.
	Source SourceKind

	// Routes are the named destinations this service targets. The distinct,
	// sorted set of their names is what the routing renderer groups on; use
	// DestinationNames to recover it.
	Routes []Route

	// ContainerName is the Docker/Podman container name, stamped as
	// container.name.
	ContainerName string

	// ComposeProject is the compose project, stamped as service.namespace when
	// present.
	ComposeProject string

	// ComposeService is the raw compose service label, retained for diagnostics
	// even though Name already folds it in.
	ComposeService string

	// Image is the container image reference, stamped as container.image.name.
	Image string

	// StaticAttrs are user-supplied static resource attributes (from
	// bilgeline.attr.<key> labels and any profile attr block), stamped verbatim.
	StaticAttrs map[string]string

	// PromotedLabels are container label keys to promote to resource attributes
	// under their verbatim key (from bilgeline.labels plus the fleet-wide
	// default set).
	PromotedLabels []string

	// Parse is the structured-log parse mode for the body.
	Parse ParseMode

	// ParsePattern is the named-group regex used when Parse is ParseRegex,
	// supplied by a profile. Empty otherwise.
	ParsePattern string

	// Timestamp optionally extracts a record timestamp from a parsed field,
	// supplied by a profile. Nil when unused.
	Timestamp *TimestampSpec

	// Multiline is the "a new entry starts here" regex that recombines
	// continuation lines (stack traces). Empty when single-line.
	Multiline string

	// LevelField names the parsed field holding the log level. Empty means probe
	// the conventional names (level, severity, lvl) when Parse extracts fields.
	LevelField string

	// LevelMapping is a custom level-string to severity mapping, supplied by a
	// profile, layered over the backend's built-in aliases. Nil when unused.
	LevelMapping map[string]string

	// LevelMin is the severity floor; records below it are dropped. Empty means
	// no floor. One of trace, debug, info, warn, error, fatal.
	LevelMin string

	// Stream selects which output streams route.
	Stream Stream

	// Drop are regexes; a record matching any is dropped. Additive union of the
	// per-container drops and the fleet-wide default set, already merged.
	Drop []string

	// RawOperators is a verbatim passthrough of backend-native stanza operators
	// from a profile's operators block, spliced in by the renderer for the
	// genuinely exotic case. Each entry is one opaque operator map. Nil when
	// unused.
	RawOperators []map[string]any

	// Profile is the resolved profile name this service referenced, retained for
	// logging and diagnostics. The profile's contributions are already merged
	// into the fields above; this is not re-resolved by the renderer.
	Profile string
}

// DestinationNames returns the distinct destination names this service routes
// to, sorted. This is the routing key the fan-out renderer groups services by:
// two services with the same sorted set share one routing-table entry.
func (s ServiceSpec) DestinationNames() []string {
	if len(s.Routes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s.Routes))
	out := make([]string, 0, len(s.Routes))
	for _, r := range s.Routes {
		if _, ok := seen[r.Destination]; ok {
			continue
		}
		seen[r.Destination] = struct{}{}
		out = append(out, r.Destination)
	}
	sort.Strings(out)
	return out
}

// Route points a service at one named destination. It is a struct rather than a
// bare string so per-route options (a route-local override, say) can be added
// later without changing the ServiceSpec contract.
type Route struct {
	// Destination is the destination name, matching a key in Spec.Destinations.
	Destination string
}

// TimestampSpec extracts a record timestamp from a parsed field using a
// strptime-style layout.
type TimestampSpec struct {
	// Field is the parsed field holding the timestamp.
	Field string
	// Layout is the strptime layout the field is parsed with.
	Layout string
}

// RenderedConfig is the backend-native configuration bilgeline writes and
// diffs. bilgeline never inspects Data: for the otelcol backend it is YAML
// bytes, for a future backend it is whatever that backend reads. Format tags the
// content so writers can pick an extension and diffs stay honest.
type RenderedConfig struct {
	// Format names the content type, e.g. "yaml". Advisory, for file naming and
	// logging.
	Format string

	// Data is the opaque config bytes. bilgeline writes and diffs these without
	// interpreting them.
	Data []byte
}

// ApplyAction is what an Apply did to make the config live, for logging.
type ApplyAction string

const (
	// ActionUnchanged means Apply found nothing to do (the live config already
	// matched); no signal was sent.
	ActionUnchanged ApplyAction = "unchanged"

	// ActionReloaded means the config was written and the backing process was
	// signalled to reload it in place.
	ActionReloaded ApplyAction = "reloaded"

	// ActionRestarted means a reload did not take (or the process wedged) and
	// the backing process was restarted to pick the config up.
	ActionRestarted ApplyAction = "restarted"

	// ActionWrittenOnly means the config was written but the backing process was
	// absent, so nothing was signalled; the config is in place for when the
	// process starts.
	ActionWrittenOnly ApplyAction = "written-only"
)

// ApplyResult reports the outcome of an Apply for logging and control-loop
// decisions.
type ApplyResult struct {
	// Action is what Apply did.
	Action ApplyAction

	// Detail is an optional human-readable note, e.g. why a reload escalated to
	// a restart. May be empty.
	Detail string
}
