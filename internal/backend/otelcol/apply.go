// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package otelcol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/core/runtime"
)

// Recognized marker prefixes, mirroring the discovery package. The collector is
// marked by "collector=true" under either the primary or the alias prefix; a
// value is true under either prefix means marked. Kept local so the otelcol
// backend stays independent of the discovery package's internals.
const (
	markerPrefixPrimary = "bilgeline."
	markerPrefixAlias   = "tagwright.log."
	collectorMarker     = "collector"
)

// envRefPattern matches a ${env:NAME} reference and captures NAME, the same
// shape config.EnvRefs recognizes. The rendered YAML on disk is the single
// source of truth for what the collector will actually reference, so the env
// preflight scans cfg.Data directly rather than re-deriving it from the config.
var envRefPattern = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Apply writes the rendered config to the shared path atomically, then, if a
// single unambiguous collector that mounts that path is running, SIGHUPs it to
// reload and recovers with one restart if the reload wedges the process.
//
// The write ALWAYS happens, even with no collector present, so the config is in
// place for when the collector starts. Selection or safety problems (ambiguity,
// a marker-vs-config disagreement, a collector that does not mount the shared
// path) never fail the whole Apply: the config is still written, no signal is
// sent, and the reason is returned in the result Detail for the daemon to alert
// on. The returned error is reserved for a genuine write failure and for the
// case where the collector dies again after its one recovery restart.
func (b *Backend) Apply(ctx context.Context, cfg backend.RenderedConfig) (backend.ApplyResult, error) {
	path := b.sharedConfigPath
	if path == "" {
		path = "/config/otelcol.yaml"
	}

	// 1. Always write the config, atomically.
	if err := writeAtomic(path, cfg.Data); err != nil {
		return backend.ApplyResult{}, fmt.Errorf("otelcol: write config: %w", err)
	}

	if b.rt == nil {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: fmt.Sprintf("config written to %s, no runtime configured so no collector signalled", path),
		}, nil
	}

	// 2. Find the collector. Identity errors (ambiguity, disagreement) are
	// surfaced but do not fail Apply and never signal.
	collectorID, note, err := b.findCollector(ctx)
	if err != nil {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: fmt.Sprintf("config written to %s, not signalled: %s", path, err.Error()),
		}, nil
	}
	if collectorID == "" {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: fmt.Sprintf("config written to %s: %s", path, note),
		}, nil
	}

	// Inspect for the authoritative mounts, env, and state. The list summary can
	// carry neither the full mounts nor the env, so the guard and preflight use
	// Inspect.
	col, err := b.rt.Inspect(ctx, collectorID)
	if err != nil {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: fmt.Sprintf("config written to %s, not signalled: inspect collector: %v", path, err),
		}, nil
	}

	// 3. Mount guard: refuse to signal a container that does not mount the shared
	// config path. This closes the class where a mislabeled marker makes
	// bilgeline SIGHUP an innocent container.
	if !mountsSharedPath(col.Mounts, path) {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: fmt.Sprintf("config written to %s, refusing to signal: collector %q does not mount the shared config path", path, col.Name),
		}, nil
	}

	// 4. Env preflight: warn on any ${env:VAR} the config references that the
	// collector does not carry. Names only, values are never read or logged.
	warnings := envPreflight(cfg.Data, col.Env)

	// 5. Reload: SIGHUP the collector to reload in place.
	if err := b.rt.Kill(ctx, collectorID, "SIGHUP"); err != nil {
		return backend.ApplyResult{
			Action: backend.ActionWrittenOnly,
			Detail: joinDetail(fmt.Sprintf("config written to %s, SIGHUP failed: %v", path, err), warnings),
		}, nil
	}

	// 6. Wedge recovery.
	return b.recoverAfterReload(ctx, collectorID, col.Name, path, warnings)
}

