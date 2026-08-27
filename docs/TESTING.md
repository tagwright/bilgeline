# Testing

bilgeline's test methodology and an honest accounting of what has actually been
proven to work end to end, as opposed to what merely compiles or is exercised
against a fake.

## How we test

Two layers, in increasing order of how much they prove:

1. **Unit tests, in-tree** (`go test ./...`). Pure-function coverage: the label
   grammar and its two-prefix conflict rules (`internal/discovery`), config load
   / env-overlay / validation (`internal/config`), the whole otelcol config
   translation (`internal/backend/otelcol/render_test.go` against golden YAML in
   `testdata/`), the apply-path selection and mount-guard logic with a fake
   runtime (`internal/backend/otelcol/apply_test.go`), and the debounce /
   hash-diff / diagnostic-routing control loop with a fake runtime and fake
   backend (`internal/daemon`). Fast, no socket. These are strong for the pure
   translation but by construction cannot prove that the generated config is one
   a real collector accepts, that a real SIGHUP reloads it, or that a real
   filelog receiver can read a real Docker json log.

2. **`test/integration/` harness, against a live Docker socket.** This is where
   bilgeline is proven end to end: build the real image from the real
   Dockerfile, run a real `otel/opentelemetry-collector-contrib:0.159.0`, real
   producer containers logging known unique lines, and the real `bilgeline`
   binary as a container driving the socket. Then read the bytes that land at
   the destination. Every object these scripts create is prefixed
   `bilgeline-itest-`, the teardown only ever removes objects matching that
   prefix, and each case tears down in a trap on exit (success or failure). See
   `test/integration/README.md` to run it.

## What is integration-PROVEN (live, against a real socket + real collector)

All five cases pass against Docker 29.1.3 with the pinned collector 0.159.0.

- **The core route, end to end** (`00_core_route.sh`). A container labeled
  `bilgeline.enable=true` / `bilgeline.destination=itestfile` is discovered;
  bilgeline generates the otelcol config, writes it atomically to the shared
  volume, finds the `bilgeline.collector=true` collector, and SIGHUPs it; the
  collector reloads, the filelog receiver tails the producer's
  `/var/lib/docker/containers/<id>/<id>-json.log`, and the lines land at the
  `file` exporter. Proven by matching the producer's known unique token in the
  exported JSON, carrying the expected attributes:

  ```
  resource.attributes:
     service.name          = bilgeline-itest-producer
     container.id          = <full 64-hex id>
     container.name        = bilgeline-itest-producer
     container.image.name  = alpine:3.20
     bilgeline.routing_key = itestfile
  body = BILGELINE-ITEST-<run>-<n>
  log record attributes:
     container_id  = <full 64-hex id>   (recovered from the file path)
     log.file.path = /var/lib/docker/containers/<id>/<id>-json.log
     log.iostream  = stdout
  ```

  This is the proof that discovery -> generation -> atomic write -> collector
  find -> SIGHUP -> in-process reload -> filelog tail -> export all work
  **together**, not just in isolation.

- **The SIGHUP reload mechanism itself.** Confirmed empirically: on SIGHUP the
  collector logs `Starting shutdown... / Shutdown complete` then
  `Starting otelcol-contrib... / Everything is ready` and comes back on the NEW
  config. otelcol-contrib 0.159.0 reloads in-process on SIGHUP rather than
  exiting, so bilgeline's `ActionReloaded` path is the real steady-state path,
  and the wedge-recovery restart is the exception.

- **The live event-driven reload** (`00_core_route.sh`, second half). With the
  stack already routing, a SECOND labeled producer is started; bilgeline gets
  the socket event, debounces, regenerates, and SIGHUPs, and the second
  producer's lines then also land at the destination. Proves the watch loop, not
  just the startup reconcile.

- **Mount guard** (`10_mount_guard.sh`). A `bilgeline.collector=true` container
  that does not mount the shared config path is never signalled: the config is
  still written, bilgeline logs `... refusing to signal: collector "..." does
  not mount the shared config path`, and the innocent container is left running
  with RestartCount 0.

- **Collector ambiguity** (`11_ambiguity.sh`). Two `bilgeline.collector=true`
  containers is a hard identity error: the config is written, bilgeline logs
  `multiple containers carry the bilgeline.collector marker: ...`, NO signal is
  sent, and neither candidate is touched.

- **Non-json-file log driver exclusion** (`12_local_driver.sh`). A producer run
  with `--log-driver local` is excluded: bilgeline logs
  `log driver "local" is not json-file; excluded from routing`, its container id
  never enters the generated filelog includes, and its lines never reach the
  destination. A json-file producer running alongside it still routes, so the
  exclusion is surgical, not a blanket failure.

- **Env preflight + the S1 secrets model** (`13_env_preflight.sh`). A
  destination referencing a `${env:VAR}` the collector lacks yields a bilgeline
  warning that NAMES the var (`missing referenced env var BILGELINE_ITEST_MISSING`).
  Both `${env:...}` references are copied verbatim into the generated config, a
  present secret's VALUE appears nowhere in the generated config, and it appears
  in NO bilgeline log line. bilgeline never expands a secret.

