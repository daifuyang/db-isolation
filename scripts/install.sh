#!/usr/bin/env bash
# db-isolation install helper.
#
# Two modes:
#
#   bash scripts/install.sh                # systemd mode (default)
#   bash scripts/install.sh --pm2          # pm2 mode (no systemd unit)
#
# Idempotent. Sets up:
#   - /etc/db-isolation (root-only config + secrets directory)
#   - /var/lib/db-isolation (sqlite + state)
#   - /var/log/db-isolation (audit + pm2 logs)
#   - the db-isolation system user if missing
#
# It does NOT configure MySQL itself — that is the operator's responsibility.

set -euo pipefail

DBI_USER="db-isolation"
DBI_GROUP="db-isolation"
ETC_DIR="/etc/db-isolation"
LIB_DIR="/var/lib/db-isolation"
LOG_DIR="/var/log/db-isolation"
SECRETS_DIR="${ETC_DIR}/apps"
BIN="/usr/local/bin/db-isolation"

MODE="systemd"
if [[ "${1:-}" == "--pm2" ]]; then
    MODE="pm2"
fi

# 1. system user
if ! id -u "${DBI_USER}" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "${DBI_USER}"
fi

# 2. directories
mkdir -p "${ETC_DIR}" "${LIB_DIR}" "${LOG_DIR}" "${SECRETS_DIR}"
chown root:root "${ETC_DIR}"
chmod 0750 "${ETC_DIR}"

chown -R "${DBI_USER}:${DBI_GROUP}" "${LIB_DIR}" "${LOG_DIR}" "${SECRETS_DIR}"
chmod 0750 "${SECRETS_DIR}" "${LOG_DIR}"
chmod 0750 "${LIB_DIR}"

# 3. config file (only if missing)
if [[ ! -f "${ETC_DIR}/config.yaml" ]]; then
    cat >"${ETC_DIR}/config.yaml" <<'YAML'
server:
  addr: 127.0.0.1:8787
  shutdown_timeout_seconds: 10
storage:
  path: /var/lib/db-isolation/db-isolation.db
mysql:
  admin_config_path: /root/.my.cnf
  allow_drop: false
secrets:
  dir: /etc/db-isolation/apps
audit:
  to_file: true
  file: /var/log/db-isolation/audit.log
logging:
  level: info
YAML
    chmod 0640 "${ETC_DIR}/config.yaml"
fi

# 4. env file (only if missing) — read by systemd; ignored by pm2.
if [[ ! -f "${ETC_DIR}/db-isolation.env" ]]; then
    cat >"${ETC_DIR}/db-isolation.env" <<'ENV'
# db-isolation runtime environment.
# Uncomment to override defaults; everything else comes from config.yaml.
# DB_ISOLATION_ADDR=127.0.0.1:8787
# MYSQL_ADMIN_DSN=
ENV
    chmod 0640 "${ETC_DIR}/db-isolation.env"
fi

# 5. mode-specific bits
case "${MODE}" in
    systemd)
        if [[ -f scripts/db-isolation.service ]]; then
            install -m 0644 scripts/db-isolation.service /etc/systemd/system/db-isolation.service
            systemctl daemon-reload
        fi
        echo "Installed (systemd mode)."
        echo "  systemctl enable --now db-isolation"
        echo "  /usr/local/bin/db-isolation token create --name local-admin"
        ;;
    pm2)
        # pm2 needs to write its log files into the log directory.
        chmod 0750 "${LOG_DIR}"
        chown "${DBI_USER}:${DBI_GROUP}" "${LOG_DIR}"
        touch "${ETC_DIR}/.pm2-mode"
        echo "Installed (pm2 mode)."
        echo "  sudo -u ${DBI_USER} pm2 start scripts/ecosystem.config.json --env production"
        echo "  sudo -u ${DBI_USER} pm2 save"
        echo "  # generate systemd unit that re-launches pm2 on boot:"
        echo "  sudo -u ${DBI_USER} pm2 startup systemd -u ${DBI_USER} --hp /var/lib/db-isolation \\"
        echo "       | sudo bash"
        ;;
    *)
        echo "unknown mode: ${MODE}" >&2
        exit 2
        ;;
esac