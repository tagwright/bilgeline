# Label reference

This is the authoritative reference for the v1 bilgeline label grammar, derived
from the code in `internal/discovery` and `internal/config`. Where this document
and the design draft ([[Bilgeline Label Grammar (Draft)]] in the wiki) ever
disagree, the code and this file win.

bilgeline reads labels off running containers and turns them into an
OpenTelemetry Collector configuration. A container opts in with `bilgeline.enable`
and describes where and how its logs should go with the rest of the keys below.

## Two prefixes, one grammar

Every key exists under two recognized prefixes with the identical suffix after
the prefix.

- `bilgeline.` is the primary, tool-branded prefix. All examples lead with it.
- `tagwright.log.` is the org-namespaced alias. `bilgeline.enable` and
  `tagwright.log.enable` mean the same thing.

The reader strips whichever prefix a key carries and parses one canonical suffix
(`enable`, `destination`, `drop.0`, `attr.tier`, and so on). Any key under
neither prefix is ignored.

### The conflict rule

The same suffix set under both prefixes with the same value collapses
harmlessly. The same suffix under both prefixes with different values is a
validation error. bilgeline skips that container, emits an error diagnostic, and
alerts. There is no silent precedence between the two prefixes. Keys are walked
in sorted order so the error names the two conflicting keys deterministically.

Two markers are read straight off the raw labels before the conflict check runs,
so an exclusion can never be masked by an unrelated conflict elsewhere on the
container. Those are `enable` (the opt-in gate that decides whether a container
is even inspected) and `collector` (the collector marker, described at the end).

## Strict opt-in

`bilgeline.enable=true` is required. Nothing is routed without it. In v1
`bilgeline.enable=false` is identical to the label being absent. The `false`
value is reserved for a future fleet opt-out mode that is not built in v1.

Boolean label values are parsed with Go's `strconv.ParseBool`, so `true`,
`false`, `1`, `0`, `t`, and `f` are all accepted spellings. A value that is not a
valid boolean is an error diagnostic and the container is skipped. All label
values are strings, quoted in YAML.

## Merge precedence

When a container also references a profile (`bilgeline.profile`), fields merge
with one rule: label over profile over global. An explicit label overrides the
profile's corresponding scalar field. The list-valued `drop` is the one
exception: a container's label drops union with the profile's drops rather than
replacing them. Promoted labels union the same way. A `bilgeline.attr.<key>`
label wins over a profile attribute of the same key.

## Core

| Label | Type | Default | Required | Semantics |
|---|---|---|---|---|
| `bilgeline.enable` | bool | absent (false) | yes | Opt the container's logs into routing. Absent or `false` means ignored. |
| `bilgeline.name` | string | compose service, else container name | no | Stable service identity. Becomes `service.name` on every record. An empty value falls through to the default chain. |
| `bilgeline.destination` | csv of names | `default_destination` | no | One or more named destinations from bilgeline.yml. A comma list fans out. |
| `bilgeline.profile` | string | none | no | A named processing profile from bilgeline.yml. Explicit labels override the profile's scalar fields. |

Service name precedence is `bilgeline.name`, then the compose service label
(`com.docker.compose.service`), then the container name with any leading `/`
stripped. Two containers resolving to the same name is legal (replicas). They
stay distinct through their `container.id` and `container.name` attributes.

Destination resolution:

- With no destination named, bilgeline falls back to `default_destination` in
  the config. With neither a named destination nor a configured default, the
  container is an error diagnostic and is skipped.
- Every named destination must be defined in the config's `destinations` map,
  with one exception: the reserved `debug` name is always addressable even when
  the config does not define it. An unknown destination name is an error.
- The reserved `none` sentinel anywhere in the list routes the container
  nowhere. This is a valid, enabled-but-dropped state, distinct from a disabled
  container. If `none` appears alongside real destination names, `none` wins and
  the container routes nowhere.
- An unknown profile name is an error diagnostic and the container is skipped.

## Parsing

| Label | Type | Default | Required | Semantics |
|---|---|---|---|---|
| `bilgeline.parse` | enum | `none` | no | `json`, `logfmt`, or `none` (raw body passthrough). Any value outside the enum is an error. |
| `bilgeline.multiline` | regex | none | no | The pattern that starts a new log entry. Continuation lines recombine into the previous entry. An invalid regex is an error. |
| `bilgeline.level.field` | string | probe `level`, `severity`, `lvl` | no | The parsed field holding the log level, mapped to OTel severity through the collector's built-in aliases. Meaningful only when the body is parsed. |

