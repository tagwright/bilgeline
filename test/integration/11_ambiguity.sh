#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Failure path 2 - collector ambiguity. TWO containers carrying
# bilgeline.collector=true is a hard identity error: bilgeline must write the
# config but send NO signal, because it cannot know which one to reload.

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

rc=0
trap teardown EXIT
teardown

log "ambiguity: two containers both marked bilgeline.collector=true"

net_create
vol_create config
vol_create blconf
seed_config config

cat <<YML | docker run --rm -i -v "${PREFIX}-blconf:/etc/bilgeline" "$BUSYBOX_IMAGE" sh -c 'cat > /etc/bilgeline/bilgeline.yml'
default_destination: debug
shared_config_path: /config/otelcol.yaml
debounce: 1s
YML

# Two marked "collectors". They need not be real collectors for this identity
# check; any container carrying the marker is a candidate bilgeline must refuse
# to disambiguate. Alpine sleepers keep the test cheap.
docker run -d --name "${PREFIX}-collector-a" --network "$NET" \
  --label bilgeline.collector=true "$ALPINE_IMAGE" sleep 3600 >/dev/null
docker run -d --name "${PREFIX}-collector-b" --network "$NET" \
  --label bilgeline.collector=true "$ALPINE_IMAGE" sleep 3600 >/dev/null

# A producer so the spec is non-inert and a full apply runs.
docker run -d --name "${PREFIX}-producer" --network "$NET" \
  --label bilgeline.enable=true --label bilgeline.destination=debug \
  "$ALPINE_IMAGE" sh -c 'while true; do echo ambiguity-probe; sleep 0.3; done' >/dev/null

docker run -d --name "${PREFIX}-bilgeline" --network "$NET" --group-add "$DOCKER_GID" \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v "${PREFIX}-config:/config" \
  -v "${PREFIX}-blconf:/etc/bilgeline:ro" \
  "$IMAGE" daemon --log-level debug >/dev/null

if wait_for_log bilgeline "multiple containers carry the bilgeline.collector marker" 25; then
  pass "ambiguity: bilgeline reported the ambiguous marker and did not signal"
  docker logs "${PREFIX}-bilgeline" 2>&1 | grep -m1 "multiple containers carry" | sed 's/^/    /'
else
  fail "ambiguity: no ambiguity diagnostic emitted"
  docker logs "${PREFIX}-bilgeline" 2>&1 | tail -15
  rc=1
fi

# The config is still written even though no signal is sent.
if gen_config config | grep -q .; then
  pass "ambiguity: config still written to the shared volume"
else
  fail "ambiguity: config was not written"; rc=1
fi

# Neither candidate was signalled/killed: both stay up, RestartCount 0.
sleep 2
ok=1
for c in collector-a collector-b; do
  st="$(docker inspect --format '{{.State.Status}} restarts={{.RestartCount}}' "${PREFIX}-$c" 2>/dev/null)"
  printf '%s' "$st" | grep -q '^running restarts=0' || { fail "ambiguity: $c state unexpected: $st"; ok=0; rc=1; }
done
[ "$ok" -eq 1 ] && pass "ambiguity: neither marked container was signalled or restarted"

if [ "$rc" -eq 0 ]; then pass "11_ambiguity: ALL PASS"; else fail "11_ambiguity: FAILURES"; fi
exit $rc
