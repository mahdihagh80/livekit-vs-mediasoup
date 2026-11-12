#!/usr/bin/env bash
# Stop all benchmark LiveKit instances (SIGTERM, then SIGKILL after 20s).
DIR="$(cd "$(dirname "$0")" && pwd)/run"
for f in "$DIR"/livekit-*.pid; do
  [[ -f $f ]] || continue
  pid=$(cat "$f"); kill "$pid" 2>/dev/null && echo "stopping $pid"
done
for _ in $(seq 20); do pgrep -f "livekit-server --config $DIR" >/dev/null || break; sleep 1; done
pkill -9 -f "livekit-server --config $DIR" 2>/dev/null || true
rm -f "$DIR"/livekit-*.pid
echo "livekit stopped"
