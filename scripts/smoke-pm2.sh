#!/usr/bin/env bash
# Smoke test the locally-running pm2-managed db-isolation service.
#
# Run from the box where the service is deployed. Assumes:
#   - pm2 is on PATH (sudo -u db-isolation pm2 status works)
#   - DB_ISOLATION_URL=http://127.0.0.1:8787 is reachable
#   - a token has been created (DB_ISOLATION_TOKEN set in env)
#
# What it does:
#   1. Confirms pm2 reports the process as 'online'.
#   2. Calls /health.
#   3. Issues a POST /v1/databases for a unique name, then a DELETE.
#   4. Confirms the audit log contains the entries.
#
# Exits non-zero on any failure so you can wire it into a deploy hook.

set -euo pipefail

URL="${DB_ISOLATION_URL:-http://127.0.0.1:8787}"
TOKEN="${DB_ISOLATION_TOKEN:?set DB_ISOLATION_TOKEN}"
PM2="${DBI_PM2_BIN:-pm2}"

err() { echo "FAIL: $*" >&2; exit 1; }
ok()  { echo " ok: $*"; }

# 1. pm2 status
echo "==> pm2 status"
${PM2} show db-isolation >/dev/null 2>&1 || err "pm2 process 'db-isolation' not found"
status=$(${PM2} jlist | grep -o '"status":"[^"]*"' | head -1 || true)
[[ "${status}" == '"status":"online"' ]] || err "pm2 status is not online (got ${status})"
ok "pm2 reports online"

# 2. health
echo "==> GET /health"
body=$(curl --fail --silent --max-time 5 "${URL}/health")
[[ "${body}" == *'"status":"ok"'* ]] || err "health body unexpected: ${body}"
ok "health ok"

# 3. create + delete
NAME="smoke-$(date +%s)"
echo "==> POST /v1/databases ${NAME}"
create_resp=$(curl --fail --silent --max-time 10 \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${NAME}\"}" \
    "${URL}/v1/databases")
[[ "${create_resp}" == *"\"name\":\"${NAME}\""* ]] || err "create response missing name: ${create_resp}"
[[ "${create_resp}" != *"password"* ]] || err "create response leaked password field"
ok "create ok"

echo "==> DELETE /v1/databases/${NAME}"
curl --fail --silent --max-time 10 \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -X DELETE \
    -d "{\"confirm\":\"${NAME}\"}" \
    "${URL}/v1/databases/${NAME}" >/dev/null
ok "delete ok"

# 4. audit dump — confirm the actions were recorded.
echo "==> audit log"
audit=$(db-isolation audit list --limit 20 2>/dev/null || \
        /usr/local/bin/db-isolation audit list --limit 20)
[[ "${audit}" == *"database.create"* ]] || err "audit log missing database.create"
[[ "${audit}" == *"database.delete"* ]] || err "audit log missing database.delete"
ok "audit recorded both operations"

echo
echo "PASS: pm2-managed db-isolation smoke test"