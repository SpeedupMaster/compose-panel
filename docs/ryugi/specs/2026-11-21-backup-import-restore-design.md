# 导入备份与恢复：设计规格说明书

## 1. 意图与背景

Compose Panel 已经实现了数据库、Docker 数据卷、项目目录与面板自身数据的加密流式备份（`zstd` + `age` + `SHA-256`），支持本地磁盘、S3、SFTP、WebDAV、Google Drive 五种存储，并支持恢复**本面板 `backups` 表中已有记录**的数据库、数据卷与项目目录。

然而在真实运维中最关键的**换机迁移、服务器损毁重建、跨环境导数据**场景下，恢复闭环仍存在三个断点：

1. 无法在「备份」页上传本地持有的外部备份文件（含 `.zst` / `.zst.age` / `.tar` / `.archive` / `.rdb` / `.db`）并直接恢复，也没有入口提供旧面板的 `age` 解密私钥。
2. 新面板接上原有的本地目录或远端存储（S3 / SFTP / WebDAV / Google Drive）后，无法扫描发现其中已存在的历史备份并纳入面板恢复。
3. 面板自身数据（`panel.db`）明确不支持在线恢复，且未提供 `master.key` 导出与跨机恢复校验流程，重装后只能登录 SSH 手工替换文件。

本次设计的目标是将这三条外部恢复路径完整打通，使管理员无需 SSH 手工执行 `age` / `zstd` / `pg_restore` / `tar` / `sqlite` 即可在面板内安全完成迁移与灾备恢复。

## 2. 目标、非目标与反目标

### 目标（Goals）

- **本地文件导入并恢复**：支持流式上传外部备份文件到选定存储、自动识别格式与加密状态、登记为备份记录，并支持一键进入恢复弹窗。
- **五种存储全量扫描导入**：为本地磁盘、S3、SFTP、WebDAV、Google Drive 统一实现 `Storage.List`，按现有路径/文件名规则推断元数据，支持筛选、修正并批量导入为备份记录。
- **跨机密钥无痛解密**：支持每次恢复/校验时临时粘贴 `age` 私钥（不落库），也支持在「备份加密密钥」页管理历史只读 `age` 私钥（或将导入私钥提升为主密钥）以及查看/导出面板 `master.key`。
- **跨目标灵活恢复与安全覆盖**：支持数据库跨同引擎实例恢复（新库或覆盖）、数据卷与项目目录的新建与覆盖恢复（强制输入目标名确认、覆盖前自动备份、运行中挂载拦截）。
- **面板自身全链路在线恢复**：支持已登录面板内（备份记录 & 系统信息页）、全新部署初始化页（`/setup`，凭 Setup Token）以及离线 CLI（`panel restore-panel`）三种入口恢复 `panel.db`，自动校验 SQLite 完整性与 `master.key` 匹配度，通过原子暂存交换与平滑重启生效。

### 非目标（Non-Goals）

- **不做整机应用数据一键打包迁移**：面板备份只包含 `panel.db`（项目编排、环境变量、站点、证书、数据库实例配置、存储配置、用户与策略）；运行中应用产生的数据卷、宿主机目录与数据库内容仍各自按对应备份恢复。
- **不向远端存储写入额外 `.meta.json` 伴随文件**：保持对象存储目录结构与现有备份 100% 一致，不增加存储写放大或破坏存量备份兼容性。
- **不实现断点续传分片上传协议**：采用原生 HTTP 流式上传（`XHR upload` + 后端边读边算哈希边流式写入目标存储），避免引入额外临时分片存储状态。
- **不做非管理员（`operator` / `viewer`）的导入与恢复权限下放**：外部备份导入、存储扫描、密钥管理与所有恢复操作均限定 `admin`。

### 反目标（Anti-Goals）

