#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Live proof of the filtering grammar: bilgeline.drop (indexed form) and
# bilgeline.level.min together, over a json-parsed body with a level field.
#
# A single producer emits six JSON lines per tick, each carrying a unique token:
#   - two WANTED lines at/above the warn floor (error, warn) with no drop match
#   - two below-floor lines (info, debug) that level.min=warn must drop
#   - two error-level lines whose body matches a drop regex (health-check noise)
#     that the drop patterns must drop DESPITE being above the floor
# We assert the wanted tokens are present at the destination and every dropped
# token is ABSENT. The two error-level health-check lines isolate the drop
# behavior from the severity floor: only a working drop regex removes them.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOKEN="BLFILT-${RUN_ID}"

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
  echo "----- out volume (first 2000 bytes) -----"
  read_vol_file out logs.json | head -c 2000 || true
  echo
}

log "filtering: RUN_ID=$RUN_ID"

# --- setup --------------------------------------------------------------------
net_create
vol_create config
vol_create checkpoints
vol_create out
vol_create blconf
seed_config config
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

docker run -d --name "${PREFIX}-collector" --network "$NET" \
  --user "$COLLECTOR_USER" \
  --label bilgeline.collector=true \
  -v "${PREFIX}-config:/config:ro" \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  -v "${PREFIX}-checkpoints:/var/lib/otelcol/file_storage" \
  -v "${PREFIX}-out:/out" \
  "$COLLECTOR_IMAGE" --config=/config/otelcol.yaml >/dev/null

if ! wait_for_log collector "Everything is ready" 30; then
  fail "collector did not come up on the inert config"; dump_diagnostics; exit 1
fi
log "collector up on inert config"

# --- producer: six labeled JSON lines per tick --------------------------------
# Indexed drop form (drop.0, drop.1) is the documented norm for multiple drops.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  -e TOK="$TOKEN" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=itestfile \
  --label bilgeline.parse=json \
  --label bilgeline.level.min=warn \
  --label "bilgeline.drop.0=GET /healthz" \
  --label "bilgeline.drop.1=kube-probe" \
  "$ALPINE_IMAGE" sh -c 'i=0; while true; do
    echo "{\"level\":\"error\",\"msg\":\"real error\",\"tok\":\"${TOK}-keep-error-${i}\"}";
    echo "{\"level\":\"warn\",\"msg\":\"real warn\",\"tok\":\"${TOK}-keep-warn-${i}\"}";
    echo "{\"level\":\"info\",\"msg\":\"chatter\",\"tok\":\"${TOK}-drop-info-${i}\"}";
    echo "{\"level\":\"debug\",\"msg\":\"trace\",\"tok\":\"${TOK}-drop-debug-${i}\"}";
    echo "{\"level\":\"error\",\"msg\":\"GET /healthz probe\",\"tok\":\"${TOK}-drop-health-${i}\"}";
    echo "{\"level\":\"error\",\"msg\":\"kube-probe/1.0\",\"tok\":\"${TOK}-drop-kube-${i}\"}";
    i=$((i+1)); sleep 0.3;
  done' >/dev/null
log "filtering producer started"

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: the filter processor is in the generated config ------------------
if gen_config config | grep -q "filter/drop"; then
  pass "generated config carries a filter/drop processor"
else
  fail "no filter/drop processor in generated config"; dump_diagnostics; rc=1
fi

# --- assert: wanted lines arrive ----------------------------------------------
log "waiting for wanted (kept) records to appear..."
if ! wait_for_token out "${TOKEN}-keep-error" 45; then
  fail "filtering: wanted error lines never reached the destination"; dump_diagnostics; exit 1
fi
# Let many ticks flow so any leaking dropped line would have shown up by now.
sleep 4
OUT="$(read_vol_file out logs.json)"

# --- assert: both wanted classes present --------------------------------------
for keep in keep-error keep-warn; do
  if printf '%s' "$OUT" | grep -q "${TOKEN}-${keep}"; then
    pass "wanted class present: $keep"
  else
    fail "wanted class missing: $keep"; rc=1
  fi
done

# --- assert: every dropped class is ABSENT ------------------------------------
# info/debug removed by the warn floor; health/kube removed by the drop regexes
# despite being error-level (so the drop, not the floor, is what removed them).
for drop in drop-info drop-debug drop-health drop-kube; do
  if printf '%s' "$OUT" | grep -q "${TOKEN}-${drop}"; then
    fail "dropped class LEAKED to destination: $drop"; rc=1
  else
    pass "dropped class absent: $drop"
  fi
done

if [ "$rc" -ne 0 ]; then dump_diagnostics; fi
if [ "$rc" -eq 0 ]; then pass "22_filtering: ALL PASS"; else fail "22_filtering: FAILURES above"; fi
exit $rc
