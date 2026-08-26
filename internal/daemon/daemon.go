// SPDX-License-Identifier: GPL-3.0-or-later

// Package daemon wires every bilgeline package together into the long-running,
// event-driven control loop: it loads config, selects the container runtime,
// resolves bilgeline's own container id for self-exclusion, builds the pluggable
// backend (otelcol in v1) and the beacon notifier, runs an initial reconcile,
// then watches the container socket and reconciles on debounced lifecycle churn
// until its context is cancelled.
//
// bilgeline is NOT cron-scheduled: unlike ballast's scheduler, there is no
// clock here. The loop reacts only to container lifecycle events (coalesced
// through a quiet-window debounce) and to a single startup pass, and every pass
// is hash-diffed so an event that changes nothing costs a discovery walk and no
// backend apply. The CLI's "daemon" command calls Run as its entry point.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/tagwright/bilgeline/internal/backend"
	"github.com/tagwright/bilgeline/internal/backend/otelcol"
	"github.com/tagwright/bilgeline/internal/config"
	"github.com/tagwright/core/runtime"
)

// defaultDockerSocket is used when neither BILGELINE_SOCKET nor DOCKER_HOST names
// a socket path and the selected runtime is Docker.
const defaultDockerSocket = "/var/run/docker.sock"

// Run loads configPath, wires up every collaborator, runs an initial reconcile,
// and then drives the debounced watch loop until ctx is cancelled. Signal
// handling belongs to the caller: Run itself only ever reacts to ctx. It always
// closes the runtime before returning.
func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("daemon: load config: %w", err)
	}

	debounce, err := cfg.DebounceDuration()
	if err != nil {
		// Load already validated this, so it cannot fail here; guard anyway so a
		// future change cannot silently ship a zero window.
		return fmt.Errorf("daemon: %w", err)
	}

	rt, err := BuildRuntime()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() {
		if cerr := rt.Close(); cerr != nil {
			logger.Warn("daemon: close runtime", "error", cerr)
		}
	}()

	selfID := ResolveSelfID(ctx, rt, logger)

	be := BuildBackend(rt, cfg)

	notifier, err := buildNotifier()
	if err != nil {
		return fmt.Errorf("daemon: build notifier: %w", err)
	}

	logger.Info("daemon: starting",
		"backend", be.Name(),
		"self_id", selfID,
		"shared_config_path", cfg.SharedConfigPath,
		"debounce", debounce.String())

	r := &reconciler{
		rt:       rt,
		cfg:      cfg,
		backend:  be,
		notifier: notifier,
		logger:   logger,
		selfID:   selfID,
		debounce: debounce,
	}
	r.run(ctx)

	return nil
}

// BuildBackend constructs the backend the loop drives, behind the backend.Backend
// interface so a future Fluent Bit backend drops in with no change to the loop.
// v1 is always otelcol. The deployment-level knobs (file-storage checkpoint dir,
// health-check endpoint, and an optional collector health URL) fall back to the
// backend's own defaults and are overridable through the environment.
func BuildBackend(rt runtime.Runtime, cfg *config.Config) backend.Backend {
	opts := []otelcol.Option{
		otelcol.WithRuntime(rt),
		otelcol.WithSharedConfigPath(cfg.SharedConfigPath),
		otelcol.WithCollectorName(cfg.Collector),
	}
	if v := os.Getenv("BILGELINE_FILE_STORAGE_DIR"); v != "" {
		opts = append(opts, otelcol.WithFileStorageDir(v))
	}
	if v := os.Getenv("BILGELINE_HEALTH_ENDPOINT"); v != "" {
		opts = append(opts, otelcol.WithHealthCheckEndpoint(v))
	}
	if v := os.Getenv("BILGELINE_COLLECTOR_HEALTH_URL"); v != "" {
		opts = append(opts, otelcol.WithCollectorHealthURL(v))
	}
	return otelcol.New(opts...)
}