- **绝不在未验证 `master.key` 或 SQLite 完整性前覆盖运行中的 `panel.db`**。
- **绝不在容器挂载并运行期间静默清空或换目录覆盖数据卷/项目目录**。
- **绝不将敏感密钥（`masterKey`、`ageIdentity`、`setupToken`）放入 URL Query 参数**。
- **绝不在删除「存储扫描导入」的备份记录时默认误删远端文件**。

## 3. 已敲定的架构决策

| 主题 | 决策 | 来源 |
| :--- | :--- | :--- |
| 面板自身恢复范围 | 支持跨机恢复（可提供原 `master.key` 并自动验密），同时覆盖**首次初始化页（`/setup`）**、**面板内**与**离线 CLI** 三个入口，恢复后平滑重启生效。 | 已确认（推荐方案） |
| 导入备份的存储模型 | 远端扫描与本地上传均统一登记为 `backups` 表记录；本地上传弹窗默认勾选「导入后立即打开恢复」，兼顾大文件可重试与一步直达体验。 | 已确认（推荐方案） |
| 外部 `age` 私钥处理 | 恢复/校验弹窗支持临时粘贴私钥（仅本次任务内存使用、不落库）；同时在「备份加密密钥」页支持导入历史只读解密私钥（并可选提升为主密钥）。 | 已确认（推荐方案） |
| 存储扫描元数据识别 | 纯路径与文件名后缀规则自动推断（零额外伴随文件、兼容全部存量备份），扫描列表与导入确认时允许管理员手动修正类型/格式/标签。 | 已确认（推荐方案） |
| 恢复目标与覆盖策略 | 数据库支持跨同引擎实例恢复（新库/覆盖）；数据卷与项目目录同时支持新建与覆盖恢复（覆盖需输入目标名确认、自动先备份当前数据，且数据卷正被运行中容器挂载时拒绝覆盖）。 | 已确认（推荐方案） |
| 流式解压/解密自动探测 | 通过文件头魔数自动识别 `age` 加密、`zstd` / `gzip` / 无压缩流，使老备份、本地 `_panel/panel-*.db` 快照与外部手工文件统一走同一读取管道。 | 已批准的工程决定 |
| 扫描导入记录的删除保护 | `DELETE /api/backups/{id}` 增加 `?keepFile=1` 参数；前端删除确认弹窗对 `origin='scan'` 的记录默认不勾选「同时删除存储文件」，其余默认勾选。 | 已批准的工程决定 |
| 面板重启生效机制 | 采用「安全关闭 HTTP/Cron/SQLite → 清理 WAL 并原子重命名 `.pending` → 进程内循环重建 `Server` 实例」方案，无需杀容器即可在 Docker、systemd 与本地开发中无差异平滑生效。 | 已批准的工程决定 |
| 项目目录覆盖挂载拦截 | 与数据卷覆盖一致：检查运行中容器的 bind 挂载（解析 `docker.Container.Mounts`），若目标目录与其祖先/子路径重叠，直接拒绝并返回占用容器名，避免 Linux 滞留旧 inode 或 `EBUSY`。 | 已批准的工程决定 |
| 面板恢复后台任务互斥 | 在 `jobs.Manager` 增加活跃检查与冻结门禁：有 `queued` / `running` 后台任务时返回 `409 Conflict` 并列出任务名；空闲时立即冻结新任务提交，避免与 `s.db.Close()` 竞态或强杀进行中的部署。 | 已批准的工程决定 |
| PostgreSQL 明文 `.sql` 覆盖清理 | `PGDMP` 保持 `pg_restore --clean --if-exists`；明文 `.sql` 覆盖时先终止目标库其他连接，再清空非系统 Schema 并按原库 Owner 重建 `public` Schema（保留库本身、Owner 与 `CONNECT` 权限），再通过 `psql` 导入。 | 已批准的工程决定 |

## 4. 约束条件

