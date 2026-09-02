// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/core/runtime"
)

// fakeRuntime is a runtime.Runtime that serves a fixed container list and hands
// the test control of the Watch channels, so the daemon loop can be exercised
// with no socket. It counts List calls so a test can assert how many discovery
// passes ran.
type fakeRuntime struct {
	mu        sync.Mutex
	listResp  []runtime.Container
	inspectFn func(id string) (runtime.Container, error)
	listCalls int

	events chan runtime.Event
	errs   chan error
}

func newFakeRuntime(containers ...runtime.Container) *fakeRuntime {
	return &fakeRuntime{
		listResp: containers,
		events:   make(chan runtime.Event),
		errs:     make(chan error),
	}
}

func (f *fakeRuntime) List(ctx context.Context) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.listResp, nil
}

func (f *fakeRuntime) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	if f.inspectFn != nil {
		return f.inspectFn(id)
	}
	for _, c := range f.listResp {
		if c.ID == id {
			return c, nil
		}
	}
	return runtime.Container{}, fmt.Errorf("fakeRuntime: no container %q", id)
}

func (f *fakeRuntime) Watch(ctx context.Context) (<-chan runtime.Event, <-chan error) {
	return f.events, f.errs
}

func (f *fakeRuntime) Exec(ctx context.Context, id string, spec runtime.ExecSpec) (*runtime.ExecHandle, error) {
	return nil, runtime.ErrNotImplemented
}
func (f *fakeRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error { return nil }
func (f *fakeRuntime) Start(ctx context.Context, id string) error                    { return nil }
func (f *fakeRuntime) Kill(ctx context.Context, id string, signal string) error      { return nil }
func (f *fakeRuntime) Restart(ctx context.Context, id string) error                  { return nil }
func (f *fakeRuntime) Close() error                                                  { return nil }

// fakeBackend records Render and Apply calls so a test can assert the hash-diff
// skip (no Apply when the spec is unchanged) without a real renderer or
// collector.
type fakeBackend struct {
	mu          sync.Mutex
	renderCalls int
	applyCalls  int
	lastSpec    backend.Spec
	renderErr   error
	healthErr   error
	applyErr    error
	applyResult backend.ApplyResult
}

func (b *fakeBackend) Render(spec backend.Spec) (backend.RenderedConfig, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.renderCalls++
	b.lastSpec = spec
	if b.renderErr != nil {
		return backend.RenderedConfig{}, b.renderErr
	}
	return backend.RenderedConfig{Format: "yaml", Data: []byte("rendered: true\n")}, nil
}

func (b *fakeBackend) Apply(ctx context.Context, cfg backend.RenderedConfig) (backend.ApplyResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applyCalls++
	if b.applyErr != nil {
		return b.applyResult, b.applyErr
	}
	if b.applyResult.Action != "" {
		return b.applyResult, nil
	}
	return backend.ApplyResult{Action: backend.ActionReloaded}, nil
}

func (b *fakeBackend) Healthy(ctx context.Context) error { return b.healthErr }
func (b *fakeBackend) Name() string                      { return "fake" }

func (b *fakeBackend) counts() (render, apply int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.renderCalls, b.applyCalls
}

// routedContainer is a container opted in via labels and routed to the reserved
// debug sink (always valid, needs no config entry), with a tailable log driver.
func routedContainer(id, name string) runtime.Container {
	return runtime.Container{
		ID:    id,
		Name:  name,
		State: "running",
		Labels: map[string]string{
			"bilgeline.enable":      "true",
			"bilgeline.destination": "debug",
		},
		LogDriver: "json-file",
	}
}
