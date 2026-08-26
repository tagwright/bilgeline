// SPDX-License-Identifier: GPL-3.0-or-later

// Package discovery turns running containers and their bilgeline.* labels into
// fully resolved backend.ServiceSpec values: the label reader that realizes the
// frozen bilgeline label grammar.
//
// It recognizes two label prefixes, "bilgeline." (primary) and "tagwright.log."
// (org-namespaced alias), holding one internal suffix grammar with two accepted
// spellings on the outside. The pure translation lives in Resolve, which takes
// a Candidate (a container's labels plus its identity and inspect data as plain
// data) and a *config.Config and returns a ServiceSpec, a silent skip, or one
// or more Diagnostics. The pure path reads no state, executes no commands, and
// touches no socket, so it is unit-testable without a live runtime. Discover is
// the thin outer function that lists containers through core/runtime and feeds
// each one to Resolve.
//
// One bad container is skipped and reported (as an error Diagnostic); it never
// aborts discovery for the rest of the fleet. See the Bilgeline Label Grammar
// for the full contract this package implements.
package discovery

import (
	"context"
	"sort"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/core/runtime"
)

// Reserved destination names that are always valid regardless of whether they
// appear in the configured destinations map.
const (
	// noneSentinel routes a container nowhere: it is enabled (opted in) but its
	// logs are dropped at the source. This is a valid state, distinct from being
	// disabled, and produces a ServiceSpec with no Routes.
	noneSentinel = "none"

	// debugDestination is the built-in debug exporter destination. It is always
	// a valid destination name even when bilgeline.yml does not define it, so
	// "point this container at debug for ten minutes" always works.
	debugDestination = "debug"

	// jsonFileDriver is the only log driver v1 can tail. See the log-driver note
	// in Resolve.
	jsonFileDriver = "json-file"
)

// Severity classifies a Diagnostic. An error means a container was skipped and
// the operator must know why; a warning is a non-fatal notice about a container
// that was either excluded for a structural reason (self, collector, or an
// untailable log driver) or routed with a caveat.
type Severity string

const (
	// SeverityError marks a container that was skipped for a validation reason.
	SeverityError Severity = "error"
	// SeverityWarning marks a non-fatal notice.
	SeverityWarning Severity = "warning"
)

// Diagnostic is one finding about a single container, carried back to the
// daemon so it can be routed to alerting. It names the container so an operator
// can act on it and never aborts the wider discovery pass.
type Diagnostic struct {
	// ContainerID is the full container id the finding is about.
	ContainerID string
	// ContainerName is the container name, for a human-readable alert.
	ContainerName string
	// Severity is error (skipped) or warning (notice).
	Severity Severity
	// Message is the human-readable explanation.
	Message string
}

// Candidate is the plain, socket-free input to Resolve: everything the pure
// label-and-config logic needs about one container, decoupled from the runtime
// client so the logic is testable without a socket.
//
// Image and LogDriver are not exposed by the current core runtime.Container
// (see candidateFromContainer): they arrive empty in v1 and light up once core
// surfaces them, with no change to Resolve.
type Candidate struct {
	// ID is the full 64-hex container id.
	ID string
	// Name is the container name (leading "/" already stripped).
	Name string
	// Labels is the container's raw label map, both prefixes intact.
	Labels map[string]string
	// ComposeProject is com.docker.compose.project, empty when not a compose
	// service.
	ComposeProject string
	// ComposeService is com.docker.compose.service, empty when not a compose
	// service.
	ComposeService string
	// Image is the container image reference, stamped as container.image.name.
	// Empty until core exposes it.
	Image string
	// LogDriver is the container's effective log driver (e.g. "json-file",
	// "local"). Empty means unknown, which v1 treats as json-file compatible so
	// a stock Docker host routes normally; a known non-json-file driver excludes
	// the container. Empty until core exposes it.
	LogDriver string
}

// Discover lists every container through rt, resolves each against cfg, and
// returns the routed services plus every diagnostic gathered along the way.
// selfID is bilgeline's own container id, always excluded from routing.
//
// The returned error is reserved for a hard failure that prevents discovery at
// all (the runtime list call failing). Per-container validation problems are
// Diagnostics, never errors: a single bad container is skipped and reported,
// and the rest of the fleet still resolves. Services are returned sorted by
// ContainerID so the daemon's Spec.Hash is stable across passes.
func Discover(ctx context.Context, rt runtime.Runtime, cfg *config.Config, selfID string) ([]backend.ServiceSpec, []Diagnostic, error) {
	containers, err := rt.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	var specs []backend.ServiceSpec
	var diags []Diagnostic

	for _, c := range containers {
		spec, ds := Resolve(candidateFromContainer(c), cfg, selfID)
		diags = append(diags, ds...)
		if spec != nil {
			specs = append(specs, *spec)
		}
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].ContainerID < specs[j].ContainerID })
	return specs, diags, nil
}

// candidateFromContainer maps a core runtime.Container into a Candidate.
//
// The current runtime.Container exposes id, name, labels, and the compose
// project/service pair, but NOT the image reference or the effective log
// driver. Those two require additions to core (Container.Image from the inspect
// Config.Image, Container.LogDriver from the inspect HostConfig.LogConfig.Type).
// Until then they map to the empty string, which Resolve handles: an empty
// image simply omits the container.image.name attribute, and an empty log
// driver is treated as json-file compatible. When core grows the fields, wire
// them here and the image attribute and non-json-file exclusion activate with
// no other change.
func candidateFromContainer(c runtime.Container) Candidate {
	return Candidate{
		ID:             c.ID,
		Name:           c.Name,
		Labels:         c.Labels,
		ComposeProject: c.Project,
		ComposeService: c.Service,
		Image:          "", // core runtime.Container does not expose the image yet
		LogDriver:      "", // core runtime.Container does not expose the log driver yet
	}
}
