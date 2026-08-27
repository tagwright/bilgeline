#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Core proof: logs actually route end to end, and the event-driven reload picks
# up a second producer live.
#
# Discovery -> config generation -> atomic write -> collector signal -> collector
# load -> filelog tail -> file export. If the producer's known unique lines land
# in the destination file carrying the expected resource attributes, every stage
# worked together against a live socket.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOKEN="BILGELINE-ITEST-${RUN_ID}"
TOKEN2="BILGELINE-ITEST-${RUN_ID}-P2"

rc=0
trap teardown EXIT
teardown

dump_diagnostics() {
  echo "----- generated collector config (shared volume) -----"
  gen_config config || echo "(no config on volume)"
  echo "----- bilgeline logs -----"
  docker logs "${PREFIX}-bilgeline" 2>&1 | tail -40 || true
  echo "----- collector logs -----"
  docker logs "${PREFIX}-collector" 2>&1 | tail -40 || true
  echo "----- out volume (first 600 bytes) -----"
  read_vol_file out logs.json | head -c 600 || true
  echo
}

log "core: RUN_ID=$RUN_ID"

# --- setup: network, volumes, seed inert config -------------------------------
net_create
vol_create config
vol_create checkpoints
vol_create out
vol_create blconf
seed_config config

# Write the bilgeline.yml into a config volume the bilgeline container mounts.
cat <<YML | docker run --rm -i -v "${PREFIX}-blconf:/etc/bilgeline" "$BUSYBOX_IMAGE" sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: itestfile
shared_config_path: /config/otelcol.yaml
debounce: 1s
destinations:
  itestfile:
    type: file
    path: /out/logs.json
YML

# --- collector: starts against the inert seed config --------------------------
docker run -d --name "${PREFIX}-collector" --network "$NET" \
  --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -v "${PREFIX}-config:/config:ro" \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  -v "${PREFIX}-checkpoints:/var/lib/otelcol/file_storage" \
  -v "${PREFIX}-out:/out" \
  "$COLLECTOR_IMAGE" --config=/config/otelcol.yaml >/dev/null

if ! wait_for_log collector "Everything is ready|Everything is ready. Begin running" 30; then
  fail "collector did not come up on the inert config"; dump_diagnostics; exit 1
fi
log "collector up on inert config"

# --- producer: prints known unique lines to stdout forever --------------------
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=itestfile \
  "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOKEN}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null
log "producer started"

# --- bilgeline: discovers, generates, signals ---------------------------------
docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: producer lines reach the destination -----------------------------
log "waiting for '${TOKEN}' to appear at the destination..."
if wait_for_token out "$TOKEN" 45; then
  pass "core route: producer lines reached the file destination"
else
  fail "core route: producer lines never reached the destination"; dump_diagnostics; rc=1
fi

# --- assert: expected resource attributes present -----------------------------
if [ "$rc" -eq 0 ]; then
  OUT="$(read_vol_file out logs.json)"
  for attr in "service.name" "producer" "container.id" "container.name" "log.iostream"; do
    if printf '%s' "$OUT" | grep -q "$attr"; then
      pass "attribute present: $attr"
    else
      fail "expected attribute missing: $attr"; rc=1
    fi
  done
  # service.name must be the producer container name.
  if printf '%s' "$OUT" | grep -q "bilgeline-itest-producer"; then
    pass "service.name/container.name resolved to bilgeline-itest-producer"
  else
    fail "container identity attribute value not found"; rc=1
  fi
  if [ "$rc" -ne 0 ]; then dump_diagnostics; fi
fi

# --- reload proof: a second labeled producer appears live ---------------------
if [ "$rc" -eq 0 ]; then
  log "reload: starting second producer, expecting a live regenerate + signal"
  docker run -d --name "${PREFIX}-producer2" --network "$NET" \
    --label bilgeline.enable=true \
    --label bilgeline.destination=itestfile \
    "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOKEN2}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null
  if wait_for_token out "$TOKEN2" 45; then
    pass "reload: second producer's lines reached the destination after a live event"
  else
    fail "reload: second producer never routed"; dump_diagnostics; rc=1
  fi
fi

if [ "$rc" -eq 0 ]; then pass "00_core_route: ALL PASS"; else fail "00_core_route: FAILURES above"; fi
exit $rc
