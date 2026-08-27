#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Live proof of bilgeline.parse=json: structured JSON log bodies are parsed and
# their fields promoted to record ATTRIBUTES (not left buried in a raw string
# body), and the default level-field probing (level, severity, lvl) drives the
# record's OTel severity.
#
# A producer emits two JSON lines per tick, one level=info and one level=error,
# each carrying a unique token. bilgeline discovers the parse=json label,
# generates the json_parser + severity-probe operator chain, and the collector
# tails + parses + exports. We then assert against the exported OTLP JSON that
# the fields landed as attributes and that the severities were mapped.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

RUN_ID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null | tr -d - | cut -c1-12)"
TOKEN="BLJSON-${RUN_ID}"

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

log "parse=json: RUN_ID=$RUN_ID"

# --- setup: network, volumes, seed inert config -------------------------------
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

# --- collector: starts against the inert seed config --------------------------
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

# --- producer: emits structured JSON, one info + one error line per tick ------
# Single-quoted host string: $TOK and $i are expanded by the container shell.
# The \" sequences become literal double quotes in the emitted JSON.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  -e TOK="$TOKEN" \
  --label bilgeline.enable=true \
  --label bilgeline.destination=itestfile \
  --label bilgeline.parse=json \
  "$ALPINE_IMAGE" sh -c 'i=0; while true; do
    echo "{\"level\":\"info\",\"msg\":\"hello\",\"user\":\"alice\",\"tok\":\"${TOK}-info-${i}\"}";
    echo "{\"level\":\"error\",\"msg\":\"boom\",\"user\":\"bob\",\"tok\":\"${TOK}-error-${i}\"}";
    i=$((i+1)); sleep 0.2;
  done' >/dev/null
log "json producer started"

# --- bilgeline: discovers, generates, signals ---------------------------------
docker run -d --name "${PREFIX}-bilgeline" --network "$NET" \
  --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null
log "bilgeline started"

# --- assert: json lines reach the destination ---------------------------------
log "waiting for '${TOKEN}' to appear at the destination..."
if ! wait_for_token out "$TOKEN" 45; then
  fail "parse=json: producer lines never reached the destination"; dump_diagnostics; exit 1
fi
pass "json producer lines reached the file destination"

# Let a couple more batches land so both info and error records are present.
sleep 2
OUT="$(read_vol_file out logs.json)"

# --- assert: fields promoted to ATTRIBUTES, not left in the raw body ----------
# In the OTLP JSON a promoted field is an attribute entry {"key":"user",...};
# in the raw body the same text appears only as escaped \"user\". Matching the
# unescaped attribute-key form proves promotion, not a body substring.
for key in user msg level tok; do
  if printf '%s' "$OUT" | grep -q "\"key\":\"$key\""; then
    pass "json field promoted to attribute: $key"
  else
    fail "json field NOT promoted to attribute: $key"; rc=1
  fi
done
# The promoted values are present as attribute string values.
for val in alice bob hello boom; do
  if printf '%s' "$OUT" | grep -q "\"stringValue\":\"$val\""; then
    pass "promoted attribute value present: $val"
  else
    fail "promoted attribute value missing: $val"; rc=1
  fi
done

# --- assert: default level.field probing set the record severity --------------
# error -> severityNumber 17, info -> severityNumber 9. These are structured
# severity fields, distinct from the "level" text sitting in the body.
if printf '%s' "$OUT" | grep -q '"severityNumber":17'; then
  pass "severity mapped: error -> severityNumber 17"
else
  fail "error record did not map to severityNumber 17"; rc=1
fi
if printf '%s' "$OUT" | grep -q '"severityNumber":9'; then
  pass "severity mapped: info -> severityNumber 9"
else
  fail "info record did not map to severityNumber 9"; rc=1
fi

if [ "$rc" -ne 0 ]; then dump_diagnostics; fi
if [ "$rc" -eq 0 ]; then pass "20_parse_json: ALL PASS"; else fail "20_parse_json: FAILURES above"; fi
exit $rc