// recoverAfterReload polls the collector's container state for a bounded window
// after the SIGHUP. If the collector stays running, the reload took. If it dies
// (exited/dead) within the window, it is restarted ONCE: recovering to running
// yields ActionRestarted, dying again yields an error result and no second
// restart. Uncertain states (paused, restarting that never resolves, an inspect
// error) never trigger a restart.
func (b *Backend) recoverAfterReload(ctx context.Context, id, name, path string, warnings []string) (backend.ApplyResult, error) {
	wait := b.reloadWait
	if wait <= 0 {
		wait = DefaultReloadWait
	}

	outcome := b.watchState(ctx, id, wait)
	switch outcome {
	case stateStayedRunning:
		detail := fmt.Sprintf("config written to %s, collector %q reloaded via SIGHUP", path, name)
		if b.collectorHealthURL != "" {
			if err := httpProbe(ctx, b.collectorHealthURL); err != nil {
				// Secondary, network-dependent signal only: note it, never
				// downgrade the container-state verdict or restart on it.
				detail += fmt.Sprintf(" (health probe unreachable: %v)", err)
			}
		}
		return backend.ApplyResult{Action: backend.ActionReloaded, Detail: joinDetail(detail, warnings)}, nil

	case stateDied:
		// Reload wedged the process: restart once.
		if err := b.rt.Restart(ctx, id); err != nil {
			return backend.ApplyResult{
					Action: backend.ActionRestarted,
					Detail: joinDetail(fmt.Sprintf("config written to %s, reload wedged collector %q and the restart call failed", path, name), warnings),
				},
				fmt.Errorf("otelcol: collector %q wedged on reload and restart failed: %w", name, err)
		}
		after := b.watchState(ctx, id, wait)
		if after == stateStayedRunning {
			return backend.ApplyResult{
				Action: backend.ActionRestarted,
				Detail: joinDetail(fmt.Sprintf("config written to %s, reload wedged collector %q, restarted and it recovered", path, name), warnings),
			}, nil
		}
		// Died again (or never came back). One attempt only: alert, do not
		// restart again.
		return backend.ApplyResult{
				Action: backend.ActionRestarted,
				Detail: joinDetail(fmt.Sprintf("config written to %s, collector %q died again after its one recovery restart, giving up", path, name), warnings),
			},
			fmt.Errorf("otelcol: collector %q did not recover after restart", name)

	default: // stateUncertain
		// Never restart on uncertainty. Report a reload with a caveat.
		return backend.ApplyResult{
			Action: backend.ActionReloaded,
			Detail: joinDetail(fmt.Sprintf("config written to %s, SIGHUP sent to collector %q, state could not be confirmed within %s", path, name, wait), warnings),
		}, nil
	}
}

// stateOutcome is the verdict of watchState.
type stateOutcome int

const (
	// stateStayedRunning means the collector was running throughout the window.
	stateStayedRunning stateOutcome = iota
	// stateDied means the collector entered a terminal-failure state
	// (exited/dead) within the window: the reliable, network-free wedge signal.
	stateDied
	// stateUncertain means the state could not be determined (inspect errors, or
	// a non-terminal transient like paused/restarting that never resolved).
	stateUncertain
)

// watchState polls the collector's container State until the window elapses,
// returning stateDied the moment it observes a terminal-failure state, or
// stateStayedRunning if it was running on the final observation and never died.
// It treats repeated inspect failures or a lingering non-running, non-terminal
// state as stateUncertain so the caller never restarts on ambiguity.
func (b *Backend) watchState(ctx context.Context, id string, window time.Duration) stateOutcome {
	deadline := time.Now().Add(window)
	interval := reloadPollInterval
	if interval > window {
		interval = window
	}
	var lastRunning bool
	var sawAny bool

	for {
		col, err := b.rt.Inspect(ctx, id)
		if err == nil {
			sawAny = true
			switch normalizeState(col.State) {
			case "running":
				lastRunning = true
			case "exited", "dead":
				return stateDied
			default:
				// paused, restarting, created, unknown: not a confirmed death,
				// not confirmed healthy.
				lastRunning = false
			}
		}

		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			// Report the best verdict so far rather than inventing a death.
			if lastRunning {
				return stateStayedRunning
			}
			return stateUncertain
		case <-time.After(interval):
		}
	}

	if sawAny && lastRunning {
		return stateStayedRunning
	}
	return stateUncertain
}

