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
	"fmt"
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
// Image and LogDriver are populated from the container's Inspect result (the
// list summary carries no HostConfig, so LogDriver is inspect-only). Discover
// inspects each opted-in container to fill them; see candidateFromContainer.
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
	// Populated from the Inspect result.
	Image string
	// LogDriver is the container's effective log driver (e.g. "json-file",
	// "local"). Empty means unknown, which v1 treats as json-file compatible so
	// a stock Docker host routes normally; a known non-json-file driver excludes
	// the container. Populated from the Inspect result (inspect-only in core).
	LogDriver string
}

// Discover lists every container through rt, inspects the ones that opted in,
// resolves each against cfg, and returns the routed services plus every
// diagnostic gathered along the way. selfID is bilgeline's own container id,
// always excluded from routing.
//
// The list summary carries no HostConfig, so the effective log driver is
// inspect-only. Rather than inspect the whole host, Discover gates cheaply on
// the list labels first: only a container whose labels already carry
// enable=true (under either recognized prefix) is inspected, and the inspect
// result (which carries Image and LogDriver) is what its Candidate is built
// from. Self and the collector marker are left to Resolve, which warns only in
// the degenerate enable=true case; a container that never opted in is skipped
// here for free, with no socket round-trip.
//
// The returned error is reserved for a hard failure that prevents discovery at
// all (the runtime list call failing). Per-container problems are Diagnostics,
// never errors: a container whose Inspect fails is skipped with a warning, a
// container that fails validation is skipped and reported, and the rest of the
// fleet still resolves. Services are returned sorted by ContainerID so the
// daemon's Spec.Hash is stable across passes.
func Discover(ctx context.Context, rt runtime.Runtime, cfg *config.Config, selfID string) ([]backend.ServiceSpec, []Diagnostic, error) {
	containers, err := rt.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	var specs []backend.ServiceSpec
	var diags []Diagnostic

	for _, c := range containers {
		// Cheap opt-in gate off the list labels: skip anything that did not ask
		// to be routed before paying for an Inspect.
		if !rawBool(c.Labels, "enable") {
			continue
		}

		// Inspect the opted-in container to populate Image and LogDriver, which
		// the list summary does not carry. A failed inspect (the container raced
		// away, say) is a per-container warning, never fatal: skip it and keep
		// resolving the rest of the fleet.
		full, ierr := rt.Inspect(ctx, c.ID)
		if ierr != nil {
			diags = append(diags, Diagnostic{
				ContainerID:   c.ID,
				ContainerName: c.Name,
				Severity:      SeverityWarning,
				Message:       fmt.Sprintf("inspect failed, container skipped: %v", ierr),
			})
			continue
		}

		spec, ds := Resolve(candidateFromContainer(full), cfg, selfID)
		diags = append(diags, ds...)
		if spec != nil {
			specs = append(specs, *spec)
		}
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].ContainerID < specs[j].ContainerID })
	return specs, diags, nil
}

// candidateFromContainer maps a core runtime.Container into a Candidate. It is
// fed an Inspect result (not a list summary), so Image and LogDriver are
// populated: an empty Image omits the container.image.name attribute, and a
// known non-json-file LogDriver drives the exclusion in Resolve.
func candidateFromContainer(c runtime.Container) Candidate {
	return Candidate{
		ID:             c.ID,
		Name:           c.Name,
		Labels:         c.Labels,
		ComposeProject: c.Project,
		ComposeService: c.Service,
		Image:          c.Image,
		LogDriver:      c.LogDriver,
	}
}