- **Alert delivery through beacon (config to the wire).** The
  notifications/telemetry path is proven at two levels. Unit
  (`internal/daemon/notifier_test.go`, `internal/secret/resolver_test.go`,
  `internal/config/config_test.go`): the config sections parse and validate,
  the domain-2 secret resolver reads a named token from a file or a
  `BILGELINE_SECRET_<NAME>` env var with consistent trailing-newline trimming,
  and `buildNotifier` wires the always-on `log` floor plus configured channels
  and sinks, delivering a real POST to an httptest ntfy and gatus with the
  resolved bearer token in the header. Live: a guarded test
  (`TestBuildNotifierLiveNtfy`, skipped unless `BILGELINE_ITEST_NTFY_URL` is
  set) stands up a throwaway `bilgeline-itest-ntfy` container, builds the
  notifier exactly as the daemon does, sends an error alert, and polls ntfy's
  JSON API to assert the message arrived. Run it with a live ntfy on the itest
  network, tearing the container down on success and failure.

## What is unit-proven / compile-only (NOT exercised live here)

- **Live wedge recovery to a healthy collector.** The `watchState` /
  restart-once logic is unit-tested with a fake runtime
  (`apply_test.go`). Live, we proved the two ends of it: a SIGHUP reload that
  *succeeds* (the steady-state path, `00_core_route`), and a reload that *wedges
  and does not recover* (a `${env:VAR}` missing from a structural config
  position makes the collector fail to load; bilgeline restarts once, it dies
  again, and Apply reports it) — this is how the reconcile-detail bug below was
  found. The middle outcome, a wedge that *recovers* on the one restart, is not
  forced live: inducing a reload that fails once and then succeeds needs a
  collector that crashes on exactly one SIGHUP, which we have no clean lever for.
  It stays unit-proven.

- **Podman.** `BILGELINE_RUNTIME=podman` selects the podman runtime
  adapter in `core/runtime`, unit-covered there, but the integration harness
  runs Docker only. Proving the podman path needs a real Podman host.

- **The full processing grammar** (multiline recombine, regex/json/logfmt
  parse, timestamp and severity extraction, drops, stream filtering, promoted
  labels, profiles, multi-destination fan-out and shared-signature receiver
  grouping). All are unit-tested at the render layer against golden YAML, and
  the live cases here use the simple `parse=none` path. The generated operators
  for the richer grammar are not byte-verified against a live collector's output
  in this harness.

## What is UNTESTED (needs resources this environment does not have)

- **Real destinations.** The live proof uses the `file` exporter because it is
  the one destination whose bytes we can verify locally. Real Loki (`otlphttp`),
  `otlp`, and `elasticsearch` destinations need the user's endpoints and
  credentials; they are unit-proven at the config-generation layer only.
  Delivery to them is not proven.

- **Credential rotation via collector recreation.** The
  `docker compose up -d --force-recreate otel-collector` rotation flow in
  `docs/DEPLOY.md` is documented, not tested here.

- **The compose stack as a unit** (`docker compose up`). The harness runs the
  same containers with the same wiring by hand (so it can inject failure cases
  and read volumes), but does not invoke `docker compose` on the shipped
  `docker-compose.yml`. The two deployment defects below were found and fixed
  in that shipped file, but a literal `docker compose up` of it was not run.

## Environment specifics these tests depend on

Two ownership facts are load-bearing for the live stack, both mirrored between
the harness and the fixed `docker-compose.yml`:

- bilgeline runs as distroless `nonroot` (uid 65532). It needs the socket group
  (`--group-add` / compose `group_add`-equivalent) and it needs the shared
  config directory owned by 65532 (the config-seed chowns it).
- the collector runs as **root** (`--user 0:0`). Docker's json-file logs are
  `root:root` mode 0640, unreadable by the collector image's default uid 10001,
  which would make the filelog receiver silently tail nothing.

## Bugs this integration pass found and fixed

1. **The shipped compose could never route: the collector could not read the
   logs.** `otel-collector-contrib` runs as uid 10001; Docker json logs are
   `root:root` 0640. The receiver read zero bytes with no error and the whole
   pipeline shipped nothing silently. Fixed by setting `user: "0:0"` on the
   collector in `docker-compose.yml`.

2. **The shipped compose could never write the config: bilgeline could not write
   the shared dir.** The config-seed created `/config` as root; bilgeline (uid
   65532) could not create its atomic temp file there and every reconcile failed
   `permission denied`. Fixed by having the config-seed `chown -R 65532:65532
   /config`.

3. **`default_destination: debug` was rejected at config load** though a
   container routing to the reserved `debug` sink is always valid. bilgeline
   refused to start. Fixed in `internal/config/config.go` (exempt the reserved
   `debug` name), with a regression test.

4. **A wedged collector reached the operator with the cause discarded.** On an
   Apply error (e.g. a collector wedged by a missing `${env:VAR}`), the reconcile
   logged only the terse error and threw away `ApplyResult.Detail` — the one
   field carrying the env-preflight warning that named the missing var and the
   wedge narrative. Fixed in `internal/daemon/reconcile.go` to log the detail on
   the error path too, with a regression test.
