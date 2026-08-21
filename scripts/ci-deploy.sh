#!/bin/bash
#
# /usr/local/bin/ci-deploy.sh
#
# The ONLY command CI can run over ssh. Invoked by sshd's
# ForceCommand in /home/ci-deployer/.ssh/authorized_keys.
#
# Signature (passed by ForceCommand from SSH_ORIGINAL_COMMAND):
#     deploy <ref> <local-path> <sha256-hex>
#       ref         git ref to deploy (must be "main")
#       local-path  path on the host to a binary that was scp'd here
#       sha256-hex  expected sha256 of that binary
#
# Hardening:
#   - ci-deployer has no shell access (force-command replaces every ssh
#     call with this script).
#   - Only "main" ref is deployable.
#   - The local-path must be inside /tmp (so ci-deployer cannot trick us
#     into reading /etc/db-isolation/admin.cnf or anything outside the
#     staging directory).
#   - Every step writes to /var/log/ci-deploy.log.
#
# Companion authorized_keys entry:
#     command="/usr/local/bin/ci-deploy.sh $SSH_ORIGINAL_COMMAND 2>&1",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty ssh-ed25519 ...

set -euo pipefail

REF="${1:-}"
SRC="${2:-}"
EXPECTED_SHA="${3:-}"
ME="$(id -un)"
AUDIT_LOG=/var/log/ci-deploy.log
BIN=/usr/local/bin/db-isolation
BAK="${BIN}.bak-$(date +%Y%m%d-%H%M%S)"

log() {
    printf '[%s] [ci-deploy] %s ref=%s src=%s user=%s\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "${REF:-?}" "${SRC:-?}" "$ME" \
        >>"$AUDIT_LOG"
}

# --- pre-flight ----------------------------------------------------------

if [[ "$ME" != "ci-deployer" && "$ME" != "root" ]]; then
    log "refused: invoked as $ME"
    echo "forbidden: this script is restricted to ci-deployer / root" >&2
    exit 2
fi

if [[ "$REF" != "main" ]]; then
    log "refused: ref '$REF' is not main"
    echo "forbidden: only main can be deployed" >&2
    exit 2
fi

# Restrict SRC to /tmp — ci-deployer cannot stage files elsewhere.
if [[ -z "$SRC" || "$SRC" != /tmp/* ]]; then
    log "refused: src '$SRC' is not under /tmp"
    echo "src must be under /tmp" >&2
    exit 2
fi

if [[ ! -f "$SRC" ]]; then
    log "refused: src '$SRC' not found"
    echo "src file not found" >&2
    exit 2
fi

if [[ ! "$EXPECTED_SHA" =~ ^[a-f0-9]{64}$ ]]; then
    log "refused: sha256 not 64 hex chars: $EXPECTED_SHA"
    echo "sha256 must be 64 hex chars" >&2
    exit 2
fi

mkdir -p "$(dirname "$AUDIT_LOG")"

# --- verify + swap -------------------------------------------------------

ACTUAL_SHA=$(sudo /usr/bin/sha256sum "$SRC" | awk '{print $1}')

if [[ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]]; then
    log "failed: sha256 mismatch expected=$EXPECTED_SHA actual=$ACTUAL_SHA"
    echo "sha256 mismatch" >&2
    exit 3
fi

log "start sha=$ACTUAL_SHA"

# Roll the old binary forward. Sudo is allowed via /etc/sudoers.d/ci-deployer.
sudo /usr/bin/cp "$BIN" "$BAK"
sudo /usr/bin/install -m 0755 "$SRC" "$BIN"

# Clean up the staged file.
rm -f "$SRC"

# Stop pm2 (run as the service user), restart with the new binary.
sudo -u db-isolation \
    HOME=/var/lib/db-isolation PM2_HOME=/var/lib/db-isolation/.pm2 \
    /usr/local/bin/db-isolation-pm2 stop db-isolation >/dev/null 2>&1 || true

sudo -u db-isolation \
    HOME=/var/lib/db-isolation PM2_HOME=/var/lib/db-isolation/.pm2 \
    /usr/local/bin/db-isolation-pm2 start \
        /var/lib/db-isolation/ecosystem.config.json \
        --env production >/dev/null

# --- health check --------------------------------------------------------

sleep 2
HEALTH=$(sudo /usr/bin/curl -fsS --max-time 5 http://127.0.0.1:8787/health 2>&1) || {
    log "failed: health check did not respond"
    echo "health check failed" >&2
    exit 4
}

# --- cert renewal (idempotent) ------------------------------------------

sudo /root/.acme.sh/acme.sh --renew -d db.zerocmf.com \
    --reloadcmd "/usr/bin/systemctl reload nginx" \
    >/dev/null 2>&1 || log "warn: cert renewal step exited non-zero"

sudo /usr/bin/systemctl reload nginx 2>/dev/null || log "warn: nginx reload failed"

log "ok health=$HEALTH"
echo "deployed $REF sha=$ACTUAL_SHA"
