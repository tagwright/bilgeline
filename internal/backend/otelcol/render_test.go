// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package otelcol

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/tagwright/bilgeline/internal/backend"
	"gopkg.in/yaml.v3"
)

// update regenerates the committed golden files instead of asserting against
// them. Run: go test ./internal/backend/otelcol -update
var update = flag.Bool("update", false, "regenerate golden files")

// destinations is the shared named-sink set the golden specs route to. loki
// carries a ${env:VAR} header to prove S1 passthrough: the reference is copied
// verbatim, never resolved.
var destinations = map[string]backend.Destination{
	"loki": {
		Name: "loki",
		Type: "loki",
		Settings: map[string]any{
			"endpoint": "http://loki:3100/otlp",
			"headers": map[string]any{
				"Authorization": "Bearer ${env:LOKI_BEARER}",
			},
		},
	},
	"archive": {
		Name:     "archive",
		Type:     "file",
		Settings: map[string]any{"path": "/var/log/archive/out.json"},
	},
	"es": {
		Name:     "es",
		Type:     "elasticsearch",
		Settings: map[string]any{"endpoint": "http://es:9200"},
	},
}

// svc builds a minimal docker-json-file ServiceSpec with sane defaults, which the
// per-case options then adjust.
func svc(id, name string, dests ...string) backend.ServiceSpec {
	routes := make([]backend.Route, 0, len(dests))
	for _, d := range dests {
		routes = append(routes, backend.Route{Destination: d})
	}
	return backend.ServiceSpec{
		ContainerID:   id,
		Name:          name,
		Source:        backend.SourceDockerJSONFile,
		ContainerName: name,
		Stream:        backend.StreamBoth,
		Parse:         backend.ParseNone,
		Routes:        routes,
	}
}

const (
	id1 = "1111111111111111111111111111111111111111111111111111111111111111"
	id2 = "2222222222222222222222222222222222222222222222222222222222222222"
	id3 = "3333333333333333333333333333333333333333333333333333333333333333"
)

func goldenCases() []struct {
	name string
	spec backend.Spec
} {
	// 1. minimal single service to loki.
	minimal := svc(id1, "web", "loki")

	// 2. JSON parse + level + drops + fan-out to two destinations.
	rich := svc(id1, "api", "loki", "archive")
	rich.ComposeProject = "shop"
	rich.ComposeService = "api"
	rich.Image = "shop/api:1.4"
	rich.Parse = backend.ParseJSON
	rich.LevelField = "level"
	rich.LevelMin = "warn"
	rich.Stream = backend.StreamStdout
	rich.Drop = []string{"GET /healthz", "kube-probe"}
	rich.StaticAttrs = map[string]string{"team": "payments", "tier": "edge"}

	// 3. a multiline group (stack-trace recombination, no body parse).
	multi := svc(id1, "worker", "archive")
	multi.Multiline = `^\d{4}-\d{2}-\d{2}`

	// 4. a profile raw-operators passthrough.
	raw := svc(id1, "legacy", "loki")
	raw.Profile = "legacy-syslog"
	raw.RawOperators = []map[string]any{
		{
			"type":  "add",
			"field": "attributes.source",
			"value": "legacy",
		},
		{
			"type":       "regex_parser",
			"parse_from": "body",
			"regex":      `^(?P<pri><\d+>)`,
		},
	}

	// 5. the none routed-nowhere case, mixed with a real route so the config is
	// not inert: one service to loki, one enabled but routed nowhere.
	routed := svc(id1, "shipper", "loki")
	nowhere := svc(id2, "silent") // no destinations

	// 6. two services sharing a signature: both plain JSON to loki. They must
	// collapse into ONE filelog receiver with two include paths.
	sharedA := svc(id1, "a", "loki")
	sharedA.Parse = backend.ParseJSON
	sharedB := svc(id2, "b", "loki")
	sharedB.Parse = backend.ParseJSON

	// 7. two services with different signatures: json vs logfmt. Two receivers.
	sigJSON := svc(id1, "j", "loki")
	sigJSON.Parse = backend.ParseJSON
	sigLogfmt := svc(id2, "l", "loki")
	sigLogfmt.Parse = backend.ParseLogfmt

	// 8. debug reserved sink plus an elasticsearch destination on distinct sets.
	dbg := svc(id1, "probe", "debug")
	toES := svc(id2, "store", "es")

	return []struct {
		name string
		spec backend.Spec
	}{
		{"minimal_loki", backend.Spec{Services: []backend.ServiceSpec{minimal}, Destinations: destinations}},
		{"json_level_drops_fanout", backend.Spec{Services: []backend.ServiceSpec{rich}, Destinations: destinations}},
		{"multiline_group", backend.Spec{Services: []backend.ServiceSpec{multi}, Destinations: destinations}},
		{"raw_operators", backend.Spec{Services: []backend.ServiceSpec{raw}, Destinations: destinations}},
		{"none_routed_nowhere", backend.Spec{Services: []backend.ServiceSpec{routed, nowhere}, Destinations: destinations}},
		{"shared_signature", backend.Spec{Services: []backend.ServiceSpec{sharedA, sharedB}, Destinations: destinations}},
		{"distinct_signatures", backend.Spec{Services: []backend.ServiceSpec{sigJSON, sigLogfmt}, Destinations: destinations}},
		{"debug_and_es", backend.Spec{Services: []backend.ServiceSpec{dbg, toES}, Destinations: destinations}},
		{"inert_empty", backend.Spec{Destinations: destinations}},
	}
}

