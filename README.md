# db-isolation

> A minimal, audit-friendly MySQL provision gateway for a single Linux
> box (laptop, dev VM, ECS, anywhere with MySQL) that AI coding agents
> (Codex, Claude Code, MCP clients) and humans can call over HTTP.
> **AI never sees MySQL root.** Each project gets its own database, its
> own user, and its own secret file — provisioned and rotated
> server-side.

```
┌─────────────┐   ┌─────────────┐   ┌─────────────┐   ┌──────────┐   ┌──────────┐
│ CLI / MCP   │ → │  HTTP API   │ → │   Auth      │ → │ Provision│ → │  MySQL   │
│  (no MySQL) │   │ 127.0.0.1   │   │ Bearer/HMAC │   │ Service  │   │ 127.0.0.1│
└─────────────┘   └─────────────┘   └─────────────┘   └──────────┘   └──────────┘
                                                                  ↘ SQLite
                                                                  (metadata +
                                                                   audit log)
```

## Why this exists

On a single-ECS deployment, it is tempting to give every project the
same MySQL user or, worse, hand the root DSN to a coding agent. Both are
how databases leak. db-isolation is a thin server-side boundary that:

* gives every project a dedicated database + user, restricted to its own schema;
* never returns the user password (applications read it from a 0600 file);
* never returns the root DSN to any client, ever;
* records every create / rotate / delete to an immutable audit log;
* is callable from a CLI, an HTTP API, and an MCP adapter, all of which
  are deliberately thin.

The MVP is intentionally small: one server binary, one CLI binary, one
MCP adapter binary, no UI, no IAM, no multi-tenant anything.

## What is in scope

| Capability               | Endpoint / Command              |
| ------------------------ | -------------------------------- |
| Health check             | `GET /health`                    |
| List databases           | `GET /v1/databases`              |
| Show one database        | `GET /v1/databases/{name}`       |
| Create database (idempotent) | `POST /v1/databases`          |
| Rotate password          | `POST /v1/databases/{name}/rotate` |
| Delete database (needs confirm) | `DELETE /v1/databases/{name}` |
| Create / list / revoke tokens | `db-isolation token ...`     |
| Read recent audit log    | `db-isolation audit list`        |

Each project's MySQL account is created with the minimum privileges
needed to run an application against its own database:

```sql
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES
  ON `<project>_db`.* TO `<project>_user`@'127.0.0.1';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES
  ON `<project>_db`.* TO `<project>_user`@'localhost';
```

`DROP` is added only if `mysql.allow_drop: true` in config — required by
some ORM migrations.

## What is **not** in scope (deliberately)

* No multi-tenant IAM, no teams, no orgs, no RBAC.
* No fine-grained PAT scopes, no project-bound tokens.
* No OAuth / OIDC / JWT / TTL tokens — a single admin token is enough for
  the MVP.
* No Policy Engine, no approval workflow.
* No Web UI. No Postgres / Redis. No Vault integration.
* No Kubernetes, no cloud APIs, no DNS / TLS termination.
* No `execute_sql`, no `execute_shell`, no `read_secret` MCP tool.

## Repository layout

```
cmd/
  server/      db-isolation binary (HTTP API + token admin + audit dump)
  dbi/         dbi CLI client (calls HTTP API only)
  mcp/         db-isolation MCP adapter (stdin/stdout JSON-RPC)
internal/
  api/         HTTP handlers, error envelope, auth middleware
  audit/       SQLite + JSONL audit writer
  auth/        Bearer token + SHA-256 verifier
  config/      YAML + env loader, my.cnf parser
  model/       DatabaseProject, Token, AuditLog, name validation
  mysqlx/      Provisioner — the only place that talks to MySQL as admin
  provision/   Create / rotate / delete workflow
  secrets/     Atomic 0600 DATABASE_URL writer
  store/       SQLite metadata store
config.example.yaml
.env.example
Makefile
scripts/
  db-isolation.service
  install.sh
```

## Security model