Anything richer than the `parse` enum (a named-group regex, a timestamp layout,
a custom severity mapping) lives in a named profile in bilgeline.yml, not in a
label. See the profile examples below.

`bilgeline.multiline` is realized as a stanza `recombine` operator after the
Docker JSON envelope is parsed. In a compose file, `$` is eaten by interpolation,
so a `$` anchor in the pattern needs `$$`. An invalid regex is compile-checked at
resolution time and skips the container with an error.

When `parse` is `json` or `logfmt` and no `level.field` is named, bilgeline
probes the conventional field names `level`, `severity`, `lvl` in that order, so
the common structured-log case needs no extra label. `level.field` set while
`parse=none` extracts nothing, because no severity parser is emitted for an
unparsed body.

## Filtering

| Label | Type | Default | Required | Semantics |
|---|---|---|---|---|
| `bilgeline.stream` | enum | `both` | no | `stdout`, `stderr`, or `both`. Any other value is an error. |
| `bilgeline.drop` | csv of regexes | none | no | Drop records whose body matches any pattern. Unions with `drop.<n>` and a referenced profile's drops. |
| `bilgeline.drop.<n>` | regex | none | no | Indexed form (`drop.0`, `drop.1`, ...) for patterns that contain commas. Collected in ascending index order. The documented norm for multiple drops. |
| `bilgeline.level.min` | enum | none | no | Severity floor, one of `trace`, `debug`, `info`, `warn`, `error`, `fatal`. Records below the floor are dropped. |

Every drop pattern, from the csv form, the indexed form, and any profile, is
compile-validated and de-duplicated. An invalid regex is an error diagnostic and
the container is skipped.

`level.min` needs a level to compare against. When it is set but `parse=none`
(so nothing extracts a severity), the container still routes, but bilgeline emits
a warning diagnostic that the floor will not apply.

## Attributes

| Label | Type | Default | Required | Semantics |
|---|---|---|---|---|
| `bilgeline.attr.<key>` | string | none | no | Stamp a static resource attribute with the key used verbatim (no forced prefix). `bilgeline.attr.tier=backend` yields the attribute `tier=backend`. |
| `bilgeline.labels` | csv of label keys | fleet-wide `labels` set | no | Promote the named container labels to resource attributes under their verbatim key. Unions with the fleet-wide set from the config and `BILGELINE_LABELS`. |

The `labels` promotion has its own sentinel. `none` anywhere in the container's
list suppresses the fleet-wide default set for that one container, while any
other keys named in the same list still promote. A promoted key the container
does not actually carry is a no-op. On a key collision an explicit
`bilgeline.attr.<key>` wins over a promoted label of the same key.

### Auto-stamped attributes

bilgeline already inspects every routed container, so it stamps this set with no
extra labels. These are resource attributes set by the generated transform
processor.

- `service.name`, always.
- `service.namespace`, when the container is a compose service (from the compose
  project).
- `container.id`, the full 64-hex id, always.
- `container.name`, when known.
- `container.image.name`, when the image is known.
- `bilgeline.routing_key`, the container's canonical sorted destination set,
  used internally by the routing connector. Present only when the container
  routes somewhere.

Two more attributes come from the collector's own operators rather than the
transform, and appear as log-record attributes. `log.iostream` (`stdout` or
`stderr`) is stamped by the container operator, and `container_id` (recovered
from the tailed file path, distinct from the resource attribute `container.id`)
plus `log.file.path` come from the filelog receiver.

Note for wiki reconciliation: the grammar draft lists `host.name` in the
auto-stamped set. The v1 renderer does not stamp `host.name`. If you want it,
add it through the collector's own configuration, not through bilgeline.

## The collector marker

This one key does not live on a log source. It lives on the OpenTelemetry
Collector container that bilgeline drives.

| Label | Type | Default | Required | Semantics |
|---|---|---|---|---|
| `bilgeline.collector` | bool | absent | no | Marks the collector container bilgeline signals. The `collector:` name in bilgeline.yml is the fallback when no container carries the marker. |

The marker is the primary way bilgeline finds its collector. The config
`collector:` name is a backstop for when you would rather name it. Two failure
cases are hard identity errors that block signalling (the config is still
written, but no SIGHUP is sent):

