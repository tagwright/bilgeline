#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Live proof of bilgeline.stream: a producer writes distinct tokens to BOTH
# stdout and stderr and is labeled stream=stderr. Only the stderr lines must
# route; the stdout lines must be dropped. The container operator stamps
# log.iostream from Docker's json-file envelope and the shared filter drops the
# unwanted stream.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
OUTTOK="BLOUT-${RUN_ID}"
ERRTOK="BLERR-${RUN_ID}"

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
  echo "----- out volume (first 1500 bytes) -----"
  read_vol_file out logs.json | head -c 1500 || true
  echo
}

log "stream: RUN_ID=$RUN_ID"

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

# --- producer: writes to BOTH streams, labeled stream=stderr ------------------
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  -e OUTTOK="$OUTTOK" -e ERRTOK="$ERRTOK" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=itestfile \
  --label bilgeline.stream=stderr \
  "$ALPINE_IMAGE" sh -c 'i=0; while true; do
    echo "${OUTTOK}-${i}";
    echo "${ERRTOK}-${i}" 1>&2;
    i=$((i+1)); sleep 0.2;
  done' >/dev/null
log "dual-stream producer started"

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: stderr lines route -----------------------------------------------
log "waiting for stderr lines to appear..."
if ! wait_for_token out "$ERRTOK" 45; then
  fail "stream=stderr: stderr lines never reached the destination"; dump_diagnostics; exit 1
fi
pass "stderr lines routed to the destination"
# Give stdout ample time to (wrongly) show up.
sleep 4
OUT="$(read_vol_file out logs.json)"

# --- assert: stdout lines were dropped ----------------------------------------
if printf '%s' "$OUT" | grep -q "$OUTTOK"; then
  fail "stdout lines LEAKED despite stream=stderr"; dump_diagnostics; rc=1
else
  pass "stdout lines correctly dropped"
fi

# --- assert: routed records carry log.iostream=stderr -------------------------
if printf '%s' "$OUT" | grep -q '"stringValue":"stderr"'; then
  pass "routed records carry log.iostream=stderr"
else
  fail "no record carries log.iostream=stderr"; rc=1
fi

if [ "$rc" -ne 0 ]; then dump_diagnostics; fi
if [ "$rc" -eq 0 ]; then pass "24_stream: ALL PASS"; else fail "24_stream: FAILURES above"; fi
exit $rc
