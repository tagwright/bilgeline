// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package otelcol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tagwright/bilgeline/internal/backend"
	"gopkg.in/yaml.v3"
)

// Render turns a Spec into deterministic otelcol-contrib YAML. It is pure: no
// I/O, no process interaction. The same Spec always renders byte-identical
// output, so the daemon's on-disk diff is meaningful. Determinism comes from
// sorting every collection derived from the Spec (services by container id,
// destination names, receiver/exporter/pipeline component names) and from
// yaml.v3 emitting map keys in sorted order.
//
// Only docker-json-file services are emitted in v1; a service with any other
// source is skipped (it cannot be tailed by the filelog receiver). When nothing
// is left to route (no services, or every service is routed nowhere) an inert but
// valid config is produced, since an otelcol config must carry at least one
// non-empty pipeline.
func (b *Backend) Render(spec backend.Spec) (backend.RenderedConfig, error) {
	root, err := b.buildConfig(spec)
	if err != nil {
		return backend.RenderedConfig{}, err
	}

	data, err := yaml.Marshal(root)
	if err != nil {
		return backend.RenderedConfig{}, fmt.Errorf("otelcol: marshal config: %w", err)
	}

	return backend.RenderedConfig{Format: "yaml", Data: data}, nil
}

// destSet is a service's canonical, sorted destination-set together with the
// pre-derived key and hash it is grouped and routed by.
type destSet struct {
	names []string // sorted destination names, non-empty
	key   string   // canonical join of names, the routing-key attribute value
	hash  string   // short hash of key, used in the pipeline name
}

// buildConfig assembles the whole config as an ordered nesting of maps and slices
// that yaml.v3 renders deterministically. It is split out from Render so tests
// can assert on structure without re-parsing YAML.
func (b *Backend) buildConfig(spec backend.Spec) (map[string]any, error) {
	// Keep only tailable services, in a stable container-id order. Discovery
	// already sorts, but the renderer does not depend on that.
	services := make([]backend.ServiceSpec, 0, len(spec.Services))
	for _, svc := range spec.Services {
		if svc.Source == backend.SourceDockerJSONFile {
			services = append(services, svc)
		}
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].ContainerID < services[j].ContainerID
	})

	// Resolve each service's destination set once. A nil set means routed
	// nowhere: the service is still ingested and enriched, then dropped at the
	// routing connector because it carries no routing key.
	svcSets := make([]*destSet, len(services))
	distinct := map[string]*destSet{}
	for i, svc := range services {
		names := svc.DestinationNames()
		if len(names) == 0 {
			continue
		}
		key := strings.Join(names, ",")
		ds := &destSet{names: names, key: key, hash: shortHash(key)}
		svcSets[i] = ds
		distinct[key] = ds
	}

	// Nothing to route anywhere: emit an inert, valid config. This covers both an
	// empty Spec and the pathological case where every service is routed nowhere.
	if len(distinct) == 0 {
		return b.inertConfig(), nil
	}

	// Gather the destinations actually referenced, so no unused exporter is
	// emitted. Every referenced name must resolve to a Destination (the reserved
	// "debug" sink is synthesized when the daemon did not define it).
	referenced := map[string]struct{}{}
	for _, ds := range distinct {
		for _, name := range ds.names {
			referenced[name] = struct{}{}
		}
	}
	exporters, err := buildExporters(spec.Destinations, referenced)
	if err != nil {
		return nil, err
	}

	receivers, receiverNames := b.buildReceivers(services)
	processors, ingestProcessors := buildProcessors(services, svcSets)
	connectors, outPipelines := buildRouting(distinct, spec.Destinations, referenced)

	extensions := map[string]any{
		"file_storage": map[string]any{
			"directory":        b.fileStorageDir,
			"create_directory": true,
		},
		"health_check": map[string]any{
			"endpoint": b.healthCheckEndpoint,
		},
	}

	pipelines := map[string]any{
		"logs/ingest": map[string]any{
			"receivers":  receiverNames,
			"processors": ingestProcessors,
			"exporters":  []string{"routing"},
		},
	}
	for name, pipe := range outPipelines {
		pipelines[name] = pipe
	}

	root := map[string]any{
		"receivers":  receivers,
		"processors": processors,
		"connectors": connectors,
		"exporters":  exporters,
		"extensions": extensions,
		"service": map[string]any{
			"extensions": []string{"file_storage", "health_check"},
			"pipelines":  pipelines,
		},
	}
	return root, nil
}

