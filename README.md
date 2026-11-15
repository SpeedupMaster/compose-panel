# Compose 面板

一个轻量的 Docker Compose 管理面板，用来替代 Dokploy 这类偏重的平台。只做四件事，并尽量做好：

- **Compose 部署**：在面板里写 compose.yml，或从 Git 仓库部署（Webhook 自动部署、SSH 部署密钥）；每次部署保存快照，失败自动回滚，可一键回滚到任意历史版本并查看差异。内置与自定义模板（`${VAR}` 占位符），镜像更新检测与可选的自动更新，项目内定时任务。
- **数据库管理与备份**：一键创建 PostgreSQL / MySQL / MariaDB / MongoDB / Redis；库、用户、会话、表浏览、慢查询、带安全确认的 SQL 控制台、导入导出；定时备份（zstd 压缩 + age 加密 + sha256 校验），GFS 保留策略，本地 / S3 兼容 / SFTP / WebDAV 存储，恢复到新库或覆盖原库（含 Redis），定期恢复演练。
- **反向代理与证书**：内置 Caddy，按域名绑定到项目服务，自动申请和续期 Let's Encrypt 证书，支持上传自有证书、Basic Auth、IP 白名单、跳转、访问日志、维护模式（含部署期间自动维护）、多副本负载均衡与主动健康检查。
- **运维管理**：容器日志（实时）、Web 终端、容器与数据卷文件浏览（上传/下载）、资源监控与历史曲线、容器崩溃/OOM/健康检查/磁盘告警、镜像/卷/网络清理、任务中心、通知（Telegram / 企业微信 / 钉钉 / 飞书 / Bark / 邮件 / Webhook）、多用户与项目级权限、API 令牌、防篡改审计日志、两步验证。

单个 Go 二进制（内嵌 React 前端 + SQLite），空闲内存约几十 MB。

## 架构

```
            ┌──────────────── 主机 ────────────────────────────────────┐
 浏览器 ──► │ Caddy :80/:443 ──(panel-proxy 网络)──► 项目容器            │
            │   ▲ admin unix socket                                     │
            │   │                                                       │
            │ panel :7800 ── /var/run/docker.sock ──► Docker Engine     │
            │   │  SQLite (/data/panel)                                 │
            │   └─ docker compose CLI（项目目录同路径挂载）             │
            │                                                           │
            │ 数据库实例 ──(panel-db 网络)── 应用容器                   │
            └───────────────────────────────────────────────────────────┘
```

- 部署时面板在项目目录生成 `.panel/override.yml`（接入 `panel-proxy` 网络、打标签）和 `.panel/panel.env`，再执行 `docker compose up -d --wait`。你的 compose.yml 保持原样。
- 所有耗时操作（部署、备份、恢复、清理）都是持久化的任务，日志可实时查看，面板重启后记录仍在。
- 敏感数据（环境变量密钥、数据库密码、存储密钥、私钥）用 `master.key` 做 AES-GCM 加密后存入 SQLite。**请备份 `/data/panel/master.key`。**

## 快速开始（Linux 服务器）

要求：Docker 24+ 与 docker compose 插件，80/443 端口空闲。

```bash
git clone <本仓库> compose-panel && cd compose-panel/deploy
# 国内网络可加：export GOPROXY=https://goproxy.cn,direct NPM_REGISTRY=https://registry.npmmirror.com
docker compose up -d --build
docker compose logs panel | grep -i token     # 首次启动打印的初始化令牌
```

打开 `http://<服务器 IP>:7800`，输入初始化令牌创建管理员。然后建议：

1. **设置 → 面板设置**：填写面板域名（如 `panel.example.com`，先把 DNS 解析到服务器）和 ACME 邮箱，之后通过 `https://panel.example.com` 访问；再把 compose 中的 `7800:7800` 改成 `127.0.0.1:7800:7800`。
2. **设置 → 我的账号**：开启两步验证。
3. **存储与通知**：添加一个 S3 兼容的异地备份存储，配置 Telegram / 企业微信通知；在「备份加密密钥」里把 age 私钥离线保存一份。

默认数据目录为 `/opt/compose-panel/{data,projects,backups}`，可用环境变量 `PANEL_ROOT` 修改。