| Layer                     | Property                                         |
| ------------------------- | ------------------------------------------------ |
| AI / CLI / MCP            | Sees only `name`, `database`, `user`, `status`, `secret_path`. Never sees passwords or root. |
| HTTP server               | Listens on `127.0.0.1` by default; Bearer auth required for every `/v1/*` route except `/health`. |
| MySQL                     | Must be `bind-address = 127.0.0.1`. Project users can only reach their own database and only from localhost. |
| Secret files              | `/etc/db-isolation/apps/<name>.env`, mode `0600`, owned by the service account. |
| Audit log                 | SQLite + JSONL mirror. Never records tokens, passwords, DSNs. |
| Tokens                    | Server stores only SHA-256 hash; plaintext shown once at creation. |

MySQL root credentials live in only two places:

1. `/root/.my.cnf` on the server (mode `0600`), or
2. the `MYSQL_ADMIN_DSN` environment variable (set in
   `/etc/db-isolation/db-isolation.env`, mode `0640`).

The server binary reads them at boot, parses them with the official
Go MySQL driver, and never persists them. The CLI / MCP binaries never
see them.

## Quick start

### 1. Build

```bash
make build
```

This produces:

```
bin/db-isolation      # server + token admin
bin/dbi               # CLI client
bin/mcp               # MCP adapter
```

### 2. Configure MySQL

In `/etc/mysql/mysql.conf.d/mysqld.cnf`:

```ini
[mysqld]
bind-address = 127.0.0.1
```

Verify:

```bash
ss -lntp | grep 3306
# 127.0.0.1:3306
```

Put the admin password on disk:

```ini
# /root/.my.cnf
[client]
user=root
password=YOUR_ROOT_PASSWORD
host=127.0.0.1
```

```bash
chmod 600 /root/.my.cnf
```

### 3. Install the service

```bash
sudo make install-svc
```

This copies the binaries into `/usr/local/bin`, drops the systemd unit
into `/etc/systemd/system/`, and creates the runtime directories:

```
/etc/db-isolation/
├── config.yaml             # editable
├── api.env                  # EnvironmentFile= (optional overrides)
└── apps/                   # per-project secret files (0600)
/var/lib/db-isolation/      # SQLite database
/var/log/db-isolation/      # audit logs
```

The unit runs as user `db-isolation` (system user, no shell). The
`apps/` directory is the only thing the unit can write that holds
secrets.

### 4. Start

```bash
sudo systemctl enable --now db-isolation
sudo systemctl status db-isolation
sudo journalctl -u db-isolation -n 50
```

### 5. Create the first admin token

```bash
sudo db-isolation token create --name local-admin
```

Sample output:

```
time=2026-01-01T00:00:00.000Z level=INFO msg="token created" name=local-admin
Token created. This token will only be shown once.

dbi_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789

Save it now. Store as DB_ISOLATION_TOKEN.
```

The token is shown exactly once. The server only stores its SHA-256 hash.

### 6. Provision a project

```bash
export DB_ISOLATION_URL=http://127.0.0.1:8787
export DB_ISOLATION_TOKEN=dbi_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789

dbi create opcos
```

Output:

```
✓ Project: opcos
✓ Database: opcos_db
✓ User: opcos_user
✓ Secret: /etc/db-isolation/apps/opcos.env
✓ Status: ready
```

The CLI never shows the password. The application reads it from
`/etc/db-isolation/apps/opcos.env`:

```bash
sudo cat /etc/db-isolation/apps/opcos.env
# DATABASE_URL=mysql://opcos_user:...@127.0.0.1:3306/opcos_db
# DB_HOST=127.0.0.1
# DB_PORT=3306
# DB_NAME=opcos_db
# DB_USER=opcos_user
# DB_PASSWORD=...
```

### 7. Verify isolation

```bash
mysql -u opcos_user -p -h 127.0.0.1 opcos_db
```

`opcos_user` can SELECT / INSERT / UPDATE / DELETE / CREATE / ALTER / INDEX /
REFERENCES on `opcos_db.*`. It cannot:

* `USE mysql` — fails with "Access denied".
* `CREATE USER ...` — fails with "Access denied".
* `DROP DATABASE opcos_db` — fails (unless `allow_drop: true`).

### 8. Rotate credentials

```bash
dbi rotate opcos
```

This regenerates the password, applies it to MySQL, and rewrites the
secret file atomically (write-temp → fsync → rename). The CLI does not
return the new password.

