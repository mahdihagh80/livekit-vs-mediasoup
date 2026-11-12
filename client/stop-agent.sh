#!/usr/bin/env bash
DIR="$(cd "$(dirname "$0")" && pwd)/run"
[[ -f "$DIR/agent.pid" ]] && kill "$(cat "$DIR/agent.pid")" 2>/dev/null && echo "agent stopping"
rm -f "$DIR/agent.pid"