- **纯 Go 零 CGO**：继续使用 `modernc.org/sqlite`、`filippo.io/age`、`klauspost/compress/zstd` 与标准库 `compress/gzip`，不引入新第三方依赖。
- **大文件流式处理**：本地上传与远端下载必须全程流式传输（`io.Reader` / `io.Pipe`），禁止将完整备份文件读入内存；仅在 SQLite 校验（`panel.db`）或需要随机读取时落盘到 `<DataDir>/tmp/` 并在结束后清理。
- **向后兼容既有 API 与 E2E 脚本**：`POST /api/backups/{id}/restore` 继续兼容既有 `{mode, target, confirm}` 请求体，不破坏 `scripts/e2e.sh` 与 `scripts/e2e2.sh`。
- **审计与权限边界**：导入、扫描、密钥查看/导入/删除、跨目标恢复与面板恢复全部写入哈希链审计日志；未登录状态下仅 `POST /api/setup/restore` 可访问且严格限定 `!s.hasUsers()`、常量时间校验 `setupToken` 并复用登录限流器防暴力破解。
- **路径与容器安全**：项目目录恢复严格防止 `..` 路径穿越与 symlink 逃逸；数据卷解包使用 `--network none` 临时辅助容器；Caddy 反代配置在面板重启后由既有 `syncCaddy` 自动重新同步。

## 5. 详细技术设计

### 5.1 存储后端扩展与统一流式解码（`internal/backup`）

在 `internal/backup/storage.go` 扩展 `Storage` 接口：

```go
type ObjectInfo struct {
    Key     string    // 相对存储根/前缀的正斜杠路径，无前导 '/'
    Size    int64     // 字节数，未知时为 -1
    ModTime time.Time // 修改时间，未知时为零值
}

type Storage interface {
    Put(ctx context.Context, key string, r io.Reader) (int64, error)
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
    Test(ctx context.Context) error
    List(ctx context.Context) ([]ObjectInfo, error)
}
```

五种存储后端的 `List` 约定（均跳过 `.panel-write-test`、隐藏临时文件与 `*.part`，上限默认 5000 条 / 最大递归深度 6 层）：

- `local`：`filepath.WalkDir(l.Root, ...)`，转正斜杠相对路径；保留 `_panel/panel-*.db` 以便扫描面板定时快照；目录不存在时返回空切片而非报错。
- `s3`：`minio.Client.ListObjects(..., Recursive: true)`，按 `s.prefix`（非空时补 `/`）过滤并剥离前缀；忽略目录占位对象（以 `/` 结尾）。
- `sftp`：`sftp.Client.Walk(s.root)`，跳过目录项；路径基于 `s.root` 计算相对 key；根目录不存在返回空切片。
- `webdav`：逐层 `PROPFIND`（`Depth: 1`），解析 XML `multistatus/response` 的 `href`、`collection`、`getcontentlength`、`getlastmodified`；对 `href` 做路径解码并剥离 `base.Path`，避免把当前目录自身当子目录递归。
- `gdrive`：Drive v3 `files.list` 按目录队列遍历，从 `rootID()` 逐段解析 `g.prefix`（前缀目录不存在时返回空切片）；分页拉取 `nextPageToken`，请求 `fields=nextPageToken,files(id,name,mimeType,size,modifiedTime)`，过滤 `trashed = false`，按目录层级拼接相对 key。

**多私钥与自动压缩探测流（`internal/backup/pipeline.go`）：**

