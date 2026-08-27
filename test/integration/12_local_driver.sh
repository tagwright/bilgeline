#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Failure path 3 - non-json-file log driver. v1 tails only Docker's json-file
# driver. A container run with --log-driver local, even labeled enable=true,
# must be EXCLUDED from routing (its lines never reach the destination) and
# bilgeline must warn. A json-file producer alongside it must still route, so
# the exclusion is proven surgical, not a blanket failure.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOK_JSON="BILGELINE-ITEST-${RUN_ID}-JSON"
TOK_LOCAL="BILGELINE-ITEST-${RUN_ID}-LOCAL"

rc=0
trap teardown EXIT
teardown

log "local driver: a --log-driver local producer must be excluded, json-file must route"

net_create
vol_create config; vol_create checkpoints; vol_create out; vol_create blconf
seed_config config
# Hand the collector-side volumes to the non-root collector's uid, the same
# checkpoint-volume ownership prep the shipped compose config-seed performs.
chown_vol checkpoints "$COLLECTOR_VOL_OWNER"
chown_vol out "$COLLECTOR_VOL_OWNER"

cat <<YML | docker run --rm -i -v "${PREFIX}-blconf:/etc/bilgeline" "$BUSYBOX_IMAGE" sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: itestfile
shared_config_path: /config/otelcol.yaml
debounce: 1s
destinations:
  itestfile:
    type: file
    path: /out/logs.json
YML

docker run -d --name "${PREFIX}-collector" --network "$NET" --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -v "${PREFIX}-config:/config:ro" \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  -v "${PREFIX}-checkpoints:/var/lib/otelcol/file_storage" \
  -v "${PREFIX}-out:/out" \
  "$COLLECTOR_IMAGE" --config=/config/otelcol.yaml >/dev/null
wait_for_log collector "Everything is ready" 30 || { fail "collector did not start"; exit 1; }

# json-file producer (default driver): should route.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true --label bilgeline.destination=itestfile \
  "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOK_JSON}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null

# local-driver producer: must be excluded. --log-driver local writes a
# daemon-private protobuf stream bilgeline cannot tail.
docker run -d --name "${PREFIX}-producer-local" --network "$NET" \
  --log-driver local \
  --label bilgeline.enable=true --label bilgeline.destination=itestfile \
  "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOK_LOCAL}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null

# bilgeline must warn about the local driver exclusion.
if wait_for_log bilgeline 'log driver .*local.* is not json-file' 25; then
  pass "local driver: bilgeline warned and excluded the local-driver container"
  docker logs "${PREFIX}-bilgeline" 2>&1 | grep -m1 'is not json-file' | sed 's/^/    /'
else
  fail "local driver: no exclusion warning emitted"
  docker logs "${PREFIX}-bilgeline" 2>&1 | tail -15; rc=1
fi

# The json-file producer must route.
if wait_for_token out "$TOK_JSON" 45; then
  pass "local driver: the json-file producer still routed"
else
  fail "local driver: json-file producer failed to route"; rc=1
fi

# The local-driver producer must NEVER reach the destination.
sleep 3
if read_vol_file out logs.json | grep -q "$TOK_LOCAL"; then
  fail "local driver: EXCLUDED producer's lines leaked to the destination"; rc=1
else
  pass "local driver: excluded producer's lines never reached the destination"
fi

# Also confirm the generated config includes only the json-file producer's log.
lid="$(docker inspect --format '{{.Id}}' "${PREFIX}-producer-local")"
if gen_config config | grep -q "$lid"; then
  fail "local driver: excluded container id present in generated filelog includes"; rc=1
else
  pass "local driver: excluded container id absent from generated config"
fi

if [ "$rc" -eq 0 ]; then pass "12_local_driver: ALL PASS"; else fail "12_local_driver: FAILURES"; fi
exit $rc
