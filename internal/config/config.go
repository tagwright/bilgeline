// SPDX-License-Identifier: GPL-3.0-or-later

// Package config loads bilgeline's daemon configuration from bilgeline.yml:
// the collector identity fallback, the named destinations logs are routed to,
// the named processing profiles that back the label grammar's escape hatch, and
// the handful of fleet-wide globals. The file is optional; the surviving
// BILGELINE_* environment variables overlay onto it, so env-only operation
// works with no file at all.
//
// Config never resolves a secret. Under the S1 secrets model, destination
// settings may carry literal ${env:VAR} reference strings (e.g. an
// Authorization header of "Bearer ${env:LOKI_BEARER}"). bilgeline copies those
// verbatim into the generated collector config and never expands them; the user
// provisions the matching env vars on the collector container. There is no
// secrets directory and no secret resolution here, by design.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is bilgeline's daemon configuration, loaded from bilgeline.yml.
type Config struct {
	// Collector is the FALLBACK collector container identity, used only when no
	// container carries the primary bilgeline.collector=true marker label. Empty
	// is valid: the marker is the primary mechanism.
	Collector string `yaml:"collector,omitempty"`

	// DefaultDestination is the destination name a routed container falls back to
	// when it names none. It must, if set, name a destination defined in
	// Destinations, and must not be "none" (the reserved route-nowhere
	// sentinel). Overridable by BILGELINE_DEFAULT_DESTINATION.
	DefaultDestination string `yaml:"default_destination,omitempty"`

	// SharedConfigPath is where bilgeline writes the generated collector config,
	// on the volume shared read-only into the collector container. Defaults to
	// DefaultSharedConfigPath.
	SharedConfigPath string `yaml:"shared_config_path,omitempty"`

	// Debounce is the event-coalescing quiet window applied to socket churn
	// before regenerating, as a Go duration string (e.g. "2s"). Defaults to
	// DefaultDebounce. Overridable by BILGELINE_DEBOUNCE. Parse it with
	// DebounceDuration; Validate guarantees it parses.
	Debounce string `yaml:"debounce,omitempty"`

	// Labels is the fleet-wide set of container label keys promoted to resource
	// attributes on every routed container, additive to each container's own
	// bilgeline.labels. BILGELINE_LABELS unions additional keys onto this set.
	Labels []string `yaml:"labels,omitempty"`

	// Destinations is the named sink map keyed by destination name. A container
	// selects one or more by name via its bilgeline.destination label.
	Destinations map[string]Destination `yaml:"destinations,omitempty"`

	// Profiles is the named processing-profile map keyed by profile name, the
	// grammar's per-container escape hatch. A container selects one by name via
	// its bilgeline.profile label. A profile name must not shadow a reserved
	// parser name (see ReservedParserNames).
	Profiles map[string]Profile `yaml:"profiles,omitempty"`
}

// Destination is one named sink from the "destinations" map in bilgeline.yml. A
// container selects it by name via bilgeline.destination.
//
// A typed core holds the fields common across sink types; anything else a sink
// needs is captured by the inline Settings passthrough. Under S1, any string
// value here may contain literal ${env:VAR} references, copied verbatim into
// the generated config and never resolved by bilgeline.
type Destination struct {
	// Type is the sink kind, one of the values in DestinationTypes:
	// otlphttp, otlp, loki, elasticsearch, file, debug.
	Type string `yaml:"type"`

	// Endpoint is the sink's URL, for the network sink types.
	Endpoint string `yaml:"endpoint,omitempty"`

	// Path is the on-disk target, for the file sink type.
	Path string `yaml:"path,omitempty"`

	// Headers are request headers for the network sink types. Values may carry
	// ${env:VAR} references (e.g. a bearer token), copied through verbatim.
	Headers map[string]string `yaml:"headers,omitempty"`

	// Settings captures any additional sink-native settings not covered by the
	// typed fields above (compression, tls blocks, retry tuning, and so on),
	// inlined at the destination's top level. Values may carry ${env:VAR}
	// references, copied through verbatim.
	Settings map[string]any `yaml:",inline"`
}

