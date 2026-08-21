# pm2 ecosystem for db-isolation

This document explains `ecosystem.config.json` and why it looks the way it does.

## Run

```bash
# dev / smoke:
pm2 start scripts/ecosystem.config.json

# production reload after a binary update:
pm2 reload scripts/ecosystem.config.json --env production

# production start (idempotent):
pm2 startOrReload scripts/ecosystem.config.json --env production
```

## Why a `.json` file, not the conventional `ecosystem.config.js`?

**pm2 v7 picks the interpreter from the ecosystem file extension.** A
file ending in `.js` or `.cjs` is loaded as a Node.js script — pm2 spawns
`node ecosystem.config.js`, the binary is never invoked, and you get a
zombie process that consumes the slot but does nothing.

This is the kind of bug that survives for weeks in production because
pm2 reports `online`, the slot is occupied, and the real binary never
starts. A `.json` ecosystem sidesteps this and lets pm2 honour the
`script` and `args` we declare.

## Why `script: /bin/bash` + `exec ...`?

pm2 v7 still doesn't have a clean "no interpreter, run this binary
directly" mode. `exec_interpreter: 'none'` is silently ignored.

The robust pattern is:

```json
{
  "script": "/bin/bash",
  "args": "-lc \"exec /usr/local/bin/db-isolation server --config /etc/db-isolation/config.yaml\""
}
```

`exec` replaces the bash process with the binary, so:

* the binary is the direct child of pm2;
* SIGTERM goes straight to the binary, not to bash;
* `kill_timeout` and restart logic work against the binary, not an
  intermediate shell.

## Security properties baked in

| Property | How |
| -------- | --- |
| MySQL root password is not in the ecosystem file | The server reads `/root/.my.cnf` at boot. The config never sees the DSN. |
| API listens only on localhost | `DB_ISOLATION_ADDR=127.0.0.1:8787` is the default in `config.yaml`. No external port exposure without a reverse proxy. |
| Process runs as a dedicated user | `uid: db-isolation`, `gid: db-isolation`. pm2 spawns the child with setuid when invoked by root. |
| Logs go to a private directory | `/var/log/db-isolation/`, mode `0750`, owned by `db-isolation`. |
| Restart is bounded | `max_restarts: 10`. After that, pm2 marks the process `errored` and stops instead of looping. |

## What is intentionally NOT here

* No `MYSQL_ADMIN_DSN` env var. Putting it here means it ends up in
  `~/.pm2/` with only the protection of pm2's home directory.
* No `--public-ip`, no `--host 0.0.0.0`. The API stays loopback-only.
* No cluster mode. db-isolation is a single-process provisioning
  gateway; multiple instances would race on the SQLite WAL.

## Adding the service to boot

Once `pm2 startOrReload ecosystem.config.json --env production` is
working, persist it:

```bash
sudo -u db-isolation pm2 save
# Generate a systemd unit that re-launches pm2 on boot:
sudo -u db-isolation pm2 startup systemd -u db-isolation --hp /var/lib/db-isolation \
    | sudo bash
```

The `pm2 save` writes the current process list to
`/var/lib/db-isolation/.pm2/dump.pm2`. The `pm2 startup` command emits a
systemd unit (`pm2-db-isolation.service`) that simply calls
`pm2 resurrect` on boot.