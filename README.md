# db-isolation

给每个项目一个独立的 MySQL 数据库和独立账号，AI 永远看不到 root 密码。

```
CLI / MCP  →  HTTP API  →  Bearer 认证  →  Provision  →  MySQL (127.0.0.1)
                                                      ↘ SQLite (audit log)
```

## 解决什么问题

一台 ECS 上跑多个项目，常见做法是：

1. 所有项目共享一个 MySQL root 或超级账号 — **泄露 = 全部完蛋**
2. 把 root 密码告诉 AI coding agent — **你信任 AI 不会写错 SQL 吗？**
3. 每个项目自己的库但共用一个账号 — **一个应用出 bug 能删其他库的表**

db-isolation 用一个轻量 HTTP 服务把这些问题收敛掉：

- 每个项目一个独立数据库 + 独立账号，**只能访问自己的库**
- 密码自动生成，存到服务器本地文件（mode `0600`），**API 不返回密码**
- 所有建库/改密/删库操作记**审计日志**，留痕可查
- CLI / MCP / HTTP API 三种入口，都走同一套认证，**永远不给 AI root 权限**

## 快速上手（5 分钟）

### 1. 编译

```bash
make build        # 产出 bin/db-isolation、bin/dbi、bin/mcp
```

### 2. 配置 MySQL

MySQL 只监听 localhost：

```ini
# /etc/mysql/mysql.conf.d/mysqld.cnf
[mysqld]
bind-address = 127.0.0.1
```

管理员凭证（服务端会读这个文件，**不会**暴露给客户端）：

```ini
# /etc/db-isolation/admin.cnf
[client]
user=root
password=YOUR_PASSWORD
host=127.0.0.1
```

```bash
sudo chown root:db-isolation /etc/db-isolation/admin.cnf
sudo chmod 0640 /etc/db-isolation/admin.cnf
```

### 3. 安装服务

```bash
sudo bash scripts/install.sh          # systemd 模式（推荐）
# 或
sudo bash scripts/install.sh --pm2    # pm2 模式
```

安装脚本会创建：
- `db-isolation` 系统用户（nologin shell）
- `/etc/db-isolation/`（配置）
- `/var/lib/db-isolation/`（数据库）
- `/var/log/db-isolation/`（审计日志）
- `/etc/db-isolation/apps/`（每项目的密码文件，mode `0750`）

### 4. 启动

```bash
# systemd 模式
sudo systemctl enable --now db-isolation

# pm2 模式
sudo -u db-isolation \
    HOME=/var/lib/db-isolation PM2_HOME=/var/lib/db-isolation/.pm2 \
    pm2 start scripts/ecosystem.config.json --env production
```

### 5. 创建管理员 token

```bash
sudo db-isolation token create --name local-admin
```

输出（**只显示一次**，服务端只存 SHA-256 hash）：

```
Token created. This token will only be shown once.

dbi_xxxxxxxxxxxxxxxxxxxxxxxxxx

Save it now. Store as DB_ISOLATION_TOKEN.
```

### 6. 创建项目数据库

```bash
export DB_ISOLATION_URL=http://127.0.0.1:8787
export DB_ISOLATION_TOKEN=dbi_xxx

dbi create opcos
```

```
✓ Project: opcos
✓ Database: opcos_db
✓ User: opcos_user
✓ Secret: /etc/db-isolation/apps/opcos.env
✓ Status: ready
```

密码**不出现在 CLI 输出里**。应用程序读 secret 文件获取连接串：

```bash
sudo cat /etc/db-isolation/apps/opcos.env
# DATABASE_URL=mysql://opcos_user:<随机密码>@127.0.0.1:3306/opcos_db
```

### 7. 验证隔离

```bash
# 项目用户能操作自己的库
mysql -u opcos_user -p -h 127.0.0.1 opcos_db -e "CREATE TABLE t(id INT);"

# 不能访问其他库
mysql -u opcos_user -p -h 127.0.0.1 opcos_db -e "USE mysql;"
# → ERROR 1044: Access denied

# 不能建用户
mysql -u opcos_user -p -h 127.0.0.1 opcos_db -e "CREATE USER x@localhost;"
# → ERROR 1227: Access denied
```

