# Operations and troubleshooting

How to run and debug bilgeline day to day. This is the operations companion to
[DEPLOY.md](DEPLOY.md), which covers the compose stack, the mounts, and the
secret model. Where this guide needs a deployment fact, it links there rather
than repeat it.

## The three inspection commands

bilgeline has three read-only commands for seeing what it would do without
touching a running collector. Run them inside the bilgeline container, for
example `docker compose exec bilgeline bilgeline generate`.

### generate: see the config that would be produced

`bilgeline generate` (alias `render`) discovers the containers currently opted
in, renders the collector config, and prints it to stdout. It writes nothing and
signals nothing. Use it to preview exactly what the daemon would apply, and to
read the `${env:...}` references so you know which environment variables the
collector must carry. Diagnostics are intentionally omitted here (that is
`validate`'s job), so `generate` is purely about the produced YAML. It is safe to
pipe to a file or a pager.

### validate: check labels and config

`bilgeline validate` loads bilgeline.yml, checks it for internal consistency,
and, unless `--config-only` is given, discovers the opted-in containers and
reports every diagnostic their labels produce. It prints `config: OK` or
`config: INVALID`, then one line per diagnostic, then a summary count. It exits
nonzero when the config fails validation or any container produces an
error-severity diagnostic (a bad enum, an unknown destination, an invalid regex,
a prefix conflict). Warnings are reported but do not fail the check. That nonzero
exit makes it a clean pre-deploy or CI gate. Use `--config-only` to check just
the file when no runtime is reachable.

### status: see discovered services and collector health

`bilgeline status` prints the backend name, the resolved collector (the marker,
or the configured fallback name), the collector's health, and each routed
service with its short container id and its destinations. A service routed
nowhere (the `none` sentinel) shows as `none (dropped at source)`. The health
line reads `healthy` when a single unambiguous collector is running, or
`unavailable: <reason>` otherwise. It changes nothing.

## What the daemon does on a change

`bilgeline daemon` is the steady-state process and the container's default
command. It loads config, resolves its own container id for self-exclusion,
builds the collector backend and the notifier, runs one reconcile at startup,
then watches the container socket. Every relevant lifecycle event (start, stop,
die, destroy) arms a debounce timer (`BILGELINE_DEBOUNCE`, default `2s`). When
the socket goes quiet for the window, one reconcile runs: discover the fleet,
route diagnostics to logs and alerts, assemble the spec, and, only when the
spec's hash differs from the last applied one, render and apply. A burst (a
compose up, a crash loop) collapses into a single reconcile, and an event that
changes nothing costs a discovery walk and no apply.

## What happens when the collector wedges

When bilgeline applies a new config it always writes the file first, atomically,
even with no collector running, so the config is in place for when the collector
starts. Then, if a single unambiguous collector that mounts the shared config
path is running, bilgeline sends it a `SIGHUP` to reload in place.

otelcol-contrib reloads in-process on SIGHUP, so a healthy reload is the normal
path. To catch the reload-crash class of bug, bilgeline then polls the
collector's container state for a bounded window (default 5 seconds).

- If the collector stays running, the reload took. bilgeline reports
  `reloaded`.
- If the collector dies (enters `exited` or `dead`) within the window, the
  reload wedged the process. bilgeline restarts the container once. If it comes
  back running, bilgeline reports `restarted` and it recovered. If it dies
  again, bilgeline gives up (one restart only), reports the failure, and alerts.
  It never restarts a second time.
- An uncertain state (paused, a restarting state that never resolves, or an
  inspect error) never triggers a restart. bilgeline reports the SIGHUP was sent
  but the state could not be confirmed.

An optional `BILGELINE_COLLECTOR_HEALTH_URL` adds a secondary HTTP probe, but it
is advisory only. A restart is never triggered because that probe was
unreachable. The container state is always the primary signal.

### Reading the alert

Every discovery and apply problem lands in the structured log stream (the
always-on floor) and, when a beacon channel is configured, an alert. On an apply
failure the alert body carries `ApplyResult.Detail` in front of the terse error,
so the actionable context travels with it: the collector name, the
wedge-recovery narrative, and any env-preflight warning naming a missing
`${env:VAR}`. A wedged-collector alert reads like `... collector "otel-collector"
died again after its one recovery restart, giving up; collector is missing
referenced env var LOKI_BEARER ...`. The missing-var line is usually the actual
cause: a `${env:VAR}` the collector cannot expand can make it fail to load the
new config and die on the reload.

## Credential rotation means recreating the collector

The collector expands `${env:VAR}` once, at config load, into its process
environment, and a running container's environment is fixed. So rotating a
credential is not a bilgeline operation and not a reload. Update the value and
recreate the collector container (for the compose stack, `docker compose up -d
--force-recreate otel-collector`). bilgeline regenerates nothing for a rotation:
the config still references the same `${env:VAR}` name, only the value behind it
changed, and only the collector reads it. The full rotation and SOPS recipe is in
[DEPLOY.md](DEPLOY.md).