### 9. Delete a database

```bash
dbi delete opcos            # refused: confirmation required
dbi delete opcos --confirm wrong   # refused: confirmation mismatch
dbi delete opcos --confirm opcos   # ok
```

Server-side, the service drops the database, drops the user, deletes the
secret file, and records the audit entry — in that order. Any partial
failure is reported in the audit log.

## HTTP API

All `/v1/*` routes require `Authorization: Bearer dbi_xxx`. Errors are
emitted as:

```json
{
  "error": {
    "code": "DATABASE_NOT_FOUND",
    "message": "database project not found"
  }
}
```

Stable error codes:

| Code                     | When                                      |
| ------------------------ | ----------------------------------------- |
| `UNAUTHORIZED`           | Missing or invalid Bearer token           |
| `INVALID_PROJECT_NAME`   | Project name fails validation             |
| `DATABASE_NOT_FOUND`     | `GET`/`DELETE` on a missing project       |
| `DATABASE_ALREADY_EXISTS` | Non-idempotent conflict (not currently emitted) |
| `MYSQL_ERROR`            | MySQL admin DDL/DCL failed                |
| `SECRET_WRITE_ERROR`     | Atomic secret write failed                |
| `CONFIRMATION_REQUIRED`  | Delete without confirm / wrong confirm    |
| `INTERNAL_ERROR`         | Anything else; the underlying cause is logged server-side and never returned to the client |

### Idempotency for create

`POST /v1/databases { "name": "opcos" }` returns:

* `201 Created` if the project did not exist;
* `200 OK` with the existing record if the project already exists and is
  `ready`.

This means repeated `dbi create opcos` calls always converge to the
same outcome.

## dbi CLI

```
dbi [--url URL] [--token TOKEN] [--json] <command> [args]

Commands:
  list
  status <name>
  create <name>
  rotate <name>
  delete <name> --confirm <name>
```

Defaults:

* `--url`   → `http://127.0.0.1:8787` (env: `DB_ISOLATION_URL`)
* `--token` → empty; required (env: `DB_ISOLATION_TOKEN`)

The CLI never speaks MySQL directly. It always calls the HTTP API.

## MCP adapter

The MCP adapter is a stdio JSON-RPC 2.0 server. Configure your AI client
to launch the binary:

```json
{
  "mcpServers": {
    "db-isolation": {
      "command": "/usr/local/bin/db-isolation-mcp",
      "args": [],
      "env": {
        "DB_ISOLATION_URL": "http://127.0.0.1:8787",
        "DB_ISOLATION_TOKEN": "dbi_xxx"
      }
    }
  }
}
```

Exposed tools:

| Tool                | Notes                                            |
| ------------------- | ------------------------------------------------ |
| `database_list`     | No args. Returns names + status.                |
| `database_status`   | `{ name }`                                       |
| `database_create`   | `{ name }`                                       |
| `database_rotate`   | `{ name }`                                       |
| `database_delete`   | `{ name, confirm }` — `confirm` must equal `name`. The tool description states this is destructive. |

There is no `execute_sql`, no `execute_shell`, no `mysql_root`, no
`read_secret` tool. The MCP process cannot read secret files; it only
calls the HTTP API.

## Token administration

```bash
db-isolation token create --name local-admin       # one-time display
db-isolation token list                            # id, name, created, last_used, revoked
db-isolation token revoke --id 1                   # immediate
```

Token format: `dbi_<24 random bytes, base64url>`. Only the SHA-256 hash
is persisted.

## Audit log

* Stored in `/var/lib/db-isolation/db-isolation.db` (SQLite).
* Mirrored as JSON lines to `/var/log/db-isolation/audit.log` when
  `audit.to_file: true`.

Inspect:

```bash
db-isolation audit list --limit 50
```

Each entry has:

```
timestamp
action             (database.create, database.rotate, database.delete,
                    auth.success, auth.failure)
resource           (database, token)
resource_name
success            (bool)
message
remote_ip
```

The audit layer never includes:

* MySQL root password
* Project DB password
* The full `DATABASE_URL`
* The Bearer token

## Tests

```bash
make test
```

The default suite runs with `-race`. It covers:

* Project-name validation (allow-list + length, rejects `../../../etc`,
  `opcos;DROP`, backticks, spaces, uppercase, leading hyphens, etc.)
* Token hashing + verification + revoke semantics
* Create / list / get idempotency
* Concurrent create (race-tested)
* Delete confirmation (missing / wrong / correct)
* API responses never include `password`, `db_password`, or
  `database_url` keys
* Secret-file atomic write + permission enforcement
* CLI end-to-end against a real `httptest.Server` (auth header, output
  format, JSON mode)
* MCP server tools + JSON-RPC dispatcher
* Audit log writes to both SQLite and JSONL

Integration tests that exercise MySQL DDL/DCL directly are skipped
unless `DB_ISOLATION_TEST_MYSQL_DSN` is set:

```bash
DB_ISOLATION_TEST_MYSQL_DSN='root@tcp(127.0.0.1:3306)/' make itest
```

## Operations notes

### MySQL hardening

* `bind-address = 127.0.0.1` — required.
* Disable `LOAD DATA LOCAL INFILE` in `my.cnf` (`local-infile=0`).
* Set `validate_password_policy` to `STRONG`.
* Rotate the root password on a regular cadence.

### Secret file hygiene

* Directory `/etc/db-isolation/apps/` is created `0750` and owned by
  the service user.
* Each `.env` file is written `0600`.
* Run `auditd` on `/etc/db-isolation` if you want file-write
  notifications.

### Backup

The SQLite database lives in `/var/lib/db-isolation/db-isolation.db`.
For nightly backups, simply copy the file while the service is running —
SQLite's WAL mode keeps it consistent.

## Development

```bash
make build    # produce ./bin/{db-isolation,dbi,mcp}
make test     # unit + integration (skips MySQL if no DSN)
make itest    # MySQL integration only
make vet      # go vet ./...
make fmt      # go fmt ./...
```

To run the server locally without MySQL for endpoint testing:

```bash
mkdir -p /tmp/dbi-smoke
cat >/tmp/dbi-smoke/config.yaml <<EOF
server: { addr: 127.0.0.1:8787 }
storage: { path: /tmp/dbi-smoke/dbi.db }
secrets: { dir: /tmp/dbi-smoke/apps }
audit: { to_file: false }
EOF

# create token (uses our loader, which respects --config anywhere on the line)
./bin/db-isolation token create --name local-admin --config /tmp/dbi-smoke/config.yaml
# → dbi_xxx

export DB_ISOLATION_TOKEN=<paste above>
MYSQL_ADMIN_DSN='root@tcp(127.0.0.1:3306)/' ./bin/db-isolation server \
  --config /tmp/dbi-smoke/config.yaml
```


## Deploying

The service has two process managers. Pick one:

| Mode | When to use |
| ---- | ----------- |
| **systemd** (`scripts/db-isolation.service`) | Default. Single binary managed by the OS init system. Best for most boxes. |
| **pm2** (`scripts/ecosystem.config.json`) | When you already use pm2 on the box, want log rotation, or want a web UI. |

Both modes install to the same paths:

```
/usr/local/bin/
  db-isolation       server binary (HTTP API + token admin + audit)
  dbi                CLI client (calls the HTTP API)
  db-isolation-mcp   MCP stdio adapter (5 tools, no SQL/shell)

/etc/db-isolation/
  config.yaml        root:db-isolation 0640, read by the service
  admin.cnf          root:db-isolation 0640, MySQL admin DSN
  apps/              db-isolation 0750
    <name>.env       db-isolation 0600, per-project DATABASE_URL

/var/lib/db-isolation/
  db-isolation.db    SQLite (projects, tokens, audit; WAL mode)
  .pm2/              pm2 state — only when using pm2 mode
  ecosystem.config.json

/var/log/db-isolation/
  audit.log          JSONL mirror of every audit row
  pm2.*.log             only when using pm2 mode

/root/.my.cnf        0600, alternative place to keep the admin password
```

### Install (systemd)

```bash
make build
sudo make install install-svc
sudo systemctl enable --now db-isolation
sudo systemctl status db-isolation
```

`make install` copies the three binaries to `/usr/local/bin/`. `make install-svc`
installs the systemd unit, creates the `db-isolation` system user, and
lays out the directories above.

