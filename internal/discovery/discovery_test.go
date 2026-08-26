// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
)

// sharedConfigYAML is a config with two destinations, a global promoted-label
// set, a default destination, and a regex profile, exercised across most tests.
const sharedConfigYAML = `
default_destination: loki
labels:
  - globalkey
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
  archive:
    type: file
    path: /archive/logs
profiles:
  springboot:
    multiline: '^\d{4}-\d{2}-\d{2}'
    parse:
      type: regex
      pattern: '^(?P<ts>\S+)\s+(?P<level>\w+)\s+(?P<msg>.*)$'
      level:
        field: level
    drop:
      - 'Tomcat started'
    attr:
      framework: spring
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bilgeline.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	cfg, err := config.Load(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// hasSeverity reports whether diags contains a diagnostic of the given severity.
func hasSeverity(diags []Diagnostic, sev Severity) bool {
	for _, d := range diags {
		if d.Severity == sev {
			return true
		}
	}
	return false
}

// routeSet flattens a spec's routes into a name->bool set.
func routeSet(spec *backend.ServiceSpec) map[string]bool {
	out := map[string]bool{}
	for _, r := range spec.Routes {
		out[r.Destination] = true
	}
	return out
}

// TestOptInGate covers the enable gate: absent and false are silent skips, true
// (with a valid destination) routes.
func TestOptInGate(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	cases := []struct {
		name     string
		labels   map[string]string
		wantSpec bool
	}{
		{"absent", map[string]string{"bilgeline.destination": "loki"}, false},
		{"false", map[string]string{"bilgeline.enable": "false", "bilgeline.destination": "loki"}, false},
		{"true", map[string]string{"bilgeline.enable": "true", "bilgeline.destination": "loki"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: tc.labels}, cfg, "self")
			if (spec != nil) != tc.wantSpec {
				t.Fatalf("spec != nil = %v, want %v (diags=%v)", spec != nil, tc.wantSpec, diags)
			}
			if !tc.wantSpec && len(diags) != 0 {
				t.Errorf("silent skip should emit no diagnostics, got %v", diags)
			}
		})
	}
}

// TestPrefixConflict proves the same suffix under both prefixes with different
// values is an error (skip), while identical values are harmless.
func TestPrefixConflict(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	t.Run("different values error", func(t *testing.T) {
		spec, diags := Resolve(Candidate{
			ID:   "c1",
			Name: "app",
			Labels: map[string]string{
				"bilgeline.enable":          "true",
				"bilgeline.destination":     "loki",
				"tagwright.log.destination": "archive",
			},
		}, cfg, "self")
		if spec != nil {
			t.Fatalf("conflicting labels must skip, got spec %+v", spec)
		}
		if !hasSeverity(diags, SeverityError) {
			t.Fatalf("want an error diagnostic, got %v", diags)
		}
	})

	t.Run("same value harmless", func(t *testing.T) {
		spec, diags := Resolve(Candidate{
			ID:   "c1",
			Name: "app",
			Labels: map[string]string{
				"bilgeline.enable":          "true",
				"bilgeline.destination":     "loki",
				"tagwright.log.destination": "loki",
			},
		}, cfg, "self")
		if spec == nil {
			t.Fatalf("identical values under both prefixes must route, got skip: %v", diags)
		}
		if !routeSet(spec)["loki"] {
			t.Errorf("routes = %v, want loki", spec.Routes)
		}
	})
}

// TestNameDefaultChain covers name override, compose-service fallback, and
// container-name fallback.
func TestNameDefaultChain(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	cases := []struct {
		name string
		cand Candidate
		want string
	}{
		{
			"override wins",
			Candidate{ID: "c1", Name: "ctr", ComposeService: "svc", Labels: map[string]string{
				"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.name": "chosen",
			}},
			"chosen",
		},
		{
			"compose service",
			Candidate{ID: "c1", Name: "ctr", ComposeService: "svc", Labels: map[string]string{
				"bilgeline.enable": "true", "bilgeline.destination": "loki",
			}},
			"svc",
		},
		{
			"container name",
			Candidate{ID: "c1", Name: "ctr", Labels: map[string]string{
				"bilgeline.enable": "true", "bilgeline.destination": "loki",
			}},
			"ctr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Resolve(tc.cand, cfg, "self")
			if spec == nil {
				t.Fatalf("want spec, got skip: %v", diags)
			}
			if spec.Name != tc.want {
				t.Errorf("Name = %q, want %q", spec.Name, tc.want)
			}
		})
	}
}

// TestDestinationCSVAndFallback covers csv fan-out and the default fallback.
func TestDestinationCSVAndFallback(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	t.Run("csv fans out", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "loki,archive",
		}}, cfg, "self")
		if spec == nil {
			t.Fatalf("want spec, got skip: %v", diags)
		}
		set := routeSet(spec)
		if !set["loki"] || !set["archive"] {
			t.Errorf("routes = %v, want loki and archive", spec.Routes)
		}
		if got := spec.DestinationNames(); len(got) != 2 {
			t.Errorf("DestinationNames = %v, want two", got)
		}
	})

	t.Run("default fallback", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true",
		}}, cfg, "self")
		if spec == nil {
			t.Fatalf("want spec, got skip: %v", diags)
		}
		if !routeSet(spec)["loki"] {
			t.Errorf("routes = %v, want the default loki", spec.Routes)
		}
	})

	t.Run("debug always valid", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "debug",
		}}, cfg, "self")
		if spec == nil {
			t.Fatalf("debug is reserved and always valid, got skip: %v", diags)
		}
		if !routeSet(spec)["debug"] {
			t.Errorf("routes = %v, want debug", spec.Routes)
		}
	})
}

// TestUnknownDestination proves an undefined destination name is a skip.
func TestUnknownDestination(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "nope",
	}}, cfg, "self")
	if spec != nil {
		t.Fatalf("unknown destination must skip, got spec %+v", spec)
	}
	if !hasSeverity(diags, SeverityError) {
		t.Fatalf("want an error diagnostic, got %v", diags)
	}
}

// TestNoDestinationNoDefault proves that naming no destination with no default
// configured anywhere is an error.
func TestNoDestinationNoDefault(t *testing.T) {
	cfg := loadConfig(t, `
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
`)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable": "true",
	}}, cfg, "self")
	if spec != nil {
		t.Fatalf("no destination and no default must skip, got spec %+v", spec)
	}
	if !hasSeverity(diags, SeverityError) {
		t.Fatalf("want an error diagnostic, got %v", diags)
	}
}

// TestNoneRoutedNowhere proves the none sentinel yields a valid spec with no
// routes, distinct from being disabled.
func TestNoneRoutedNowhere(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "none",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("none is enabled-but-routed-nowhere, want a spec, got skip: %v", diags)
	}
	if len(spec.Routes) != 0 {
		t.Errorf("routes = %v, want none", spec.Routes)
	}
	if hasSeverity(diags, SeverityError) {
		t.Errorf("none is valid, want no error diagnostic, got %v", diags)
	}
}

// TestInvalidRegexRejected proves an uncompilable multiline or drop regex is a
// skip.
func TestInvalidRegexRejected(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	cases := []struct {
		name  string
		label string
	}{
		{"multiline", "bilgeline.multiline"},
		{"drop", "bilgeline.drop"},
		{"indexed drop", "bilgeline.drop.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
				"bilgeline.enable": "true", "bilgeline.destination": "loki", tc.label: "(unclosed",
			}}, cfg, "self")
			if spec != nil {
				t.Fatalf("invalid regex must skip, got spec %+v", spec)
			}
			if !hasSeverity(diags, SeverityError) {
				t.Fatalf("want an error diagnostic, got %v", diags)
			}
		})
	}
}

// TestIndexedAndCSVDrop proves the csv, the indexed escape hatch, and profile
// drops all land, unioned and deduplicated.
func TestIndexedAndCSVDrop(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable":      "true",
		"bilgeline.destination": "loki",
		"bilgeline.drop":        "GET /healthz",
		"bilgeline.drop.0":      "connection reset, by peer",
		"bilgeline.drop.1":      "GET /healthz",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec, got skip: %v", diags)
	}
	got := map[string]bool{}
	for _, d := range spec.Drop {
		got[d] = true
	}
	if !got["GET /healthz"] || !got["connection reset, by peer"] {
		t.Errorf("Drop = %v, want both the csv and indexed patterns", spec.Drop)
	}
	if len(spec.Drop) != 2 {
		t.Errorf("Drop = %v, want exactly 2 after dedup", spec.Drop)
	}
}

// TestProfileScalarOverride proves an explicit label beats the profile's scalar
// (parse), while the profile still supplies fields the label does not set
// (multiline).
func TestProfileScalarOverride(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable":      "true",
		"bilgeline.destination": "loki",
		"bilgeline.profile":     "springboot",
		"bilgeline.parse":       "json",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec, got skip: %v", diags)
	}
	if spec.Parse != backend.ParseJSON {
		t.Errorf("Parse = %q, want json (label overrides the profile's regex)", spec.Parse)
	}
	if spec.ParsePattern != "" {
		t.Errorf("ParsePattern = %q, want empty (the profile regex is overridden)", spec.ParsePattern)
	}
	if spec.Multiline == "" {
		t.Errorf("Multiline is empty, want the profile's pattern to survive")
	}
	if spec.Profile != "springboot" {
		t.Errorf("Profile = %q, want springboot", spec.Profile)
	}
}

// TestProfileDropUnion proves the profile's drops union with the container's own
// drop label.
func TestProfileDropUnion(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable":      "true",
		"bilgeline.destination": "loki",
		"bilgeline.profile":     "springboot",
		"bilgeline.drop":        "GET /healthz",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec, got skip: %v", diags)
	}
	got := map[string]bool{}
	for _, d := range spec.Drop {
		got[d] = true
	}
	if !got["GET /healthz"] || !got["Tomcat started"] {
		t.Errorf("Drop = %v, want the label drop unioned with the profile drop", spec.Drop)
	}
}

// TestProfileAttrMerge proves the profile's static attributes land and a label
// attr overrides the profile on a key collision.
func TestProfileAttrMerge(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable":         "true",
		"bilgeline.destination":    "loki",
		"bilgeline.profile":        "springboot",
		"bilgeline.attr.tier":      "backend",
		"bilgeline.attr.framework": "spring-boot-3",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec, got skip: %v", diags)
	}
	if spec.StaticAttrs["tier"] != "backend" {
		t.Errorf("StaticAttrs[tier] = %q, want backend", spec.StaticAttrs["tier"])
	}
	if spec.StaticAttrs["framework"] != "spring-boot-3" {
		t.Errorf("StaticAttrs[framework] = %q, want the label to override the profile", spec.StaticAttrs["framework"])
	}
}

// TestPromotedLabels covers the union of the global set with the container's own
// labels, and the none suppression.
func TestPromotedLabels(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	t.Run("union with global", func(t *testing.T) {
		spec, _ := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.labels": "custom.key",
		}}, cfg, "self")
		got := map[string]bool{}
		for _, l := range spec.PromotedLabels {
			got[l] = true
		}
		if !got["globalkey"] || !got["custom.key"] {
			t.Errorf("PromotedLabels = %v, want the global plus the container key", spec.PromotedLabels)
		}
	})

	t.Run("none suppresses global", func(t *testing.T) {
		spec, _ := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.labels": "none",
		}}, cfg, "self")
		for _, l := range spec.PromotedLabels {
			if l == "globalkey" {
				t.Errorf("PromotedLabels = %v, want the global set suppressed by none", spec.PromotedLabels)
			}
		}
	})

	t.Run("none keeps other explicit keys", func(t *testing.T) {
		spec, _ := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.labels": "none,keepme",
		}}, cfg, "self")
		got := map[string]bool{}
		for _, l := range spec.PromotedLabels {
			got[l] = true
		}
		if got["globalkey"] || !got["keepme"] {
			t.Errorf("PromotedLabels = %v, want [keepme] with globalkey suppressed", spec.PromotedLabels)
		}
	})
}

// TestSelfAndCollectorExclusion proves bilgeline's own container and the
// collector marker are never routed, even with enable=true, and that the
// degenerate enable=true case emits a warning.
func TestSelfAndCollectorExclusion(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)

	t.Run("self excluded", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "self123", Name: "bilgeline", Labels: map[string]string{
			"bilgeline.enable": "true", "bilgeline.destination": "loki",
		}}, cfg, "self123")
		if spec != nil {
			t.Fatalf("self must never route, got spec %+v", spec)
		}
		if !hasSeverity(diags, SeverityWarning) {
			t.Errorf("enable=true on self should warn, got %v", diags)
		}
	})

	t.Run("collector excluded", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "c1", Name: "otelcol", Labels: map[string]string{
			"bilgeline.collector": "true", "bilgeline.enable": "true", "bilgeline.destination": "loki",
		}}, cfg, "self")
		if spec != nil {
			t.Fatalf("collector must never route, got spec %+v", spec)
		}
		if !hasSeverity(diags, SeverityWarning) {
			t.Errorf("enable=true on collector should warn, got %v", diags)
		}
	})

	t.Run("collector via alias, no enable, silent", func(t *testing.T) {
		spec, diags := Resolve(Candidate{ID: "c1", Name: "otelcol", Labels: map[string]string{
			"tagwright.log.collector": "true",
		}}, cfg, "self")
		if spec != nil {
			t.Fatalf("collector must never route, got spec %+v", spec)
		}
		if len(diags) != 0 {
			t.Errorf("collector without enable should be silent, got %v", diags)
		}
	})
}

// TestLevelMinWithoutExtraction proves a severity floor with parse=none routes
// the container but emits a warning.
func TestLevelMinWithoutExtraction(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.level.min": "info",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec (routed with a caveat), got skip: %v", diags)
	}
	if !hasSeverity(diags, SeverityWarning) {
		t.Errorf("level.min with parse=none should warn, got %v", diags)
	}
}

// TestNonJSONFileLogDriverExcluded proves a container on a non-json-file driver
// is excluded from routing with a warning.
func TestNonJSONFileLogDriverExcluded(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", LogDriver: "local", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "loki",
	}}, cfg, "self")
	if spec != nil {
		t.Fatalf("a non-json-file driver must be excluded, got spec %+v", spec)
	}
	if !hasSeverity(diags, SeverityWarning) {
		t.Errorf("non-json-file driver should warn, got %v", diags)
	}
}

// TestEmptyLogDriverRoutes proves an unknown (empty) log driver is treated as
// json-file compatible so a stock host routes normally.
func TestEmptyLogDriverRoutes(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", LogDriver: "", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "loki",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("empty log driver should route (assumed json-file), got skip: %v", diags)
	}
}

// TestParseEnumRejected proves a parse value outside the enum is a skip.
func TestParseEnumRejected(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"bilgeline.enable": "true", "bilgeline.destination": "loki", "bilgeline.parse": "xml",
	}}, cfg, "self")
	if spec != nil {
		t.Fatalf("an out-of-enum parse must skip, got spec %+v", spec)
	}
	if !hasSeverity(diags, SeverityError) {
		t.Fatalf("want an error diagnostic, got %v", diags)
	}
}

// TestAliasPrefixParsed proves the tagwright.log.* alias is honored on its own.
func TestAliasPrefixParsed(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{ID: "c1", Name: "app", Labels: map[string]string{
		"tagwright.log.enable": "true", "tagwright.log.destination": "archive",
	}}, cfg, "self")
	if spec == nil {
		t.Fatalf("alias-only labels must route, got skip: %v", diags)
	}
	if !routeSet(spec)["archive"] {
		t.Errorf("routes = %v, want archive via the alias", spec.Routes)
	}
}

// TestIdentityFieldsStamped proves the auto-attribute identity fields are
// carried onto the spec for the renderer.
func TestIdentityFieldsStamped(t *testing.T) {
	cfg := loadConfig(t, sharedConfigYAML)
	spec, diags := Resolve(Candidate{
		ID: "abc", Name: "ctr", ComposeProject: "proj", ComposeService: "svc", Image: "img:1",
		Labels: map[string]string{"bilgeline.enable": "true", "bilgeline.destination": "loki"},
	}, cfg, "self")
	if spec == nil {
		t.Fatalf("want spec, got skip: %v", diags)
	}
	if spec.ContainerID != "abc" || spec.ContainerName != "ctr" ||
		spec.ComposeProject != "proj" || spec.ComposeService != "svc" || spec.Image != "img:1" {
		t.Errorf("identity fields not stamped: %+v", spec)
	}
	if spec.Source != backend.SourceDockerJSONFile {
		t.Errorf("Source = %q, want %q", spec.Source, backend.SourceDockerJSONFile)
	}
}