// BuildRuntime selects and constructs the container runtime. bilgeline carries
// no runtime field in its config file, so selection is env-only: BILGELINE_RUNTIME
// picks docker (the default) or podman, and BILGELINE_SOCKET overrides the socket
// path. This mirrors ballast's docker/podman split without adding config surface
// bilgeline does not otherwise need.
func BuildRuntime() (runtime.Runtime, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BILGELINE_RUNTIME"))) {
	case "", "docker":
		return runtime.NewDocker(dockerSocket()), nil
	case "podman":
		return runtime.NewPodman(podmanSocket()), nil
	default:
		return nil, fmt.Errorf("unknown runtime %q, want \"docker\" or \"podman\"", os.Getenv("BILGELINE_RUNTIME"))
	}
}

// dockerSocket resolves the Docker API socket path: BILGELINE_SOCKET if set,
// otherwise DOCKER_HOST (with a "unix://" scheme prefix stripped, since NewDocker
// wants a bare path), otherwise the conventional default.
func dockerSocket() string {
	if v := os.Getenv("BILGELINE_SOCKET"); v != "" {
		return v
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return defaultDockerSocket
}

// podmanSocket resolves the Podman API socket path: BILGELINE_SOCKET if set,
// otherwise CONTAINER_HOST (with a "unix://" scheme prefix stripped, the same
// convention podman-remote uses), otherwise empty, which tells runtime.NewPodman
// to fall back to its own rootless/rootful default-socket resolution.
func podmanSocket() string {
	if v := os.Getenv("BILGELINE_SOCKET"); v != "" {
		return v
	}
	if v := os.Getenv("CONTAINER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return ""
}

// containerIDPattern matches a full 64-hex container id anywhere in a line, the
// shape Docker and Podman both write into /proc/self/cgroup and mountinfo.
var containerIDPattern = regexp.MustCompile(`[0-9a-f]{64}`)

// ResolveSelfID determines bilgeline's OWN full container id, passed to Discover
// so bilgeline never routes its own logs (a pipeline shipping its own errors to a
// failing sink is a feedback loop). The hint is normalized to the full 64-hex id
// through an Inspect, since a bare hostname is only the short id and Discover
// compares against the full one.
//
// Resolution order for the hint: the BILGELINE_SELF_ID override, then a 64-hex id
// scraped from /proc/self/cgroup or /proc/self/mountinfo, then the hostname
// (which Docker sets to the short container id by default). An empty result (not
// running in a container, id not discoverable) is tolerated: self-exclusion then
// falls back to the bilgeline.collector marker the operator sets, and the loop
// still runs. A best-effort resolution failure is logged, not fatal.
func ResolveSelfID(ctx context.Context, rt runtime.Runtime, logger *slog.Logger) string {
	hint := selfIDHint()
	if hint == "" {
		logger.Warn("daemon: could not determine self container id; relying on the bilgeline.collector marker for self-exclusion")
		return ""
	}

	// Normalize to the full id. Inspect accepts a short id, a name, or a full id
	// and returns the canonical full id Discover compares against.
	if c, err := rt.Inspect(ctx, hint); err == nil && c.ID != "" {
		return c.ID
	}

	// Inspect failed (id already full and valid but the socket is unreachable, or
	// the hint is not a container we can see). A full 64-hex hint is usable as-is;
	// a short hint that would not match is still returned, with a warning.
	if len(hint) == 64 && containerIDPattern.MatchString(hint) {
		return hint
	}
	logger.Warn("daemon: could not normalize self id to a full container id; self-exclusion may not match",
		"hint", hint)
	return hint
}

// selfIDHint gathers the best available hint for bilgeline's own container id
// without touching the socket: the env override, else a 64-hex id from the
// process's cgroup or mountinfo, else the hostname.
func selfIDHint() string {
	if v := strings.TrimSpace(os.Getenv("BILGELINE_SELF_ID")); v != "" {
		return v
	}
	for _, path := range []string{"/proc/self/cgroup", "/proc/self/mountinfo"} {
		if id := containerIDFromFile(path); id != "" {
			return id
		}
	}
	if h, err := os.Hostname(); err == nil {
		return strings.TrimSpace(h)
	}
	return ""
}

// containerIDFromFile returns the first 64-hex container id found in the file at
// path, or empty if the file is unreadable or carries none. Works for both cgroup
// v1 (the id is in /proc/self/cgroup) and v2 (the id is in the container's
// /var/lib/docker/containers/<id> mount recorded in /proc/self/mountinfo).
func containerIDFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return containerIDPattern.FindString(string(data))
}
