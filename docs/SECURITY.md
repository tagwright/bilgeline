<!-- SPDX-License-Identifier: GPL-3.0-or-later -->
# Security

bilgeline routes logs, and logs are where secrets leak. A bearer token in a
header, a password echoed by a crashing process, an API key printed at debug
level: any of these can pass through a log pipeline. So the security question for
bilgeline is what it touches and what it does not, and the honest answer has a
sharp edge. bilgeline is careful with the credentials it is given to name, and it
does nothing to the content of the log lines themselves. This document draws that
line clearly. The claims here are checkable against the code, against
[NOTIFICATIONS.md](NOTIFICATIONS.md), and against the coverage in
[TESTING.md](TESTING.md).

## bilgeline never reads a log byte

bilgeline is a generator, not a shipper. It reads `bilgeline.*` labels off your
running containers and writes an OpenTelemetry Collector configuration to a shared
volume, then signals the collector to reload. The collector does the tailing and
the shipping. bilgeline never opens a log file, never sees a log record, and holds
no exporter credentials. That division is the first security fact about it: the
process that discovers and generates is not the process that touches your log data
or the tokens that ship it.

## Two secret domains, kept apart

bilgeline deals with two kinds of secret, and the whole point of the design is
that they never mix.

**Destination secrets belong to the collector, not to bilgeline.** A Loki bearer
token or an S3 access key is a credential the exporter needs. In bilgeline's
config a destination carries a reference like `Bearer ${env:LOKI_BEARER}`.
bilgeline copies that string verbatim into the generated collector config and
never expands it. The collector provisions the matching environment variable and
expands it at config load. No destination secret value ever reaches bilgeline's
process, its logs, or the generated config on disk. The live env-preflight test
confirms both halves: a present secret's value appears in neither the generated
config nor any bilgeline log line, and a missing reference produces a warning that
names the variable rather than leaking a value (`13_env_preflight.sh` in
[TESTING.md](TESTING.md)). bilgeline resolves no destination secret, ever.

**Notification secrets are bilgeline's own.** bilgeline can deliver its own
discovery and apply problems to alert channels, and those channels have their own
credentials (an ntfy token, an SMTP password, a Gatus push token). Because that
notifier runs inside bilgeline's process, bilgeline does resolve these, at send
time, from its own secrets directory. Every credential in the `notifications` and
`telemetry` config is a secret name, never a literal token. A name resolves from a
file at `<secrets_dir>/<name>` or from `BILGELINE_SECRET_<NAME>`, and if neither
exists the send fails loudly rather than going out with an empty credential. The
rule the two code paths exist to keep true: a destination credential must never be
readable from bilgeline's process, and a notification credential must never land
in a collector config file.

## Labels are world-readable, so credentials do not live in them

A `bilgeline.destination` label names a destination. It never carries the
endpoint, the header, or the token. Anything that can run `docker inspect` can
read a container's labels, so a secret in a label is a secret handed to every
process that can reach the socket. The endpoint and its credentials are defined
once in bilgeline's own config file instead, and the label only points at that
definition by name.

## What bilgeline does not protect

This is the sharp edge, stated plainly so nobody deploys expecting a guard that
is not there.

- **bilgeline does not inspect, redact, or scrub log content.** If a container
  writes a secret to stdout, that line is log data, and bilgeline routes the
  service that produced it without ever looking at the bytes. Keeping secrets out
  of application logs is the application's job and the destination's, not
  bilgeline's. The filter grammar (`drop`, `level.min`, stream selection) is a
  routing tool for choosing which records go where, not a redaction layer, and it
  should not be relied on as one.
- **bilgeline drives a collector it does not itself secure.** You deploy your own
  `otelcol-contrib`. Its transport security to your destinations, the auth on
  those destinations, and the protection of the shared config volume are the
  operator's responsibility. bilgeline writes a config into that volume and
  reloads the collector. It does not harden the collector, and it cannot.
- **bilgeline authenticates nothing on the socket.** It reads the container socket
  to discover services, the same trust boundary every socket-watching tool in this
  family accepts. Anything that can reach the socket is already inside that
  boundary.

## A privilege note the deployment depends on

The shipped collector runs as root (`user: "0:0"`). This is not incidental.
Docker's json-file logs are written `root:root` mode 0640, and a non-root
collector silently tails nothing, which is worse than failing loudly because the
pipeline looks healthy while shipping no logs. bilgeline itself runs as distroless
nonroot (uid 65532) and needs only the socket group and a config directory it
owns. The root collector is a real privilege the operator is granting to read the
host's container logs, and it is called out here so it is a decision, not a
surprise. Both ownership facts are mirrored between the test harness and the
shipped compose (see [TESTING.md](TESTING.md)).

## Testing honesty

The core route is proven end to end against a live Docker socket and a real
collector, including the `${env:VAR}` preflight that keeps a secret value out of
the generated config and the logs. Real network destinations (Loki, OTLP,
Elasticsearch) are proven only at the config-generation layer, because verifying
delivery to them needs your endpoints and credentials. The `file` exporter is the
one whose bytes the harness can verify locally, and the content-level claims above
rest on that. The Podman path is unit-covered but not run against a real Podman
host. [TESTING.md](TESTING.md) reports all of this path by path.

## Reporting a vulnerability

Report a suspected vulnerability through GitHub's private vulnerability reporting
on this repository: the **Security** tab, then **Report a vulnerability**. That
keeps the report private to the maintainer while it is triaged.

Please give it a chance to be fixed before disclosing it publicly. Coordinated
disclosure, where the fix ships before the details are public, is the outcome we
are asking for, and the one that protects everyone running the tool.
