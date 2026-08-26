// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// validLevels is the severity floor enum for bilgeline.level.min, lowest to
// highest. A record whose level is below the floor is dropped downstream.
var validLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// parseBool reads a "true"/"false" label, defaulting to def when the suffix is
// absent or empty.
func parseBool(m map[string]string, key string, def bool) (bool, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("discovery: label %q: invalid bool %q: %w", key, v, err)
	}
	return b, nil
}

// splitCSV splits a comma-separated label value, trimming whitespace and
// dropping empty elements. An absent or empty value yields nil.
func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// indexedValues collects the "<prefix>.<n>" escape-hatch labels (e.g. drop.0,
// drop.1, ...) in ascending index order. Non-numeric suffixes after prefix+"."
// are ignored, since they belong to a different label. This is the documented
// norm for comma-bearing values like drop regexes.
func indexedValues(m map[string]string, prefix string) []string {
	type kv struct {
		idx int
		val string
	}
	var items []kv

	want := prefix + "."
	for k, v := range m {
		rest, ok := strings.CutPrefix(k, want)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		items = append(items, kv{n, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })

	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.val)
	}
	return out
}

// unionStrings appends the elements of add not already in base, preserving
// order and de-duplicating. Returns nil, not an empty slice, when both are
// empty, matching every optional spec field's zero value.
func unionStrings(base, add []string) []string {
	if len(base) == 0 && len(add) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(base)+len(add))
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
	if len(out) == 0 {
		return nil
	}
	return out
}

// validLevel reports whether v is one of the severity-floor enum values.
func validLevel(v string) bool {
	for _, l := range validLevels {
		if l == v {
			return true
		}
	}
	return false
}
