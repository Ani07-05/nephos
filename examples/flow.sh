#!/usr/bin/env bash
# One full run of the nephos story against a real server:
#   an AI agent (over MCP) builds and deploys an app with server code and a secret,
#   opens it to the organization and invites a colleague by email;
#   colleagues sign in with emailed links and use it; outsiders can't.
set -euo pipefail

# An empty value beats the .env file, so this run prints emails instead of sending real ones.
export NEPHOS_SMTP=
cd "$(dirname "$0")/.."

PORT=${PORT:-8099}
DATA=$(mktemp -d)
LOG=$DATA/server.log
JAR=$DATA/jar
fails=0
ok()   { echo "ok    $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
check() { local what=$1; shift; if "$@" >/dev/null 2>&1; then ok "$what"; else fail "$what"; fi; }

go build -o "$DATA/nephos" .
"$DATA/nephos" serve -addr ":$PORT" -url "http://localhost:$PORT" -data "$DATA/data" -org-domain acme.com >"$LOG" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null; rm -rf "$DATA"' EXIT
for _ in $(seq 40); do curl -s "http://localhost:$PORT/login" >/dev/null 2>&1 && break; sleep 0.25; done

export NEPHOS_URL=http://localhost:$PORT
export NEPHOS_TOKEN=$(cat "$DATA/data/admin_token")
APP=http://standup.localhost:$PORT

echo "-- the agent works through MCP (stdio), the same way Claude Code would"
INDEX='<!doctype html><title>standup</title><h1>standup</h1><ul id=n></ul><script>
const draw = d => n.innerHTML = d.notes.map(x => `<li>${x.by}: ${x.text}</li>`).join("");
fetch("/api/").then(r => r.json()).then(draw);
</script>'
HANDLER='async function handle(req) {
  if (req.method === "POST") {
    const notes = kv.get("notes") || [];
    notes.push({ by: req.user, text: JSON.parse(req.body).text });
    kv.set("notes", notes);
  }
  return { team: env.TEAM, notes: kv.get("notes") || [] };
}'
call() { jq -nc --argjson id "$1" --arg tool "$2" --argjson args "$3" '{jsonrpc:"2.0",id:$id,method:"tools/call",params:{name:$tool,arguments:$args}}'; }
{
  echo '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  call 1 deploy_app "$(jq -nc --arg i "$INDEX" --arg h "$HANDLER" '{name:"standup",files:{"index.html":$i,"handler.js":$h}}')"
  call 2 set_env '{"name":"standup","key":"TEAM","value":"Platform"}'
  call 3 set_access '{"name":"standup","access":"org"}'
  call 4 share_app '{"name":"standup","email":"dana@acme.com"}'
  sleep 1
} | "$DATA/nephos" mcp 2>/dev/null | jq -r 'select(.id >= 1) | "agent> " + (.result.structuredContent.note // .result.content[0].text)'

mails() { grep -c '^.*mail to ' "$LOG" || true; }
last_link() { grep -o "http://standup[^ ]*join?t=[0-9a-f]*" "$LOG" | tail -1; }
sign_in() {  # $1 = email, $2 = cookie jar
  curl -s -o /dev/null -H 'Accept: text/html' -d "email=$1" "$APP/_nephos/login"; sleep 0.5
  local link; link=$(last_link)
  curl -s "$link" | grep -q "Continue as" || return 1
  curl -s -c "$2" -o /dev/null -d "t=${link##*t=}" "$APP/_nephos/join"
}

echo "-- a stranger gets nothing"
check "anonymous request is refused" test "$(curl -s -o /dev/null -w '%{http_code}' -H 'Accept: text/html' $APP/)" = 401

echo "-- dana (invited) opens the emailed link"
LINK=$(last_link)
check "link opens a confirm page, not a session" bash -c "curl -s '$LINK' | grep -q 'Continue as'"
check "opening the link twice (a mail scanner) does not burn it" bash -c "curl -s '$LINK' | grep -q 'Continue as'"
curl -s -c "$JAR.dana" -o /dev/null -d "t=${LINK##*t=}" "$APP/_nephos/join"
check "dana sees the app" bash -c "curl -s -b '$JAR.dana' $APP/ | grep -q standup"
curl -s -b "$JAR.dana" -X POST -d '{"text":"shipped the importer"}' "$APP/api/" >/dev/null
check "server code ran with the TEAM secret and dana's identity" bash -c "curl -s -b '$JAR.dana' $APP/api/ | grep -q '\"team\":\"Platform\".*dana@acme.com'"
check "the link can't be used a second time" test "$(curl -s -o /dev/null -w '%{http_code}' -d "t=${LINK##*t=}" $APP/_nephos/join)" = 410

echo "-- sam works at acme.com but was never invited"
sign_in sam@acme.com "$JAR.sam" || fail "sam's sign-in"
check "sam signs in by email and sees dana's note" bash -c "curl -s -b '$JAR.sam' $APP/api/ | grep -q 'shipped the importer'"

echo "-- eve is outside the organization"
before=$(mails)
curl -s -o /dev/null -H 'Accept: text/html' -d "email=eve@other.com" "$APP/_nephos/login"; sleep 0.5
check "no email is sent to eve" test "$(mails)" = "$before"

echo "-- the agent can undo a bad deploy"
BAD=$(jq -nc '{name:"standup",files:{"index.html":"<h1>oops</h1>","handler.js":"function handle(){ throw new Error(\"broken\") }"}}')
{
  echo '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  call 1 deploy_app "$BAD"
  sleep 0.5
} | "$DATA/nephos" mcp >/dev/null 2>&1
check "the broken version fails" test "$(curl -s -o /dev/null -w '%{http_code}' -b $JAR.sam $APP/api/)" = 500
"$DATA/nephos" rollback standup
check "after rollback sam's data is back" bash -c "curl -s -b '$JAR.sam' $APP/api/ | grep -q 'shipped the importer'"

echo
if [ "$fails" = 0 ]; then echo "all steps passed"; else echo "$fails step(s) failed"; exit 1; fi
