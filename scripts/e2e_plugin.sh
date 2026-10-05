#!/usr/bin/env bash
#
# End-to-end plugin runtime test. Starts a real AstrBot (Go) binary, installs
# the real `echo` plugin (git) plus a Go tool plugin and a Python plugin, then
# drives the dashboard chat (webchat) and asserts replies.
#
# Covered per platform:
#   - Go gRPC   : install echo as grpc, run /echo
#   - Go Native : install echo as native (.so plugin.Open / .dll C-ABI bridge), run /echo
#   - Go tool   : install toolgen (LLM tool), ask the model to call echo_tool
#   - Python    : install python_e2e as shared and grpc, run am_status
#
# Requires: ASTRBOT_BIN, and (for tool tests) OPENROUTER_API_KEY.
# Usage: e2e_plugin.sh [data_dir] [port]
set -euo pipefail

ASTRBOT_BIN="${ASTRBOT_BIN:?set ASTRBOT_BIN to the built astrbot binary}"
DATA_DIR="${1:-$(pwd)/e2e-data}"
PORT="${2:-6185}"
BASE="http://127.0.0.1:${PORT}"
DATATEST_DIR="${DATATEST_DIR:-datatest}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ECHO_REPO="${ECHO_REPO:-https://github.com/Astrbot-Go-Market/Astrbot-go-plugin-echo}"

log() { echo "[e2e] $*"; }
fail() { echo "[e2e][FAIL] $*" >&2; exit 1; }

# ── prepare data dir from the datatest template ──────────────────────────────
prepare_data() {
  rm -rf "$DATA_DIR"
  mkdir -p "$DATA_DIR"
  if [ -f "$DATATEST_DIR/cmd_config.json" ]; then
    sed "s/__OPENROUTER_API_KEY__/${OPENROUTER_API_KEY:-}/" \
      "$DATATEST_DIR/cmd_config.json" > "$DATA_DIR/cmd_config.json"
  fi
  # force the test port/host regardless of template
  python3 - "$DATA_DIR/cmd_config.json" "$PORT" <<'PY'
import json,sys
p,port=sys.argv[1],int(sys.argv[2])
try: d=json.load(open(p))
except Exception: d={"config_version":3}
d.setdefault("dashboard",{})["enable"]=True
d["dashboard"]["host"]="127.0.0.1"
d["dashboard"]["port"]=port
json.dump(d,open(p,"w"),ensure_ascii=False,indent=2)
PY
}

start_host() {
  log "starting host on :$PORT (data=$DATA_DIR)"
  ASTRBOT_DATA_PATH="$DATA_DIR" ASTRBOT_GO_SDK="$REPO_ROOT/../AstrbotDev/astrbot-go-plugin-sdk" \
    "$ASTRBOT_BIN" > "$DATA_DIR/host.log" 2>&1 &
  HOST_PID=$!
  for _ in $(seq 1 40); do
    if curl -s -o /dev/null --max-time 2 "$BASE/"; then return 0; fi
    sleep 2
  done
  tail -50 "$DATA_DIR/host.log" >&2 || true
  fail "host did not become ready"
}

login() {
  local pw="$1"
  TOKEN=$(curl -s -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"astrbot\",\"password\":\"$pw\"}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['token'])")
  [ -n "$TOKEN" ] || fail "login failed"
}

api() { # method path [json-body]
  local m="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -X "$m" "$BASE$path" -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' -d "$body"
  else
    curl -s -X "$m" "$BASE$path" -H "Authorization: Bearer $TOKEN"
  fi
}

install_git() { # url runtime [extra-json]
  local url="$1" runtime="$2" extra="${3:-}"
  api POST /api/v1/plugins/install/git \
    "{\"url\":\"$url\",\"go_choice\":\"download\",\"deps_choice\":\"lazy\",\"preferred_runtime\":\"$runtime\",\"cc_choice\":\"gcc\"${extra:+,$extra}}"
}

install_upload() { # zip runtime
  local zip="$1" runtime="$2"
  curl -s -X POST "$BASE/api/v1/plugins/install/upload" -H "Authorization: Bearer $TOKEN" \
    -F "file=@$zip" -F "deps_choice=lazy" -F "preferred_runtime=$runtime"
}

chat() { # session text -> prints plain answer text
  local session="$1" text="$2"
  curl -s -N -X POST "$BASE/api/v1/chat" -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$session\",\"message\":[{\"type\":\"plain\",\"text\":\"$text\"}]}" \
    --max-time 120 \
    | grep -a '^data: ' | sed 's/^data: //' \
    | python3 -c "
import sys,json
out=[]
for line in sys.stdin:
    try: ev=json.loads(line)
    except Exception: continue
    if ev.get('type')=='plain' and ev.get('chain_type')=='text':
        out.append(str(ev.get('data','')))
print('\n'.join(out))
"
}

check_contains() { # label haystack needle
  if echo "$2" | grep -qF "$3"; then log "OK: $1"; else fail "$1: expected '$3' in reply, got: $2"; fi
}

# ── run ──────────────────────────────────────────────────────────────────────
prepare_data
start_host
trap 'kill -9 "$HOST_PID" 2>/dev/null || true' EXIT
login "AstrbotE2E!"

log "installing echo (grpc)"
install_git "$ECHO_REPO" grpc | grep -q '"status":"ok"' || fail "echo grpc install failed"
check_contains "echo gRPC command" "$(chat s-echo-grpc '/echo grpc-hello')" "grpc-hello"

log "installing echo (native)"
# uninstall previous echo so we can reinstall as native
EID=$(api GET /api/v1/plugins | python3 -c "import sys,json;[print(p['id']) for p in json.load(sys.stdin)['data'] if p.get('name')=='echo']" | head -1)
[ -n "$EID" ] && api DELETE "/api/v1/plugins/by-id?plugin_id=$EID" '{"delete_config":true,"delete_data":true}' >/dev/null
install_git "$ECHO_REPO" native | grep -q '"status":"ok"' || fail "echo native install failed"
check_contains "echo Native command" "$(chat s-echo-native '/echo native-hello')" "native-hello"

log "installing toolgen (go tool, native)"
install_git "$REPO_ROOT/internal/plugin/testdata/toolgen_go" native | grep -q '"status":"ok"' || fail "toolgen install failed"
if [ -n "${OPENROUTER_API_KEY:-}" ]; then
  reply=$(chat s-tool 'Call echo_tool with text=TOOLSWORK then tell me the result.')
  check_contains "Go Native LLM tool" "$reply" "TOOL_ECHO:TOOLSWORK"
else
  log "SKIP tool test (no OPENROUTER_API_KEY)"
fi

log "installing python_e2e (shared)"
install_upload "$REPO_ROOT/internal/plugin/testdata/python_e2e.zip" shared | grep -q '"status":"ok"' || fail "python shared install failed"
check_contains "Python shared command" "$(chat s-py-shared '/am_status')" "agentmemory unavailable"

log "installing python_e2e (grpc)"
PID2=$(api GET /api/v1/plugins | python3 -c "import sys,json;[print(p['id']) for p in json.load(sys.stdin)['data'] if 'agentmemory' in p.get('id','')]" | head -1)
[ -n "$PID2" ] && api DELETE "/api/v1/plugins/by-id?plugin_id=$PID2" '{"delete_config":true,"delete_data":true}' >/dev/null
install_upload "$REPO_ROOT/internal/plugin/testdata/python_e2e.zip" grpc | grep -q '"status":"ok"' || fail "python grpc install failed"
check_contains "Python grpc command" "$(chat s-py-grpc '/am_status')" "agentmemory unavailable"

log "ALL E2E CHECKS PASSED"
