#!/usr/bin/env bash
# Red-team run against a throwaway nephos server: each line is an attack, and "blocked"
# means the control held. Nothing here sends real email.
set -uo pipefail
cd "$(dirname "$0")/.."

# An empty value beats the .env file, so emails are printed to the log instead of sent.
export NEPHOS_SMTP=

PORT=${PORT:-8098}
DATA=$(mktemp -d)
LOG=$DATA/server.log
bad=0
blocked() { printf 'blocked   %s\n' "$1"; }
VULNERABLE() { printf 'VULNERABLE  %s\n' "$1"; bad=$((bad + 1)); }
expect() { local what=$1; shift; if "$@" >/dev/null 2>&1; then blocked "$what"; else VULNERABLE "$what"; fi; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

go build -o "$DATA/nephos" . || exit 1
"$DATA/nephos" serve -addr ":$PORT" -url "http://localhost:$PORT" -data "$DATA/data" >"$LOG" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null; rm -rf "$DATA"' EXIT
for _ in $(seq 40); do curl -s "http://localhost:$PORT/login" >/dev/null 2>&1 && break; sleep 0.25; done

TOKEN=$(cat "$DATA/data/admin_token")
export NEPHOS_URL=http://localhost:$PORT NEPHOS_TOKEN=$TOKEN
ROOT=http://localhost:$PORT
APP=http://vault.localhost:$PORT
OTHER=http://other.localhost:$PORT
AUTH=(-H "Authorization: Bearer $TOKEN")

mkdir -p "$DATA/vault" "$DATA/probe"
echo '<h1>secret notes</h1>' >"$DATA/vault/index.html"
cp "$DATA/vault/index.html" "$DATA/probe/index.html"
cat >"$DATA/probe/handler.js" <<'EOF'
async function handle(req) {
  if (req.query.spin) { while (true) {} }
  const seen = [typeof os, typeof std, typeof fetch, typeof process, typeof require].join(',');
  let files = 'error';
  try { const m = await import('os'); files = JSON.stringify(m.readdir('.')); } catch (e) {}
  return { seen, files };
}
EOF
"$DATA/nephos" deploy "$DATA/vault" vault >/dev/null
"$DATA/nephos" deploy "$DATA/probe" probe >/dev/null
"$DATA/nephos" deploy "$DATA/vault" other >/dev/null
PROBE=http://probe.localhost:$PORT

echo "== access control"
expect "stranger opens a private app" test "$(code -H 'Accept: text/html' $APP/)" = 401
expect "stranger calls an app's API" test "$(code $APP/_nephos/kv)" = 401
expect "deploying without the admin token" test "$(code -X POST $ROOT/deploy?name=evil)" = 401
expect "admin dashboard without the token" test "$(code $ROOT/)" = 401

echo "== sign-in links"
LINK=$("$DATA/nephos" share vault dana@example.com | tail -1 | tr -d ' ')
T=${LINK##*t=}
expect "mail scanner opening the link does not sign anyone in" bash -c "[ \$(curl -s -D- -o /dev/null '$LINK' | grep -ci '^set-cookie') = 0 ]"
curl -s -o /dev/null "$LINK"
curl -s -c "$DATA/dana" -o /dev/null -d "t=$T" $APP/_nephos/join
expect "the link signs in once" test "$(code -b "$DATA/dana" $APP/_nephos/me)" = 200
expect "reusing the same link" test "$(code -d "t=$T" $APP/_nephos/join)" = 410
expect "a made-up link" test "$(code -d "t=0000000000000000" $APP/_nephos/join)" = 410
expect "tokens are stored hashed, not in plaintext" bash -c "! grep -q '$T' '$DATA/data/auth.json'"

echo "== who has access"
known=$(curl -s -H 'Accept: text/html' -d 'email=dana@example.com' $APP/_nephos/login | sed 's/dana@example.com/EMAIL/g')
unknown=$(curl -s -H 'Accept: text/html' -d 'email=ghost@example.com' $APP/_nephos/login | sed 's/ghost@example.com/EMAIL/g')
expect "sign-in page reveals who is on the list" test "$known" = "$unknown"
sleep 0.5
expect "no email goes to an address without access" bash -c "! grep -q 'mail to ghost@example.com' '$LOG'"
expect "flooding sign-in emails (rate limit)" bash -c "for i in 1 2 3 4; do c=\$(curl -s -o /dev/null -w '%{http_code}' -d 'email=flood@example.com' $APP/_nephos/login); done; [ \$c = 429 ]"

echo "== web attacks"
expect "sign-in form posted from another site (CSRF)" test "$(code -H 'Origin: https://evil.example' -d 'email=dana@example.com' $APP/_nephos/login)" = 403
expect "sign-in form with a null origin" test "$(code -H 'Origin: null' -d 'email=dana@example.com' $APP/_nephos/login)" = 403
COOKIE=$(awk '$6=="nephos"{print $7}' "$DATA/dana")
expect "one app's session used on another app" test "$(code -H "Cookie: nephos=$COOKIE" $OTHER/_nephos/me)" = 401
expect "a person's session used on the admin dashboard" test "$(code -H "Cookie: nephos=$COOKIE" $ROOT/)" = 401

echo "== upload and settings"
python3 - "$DATA/evil.zip" <<'EOF'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("index.html", "<h1>x</h1>")
    z.writestr("../escaped.txt", "written outside the app")
EOF
expect "zip that writes outside its folder (zip-slip)" bash -c "[ \$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer $TOKEN' --data-binary @'$DATA/evil.zip' '$ROOT/deploy?name=evil') -ge 400 ] && [ ! -e '$DATA/data/apps/evil/escaped.txt' ] && [ ! -e '$DATA/data/apps/escaped.txt' ]"
"$DATA/nephos" env vault DB_PASSWORD=hunter2-correct-horse >/dev/null
APPS=$(curl -s "${AUTH[@]}" $ROOT/api/apps)
expect "reading a secret back through the API" bash -c "echo '$APPS' | grep -q DB_PASSWORD && ! echo '$APPS' | grep -q hunter2"

echo "== running untrusted code"
OWNER=$(curl -s -D- -o /dev/null "${AUTH[@]}" $ROOT/open/probe | grep -i '^location:' | cut -d' ' -f2 | tr -d '\r')
curl -s -c "$DATA/owner" -o /dev/null "$OWNER"
OUT=$(curl -s -b "$DATA/owner" $PROBE/api/)
expect "app code reaching os, std, fetch, process or require" bash -c "echo '$OUT' | grep -q '\"seen\":\"undefined,undefined,undefined,undefined,undefined\"'"
expect "app code listing the server's files" bash -c "! echo '$OUT' | grep -q -e admin_token -e go.mod -e data"
expect "an infinite loop taking the server down" bash -c "[ \$(curl -s -o /dev/null -m 8 -w '%{http_code}' -b '$DATA/owner' '$PROBE/api/?spin=1') = 504 ] && [ \$(curl -s -o /dev/null -m 5 -w '%{http_code}' -b '$DATA/owner' $PROBE/api/) = 200 ]"

echo "== AI agent"
TOOLS=$( {
  echo '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
  sleep 0.7
} | "$DATA/nephos" mcp 2>/dev/null | jq -r 'select(.id==1) | [.result.tools[].name] | join(",")')
echo "          agent tools: $TOOLS"
expect "agent deleting apps or reading secrets" bash -c "! echo '$TOOLS' | grep -q -e delete -e get_env -e read"

echo "== removing someone"
"$DATA/nephos" share vault dana@example.com >/dev/null
curl -s -o /dev/null -X POST "${AUTH[@]}" -H 'Accept: application/json' -d 'email=dana@example.com' $ROOT/apps/vault/revoke
expect "a removed person's open session" test "$(code -b "$DATA/dana" $APP/_nephos/me)" = 401

echo
if [ "$bad" = 0 ]; then echo "every attack was blocked"; else echo "$bad attack(s) got through"; exit 1; fi
