# bilgeline live integration harness

Shell-driven integration tests that run bilgeline as a real container against a
**live Docker socket**, alongside a real `otel/opentelemetry-collector-contrib`
collector and real producer containers, and prove the whole chain end to end:

    discover labels -> generate otelcol config -> atomic write to the shared
    volume -> find + SIGHUP the collector -> collector reloads -> filelog tails
    the producer's json log -> export to the destination

These are NOT unit tests. They need a Docker daemon and the right to create
sibling containers, volumes, and a network. Run them where that is acceptable.

## Safety

Every Docker object the harness creates is prefixed `bilgeline-itest-`
(containers, volumes, network). The teardown (`teardown` in `lib.sh`) only ever
removes objects matching that prefix, via anchored name filters. It never stops,
removes, or signals anything else on the host. Each case sets a trap so a
mid-test failure still cleans up, and `run.sh` does a final sweep and fails if
any `bilgeline-itest-*` object is left behind.

## Prerequisites

- A reachable Docker socket at `/var/run/docker.sock`.
- The bilgeline image tagged `bilgeline-itest:local`. Build it from the parent
  of `core/` and `bilgeline/` (the Dockerfile needs both because `go.mod` has
  `replace github.com/tagwright/core => ../core`). A `.git`-free staging copy
  keeps the build context small:

  ```sh
  stage=$(mktemp -d)
  for d in core bilgeline; do
    mkdir -p "$stage/$d"
    (cd /path/to/$d && tar --exclude=.git -cf - .) | (cd "$stage/$d" && tar -xf -)
  done
  docker build -f "$stage/bilgeline/Dockerfile" -t bilgeline-itest:local "$stage"
  ```

- The pinned collector, plus busybox and alpine (the harness pulls nothing;
  pull first if offline):

  ```sh
  docker pull otel/opentelemetry-collector-contrib:0.159.0
  docker pull busybox:1.37
  docker pull alpine:3.20
  ```

## Running

```sh
test/integration/run.sh            # all cases
test/integration/run.sh 00 12      # only cases whose file starts 00 / 12
```

Each case is also runnable on its own, e.g. `test/integration/00_core_route.sh`.

## The cases

| File | Proves |
| --- | --- |
| `00_core_route.sh` | Core route end to end: a labeled producer's known unique lines reach a `file` destination carrying `service.name`, `container.id`, `container.name`, `log.iostream`. Then a SECOND producer started live proves the event-driven regenerate + SIGHUP reload. |
| `10_mount_guard.sh` | A `bilgeline.collector=true` container that does not mount the shared config path is NOT signalled: config still written, refusal diagnostic emitted, innocent container untouched (RestartCount 0). |
| `11_ambiguity.sh` | TWO marked collectors is a hard identity error: config written, NO signal sent, both candidates untouched. |
| `12_local_driver.sh` | A `--log-driver local` producer is excluded (its lines never reach the destination, its id never enters the generated config) and bilgeline warns; a json-file producer alongside it still routes. |
| `13_env_preflight.sh` | A destination referencing a `${env:VAR}` the collector lacks yields a warning that NAMES the var; `${env:...}` references are copied verbatim; a present secret's VALUE never appears in the generated config nor in any bilgeline log line (S1 secrets model). |

## Environment notes

- bilgeline runs as distroless `nonroot` (uid 65532) and must reach the socket,
  so the harness adds it to the socket's group (`--group-add`). It also seeds
  the shared config volume and hands that directory to uid 65532, mirroring the
  fixed `docker-compose.yml` config-seed. Both are the minimal, legitimate
  grants any real deployment arranges.
- The collector runs as **root** (`--user 0:0`). Docker's json-file logs are
  `root:root` mode `0640`; the collector image's default uid (10001) cannot read
  them, so it would silently tail nothing. This mirrors the `user: "0:0"` the
  fixed `docker-compose.yml` sets on the collector.

See `../../docs/TESTING.md` for the honest coverage map: what these prove live,
what is unit/compile-only, and what is genuinely untested.