### 8. 轮换密码

```bash
dbi rotate opcos   # 新密码写入 secret 文件，旧密码立即失效
```

### 9. 删除数据库

```bash
dbi delete opcos                # 拒绝：必须 confirm
dbi delete opcos --confirm wrong  # 拒绝：confirm 不匹配
dbi delete opcos --confirm opcos  # OK：删除库 + 用户 + secret 文件 + 审计记录
```

## 用法

### dbi CLI

```
dbi [--url URL] [--token TOKEN] [--json] <command> [args]

Commands:
  list                          列出所有项目
  status <name>                 查看单个项目
  create <name>                 创建项目数据库
  rotate <name>                 轮换密码
  delete <name> --confirm <name>  删除（需二次确认）
```

默认连接 `http://127.0.0.1:8787`，token 从 `DB_ISOLATION_TOKEN` 环境变量读。

`--json` 输出机器可读的 JSON，方便脚本和 AI 解析。

### HTTP API

所有 `/v1/*` 路由需要 `Authorization: Bearer dbi_xxx`，`/health` 不需要。

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 健康检查 |
| `GET` | `/v1/databases` | 列出所有项目 |
| `GET` | `/v1/databases/{name}` | 查看单个项目 |
| `POST` | `/v1/databases` | 创建项目（幂等，已存在返回 200） |
| `POST` | `/v1/databases/{name}/rotate` | 轮换密码 |
| `DELETE` | `/v1/databases/{name}` | 删除（body 需 `{"confirm":"name"}`） |

错误响应格式：

```json
{
  "error": {
    "code": "DATABASE_NOT_FOUND",
    "message": "database project not found"
  }
}
```

错误码：`UNAUTHORIZED` / `INVALID_PROJECT_NAME` / `DATABASE_NOT_FOUND` / `MYSQL_ERROR` / `SECRET_WRITE_ERROR` / `CONFIRMATION_REQUIRED` / `INTERNAL_ERROR`

### MCP Adapter

MCP 是给 AI coding agent（Codex / Claude Code / opencode）用的 stdio JSON-RPC 服务器：

```json
{
  "mcpServers": {
    "db-isolation": {
      "command": "/usr/local/bin/db-isolation-mcp",
      "env": {
        "DB_ISOLATION_URL": "http://127.0.0.1:8787",
        "DB_ISOLATION_TOKEN": "dbi_xxx"
      }
    }
  }
}
```

暴露 5 个工具：`database_list` / `database_status` / `database_create` / `database_rotate` / `database_delete`

**没有** `execute_sql`、`execute_shell`、`mysql_root`、`read_secret` — MCP 进程只调 HTTP API，不碰文件系统。

## 安全模型

```
AI / CLI / MCP
    ↓  只能调 HTTP API，只知道 name/database/user/status
HTTP API (127.0.0.1:8787)
    ↓  Bearer token 认证，token 只存 SHA-256 hash
Provision Service
    ↓  生成密码，只写到本地文件
MySQL (127.0.0.1)
    ↓  项目用户只能访问自己库，host 限定 127.0.0.1/localhost
```

MySQL root 凭证**只存在** `/etc/db-isolation/admin.cnf`（mode `0640`，root:db-isolation），服务端启动时读一次，CLI / MCP 永远接触不到。

API **永不返回**密码、`DATABASE_URL`、root DSN。审计日志也**不记录**这些。

## 外网访问

API 默认只监听 `127.0.0.1:8787`。对外提供 HTTPS 推荐 nginx 反代：

```bash
# 安装 nginx
sudo cp scripts/nginx-db-isolation.conf /etc/nginx/conf.d/db-isolation.conf

# 申请 TLS 证书（用 aic + acme.sh，DNS 验证）
aic cert:issue db.zerocmf.com

# nginx reload
sudo nginx -t && sudo systemctl reload nginx
```

