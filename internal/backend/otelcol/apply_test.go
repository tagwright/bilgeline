// SPDX-License-Identifier: GPL-3.0-or-later

package otelcol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/core/runtime"
)

// fakeRuntime is a scripted runtime.Runtime for the apply path. It records the
// Kill and Restart calls it receives, returns scripted List and Inspect
// results, and lets a test mutate container state after a Kill or Restart so the
// wedge-recovery path can be exercised (a collector that "exits" after a SIGHUP).
type fakeRuntime struct {
	mu sync.Mutex

	list       []runtime.Container
	byID       map[string]*runtime.Container
	listErr    error
	inspectErr map[string]error

	kills    []killRecord
	restarts []string

	// onKill runs after a recorded Kill, onRestart after a recorded Restart, so
	// a test can flip a container's State to simulate a wedge or a recovery.
	onKill    func(f *fakeRuntime)
	onRestart func(f *fakeRuntime)
}

type killRecord struct {
	id     string
	signal string
}

func newFakeRuntime(containers ...runtime.Container) *fakeRuntime {
	f := &fakeRuntime{byID: map[string]*runtime.Container{}}
	for i := range containers {
		c := containers[i]
		f.list = append(f.list, c)
		cc := c
		f.byID[c.ID] = &cc
	}
	return f
}

func (f *fakeRuntime) setState(id, state string) {
	if c, ok := f.byID[id]; ok {
		c.State = state
	}
}

func (f *fakeRuntime) List(ctx context.Context) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Return snapshots reflecting current state.
	out := make([]runtime.Container, 0, len(f.list))
	for _, c := range f.list {
		if cur, ok := f.byID[c.ID]; ok {
			out = append(out, *cur)
		} else {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		if err, ok := f.inspectErr[id]; ok {
			return runtime.Container{}, err
		}
	}
	if c, ok := f.byID[id]; ok {
		return *c, nil
	}
	return runtime.Container{}, os.ErrNotExist
}

func (f *fakeRuntime) Kill(ctx context.Context, id, signal string) error {
	f.mu.Lock()
	f.kills = append(f.kills, killRecord{id: id, signal: signal})
	hook := f.onKill
	f.mu.Unlock()
	if hook != nil {
		hook(f)
	}
	return nil
}

func (f *fakeRuntime) Restart(ctx context.Context, id string) error {
	f.mu.Lock()
	f.restarts = append(f.restarts, id)
	hook := f.onRestart
	f.mu.Unlock()
	if hook != nil {
		hook(f)
	}
	return nil
}

// Unused interface methods.
func (f *fakeRuntime) Watch(ctx context.Context) (<-chan runtime.Event, <-chan error) {
	return nil, nil
}
func (f *fakeRuntime) Exec(ctx context.Context, id string, spec runtime.ExecSpec) (*runtime.ExecHandle, error) {
	return nil, runtime.ErrNotImplemented
}
func (f *fakeRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error { return nil }
func (f *fakeRuntime) Start(ctx context.Context, id string) error                    { return nil }
func (f *fakeRuntime) Close() error                                                  { return nil }

func (f *fakeRuntime) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.kills)
}

func (f *fakeRuntime) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restarts)
}

// collectorContainer builds a running collector container that mounts the shared
// config directory, marked under the primary prefix.
func collectorContainer(id, name, sharedDir string) runtime.Container {
	return runtime.Container{
		ID:     id,
		Name:   name,
		State:  "running",
		Labels: map[string]string{"bilgeline.collector": "true"},
		Mounts: []runtime.Mount{{Type: runtime.MountVolume, Destination: sharedDir}},
	}
}

// newBackend wires a Backend for the apply path with a short reload window.
func newBackend(rt runtime.Runtime, sharedPath string, opts ...Option) *Backend {
	base := []Option{
		WithRuntime(rt),
		WithSharedConfigPath(sharedPath),
		WithReloadWait(30 * time.Millisecond),
	}
	return New(append(base, opts...)...)
}

func sharedPathIn(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "otelcol.yaml")
}

func rendered(body string) backend.RenderedConfig {
	return backend.RenderedConfig{Format: "yaml", Data: []byte(body)}
}

