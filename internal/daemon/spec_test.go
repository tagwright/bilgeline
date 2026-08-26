// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"reflect"
	"testing"

	"github.com/tagwright/beacon"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/bilgeline/internal/discovery"
)

func TestFlattenDestination(t *testing.T) {
	d := config.Destination{
		Type:     "otlphttp",
		Endpoint: "https://loki.example:4318",
		Headers: map[string]string{
			// S1: an ${env:VAR} reference must pass through verbatim, unresolved.
			"Authorization": "Bearer ${env:LOKI_BEARER}",
		},
		Settings: map[string]any{
			"compression": "gzip",
			// An inline key that the typed Endpoint field must win over.
			"endpoint": "https://should-be-overridden",
		},
	}

	got := flattenDestination("central", d)

	if got.Name != "central" {
		t.Errorf("Name = %q, want central", got.Name)
	}
	if got.Type != "otlphttp" {
		t.Errorf("Type = %q, want otlphttp", got.Type)
	}
	// Typed endpoint wins over the inline settings key of the same name.
	if got.Settings["endpoint"] != "https://loki.example:4318" {
		t.Errorf("endpoint = %v, want the typed value", got.Settings["endpoint"])
	}
	if got.Settings["compression"] != "gzip" {
		t.Errorf("compression passthrough = %v, want gzip", got.Settings["compression"])
	}
	headers, ok := got.Settings["headers"].(map[string]string)
	if !ok {
		t.Fatalf("headers = %T, want map[string]string", got.Settings["headers"])
	}
	if headers["Authorization"] != "Bearer ${env:LOKI_BEARER}" {
		t.Errorf("env ref was not passed through verbatim: %q", headers["Authorization"])
	}
}

func TestFlattenDestinationFileSink(t *testing.T) {
	d := config.Destination{Type: "file", Path: "/var/log/out.json"}
	got := flattenDestination("archive", d)
	if got.Settings["path"] != "/var/log/out.json" {
		t.Errorf("path = %v, want /var/log/out.json", got.Settings["path"])
	}
	if _, present := got.Settings["endpoint"]; present {
		t.Errorf("endpoint should be absent for a file sink, got %v", got.Settings["endpoint"])
	}
	if _, present := got.Settings["headers"]; present {
		t.Errorf("headers should be absent when none are set")
	}
}

func TestAssembleSpecUnionAndDebugOmission(t *testing.T) {
	cfg := &config.Config{
		Destinations: map[string]config.Destination{
			"central": {Type: "otlphttp", Endpoint: "https://loki.example:4318"},
			"unused":  {Type: "file", Path: "/tmp/nope"},
		},
	}

	services := []backend.ServiceSpec{
		{
			ContainerID: "aaa",
			Name:        "web",
			Routes:      []backend.Route{{Destination: "central"}, {Destination: "debug"}},
		},
		{
			ContainerID: "bbb",
			Name:        "api",
			Routes:      []backend.Route{{Destination: "central"}},
		},
	}

	spec := AssembleSpec(services, cfg)

	if len(spec.Services) != 2 {
		t.Fatalf("Services len = %d, want 2", len(spec.Services))
	}
	// Only referenced, non-debug destinations are carried: "central" is in,
	// "unused" is out (not referenced), "debug" is out (renderer synthesizes it).
	if _, ok := spec.Destinations["central"]; !ok {
		t.Errorf("central should be in the spec destinations")
	}
	if _, ok := spec.Destinations["unused"]; ok {
		t.Errorf("unused destination must not be carried")
	}
	if _, ok := spec.Destinations["debug"]; ok {
		t.Errorf("debug must be omitted; the renderer synthesizes it")
	}
	if len(spec.Destinations) != 1 {
		t.Errorf("Destinations len = %d, want 1 (central only)", len(spec.Destinations))
	}
}

func TestAssembleSpecRoutedNowhere(t *testing.T) {
	cfg := &config.Config{Destinations: map[string]config.Destination{}}
	// A service opted in but routed nowhere carries no routes and must pull no
	// destination into the union.
	services := []backend.ServiceSpec{{ContainerID: "aaa", Name: "quiet"}}
	spec := AssembleSpec(services, cfg)
	if len(spec.Destinations) != 0 {
		t.Errorf("Destinations len = %d, want 0 for a route-nowhere service", len(spec.Destinations))
	}
}

func TestDiagLevel(t *testing.T) {
	if got := diagLevel(discovery.SeverityError); got != beacon.LevelError {
		t.Errorf("SeverityError -> %v, want LevelError", got)
	}
	if got := diagLevel(discovery.SeverityWarning); got != beacon.LevelWarning {
		t.Errorf("SeverityWarning -> %v, want LevelWarning", got)
	}
	// An unknown severity maps to the safe, non-error floor.
	if got := diagLevel(discovery.Severity("bogus")); got != beacon.LevelWarning {
		t.Errorf("unknown severity -> %v, want LevelWarning", got)
	}
}

// TestFlattenDestinationEmptyStable guards that an empty destination still yields
// a non-nil, empty settings map the renderer can range over safely.
func TestFlattenDestinationEmptyStable(t *testing.T) {
	got := flattenDestination("bare", config.Destination{Type: "debug"})
	if got.Settings == nil {
		t.Fatal("Settings must never be nil")
	}
	if !reflect.DeepEqual(got.Settings, map[string]any{}) {
		t.Errorf("Settings = %v, want empty map", got.Settings)
	}
}