### 忘记密码

```bash
docker exec panel panel reset-password <用户名>
```

会生成新密码并关闭该用户的两步验证。

## 使用要点

- **项目域名**：新建项目时可直接填写域名、服务和容器端口；也可以在「站点」页面单独添加。服务不需要映射宿主机端口。
- **连接数据库**：应用的 compose 里加入外部网络 `panel-db`，用实例名作为主机名：

  ```yaml
  services:
    app:
      image: my/app
      environment:
        DATABASE_URL: postgres://app:${DB_PASSWORD}@pg-main:5432/app
      networks: [default, panel-db]
  networks:
    panel-db:
      external: true
  ```

- **Git 自动部署**：项目设置里复制 Webhook 地址和签名密钥填到 GitHub / Gitea / GitLab（Content type 选 `application/json`）。只有推送到配置的分支才会触发。
- **CI 调用**：创建「部署」权限的 API 令牌，`curl -X POST -H "Authorization: Bearer cpt_…" https://panel.example.com/api/projects/<id>/deploy`。
- **导入已有项目**：设置 → 导入项目，会扫描主机上由 `docker compose` 启动的项目。其目录需要以同路径挂载进面板容器。
- **模板**：内联项目可「保存为模板」（默认把 `.env` 中的密钥替换为 `${VAR}` 占位符）；从模板新建时填写变量，附带的配置文件一并写入项目目录。
- **镜像更新**：每天按「设置 → 镜像检查计划」比对 registry 上的 digest，概览页提示可更新的项目；项目开启「自动更新」后发现新镜像会自动重新部署（失败照常回滚）。固定 digest 的镜像不会被检查。
- **SQL 控制台**：默认只读；写入/DDL 语句需要二次确认，`DROP`/`TRUNCATE`/不带 `WHERE` 的 `DELETE`/`UPDATE` 会列出原因要求再确认。单条语句 30 秒超时，最多返回 1000 行（可导出 CSV），所有执行写入审计日志。
- **恢复演练**：在「备份 → 恢复演练」为策略设置计划，面板把最新备份恢复到无网络的临时容器/卷里执行检查（SQL 计数或文件数），通过后备份标记为「已演练」，失败发送通知。
- **站点维护**：站点列表「维护」立即返回 503 维护页；勾选「部署期间自动显示维护页面」后，项目部署时自动开启、结束后恢复。负载均衡把请求轮询到服务的所有副本（`deploy.replicas` 或 `scale`），配置健康检查路径后不健康的副本会被摘除。

### 手动解密备份

```bash
age -d -i panel-age-key.txt xxx.pgdump.zst.age | zstd -d > xxx.pgdump
pg_restore -d mydb xxx.pgdump        # PostgreSQL（custom 格式）
mysql mydb < xxx.sql                 # MySQL / MariaDB
mongorestore --archive=xxx.archive   # MongoDB
```

## 配置（环境变量）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PANEL_LISTEN` | `:7800` | 监听地址 |
| `PANEL_DATA_DIR` | `/data/panel` | SQLite、master.key、任务日志 |
| `PANEL_PROJECTS_DIR` | `/data/projects` | 项目目录，**需与宿主机同路径** |
| `PANEL_BACKUPS_DIR` | `/data/backups` | 默认本地备份目录，**需与宿主机同路径** |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker Engine |
| `PANEL_CADDY_ADMIN` | `/run/caddy/admin.sock` | Caddy admin socket |
| `PANEL_CADDY_DATA` | `/caddy-data/caddy` | Caddy 证书存储（只读，用于展示证书） |
| `PANEL_PROXY_NETWORK` | `panel-proxy` | 代理网络 |
| `PANEL_DB_NETWORK` | `panel-db` | 数据库网络 |
| `PANEL_UPSTREAM` | `panel:7800` | Caddy 访问面板的地址 |
| `PANEL_HELPER_IMAGE` | `alpine:3.20` | 数据卷备份/恢复使用的临时容器镜像 |
| `PANEL_DEV_PROXY` | 空 | 开发时把非 API 请求转发到 Vite |

## 开发