- 新增 `OpenStream(rc io.ReadCloser, identities []string) (io.ReadCloser, error)`，并将 `OpenObject` 升级为 `OpenObject(ctx, st, key, encrypted, identities ...string)`（向后兼容单字符串调用）。
- 用 `bufio.NewReaderSize(rc, 64<<10)` 预读头部 22 字节：若 `encrypted == true` 或头部匹配 `age-encryption.org/v1\n`，解析全部非空 `identities` 为 `[]age.Identity` 后调用 `age.Decrypt(br, ids...)`。若全部私钥均无法解密，返回明确提示。
- 解密后的流再次用 `bufio.Reader` 预读 4 字节魔数：`28 B5 2F FD` 走 `zstd.NewReader`；`1F 8B` 走 `gzip.NewReader`；其余按未压缩明文流直接返回。
- **文件名规则推断（`internal/backup/detect.go`）**：`ParseBackupKey(key string) ParsedBackup` 依次剥离 `.age`、`.zst`/`.gz`（`.tgz` 归一为 `.tar` + `gz`），映射核心格式与类型：
  - `.pgdump` / `.dump` / `.backup` → `kind=database, format=pgdump, engineHint=postgres`
  - `.sql` → `kind=database, format=sql`
  - `.archive` → `kind=database, format=archive, engineHint=mongo`
  - `.rdb` → `kind=database, format=rdb, engineHint=redis`
  - `.sqlite` / `.db` / `.sqlite3` → `kind=panel, format=sqlite`
  - `.tar` → `format=tar`；首段目录或文件名以 `volume_` 开头推为 `kind=volume`（并提取卷名提示），否则推为 `kind=dir`
  - 同时从文件名提取 `YYYYMMDD-HHMMSS` 时间戳（兼容 `panel-YYYYMMDD-HHMMSS.db`），无法识别扩展名时标记 `recognized=false`。

### 5.2 密钥环与 `master.key` 管理（`internal/server`）

- 在 `settings` 表新增加密项 `backup_age_extra_identities`，使用 `s.box.Seal` 存储 JSON 数组 `[{id, label, identity, recipient, addedAt}]`。所有解密路径统一通过 `s.allAgeIdentities(tempKey)` 收集「临时私钥（若提供）→ 当前主私钥 → 历史密钥环」。
- 新增接口（全部 `admin` + 审计日志）：
  - `GET /api/backup-key`：保持返回 `recipient` 与 `?reveal=1` 下的 `identity`，新增返回 `extraKeys: [{id, label, recipient, addedAt}]`（绝不下发历史私钥明文）。
  - `POST /api/backup-key/import`：入参 `{label, identity, setPrimary?: bool}`。校验 `age.ParseX25519Identity` 并推导公钥去重；若 `setPrimary=true`，先将原主私钥无损转入历史密钥环，再写入新主密钥。
  - `DELETE /api/backup-key/extra/{id}`：删除指定历史只读私钥。
  - `GET /api/system/master-key?reveal=1`：审计记录 `system.reveal_master_key` 后返回 `<DataDir>/master.key` 的 64 位 hex 内容。

### 5.3 数据模型变更与外部备份导入接口

- **SQLite 增量迁移（`internal/store/store.go`）**：新增 `ALTER TABLE backups ADD COLUMN origin TEXT NOT NULL DEFAULT 'backup'`（取值 `backup` / `upload` / `scan`），在 `backupRow` 中暴露 `origin` 字段。
- **本地上传导入 `POST /api/backups/import/upload`（`admin`）**：
  - 采用原始二进制请求体（`application/octet-stream`）流式接收，元数据通过 Query 传递：`name`（必填）、`storageId`（留空取默认本地存储）、`kind`、`format`、`label`（可选覆盖）。
  - 后端先用 `bufio.Reader` 预读 32 字节检测 `age-encryption.org/v1\n` 魔数，再通过 `io.TeeReader` 边计算 `SHA-256` 和字节数边流式调用 `st.Put(ctx, objectKey, ...)`，对象路径 `imported/<YYYYMMDD-HHMMSS>-<safeName>`。
  - 写入 `backups` 表（`policy_id=NULL`、`origin='upload'`、`status='success'`），记录审计 `backup.import_upload`，返回 `{backup: backupRow}`。
- **远端存储扫描 `POST /api/storage/{id}/scan`（`admin`）**：
  - 使用 60 秒上下文调用 `st.List(ctx)`，比对该存储下已有的 `backups(object_key → id)` 映射，对每个对象调用 `backup.ParseBackupKey`，返回按时间倒序排列的对象列表及 `truncated` 标志。
