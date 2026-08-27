#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Failure path 4 - env preflight and the S1 secrets model. A destination that
# references ${env:VAR}s the collector does not carry must produce a bilgeline
# warning that NAMES the missing var, so the operator knows what to provision.
# And, load-bearing for S1: bilgeline copies ${env:...} references verbatim and
# NEVER expands or logs a secret VALUE. A present secret's value must appear
# nowhere in the generated config nor in any bilgeline log line.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

SECRET_VALUE="s3kr1t-$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-8)-VALUE"

rc=0
trap teardown EXIT
teardown

log "env preflight: missing var named, present secret value never leaked"

net_create
vol_create config; vol_create blconf
seed_config config

# Destination references TWO env vars: one the collector carries (SECRET, a
# stand-in credential) and one it does not (MISSING). bilgeline must warn about
# MISSING by name and must copy both references verbatim without expanding SECRET.
cat <<'YML' | docker run --rm -i -v "bilgeline-itest-blconf:/etc/bilgeline" busybox:1.37 sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: itesthttp
shared_config_path: /config/otelcol.yaml
debounce: 1s
destinations:
  itesthttp:
    type: otlphttp
    endpoint: https://collector.invalid/otlp
    headers:
      Authorization: "Bearer ${env:BILGELINE_ITEST_SECRET}"
      X-Extra: "${env:BILGELINE_ITEST_MISSING}"
YML

# Collector carries SECRET but NOT MISSING. It is marked and mounts the shared
# config so the mount guard passes and the env preflight actually runs.
docker run -d --name "${PREFIX}-collector" --network "$NET" --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -e "BILGELINE_ITEST_SECRET=${SECRET_VALUE}" \
  -v "${PREFIX}-config:/config:ro" \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  "$COLLECTOR_IMAGE" --config=/config/otelcol.yaml >/dev/null

docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true --label bilgeline.destination=itesthttp \
  "$ALPINE_IMAGE" sh -c 'while true; do echo env-preflight-probe; sleep 0.3; done' >/dev/null

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null

# 1. bilgeline warns, naming the missing var.
if wait_for_log bilgeline "missing referenced env var BILGELINE_ITEST_MISSING" 25; then
  pass "env preflight: bilgeline named the missing env var"
  docker logs "${PREFIX}-bilgeline" 2>&1 | grep -m1 "missing referenced env var" | sed 's/^/    /'
else
  fail "env preflight: missing-var warning not surfaced in bilgeline logs"
  docker logs "${PREFIX}-bilgeline" 2>&1 | tail -20; rc=1
fi

# 2. The generated config carries the ${env:...} references VERBATIM.
cfg="$(gen_config config)"
if printf '%s' "$cfg" | grep -q '${env:BILGELINE_ITEST_SECRET}' && \
   printf '%s' "$cfg" | grep -q '${env:BILGELINE_ITEST_MISSING}'; then
  pass "env preflight: both \${env:...} references copied verbatim into the config"
else
  fail "env preflight: \${env:...} references not found verbatim in the config"; rc=1
fi

# 3. The secret VALUE must never appear in the generated config.
if printf '%s' "$cfg" | grep -q "$SECRET_VALUE"; then
  fail "env preflight: SECRET VALUE leaked into the generated config"; rc=1
else
  pass "env preflight: secret value absent from the generated config (S1 verbatim, never expanded)"
fi

# 4. The secret VALUE must never appear in ANY bilgeline log line.
if docker logs "${PREFIX}-bilgeline" 2>&1 | grep -q "$SECRET_VALUE"; then
  fail "env preflight: SECRET VALUE leaked into bilgeline logs"; rc=1
else
  pass "env preflight: secret value never printed in any bilgeline log line"
fi

if [ "$rc" -eq 0 ]; then pass "13_env_preflight: ALL PASS"; else fail "13_env_preflight: FAILURES"; fi
exit $rc
