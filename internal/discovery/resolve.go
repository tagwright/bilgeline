// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/config"
)

// Resolve is the pure, socket-free heart of discovery: it turns one Candidate
// plus the loaded config into either a routed *backend.ServiceSpec, a silent
// skip, or a set of Diagnostics explaining a skip or a caveat.
//
// Returns:
//   - (nil, nil): the container is not opted in (enable absent or false), the
//     normal silent case, or it is self/collector excluded without an enable
//     label. No noise.
//   - (nil, diags): the container was excluded or skipped. diags carries an
//     error Diagnostic for a validation failure (prefix conflict, bad enum,
//     invalid regex, unknown destination, no destination and no default) or a
//     warning Diagnostic for a structural exclusion (self, collector, or a
//     non-json-file log driver).
//   - (spec, nil) or (spec, warnings): the container is routed. Warnings, if
//     any, are non-fatal notices (e.g. level.min with no parsing to extract a
//     level) that accompany a valid spec.
//
// selfID is bilgeline's own container id. A skip here is always local to this
// container: Discover keeps resolving the rest of the fleet.
//
// Merge precedence, applied throughout, is label > profile > global. Scalar
// fields take the explicit label over the profile value; list-valued fields
// (drop, promoted labels) union across the sources. See the per-field comments.
func Resolve(cand Candidate, cfg *config.Config, selfID string) (*backend.ServiceSpec, []Diagnostic) {
	var diags []Diagnostic
	diag := func(sev Severity, format string, args ...any) {
		diags = append(diags, Diagnostic{
			ContainerID:   cand.ID,
			ContainerName: cand.Name,
			Severity:      sev,
			Message:       fmt.Sprintf(format, args...),
		})
	}

	// Self-exclusion and the collector marker win over everything, including a
	// stray enable=true. Read the markers from the raw labels so a prefix
	// conflict elsewhere on the container cannot mask the exclusion. A pipeline
	// that ships its own error logs to a failing destination is a feedback loop.
	isSelf := selfID != "" && cand.ID == selfID
	isCollector := rawBool(cand.Labels, "collector")
	if isSelf || isCollector {
		if rawBool(cand.Labels, "enable") {
			which := "the bundled collector"
			if isSelf {
				which = "bilgeline's own container"
			}
			diag(SeverityWarning, "excluded from routing (%s) despite enable=true; exclusion wins", which)
		}
		return nil, diags
	}

	norm, err := normalizeLabels(cand.Labels)
	if err != nil {
		diag(SeverityError, "%s", err.Error())
		return nil, diags
	}

	enabled, err := parseBool(norm, "enable", false)
	if err != nil {
		diag(SeverityError, "%s", err.Error())
		return nil, diags
	}
	if !enabled {
		// Opt-in gate: absent or false means ignored. No fleet flip in v1.
		return nil, nil
	}

	// Resolve the optional profile up front; every field below folds its
	// contribution in under the label > profile precedence.
	var profile *config.Profile
	profileName := norm["profile"]
	if profileName != "" {
		p, ok := cfg.Profiles[profileName]
		if !ok {
			diag(SeverityError, "label %q: unknown profile %q", "profile", profileName)
			return nil, diags
		}
		profile = &p
	}

	spec := &backend.ServiceSpec{
		ContainerID:    cand.ID,
		Name:           resolveServiceName(cand, norm),
		Source:         backend.SourceDockerJSONFile,
		ContainerName:  cand.Name,
		ComposeProject: cand.ComposeProject,
		ComposeService: cand.ComposeService,
		Image:          cand.Image,
		Profile:        profileName,
		Stream:         backend.StreamBoth,
	}

	// parse: label enum overrides the profile's parse type (scalar). The
	// profile's timestamp and level mapping still fold in, since they apply to
	// json/logfmt fields just as they do to regex groups.
	parse, pattern, ts, mapping, profileLevelField, perr := resolveParse(norm, profile)
	if perr != nil {
		diag(SeverityError, "%s", perr.Error())
		return nil, diags
	}
	spec.Parse = parse
	spec.ParsePattern = pattern
	spec.Timestamp = ts
	spec.LevelMapping = mapping

	// level.field: explicit label overrides the profile's level field (scalar).
	if v, ok := norm["level.field"]; ok && v != "" {
		spec.LevelField = v
	} else {
		spec.LevelField = profileLevelField
	}

	// multiline: explicit label overrides the profile (scalar). Compile-validate
	// whichever wins.
	multiline := ""
	if profile != nil {
		multiline = profile.Multiline
	}
	if v, ok := norm["multiline"]; ok {
		multiline = v
	}
	if multiline != "" {
		if _, e := regexp.Compile(multiline); e != nil {
			diag(SeverityError, "label %q: invalid regex %q: %v", "multiline", multiline, e)
			return nil, diags
		}
	}
	spec.Multiline = multiline

	// stream: enum, no profile equivalent.
	stream, serr := resolveStream(norm)
	if serr != nil {
		diag(SeverityError, "%s", serr.Error())
		return nil, diags
	}
	spec.Stream = stream

	// level.min: severity-floor enum, no profile equivalent.
	if v, ok := norm["level.min"]; ok && v != "" {
		if !validLevel(v) {
			diag(SeverityError, "label %q: invalid level %q, want one of %s",
				"level.min", v, strings.Join(validLevels, ", "))
			return nil, diags
		}
		spec.LevelMin = v
	}

	// drop: UNION of the per-container csv, the indexed drop.<n> escape hatch,
	// and the profile's drops (list-valued field, not overridden). Each pattern
	// is compile-validated.
	drops, derr := resolveDrops(norm, profile)
	if derr != nil {
		diag(SeverityError, "%s", derr.Error())
		return nil, diags
	}
	spec.Drop = drops

	// static attributes: the profile's attr block, then bilgeline.attr.<key>
	// labels, the label winning on a key collision (label > profile).
	spec.StaticAttrs = resolveAttrs(norm, profile)

	// promoted labels: the fleet-wide default set (config.Labels) unioned with
	// the container's own bilgeline.labels, with the sentinel "none" suppressing
	// the global set for this one container.
	spec.PromotedLabels = resolvePromotedLabels(norm, cfg)

	// raw operators: verbatim passthrough from the profile's operators block.
	if profile != nil && len(profile.Operators) > 0 {
		spec.RawOperators = profile.Operators
	}

	// A severity floor with nothing parsing a level out of the record cannot do
	// anything: warn, but still route the container.
	if spec.LevelMin != "" && spec.Parse == backend.ParseNone {
		diag(SeverityWarning, "level.min=%q set but parse=none extracts no level; the floor will not apply", spec.LevelMin)
	}

	// Log-driver handling. v1 tails only Docker's json-file driver via the
	// collector's filelog receiver. Docker's "local" driver stores a compressed,
	// length-prefixed protobuf log stream that is daemon-private and cannot be
	// tailed as plain text, so a non-json-file driver is excluded (with a
	// warning) rather than silently generating a dead receiver that mis-routes.
	// An empty LogDriver means the runtime does not yet expose it; v1 treats
	// that as json-file compatible so a stock Docker host routes normally, and
	// the exclusion activates automatically once core surfaces the driver.
	if cand.LogDriver != "" && cand.LogDriver != jsonFileDriver {
		diag(SeverityWarning, "log driver %q is not %s; excluded from routing (v1 tails %s only)",
			cand.LogDriver, jsonFileDriver, jsonFileDriver)
		return nil, diags
	}

	// destinations: the resolved routes, or a skip if the container names an
	// unknown destination or names none with no default configured anywhere.
	routes, rerr := resolveRoutes(norm, cfg)
	if rerr != nil {
		diag(SeverityError, "%s", rerr.Error())
		return nil, diags
	}
	spec.Routes = routes

	return spec, diags
}

