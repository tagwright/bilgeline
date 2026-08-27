// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tagwright/beacon"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/discovery"
	"github.com/tagwright/core/runtime"
)

// reconciler holds the collaborators the control loop drives and the single
// piece of loop state that survives across passes: the hash of the last spec it
// applied. It is the event-driven heart of the daemon, kept as a struct so the
// pure-ish reconcile step and the debounce loop are testable with a fake runtime
// and a fake backend, no socket or live collector required.
type reconciler struct {
	rt       runtime.Runtime
	cfg      *config.Config
	backend  backend.Backend
	notifier *beacon.Beacon
	logger   *slog.Logger
	selfID   string
	debounce time.Duration

	// mu serializes reconcile so an Apply can never overlap the next pass. The
	// loop is single-goroutine so contention is not the point; the lock makes the
	// "one Apply at a time" guarantee explicit and guards lastHash.
	mu       sync.Mutex
	lastHash string
}

// reconcile runs one full pass: discover the fleet, route every diagnostic to
// logs and alerting, assemble the spec, and, only when its hash differs from the
// last applied, render and apply it. A discovery error (the socket list failing)
// aborts this pass without touching lastHash, so the next event retries cleanly.
func (r *reconciler) reconcile(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Track the pass outcome and push it to telemetry once, on the way out,
	// whichever return path we take. A clean pass reports healthy; a discovery,
	// render, or apply failure, or a pass that skipped a container on an
	// error-severity diagnostic, reports degraded with the reason. This is the
	// event-driven telemetry axis: one Gatus-style push per reconcile, no
	// background clock. See notifier.report.
	start := time.Now()
	ok := true
	healthMsg := ""
	defer func() { report(r.notifier, ok, healthMsg, time.Since(start)) }()

	services, diags, err := discovery.Discover(ctx, r.rt, r.cfg, r.selfID)
	if errDiags := r.routeDiagnostics(diags); errDiags > 0 {
		// Containers were skipped for validation faults. The pass may still apply
		// cleanly for the rest of the fleet, so continue, but report degraded: a
		// later apply failure overwrites this with the more severe reason.
		ok = false
		healthMsg = fmt.Sprintf("%d container(s) skipped on error diagnostics", errDiags)
	}
	if err != nil {
		r.logger.Error("discovery failed", "error", err)
		notify(r.notifier, beacon.LevelError, "bilgeline: discovery failed", err.Error())
		ok, healthMsg = false, "discovery failed: "+err.Error()
		return
	}

	spec := AssembleSpec(services, r.cfg)
	hash := spec.Hash()
	if hash == r.lastHash {
		r.logger.Debug("reconcile: spec unchanged, nothing to apply",
			"services", len(services), "hash", hash)
		return
	}

	rendered, err := r.backend.Render(spec)
	if err != nil {
		r.logger.Error("render failed", "backend", r.backend.Name(), "error", err)
		notify(r.notifier, beacon.LevelError, "bilgeline: render failed", err.Error())
		ok, healthMsg = false, "render failed: "+err.Error()
		return
	}

	result, err := r.backend.Apply(ctx, rendered)
	if err != nil {
		// result.Detail carries the actionable context Apply assembled even on
		// failure: the collector name, the wedge-recovery narrative, and any
		// env-preflight warnings naming a missing ${env:VAR}. The returned error
		// itself is terse ("collector X did not recover after restart"), so a
		// missing-env-var wedge would otherwise reach the operator with the one
		// hint that explains WHY discarded. Log and alert the detail alongside it.
		r.logger.Error("apply failed", "backend", r.backend.Name(), "error", err, "detail", result.Detail)
		body := err.Error()
		if result.Detail != "" {
			body = result.Detail + ": " + err.Error()
		}
		notify(r.notifier, beacon.LevelError, "bilgeline: apply failed", body)
		ok, healthMsg = false, "apply failed: "+body
		return
	}

	// Record the applied hash only once Apply succeeded, so a failed apply is
	// retried on the next event rather than skipped as a no-op.
	r.lastHash = hash
	r.logger.Info("reconcile: applied",
		"backend", r.backend.Name(),
		"action", string(result.Action),
		"detail", result.Detail,
		"services", len(services),
		"destinations", len(spec.Destinations),
		"hash", hash)
}

// run drives the daemon until ctx is cancelled: an initial reconcile before any
// event (so a fresh start converges without waiting for socket churn), then the
// debounced watch loop.
func (r *reconciler) run(ctx context.Context) {
	r.reconcile(ctx)
	events, errs := r.rt.Watch(ctx)
	r.loop(ctx, events, errs)
}

// loop consumes the runtime's lifecycle events and coalesces bursts through a
// single quiet-window timer: every relevant event (re)arms the timer, and only
// when the socket has been quiet for the debounce window does one reconcile run.
// A compose up or a crash-loop that fires a dozen events in a second thus drives
// exactly one reconcile. It returns on context cancellation or when the event
// stream closes.
//
// events and errs are passed in (rather than opened here) so the loop is
// testable with a hand-fed channel, no socket required.
func (r *reconciler) loop(ctx context.Context, events <-chan runtime.Event, errs <-chan error) {
	// A stopped timer with a drained channel: no reconcile is pending until an
	// event arms it. timer.C is only selected on while armed is true, so the
	// initial stopped timer never spuriously fires a pass.
	timer := time.NewTimer(r.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-events:
			if !ok {
				return
			}
			if !relevantEvent(ev.Type) {
				continue
			}
			// (Re)arm the quiet-window timer, coalescing this event with any
			// still-pending burst.
			if armed && !timer.Stop() {
				// Timer already fired and its tick is waiting; drain it so the
				// reset below starts a clean window.
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.debounce)
			armed = true

		case err, ok := <-errs:
			if !ok {
				// The error channel closed. The event channel closes alongside it
				// (per the runtime contract), so the events case will return.
				continue
			}
			if err != nil {
				r.logger.Error("watch error", "error", err)
			}

		case <-timer.C:
			armed = false
			r.reconcile(ctx)
		}
	}
}

// relevantEvent reports whether a lifecycle event should trigger a reconcile.
// Every known container lifecycle transition can change what bilgeline routes
// (a start adds a candidate, a die or destroy removes one, a stop takes a
// container out of the running set), so all of them arm the debounce. Reconcile
// is hash-diffed, so an event that changes nothing costs a discovery pass and no
// apply. Unknown future event types are ignored.
func relevantEvent(t runtime.EventType) bool {
	switch t {
	case runtime.EventStart, runtime.EventStop, runtime.EventDie, runtime.EventDestroy:
		return true
	default:
		return false
	}
}
