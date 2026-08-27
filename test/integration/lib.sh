# SPDX-License-Identifier: GPL-3.0-or-later
#
# Shared helpers for the bilgeline live integration harness. Every Docker object
# this suite creates is prefixed "bilgeline-itest-" and the teardown only ever
# touches objects carrying that prefix. Sourced by the case scripts and run.sh.
#
# This harness needs a live Docker socket at /var/run/docker.sock and the right
# to create sibling containers, volumes, and a network. It never touches, stops,
# or removes anything that is not bilgeline-itest-* prefixed.

set -u

PREFIX="bilgeline-itest"
IMAGE="${BILGELINE_ITEST_IMAGE:-bilgeline-itest:local}"
COLLECTOR_IMAGE="otel/opentelemetry-collector-contrib:0.159.0"
# The collector runs as root: Docker's json-file logs are root:root 0640 and
# unreadable by the image's default uid 10001, so the receiver would silently
# tail nothing. This mirrors the fixed docker-compose.yml (user: "0:0").
COLLECTOR_USER="0:0"
BUSYBOX_IMAGE="busybox:1.37"
ALPINE_IMAGE="alpine:3.20"

NET="${PREFIX}-net"

# The gid that owns the Docker socket. bilgeline runs as distroless nonroot
# (uid 65532), so it must be added to the socket's group to reach the API. This
# is the minimal, legitimate grant any real deployment arranges (compose
# group_add); it grants socket access only and masks no bilgeline logic.
DOCKER_GID="$(stat -c %g /var/run/docker.sock 2>/dev/null || echo 0)"

# Terminal colors, disabled when not a tty. No emojis anywhere, per house style.
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_BLU=$'\033[34m'; C_RST=$'\033[0m'
else
  C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_RST=""
fi

log()  { printf '%s[itest]%s %s\n' "$C_BLU" "$C_RST" "$*"; }
pass() { printf '%s[PASS]%s %s\n' "$C_GRN" "$C_RST" "$*"; }
warn() { printf '%s[warn]%s %s\n' "$C_YEL" "$C_RST" "$*"; }
fail() { printf '%s[FAIL]%s %s\n' "$C_RED" "$C_RST" "$*"; return 1; }

# The inert-but-valid collector config, byte-compatible with what the compose
# config-seed writes and what bilgeline emits when nothing is routed. Seeded so
# the collector has a valid --config to load before bilgeline writes the real one.
INERT_CONFIG='exporters:
    nop: {}
extensions:
    health_check:
        endpoint: 0.0.0.0:13133
receivers:
    nop: {}
service:
    extensions:
        - health_check
    pipelines:
        logs/inert:
            exporters:
                - nop
            receivers:
                - nop'

# teardown removes EVERY bilgeline-itest-* object. Anchored name filters mean it
# can only ever match objects this harness created. Safe to call repeatedly and
# from a trap, so a mid-test failure still cleans up.
teardown() {
  local ids
  ids="$(docker ps -aq --filter "name=^/${PREFIX}-" 2>/dev/null || true)"
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1 || true
  local vols
  vols="$(docker volume ls -q --filter "name=^${PREFIX}-" 2>/dev/null || true)"
  [ -n "$vols" ] && docker volume rm -f $vols >/dev/null 2>&1 || true
  local nets
  nets="$(docker network ls -q --filter "name=^${PREFIX}-" 2>/dev/null || true)"
  [ -n "$nets" ] && docker network rm $nets >/dev/null 2>&1 || true
  return 0
}

# net_create makes the shared network idempotently.
net_create() {
  docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null
}

# vol_create makes a named volume ($1 is the suffix after the prefix).
vol_create() { docker volume create "${PREFIX}-$1" >/dev/null; }

# chown_vol hands a volume's root to a uid:gid. Used to make collector-side
# volumes (checkpoints, the file-exporter output) writable by the collector's
# uid (10001), the same volume-ownership setup an operator arranges on the
# collector deployment side of the two-container split.
chown_vol() {
  docker run --rm -v "${PREFIX}-$1:/v" "$BUSYBOX_IMAGE" chown -R "$2" /v >/dev/null
}

# seed_config writes the inert config into a config volume ($1 = volume suffix)
# at /config/otelcol.yaml, mirroring the compose config-seed one-shot.
# Mirrors the fixed compose config-seed: writes the inert config AND hands the
# shared config dir to bilgeline's nonroot uid (65532) so the atomic write can
# create its temp file there.
seed_config() {
  printf '%s\n' "$INERT_CONFIG" | \
    docker run --rm -i -v "${PREFIX}-$1:/config" "$BUSYBOX_IMAGE" \
      sh -c 'cat > /config/otelcol.yaml && chown -R 65532:65532 /config' >/dev/null
}

# read_out cats a file from a volume. $1 = volume suffix, $2 = path inside.
read_vol_file() {
  docker run --rm -v "${PREFIX}-$1:/v:ro" "$BUSYBOX_IMAGE" cat "/v/$2" 2>/dev/null
}

# gen_config prints the generated collector config from the shared config volume.
gen_config() { read_vol_file "$1" "otelcol.yaml"; }

# wait_for_token retries until the token ($2) appears in the out volume file, or
# times out. $1 = out volume suffix, $2 = token, $3 = timeout seconds.
wait_for_token() {
  local vol="$1" token="$2" timeout="${3:-40}" i=0
  while [ "$i" -lt "$timeout" ]; do
    if read_vol_file "$vol" "logs.json" | grep -q "$token"; then
      return 0
    fi
    i=$((i+1)); sleep 1
  done
  return 1
}

# wait_for_log retries until the pattern ($2, a grep -E regex) appears in a
# container's logs. $1 = container name suffix, $2 = pattern, $3 = timeout.
wait_for_log() {
  local name="${PREFIX}-$1" pat="$2" timeout="${3:-20}" i=0
  while [ "$i" -lt "$timeout" ]; do
    if docker logs "$name" 2>&1 | grep -Eq "$pat"; then
      return 0
    fi
    i=$((i+1)); sleep 1
  done
  return 1
}

# require_image fails fast if a prerequisite image is missing.
require_images() {
  local missing=0
  for img in "$IMAGE" "$COLLECTOR_IMAGE" "$BUSYBOX_IMAGE" "$ALPINE_IMAGE"; do
    if ! docker image inspect "$img" >/dev/null 2>&1; then
      warn "missing image: $img"
      missing=1
    fi
  done
  return $missing
}