// resolveServiceName applies the service-identity precedence: bilgeline.name,
// then the compose service label, then the container name with any leading "/"
// stripped. Duplicates across containers are legal (replicas) and are not an
// error here.
func resolveServiceName(cand Candidate, norm map[string]string) string {
	if v := norm["name"]; v != "" {
		return v
	}
	if cand.ComposeService != "" {
		return cand.ComposeService
	}
	return strings.TrimPrefix(cand.Name, "/")
}

// resolveParse resolves the parse mode and the profile's structured-parse
// contributions under the label > profile precedence. The label carries a
// closed enum (json, logfmt, none); a regex parse only ever reaches a
// ServiceSpec through a profile. When the label overrides the profile's parse
// type, the profile's regex pattern is dropped (it is meaningless for a
// non-regex parse) but its timestamp and level mapping still fold in.
func resolveParse(norm map[string]string, profile *config.Profile) (
	mode backend.ParseMode,
	pattern string,
	ts *backend.TimestampSpec,
	mapping map[string]string,
	levelField string,
	err error,
) {
	profileMode := backend.ParseNone
	if profile != nil && profile.Parse != nil {
		pp := profile.Parse
		switch pp.Type {
		case "regex":
			profileMode, pattern = backend.ParseRegex, pp.Pattern
		case "json":
			profileMode = backend.ParseJSON
		case "logfmt":
			profileMode = backend.ParseLogfmt
		case "", "none":
			profileMode = backend.ParseNone
		default:
			return "", "", nil, nil, "", fmt.Errorf("discovery: profile parse type %q is invalid, want regex|json|logfmt|none", pp.Type)
		}
		if pp.Timestamp != nil {
			ts = &backend.TimestampSpec{Field: pp.Timestamp.Field, Layout: pp.Timestamp.Layout}
		}
		if pp.Level != nil {
			levelField = pp.Level.Field
			if len(pp.Level.Mapping) > 0 {
				mapping = pp.Level.Mapping
			}
		}
	}

	if v, ok := norm["parse"]; ok {
		switch v {
		case "json":
			return backend.ParseJSON, "", ts, mapping, levelField, nil
		case "logfmt":
			return backend.ParseLogfmt, "", ts, mapping, levelField, nil
		case "none":
			return backend.ParseNone, "", ts, mapping, levelField, nil
		default:
			return "", "", nil, nil, "", fmt.Errorf("discovery: label %q: invalid value %q, want json|logfmt|none", "parse", v)
		}
	}

	return profileMode, pattern, ts, mapping, levelField, nil
}

