#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Live proof of multi-destination fan-out and routing-connector correctness.
#
# Two file destinations, fileA and fileB, are defined in bilgeline.yml. One
# producer is labeled destination=fileA,fileB and MUST land in BOTH outputs. A
# second producer is labeled destination=fileA only and MUST land in fileA but
# NOT fileB. This proves the routing connector builds one table entry per
# distinct destination set (fileA vs fileA,fileB), that a fan-out set exports to
# every member, and that a narrower set does not leak into the other output.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOKEN_BOTH="BLFANBOTH-${RUN_ID}"
TOKEN_A="BLFANA-${RUN_ID}"

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
  echo "----- outA (first 800 bytes) -----"
  read_vol_file outA logs.json | head -c 800 || true
  echo
  echo "----- outB (first 800 bytes) -----"
  read_vol_file outB logs.json | head -c 800 || true
  echo
}

log "fan-out: RUN_ID=$RUN_ID"

# --- setup: two output volumes ------------------------------------------------
net_create
vol_create config
vol_create checkpoints
vol_create outA
vol_create outB
vol_create blconf
seed_config config
chown_vol checkpoints "$COLLECTOR_VOL_OWNER"
chown_vol outA "$COLLECTOR_VOL_OWNER"
chown_vol outB "$COLLECTOR_VOL_OWNER"

cat <<YML | docker run --rm -i -v "${PREFIX}-blconf:/etc/bilgeline" "$BUSYBOX_IMAGE" sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: fileA
shared_config_path: /config/otelcol.yaml
debounce: 1s
destinations:
  fileA:
    type: file
    path: /outA/logs.json
  fileB:
    type: file
    path: /outB/logs.json
YML

docker run -d --name "${PREFIX}-collector" --network "$NET" \
  --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -v "${PREFIX}-config:/config:ro" \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  -v "${PREFIX}-checkpoints:/var/lib/otelcol/file_storage" \
  -v "${PREFIX}-outA:/outA" \
  -v "${PREFIX}-outB:/outB" \
  "$COLLECTOR_IMAGE" --config=/config/otelcol.yaml >/dev/null

if ! wait_for_log collector "Everything is ready" 30; then
  fail "collector did not come up on the inert config"; dump_diagnostics; exit 1
fi
log "collector up on inert config"

# --- producers: one fans out to both, one goes to fileA only ------------------
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=fileA,fileB \
  "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOKEN_BOTH}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null
docker run -d --name "${PREFIX}-producer2" --network "$NET" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=fileA \
  "$ALPINE_IMAGE" sh -c "i=0; while true; do echo \"${TOKEN_A}-\$i\"; i=\$((i+1)); sleep 0.2; done" >/dev/null
log "both producers started"

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: the routing table has both distinct destination sets -------------
CFG="$(gen_config config)"
if printf '%s' "$CFG" | grep -q '"fileA,fileB"' && printf '%s' "$CFG" | grep -q '"bilgeline.routing_key"] == "fileA"'; then
  pass "routing table carries both distinct sets (fileA and fileA,fileB)"
else
  warn "could not confirm both routing-key conditions in the generated config"
fi

# --- assert: the fan-out producer reaches BOTH outputs ------------------------
log "waiting for the fan-out producer in fileA and fileB..."
if wait_for_token outA "$TOKEN_BOTH" 45; then
  pass "fan-out producer reached fileA"
else
  fail "fan-out producer never reached fileA"; dump_diagnostics; rc=1
fi
if wait_for_token outB "$TOKEN_BOTH" 45; then
  pass "fan-out producer reached fileB"
else
  fail "fan-out producer never reached fileB"; dump_diagnostics; rc=1
fi

# --- assert: the fileA-only producer reaches fileA but NOT fileB --------------
log "waiting for the fileA-only producer in fileA..."
if wait_for_token outA "$TOKEN_A" 45; then
  pass "fileA-only producer reached fileA"
else
  fail "fileA-only producer never reached fileA"; dump_diagnostics; rc=1
fi
# Give fileB ample time to (wrongly) receive it, then assert it did not.
sleep 4
if read_vol_file outB logs.json | grep -q "$TOKEN_A"; then
  fail "fileA-only producer LEAKED into fileB (routing set not isolated)"; dump_diagnostics; rc=1
else
  pass "fileA-only producer correctly absent from fileB"
fi

if [ "$rc" -eq 0 ]; then pass "23_fanout: ALL PASS"; else fail "23_fanout: FAILURES above"; fi
exit $rc
