// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeConfig writes contents to a temp bilgeline.yml and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bilgeline.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		check func(t *testing.T, c *Config)
	}{
		{
			name: "minimal single destination",
			yaml: `
collector: otelcol
default_destination: loki
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
`,
			check: func(t *testing.T, c *Config) {
				if c.Collector != "otelcol" {
					t.Errorf("Collector = %q, want otelcol", c.Collector)
				}
				if c.DefaultDestination != "loki" {
					t.Errorf("DefaultDestination = %q, want loki", c.DefaultDestination)
				}
				if got := c.Destinations["loki"].Type; got != "otlphttp" {
					t.Errorf("loki type = %q, want otlphttp", got)
				}
				if c.SharedConfigPath != DefaultSharedConfigPath {
					t.Errorf("SharedConfigPath = %q, want default %q", c.SharedConfigPath, DefaultSharedConfigPath)
				}
				if c.Debounce != DefaultDebounce {
					t.Errorf("Debounce = %q, want default %q", c.Debounce, DefaultDebounce)
				}
			},
		},
		{
			name: "fan-out multi destination with env-ref header",
			yaml: `
default_destination: loki
shared_config_path: /shared/otelcol.yaml
debounce: 5s
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
    headers:
      Authorization: "Bearer ${env:LOKI_BEARER}"
  archive:
    type: file
    path: /archive/logs
  debug:
    type: debug
`,
			check: func(t *testing.T, c *Config) {
				if len(c.Destinations) != 3 {
					t.Fatalf("len(Destinations) = %d, want 3", len(c.Destinations))
				}
				if c.SharedConfigPath != "/shared/otelcol.yaml" {
					t.Errorf("SharedConfigPath = %q", c.SharedConfigPath)
				}
				if got := c.Destinations["loki"].Headers["Authorization"]; got != "Bearer ${env:LOKI_BEARER}" {
					t.Errorf("Authorization header = %q, want the env-ref copied verbatim", got)
				}
				if got := c.Destinations["archive"].Path; got != "/archive/logs" {
					t.Errorf("archive path = %q", got)
				}
				d, err := c.DebounceDuration()
				if err != nil {
					t.Fatalf("DebounceDuration: %v", err)
				}
				if d != 5*time.Second {
					t.Errorf("DebounceDuration = %v, want 5s", d)
				}
			},
		},
		{
			name: "profile with raw operators passthrough",
			yaml: `
default_destination: loki
destinations:
  loki:
    type: loki
    endpoint: https://loki.home.lan
profiles:
  springboot:
    multiline: '^\d{4}-\d{2}-\d{2}'
    parse:
      type: regex
      pattern: '^(?P<ts>\S+ \S+)\s+(?P<level>\w+)\s+(?P<msg>.*)$'
      timestamp:
        field: ts
        layout: '%Y-%m-%d %H:%M:%S'
      level:
        field: level
        mapping:
          WARNING: warn
    drop:
      - 'Tomcat started on port'
    attr:
      tier: backend
    operators:
      - type: add
        field: attributes.custom
        value: fixed
`,
			check: func(t *testing.T, c *Config) {
				p, ok := c.Profiles["springboot"]
				if !ok {
					t.Fatal("springboot profile missing")
				}
				if p.Parse == nil || p.Parse.Type != "regex" {
					t.Fatalf("parse spec = %+v, want regex", p.Parse)
				}
				if p.Parse.Timestamp == nil || p.Parse.Timestamp.Field != "ts" {
					t.Errorf("timestamp = %+v", p.Parse.Timestamp)
				}
				if p.Parse.Level == nil || p.Parse.Level.Mapping["WARNING"] != "warn" {
					t.Errorf("level mapping = %+v", p.Parse.Level)
				}
				if len(p.Operators) != 1 {
					t.Fatalf("len(Operators) = %d, want 1", len(p.Operators))
				}
				if p.Operators[0]["type"] != "add" {
					t.Errorf("operator[0].type = %v, want add", p.Operators[0]["type"])
				}
				if p.Attr["tier"] != "backend" {
					t.Errorf("attr tier = %q", p.Attr["tier"])
				}
			},
		},
		{
			name: "inline passthrough settings on a destination",
			yaml: `
default_destination: es
destinations:
  es:
    type: elasticsearch
    endpoint: https://es.home.lan:9200
    logs_index: bilgeline-logs
    tls:
      insecure: false
`,
			check: func(t *testing.T, c *Config) {
				es := c.Destinations["es"]
				if es.Settings["logs_index"] != "bilgeline-logs" {
					t.Errorf("logs_index = %v, want bilgeline-logs", es.Settings["logs_index"])
				}
				if _, ok := es.Settings["tls"]; !ok {
					t.Errorf("tls block not captured in inline Settings: %+v", es.Settings)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.SharedConfigPath != DefaultSharedConfigPath {
		t.Errorf("SharedConfigPath = %q, want default", cfg.SharedConfigPath)
	}
	if cfg.Debounce != DefaultDebounce {
		t.Errorf("Debounce = %q, want default", cfg.Debounce)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if err != nil {
		t.Fatalf("Load(missing): want no error, got %v", err)
	}
	if cfg.SharedConfigPath != DefaultSharedConfigPath {
		t.Errorf("SharedConfigPath = %q, want default", cfg.SharedConfigPath)
	}
}

func TestEnvOverlayPrecedence(t *testing.T) {
	yaml := `
default_destination: loki
debounce: 2s
labels:
  - com.docker.compose.project
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
  archive:
    type: file
    path: /archive/logs
`
	t.Setenv("BILGELINE_DEFAULT_DESTINATION", "archive")
	t.Setenv("BILGELINE_DEBOUNCE", "10s")
	t.Setenv("BILGELINE_LABELS", "org.opencontainers.image.version, com.docker.compose.project")

	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultDestination != "archive" {
		t.Errorf("DefaultDestination = %q, want archive (env override)", cfg.DefaultDestination)
	}
	if cfg.Debounce != "10s" {
		t.Errorf("Debounce = %q, want 10s (env override)", cfg.Debounce)
	}
	// BILGELINE_LABELS is additive and de-duplicated: the file's project key
	// stays, the new version key is appended, the duplicate project key is not
	// repeated.
	want := []string{"com.docker.compose.project", "org.opencontainers.image.version"}
	if !reflect.DeepEqual(cfg.Labels, want) {
		t.Errorf("Labels = %v, want %v", cfg.Labels, want)
	}
}

func TestValidateFailures(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "invalid destination type",
			yaml: `
destinations:
  bad:
    type: kafka
`,
		},
		{
			name: "default_destination is none",
			yaml: `
default_destination: none
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
`,
		},
		{
			name: "default_destination names undefined destination",
			yaml: `
default_destination: nowhere
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
`,
		},
		{
			name: "profile shadows reserved word",
			yaml: `
destinations:
  loki:
    type: loki
    endpoint: https://loki.home.lan
profiles:
  json:
    multiline: '^x'
`,
		},
		{
			name: "unparseable debounce",
			yaml: `
debounce: soon
destinations:
  loki:
    type: loki
    endpoint: https://loki.home.lan
`,
		},
		{
			name: "destination missing type",
			yaml: `
destinations:
  bare:
    endpoint: https://x.home.lan
`,
		},
		{
			name: "notification unknown type",
			yaml: `
notifications:
  - type: carrierpigeon
    settings:
      topic: x
`,
		},
		{
			name: "notification missing type",
			yaml: `
notifications:
  - min_level: warn
    settings:
      topic: x
`,
		},
		{
			name: "notification bad min_level",
			yaml: `
notifications:
  - type: ntfy
    min_level: screaming
    settings:
      topic: x
`,
		},
		{
			name: "telemetry unknown type",
			yaml: `
telemetry:
  - type: prometheus
    settings:
      url: https://x.home.lan
`,
		},
		{
			name: "telemetry missing type",
			yaml: `
telemetry:
  - settings:
      url: https://x.home.lan
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, tt.yaml)); err == nil {
				t.Fatal("Load: want validation error, got nil")
			}
		})
	}
}

// TestLoadNotificationsTelemetry pins the notifications, telemetry, and
// secrets_dir surface: the sections parse, credential values stay secret NAMES
// (never expanded here), and known types plus levels pass validation.
func TestLoadNotificationsTelemetry(t *testing.T) {
	yaml := `
secrets_dir: /run/bilgeline/secrets
notifications:
  - type: ntfy
    min_level: warn
    settings:
      server: https://ntfy.home.lan
      topic: bilgeline-alerts
      token_secret: ntfy-bilgeline-token
  - type: discord
    min_level: error
    settings:
      webhook_secret: discord-bilgeline-webhook
telemetry:
  - type: gatus
    settings:
      url: https://status.home.lan
      endpoint_key: infra_bilgeline
      token_secret: gatus-bilgeline-push-token
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SecretsDir != "/run/bilgeline/secrets" {
		t.Errorf("SecretsDir = %q, want /run/bilgeline/secrets", c.SecretsDir)
	}
	if len(c.Notifications) != 2 {
		t.Fatalf("Notifications len = %d, want 2", len(c.Notifications))
	}
	ntfy := c.Notifications[0]
	if ntfy.Type != "ntfy" || ntfy.MinLevel != "warn" {
		t.Errorf("channel[0] = %+v, want ntfy/warn", ntfy)
	}
	// The token is a secret NAME, carried through verbatim, not resolved here.
	if got := ntfy.Settings["token_secret"]; got != "ntfy-bilgeline-token" {
		t.Errorf("channel[0] token_secret = %q, want the literal secret name", got)
	}
	if len(c.Telemetry) != 1 || c.Telemetry[0].Type != "gatus" {
		t.Fatalf("Telemetry = %+v, want one gatus sink", c.Telemetry)
	}
	if got := c.Telemetry[0].Settings["endpoint_key"]; got != "infra_bilgeline" {
		t.Errorf("gatus endpoint_key = %q, want infra_bilgeline", got)
	}
}

// TestSecretsDirEnvOverlay pins that BILGELINE_SECRETS_DIR overrides the file's
// secrets_dir (domain-2 alerting secrets only).
func TestSecretsDirEnvOverlay(t *testing.T) {
	yaml := "secrets_dir: /from/file\n"
	t.Setenv("BILGELINE_SECRETS_DIR", "/from/env")
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SecretsDir != "/from/env" {
		t.Errorf("SecretsDir = %q, want /from/env (env wins over file)", c.SecretsDir)
	}
}

// TestDefaultDestinationDebug pins that the reserved "debug" sink is a valid
// default_destination even when the destinations map does not define it, the
// same always-valid treatment discovery gives a per-container debug route. This
// guards the config-vs-discovery consistency an integration run caught: bilgeline
// refused to start on default_destination: debug though a container routing to
// debug was fine.
func TestDefaultDestinationDebug(t *testing.T) {
	yaml := `
default_destination: debug
shared_config_path: /config/otelcol.yaml
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: default_destination: debug should be valid, got %v", err)
	}
	if c.DefaultDestination != "debug" {
		t.Errorf("DefaultDestination = %q, want debug", c.DefaultDestination)
	}
}

func TestValidateAggregatesMultiple(t *testing.T) {
	// Two independent faults: a bad type and an undefined default. Validate
	// should surface both, not stop at the first.
	c := &Config{
		Debounce:           DefaultDebounce,
		DefaultDestination: "nowhere",
		Destinations: map[string]Destination{
			"bad": {Type: "kafka"},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate: want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"kafka", "nowhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error %q missing %q", msg, want)
		}
	}
}

func TestEnvRefs(t *testing.T) {
	yaml := `
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
    headers:
      Authorization: "Bearer ${env:LOKI_BEARER}"
      X-Scope-OrgID: "${env:LOKI_TENANT}"
  es:
    type: elasticsearch
    endpoint: "https://${env:ES_HOST}:9200"
    auth:
      password: "${env:ES_PASSWORD}"
  archive:
    type: file
    path: /archive/logs
`
	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.EnvRefs()
	want := []string{"ES_HOST", "ES_PASSWORD", "LOKI_BEARER", "LOKI_TENANT"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnvRefs = %v, want %v", got, want)
	}
}