func TestApplyWritesConfigAtomically(t *testing.T) {
	dir, path := sharedPathIn(t)
	// A nested, not-yet-existing subdir to prove MkdirAll runs.
	path = filepath.Join(dir, "sub", "otelcol.yaml")

	rt := newFakeRuntime() // no collector
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("receivers: {}\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only", res.Action)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(got) != "receivers: {}\n" {
		t.Fatalf("written config = %q", string(got))
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
	if rt.killCount() != 0 {
		t.Fatalf("no collector present, Kill must not be called")
	}
}

func TestApplyFoundByMarker(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("aaaa000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionReloaded {
		t.Fatalf("action = %q, want reloaded (detail: %s)", res.Action, res.Detail)
	}
	if rt.killCount() != 1 || rt.kills[0].signal != "SIGHUP" {
		t.Fatalf("expected one SIGHUP, got %+v", rt.kills)
	}
}

func TestApplyFoundByFallbackName(t *testing.T) {
	dir, path := sharedPathIn(t)
	// No marker label, resolved by cfg.Collector name.
	col := runtime.Container{
		ID:     "bbbb000000000000000000000000000000000000000000000000000000000000",
		Name:   "my-collector",
		State:  "running",
		Labels: map[string]string{},
		Mounts: []runtime.Mount{{Destination: dir}},
	}
	rt := newFakeRuntime(col)
	b := newBackend(rt, path, WithCollectorName("my-collector"))

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionReloaded {
		t.Fatalf("action = %q, want reloaded (detail: %s)", res.Action, res.Detail)
	}
	if rt.killCount() != 1 {
		t.Fatalf("expected one Kill, got %d", rt.killCount())
	}
}

func TestApplyNoCollector(t *testing.T) {
	_, path := sharedPathIn(t)
	rt := newFakeRuntime() // empty
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only", res.Action)
	}
	if !strings.Contains(res.Detail, "no collector found") {
		t.Fatalf("detail = %q, want no-collector note", res.Detail)
	}
	if rt.killCount() != 0 {
		t.Fatalf("Kill must not be called with no collector")
	}
}

func TestApplyMultipleMarkersIsError(t *testing.T) {
	dir, path := sharedPathIn(t)
	c1 := collectorContainer("1111000000000000000000000000000000000000000000000000000000000000", "col-a", dir)
	c2 := collectorContainer("2222000000000000000000000000000000000000000000000000000000000000", "col-b", dir)
	rt := newFakeRuntime(c1, c2)
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply should not fail the whole apply: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only", res.Action)
	}
	if !strings.Contains(res.Detail, "multiple") {
		t.Fatalf("detail = %q, want ambiguity note", res.Detail)
	}
	if rt.killCount() != 0 {
		t.Fatalf("ambiguity must not signal, got %d kills", rt.killCount())
	}
}

func TestApplyMarkerVsConfigDisagreement(t *testing.T) {
	dir, path := sharedPathIn(t)
	marked := collectorContainer("3333000000000000000000000000000000000000000000000000000000000000", "marked-col", dir)
	other := runtime.Container{
		ID:     "4444000000000000000000000000000000000000000000000000000000000000",
		Name:   "configured-col",
		State:  "running",
		Labels: map[string]string{},
		Mounts: []runtime.Mount{{Destination: dir}},
	}
	rt := newFakeRuntime(marked, other)
	// cfg.Collector names a DIFFERENT existing container than the marked one.
	b := newBackend(rt, path, WithCollectorName("configured-col"))

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply should not fail the whole apply: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only", res.Action)
	}
	if !strings.Contains(res.Detail, "disagree") {
		t.Fatalf("detail = %q, want disagreement note", res.Detail)
	}
	if rt.killCount() != 0 {
		t.Fatalf("disagreement must not signal, got %d kills", rt.killCount())
	}
}

func TestApplyMarkerAgreesWithConfigSameContainer(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("5555000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	rt := newFakeRuntime(col)
	// cfg.Collector names the SAME container as the marker: not a conflict.
	b := newBackend(rt, path, WithCollectorName("otelcol"))

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionReloaded {
		t.Fatalf("action = %q, want reloaded", res.Action)
	}
	if rt.killCount() != 1 {
		t.Fatalf("expected one Kill, got %d", rt.killCount())
	}
}

func TestApplyMountGuardRefusesToSignal(t *testing.T) {
	dir, path := sharedPathIn(t)
	// Marked, but mounts a totally different path.
	col := runtime.Container{
		ID:     "6666000000000000000000000000000000000000000000000000000000000000",
		Name:   "mislabeled",
		State:  "running",
		Labels: map[string]string{"bilgeline.collector": "true"},
		Mounts: []runtime.Mount{{Destination: "/some/other/path"}},
	}
	_ = dir
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply should not fail the whole apply: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only", res.Action)
	}
	if !strings.Contains(res.Detail, "does not mount") {
		t.Fatalf("detail = %q, want mount-guard note", res.Detail)
	}
	if rt.killCount() != 0 {
		t.Fatalf("mount-guard failure must not signal, got %d kills", rt.killCount())
	}
}

func TestApplyMountGuardAcceptsFileMount(t *testing.T) {
	_, path := sharedPathIn(t)
	// Collector binds the config FILE itself, not the directory.
	col := runtime.Container{
		ID:     "7777000000000000000000000000000000000000000000000000000000000000",
		Name:   "otelcol",
		State:  "running",
		Labels: map[string]string{"bilgeline.collector": "true"},
		Mounts: []runtime.Mount{{Destination: path}},
	}
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionReloaded {
		t.Fatalf("action = %q, want reloaded (file mount should satisfy guard)", res.Action)
	}
}

