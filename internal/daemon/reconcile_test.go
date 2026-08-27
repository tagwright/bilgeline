// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/core/runtime"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestReconciler(rt runtime.Runtime, be *fakeBackend, debounce time.Duration) *reconciler {
	return &reconciler{
		rt:       rt,
		cfg:      &config.Config{Debounce: config.DefaultDebounce},
		backend:  be,
		notifier: nil, // notify tolerates a nil notifier; the log is the floor
		logger:   testLogger(),
		selfID:   "",
		debounce: debounce,
	}
}

// TestReconcileHashDiffSkip proves an unchanged spec is applied once, not twice:
// the second reconcile sees the same hash and skips Render and Apply.
func TestReconcileHashDiffSkip(t *testing.T) {
	rt := newFakeRuntime(routedContainer("aaaaaaaaaaaa", "web"))
	be := &fakeBackend{}
	r := newTestReconciler(rt, be, 10*time.Millisecond)

	ctx := context.Background()
	r.reconcile(ctx)
	r.reconcile(ctx)

	render, apply := be.counts()
	if render != 1 {
		t.Errorf("Render calls = %d, want 1 (second pass is a no-op)", render)
	}
	if apply != 1 {
		t.Errorf("Apply calls = %d, want 1 (unchanged spec must not re-apply)", apply)
	}
	// Both passes still discovered (List) so a real change would be caught.
	if rt.listCount() != 2 {
		t.Errorf("List calls = %d, want 2 (each reconcile discovers)", rt.listCount())
	}
}

// TestReconcileChangeReapplies proves a changed fleet triggers a second Apply.
func TestReconcileChangeReapplies(t *testing.T) {
	rt := newFakeRuntime(routedContainer("aaaaaaaaaaaa", "web"))
	be := &fakeBackend{}
	r := newTestReconciler(rt, be, 10*time.Millisecond)

	ctx := context.Background()
	r.reconcile(ctx)

	// A second container appears: the spec changes, so Apply must run again.
	rt.mu.Lock()
	rt.listResp = append(rt.listResp, routedContainer("bbbbbbbbbbbb", "api"))
	rt.mu.Unlock()
	r.reconcile(ctx)

	render, apply := be.counts()
	if render != 2 || apply != 2 {
		t.Errorf("Render/Apply = %d/%d, want 2/2 after a real change", render, apply)
	}
}

// TestReconcileApplyErrorLogsDetail proves that when Apply returns an error, the
// reconcile still logs the ApplyResult.Detail alongside the terse error. This is
// the observability an integration run caught: a collector wedged by a missing
// ${env:VAR} returns only "did not recover after restart", while Apply's Detail
// (which names the missing var and narrates the wedge) is the one actionable
// hint and must not be discarded.
func TestReconcileApplyErrorLogsDetail(t *testing.T) {
	rt := newFakeRuntime(routedContainer("aaaaaaaaaaaa", "web"))
	be := &fakeBackend{
		applyErr: errors.New("collector \"col\" did not recover after restart"),
		applyResult: backend.ApplyResult{
			Action: backend.ActionRestarted,
			Detail: "collector \"col\" died again; collector is missing referenced env var LOKI_BEARER",
		},
	}

	var buf bytes.Buffer
	r := newTestReconciler(rt, be, 10*time.Millisecond)
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	r.reconcile(context.Background())

	out := buf.String()
	if !strings.Contains(out, "apply failed") {
		t.Fatalf("expected an apply-failed log line, got: %s", out)
	}
	if !strings.Contains(out, "missing referenced env var LOKI_BEARER") {
		t.Errorf("apply-failed log must carry the ApplyResult.Detail hint; got: %s", out)
	}
}

// TestLoopDebounceCoalesces proves a burst of rapid events drives exactly one
// reconcile: each event rearms the quiet-window timer, so only after the socket
// goes quiet does a single discovery pass run.
func TestLoopDebounceCoalesces(t *testing.T) {
	rt := newFakeRuntime(routedContainer("aaaaaaaaaaaa", "web"))
	be := &fakeBackend{}
	debounce := 60 * time.Millisecond
	r := newTestReconciler(rt, be, debounce)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.loop(ctx, rt.events, rt.errs)
	}()

	// Fire a burst of events with no quiet gap between them. Each send is received
	// by the loop (unbuffered channel), rearming the timer well within the window.
	for i := 0; i < 5; i++ {
		rt.events <- runtime.Event{Type: runtime.EventStart, ID: "x"}
	}

	// Wait past the debounce window for the single coalesced reconcile.
	deadline := time.After(debounce + 300*time.Millisecond)
	for rt.listCount() < 1 {
		select {
		case <-deadline:
			t.Fatalf("no reconcile fired within the debounce window")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Give any erroneous second reconcile a chance to show up, then assert exactly
	// one ran for the whole burst.
	time.Sleep(80 * time.Millisecond)
	if got := rt.listCount(); got != 1 {
		t.Errorf("List calls = %d, want 1 (five events must coalesce into one reconcile)", got)
	}

	cancel()
	<-done
}

// TestLoopIgnoresIrrelevantEvents proves an unrecognized event type does not arm
// the debounce, so no reconcile runs for it.
func TestLoopIgnoresIrrelevantEvents(t *testing.T) {
	rt := newFakeRuntime(routedContainer("aaaaaaaaaaaa", "web"))
	be := &fakeBackend{}
	debounce := 40 * time.Millisecond
	r := newTestReconciler(rt, be, debounce)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.loop(ctx, rt.events, rt.errs)
	}()

	rt.events <- runtime.Event{Type: runtime.EventType("pause"), ID: "x"}
	time.Sleep(debounce + 60*time.Millisecond)

	if got := rt.listCount(); got != 0 {
		t.Errorf("List calls = %d, want 0 (an irrelevant event must not reconcile)", got)
	}

	cancel()
	<-done
}