- **确认导入扫描结果 `POST /api/backups/import/storage`（`admin`）**：
  - 入参 `{storageId, items: [{objectKey, kind, format, label, encrypted, size, startedAt}]}`。校验存储存在与对象路径合法（拒绝 `..`），对同 `(storage_id, object_key)` 自动跳过计数，其余批量写入 `backups`（`origin='scan'`、`status='success'`、`sha256=''`），记录审计 `backup.import_storage`，返回 `{imported: []backupRow, skipped: int}`。
  - 当对 `sha256=''` 的扫描导入备份执行 `POST /api/backups/{id}/verify`（支持请求体可选传 `{ageIdentity}`）且校验/解密成功时，自动回填 `sha256` 与真实 `size`。
- **安全删除 `DELETE /api/backups/{id}`**：新增可选 Query `?keepFile=1`，为 `1` 时仅删除数据库记录而不调用 `st.Delete`。

### 5.4 统一跨目标恢复与覆盖护栏（`POST /api/backups/{id}/restore`）

扩展请求结构并保持对旧载荷 `{mode, target, confirm}` 的完全向后兼容：

```json
{
  "kind": "database | volume | dir | panel",
  "mode": "new | overwrite",
  "instanceId": 12,
  "database": "app_prod",
  "sourceDatabase": "可选，MongoDB 归档内原库名",
  "volume": "app_data",
  "projectId": 5,
  "path": "uploads",
  "target": "旧协议字段，缺省时按 kind 映射到 database/volume/path",
  "confirm": "覆盖模式必填，需与目标名一致",
  "ageIdentity": "可选，本次任务临时解密私钥（不落库）",
  "masterKey": "可选，面板恢复跨机时提供的原 master.key"
}
```

- **数据库跨实例恢复**：
  - 允许选择任意同引擎兼容实例（`pgdump` → PostgreSQL；`sql` → PostgreSQL / MySQL / MariaDB；`archive` → MongoDB；`rdb` → Redis）。
  - PostgreSQL 恢复预读明文流前 5 字节：若为 `PGDMP` 走 `pg_restore`（`overwrite` 沿用 `--clean --if-exists`）；否则走 `psql`（`ImportCmd(..., "sql")`）。若为明文 `.sql` 且 `overwrite`，导入前先执行 `pg_terminate_backend` 断开目标库其他连接、查询库 Owner、`DROP SCHEMA ... CASCADE` 清理所有非系统 Schema（排除 `pg_%` 与 `information_schema`）并按原 Owner 重建 `public` Schema。
  - MongoDB 恢复升级 `dbmgr.Client.RestoreCmd`：当 `sourceDB != ""` 时沿用 `--nsFrom <sourceDB>.* --nsTo <target>.*`；当外部归档未知 `sourceDB` 时使用 `--nsFrom '$db$.$coll$' --nsTo '<target>.$coll$'` 将单库归档映射到目标库（并支持手填原库名覆盖）。
  - `overwrite` 模式校验 `confirm == target`（Redis 校验 `confirm == instance.Name`），若目标库/实例存在则先执行一次自动安全备份（失败即中止恢复）。
- **数据卷新建与覆盖恢复**：
  - `new`：检查卷不存在后创建并解包。
  - `overwrite`：要求 `confirm == volume`；校验目标卷已存在；在 `internal/docker` 的 `Container` 结构体解析 `/containers/json` 已返回的 `Mounts` 字段，枚举所有 `running` 容器，若有任何运行中容器挂载了该卷，直接拒绝并返回占用容器名称列表；自动对当前卷执行一次安全备份；通过 `--network none` 辅助容器清空 `/v` 后流式 `tar -C /v -xf -`。
