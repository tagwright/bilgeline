#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Failure path 1 - mount guard. A container marked bilgeline.collector=true that
# does NOT mount the shared config path must never be signalled: a mislabeled
# marker must not let bilgeline SIGHUP an innocent container. bilgeline must
# still write the config, refuse to signal, emit a diagnostic, and not crash-loop.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

rc=0
trap teardown EXIT
teardown

log "mount guard: collector is marked but does not mount /config"

net_create
vol_create config
vol_create blconf
vol_create othercfg
seed_config config

# A standalone valid config for the collector on a NON-shared path, so it runs
# but never mounts /config.
printf '%s\n' "$INERT_CONFIG" | \
  docker run --rm -i -v "${PREFIX}-othercfg:/etc/otelcfg" "$BUSYBOX_IMAGE" \
    sh -c 'cat > /etc/otelcfg/otelcol.yaml' >/dev/null

cat <<YML | docker run --rm -i -v "${PREFIX}-blconf:/etc/bilgeline" "$BUSYBOX_IMAGE" sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: debug
shared_config_path: /config/otelcol.yaml
debounce: 1s
YML

# Marked collector that mounts othercfg, NOT the shared /config.
docker run -d --name "${PREFIX}-collector" --network "$NET" \
  --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -v "${PREFIX}-othercfg:/etc/otelcfg:ro" \
  "$COLLECTOR_IMAGE" --config=/etc/otelcfg/otelcol.yaml >/dev/null

# A producer so bilgeline has something to route and runs a full apply.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true --label bilgeline.destination=debug \
  "$ALPINE_IMAGE" sh -c 'while true; do echo mount-guard-probe; sleep 0.3; done' >/dev/null

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null

# Assert bilgeline refuses to signal, naming the mount reason.
if wait_for_log bilgeline "does not mount the shared config path" 25; then
  pass "mount guard: bilgeline refused to signal the non-mounting collector"
  docker logs "${PREFIX}-bilgeline" 2>&1 | grep -m1 "does not mount the shared config path" | sed 's/^/    /'
else
  fail "mount guard: no refusal diagnostic emitted"
  docker logs "${PREFIX}-bilgeline" 2>&1 | tail -15
  rc=1
fi

# The config must still be written to the shared volume.
if gen_config config | grep -q .; then
  pass "mount guard: config still written to the shared volume"
else
  fail "mount guard: config was not written"; rc=1
fi

# The innocent collector must not have been killed or restarted: it stays up on
# its own config (RestartCount 0, still running).
sleep 2
st="$(docker inspect --format '{{.State.Status}} restarts={{.RestartCount}}' "${PREFIX}-collector" 2>/dev/null)"
if printf '%s' "$st" | grep -q '^running restarts=0'; then
  pass "mount guard: innocent collector untouched ($st)"
else
  fail "mount guard: collector state unexpected: $st"; rc=1
fi

if [ "$rc" -eq 0 ]; then pass "10_mount_guard: ALL PASS"; else fail "10_mount_guard: FAILURES"; fi
exit $rc
