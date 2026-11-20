# Compose Panel

**轻量、克制、零侵入的单机 Docker Compose 部署与运维面板。**

Compose Panel 旨在替代 Dokploy、Coolify 等偏重的 PaaS 平台。它被编译为**单个静态 Go 二进制文件**（内嵌 React 前端与纯 Go 实现的 SQLite，无 CGO 依赖），空闲内存占用仅数十 MB，专注把单台服务器最核心的日常工作做好：

1. **Compose 项目编排**：在线编辑或 Git 拉取部署、版本快照、Diff 对比、失败自动回滚、一键回滚、Webhook / API 触发、模板库、镜像更新检测与容器内定时任务。
2. **数据库交付与运维**：一键创建 PostgreSQL / MySQL / MariaDB / MongoDB / Redis（支持自定义镜像），提供库/用户/会话管理、慢查询分析、在线导入导出与带高危拦截的 SQL 控制台。
3. **反向代理与 SSL 证书**：基于内置 Caddy 2 动态网关，免宿主机端口映射直接按域名反代容器服务，自动签发/续期 Let's Encrypt 证书，支持多副本负载均衡、主动健康检查、gRPC (h2c) 与部署期自动维护页。
4. **加密备份与恢复演练**：支持数据库、数据卷、项目目录与面板自身数据的定时备份；采用 `zstd` 压缩 + `age` 非对称加密 + `SHA-256` 校验，支持本地 / S3 / SFTP / WebDAV 存储、GFS 保留策略，以及在断网沙箱容器中自动验证备份可用性的**恢复演练**。
5. **容器监控、告警与审计**：实时日志、Web 终端、容器与数据卷文件管理、7 天资源历史曲线、7 种通知渠道、RBAC 项目级权限、TOTP 两步验证、防篡改哈希链审计日志，并提供完整的移动端响应式界面与 PWA 支持。

---

## 目录