// Profile is one named processing profile from the "profiles" map in
// bilgeline.yml, the label grammar's F6 escape hatch. Every field is optional;
// explicit labels on a container override the profile's corresponding
// contribution.
type Profile struct {
	// Parse declares structured-log parsing beyond the label enum (a regex with
	// named groups, a timestamp layout, a custom level mapping).
	Parse *ParseSpec `yaml:"parse,omitempty"`

	// Multiline is the "a new entry starts here" regex recombining continuation
	// lines.
	Multiline string `yaml:"multiline,omitempty"`

	// Drop are regexes; a record matching any is dropped.
	Drop []string `yaml:"drop,omitempty"`

	// Attr are static resource attributes stamped verbatim.
	Attr map[string]string `yaml:"attr,omitempty"`

	// Operators is a verbatim passthrough of collector-native stanza operators
	// for the genuinely exotic case. bilgeline splices each map in as-is and
	// does not interpret it.
	Operators []map[string]any `yaml:"operators,omitempty"`
}

// ParseSpec is a profile's structured-log parsing declaration.
type ParseSpec struct {
	// Type is the parser kind, e.g. "regex", "json", "logfmt".
	Type string `yaml:"type,omitempty"`

	// Pattern is the named-group regex used when Type is "regex".
	Pattern string `yaml:"pattern,omitempty"`

	// Timestamp optionally extracts a record timestamp from a parsed field.
	Timestamp *TimestampSpec `yaml:"timestamp,omitempty"`

	// Level optionally maps a parsed field to a log severity.
	Level *LevelSpec `yaml:"level,omitempty"`
}

// TimestampSpec extracts a record timestamp from a parsed field.
type TimestampSpec struct {
	// Field is the parsed field holding the timestamp.
	Field string `yaml:"field,omitempty"`
	// Layout is the strptime layout the field is parsed with.
	Layout string `yaml:"layout,omitempty"`
}

// LevelSpec maps a parsed field to a log severity.
type LevelSpec struct {
	// Field is the parsed field holding the level.
	Field string `yaml:"field,omitempty"`
	// Mapping is a custom level-string to severity mapping layered over the
	// collector's built-in aliases.
	Mapping map[string]string `yaml:"mapping,omitempty"`
}

// DestinationTypes is the set of valid destination Type values for v1.
var DestinationTypes = []string{"otlphttp", "otlp", "loki", "elasticsearch", "file", "debug"}

// ReservedParserNames are the built-in parser names a profile name must not
// shadow. Mirrors the label grammar's reserved words.
var ReservedParserNames = []string{"json", "logfmt", "none", "auto", "debug"}

// Defaults applied to any Config that leaves the field unset.
const (
	// DefaultSharedConfigPath is where the generated collector config is written
	// on the shared volume when shared_config_path is unset.
	DefaultSharedConfigPath = "/config/otelcol.yaml"

	// DefaultDebounce is the event-coalescing window when debounce is unset.
	DefaultDebounce = "2s"

	// noneSentinel is the reserved "route nowhere" / "explicitly nothing"
	// value. It is never a valid default_destination.
	noneSentinel = "none"
)

