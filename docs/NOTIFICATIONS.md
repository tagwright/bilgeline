# Notifications and telemetry

bilgeline can deliver its discovery and apply problems to real alert channels
(ntfy, Telegram, Discord, SMTP, and more) and push reconcile health to a Gatus
external endpoint. Both are configured in `bilgeline.yml` and both run inside
bilgeline's own process through the shared beacon notification library.

This is the operations companion for the alert output: [OPERATIONS.md](OPERATIONS.md)
covers what triggers an alert and at what level, this document covers how to
configure the channels and where their credentials live.

## The two secret domains, kept separate

bilgeline has TWO distinct secret domains. They never mix, and understanding the
split is the whole point of this section.

### Domain 1: exporter / destination secrets (on the collector, unchanged)

A Loki bearer token, an S3 access key, any credential an exporter needs. These
belong to the COLLECTOR's process, not bilgeline's. Under the S1 secret model a
destination in `bilgeline.yml` carries a `${env:VAR}` reference (for example an
`Authorization` header of `Bearer ${env:LOKI_BEARER}`), bilgeline copies that
string verbatim into the generated collector config and NEVER expands it, and
the collector expands it at config load from an environment variable set on the
collector container. bilgeline resolves no destination secret, ever. This is the
existing model and it is unchanged. See [DEPLOY.md](DEPLOY.md) for the full S1
recipe.

### Domain 2: notification / telemetry secrets (bilgeline's own, new)

An ntfy token, a Telegram bot token, an SMTP password, a Gatus push token. These
belong to bilgeline's OWN process, because the beacon notifier runs inside
bilgeline. The `notifications` and `telemetry` settings name these secrets, and
bilgeline resolves them at send time from its OWN secrets directory.

The rule: **the bilgeline secrets directory is for alerting credentials only.**
Never put an exporter/destination credential there, and never put an alerting
credential in the collector's env. A Loki token must never be readable from
bilgeline's process, and an ntfy token must never leak into a collector config
file. The two code paths are deliberately separate so this stays true.

## Where domain-2 secrets live

Every credential value in the `notifications` and `telemetry` sections is a
secret NAME, never a literal token. bilgeline resolves a name in this order:

1. A file at `<secrets_dir>/<name>`.
2. The environment variable `BILGELINE_SECRET_<NAME>`, where `<NAME>` is the
   name uppercased with `-` replaced by `_`.
3. Neither found: the send fails loudly and the failure is logged, rather than a
   channel sending with an empty credential.

Whichever source supplies the value, a leading or trailing whitespace run
(spaces, tabs, CR, LF) is trimmed, so a token pasted into an env var with a
stray newline behaves exactly like one read from a file.

The secrets directory is set by `secrets_dir` in `bilgeline.yml`, overridden by
`BILGELINE_SECRETS_DIR`, and defaults to `/run/bilgeline/secrets`. Mount it as a
tmpfs and drop one file per secret into it at deploy time (for example via SOPS,
the same way domain-1 secrets are decrypted into `collector.env`).

A secret named `ntfy-bilgeline-token` is therefore read from
`/run/bilgeline/secrets/ntfy-bilgeline-token`, or from
`BILGELINE_SECRET_NTFY_BILGELINE_TOKEN` when no file exists.

## Configuring notification channels

Each entry in `notifications` is one channel: a backend `type`, an optional
`min_level` (`info`, `warn`, or `error`, where empty means receive everything),
and a
backend-specific `settings` map. Config validation rejects an unknown `type` or
`min_level` at load, before the daemon starts.

```yaml
notifications:
  - type: ntfy
    min_level: warn
    settings:
      server: https://ntfy.home.lan
      topic: bilgeline-alerts
      # A secret NAME, resolved from secrets_dir, not the literal token.
      token_secret: ntfy-bilgeline-token
```

The always-on `log` floor channel is wired in addition to whatever you list, so
a diagnostic is never lost even with this section empty. A channel's `min_level`
filters what reaches it (see OPERATIONS.md for the trigger-to-level mapping).

Supported channel types: `log`, `smtp`, `ntfy`, `gotify`, `telegram`,
`discord`, `slack`, `mattermost`, `pushover`, `webhook`, `matrix`. Each
backend's `settings` keys are defined by beacon, the shared notification
library. Any settings value that is a credential is a secret name, resolved from
the bilgeline secrets directory.

## Configuring the Gatus telemetry sink

Each entry in `telemetry` is one health/status push sink. v1 supports the
`gatus` external-endpoint push.

```yaml
telemetry:
  - type: gatus
    settings:
      url: https://status.home.lan
      # The "group_endpoint-name" key you configured for the external endpoint.
      endpoint_key: infra_bilgeline
      # A secret NAME, resolved from secrets_dir.
      token_secret: gatus-bilgeline-push-token
```

bilgeline pushes a health result on every reconcile pass: `success=true` on a
clean pass, `success=false` with the reason on a pass that failed or skipped a
container on an error diagnostic. The push is event-driven, one per reconcile,
not on a background clock.

One consequence worth noting: Gatus treats an external endpoint as unhealthy if
it does not receive a push within its configured interval (a dead-man's switch).
Because bilgeline is event-driven and does not reconcile on a clock, a healthy
but quiet fleet may not push for a long time. Set the Gatus endpoint's interval
generously, with bilgeline's event-driven nature in mind, or accept that a long
quiet period reads as degraded. bilgeline deliberately does not run a heavyweight
periodic scheduler just to emit keepalives.

## Verifying delivery

The live integration test proves the whole path end to end against a real ntfy
server: it builds the notifier exactly as the daemon does, resolves a named
token from a secrets directory, sends through beacon to ntfy, and asserts the
message arrived. See [TESTING.md](TESTING.md).