// normalizeState lowercases and trims a runtime State string so the switch is
// insensitive to adapter formatting.
func normalizeState(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// findCollector resolves the collector container id from the marker label, or
// the configured fallback name when nothing is marked. It returns:
//
//   - id != "": a single, unambiguous, running collector was resolved.
//   - id == "", err == nil: no collector to signal (none found, or the resolved
//     one is not running). note explains it, informationally.
//   - err != nil: an identity error (multiple markers, or a marker that
//     disagrees with a different configured collector). No signal must be sent.
//
// The marker is read with the same both-prefix awareness discovery uses: a
// "collector" value that parses as true under either prefix marks the container.
func (b *Backend) findCollector(ctx context.Context) (id string, note string, err error) {
	containers, err := b.rt.List(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list containers: %w", err)
	}

	var marked []runtime.Container
	for _, c := range containers {
		if markedCollector(c.Labels) {
			marked = append(marked, c)
		}
	}

	// Ambiguity: more than one marked container is a hard identity error.
	if len(marked) > 1 {
		names := make([]string, 0, len(marked))
		for _, c := range marked {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("multiple containers carry the bilgeline.collector marker: %s", strings.Join(names, ", "))
	}

	var chosen *runtime.Container
	if len(marked) == 1 {
		m := marked[0]
		chosen = &m
		// Disagreement: a marker AND a configured collector naming a DIFFERENT
		// existing container. A configured name that matches the marked
		// container, or that names nothing here, is not a conflict.
		if b.collectorName != "" {
			if other, ok := findByNameOrID(containers, b.collectorName); ok && other.ID != m.ID {
				return "", "", fmt.Errorf("collector marker on %q disagrees with configured collector %q", m.Name, b.collectorName)
			}
		}
	} else if b.collectorName != "" {
		if c, ok := findByNameOrID(containers, b.collectorName); ok {
			chosen = &c
		}
	}

	if chosen == nil {
		return "", "no collector found, config written, it will be read when the collector starts", nil
	}
	if normalizeState(chosen.State) != "running" {
		return "", fmt.Sprintf("collector %q is not running, config written, it will be read when the collector starts", chosen.Name), nil
	}
	return chosen.ID, "", nil
}

// Healthy reports whether the collector is currently discoverable and running.
// When a health URL is configured it additionally probes it, but the primary,
// network-independent signal is the container state.
func (b *Backend) Healthy(ctx context.Context) error {
	if b.rt == nil {
		return errors.New("otelcol: no runtime configured")
	}
	id, note, err := b.findCollector(ctx)
	if err != nil {
		return fmt.Errorf("otelcol: %w", err)
	}
	if id == "" {
		return fmt.Errorf("otelcol: %s", note)
	}
	col, err := b.rt.Inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("otelcol: inspect collector: %w", err)
	}
	if normalizeState(col.State) != "running" {
		return fmt.Errorf("otelcol: collector %q is not running (state %q)", col.Name, col.State)
	}
	if b.collectorHealthURL != "" {
		if err := httpProbe(ctx, b.collectorHealthURL); err != nil {
			return fmt.Errorf("otelcol: collector %q running but health probe failed: %w", col.Name, err)
		}
	}
	return nil
}

// markedCollector reports whether a container's raw labels mark it as the
// collector under either recognized prefix. A present value that parses as
// boolean true marks it.
func markedCollector(labels map[string]string) bool {
	for _, p := range []string{markerPrefixPrimary, markerPrefixAlias} {
		if v, ok := labels[p+collectorMarker]; ok {
			if parsed, err := strconv.ParseBool(v); err == nil && parsed {
				return true
			}
		}
	}
	return false
}

// findByNameOrID returns the first container whose name (with or without a
// leading slash) or id (full or a prefix) matches ref.
func findByNameOrID(containers []runtime.Container, ref string) (runtime.Container, bool) {
	ref = strings.TrimPrefix(ref, "/")
	for _, c := range containers {
		if strings.TrimPrefix(c.Name, "/") == ref {
			return c, true
		}
		if c.ID == ref || (len(ref) >= 12 && strings.HasPrefix(c.ID, ref)) {
			return c, true
		}
	}
	return runtime.Container{}, false
}

// mountsSharedPath reports whether any mount covers the shared config path: a
// mount whose Destination is the file itself, is the directory containing it, or
// is an ancestor directory of it. This accepts both a file bind of the config
// and a volume or bind of the directory it lives in.
func mountsSharedPath(mounts []runtime.Mount, sharedPath string) bool {
	target := filepath.Clean(sharedPath)
	dir := filepath.Dir(target)
	for _, m := range mounts {
		dest := filepath.Clean(m.Destination)
		if dest == target || dest == dir || isAncestorDir(dest, target) {
			return true
		}
	}
	return false
}

// isAncestorDir reports whether dir is an ancestor directory of path (a strict
// prefix on a path-separator boundary), so "/config" covers "/config/otelcol.yaml"
// but "/con" does not cover "/config/otelcol.yaml".
func isAncestorDir(dir, path string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/") && path != "/"
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// envPreflight returns a warning line for every ${env:VAR} the rendered config
// references that the collector's environment does not carry. It compares NAMES
// only: env entries are split at the first "=" and the value is never read,
// compared, or logged.
func envPreflight(data []byte, env []string) []string {
	refs := referencedEnvNames(data)
	if len(refs) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(env))
	for _, e := range env {
		name := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			name = e[:i]
		}
		present[name] = struct{}{}
	}
	var warnings []string
	for _, name := range refs {
		if _, ok := present[name]; !ok {
			warnings = append(warnings, fmt.Sprintf("collector is missing referenced env var %s (its exporter may fail silently)", name))
		}
	}
	return warnings
}

// referencedEnvNames returns the sorted, de-duplicated set of ${env:NAME} names
// in the rendered config bytes.
func referencedEnvNames(data []byte) []string {
	seen := map[string]struct{}{}
	for _, m := range envRefPattern.FindAllSubmatch(data, -1) {
		seen[string(m[1])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// joinDetail appends any warning lines to a base detail string.
func joinDetail(base string, warnings []string) string {
	if len(warnings) == 0 {
		return base
	}
	return base + "; " + strings.Join(warnings, "; ")
}

// writeAtomic writes data to path by writing a temp file in the same directory
// and renaming it into place, creating the directory if it is missing. The
// rename is atomic on the same filesystem, so a reader never sees a partial
// config.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".otelcol-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// httpProbe is a best-effort GET used only as a secondary confirmation. A
// non-2xx or a transport error is returned; the caller decides how much weight
// to give it, and never restarts solely because it failed.
func httpProbe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}