// inertConfig is a minimal, valid collector config used when there is nothing to
// route. It tails nothing and exports nothing but keeps health_check up so the
// apply chunk can still probe the collector.
func (b *Backend) inertConfig() map[string]any {
	return map[string]any{
		"receivers": map[string]any{"nop": map[string]any{}},
		"exporters": map[string]any{"nop": map[string]any{}},
		"extensions": map[string]any{
			"health_check": map[string]any{"endpoint": b.healthCheckEndpoint},
		},
		"service": map[string]any{
			"extensions": []string{"health_check"},
			"pipelines": map[string]any{
				"logs/inert": map[string]any{
					"receivers": []string{"nop"},
					"exporters": []string{"nop"},
				},
			},
		},
	}
}

// signature captures every ServiceSpec field that shapes a filelog receiver's
// operator chain. Two services with an equal signature share one receiver; the
// signature's hash names it. level.min, stream, and drop are deliberately absent:
// they are realized in the shared filter processor per container, not in the
// receiver, so they never fragment receiver groups.
type signature struct {
	Parse        backend.ParseMode      `json:"parse"`
	Pattern      string                 `json:"pattern"`
	Multiline    string                 `json:"multiline"`
	LevelField   string                 `json:"level_field"`
	LevelMapping map[string]string      `json:"level_mapping"`
	Timestamp    *backend.TimestampSpec `json:"timestamp"`
	RawOperators []map[string]any       `json:"raw_operators"`
}

// signatureOf derives a service's grouping signature.
func signatureOf(svc backend.ServiceSpec) signature {
	return signature{
		Parse:        svc.Parse,
		Pattern:      svc.ParsePattern,
		Multiline:    svc.Multiline,
		LevelField:   svc.LevelField,
		LevelMapping: svc.LevelMapping,
		Timestamp:    svc.Timestamp,
		RawOperators: svc.RawOperators,
	}
}

// receiverGroup is the members of one processing signature, ordered by container
// id, with the representative service used to build the operator chain.
type receiverGroup struct {
	hash    string
	rep     backend.ServiceSpec
	members []backend.ServiceSpec
}

// buildReceivers groups the services by signature into one filelog receiver each
// and returns the receivers map plus the sorted receiver names for the ingest
// pipeline.
func (b *Backend) buildReceivers(services []backend.ServiceSpec) (map[string]any, []string) {
	groups := map[string]*receiverGroup{}
	for _, svc := range services {
		h := shortHash(canonicalJSON(signatureOf(svc)))
		g, ok := groups[h]
		if !ok {
			g = &receiverGroup{hash: h, rep: svc}
			groups[h] = g
		}
		g.members = append(g.members, svc)
	}

	receivers := map[string]any{}
	names := make([]string, 0, len(groups))
	for h, g := range groups {
		name := "filelog/" + h
		names = append(names, name)

		// Explicit per-container include paths, sorted for determinism.
		members := append([]backend.ServiceSpec(nil), g.members...)
		sort.Slice(members, func(i, j int) bool {
			return members[i].ContainerID < members[j].ContainerID
		})
		includes := make([]string, 0, len(members))
		for _, m := range members {
			includes = append(includes, dockerLogPath(m.ContainerID))
		}

		receivers[name] = map[string]any{
			"include":           includes,
			"include_file_path": true,
			"storage":           "file_storage",
			"operators":         buildOperators(g.rep),
		}
	}
	sort.Strings(names)
	return receivers, names
}

