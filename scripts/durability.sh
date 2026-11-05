#!/usr/bin/env bash
# Fires N parallel PostToolUse hooks, kills the daemon with SIGKILL in the middle, and checks
# that every event ends up in the database (directly or via the spool).
set -euo pipefail
BIN=${BIN:-./amem}
N=${N:-200}
export AMEM_HOME=${AMEM_HOME:?set AMEM_HOME to a scratch dir}
mkdir -p "$AMEM_HOME"
payload() { printf '{"session_id":"dur","cwd":"/tmp","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"echo %d"},"tool_response":{"stdout":"%d"}}' "$1" "$1"; }
killed=0
for i in $(seq 1 "$N"); do
  payload "$i" | "$BIN" hook claude-code >/dev/null &
  if [ "$i" -eq $((N / 2)) ]; then
    sleep 0.05
    # Only the scratch daemon: never touch daemons of other AMEM_HOMEs.
    pid=$(cat "$AMEM_HOME/amemd.lock" 2>/dev/null || true)
    [ -n "$pid" ] && kill -KILL "$pid" 2>/dev/null && killed=1 || true
  fi
done
wait
# Next hook restarts the daemon, which drains the spool.
payload 0 | "$BIN" hook claude-code >/dev/null
sleep 2.5
read -r got total < <(python3 -c "import sqlite3,sys;print(*sqlite3.connect(sys.argv[1]).execute(\"select count(distinct input), count(*) from events where session_id='claude-code:dur'\").fetchone())" "$AMEM_HOME/amem.db")
echo "events stored: $got distinct, $total rows / $((N + 1)) expected (daemon killed: $killed)"
pid=$(cat "$AMEM_HOME/amemd.lock" 2>/dev/null || true)
[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
[ "$killed" -eq 1 ] && [ "$got" -eq $((N + 1)) ] && [ "$total" -eq $((N + 1)) ]
