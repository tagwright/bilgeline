#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Live proof of bilgeline.multiline: a multi-line stack trace, a timestamped
# first line followed by indented continuation lines, is recombined into ONE
# exported record rather than one record per physical line.
#
# A producer emits blocks of a timestamp line plus three continuation lines.
# bilgeline generates the recombine operator (is_first_entry: body matches the
# timestamp regex) and we assert against the exported OTLP JSON that the record
# body carrying the first-line token ALSO carries the last continuation line,
# and that no continuation line escaped into a record of its own.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOKEN="BLMULTI-${RUN_ID}"
FIRST_LINE_RE='^\d{4}-\d{2}-\d{2}'

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

log "multiline: RUN_ID=$RUN_ID"

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

# --- producer: emits blocks of a timestamp line + 3 continuation lines --------
# Only the first line of each block matches the timestamp regex, so the collector
# must recombine each block into a single entry. The token rides the first line,
# the fixed marker "caused by: boom" rides the last continuation line.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  -e TOK="$TOKEN" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=itestfile \
  --label "bilgeline.multiline=$FIRST_LINE_RE" \
  "$ALPINE_IMAGE" sh -c 'i=0; while true; do
    echo "2026-08-27 12:00:00 ERROR request failed ${TOK}-${i}";
    echo "    at server.handle (server.go:42)";
    echo "    at router.route (router.go:11)";
    echo "    caused by: boom";
    i=$((i+1)); sleep 1;
  done' >/dev/null
log "multiline producer started"

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: the recombine operator is in the generated config ----------------
if gen_config config | grep -q "recombine"; then
  pass "generated config carries a recombine operator"
else
  fail "no recombine operator in generated config"; dump_diagnostics; rc=1
fi

# --- assert: records arrive (recombine flushes a block when the NEXT block's
# timestamp line arrives, so we need at least two blocks emitted) --------------
log "waiting for recombined trace records to appear..."
if ! wait_for_token out "$TOKEN" 45; then
  fail "multiline: producer lines never reached the destination"; dump_diagnostics; exit 1
fi
# Give several blocks time to emit and flush.
sleep 4
OUT="$(read_vol_file out logs.json)"

# Extract each record body as one JSON string value. The recombined body carries
# escaped newlines (\n, two literal chars) so it stays a single [^"] run.
BODIES="$(printf '%s' "$OUT" | grep -o '"stringValue":"[^"]*"')"

# --- assert: the token-bearing body ALSO holds the last continuation line -----
# One record per block => the same stringValue contains the first-line token and
# the last-line marker. Without recombine they would be in separate records.
if printf '%s' "$BODIES" | grep "$TOKEN" | grep -q "caused by: boom"; then
  pass "trace recombined: first-line token and last continuation line share one record body"
else
  fail "trace NOT recombined: token and continuation landed in different records"; rc=1
fi

# --- assert: no continuation line escaped into a record of its own ------------
# Every body holding the continuation marker must also hold the first-line token.
orphans="$(printf '%s' "$BODIES" | grep "caused by: boom" | grep -vc "$TOKEN")"
if [ "$orphans" -eq 0 ]; then
  pass "no orphaned continuation-line records (each block is exactly one record)"
else
  fail "found $orphans continuation-only record(s): recombine split the trace"; rc=1
fi

if [ "$rc" -ne 0 ]; then dump_diagnostics; fi
if [ "$rc" -eq 0 ]; then pass "21_multiline: ALL PASS"; else fail "21_multiline: FAILURES above"; fi
exit $rc