// buildOperators builds the stanza operator chain for one signature's
// representative service. Order: recover container identity, parse the body,
// extract a timestamp, recombine multiline entries, parse severity, then splice
// any profile raw operators in verbatim at the end.
func buildOperators(svc backend.ServiceSpec) []any {
	ops := []any{
		// The container operator parses Docker's json-file envelope
		// (log/stream/time) into body, log.iostream, and the record timestamp.
		// add_metadata_from_filepath is K8s-only, so it is off for the Docker path.
		map[string]any{
			"type":                       "container",
			"format":                     "docker",
			"add_metadata_from_filepath": false,
		},
		// Recover the 64-hex container id from the file path into an attribute the
		// downstream processors key on.
		map[string]any{
			"type":       "regex_parser",
			"parse_from": `attributes["log.file.path"]`,
			"regex":      `^/var/lib/docker/containers/(?P<container_id>[a-f0-9]{64})/`,
		},
	}

	// Body parse, if any.
	switch svc.Parse {
	case backend.ParseJSON:
		ops = append(ops, map[string]any{
			"type":       "json_parser",
			"parse_from": "body",
			"parse_to":   "attributes",
		})
	case backend.ParseLogfmt:
		// Stanza has no logfmt operator; logfmt is key=value pairs separated by
		// spaces, which key_value_parser handles.
		ops = append(ops, map[string]any{
			"type":           "key_value_parser",
			"parse_from":     "body",
			"delimiter":      "=",
			"pair_delimiter": " ",
		})
	case backend.ParseRegex:
		if svc.ParsePattern != "" {
			ops = append(ops, map[string]any{
				"type":       "regex_parser",
				"parse_from": "body",
				"parse_to":   "attributes",
				"regex":      svc.ParsePattern,
			})
		}
	}

	// Timestamp extraction from a parsed field (profile-supplied).
	if svc.Timestamp != nil && svc.Timestamp.Field != "" {
		ops = append(ops, map[string]any{
			"type":        "time_parser",
			"parse_from":  "attributes." + svc.Timestamp.Field,
			"layout_type": "strptime",
			"layout":      svc.Timestamp.Layout,
		})
	}

	// Multiline recombination: a new entry begins when the regex matches.
	if svc.Multiline != "" {
		ops = append(ops, map[string]any{
			"type":           "recombine",
			"combine_field":  "body",
			"is_first_entry": fmt.Sprintf("body matches %q", svc.Multiline),
		})
	}

	// Severity parsing, only meaningful when the body was parsed into fields. An
	// explicit level field parses that field; otherwise probe the conventional
	// names, each guard skipping the parser when its field is absent.
	if svc.Parse != backend.ParseNone {
		ops = append(ops, severityOperators(svc)...)
	}

	// Raw operators from a profile, spliced verbatim at the end.
	for _, raw := range svc.RawOperators {
		ops = append(ops, raw)
	}

	return ops
}

// severityOperators builds the severity_parser operators for a service.
func severityOperators(svc backend.ServiceSpec) []any {
	if svc.LevelField != "" {
		op := map[string]any{
			"type":       "severity_parser",
			"parse_from": "attributes." + svc.LevelField,
		}
		if len(svc.LevelMapping) > 0 {
			op["mapping"] = svc.LevelMapping
		}
		return []any{op}
	}

	// No explicit field: probe the conventional level field names in order,
	// guarding each so a record missing the field is not treated as an error.
	var ops []any
	for _, field := range []string{"level", "severity", "lvl"} {
		op := map[string]any{
			"type":       "severity_parser",
			"parse_from": "attributes." + field,
			"if":         fmt.Sprintf(`attributes[%q] != nil`, field),
		}
		if len(svc.LevelMapping) > 0 {
			op["mapping"] = svc.LevelMapping
		}
		ops = append(ops, op)
	}
	return ops
}

// buildProcessors builds the processors map and the ordered list of processor
// names the ingest pipeline runs: memory_limiter first (backpressure), then the
// identity/attribute transform, then the per-service filter if any service needs
// one. batch is defined here too for the per-destination out pipelines.
func buildProcessors(services []backend.ServiceSpec, svcSets []*destSet) (map[string]any, []string) {
	processors := map[string]any{
		"memory_limiter": map[string]any{
			"check_interval":         "1s",
			"limit_percentage":       80,
			"spike_limit_percentage": 25,
		},
		"batch": map[string]any{},
	}

	processors["transform/enrich"] = map[string]any{
		"error_mode":     "ignore",
		"log_statements": enrichStatements(services, svcSets),
	}

	ingest := []string{"memory_limiter", "transform/enrich"}

	if conds := filterConditions(services); len(conds) > 0 {
		processors["filter/drop"] = map[string]any{
			"error_mode": "ignore",
			"logs": map[string]any{
				"log_record": conds,
			},
		}
		ingest = append(ingest, "filter/drop")
	}

	return processors, ingest
}

