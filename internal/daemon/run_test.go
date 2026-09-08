// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/core/runtime"
	"github.com/tagwright/core/runtime/runtimetest"
)

// syncBuffer is a concurrency-safe bytes.Buffer: the daemon goroutine writes log
// records into it while the test goroutine polls its contents.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_InjectedListFailureSurfaces is bilgeline's Level 2 wiring test for the
// Testing Standard: it drives the daemon through its real production entry seam,
// run(Deps), with the canonical fake runtime (core/runtime/runtimetest), injects
// a failure on a runtime operation the reconcile path actually uses (the
// container List that every discovery pass begins with), and asserts that
// failure SURFACES as an error record in the daemon's structured log, never a
// silent success.
//
// List is the injection point because it is the runtime fault the wiring
// genuinely turns into a failure. The apply path deliberately swallows a Kill
// (SIGHUP) or Inspect fault into a written-only, non-fatal result, and discovery
// downgrades a per-container Inspect fault to a warning that skips one container;
// only a List failure aborts the pass and is reported loud. Without the injected
// Faults.List this test would prove only the happy path, which the suite audit
// found is where the fake tier misses every real bug; the standard forbids a
// double with no error knob for exactly that reason.
func TestRun_InjectedListFailureSurfaces(t *testing.T) {
	rt := runtimetest.New()
	// A fleet exists, but the socket list fails: the realistic "the daemon
	// reacted to churn and went to re-derive the fleet, and the runtime call
	// errored" shape. The container is never reached because List faults first.
	rt.Containers = []runtime.Container{{
		ID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Name:      "web",
		State:     "running",
		LogDriver: "json-file",
		Labels: map[string]string{
			"bilgeline.enable":      "true",
			"bilgeline.destination": "debug",
		},
	}}
	// The injected failure: the container list fails on demand. This is the knob
	// the standard requires a wiring test to trip.
	injected := errors.New("injected list failure")
	rt.Faults.List = injected

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	deps := Deps{
		Runtime:  rt,
		Backend:  &fakeBackend{},
		Notifier: nil, // notify/report tolerate a nil notifier; the log is the floor
		Clock:    rt.Clock.Now,
		Config:   &config.Config{Debounce: config.DefaultDebounce},
		Logger:   logger,
		SelfID:   "",
		Debounce: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, deps) }()

	// run's initial reconcile discovers before the watch loop, so the injected
	// List failure is logged almost immediately. Poll for the error record.
	waitForLog(t, buf, "discovery failed", 10*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after ctx cancel")
	}

	out := buf.String()
	if !strings.Contains(out, "discovery failed") {
		t.Fatalf("injected list failure did not surface: no discovery-failed record in the log:\n%s", out)
	}
	if !strings.Contains(out, injected.Error()) {
		t.Errorf("discovery-failed record must carry the underlying error %q; got:\n%s", injected.Error(), out)
	}
	// The failure must not be recorded as a silent success: a pass that logged
	// "reconcile: applied" would mean the fault was swallowed and the collector
	// config treated as freshly applied.
	if strings.Contains(out, "reconcile: applied") {
		t.Errorf("injected list failure was swallowed: the pass recorded a successful apply:\n%s", out)
	}
}

// waitForLog polls buf until it contains substr, or fails the test after timeout.
func waitForLog(t *testing.T, buf *syncBuffer, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log did not contain %q within %v:\n%s", substr, timeout, buf.String())
}