```bash
# 后端（需要 Go 1.23+ 和可访问的 Docker）
go mod tidy
PANEL_DATA_DIR=.devdata/panel PANEL_PROJECTS_DIR=$PWD/.devdata/projects PANEL_BACKUPS_DIR=$PWD/.devdata/backups \
PANEL_DEV_PROXY=http://localhost:5173 go run ./cmd/panel

# 前端
cd web && npm install && npm run dev   # http://localhost:5173，/api 代理到 :7800
```

构建单个二进制：`cd web && npm run build && cd .. && CGO_ENABLED=0 go build -o bin/panel ./cmd/panel`。

### 测试

在一台**测试用**的 Docker 主机上（root）：

```bash
sh scripts/e2e-reset.sh   # 清空面板数据与它创建的所有项目/数据库/卷，重新启动整套服务（切勿在生产机运行）
sh scripts/e2e.sh         # 端到端：初始化 → 部署 → *.localhost 站点经 Caddy → 建 PostgreSQL → 加密备份 → 校验 → 恢复 → 重新部署 → 回滚
sh scripts/e2e2.sh        # 第二期（在 e2e.sh 之后运行）：指标历史、模板、镜像检查、定时任务、文件浏览、SQL 安全、导入导出、恢复演练、Redis 恢复、SFTP/WebDAV、维护模式、访问日志、负载均衡
sh scripts/smoke.sh http://127.0.0.1:7800 <初始化令牌>   # 只读接口冒烟测试
```

Windows 上用 WSL 开发：在 Ubuntu（WSL2，装好 Docker Engine）里运行 `sh /mnt/f/代码/compose-panel/scripts/wsl-sync.sh`，会把源码同步到 `/opt/src/compose-panel` 并重新构建、启动；Windows 浏览器访问 `http://localhost:7800`。WSL 空闲时会自动关机，测试期间可开一个 `wsl -- sleep infinity` 保持运行。

### 代码结构

```
cmd/panel            入口：serve / reset-password / version
internal/store       SQLite schema 与迁移
internal/secret      master.key 与 AES-GCM
internal/auth        密码、TOTP、令牌
internal/docker      Docker Engine API 客户端（无 SDK 依赖）
internal/compose     docker compose CLI 封装与 override 生成
internal/proxy       Caddy JSON 配置生成与证书扫描
internal/dbmgr       各数据库引擎的管理命令（docker exec）
internal/backup      存储（本地/S3/SFTP/WebDAV）、加密管道、GFS 保留
internal/jobs        持久化任务队列与日志
internal/notify      通知渠道（含钉钉/飞书/Bark）
internal/server      HTTP API、调度、监控
web/                 React + Vite 前端（构建后嵌入二进制）
deploy/              docker-compose.yml 与 Caddy 引导配置
```

## 当前状态与限制

- 前端通过 TypeScript 检查并构建成功；后端在 Go 1.27 下通过 `go vet` 和 `go build`（`CGO_ENABLED=0`，约 16 MB）。
- 在 WSL2 Ubuntu 24.04 + Docker 29 + Compose v5 上按 `deploy/docker-compose.yml` 实际部署，`scripts/e2e.sh` 与 `scripts/e2e2.sh` 全部通过；浏览器逐页检查无前端报错，Web 终端、实时日志、任务日志流均可用。
- 尚未实测：MySQL / MariaDB / MongoDB 实例、S3 存储、Git 来源与 Webhook、通知渠道发送、真实域名的 Let's Encrypt 签发、两步验证、非管理员角色的权限边界。
- 尚未实现：终端会话录像（目前只审计开始/结束）、邀请链接（改为生成一次性初始密码）、纯 HTTP 站点、私有 Registry 凭据管理、多节点 Agent。
- 需要自定义 Caddy 构建、暂未实现：通配符证书自动签发（DNS-01，需对应 DNS 服务商插件；可上传证书代替）、请求限流（需 rate_limit 插件）。
- 面板自身数据（`panel` 类型备份）的恢复仍需按「手动解密备份」离线操作；慢查询分析只支持 PostgreSQL（pg_stat_statements）与 MySQL/MariaDB（performance_schema），MongoDB / Redis 暂不支持。
- 只管理单台主机；多主机请各自部署一个面板。
