# bilgeline

Label-driven log routing for Docker and Podman. bilgeline reads `bilgeline.*`
labels off your running containers and generates an OpenTelemetry Collector
configuration that routes each service's logs to the destination you named. You
describe where logs should go in the compose file, next to the service they
belong to, and bilgeline keeps the collector config in sync as containers come
and go.

bilgeline is a pure generator. It does not bundle, ship, or run a collector. You
deploy your own `otelcol-contrib` container, and bilgeline drives it: it writes
the generated config to a shared volume and signals the collector to reload.
Because the collector does the tailing and shipping, bilgeline holds no exporter
credentials and never reads a log byte.

## The two-container model

bilgeline runs as two containers: bilgeline itself, and a collector you own.
bilgeline mounts the container socket and a shared config volume. The collector
mounts that same config volume read-only, plus `/var/lib/docker/containers` where
the Docker json logs live. bilgeline discovers and generates, the collector tails
and ships, and secrets live only on the collector side. The full compose stack,
the mounts, first-boot ordering, and the SOPS secret recipe are in
[docs/DEPLOY.md](docs/DEPLOY.md).

## Quickstart

A service opts in with labels next to the service it describes:

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    labels:
      bilgeline.enable: "true"
      bilgeline.destination: "loki"
```

The label names a destination. The destination itself (its endpoint, headers,
and any credentials) is defined once in bilgeline's own config file, not in the
label, since labels are world-readable through `docker inspect`:

```yaml
# bilgeline.yml
destinations:
  loki:
    type: otlphttp
    endpoint: https://loki.home.lan/otlp
    headers:
      Authorization: "Bearer ${env:LOKI_BEARER}"
```

bilgeline emits the `${env:LOKI_BEARER}` reference into the generated config
verbatim and never expands it. The collector provisions the matching environment
variable and expands it at config load, so no secret value ever reaches disk or a
bilgeline log line. The full grammar (every label, its type, and its defaults) is
in [docs/LABELS.md](docs/LABELS.md), and a fully commented config is in
[bilgeline.example.yml](bilgeline.example.yml).

## Commands

bilgeline is a single binary with five commands. `--config` defaults to
`/etc/bilgeline/bilgeline.yml` (the file is optional, and env-only operation
works),
and `--log-level` is one of `debug`, `info`, `warn`, `error`.

- `bilgeline daemon` runs the long-running, event-driven control loop. It
  watches the container socket, and on each debounced lifecycle change it
  regenerates the config and signals the collector. This is the container's
  default command.
- `bilgeline generate` (alias `render`) is a dry run. It discovers the current
  containers, renders the collector config, and prints it to stdout. It writes
  nothing and signals nothing.
- `bilgeline validate` loads and checks bilgeline.yml and, unless
  `--config-only`, discovers containers and reports every label diagnostic. It
  exits nonzero on any config or error-severity finding, so it slots into CI.
- `bilgeline status` prints the discovered services, the resolved collector, and
  the collector's health. It is read-only.
- `bilgeline version` prints the build version.

Day-to-day operation and troubleshooting are in
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Status

Beta, `00.01.00b1`. The core route is proven end to end against a live Docker
socket and a real collector: discovery, config generation, the atomic write, the
collector find, the SIGHUP reload, the filelog tail, and export to a `file`
destination all work together, along with the live event-driven reload, the
mount guard, collector-ambiguity refusal, non-json-file driver exclusion, and the
`${env:VAR}` preflight. The richer processing grammar (multiline, json/logfmt/
regex parse, severity extraction, drops, fan-out) is unit-proven at the render
layer against golden YAML, and real network destinations (Loki, OTLP,
Elasticsearch) are proven only at the config-generation layer. The full,
honest coverage map is in [docs/TESTING.md](docs/TESTING.md).

## Design

The architecture and the frozen label grammar are documented in the wiki:

- tagwright/Bilgeline Architecture
- tagwright/Bilgeline Label Grammar (Draft)

The in-repo [docs/LABELS.md](docs/LABELS.md) is the authoritative label reference
and tracks the code where the wiki draft drifts.

## License

GPL-3.0-or-later. You can run it, charge for it, and modify it. If you distribute
a modified version, it stays open under the same license. Each source file
carries an `SPDX-License-Identifier: GPL-3.0-or-later` header. See
[LICENSE](LICENSE).
