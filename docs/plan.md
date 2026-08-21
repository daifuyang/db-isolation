# db-isolation 规划文档

## 背景

当前所有数据库（MySQL/PG/Redis）直连 ECS 公网 IP `139.196.89.64:3306/6379`，
端口对外暴露不合理；凭证散落在 `env-config` yaml 与 `.env` 中；AI 直连 DB 无审计、无拦截。

用户真实诉求（保留原话）：
> 不给权限过高的账户，防止 ai 连上覆盖了其他的库。
> 现在没开新项目，都要创建新库，希望隔离。
> 然后每个应用管自己的，删了影响面最小

## 整体架构

```
┌─────────────────────────────────────────────────────┐
│  公网 (139.196.89.64)                                │
│  � MySQL/PG/Redis 端口已关闭                          │
│  ✓ 仅 SSH 22 + Web 管理（如 adminer，BasicAuth）      │
└─────────────────────────────────────────────────────┘
                        │
        �───────────────┼───────────────┐
        │ SSH 隧道       │ SSH 隧道       │ SSH 隧道
        ▼               ▼               ▼
   ┌─────────┐    ┌─────────┐    ┌─────────┐
   │ opcos   │    │iximei_kf│    │yishan_mp│
   │ 应用    │    │ 应用    │    │ 应用    │
   └────┬────┘    └────┬────┘    └────┬────┘
        │ 持有各自专属账号（最小权限）  │
        ▼               ▼               ▼
   ┌────────────────────────────────────────┐
   │  MySQL @ 127.0.0.1:3306                │
   │  ├─ opcos_db     ← opcos_user 只能进   │
   │  ├─ iximei_kf_db ← iximei_kf_user     │
   │  └─ yishan_mp_db ← yishan_mp_user     │
   └────────────────────────────────────────┘
```

## 分阶段落地

### Phase 1：建库脚本（今天就能做）

**产出**：`scripts/new-db.sh` —— 一键创建"库 + 专属账号"，root 凭证只在这个脚本里。

用法：
```bash
new-db.sh <db_name> <app_user> <app_pass> [--readwrite|--readonly]
```

效果：
- AI 调用这个脚本就能自助建库（不用 root）
- 自动应用最小权限模板
- 日志记录到 `logs/db-isolation.log`

### Phase 2：盘点 + 现有应用拆库

整理现有库清单，逐个应用换账号：

| 应用名 | 数据库 | 现有账号 | 目标账号 | 权限 |
|---|---|---|---|---|
| opcos | ? | ? | opcos_user | SELECT, INSERT, UPDATE, DELETE |
| iximei-kf | ? | ? | iximei_kf_user | SELECT, INSERT, UPDATE, DELETE |
| yishan-mp | ? | ? | yishan_mp_user | SELECT, INSERT, UPDATE, DELETE |

流程：
1. 用 `new-db.sh` 建新库（或者复用现有库，名字规范化）
2. 改应用配置连接串到新账号
3. **保留旧账号一段时间做回滚兜底**，验证没问题后 DROP

### Phase 3：公网端口收口

- MySQL/PG/Redis 的 `bind-address` 改 `127.0.0.1`
- 或者用 iptables 拒绝公网入站到 3306/5432/6379
- 验证：ECS 上 `mysql -h139.196.89.64 -P3306 -uroot -p` 应该连不上

### Phase 4：凭证分级 + AI 配置

- `~/.config/env-config/apps.yaml` 下每个应用一个连接串
- 移除各项目 `.env` 里的明文 DB 密码
- AI 只能读它当前任务对应的那个连接串

### Phase 5：Web 管理界面（可选但推荐）

- adminer（单 PHP 文件）反代到 Nginx + BasicAuth
- 只对你自己开放，方便手动 DDL / 排查
- AI 不碰这块

## 待确认问题

1. **新库 vs 复用旧库**：现有应用继续用原库名、只换账号？还是规范化重命名？
2. **MySQL 版本**：ECS 上 MySQL 是什么版本？8.0+ 有更强认证方式
3. **PostgreSQL 怎么处理**：同样方案（`GRANT ON DATABASE ... TO ...`），要不要也搞个 `new-db.sh` PG 版？
4. **审计强度**：需不需要记录每个应用的慢查询/异常 SQL？还是只记录 DDL？

## 当前进度

- [x] 项目骨架建立
- [ ] Phase 1：建库脚本 `new-db.sh`
- [ ] Phase 2：现有应用拆库
- [ ] Phase 3：公网端口收口
- [ ] Phase 4：凭证分级
- [ ] Phase 5：Web 管理界面
