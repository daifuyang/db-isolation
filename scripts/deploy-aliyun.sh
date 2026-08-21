#!/usr/bin/env bash
# db-isolation Aliyun ECS deploy via pm2.
#
# This script:
#   1. Cross-compiles the linux/amd64 binary from the dev machine.
#   2. rsyncs the binary + ecosystem config to the ECS over ssh.
#   3. Reloads pm2 (graceful restart) on the ECS.
#
# Usage:
#   scripts/deploy-aliyun.sh <user@host> [ssh-port]
#
# Environment variables (with defaults):
#   DBI_REMOTE_BIN_DIR   /usr/local/bin           (where the binary lands)
#   DBI_REMOTE_ETC_DIR   /etc/db-isolation        (where config.yaml lives)
#   DBI_REMOTE_LIB_DIR   /var/lib/db-isolation    (sqlite + pm2 home)
#   DBI_REMOTE_USER      db-isolation             (pm2-managed unix user)
#   DBI_PM2_BIN          pm2                      (override if not on PATH)
#   DBI_SSH_KEY          (optional ssh identity file)
#
# Requires on the dev machine:
#   - Go toolchain able to cross-compile linux/amd64
#   - rsync
#   - ssh
#
# Requires on the ECS (one-time):
#   - bash scripts/install.sh --pm2      (creates user, dirs, drops config.yaml)
#   - /root/.my.cnf (mode 0600)          (MySQL root credentials)
#   - Node + pm2 installed system-wide or for the db-isolation user
#   - The db-isolation service already started once via:
#       sudo -u db-isolation pm2 start scripts/ecosystem.config.json --env production
#       sudo -u db-isolation pm2 save

set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <user@host> [ssh-port]" >&2
    exit 2
fi

REMOTE="${1}"
SSH_PORT="${2:-22}"

REMOTE_BIN_DIR="${DBI_REMOTE_BIN_DIR:-/usr/local/bin}"
REMOTE_ETC_DIR="${DBI_REMOTE_ETC_DIR:-/etc/db-isolation}"
REMOTE_LIB_DIR="${DBI_REMOTE_LIB_DIR:-/var/lib/db-isolation}"
REMOTE_USER="${DBI_REMOTE_USER:-db-isolation}"
PM2="${DBI_PM2_BIN:-pm2}"

SSH_OPTS=(-p "${SSH_PORT}" -o StrictHostKeyChecking=accept-new)
if [[ -n "${DBI_SSH_KEY:-}" ]]; then
    SSH_OPTS+=(-i "${DBI_SSH_KEY}")
fi

# 1. cross-compile
echo "==> Building linux/amd64 binary"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' \
    -o bin/db-isolation.linux-amd64 ./cmd/server

# Also build the CLI/MCP for human use; the server is the only thing we
# hot-reload via pm2.
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' \
    -o bin/dbi.linux-amd64 ./cmd/dbi
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' \
    -o bin/mcp.linux-amd64 ./cmd/mcp

# 2. rsync
echo "==> Uploading binary + ecosystem to ${REMOTE}"

# The ecosystem config is shipped so the operator can pm2 reload without
# having to keep an extra copy on the box.
rsync -avz --delete \
    -e "ssh ${SSH_OPTS[*]}" \
    bin/db-isolation.linux-amd64 \
    "${REMOTE}:${REMOTE_BIN_DIR}/db-isolation.new"

# IMPORTANT: ecosystem must be a .json file (see ecosystem.config.md).
# pm2 v7 picks the interpreter from the file extension and will refuse
# to fork a Go binary if the ecosystem ends in .js / .cjs.
rsync -avz \
    -e "ssh ${SSH_OPTS[*]}" \
    scripts/ecosystem.config.json \
    "${REMOTE}:${REMOTE_LIB_DIR}/ecosystem.config.json"

# 3. atomic swap + reload
echo "==> Reloading via pm2"
ssh "${SSH_OPTS[@]}" "${REMOTE}" "
    set -e
    # Replace the running binary atomically. 'install -m' does the copy
    # + chmod in one shot.
    install -m 0755 ${REMOTE_BIN_DIR}/db-isolation.new ${REMOTE_BIN_DIR}/db-isolation
    rm -f ${REMOTE_BIN_DIR}/db-isolation.new

    # pm2 reload picks up the new binary path (the script path is the
    # same; only the file content changed) and triggers a graceful
    # restart. The process keeps the same id.
    sudo -u ${REMOTE_USER} \
        HOME=${REMOTE_LIB_DIR} \
        PM2_HOME=${REMOTE_LIB_DIR}/.pm2 \
        ${PM2} reload ecosystem.config.json --env production

    # Show the running state so the operator can verify.
    sudo -u ${REMOTE_USER} \
        HOME=${REMOTE_LIB_DIR} \
        PM2_HOME=${REMOTE_LIB_DIR}/.pm2 \
        ${PM2} status
"

echo "==> Done."
echo "Verify with:"
echo "  curl http://127.0.0.1:8787/health     # from the box itself"
echo "  dbi --url http://<host>:8787 list     # via nginx proxy if configured"