## Troubleshooting

### Logs are not arriving at the destination

Work down the chain from the container to the sink.

- **The container's log driver must be `json-file`.** v1 tails only Docker's
  json-file driver through the collector's filelog receiver. A container run with
  a different driver (for example `--log-driver local`, whose compressed,
  length-prefixed stream cannot be tailed as text) is excluded with a warning
  diagnostic and its id never enters the generated filelog includes. Check for a
  `log driver "..." is not json-file` warning in the bilgeline logs. An empty or
  unknown driver is treated as json-file compatible, so a stock Docker host
  routes normally.
- **The collector must mount both paths.** It needs the shared config volume
  (read-only) so it reads what bilgeline writes, and `/var/lib/docker/containers`
  (read-only) as the filelog source. Only the collector mounts the containers
  path, never bilgeline. See the mounts in [DEPLOY.md](DEPLOY.md).
- **The collector must be able to read the json logs.** Docker writes them
  `root:root` mode 0640, so the collector image's default non-root uid reads zero
  bytes with no error and the pipeline silently ships nothing. The shipped stack
  runs the collector as root (`user: "0:0"`) for exactly this reason. If your
  host makes the json logs readable by another uid or gid, pin that instead. This
  is the single most common cause of a silent no-logs failure.
- **A referenced `${env:VAR}` must be present on the collector.** bilgeline runs
  an env preflight on apply and warns, by name, on any `${env:VAR}` the generated
  config references that the collector does not carry (values are never read or
  logged). A missing var means the exporter may fail to authenticate or the
  collector may fail to load the config. Run `bilgeline generate` and read the
  `${env:...}` references, then confirm each is set on the collector.
- **Confirm what bilgeline actually discovered** with `bilgeline status` and
  **what it would generate** with `bilgeline generate`. If a container you
  expected is missing from `status`, it did not opt in (no `bilgeline.enable=true`)
  or it hit an error diagnostic. Run `bilgeline validate` to see the reason.

### bilgeline wrote the config but did not signal the collector

bilgeline always writes the config, then applies two safety refusals that skip
the signal without failing the apply. The config is still in place, and the
reason is logged and alerted.

- **Mount guard.** bilgeline refuses to SIGHUP a marked collector that does not
  mount the shared config path, because a mislabeled marker would otherwise make
  it signal an innocent container. The log reads `refusing to signal: collector
  "..." does not mount the shared config path`. Fix the collector's config-volume
  mount.
- **Collector ambiguity.** More than one container carrying the
  `bilgeline.collector` marker is a hard identity error, and so is a marker that
  disagrees with a different configured `collector:` name. bilgeline logs
  `multiple containers carry the bilgeline.collector marker: ...` (or the
  disagreement message) and sends no signal. Ensure exactly one container is
  marked, and that any `collector:` name in bilgeline.yml either matches it or
  names nothing present.

### A container is skipped with an error diagnostic

A single bad container is skipped and reported. It never aborts discovery for the
rest of the fleet. The error diagnostic names the container and the fault: an
invalid boolean or enum value, an invalid regex in `multiline` or `drop`, an
unknown destination name, an unknown profile name, no destination and no
configured default, or a prefix conflict (the same suffix under `bilgeline.*` and
`tagwright.log.*` with different values). Run `bilgeline validate` to see all of
them at once, and consult [LABELS.md](LABELS.md) for the exact rule.

## Runtime and deployment environment variables

These are set on the bilgeline container. The fleet-wide grammar globals
(`BILGELINE_DEFAULT_DESTINATION`, `BILGELINE_LABELS`, `BILGELINE_DEBOUNCE`) are
documented in [LABELS.md](LABELS.md). The deployment and runtime knobs are:

| Variable | Default | Meaning |
|---|---|---|
| `BILGELINE_RUNTIME` | `docker` | Container runtime, `docker` or `podman`. |
| `BILGELINE_SOCKET` | runtime default | Override the container API socket path. Falls back to `DOCKER_HOST` / `CONTAINER_HOST` (with a `unix://` prefix stripped), then the conventional default. |
| `BILGELINE_SELF_ID` | auto-detected | Override bilgeline's own container id for self-exclusion. Normally scraped from `/proc/self/cgroup` or `/proc/self/mountinfo`, or the hostname. |
| `BILGELINE_FILE_STORAGE_DIR` | `/var/lib/otelcol/file_storage` | The filelog checkpoint directory the generated config points the collector at. |
| `BILGELINE_HEALTH_ENDPOINT` | `0.0.0.0:13133` | The health_check listen endpoint in the generated config. |
| `BILGELINE_COLLECTOR_HEALTH_URL` | unset (off) | Optional secondary HTTP health probe. Advisory only, never drives a restart. |

The default Docker socket is `/var/run/docker.sock`. For Podman, an empty
`BILGELINE_SOCKET` lets the runtime resolve its own rootless or rootful default.
If bilgeline cannot determine its own container id, self-exclusion falls back to
the `bilgeline.collector` marker and the loop still runs.