- More than one container carries the marker. bilgeline cannot know which to
  signal.
- The marker is on one container and the config `collector:` names a different
  existing container. A configured name that matches the marked container, or
  that names nothing present, is not a conflict.

The marker is read under both prefixes, so `tagwright.log.collector=true` marks
the collector just as `bilgeline.collector=true` does. bilgeline always excludes
its own container and the marked collector from routing, so a log pipeline never
ships its own error logs into itself.

## Reserved words

- Destination names `debug` and `none` are reserved. `debug` maps to the
  collector's debug exporter (records land on the collector's own stdout, seen
  through `docker logs <collector>`), and is always addressable even when the
  config does not define it. `none` routes nowhere.
- Profile names may not shadow a reserved parser name. The reserved set is
  `json`, `logfmt`, `none`, `auto`, and `debug`. `auto` is held for a possible
  future sniffing mode and is not a v1 parse value.

## Global defaults

The surviving fleet-wide globals are environment variables on the bilgeline
container. The set is deliberately small.

- `BILGELINE_DEFAULT_DESTINATION` overrides the config `default_destination`.
- `BILGELINE_LABELS` unions more container-label keys onto the fleet-wide
  promotion set (comma-separated).
- `BILGELINE_DEBOUNCE` sets the socket-event coalescing window (a Go duration,
  default `2s`).

The per-container filtering keys (`parse`, `stream`, `level.min`, `drop`) have
no global form, by design. A fleet-wide filtering default silently eats lines
someone may have wanted, so those knobs stay per-container. There is no secrets
directory or per-secret environment variable either, because bilgeline holds no
destination credentials (see the S1 secret model in [DEPLOY.md](DEPLOY.md)).

For the deployment and runtime environment variables (`BILGELINE_RUNTIME`,
`BILGELINE_SOCKET`, `BILGELINE_SELF_ID`, and the collector-tuning knobs), see
[OPERATIONS.md](OPERATIONS.md).

## Worked examples

Each example below parses under the v1 grammar. The destinations they name must
be defined in bilgeline.yml (see [bilgeline.example.yml](../bilgeline.example.yml)).

### Minimal: route one service to Loki

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    labels:
      bilgeline.enable: "true"
      bilgeline.destination: "loki"
```

Two labels, or one if `loki` is the `default_destination`. The logs land at the
`loki` destination stamped `service.name=silverbullet`, `service.namespace` (the
compose project), `container.name`, `container.id`, `container.image.name`, and
`log.iostream`. The same routing through the org doorway uses
`tagwright.log.enable` and `tagwright.log.destination`.

### Richer: JSON logs, two destinations, noise dropped, custom attribute

```yaml
services:
  api:
    image: example/api
    labels:
      bilgeline.enable: "true"
      bilgeline.destination: "loki,archive"
      bilgeline.parse: "json"
      bilgeline.level.field: "level"
      bilgeline.level.min: "info"
      bilgeline.drop.0: "GET /healthz"
      bilgeline.drop.1: "GET /metrics"
      bilgeline.attr.tier: "backend"
```

The JSON body is parsed to attributes, severity is read from the `level` field,
records below `info` and the two health-check patterns are dropped, the routed
records carry `tier=backend`, and the service fans out to both `loki` and
`archive`.

### A legacy app through a profile

```yaml
services:
  legacy:
    image: example/springboot
    labels:
      bilgeline.enable: "true"
      bilgeline.profile: "springboot"
      bilgeline.destination: "loki"
      bilgeline.drop.0: "GET /healthz"
```

with the profile defined once in bilgeline.yml:

```yaml
profiles:
  springboot:
    multiline: '^\d{4}-\d{2}-\d{2}'
    parse:
      type: regex
      pattern: '^(?P<ts>\S+ \S+)\s+(?P<level>\w+)\s+(?P<msg>.*)$'
      timestamp:
        field: ts
        layout: '%Y-%m-%d %H:%M:%S'
      level:
        field: level
    drop:
      - 'Tomcat started on port'
```

The container ships to `loki` with its timestamped multiline traces recombined,
its date-level-message lines parsed, and both the profile's `Tomcat started on
port` drop and the container's own `drop.0` applied. Because `drop` is the
list-union exception, the container drop adds to the profile drop rather than
replacing it.