- **项目目录新建与覆盖恢复**：
  - 允许选择任意 `app` 类型项目与相对路径（严格拦截空路径、`.panel` 与 `..`）。
  - `new`：目标目录不得已存在，直接 `UntarDir`。
  - `overwrite`：要求 `confirm == projectName+"/"+path`（或 `path`）；枚举所有 `running` 容器的 `type=bind` 挂载，若任何宿主机源路径与目标目录存在路径重叠（相同、祖先或子目录），直接拒绝并返回占用容器名称列表；通过检查后若目标目录存在则先自动做一次安全备份；随后解包到同级临时目录 `.panel-restore-tmp-<ts>`，成功后将原目录改名为 `.panel-restore-old-<ts>`、新目录原子重命名到位、最后清理旧目录（失败立即回滚原目录）。

### 5.5 面板自身备份在线恢复、初始化页恢复与 CLI 兜底

1. **前置解包与完整性/主密钥双校验（支持 Dry-Run 预览）**：
   - 将上传文件、本地 `_panel/panel-*.db` 快照或存储中的 `panel` 备份记录流式解密解压到 `<DataDir>/tmp/panel-restore-<ts>.db`，执行 `validatePanelBackup(dbPath, candidateMasterKey)`：
     - 校验前 16 字节为 `SQLite format 3\x00`，只读打开后执行 `PRAGMA integrity_check`（必须返回 `ok`）。
     - 校验核心表（`users`、`settings`、`projects`、`storage_targets`、`sites`、`backups`）存在，且存在至少 1 名未停用的 `admin` 用户。
     - 扫描候选库中的 `v1:` 密文字段样本（`settings.backup_age_identity`、`storage_targets.config`、`db_instances.root_password`、`projects.env`、`notify_channels.config`、`users.totp_secret`）：若用户提供了 `masterKey`（64 位 hex）用 `secret.New` 测试解密，否则用本机当前 `s.box` 测试解密。存在密文但解密失败时直接报错返回 `needMasterKey: true`。
     - 返回摘要统计：`{users, projects, databases, sites, storages, policies, backups, keyMatched, needMasterKey}`。
2. **后台任务门禁、自动前备份与原子暂存（Staged Swap）**：
   - 调用 `s.jobs.TryFreezeForRestore()`：若存在 `queued` / `running` 后台任务，直接返回 `409 Conflict` 并列出活跃任务标题；空闲时原子冻结新任务提交（后续步骤失败时调用 `Unfreeze()` 恢复）。
   - 对当前在线数据库执行 `VACUUM INTO <BackupsDir>/_panel/panel-pre-restore-<ts>.db`（初始化页空库时可跳过），更换 `master.key` 时同步备份旧密钥到 `<DataDir>/master.key.pre-restore-<ts>`（权限 `0600`）。
   - 将校验通过的候选库 fsync 后写入 `<DataDir>/panel.db.pending`；提供新 `master.key` 时以 `0600` 写入 `<DataDir>/master.key.pending`；写入标记 `<DataDir>/panel.restore.clear_sessions`。
   - 记录审计日志（面板内恢复时），返回 `200 OK {restarting: true, summary: ...}`。
3. **安全关闭、替换文件与进程内平滑重启**：
   - 响应写出后延迟约 250ms 触发 `s.requestRestart()`，取消本次 `Server.Run` 的子 `Context`。
   - `Server.Run` 按序停止 Cron、`http.Server.Shutdown`、退出后台循环、关闭 `s.db.Close()`。
   - 执行 `applyPendingPanelRestore(cfg.DataDir)`：删除残留 `panel.db-wal` / `panel.db-shm`，原子重命名 `.pending` 文件（该函数同样在 `server.New` 最早期执行，保障崩溃断电恢复）。
   - `cmd/panel/main.go` 主循环检测到 `srv.RestartRequested()` 后立即重建 `server.New(cfg, web.FS())`：自动跑增量迁移、按标记清空旧 `sessions` 表、加载新 `master.key`、重启调度器并同步 Caddy。