// envRefPattern matches a ${env:NAME} reference and captures NAME. NAME follows
// the conventional environment-variable identifier shape.
var envRefPattern = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the YAML config at path, overlays the surviving BILGELINE_*
// environment variables (env wins over the file), applies defaults, and
// validates the result. A valid *Config is returned only when Validate passes.
//
// path is optional: an empty path, or a path that does not exist, is not an
// error. Load returns a defaulted, validated Config in that case, so env-only
// operation with no file at all works.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: read %s: %w", path, err)
			}
		} else if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	overlayEnv(cfg)
	applyDefaults(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// overlayEnv applies the surviving BILGELINE_* globals. Any variable that is set
// wins over the file (or the zero value). BILGELINE_LABELS is additive: its keys
// are unioned onto whatever the file supplied, matching the grammar's
// fleet-wide additive promotion.
func overlayEnv(cfg *Config) {
	if v, ok := os.LookupEnv("BILGELINE_DEFAULT_DESTINATION"); ok {
		cfg.DefaultDestination = v
	}
	if v, ok := os.LookupEnv("BILGELINE_DEBOUNCE"); ok {
		cfg.Debounce = v
	}
	if v, ok := os.LookupEnv("BILGELINE_LABELS"); ok {
		cfg.Labels = unionStrings(cfg.Labels, splitList(v))
	}
}

// applyDefaults fills sane defaults for anything unset after the file and env
// passes.
func applyDefaults(cfg *Config) {
	if cfg.SharedConfigPath == "" {
		cfg.SharedConfigPath = DefaultSharedConfigPath
	}
	if cfg.Debounce == "" {
		cfg.Debounce = DefaultDebounce
	}
}

// Validate checks the config for internal consistency, aggregating every
// problem it finds into one error so a bad config surfaces all its faults at
// once rather than one per run.
func (c *Config) Validate() error {
	var errs []error

	// Destination types must be within the v1 enum.
	for name, dest := range c.Destinations {
		if dest.Type == "" {
			errs = append(errs, fmt.Errorf("destination %q: missing type", name))
			continue
		}
		if !contains(DestinationTypes, dest.Type) {
			errs = append(errs, fmt.Errorf("destination %q: invalid type %q, want one of %s",
				name, dest.Type, strings.Join(DestinationTypes, ", ")))
		}
	}

	// default_destination, if set, must not be the none sentinel and must name a
	// defined destination.
	if c.DefaultDestination != "" {
		if c.DefaultDestination == noneSentinel {
			errs = append(errs, fmt.Errorf("default_destination: %q is not a valid default", noneSentinel))
		} else if _, ok := c.Destinations[c.DefaultDestination]; !ok {
			errs = append(errs, fmt.Errorf("default_destination: %q names no defined destination", c.DefaultDestination))
		}
	}

	// Debounce must parse as a duration.
	if _, err := c.DebounceDuration(); err != nil {
		errs = append(errs, err)
	}

	// Profile names must not shadow a reserved parser name.
	for name := range c.Profiles {
		if contains(ReservedParserNames, name) {
			errs = append(errs, fmt.Errorf("profile %q: shadows a reserved parser name (%s)",
				name, strings.Join(ReservedParserNames, ", ")))
		}
	}

	return errors.Join(errs...)
}

// DebounceDuration parses the Debounce window. Validate guarantees it succeeds
// for a loaded Config; it is exported so callers can reach the parsed value
// without re-parsing by hand.
func (c *Config) DebounceDuration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Debounce)
	if err != nil {
		return 0, fmt.Errorf("debounce: invalid duration %q: %w", c.Debounce, err)
	}
	return d, nil
}

// EnvRefs returns the sorted, de-duplicated set of variable names referenced as
// ${env:NAME} anywhere in the destinations (endpoint, path, header values, and
// inline settings, recursively). A later preflight uses it to warn when the
// collector container is missing an env var the generated config will reference.
// bilgeline itself never resolves these references.
func (c *Config) EnvRefs() []string {
	seen := map[string]struct{}{}
	for _, dest := range c.Destinations {
		collectEnvRefs(dest.Endpoint, seen)
		collectEnvRefs(dest.Path, seen)
		for _, v := range dest.Headers {
			collectEnvRefs(v, seen)
		}
		walkEnvRefs(dest.Settings, seen)
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectEnvRefs adds every ${env:NAME} name in s to seen.
func collectEnvRefs(s string, seen map[string]struct{}) {
	for _, m := range envRefPattern.FindAllStringSubmatch(s, -1) {
		seen[m[1]] = struct{}{}
	}
}

// walkEnvRefs recursively scans an arbitrary decoded YAML value (the shape of an
// inline Settings map) for ${env:NAME} references in any string it contains.
func walkEnvRefs(v any, seen map[string]struct{}) {
	switch t := v.(type) {
	case string:
		collectEnvRefs(t, seen)
	case map[string]any:
		for _, e := range t {
			walkEnvRefs(e, seen)
		}
	case map[any]any:
		for _, e := range t {
			walkEnvRefs(e, seen)
		}
	case []any:
		for _, e := range t {
			walkEnvRefs(e, seen)
		}
	}
}

// splitList splits a comma-separated value, trimming whitespace and dropping
// empty elements.
func splitList(v string) []string {
	if v == "" {
		return nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// unionStrings appends the elements of add not already in base, preserving
// order and de-duplicating.
func unionStrings(base, add []string) []string {
	seen := make(map[string]struct{}, len(base))
	out := make([]string, 0, len(base)+len(add))
	for _, s := range base {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range add {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