// enrichStatements builds one transform statement group per service, keyed on the
// recovered container id, stamping identity, static attributes, and the routing
// key. Every group is guarded by the same container-id condition so the whole
// enrichment stays inside one processor at O(services) groups.
//
// PromotedLabels are intentionally NOT stamped here: the ServiceSpec carries the
// label KEYS to promote but not their VALUES (the collector cannot read a
// container's labels from a tailed file), so promotion has to be resolved into
// StaticAttrs upstream by the daemon. This is a contract gap flagged for that
// chunk, not something the renderer can complete on its own.
func enrichStatements(services []backend.ServiceSpec, svcSets []*destSet) []any {
	groups := make([]any, 0, len(services))
	for i, svc := range services {
		var stmts []string
		set := func(key, val string) {
			stmts = append(stmts, fmt.Sprintf("set(resource.attributes[%s], %s)", ottlString(key), ottlString(val)))
		}

		set("service.name", svc.Name)
		if svc.ComposeProject != "" {
			set("service.namespace", svc.ComposeProject)
		}
		set("container.id", svc.ContainerID)
		if svc.ContainerName != "" {
			set("container.name", svc.ContainerName)
		}
		if svc.Image != "" {
			set("container.image.name", svc.Image)
		}

		for _, k := range sortedKeys(svc.StaticAttrs) {
			set(k, svc.StaticAttrs[k])
		}

		if ds := svcSets[i]; ds != nil {
			set(routingKeyAttr, ds.key)
		}

		groups = append(groups, map[string]any{
			"context":    "log",
			"conditions": []string{containerIDCond(svc.ContainerID)},
			"statements": stmts,
		})
	}
	return groups
}

// filterConditions builds the OTTL drop conditions for the shared filter
// processor. Each condition is guarded by container id so it applies only to its
// service: a stream restriction, a severity floor, and each drop regex. The
// filter drops a record matching ANY condition.
func filterConditions(services []backend.ServiceSpec) []string {
	var conds []string
	for _, svc := range services {
		id := containerIDCond(svc.ContainerID)

		// Stream: drop the stream that is not wanted.
		switch svc.Stream {
		case backend.StreamStdout:
			conds = append(conds, id+` and attributes["log.iostream"] == "stderr"`)
		case backend.StreamStderr:
			conds = append(conds, id+` and attributes["log.iostream"] == "stdout"`)
		}

		// Level floor: drop records whose parsed severity is below the floor. The
		// severity_number != 0 guard keeps records whose severity was never parsed.
		if n, ok := severityFloor(svc.LevelMin); ok {
			conds = append(conds, fmt.Sprintf("%s and severity_number != 0 and severity_number < %d", id, n))
		}

		// Drops: drop records whose body matches any drop regex.
		for _, pat := range svc.Drop {
			conds = append(conds, fmt.Sprintf("%s and IsMatch(body, %s)", id, ottlString(pat)))
		}
	}
	return conds
}

// buildRouting builds the routing connector and the per-destination-set out
// pipelines. The connector has one table entry per distinct destination set,
// matching the routing-key resource attribute and routing to that set's pipeline.
// No default_pipelines is set: a record with no matching key (a service routed
// nowhere) is dropped, which is the intended handling of the "none" case.
func buildRouting(distinct map[string]*destSet, dests map[string]backend.Destination, referenced map[string]struct{}) (map[string]any, map[string]any) {
	// Stable order: sort sets by their canonical key.
	sets := make([]*destSet, 0, len(distinct))
	for _, ds := range distinct {
		sets = append(sets, ds)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].key < sets[j].key })

	var table []any
	outPipelines := map[string]any{}
	for _, ds := range sets {
		pipeName := "logs/out_" + ds.hash
		table = append(table, map[string]any{
			"context":   "resource",
			"condition": fmt.Sprintf("attributes[%s] == %s", ottlString(routingKeyAttr), ottlString(ds.key)),
			"pipelines": []string{pipeName},
		})

		exps := make([]string, 0, len(ds.names))
		for _, name := range ds.names {
			exps = append(exps, exporterName(name, destType(name, dests)))
		}
		sort.Strings(exps)
		outPipelines[pipeName] = map[string]any{
			"receivers":  []string{"routing"},
			"processors": []string{"batch"},
			"exporters":  exps,
		}
	}

	connectors := map[string]any{
		"routing": map[string]any{
			"error_mode": "ignore",
			"table":      table,
		},
	}
	return connectors, outPipelines
}

