// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tagwright/bilgeline/internal/backend/otelcol"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/core/runtime"
)

// fakeRuntime serves a fixed container list so generate can be exercised with no
// socket. Only List and Inspect are meaningful; the rest satisfy the interface.
type fakeRuntime struct {
	containers []runtime.Container
}

func (f *fakeRuntime) List(ctx context.Context) ([]runtime.Container, error) {
	return f.containers, nil
}
func (f *fakeRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	for _, c := range f.containers {
		if c.ID == id {
			return c, nil
		}
	}
	return runtime.Container{}, fmt.Errorf("no container %q", id)
}
func (f *fakeRuntime) Watch(ctx context.Context) (<-chan runtime.Event, <-chan error) {
	return nil, nil
}
func (f *fakeRuntime) Exec(ctx context.Context, id string, spec runtime.ExecSpec) (*runtime.ExecHandle, error) {
	return nil, runtime.ErrNotImplemented
}
func (f *fakeRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error { return nil }
func (f *fakeRuntime) Start(ctx context.Context, id string) error                    { return nil }
func (f *fakeRuntime) Kill(ctx context.Context, id string, signal string) error      { return nil }
func (f *fakeRuntime) Restart(ctx context.Context, id string) error                  { return nil }
func (f *fakeRuntime) Close() error                                                  { return nil }

// TestGenerateConfigProducesYAML drives the generate core end to end with a fake
// runtime and the real otelcol backend (Render is pure, no socket), proving the
// discover -> assemble -> render path yields a non-empty collector config.
func TestGenerateConfigProducesYAML(t *testing.T) {
	rt := &fakeRuntime{containers: []runtime.Container{
		{
			ID:    "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccddddddddddddeeeeeeeeeeeeffff",
			Name:  "web",
			State: "running",
			Labels: map[string]string{
				"bilgeline.enable":      "true",
				"bilgeline.destination": "debug",
			},
			LogDriver: "json-file",
		},
	}}
	be := otelcol.New()
	cfg := &config.Config{SharedConfigPath: config.DefaultSharedConfigPath}

	data, err := generateConfig(context.Background(), rt, be, cfg, "")
	if err != nil {
		t.Fatalf("generateConfig: %v", err)
	}
	out := string(data)
	for _, want := range []string{"receivers:", "exporters:", "service:", "pipelines:"} {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("generated config should end in a newline")
	}
}

// TestValidateNonzeroOnBadConfig proves "validate --config-only" fails on an
// invalid config file, so it is usable as a pre-deploy gate. --config-only keeps
// it off the socket.
func TestValidateNonzeroOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bilgeline.yml")
	// An invalid destination type fails config.Validate.
	if err := os.WriteFile(bad, []byte("destinations:\n  central:\n    type: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd("test")
	root.SetArgs([]string{"validate", "--config-only", "--config", bad})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))

	if err := root.Execute(); err == nil {
		t.Fatal("validate must return a nonzero (error) result on an invalid config")
	}
}

// TestValidateConfigOnlyPassesOnGoodConfig is the positive counterpart: a valid
// config with --config-only exits clean.
func TestValidateConfigOnlyPassesOnGoodConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "bilgeline.yml")
	if err := os.WriteFile(good, []byte("destinations:\n  central:\n    type: otlphttp\n    endpoint: https://loki.example:4318\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd("test")
	root.SetArgs([]string{"validate", "--config-only", "--config", good})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))

	if err := root.Execute(); err != nil {
		t.Fatalf("validate --config-only on a good config: unexpected error %v", err)
	}
}
