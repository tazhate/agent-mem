#!/usr/bin/env bash
# Fires N parallel PostToolUse hooks, kills the daemon with SIGKILL in the middle, and checks
# that every event ends up in the database (directly or via the spool).
set -euo pipefail
BIN=${BIN:-./amem}
N=${N:-200}
export AMEM_HOME=${AMEM_HOME:?set AMEM_HOME to a scratch dir}
mkdir -p "$AMEM_HOME"
payload() { printf '{"session_id":"dur","cwd":"/tmp","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"echo %d"},"tool_response":{"stdout":"%d"}}' "$1" "$1"; }
for i in $(seq 1 "$N"); do
  payload "$i" | "$BIN" hook claude-code >/dev/null &
  if [ "$i" -eq $((N / 2)) ]; then
    sleep 0.05
    pkill -KILL -f "[a]mem daemon" || true
  fi
done
wait
# Next hook restarts the daemon, which drains the spool.
payload 0 | "$BIN" hook claude-code >/dev/null
sleep 2.5
got=$(python3 -c "import sqlite3,sys;print(sqlite3.connect(sys.argv[1]).execute(\"select count(distinct input) from events where session_id='claude-code:dur'\").fetchone()[0])" "$AMEM_HOME/amem.db")
echo "events stored: $got / $((N + 1))"
[ "$got" -eq $((N + 1)) ]
