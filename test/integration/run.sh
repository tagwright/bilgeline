#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# bilgeline live integration harness - orchestrator.
#
# Runs every case against a LIVE Docker socket. Each case is self-contained:
# it tears down any prior bilgeline-itest-* objects, builds its own stack, makes
# its assertions, and tears down on exit (including on failure, via a trap). This
# orchestrator just runs them in order and aggregates the result.
#
# Requirements:
#   - a reachable Docker socket at /var/run/docker.sock
#   - the images the harness uses (see lib.sh require_images): the bilgeline
#     image tagged bilgeline-itest:local (build it first, see README.md), the
#     pinned collector, busybox, and alpine.
#
# Safety: every Docker object this suite touches is prefixed bilgeline-itest-.
# The teardown only ever removes objects carrying that prefix. It never touches
# anything else on the host.
#
# Usage:
#   test/integration/run.sh              # run all cases
#   test/integration/run.sh 00 12        # run only cases whose file starts 00/12

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

if ! docker info >/dev/null 2>&1; then
  fail "no reachable Docker daemon; this harness needs a live socket"; exit 2
fi
if ! require_images; then
  warn "one or more images are missing. Build the bilgeline image first:"
  warn "  see test/integration/README.md (docker build ... -t bilgeline-itest:local)"
  exit 2
fi

CASES=(00_core_route 10_mount_guard 11_ambiguity 12_local_driver 13_env_preflight)

# Optional filter args: keep only cases whose basename matches any given prefix.
if [ "$#" -gt 0 ]; then
  filtered=()
  for c in "${CASES[@]}"; do
    for pat in "$@"; do
      case "$c" in "$pat"*) filtered+=("$c");; esac
    done
  done
  CASES=("${filtered[@]}")
fi

log "running ${#CASES[@]} integration case(s) against $(docker version --format '{{.Server.Version}}' 2>/dev/null)"
overall=0
declare -a results
for c in "${CASES[@]}"; do
  echo
  printf '%s================ %s ================%s\n' "$C_BLU" "$c" "$C_RST"
  if bash "$HERE/$c.sh"; then
    results+=("PASS $c")
  else
    results+=("FAIL $c")
    overall=1
  fi
done

echo
log "summary:"
for r in "${results[@]}"; do
  case "$r" in
    PASS*) pass "$r";;
    FAIL*) fail "$r";;
  esac
done

# Final safety sweep: make sure nothing bilgeline-itest-* was left behind.
teardown
leftover="$(docker ps -aq --filter "name=^/${PREFIX}-" 2>/dev/null)$(docker volume ls -q --filter "name=^${PREFIX}-" 2>/dev/null)$(docker network ls -q --filter "name=^${PREFIX}-" 2>/dev/null)"
if [ -n "$leftover" ]; then
  fail "leftover bilgeline-itest-* objects after teardown"; overall=1
else
  log "clean: no bilgeline-itest-* objects remain"
fi

[ "$overall" -eq 0 ] && pass "ALL INTEGRATION CASES PASSED" || fail "SOME INTEGRATION CASES FAILED"
exit $overall
