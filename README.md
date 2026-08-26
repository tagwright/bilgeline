# bilgeline

Label-driven log routing for Docker and Podman services. bilgeline reads
`bilgeline.*` labels off your running containers and generates an
OpenTelemetry Collector configuration that routes each service's logs to the
destination you named. You describe where logs should go in the compose
file, next to the service they belong to, and bilgeline keeps the collector
config in sync as containers come and go.

bilgeline does not ship or run a collector. You deploy your own
otelcol-contrib container. bilgeline runs as a separate single-binary
container that watches the container socket, writes collector YAML to a
shared config volume, and signals the collector to reload. The collector
does the log tailing and shipping. bilgeline owns discovery and config
generation only, so it holds no exporter credentials and never touches your
log bytes.

Status: early, under construction, not yet functional. This repository is a
scaffold. The discovery, config generation, and reload logic are not built
yet.

## The idea

A service opts in with labels next to the service they describe:

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    labels:
      bilgeline.enable: "true"
      bilgeline.destination: "loki"
```

The label names a destination. The destination itself (its endpoint,
authentication, and any credentials) is defined once in bilgeline's own
config file, not in the label, since labels are world-readable through
`docker inspect`. bilgeline emits `${env:VAR}` references into the generated
config and never writes a secret value to disk. The collector deployment
provisions those environment variables.

## Design

The architecture and the label grammar are documented in the wiki:

- tagwright/Bilgeline Architecture
- tagwright/Bilgeline Label Grammar (Draft)

## License

GPL-3.0-or-later. You can run it, charge for it, and modify it. If you
distribute a modified version, it stays open under the same license. Each
source file carries an `SPDX-License-Identifier: GPL-3.0-or-later` header.
See [LICENSE](LICENSE).