func TestApplyHappyReloadStaysRunning(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("8888000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	rt := newFakeRuntime(col)
	// No onKill: the collector stays running through the window.
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionReloaded {
		t.Fatalf("action = %q, want reloaded", res.Action)
	}
	if rt.restartCount() != 0 {
		t.Fatalf("healthy reload must not restart, got %d", rt.restartCount())
	}
}

func TestApplyWedgeThenSuccessfulRestart(t *testing.T) {
	dir, path := sharedPathIn(t)
	id := "9999000000000000000000000000000000000000000000000000000000000000"
	col := collectorContainer(id, "otelcol", dir)
	rt := newFakeRuntime(col)
	// The SIGHUP wedges the collector: it exits. The restart brings it back.
	rt.onKill = func(f *fakeRuntime) { f.setState(id, "exited") }
	rt.onRestart = func(f *fakeRuntime) { f.setState(id, "running") }
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionRestarted {
		t.Fatalf("action = %q, want restarted (detail: %s)", res.Action, res.Detail)
	}
	if rt.killCount() != 1 {
		t.Fatalf("expected one Kill, got %d", rt.killCount())
	}
	if rt.restartCount() != 1 {
		t.Fatalf("expected exactly one Restart, got %d", rt.restartCount())
	}
}

func TestApplyWedgeThenRepeatedFailure(t *testing.T) {
	dir, path := sharedPathIn(t)
	id := "aaaabbbb00000000000000000000000000000000000000000000000000000000"
	col := collectorContainer(id, "otelcol", dir)
	rt := newFakeRuntime(col)
	// The collector dies on SIGHUP and stays dead through the restart.
	rt.onKill = func(f *fakeRuntime) { f.setState(id, "exited") }
	rt.onRestart = func(f *fakeRuntime) { f.setState(id, "exited") }
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err == nil {
		t.Fatalf("expected an error result on repeated failure, got nil (detail: %s)", res.Detail)
	}
	if rt.killCount() != 1 {
		t.Fatalf("expected one Kill, got %d", rt.killCount())
	}
	if rt.restartCount() != 1 {
		t.Fatalf("expected exactly ONE Restart (no crash-loop), got %d", rt.restartCount())
	}
}

func TestEnvPreflightWarnsOnMissingName(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("cccc000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	// Collector carries only ONE of the two referenced env names, with a secret
	// value that must never surface in diagnostics.
	col.Env = []string{"LOKI_USER=present", "SECRET_TOKEN=super-secret-value"}
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	body := "headers:\n  auth: \"Bearer ${env:LOKI_BEARER}\"\n  user: \"${env:LOKI_USER}\"\n"
	res, err := b.Apply(context.Background(), rendered(body))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if !strings.Contains(res.Detail, "LOKI_BEARER") {
		t.Fatalf("detail = %q, want a warning for the missing LOKI_BEARER", res.Detail)
	}
	if strings.Contains(res.Detail, "LOKI_USER") {
		t.Fatalf("detail = %q, must not warn for the present LOKI_USER", res.Detail)
	}
	if strings.Contains(res.Detail, "super-secret-value") || strings.Contains(res.Detail, "SECRET_TOKEN") {
		t.Fatalf("env values/unreferenced names must never appear in a diagnostic: %q", res.Detail)
	}
}

func TestEnvPreflightNoWarnWhenPresent(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("dddd000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	col.Env = []string{"LOKI_BEARER=abc"}
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	body := "auth: \"Bearer ${env:LOKI_BEARER}\"\n"
	res, err := b.Apply(context.Background(), rendered(body))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if strings.Contains(res.Detail, "missing referenced env var") {
		t.Fatalf("detail = %q, should carry no missing-env warning", res.Detail)
	}
}

func TestApplyCollectorNotRunningWritesOnly(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("eeee000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	col.State = "exited"
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	res, err := b.Apply(context.Background(), rendered("x: 1\n"))
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if res.Action != backend.ActionWrittenOnly {
		t.Fatalf("action = %q, want written-only for a stopped collector", res.Action)
	}
	if rt.killCount() != 0 {
		t.Fatalf("must not signal a stopped collector, got %d kills", rt.killCount())
	}
}

func TestHealthy(t *testing.T) {
	dir, path := sharedPathIn(t)
	col := collectorContainer("ffff000000000000000000000000000000000000000000000000000000000000", "otelcol", dir)
	rt := newFakeRuntime(col)
	b := newBackend(rt, path)

	if err := b.Healthy(context.Background()); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}

	// Stop it: Healthy must report the reason.
	col2 := col
	col2.State = "exited"
	rt2 := newFakeRuntime(col2)
	b2 := newBackend(rt2, path)
	if err := b2.Healthy(context.Background()); err == nil {
		t.Fatalf("expected unhealthy for a stopped collector")
	}

	// No collector at all.
	b3 := newBackend(newFakeRuntime(), path)
	if err := b3.Healthy(context.Background()); err == nil {
		t.Fatalf("expected unhealthy when no collector is discoverable")
	}
}

// TestRenderIndependentOfApplyWiring proves Render works on a zero-value
// Backend with no runtime, shared path, or collector wired in.
func TestRenderIndependentOfApplyWiring(t *testing.T) {
	b := New() // no apply options
	_, err := b.Render(backend.Spec{})
	if err != nil {
		t.Fatalf("Render on a render-only backend failed: %v", err)
	}
}