4. **三个入口**：
   - 面板内：「备份 → 备份记录」`kind='panel'` 直接恢复（支持 `dryRun` 预检）；「设置 → 系统信息」新增本地 `_panel` 快照列表 `GET /api/system/panel-backups` 与 `POST /api/system/panel-restore`（快照选取或上传文件，支持 `dryRun=1`）。
   - 初始化页：`POST /api/setup/restore`（仅 `!s.hasUsers()` 时开放，否则 409；`multipart/form-data` 携带 `token`、`file`、`ageIdentity`、`masterKey`、`dryRun`，复用 `tooManyFailures("setup:"+clientIP(r))` 限流与 `auth.ConstantEqual` 校验）。
   - 离线 CLI：`panel restore-panel <备份文件路径> [--master-key <hex或文件路径>] [--age-key <私钥或文件路径>]`。

### 5.6 前端界面与交互方案（`web/src`）

视觉方向严格延续 Compose Panel 现有克制运维工具风格（`#F4F4F2` / `#FFFFFF` / `#18191B` / `#B54708` 琥珀主色），复用 `ui.tsx` 与 `ResponsiveList.tsx` 的 `Modal`、`Card`、`Tabs`、`Select`、`FilePicker`、`Confirm`、`SecretBox`、`ResponsiveTable` / `MobileCard`：

- `pages/Backups.tsx`：顶部操作栏增加「上传备份」与「从存储导入」按钮（`admin` 可见）。
- `components/backup.tsx`（及拆分弹窗组件）：新增 `ImportUploadModal`、`ScanStorageModal`，升级 `RestoreModal`（跨实例/项目选择、数据卷/目录覆盖与确认、可选临时 `age` 私钥、面板预检与 `master.key` 恢复重启重连）、升级 `BackupTable`（`origin` 标签、校验临时私钥、删除保留文件选项）。
- `pages/Integrations.tsx`：「备份加密密钥」Tab 扩充历史只读 `age` 私钥列表与导入弹窗（可选设为主密钥）以及「面板主密钥（`master.key`）」查看复制卡片。
- `pages/Settings.tsx`：「系统信息」卡片新增「恢复面板数据」入口与本地 `_panel` 快照列表。
- `pages/Login.tsx`：`mode === 'setup'` 新增 `创建管理员` | `从备份恢复面板` 分段切换与预检/重启轮询跳登录页流程。

## 6. 交由实现者决定的细节

- Go 文件拆分组织与前端弹窗组件文件拆分。
- WebDAV XML 解析与 Google Drive 分页队列的内部辅助结构与具体超时/缓冲区常量。
- 前端重启等待遮罩与错误重试提示文案润色和轮询退避间隔（建议 1 秒轮询，30 秒超时后提示手动刷新）。
- 单元测试中的 fake/mock 构造方式。

## 7. 验证与测试要求

- `go test ./...` 覆盖全部新增包和边界用例（5 后端 `List`、`OpenStream`、`ParseBackupKey`、上传/扫描导入、`keepFile=1`、密钥环、面板恢复校验/冻结门禁/原子替换/初始化页限流、目录与卷占用拦截、PG 明文 `.sql` Schema 重置）。
- `cd web && npm run build`（含 `tsc --noEmit`）与现有 `node --test web/test/geometry.test.mjs` 全部通过。
- 在真实运行的本地面板实例上实测上传导入、目录扫描导入、`age` 密钥环解密、面板恢复与平滑重启跳登录页流程。

## 8. 本次对话使用的 Artifact 记录

- **设计讨论文档**：`document/backup-import-restore`（路径 `.ryugi/artifacts/document/backup-import-restore/doc.xml`，本地访问 `http://localhost:27319/document/9eOVyQMhYqmNBUkTYBHPd`），第 1 轮已确认全部 5 项核心架构决策并完成独立评审增补，所有决策已完整体现在本文档第 3 节与第 5 节中。