func TestRenderGolden(t *testing.T) {
	b := New()
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.Render(tc.spec)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got.Format != "yaml" {
				t.Errorf("Format = %q, want yaml", got.Format)
			}
			golden := filepath.Join("testdata", tc.name+".yaml")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got.Data, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if !bytes.Equal(want, got.Data) {
				t.Errorf("rendered config does not match %s\n--- got ---\n%s", golden, got.Data)
			}
		})
	}
}

// TestRenderDeterministic renders each case twice and requires byte-identical
// output, since the daemon diffs successive renders.
func TestRenderDeterministic(t *testing.T) {
	b := New()
	for _, tc := range goldenCases() {
		a, err := b.Render(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c, err := b.Render(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !bytes.Equal(a.Data, c.Data) {
			t.Errorf("%s: render is not deterministic", tc.name)
		}
	}
}

// parseYAML decodes rendered config for structural assertions.
func parseYAML(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func section(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	s, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("section %q missing or not a map", key)
	}
	return s
}

// TestSharedSignatureOneReceiver proves same-signature services collapse to one
// receiver and distinct signatures do not.
func TestSharedSignatureOneReceiver(t *testing.T) {
	b := New()

	shared := backend.Spec{Destinations: destinations, Services: []backend.ServiceSpec{
		withParse(svc(id1, "a", "loki"), backend.ParseJSON),
		withParse(svc(id2, "b", "loki"), backend.ParseJSON),
	}}
	out, err := b.Render(shared)
	if err != nil {
		t.Fatal(err)
	}
	recv := section(t, parseYAML(t, out.Data), "receivers")
	if len(recv) != 1 {
		t.Fatalf("shared signature: got %d receivers, want 1", len(recv))
	}
	for _, v := range recv {
		inc, _ := v.(map[string]any)["include"].([]any)
		if len(inc) != 2 {
			t.Errorf("shared receiver should tail 2 files, got %d", len(inc))
		}
	}

	distinct := backend.Spec{Destinations: destinations, Services: []backend.ServiceSpec{
		withParse(svc(id1, "a", "loki"), backend.ParseJSON),
		withParse(svc(id2, "b", "loki"), backend.ParseLogfmt),
	}}
	out2, err := b.Render(distinct)
	if err != nil {
		t.Fatal(err)
	}
	if recv2 := section(t, parseYAML(t, out2.Data), "receivers"); len(recv2) != 2 {
		t.Fatalf("distinct signatures: got %d receivers, want 2", len(recv2))
	}
}

func withParse(s backend.ServiceSpec, p backend.ParseMode) backend.ServiceSpec {
	s.Parse = p
	return s
}

// TestFanOutPipelines proves one service to two destinations produces one out
// pipeline with two exporters, and env references pass through verbatim.
func TestFanOutPipelines(t *testing.T) {
	s := svc(id1, "api", "loki", "archive")
	out, err := New().Render(backend.Spec{Destinations: destinations, Services: []backend.ServiceSpec{s}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Data, []byte("Bearer ${env:LOKI_BEARER}")) {
		t.Error("env reference was not passed through verbatim")
	}
	m := parseYAML(t, out.Data)
	pipes := section(t, section(t, m, "service"), "pipelines")
	var outPipes int
	for name := range pipes {
		if name != "logs/ingest" {
			outPipes++
			exps, _ := pipes[name].(map[string]any)["exporters"].([]any)
			if len(exps) != 2 {
				t.Errorf("fan-out pipeline %q: got %d exporters, want 2", name, len(exps))
			}
		}
	}
	if outPipes != 1 {
		t.Errorf("got %d out pipelines, want 1", outPipes)
	}
}

// TestNoneRoutedNowhere proves an enabled-but-nowhere service is ingested but
// carries no routing key, so no out pipeline is created for it.
func TestNoneRoutedNowhere(t *testing.T) {
	out, err := New().Render(backend.Spec{Destinations: destinations, Services: []backend.ServiceSpec{
		svc(id1, "routed", "loki"),
		svc(id2, "silent"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	conn := section(t, parseYAML(t, out.Data), "connectors")
	table, _ := conn["routing"].(map[string]any)["table"].([]any)
	if len(table) != 1 {
		t.Errorf("routing table: got %d entries, want 1 (the silent service adds none)", len(table))
	}
}

// TestInertEmpty proves an empty spec renders a valid inert config with a nop
// pipeline.
func TestInertEmpty(t *testing.T) {
	out, err := New().Render(backend.Spec{Destinations: destinations})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out.Data)
	if _, ok := section(t, m, "receivers")["nop"]; !ok {
		t.Error("inert config should use the nop receiver")
	}
}

func TestUndefinedDestinationErrors(t *testing.T) {
	s := svc(id1, "x", "ghost")
	if _, err := New().Render(backend.Spec{Services: []backend.ServiceSpec{s}}); err == nil {
		t.Error("expected error for route to undefined destination")
	}
}

func TestUnsupportedTypeErrors(t *testing.T) {
	dests := map[string]backend.Destination{"weird": {Name: "weird", Type: "kafka"}}
	s := svc(id1, "x", "weird")
	if _, err := New().Render(backend.Spec{Services: []backend.ServiceSpec{s}, Destinations: dests}); err == nil {
		t.Error("expected error for unsupported destination type")
	}
}

func TestName(t *testing.T) {
	b := New()
	if b.Name() != "otelcol" {
		t.Errorf("Name = %q, want otelcol", b.Name())
	}
	// With no runtime wired, Apply and Healthy report the missing runtime rather
	// than panicking. The full apply-path behavior is covered in apply_test.go.
	if err := b.Healthy(context.Background()); err == nil {
		t.Error("Healthy with no runtime should error")
	}
}