然后外部可以用 `dbi --url https://db.zerocmf.com list`。

完整 nginx 配置见 `scripts/nginx-db-isolation.conf`。

## CI/CD

`git push main` 会自动触发 GitHub Actions：

1. 在 runner 上 `go build` 编译 linux/amd64 binary
2. `scp` 传到 ECS
3. 远程跑 `ci-deploy.sh`：sha256 校验 → atomic swap → pm2 重启 → 证书续签 → 健康检查

所有操作记审计日志（`/var/log/ci-deploy.log`）。

**密钥管理**：CI 用单独的 SSH key（AES-256 加密），passphrase 存 GitHub Secrets。ci-deployer 用户只有受限 sudo（只能跑 install / pm2 / curl / acme.sh），不能读 secret 文件，不能开 shell。

## 运维

### 创建 / 列出 / 吊销 token

```bash
sudo db-isolation token create --name local-admin    # 显示一次
sudo db-isolation token list
sudo db-isolation token revoke --id 1
```

### 审计日志

```bash
sudo db-isolation audit list --limit 50
```

记录每次 create / rotate / delete / auth 操作的 timestamp、action、resource、remote_ip、success、message。**不记录密码、token、DSN**。

### 数据库备份

SQLite WAL 模式下可直接 `cp`，不需要停止服务：

```bash
cp /var/lib/db-isolation/db-isolation.db /backup/db-isolation-$(date +%F).db
```

### MySQL 硬化

- `bind-address = 127.0.0.1`（必须）
- `local-infile=0`
- `validate_password_policy=MEDIUM` 或 `STRONG`

## 开发

```bash
make build    # 编译三个 binary
make test     # 单元测试（-race）
make itest    # MySQL 集成测试（需要 DB_ISOLATION_TEST_MYSQL_DSN）
make vet      # go vet
make fmt      # go fmt
```

本地跑服务（不需要 MySQL）：

```bash
mkdir -p /tmp/dbi-smoke
cat > /tmp/dbi-smoke/config.yaml <<EOF
server: { addr: 127.0.0.1:8787 }
storage: { path: /tmp/dbi-smoke/dbi.db }
secrets: { dir: /tmp/dbi-smoke/apps }
audit: { to_file: false }
EOF
./bin/db-isolation token create --name dev --config /tmp/dbi-smoke/config.yaml
MYSQL_ADMIN_DSN='root@tcp(127.0.0.1:3306)/' ./bin/db-isolation server --config /tmp/dbi-smoke/config.yaml
```

## 项目结构

```
cmd/
  server/        db-isolation 二进制（HTTP API + token 管理 + 审计）
  dbi/           CLI 客户端（只调 HTTP API）
  mcp/           MCP stdio 适配器（5 个工具，无 SQL/shell）
internal/
  api/           HTTP handler、错误格式、认证中间件
  audit/         SQLite + JSONL 审计写入
  auth/          Bearer token SHA-256 校验
  config/        YAML + 环境变量 + my.cnf 解析
  model/         DatabaseProject、Token、AuditLog、项目名校验
  mysqlx/        Provisioner（唯一接触 MySQL admin 的包）
  provision/     create / rotate / delete 工作流
  secrets/       原子写 DATABASE_URL 文件（0600）
  store/         SQLite 元数据存储
scripts/
  db-isolation.service     systemd unit
  ecosystem.config.json    pm2 配置
  ecosystem.config.md      pm2 配置说明（为什么用 .json、bash+exec）
  install.sh               安装脚本（systemd / pm2 两种模式）
  ci-deploy.sh             CI/CD 部署脚本（传到 ECS /usr/local/bin/）
  deploy-aliyun.sh         手动部署脚本
  nginx-db-isolation.conf  nginx 反代配置
  smoke-pm2.sh             部署后健康检查
.github/workflows/
  deploy.yml               GitHub Actions CI/CD
```

## License

自选。代码按原样提供，不附带任何保证。