// resolveStream reads the stream selector enum, defaulting to both.
func resolveStream(norm map[string]string) (backend.Stream, error) {
	v, ok := norm["stream"]
	if !ok || v == "" {
		return backend.StreamBoth, nil
	}
	switch v {
	case "both":
		return backend.StreamBoth, nil
	case "stdout":
		return backend.StreamStdout, nil
	case "stderr":
		return backend.StreamStderr, nil
	default:
		return "", fmt.Errorf("discovery: label %q: invalid value %q, want stdout|stderr|both", "stream", v)
	}
}

// resolveDrops unions the per-container csv drop, the indexed drop.<n> escape
// hatch, and the profile's drops, de-duplicating in a stable order and
// compile-validating every pattern.
func resolveDrops(norm map[string]string, profile *config.Profile) ([]string, error) {
	var all []string
	all = append(all, splitCSV(norm["drop"])...)
	all = append(all, indexedValues(norm, "drop")...)
	if profile != nil {
		all = append(all, profile.Drop...)
	}

	seen := make(map[string]struct{}, len(all))
	out := make([]string, 0, len(all))
	for _, d := range all {
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		if _, e := regexp.Compile(d); e != nil {
			return nil, fmt.Errorf("discovery: label %q: invalid regex %q: %v", "drop", d, e)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// resolveAttrs folds the profile's static attributes in first, then overlays
// the container's bilgeline.attr.<key> labels, the label winning on a key
// collision. Keys are used verbatim (no forced prefix).
func resolveAttrs(norm map[string]string, profile *config.Profile) map[string]string {
	out := map[string]string{}
	if profile != nil {
		for k, v := range profile.Attr {
			out[k] = v
		}
	}
	for k, v := range norm {
		if key, ok := strings.CutPrefix(k, "attr."); ok && key != "" {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolvePromotedLabels unions the fleet-wide default promotion set
// (config.Labels) with the container's own bilgeline.labels csv. The sentinel
// "none" anywhere in the container's list suppresses the global set for this
// container; any other explicit keys still promote.
func resolvePromotedLabels(norm map[string]string, cfg *config.Config) []string {
	explicit := splitCSV(norm["labels"])
	suppress := false
	keys := make([]string, 0, len(explicit))
	for _, k := range explicit {
		if k == noneSentinel {
			suppress = true
			continue
		}
		keys = append(keys, k)
	}

	var base []string
	if !suppress {
		base = cfg.Labels
	}
	return unionStrings(base, keys)
}

// resolveRoutes resolves the destination csv into routes. With no destination
// named it falls back to cfg.DefaultDestination; with neither it is a
// validation error. The sentinel "none" anywhere means route nowhere (a valid,
// enabled-but-dropped state, nil routes). Every named destination must exist in
// cfg.Destinations, except the always-valid reserved "debug".
func resolveRoutes(norm map[string]string, cfg *config.Config) ([]backend.Route, error) {
	names := splitCSV(norm["destination"])
	if len(names) == 0 {
		if cfg.DefaultDestination == "" {
			return nil, fmt.Errorf("discovery: label %q: no destination named and no default_destination configured", "destination")
		}
		names = []string{cfg.DefaultDestination}
	}

	for _, n := range names {
		if n == noneSentinel {
			// Enabled but routed nowhere: valid, distinct from disabled.
			return nil, nil
		}
	}

	seen := make(map[string]struct{}, len(names))
	routes := make([]backend.Route, 0, len(names))
	for _, n := range names {
		if !destinationValid(n, cfg) {
			return nil, fmt.Errorf("discovery: label %q: unknown destination %q", "destination", n)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		routes = append(routes, backend.Route{Destination: n})
	}
	return routes, nil
}

// destinationValid reports whether name is a routable destination: the reserved
// debug exporter, or a destination defined in the config.
func destinationValid(name string, cfg *config.Config) bool {
	if name == debugDestination {
		return true
	}
	_, ok := cfg.Destinations[name]
	return ok
}
