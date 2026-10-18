#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds ermite from this checkout and starts a throw-away server for the load
# tests, seeded with repositories whose history spreads over 90 days. The
# server is pinned to CPUS cores (GOMAXPROCS and taskset), so that the figures
# say what a small host can do, not what the workstation can.
#
#   scripts/loadtest/serve.sh            # 1 core, http://127.0.0.1:7418
#   CPUS=2 scripts/loadtest/serve.sh
#   scripts/loadtest/serve.sh stop
set -euo pipefail

ROOT=${LOADTEST_DIR:-${TMPDIR:-/tmp}/ermite-loadtest}
ADDR=${ADDR:-127.0.0.1:7418}
CPUS=${CPUS:-1}
URL=http://$ADDR
TOKEN=loadtest-admin-token-0123456789abcdefghijklmnop

# stop ends the server and waits for it (it shuts down gracefully), so that
# its data is never removed under it.
stop() {
  [[ -f $ROOT/pid ]] || return 0
  local pid; pid=$(cat "$ROOT/pid")
  if kill "$pid" 2>/dev/null; then
    for _ in $(seq 150); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$ROOT/pid"
}
if [[ ${1:-} == stop ]]; then stop; exit 0; fi
stop

cd "$(git rev-parse --show-toplevel)"
rm -rf "$ROOT" && mkdir -p "$ROOT/data" "$ROOT/wt"
GOWORK=off go build -o "$ROOT/ermite" ./cmd/ermite
BIN=$ROOT/ermite

export ERMITE_DATA_DIR=$ROOT/data ERMITE_PUBLIC_URL=$URL ERMITE_ADDR=$ADDR
export ERMITE_ADMIN_TOKEN=$TOKEN ERMITE_LOG_FORMAT=console ERMITE_LOG_LEVEL=warn
export ERMITE_URL=$URL ERMITE_TOKEN=$TOKEN ERMITE_CONFIG=$ROOT/cfg

# The last CPUS of the CPUs this shell may use run the server; run the load
# generator on the others (see README.md).
mapfile -t allowed < <(python3 -c 'import os; print(*sorted(os.sched_getaffinity(0)), sep="\n")')
if ! [[ $CPUS =~ ^[1-9][0-9]*$ ]] || ((CPUS >= ${#allowed[@]})); then
  echo "CPUS must be between 1 and $((${#allowed[@]} - 1)), got $CPUS" >&2; exit 2
fi
server_cpus=$(IFS=,; echo "${allowed[*]: -CPUS}")
GOMAXPROCS=$CPUS taskset -c "$server_cpus" "$BIN" serve >"$ROOT/log" 2>&1 &
echo $! >"$ROOT/pid"
for _ in $(seq 100); do curl -fs "$URL/-/health" >/dev/null && break; sleep 0.1; done

now=$(date +%s)
seed() { # name visibility description profile
  local name=$1 vis=$2 desc=$3 prof=$4 d=$ROOT/wt/$1
  "$BIN" repo create "$name" --visibility "$vis" --description "$desc" >/dev/null
  mkdir -p "$d" && cd "$d"
  git init -q -b main . && git config user.email loadtest@example.test && git config user.name Loadtest
  python3 - "$prof" "$now" >"$ROOT/$name.dates" <<'PY'
import sys, math, random
prof, now = sys.argv[1], int(sys.argv[2]); random.seed(prof)
out = []
for day in range(89, -1, -1):
    if prof == 'busy': lam = 6 + 5*math.sin(day/9) + (14 if day < 6 else 0)
    elif prof == 'bursty': lam = 18 if 40 < day < 48 or 10 < day < 14 else 0.3
    elif prof == 'sleepy': lam = 3 if day > 60 else 0
    else: lam = 1.2
    for _ in range(max(0, int(random.gauss(lam, math.sqrt(lam + 0.1))))):
        out.append(now - day*86400 - random.randint(0, 80000))
for t in sorted(out): print(t)
PY
  local i=0 subjects=("initial import" "add the parser" "fix an off-by-one error in the tokenizer" "refactor storage layer" "bump dependencies" "document the public API" "tidy up")
  while read -r t; do
    i=$((i + 1)); echo $i >f.txt; git add -A
    GIT_AUTHOR_DATE="@$t" GIT_COMMITTER_DATE="@$t" git commit -qm "${subjects[$((i % 7))]} $i"
  done <"$ROOT/$name.dates"
  git tag "v1.$((i / 10)).0"
  git push -q --tags "http://x:$TOKEN@$ADDR/$name.git" main
}
seed alpha public "A small Wayland compositor that spends its frames on your apps" busy
seed bravo public "Keyboard-driven web browser for tiling window managers" bursty
seed charlie public "Self-hosted container deployment: push an image, deploy it" steady
seed delta private "Library that reads terminal output and tracks the screen" sleepy
"$BIN" repo create echo --visibility public --description "Never pushed" >/dev/null

echo "ermite on $URL, CPU(s) $server_cpus, data in $ROOT (pid $(cat "$ROOT/pid"))"
echo "admin token: $TOKEN"