### Install (pm2)

```bash
# 1. install Node + pm2 (one-time per box)
curl -fsSL https://rpm.nodesource.com/setup_20.x | sudo bash -
sudo yum install -y nodejs   # or apt-get
sudo npm install -g pm2

# 2. install the service (creates user, dirs, drops config.yaml)
sudo bash scripts/install.sh --pm2

# 3. start
sudo -u db-isolation \
    HOME=/var/lib/db-isolation \
    PM2_HOME=/var/lib/db-isolation/.pm2 \
    pm2 start scripts/ecosystem.config.json --env production

# 4. survive reboots
sudo -u db-isolation pm2 save
sudo env PM2_HOME=/var/lib/db-isolation/.pm2 HOME=/var/lib/db-isolation \
    -u db-isolation \
    pm2 startup systemd -u db-isolation --hp /var/lib/db-isolation \
    | sudo bash
```

After step 4, systemd re-launches pm2 on boot, and pm2 resurrects
the saved process list.

#### Why the ecosystem file is `.json` (not `.js`)

`pm2 v7` picks the interpreter from the file extension. A file ending
in `.js` or `.cjs` is loaded as a Node.js script — pm2 spawns
`node ecosystem.config.js`, the binary is never invoked, and you get
a zombie process that consumes the slot but does nothing. A `.json`
ecosystem sidesteps this.

See `scripts/ecosystem.config.md` for the full rationale and the
`bash + exec` trick used to launch a non-Node binary under pm2.

### Configuring the MySQL admin credential

Both `systemd` and `pm2` modes run the service as user `db-isolation`,
which cannot read `/root/.my.cnf`. Put the MySQL admin DSN in
`/etc/db-isolation/admin.cnf` instead:

```ini
# /etc/db-isolation/admin.cnf
[client]
user=YOUR_ADMIN_USER
password=YOUR_ADMIN_PASSWORD
host=127.0.0.1
```

```bash
sudo chown root:db-isolation /etc/db-isolation/admin.cnf
sudo chmod 0640 /etc/db-isolation/admin.cnf
```

The admin account **must** have `CREATE USER`, `GRANT OPTION`, and
`DROP USER` privileges. The default `root` works. If `root@localhost`
is disabled on your box (common on Aliyun Linux), pick another SUPER
account, e.g., one from your env-config.

Then in `config.yaml`:

```yaml
mysql:
  admin_config_path: /etc/db-isolation/admin.cnf
```

Alternatively, set `MYSQL_ADMIN_DSN` in `/etc/db-isolation/db-isolation.env`
(env-vars are read by systemd but ignored by pm2; for pm2 use the file
above).

### Redeploying a new binary

```bash
# dev machine: cross-compile
make build-linux    # produces bin/*.linux-amd64

# push (requires ssh key access; the deploy script assumes root)
scp bin/db-isolation.linux-amd64 root@HOST:/tmp/

# on the host, atomic swap + restart
ssh root@HOST '
  sudo -u db-isolation \
      HOME=/var/lib/db-isolation PM2_HOME=/var/lib/db-isolation/.pm2 \
      /usr/local/bin/db-isolation-pm2 stop db-isolation
  rm -f /usr/local/bin/db-isolation
  cp /tmp/db-isolation.linux-amd64 /usr/local/bin/db-isolation
  chmod 0755 /usr/local/bin/db-isolation
  sudo -u db-isolation \
      HOME=/var/lib/db-isolation PM2_HOME=/var/lib/db-isolation/.pm2 \
      /usr/local/bin/db-isolation-pm2 start /var/lib/db-isolation/ecosystem.config.json --env production
  curl -s http://127.0.0.1:8787/health
'
```

Or use the helper:

```bash
scripts/deploy-aliyun.sh root@HOST
```

Why stop → swap → start instead of `pm2 reload`? `pm2 reload` does NOT
swap the file under a running process on Linux (ETXTBSY). The binary is
offline for ~1 second; pm2's `kill_timeout: 10000` ensures clean
shutdown.

## Exposing the API to other hosts

By default the service binds to `127.0.0.1:8787` — only callable from
the box itself. Do **not** open that port directly. Three acceptable
ways to expose it:

| Option | When |
| ------ | --- |
| **nginx reverse proxy + TLS** | Most common. Public or internal users. |
| **SSH tunnel** `ssh -L 8787:127.0.0.1:8787 HOST` | Ad-hoc access from a developer laptop. |
| **Tailscale / WireGuard** | Zero-trust mesh for a small team. |

### nginx reverse proxy

`scripts/nginx-db-isolation.conf` is shipped ready to drop in. Two ways
to use it:

**Option A — full site config** (you only need to add TLS):

```bash
sudo cp scripts/nginx-db-isolation.conf \
    /etc/nginx/conf.d/db-isolation.conf
sudo certbot --nginx -d dbi.example.com
```

The shipped file already sets the right proxy headers, body-size limit
(8 KB — bodies are tiny JSON), and `proxy_buffering off`. It listens
on the public interface and proxies to `127.0.0.1:8787`.

**Option B — snippet for an existing server block** (mix into your
existing TLS site):

```bash
sudo install -d /etc/nginx/snippets
sudo cp scripts/nginx-db-isolation.conf \
    /etc/nginx/snippets/db-isolation-location.conf
```

Then in your existing `/etc/nginx/sites-enabled/dbi.example.com.conf`:

```nginx
server {
    listen 443 ssl;
    server_name dbi.example.com;
    ssl_certificate     /etc/letsencrypt/live/dbi.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/dbi.example.com/privkey.pem;

    # ... your other locations ...

    location / {
        include snippets/db-isolation-location.conf;
    }
}
```

Reload nginx: `sudo nginx -t && sudo systemctl reload nginx`.

After this, applications can use:

```bash
dbi --url https://dbi.example.com --token dbi_xxx list
```

The API never sees a public IP. nginx terminates TLS, forwards
`Authorization: Bearer ...` headers unchanged, and rate-limits at the
web tier if you want.

### Smoke test the deploy

```bash
# on the deploy host, after the first start:
DB_ISOLATION_TOKEN=$(/usr/local/bin/db-isolation token create --name smoke --config /etc/db-isolation/config.yaml)
export DB_ISOLATION_TOKEN DB_ISOLATION_URL=http://127.0.0.1:8787
bash scripts/smoke-pm2.sh
```

Exits non-zero on any failure; safe to wire into a deploy hook.

## Bugs caught during real deployment

These are not in the original design — they surfaced only when the
binary met MySQL 8 and a real production filesystem. The fixes are in
HEAD; redeploying picks them up automatically.

1. **MySQL 8 MEDIUM password policy rejected our passwords.**
   `RandomPassword` produced base64url-only output, which has upper /
   lower / digits but no special character. MySQL 8's
   `validate_password` plugin requires one of each.
   *Fix:* `RandomPassword` now splices one character from each class
   into the base64url output. Verified by
   `TestRandomPasswordSatisfiesMysqlMedium`.

2. **`quoteMySQLString` panicked on the new passwords.**
   The password generator now includes `!@#$%^&*`, but the
   `isSafePasswordLiteral` allow-list in `mysqlx` was still
   base64url-only. The panic crashed the request goroutine; pm2's
   `autorestart` made the failure look like a flaky server.
   *Fix:* broadened `isSafePasswordLiteral` to accept the four
   special characters while still rejecting SQL-injection characters.

3. **`secrets.validateIdentifier` rejected project names with `-`.**
   `secrets.Write("iximei-kf", ...)` panicked because the on-disk
   filename validator only allowed `A-Za-z0-9_`.
   *Fix:* broadened the filename allow-list to `A-Za-z0-9-_.`. The
   MySQL identifier check (in `mysqlx`) is enforced separately and
   remains strict.

4. **pm2 ecosystem must use `.json` extension.**
   See "Why the ecosystem file is `.json`" above.

If you ever see a test fail with what looks like a password issue,
check MySQL's policy first:

```sql
SHOW VARIABLES LIKE 'validate_password.%';
```

If the policy tightens further (e.g., STRONG), `RandomPassword` will
need to splice more characters; `TestRandomPasswordSatisfiesMysqlMedium`
will fail loudly.


## License

Pick something appropriate for your org before deploying. This codebase is
provided as-is, without warranty.