// buildExporters builds the exporters map for every referenced destination. The
// exporter config is a verbatim copy of the destination's Settings, so any
// ${env:VAR} strings pass through untouched and bilgeline never resolves a
// secret. Type selects only which exporter the settings drive.
func buildExporters(dests map[string]backend.Destination, referenced map[string]struct{}) (map[string]any, error) {
	exporters := map[string]any{}
	for name := range referenced {
		dest, ok := dests[name]
		if !ok {
			// The reserved debug sink is valid without a definition.
			if name == "debug" {
				dest = backend.Destination{Name: "debug", Type: "debug", Settings: map[string]any{"verbosity": "detailed"}}
			} else {
				return nil, fmt.Errorf("otelcol: destination %q referenced by a route but not defined", name)
			}
		}
		exp, err := exporterConfig(dest)
		if err != nil {
			return nil, err
		}
		exporters[exporterName(name, dest.Type)] = exp
	}
	return exporters, nil
}

// exporterConfig maps a Destination to its exporter config block: a verbatim copy
// of Settings. Settings values (endpoint, path, headers, and any passthrough
// keys, including ${env:VAR} references) are copied as-is.
func exporterConfig(dest backend.Destination) (map[string]any, error) {
	if !knownExporterType(dest.Type) {
		return nil, fmt.Errorf("otelcol: destination %q has unsupported type %q", dest.Name, dest.Type)
	}
	out := make(map[string]any, len(dest.Settings))
	for k, v := range dest.Settings {
		out[k] = v
	}
	return out, nil
}

// exporterName is the collector component id for a destination: the exporter type
// for its kind, slashed with the destination name (unique, since names are unique
// keys), e.g. a loki destination "central" becomes otlphttp/central.
func exporterName(destName, destType string) string {
	return exporterTypeFor(destType) + "/" + destName
}

// exporterTypeFor maps a bilgeline destination type to the otelcol exporter type
// that serves it. loki speaks OTLP over HTTP, so it uses the otlphttp exporter.
func exporterTypeFor(t string) string {
	switch t {
	case "loki", "otlphttp":
		return "otlphttp"
	case "otlp":
		return "otlp"
	case "elasticsearch":
		return "elasticsearch"
	case "file":
		return "file"
	case "debug":
		return "debug"
	default:
		return t
	}
}

// knownExporterType reports whether the type maps to a supported exporter.
func knownExporterType(t string) bool {
	switch t {
	case "loki", "otlphttp", "otlp", "elasticsearch", "file", "debug":
		return true
	default:
		return false
	}
}

// destType returns a destination's declared type, defaulting the reserved debug
// sink when the daemon did not define it.
func destType(name string, dests map[string]backend.Destination) string {
	if d, ok := dests[name]; ok {
		return d.Type
	}
	if name == "debug" {
		return "debug"
	}
	return ""
}

// dockerLogPath is the explicit json-file path the filelog receiver tails for a
// container id.
func dockerLogPath(id string) string {
	return fmt.Sprintf("/var/lib/docker/containers/%s/%s-json.log", id, id)
}

// containerIDCond is the OTTL fragment matching a record's recovered container id.
func containerIDCond(id string) string {
	return fmt.Sprintf("attributes[%q] == %s", "container_id", ottlString(id))
}

// severityFloor maps a level.min name to its OTLP severity number. The returned
// bool is false when there is no floor.
func severityFloor(min string) (int, bool) {
	switch min {
	case "trace":
		return 1, true
	case "debug":
		return 5, true
	case "info":
		return 9, true
	case "warn":
		return 13, true
	case "error":
		return 17, true
	case "fatal":
		return 21, true
	default:
		return 0, false
	}
}

// ottlString renders a Go string as an OTTL double-quoted string literal,
// escaping backslashes and quotes so the embedded value cannot break out of the
// literal. yaml.v3 handles the outer YAML-level escaping on top of this.
func ottlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// shortHash is the first 12 hex chars of the SHA-256 of s: enough to name
// receiver and pipeline groups collision-free in practice, stable for a given
// input.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// canonicalJSON is a stable JSON encoding (sorted map keys) used to hash a
// signature. A marshal error is not reachable for the JSON-encodable signature
// type; the sentinel keeps the function total.
func canonicalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "uncanonicalizable"
	}
	return string(data)
}

// sortedKeys returns a map's keys in sorted order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