- [设计理念与架构](#设计理念与架构)
- [功能特性一览](#功能特性一览)
- [快速开始（生产环境部署）](#快速开始生产环境部署)
- [首次登录与安全加固建议](#首次登录与安全加固建议)
- [常用操作指南](#常用操作指南)
- [环境变量与配置参考](#环境变量与配置参考)
- [本地开发与测试](#本地开发与测试)
- [目录结构](#目录结构)

---

## 设计理念与架构

```text
             ┌──────────────────────────── 宿主机 (Host) ────────────────────────────┐
             │                                                                       │
  浏览器 ──► │  Caddy (:80 / :443) ──────────( panel-proxy 网络 )────────► 项目容器   │
             │    ▲             │                                            ▲       │
             │    │ Unix Socket │ 反代面板自身                               │       │
             │    ▼             ▼                                            │       │
             │  Panel (:7800) ─────────── /var/run/docker.sock ──────► Docker Engine │
             │    ├── SQLite + master.key (/data/panel)                              │
             │    └── docker compose CLI（项目与备份目录同路径挂载）                 │
             │                                                                       │
             │  托管数据库实例 ──────────────( panel-db 网络 )───────────► 应用容器   │
             └───────────────────────────────────────────────────────────────────────┘
```

- **对 `compose.yml` 零侵入**：面板不会改写你编写的 `compose.yml`。部署时，面板在项目目录下生成 `.panel/override.yml`（自动为服务注入 `panel-proxy` 网络与元数据标签，对声明了 `network_mode` 的服务自动跳过）和 `.panel/panel.env`，随后调用原生 `docker compose up -d --wait`。即使未来停用面板，所有服务依然可以用原生 `docker compose` 命令独立管理。
- **持久化异步任务引擎**：所有耗时操作（部署、回滚、备份、恢复、恢复演练、镜像巡检）均作为持久化后台任务执行，通过 SSE 实时推送日志流；面板重启后历史任务状态与日志依然完整保留。
- **落盘即加密**：环境变量密钥、数据库密码、Registry 凭据、存储密钥、Git SSH 私钥与通知渠道配置均使用 `/data/panel/master.key` 经 **AES-256-GCM** 加密后写入 SQLite。
- **审计防篡改**：所有关键操作（登录、部署、文件修改、SQL 执行、容器终端会话、权限变更）均记录审计日志，每条记录通过 `prev_hash -> hash` 构成 **SHA-256 哈希链**，支持在面板内一键校验日志链完整性。

---

## 功能特性一览

### 1. Compose 项目编排与持续部署

- **双来源支持**：
  - **面板内编辑（Inline）**：内置语法高亮编辑器，在线维护 `compose.yml`、环境变量（支持普通变量与加密 Secret）及附加配置文件（如 `nginx.conf`、`config.yaml`）。
  - **Git 仓库部署**：支持 HTTPS 与 SSH 克隆，一键生成项目专属 SSH Deploy Key，支持指定分支与仓库内自定义 Compose 文件路径。
- **智能解析与存量导入**：新建项目时自动解析 `compose.yml`，提取候选 Web 服务名、容器端口与域名建议；支持一键扫描宿主机上已由 `docker compose` 启动的存量项目并导入面板纳管。
- **版本快照、Diff 与自动回滚**：每次部署自动归档当时的 `compose.yml`、环境变量与 Git Commit SHA；部署失败时默认**自动回滚**至最近一次成功版本，支持任意两个历史部署版本间的代码 Diff 对比与一键回滚。
- **Webhook 与 API 自动部署**：原生适配 GitHub / GitLab / Gitea Push Webhook（支持 HMAC-SHA256 签名验证与一键轮换密钥，仅匹配指定分支时触发）；支持创建 `deploy` 作用域的 API 令牌供 CI/CD 流水线调用。
- **应用模板库**：内置 `whoami`、Nginx 静态站点、PostgreSQL、MySQL、Redis、MinIO、Uptime Kuma、Gitea、WordPress 等即用模板；支持将任意内联项目**一键保存为自定义模板**（自动识别 `${VAR}` 占位符，默认清空敏感密钥并在新建时自动生成随机强密码）。
- **镜像更新检测与自动升级**：按计划向远程 Registry 查询镜像 Digest 变更（自动忽略已固定 `@sha256` 的镜像）；项目开启「自动更新」后，检测到新镜像将自动拉取并重新部署（失败自动回滚）。支持配置 Docker Hub、GHCR、阿里云 ACR 等私有或公共 Registry 登录凭据。
- **项目级定时任务（Cron）**：按标准 Cron 表达式在项目指定服务容器内定时执行命令（`docker compose exec -T`），支持手动立即触发与执行日志追踪。

### 2. 数据库实例管理与安全控制台

- **多引擎与自定义镜像**：
  - **PostgreSQL**：支持 `18`、`17`、`16`、`15`（自动适配 PostgreSQL 18 `/var/lib/postgresql` 新版数据卷挂载路径）。
  - **MySQL**：支持 `8.4`、`8.0`。
  - **MariaDB**：支持 `11.4`、`10.11`。
  - **MongoDB**：支持 `8.0`、`7.0`。
  - **Redis**：支持 `7.4`、`7.2`（默认启用密码认证与 AOF + RDB 持久化）。
  - **自定义镜像**：创建实例时可指定任意兼容镜像（如 `pgvector/pgvector:pg17`、`postgis/postgis:17-3.5`、`timescale/timescaledb:latest-pg16`）。
- **灵活的网络暴露策略**：默认仅接入 `panel-db` 内部网络，应用容器通过实例名即可内网直连；亦支持按需绑定宿主机 `127.0.0.1`（配合 SSH 隧道）或 `0.0.0.0` 公网端口，并支持配置实例内存上限。
- **可视化日常运维**：
  - 数据库（Database）与用户账号的创建、删除及密码重置。
  - 实时查看当前活跃连接/会话（Sessions），支持一键终止（Kill）阻塞会话。
  - 表/集合列表、字段结构、索引详情浏览与数据行分页预览。
  - **慢查询分析**：PostgreSQL 自动启用 `pg_stat_statements`，MySQL / MariaDB 基于 `performance_schema` 展示高耗时 SQL 聚合统计。
  - **在线导入与导出**：支持从浏览器直接上传 `.sql` / `.pgdump` / `.archive` 文件导入指定库，或一键流式导出下载。
- **带安全护栏的交互式控制台**：
  - 内置语法高亮 SQL / MongoDB / Redis 查询控制台，**默认锁定为只读模式**。
  - 取消只读后执行写入或 DDL 语句需二次确认；针对 `DROP`、`TRUNCATE` 以及不带 `WHERE` 条件的 `DELETE` / `UPDATE` 等高危语句自动弹出风险原因并要求显式确认。
  - 单条查询 30 秒超时保护，最多返回 1000 行并支持一键导出 CSV；所有执行的语句均记入防篡改审计日志。

### 3. 反向代理、流量网关与 SSL 证书

- **零端口暴露反代**：面板通过 `/run/caddy/admin.sock` 动态渲染并推送 Caddy JSON 配置。应用容器无需在宿主机映射端口，只要加入 `panel-proxy` 网络（面板部署时自动注入），即可按域名绑定到容器内部端口。
- **HTTPS 与证书管理**：
  - 自动通过 Let's Encrypt (ACME) 申请与续期证书，自动开启 HTTP -> HTTPS 跳转。
  - 自动扫描 Caddy 数据目录，展示已签发证书的域名、颁发机构、到期时间与剩余天数，到期前自动触发告警。
  - 支持上传自有 PEM 证书链与私钥（如通配符证书或商业证书），由 Caddy 按 SNI 自动匹配。
- **高级站点特性**：
  - **多副本负载均衡与主动健康检查**：当 Compose 服务配置了多副本（`deploy.replicas` 或 `scale`）时，自动发现所有运行中的容器副本并配置轮询（Round-Robin）负载均衡；配置健康检查路径（如 `/healthz`）后自动摘除异常副本。
  - **gRPC (h2c) 代理**：支持一键开启明文 HTTP/2 (`h2c`) 上游传输，满足 gRPC 服务反代需求。
  - **安全与流量策略**：支持 HTTP Basic Auth（bcrypt 加密）、客户端 IP 白名单（CIDR）、HSTS、Gzip/Zstd 响应压缩、请求体大小限制（Max Body Size）、自定义响应头以及域名 301/302 重定向。
  - **维护模式与零报错发布**：支持一键切换站点为 503 维护页（可自定义提示语）；开启**「部署期间自动显示维护页面」**后，项目每次部署时自动切换至维护页，容器健康就绪后自动恢复流量。
  - **站点访问日志**：支持按站点开启 JSON 访问日志，在面板内按状态码或关键词实时检索请求方法、URI、状态码、耗时与访客 IP。

### 4. 加密备份、多端存储与恢复演练

- **四类备份源**：
  - **数据库实例**：PostgreSQL (`pg_dump -Fc`)、MySQL/MariaDB (`mysqldump --single-transaction`)、MongoDB (`mongodump --archive`)、Redis (`BGSAVE` + RDB 快照提取)。
  - **Docker 数据卷**：通过轻量临时容器将命名数据卷打包归档。
  - **项目子目录**：备份指定项目工作目录下的任意子路径。
  - **面板自身数据**：一键在线热备面板 SQLite 数据库（`VACUUM INTO`）与配置元数据。
- **安全流式管道**：备份数据在内存中流式完成 `zstd` 压缩与 `age` 公钥加密，并计算 `SHA-256` 校验和；即使远端对象存储被攻破，没有离线保存的 `age` 私钥也无法解密任何数据。
- **多存储后端与 GFS 保留**：
  - 支持 **本地目录**、**S3 兼容存储**（AWS S3 / Cloudflare R2 / MinIO / OSS / COS）、**SFTP**、**WebDAV**，支持存储连通性一键测试。
  - 支持 **GFS（日 / 周 / 月）分层保留策略**，每次定时备份完成后自动清理过期历史归档。
- **一键恢复与自动化恢复演练（Restore Drill）**：
  - 支持将数据库备份恢复到**同实例新库**或**覆盖原库**（含 Redis 停机替换 RDB 并禁用 AOF 重写防冲刷），支持数据卷与项目目录一键覆盖恢复。
  - **恢复演练**：可为关键备份策略设定演练计划（如每周一次）。面板会自动从远端存储下载最新备份、解密校验后，在**完全无网络（`--network none`）的沙箱临时容器/临时卷**中执行真实恢复，并运行预设的校验 SQL（如 `SELECT count(*) FROM users`）或文件数量检查。演练通过后将该备份标记为「已演练」，失败则立即发送告警通知。

### 5. 容器运维、监控告警与权限审计

- **容器与资源管理**：
  - 容器列表与实时 CPU / 内存 / 网络 / 磁盘 I/O 速率监控。
  - **实时日志流**：基于 SSE 的低延迟日志查看，支持行数限制、时间戳显示与关键词过滤。
  - **Web 终端**：基于 WebSocket 的交互式终端（自动探测 `bash` / `ash` / `sh`），内置移动端虚拟快捷键栏（`Esc`、`Tab`、`Ctrl+C`、`Ctrl+D`、方向键等）。
  - **在线文件浏览器**：无需进终端即可直接浏览运行中容器或 Docker 命名数据卷的目录结构，支持文件在线上传与打包下载。
  - **强制拉取重建（Recreate）**：支持对单容器一键强制拉取最新镜像并按原参数重建。
  - **Docker 空间治理**：可视化展示镜像、容器、数据卷、构建缓存的总占用与可回收空间比例，支持一键清理（Prune）悬空或未使用资源。
- **历史指标趋势**：每分钟自动采样宿主机与所有运行中容器的 CPU、内存、网络收发速率与磁盘读写速率，保留 7 天历史数据，支持 `1h` / `6h` / `24h` / `7d` 趋势图表查看。
- **多渠道告警中心**：
  - 支持 **Telegram Bot**、**企业微信群机器人**、**钉钉群机器人（含加签）**、**飞书群机器人（含加签）**、**Bark (iOS 推送)**、**SMTP 邮件**、**通用 Webhook（含 HMAC-SHA256 签名）**。
  - 支持按事件类型灵活订阅：部署失败/成功、容器异常退出/不健康、容器 OOM、镜像有新版本、定时任务失败、备份失败、恢复演练失败、证书即将过期、磁盘空间不足（阈值可配）、安全登录事件。
- **多用户 RBAC 与安全访问**：
  - 三级角色：**管理员 (`admin`)**、**运维 (`operator`)**、**只读 (`viewer`)**；非管理员账号可按项目勾选授权范围，实现多项目团队权限隔离。
  - 支持 **TOTP 两步验证（2FA）** 与一次性恢复码、活跃登录会话查看与强制下线、细粒度 API 令牌（支持设置过期时间与 `read` / `deploy` / `admin` 权限范围）。
- **移动端与 PWA 体验**：全站响应式布局，在手机端自动切换为抽屉导航、卡片视图与底部操作栏；支持 Web App Manifest 与 Service Worker，可作为 **PWA 应用**安装到手机主屏幕或桌面。

---

## 快速开始（生产环境部署）

### 环境要求

- **操作系统**：Linux（推荐 Ubuntu 22.04 / 24.04、Debian 12），`x86_64 (amd64)` 架构（ARM64 主机请使用下方「方式二：从源码构建」）
- **运行时**：Docker `24.0+` 及 `docker compose` v2 插件
- **端口要求**：宿主机 `80`、`443` 端口未被占用（供内置 Caddy 监听），首次初始化时需临时访问 `7800` 端口

### 方式一：使用官方预构建镜像（推荐）

VPS 上无需安装编译工具链，仅需下载 `docker-compose.yml` 与引导 `Caddyfile` 两个文件即可启动。镜像由 GitHub Actions 自动构建并发布至 `ghcr.io/speedupmaster/compose-panel`：

```bash
# 1. 创建部署目录并下载编排文件
mkdir -p /opt/compose-panel/deploy && cd /opt/compose-panel/deploy
curl -fsSLo docker-compose.yml https://raw.githubusercontent.com/SpeedupMaster/compose-panel/main/deploy/docker-compose.ghcr.yml
curl -fsSLo Caddyfile https://raw.githubusercontent.com/SpeedupMaster/compose-panel/main/deploy/Caddyfile

# 2. 启动面板与 Caddy
docker compose up -d

# 3. 查看首次启动生成的一次性初始化令牌（Setup Token）
docker compose logs panel | grep -i token
```

**日常升级与版本固定**：

```bash
# 升级到最新版本
cd /opt/compose-panel/deploy
docker compose pull && docker compose up -d
```

> 默认使用 `ghcr.io/speedupmaster/compose-panel:latest`。如需固定版本或回滚，可在 `/opt/compose-panel/deploy/.env` 中写入 `PANEL_IMAGE=ghcr.io/speedupmaster/compose-panel:1.2.3`（对应 Git 标签 `v1.2.3`），随后执行 `docker compose up -d`。

### 方式二：从源码构建

适用于二次开发、自定义修改或非 `amd64` 架构服务器（注意：首次编译前后端需要约 1.5 GB 可用内存，小内存机器建议配置 Swap 或使用方式一）：

```bash
git clone https://github.com/SpeedupMaster/compose-panel.git /opt/compose-panel/src
cd /opt/compose-panel/src/deploy

# 若服务器位于中国大陆，可配置 Go 与 npm 镜像加速构建：
# export GOPROXY=https://goproxy.cn,direct
# export NPM_REGISTRY=https://registry.npmmirror.com

docker compose up -d --build
docker compose logs panel | grep -i token
```

---

## 首次登录与安全加固建议

1. **完成管理员初始化**：
   在浏览器打开 `http://<服务器公网 IP>:7800`，粘贴终端日志中打印的**初始化令牌**，创建首个管理员账号。
2. **绑定面板专属域名并关闭 `7800` 公网暴露**：
   - 将一个域名（如 `panel.example.com`）的 A / AAAA 记录解析到服务器 IP。
   - 进入面板 **「设置 → 面板设置」**，填写**面板域名**（`panel.example.com`）与 **ACME 证书邮箱**并保存，Caddy 会自动为面板申请 HTTPS 证书并配置反向代理。
   - 验证 `https://panel.example.com` 可正常访问后，编辑 `/opt/compose-panel/deploy/docker-compose.yml`，将 `panel` 服务的端口映射改为仅监听本地回环：
     ```yaml
     ports:
       - "127.0.0.1:7800:7800"
     ```
     然后执行 `docker compose up -d`，彻底关闭 `7800` 端口的公网直接访问。
3. **开启两步验证（2FA）**：
   进入 **「访问控制 → 我的账号」**（或右上角个人菜单），使用身份验证器 App（如 Authy、Google Authenticator、1Password）扫码开启 TOTP 两步验证，并妥善保存一次性恢复码。
4. **离线备份两大核心密钥（极重要）**：
   - **主加密密钥 `/opt/compose-panel/data/master.key`**：用于加密 SQLite 数据库中的所有密码与凭据。请将其复制一份到安全的离线密码库中。
   - **备份解密私钥（age Private Key）**：进入 **「备份 → 备份加密密钥」**，将 `AGE-SECRET-KEY-1...` 私钥复制并离线保存。一旦服务器彻底损毁，你需要凭此私钥解密远端对象存储中的备份归档。
5. **配置异地存储与告警渠道**：
   - 在 **「集成 → 外部存储」** 中添加 S3 / R2 / SFTP / WebDAV 存储，并设为定时备份策略的目标。
   - 在 **「集成 → 通知渠道」** 中添加 Telegram / 企业微信 / 钉钉 / 飞书等通知方式，勾选关注的风险事件。

---

## 常用操作指南

### 1. 部署 Web 项目并绑定域名

在 Compose Panel 中，你的业务容器**无需映射宿主机端口（无需写 `ports:`）**：

1. 进入 **「项目 → 新建项目」**，直接粘贴 `compose.yml`（或选择模板 / Git 仓库）。
2. 面板会自动检测 Compose 中的 Web 服务名与端口；你只需在下方填写域名（如 `app.example.com`），点击**「创建并部署」**。
3. 面板会自动将该服务接入 `panel-proxy` 网络，并在 Caddy 中生成指向 `<项目名>-<服务名>:端口` 的 HTTPS 反代站点。

### 2. 让应用连接面板托管的数据库

通过 **「数据库」** 页面创建的 PostgreSQL / MySQL / MariaDB / MongoDB / Redis 实例均位于名为 `panel-db` 的桥接网络中，并以**实例名称**作为网络别名（Hostname）。

在应用项目的 `compose.yml` 中声明外部网络 `panel-db` 即可通过内网直连：

```yaml
services:
  app:
    image: myorg/myapp:latest
    restart: unless-stopped
    environment:
      # 直接使用数据库实例名称（例如 pg-main、redis-cache）作为主机名
      DATABASE_URL: postgres://app:${DB_PASSWORD}@pg-main:5432/app?sslmode=disable
      REDIS_URL: redis://:${REDIS_PASSWORD}@redis-cache:6379/0
    networks:
      - default
      - panel-db

networks:
  panel-db:
    external: true
```

### 3. 配置 Git Webhook 与 CI/CD 远程触发

- **GitHub / GitLab / Gitea Webhook**：
  进入对应 Git 项目的 **「项目详情 → 设置」**，复制 **Webhook URL** 与 **Webhook 密钥**，在代码托管平台的仓库 Webhook 设置中填入（Content-Type 选择 `application/json`）。当有代码推送到项目所配置的分支时，面板将自动拉取最新 Commit 并执行部署。
- **CI/CD 命令行或脚本触发**：
  在 **「访问控制 → API 令牌」** 中创建一个权限范围为 `deploy` 的令牌（前缀为 `cpt_`），在 GitHub Actions 或 GitLab CI 中调用：
  ```bash
  curl -fsSL -X POST \
    -H "Authorization: Bearer cpt_your_token_here" \
    "https://panel.example.com/api/projects/<项目ID>/deploy"
  ```

### 4. 离线手动解密并恢复备份文件

所有启用加密的备份文件均采用标准的 `zstd` + [`age`](https://github.com/FiloSottile/age) 格式（后缀如 `.pgdump.zst.age`、`.sql.zst.age`、`.tar.zst.age`），不依赖面板即可在任何装有 `age` 和 `zstd` 的机器上手动解密恢复：

```bash
# 将离线保存的 age 私钥写入文件 panel-age-key.txt，然后流式解密并解压：
age -d -i panel-age-key.txt backup.pgdump.zst.age | zstd -d > backup.pgdump

# 1. 恢复 PostgreSQL（custom 格式）
pg_restore -h 127.0.0.1 -U postgres -d mydb --clean --if-exists backup.pgdump

# 2. 恢复 MySQL / MariaDB
age -d -i panel-age-key.txt backup.sql.zst.age | zstd -d | mysql -h 127.0.0.1 -u root -p mydb

# 3. 恢复 MongoDB
age -d -i panel-age-key.txt backup.archive.zst.age | zstd -d > backup.archive
mongorestore --archive=backup.archive --drop

# 4. 解包数据卷 / 目录 / 面板备份
age -d -i panel-age-key.txt backup.tar.zst.age | zstd -d | tar -xvf -
```

### 5. 忘记管理员密码或重置 2FA

如果忘记登录密码或丢失两步验证设备，可通过 SSH 登录宿主机执行内置 CLI 命令重置：

```bash
docker exec panel panel reset-password <用户名>
```

该命令会为指定用户生成随机新密码打印在终端、自动关闭该用户的两步验证（2FA），并清空其已有的登录会话。

---

## 环境变量与配置参考

面板容器通过环境变量进行配置（在 `deploy/docker-compose.yml` 或同目录 `.env` 中设置）：

| 变量名 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `PANEL_ROOT` | `/opt/compose-panel` | 宿主机数据根目录（仅在 `docker-compose.yml` 中用于推导下方挂载路径） |
| `PANEL_PORT` | `7800` | 宿主机映射的面板 HTTP 端口（绑定域名后建议改为 `127.0.0.1:7800:7800`） |
| `PANEL_IMAGE` | `ghcr.io/speedupmaster/compose-panel:latest` | `docker-compose.ghcr.yml` 使用的面板镜像地址与版本标签 |
| `PANEL_LISTEN` | `:7800` | 面板进程在容器内的 HTTP 监听地址 |
| `PANEL_DATA_DIR` | `/data/panel` | 存储 `panel.db` (SQLite)、`master.key` 与任务日志的目录 |
| `PANEL_PROJECTS_DIR` | `/data/projects` | 项目工作目录，**容器内外挂载路径必须完全一致**（便于 Docker 守护进程解析相对路径挂载） |
| `PANEL_BACKUPS_DIR` | `/data/backups` | 默认本地备份存放目录，**容器内外挂载路径必须完全一致** |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker Engine API 端点地址 |
| `PANEL_CADDY_ADMIN` | `/run/caddy/admin.sock` | Caddy Admin API 的 Unix Domain Socket 路径 |
| `PANEL_CADDY_DATA` | `/caddy-data/caddy` | Caddy 数据目录（以只读方式挂载进面板容器，用于扫描展示已签发的 SSL 证书） |
| `PANEL_PROXY_NETWORK` | `panel-proxy` | Caddy 与项目 Web 容器互通的 Docker 网络名称 |
| `PANEL_DB_NETWORK` | `panel-db` | 托管数据库实例与应用容器互通的 Docker 网络名称 |
| `PANEL_UPSTREAM` | `panel:7800` | 配置面板域名后，Caddy 反代面板自身时使用的上游地址 |
| `PANEL_HELPER_IMAGE` | `alpine:3.20` | 数据卷打包备份、恢复与文件浏览所使用的轻量辅助容器镜像 |
| `PANEL_DEV_PROXY` | 空 | 本地开发时将非 `/api` 请求反向代理到前端 Vite 开发服务器的地址 |
| `TZ` | `Asia/Shanghai` | 容器时区（影响 Cron 定时任务与日志时间戳） |

---

## 本地开发与测试

### 本地前后端联调

需要本地安装 **Go 1.26+**、**Node.js 20+** 并可访问本地 Docker Daemon：

```bash
# 1. 启动后端（自动将前端请求代理至 Vite 的 :5173 端口）
go mod tidy
PANEL_DATA_DIR=.devdata/panel \
PANEL_PROJECTS_DIR=$PWD/.devdata/projects \
PANEL_BACKUPS_DIR=$PWD/.devdata/backups \
PANEL_DEV_PROXY=http://localhost:5173 \
go run ./cmd/panel

# 2. 另开终端启动前端开发服务器
cd web
npm install
npm run dev
```

打开 `http://localhost:5173`（或 `http://localhost:7800`）即可进行热更新联调。

### 编译单二进制文件

前端构建产物通过 Go `embed.FS`（[`web/embed.go`](web/embed.go)）直接打包进二进制文件中：

```bash
cd web && npm run build && cd ..
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/panel ./cmd/panel
```

### 自动化端到端测试（E2E）

仓库内置了覆盖全部核心流程的自动化测试脚本（**注意：`e2e-reset.sh` 会清空测试机上的面板数据及面板创建的容器与数据卷，严禁在生产环境运行**）：

```bash
# 单元与静态检查
go test ./...
cd web && npm test && npm run build && cd ..

# 在独立的测试 Docker 主机上运行完整 E2E 测试
sh scripts/e2e-reset.sh   # 重置测试环境并启动整套容器
sh scripts/e2e.sh         # 基础链路：初始化 → 部署项目 → Caddy 反代 → 创建 PostgreSQL → 加密备份/校验/恢复 → 重部署与回滚
sh scripts/e2e2.sh        # 进阶链路：监控历史、模板、镜像更新检测、定时任务、文件浏览、SQL 安全拦截、导入导出、恢复演练、Redis 恢复、SFTP/WebDAV、维护模式、访问日志、多副本负载均衡
sh scripts/smoke.sh http://127.0.0.1:7800 <初始化令牌>   # 只读接口冒烟测试
```

---

## 目录结构

```text
.
├── cmd/panel/           # 命令行入口：serve / reset-password / version
├── internal/
│   ├── auth/            # 密码哈希 (bcrypt)、TOTP 两步验证、随机令牌生成
│   ├── backup/          # 存储后端 (本地/S3/SFTP/WebDAV)、zstd+age 流式加密管道、GFS 保留策略
│   ├── compose/         # docker compose CLI 封装与 .panel/override.yml 生成器
│   ├── config/          # 环境变量配置加载
│   ├── dbmgr/           # 5 种数据库引擎的编排生成、账号/库管理、慢查询与安全控制台
│   ├── docker/          # 零第三方 SDK 依赖的 Docker Engine HTTP/Unix Socket 客户端
│   ├── jobs/            # 持久化异步任务队列、互斥锁与实时日志广播
│   ├── notify/          # 7 种通知渠道发送器与 30 分钟告警去重
│   ├── proxy/           # Caddy Admin API 客户端、动态 JSON 配置渲染与证书扫描
│   ├── secret/          # master.key 管理与 AES-256-GCM 凭据加解密
│   ├── server/          # HTTP 路由、RBAC 鉴权、监控采样、部署/备份/恢复演练调度与审计哈希链
│   └── store/           # SQLite Schema 定义与自动迁移
├── web/                 # React + TypeScript + Vite 前端（支持 PWA 与移动端响应式，构建后嵌入二进制）
├── deploy/              # 生产环境部署编排：docker-compose.ghcr.yml（预构建镜像）、docker-compose.yml（源码构建）与 Caddyfile
├── scripts/             # E2E 自动化测试与冒烟测试脚本
└── Dockerfile           # 多阶段构建（Node 前端构建 -> Go 静态编译 -> Alpine 运行时）
```
