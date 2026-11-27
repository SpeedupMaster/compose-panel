# 容器日志优化（重建前自动归档 + SSE 断线增量续传）：设计规格说明书

## 1. 意图与背景

Compose Panel 当前的容器日志查看与留存存在两类导致“日志过段时间被清空”的问题：

1. **重建容器导致历史日志彻底丢失**：Docker 默认将容器日志绑定在容器实例上，面板内触发重新部署、镜像自动更新、回滚、强制重建、项目下线（`down`）或删除容器时，旧容器被替换/移除后，其日志随即被 Docker 删除，排障时无法回看重建前的运行记录。
2. **前端 SSE 断线重连或容器短暂重启会清空页面日志**：
   - `web/src/api.ts` 的 `streamLines` 在浏览器 `EventSource` 断线自动重连（`opens++ > 0`）时会回调 `onReset()`，`Container.tsx` 直接清空缓冲并 `setLines([])`，仅按当前 `tail` 重放，导致页面闪白或丢失已累计的历史行。
   - `Container.tsx` 每 10 秒轮询容器详情并将 `running` 放入 `useEffect` 依赖，容器重启瞬间状态短暂变为 `false` 再变回 `true` 时，也会触发重新订阅并清空页面。

本次优化在契合面板现有 `192MiB` 轻量化内存定位的前提下，同时解决上述两类问题。

## 2. 目标、非目标与反目标

### 目标（Goals）

- **重建/删除前自动归档容器日志**：在面板触发项目部署（含手动、Webhook、镜像自动更新、回滚与自动回滚）、项目重建（`recreate`）、项目下线（`down`）与删除单个容器（`remove`）之前，自动将即将被替换或移除的容器日志压缩归档至 `<DataDir>/container-logs/` 并记录到 SQLite。
- **零冗余归档**：仅对容器 ID 实际发生替换或消失的容器提交归档，部署中未被重建的同项目服务不产生重复归档。
- **面板内便捷回看与下载**：
  - 在「容器 → 日志」页直接提供「实时日志 / 历史归档」切换，无缝复用现有筛选、行数、时间戳与下载操作。
  - 在「项目详情 → 概览」的容器卡片提供「历史日志」入口，即使项目已 `down` 或某服务已被移除、没有存活容器，仍可查看与下载该项目的历史归档日志。
- **SSE 断线增量续传不清空**：容器实时日志支持基于 Docker 纳秒时间戳的断点续传与边界精准去重，网络抖动、反代超时重连或容器原地重启时不再清空画面，也不产生重复行。
- **有界资源与自动清理**：单份归档、单服务保留份数、保留天数与总磁盘占用均受上限保护，并在日常维护循环中自动清理孤儿文件与失效索引。

### 非目标（Non-Goals）

- **不做后台常驻容器日志采集器**：不覆盖面板外部工具（如 Watchtower 或宿主机手工 `docker compose up --force-recreate`）触发的容器重建（已与用户确认）。
- **不将容器日志归档纳入面板数据库备份**：`panel.db` 备份仍保持轻量化，不包含 `<DataDir>/container-logs/*.log.gz`；跨机恢复 `panel.db` 后若对应归档文件不存在，优雅提示并在后台维护中清理失效索引。
- **不做全文索引 / 跨容器集中检索平台**：保持单机单二进制简洁定位，筛选仍在前端 `LogView` 内完成。
- **不覆盖 `POST /api/docker/prune`（清理已停止容器）**（我方默认决策）：`docker/prune` 属于主动释放磁盘空间操作，不自动归档被清理的已停止容器。
- **不新增系统设置页配置表单**（我方默认决策）：先采用合理内置上限（见第 3 节），避免引入低频配置复杂度，后续有需要再暴露到设置页。

### 反目标（Anti-Goals）

- **绝不因归档失败或超大日志阻塞/中断正常部署或容器删除**：归档全程 best-effort，受单容器超时、行数与字节上限约束，异常时仅记警告日志并继续主流程。
- **绝不破坏现有任务日志（`/api/jobs/{id}/log`）和部署详情的 SSE 行为**：`streamLines` 保持默认向后兼容，仅容器日志显式启用增量续传模式。
- **绝不绕过项目级权限隔离**：任何归档列表、查看、下载与删除接口均遵循与 `containerFor` / `userCanContainer` 一致的权限边界，且禁止路径穿越。

## 3. 核心架构与决策（我方默认决策均已标注）

| 主题 | 决策 | 来源 |
| :--- | :--- | :--- |
| 归档触发范围 | 覆盖面板内全部会替换/移除容器的操作：`deploy`（手动/Webhook）、`auto_update`、`rollback`（含自动回滚）、`project.recreate`、`project.down`、`container.remove`。 | 已确认方向 + 我方补全细项 |
| 仅归档实际被替换的容器 | 执行 `compose up` / `recreate` / `down` 前先暂存候选容器日志到 `.tmp` 文件；操作结束后（含中途失败）比对容器 ID，仅提交已消失容器的归档，仍存活的未变更容器直接删除临时文件。 | 我方工程决策 |
| 存储格式与位置 | 归档文件存放在 `<DataDir>/container-logs/<id>.log.gz`（Go 标准库 `compress/gzip` 压缩，原始日志保留 Docker RFC3339Nano 时间戳）；元数据存放在 SQLite 新表 `container_log_archives`（schema + migration v22）。 | 我方工程决策 |
| 容量与保留策略 | 单份归档上限：最近 `20,000` 行且解压后不超过 `20 MiB`（单容器归档超时 `15s`），超限时标记 `truncated=1`；空日志（0 字节）跳过归档。保留策略：每个 `(project, service)`（独立容器按 `container_name`）最多保留 `15` 份、最长 `30` 天、全局压缩总大小上限 `256 MiB`（超限时按 `archived_at` 从旧到新清理）。 | 我方工程决策 |
| 删除项目时的归档处理 | 删除整个项目（`DELETE /api/projects/{id}`）时一并清理该项目的历史日志归档记录与 `.log.gz` 文件，防止磁盘残留。 | 我方工程决策 |
| 归档查看入口 | 1）容器详情「日志」页顶部工具栏增加数据源选择器（`实时日志（当前容器）` 与各历史归档）；2）项目详情「概览」的「容器」卡片右上角在存在归档时显示「历史日志 (N)」按钮，弹窗查看/下载/删除（覆盖项目 `down` 无存活容器场景）；3）「Docker 资源 → 容器」页右上角同样提供「历史日志 (N)」入口，复用同一归档弹窗，覆盖独立容器或未纳管 Compose 容器被删除后的回看与清理。 | 我方工程决策 |
| SSE 断线增量续传协议 | 服务端对每条带 Docker 纳秒时间戳的日志发送 `id: <RFC3339Nano>|<同时间戳序号>`；浏览器原生 `EventSource` 重连自动携带 `Last-Event-ID`，同时接口支持 `?after=<RFC3339Nano>|<序号>`（供容器停止/重启后前端续连）。后端调用 Docker Engine 时将 `since` 转为整型 Unix 秒（`cursorTS.Unix()`，保持与所有 Docker Engine 版本 100% 兼容），再在服务端按纳秒时间戳与序号精确去重，避免漏行或重复行。 | 我方工程决策 |
| 容器重启/停止前端行为 | `Container.tsx` 不再因 `running` 变化清空页面，而是把 `running`、`startedAt`、`restartCount` 与 `onStreamEnd`（触发容器状态刷新）传入 `Logs`：`true → false` 保留当前日志等待流自然结束；当 `ended` 后检测到容器重新运行（无论经历 `false → true`，还是「重启」按钮/快速崩溃重启导致的 `startedAt` / `restartCount` 递增）且勾选「实时跟随」时，携带最后游标 `after` 增量续连；点击「继续」取消暂停时立即同步缓冲到界面。 | 我方工程决策 |

## 4. 约束条件

- **纯 Go 零新依赖**：继续使用 Go 标准库 `compress/gzip`、`bufio`、`database/sql` 与现有依赖，不引入新 Go 或 npm 包。
- **内存受控（`GOMEMLIMIT=192MiB`）**：
  - 归档写入全程流式从 Docker `Logs` 读入并写入 `gzip.Writer`，不在内存中缓存整份日志。
  - 读取历史归档 `?tail=N` 使用长度为 `N` 的环形缓冲区，下载 `?download=1` 直接流式解压输出到 `http.ResponseWriter`。
  - 归档写入先写 `<id>.log.gz.tmp` 并关闭刷新后再原子重命名为 `<id>.log.gz`，防止中断留下损坏压缩包。
- **权限与安全**：
  - 查看/下载归档要求 `roleViewer`，且按项目权限校验（`project != ""` 校验 `userCanProjectName`，`project == ""` 独立容器要求管理员或 `all_projects=1`，与 `userCanContainer` 完全对齐）。
  - 删除归档要求 `roleOperator`（同时通过上述项目归属校验）并写入审计日志 `container.log_archive_delete`。
  - 归档文件 ID 格式严格限定为 `cla_[0-9a-f]+`，服务端始终通过数据库记录和校验后的 ID 拼接路径，杜绝路径穿越。

## 5. 详细技术设计

### 5.1 Docker 客户端兼容（`internal/docker/client.go`）

Docker Engine 原生 HTTP 接口 `GET /containers/{id}/logs` 的 `since` 参数按 Unix 时间戳解析（原始 RFC3339Nano 字符串在直连 daemon 时会被拒返回 400）。因此 `Client.Logs(..., since int64)` **保持既有 `int64` 签名不变**，仅传整型 Unix 秒数（`cursorTS.Unix()`），由面板服务端在流式读取阶段对返回行的完整 RFC3339Nano 前缀执行纳秒级精确边界去重。

### 5.2 容器日志 SSE 增量续传（`internal/server/containers.go` & `web/src`）

1. **游标格式与解析**：
   - Docker 开启 `timestamps=1` 时，每行以 `<RFC3339Nano UTC> ` 开头（例如 `2026-11-27T02:51:13.123456789Z hello`）。
   - 定义游标 `<ts>|<seq>`（如 `2026-11-27T02:51:13.123456789Z|2`，表示该纳秒时间戳已发送第 2 条，从 1 开始计数）。
   - 解析函数 `parseLogCursor(raw string) (ts time.Time, seq int, ok bool)` 严格校验 `time.Parse(time.RFC3339Nano, tsStr)` 与 `seq >= 1`。
2. **后端 `handleContainerLogs`**：
   - 优先读取 `r.Header.Get("Last-Event-ID")`，若为空再读取 Query `after`；若均为空再读取传统 Query `since`（整型 Unix 秒数，兼容既有调用）。
   - **当存在有效续传游标 `(cursorTS, cursorSeq)` 时**：
     - 计算 `sinceSec := cursorTS.Unix()`（若 `sinceSec < 0` 置 0），调用 `s.dk.Logs(r.Context(), c.Id, "all", follow && c.State.Running, true, sinceSec)`。
     - 过滤边界行：
       - 若行时间戳 `< cursorTS`：跳过。
       - 若行时间戳 `== cursorTS` 且同时间戳计数 `<= cursorSeq`：跳过。
       - 其余行正常下发，并维护 `(lastTS, sameTSSeq)` 随每条 SSE 事件输出 `id: <lastTS>|<sameTSSeq>\n` 与 `data: ...`。
     - 注意：如果某一行不含有效时间戳，仅在已越过 `cursorTS` 边界后透传（未越过前不误丢也不更新 `id:`），保证续传不重不漏。
   - **常规首次连接（无游标）**：
     - 按请求 `tail` 拉取，正常随每条带时间戳日志输出 `id: <lastTS>|<sameTSSeq>\n`。
3. **前端 `web/src/api.ts` & `web/src/pages/Container.tsx`**：
   - `streamLines` 签名向后兼容扩展第 5 个可选参数 `opts?: { resetOnReconnect?: boolean }`（默认 `true`，`JobDrawer` 与 `DeploymentDetail` 行为完全不变）：
     - `resetOnReconnect: false` 时，`es.onopen` 重连不再触发 `onReset?.()`（依赖浏览器原生 `Last-Event-ID` 增量续传）。
   - `Container.tsx` 的 `Logs` 组件接收 `{ id, running, startedAt, restartCount, onStreamEnd }`：
     - 维护 `cursorRef`（记录当前缓冲中最后一条带时间戳日志的 `<ts>|<seq>`）与 `resumeEpoch` 状态。
     - 切换容器 `id`、行数 `tail`、跟随开关 `follow` 或切换归档源 `source` 时重置缓冲并发起新连接；实时模式下传 `{ resetOnReconnect: false }`。
     - 当 SSE 收到 `event: end`（`ended = true`）且仍处于实时跟随模式时，调用 `onStreamEnd()` 触发一次容器详情刷新（并在短暂延迟如 1.5s 后若仍处于重启中再刷新一次，最多 2 次），以便及时感知快速原地重启。
     - 监听 `[running, startedAt, restartCount, ended, follow, source]`：
       - `running` 从 `true → false`：不清空画面，让现有流自然结束。
       - 当 `source === 'live' && follow && ended && running` 且检测到新启动信号（`running` 由 `false` 变为 `true`，或 `startedAt` / `restartCount` 相对建流时发生变化）时：保留当前 `buf` 与 `lines`，递增 `resumeEpoch` 使用 `?tail=all&follow=1&after=<cursorRef.current>`（若 `cursorRef` 为空则按 `tail`）发起增量续连。
     - 切换 `paused` 从 `true → false` 时，立即将 `buf` 刷新到 `lines`，避免必须等到新日志到来才更新画面。

### 5.3 数据库表结构与迁移（`internal/store`）

在 `internal/store/schema.sql` 增加建表与索引，并在 `internal/store/store.go` 增加 migration `22`（对存量数据库安全幂等执行）：

```sql
CREATE TABLE IF NOT EXISTS container_log_archives (
  id             TEXT PRIMARY KEY,
  project        TEXT NOT NULL DEFAULT '',
  service        TEXT NOT NULL DEFAULT '',
  container_id   TEXT NOT NULL,
  container_name TEXT NOT NULL,
  image          TEXT NOT NULL DEFAULT '',
  reason         TEXT NOT NULL,
  started_at     INTEGER NOT NULL DEFAULT 0,
  archived_at    INTEGER NOT NULL,
  size_bytes     INTEGER NOT NULL DEFAULT 0,
  raw_bytes      INTEGER NOT NULL DEFAULT 0,
  line_count     INTEGER NOT NULL DEFAULT 0,
  truncated      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cla_project_service ON container_log_archives(project, service, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_cla_name ON container_log_archives(container_name, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_cla_archived_at ON container_log_archives(archived_at);
```

### 5.4 归档存储、生命周期钩子与清理（`internal/server/log_archives.go`）

新增独立文件 `internal/server/log_archives.go` 集中管理归档逻辑：

1. **暂存与提交两阶段接口**：
   - `stagedLogArchive` 持有候选容器元信息与暂存路径 `<DataDir>/container-logs/<id>.log.gz.tmp`。
   - `stageContainerLog(ctx, containerMeta, reason) *stagedLogArchive`：
     - 先检查该 `container_id` 是否已归档，若已存在直接跳过。
     - 使用 `context.WithTimeout(ctx, 15*time.Second)` 调用 `s.dk.Logs(sctx, id, "20000", false, true, "")`。
     - 流式逐行读取（`bufio.Scanner`，单行缓冲上限 `1 MiB`）写入 `gzip.Writer`，累计 `rawBytes` 和 `lineCount`；
       - 若 Docker 返回达到 `20000` 行或 `rawBytes` 达到 `20 MiB`（单行写入后触发截断），停止读取并标记 `truncated = 1`。（注：为保留最新日志，若因 20 MiB 字节上限中断，记录 `truncated = 1`；常规场景下 20,000 行本身由 Docker 从末尾截取，已是最新的 20,000 行。）
     - 若 `lineCount == 0`，删除 `.tmp` 返回 `nil`。
     - 刷盘关闭 `gzip.Writer` 与文件句柄，记录压缩后 `sizeBytes`。
   - `stageProjectLogs(ctx, project, service, reason) []*stagedLogArchive`：
     - 列举 `s.dk.ProjectContainers(ctx, project)`，过滤掉 `com.docker.compose.oneoff == "True"` 及不匹配 `service`（当 `service != ""` 时）的容器，逐一调用 `stageContainerLog`。
     - 为取到准确的 `StartedAt`，通过 `InspectContainer` 补齐（失败时 `started_at = 0` 不阻断）。
   - `finalizeProjectLogs(ctx, project, staged, run)`：
     - 重新拉取当前 `s.dk.ProjectContainers(ctx, project)` 构建存活 `aliveIDs` 集合。
     - 若 `staged[i].containerID` 不在 `aliveIDs` 中：原子 `os.Rename(.tmp → .log.gz)` 并 `INSERT INTO container_log_archives`，在 `run != nil` 时记录一条简洁任务日志（如 `已归档旧容器日志：telegram-bridge（142 行，12 KB）`）。
     - 若仍在 `aliveIDs` 中：删除 `.tmp` 文件（该容器未被重建）。
     - 最后调用 `s.pruneLogArchives()` 执行保留策略清理。
   - `commitStagedLog(st)` / `discardStagedLog(st)`：供单容器删除使用。
2. **四个接入点**：
   - `deploy.go`：`composeUp(run, p, reason)`（`reason` 取值：回滚或自动回滚为 `"rollback"`，镜像自动更新为 `"auto_update"`，其余为 `"deploy"`）：在 `pull` 完成、`up -d` 执行前调用 `stageProjectLogs`，并在 `defer` 中用独立 10s context 调用 `finalizeProjectLogs`（确保即使 `up -d` 中途失败或被取消，已被替换掉的容器日志依然能妥善提交，未替换的则清理 `.tmp`）。
   - `projects.go`：`handleProjectAction`：
     - `action == "recreate"`：在 `up -d --force-recreate` 步骤前调用 `stageProjectLogs(ctx, p.Name, service, "recreate")`，步骤后 `defer finalizeProjectLogs`。
     - `action == "down"`：在 `down` 步骤前调用 `stageProjectLogs(ctx, p.Name, "", "down")`，步骤后 `defer finalizeProjectLogs`。
   - `containers.go`：`handleContainerAction`（`action == "remove"`）：在 `RemoveContainer` 前调用 `stageContainerLog(ctx, ..., "remove")`；删除成功后提交并 prune，删除失败则丢弃 `.tmp`。
   - `projects.go`：`deleteProjectJob`：项目删除成功后调用 `s.deleteProjectLogArchives(p.Name)` 清理该项目所有归档记录与文件。
3. **保留策略与孤儿清理**：
   - `pruneLogArchives()`（串行加锁 `logArchiveMu`）：
     1. 删除超过 30 天（`archived_at < now - 30d`）的归档。
     2. 按分组（`CASE WHEN project<>'' AND service<>'' THEN 'svc:'||project||'/'||service ELSE 'name:'||container_name END`）每个分组仅保留最新 15 份，删除多余旧归档。
     3. 若全局压缩文件总大小超过 `256 MiB`，按 `archived_at ASC` 依次删除最旧归档直到低于阈值。
     4. 每删除一条记录时同步删除对应的 `<id>.log.gz`。
   - `cleanupOrphans(ctx)` / `housekeep(ctx)`（`internal/server/monitor.go`）：
     - 沿用既有 `orphanTempMaxAge`（`6 * time.Hour`，不改动现有常量）清理 `<DataDir>/container-logs/*.tmp` 中超龄的残留临时文件，避免误删耗时较长的部署中的暂存文件。
     - 清理 DB 中对应 `.log.gz` 文件已缺失的失效记录（兼容 `panel.db` 跨机恢复）。
     - 清理磁盘上未被 DB 引用且修改时间超过 `orphanTempMaxAge`（6 小时）的孤儿 `.log.gz` 文件。
     - 周期执行 `pruneLogArchives()`。

### 5.5 HTTP API（`internal/server/routes.go` & `internal/server/log_archives.go`）

- `GET /api/containers/{id}/log-archives`（`roleViewer`）：
  - 通过 `containerFor` 校验容器存在与权限；按 `(project, service)`（有 compose 标签时）或 `(project='', container_name)` 查询归档列表（按 `archived_at DESC` 排序）。
- `GET /api/projects/{id}/log-archives`（`roleViewer`）：
  - 通过 `projectForUser` 校验权限；返回该项目全部归档（按 `archived_at DESC` 排序）。
- `GET /api/log-archives`（`roleViewer`）：
  - 返回当前用户有权限查看的全部归档（按 `archived_at DESC` 排序），供「Docker 资源 → 容器」页查看已删除独立容器或未纳管 Compose 容器的历史日志；严格按用户项目权限过滤（非管理员且限定项目的用户仅看到自己有权限的项目归档，看不到独立容器或未纳管项目归档）。
- `GET /api/log-archives/{id}`（`roleViewer`）：
  - 校验归档存在及项目归属权限；检查磁盘 `.log.gz` 存在（缺失返回 404）。
  - `?download=1`：流式解压返回 `text/plain; charset=utf-8` 附件，文件名 `<containerName>-<YYYYMMDD-HHMMSS>.log`。
  - 默认 JSON：支持 `?tail=200|500|2000|10000|all`（默认 `500`），流式解压并用环形缓冲读取最后 N 行，返回 `{archive, lines, totalLines}`。
- `DELETE /api/log-archives/{id}`（`roleOperator`）：
  - 校验项目归属权限，删除文件与数据库记录，记录审计日志 `container.log_archive_delete`。

### 5.6 前端界面与交互（`web/src/pages/Container.tsx`、`ProjectDetail.tsx` & `DockerResources.tsx`）

严格延续 Compose Panel 现有视觉体系（复用 `Select`、`LogView`、`Modal`、`Confirm`、`bytes`、`dateTime`），并抽出一份共用的 `LogArchivesModal` 组件供项目详情页与 Docker 资源页复用：

- **容器「日志」页（`Container.tsx`）**：
  - 加载 `/containers/${id}/log-archives`。当存在归档时，在顶部工具栏左侧显示数据源 `Select`：
    - `实时日志（当前容器）`
    - `<原因文案> · <MM-DD HH:mm>（<N> 行）`（原因映射：`deploy`→部署前、`auto_update`→自动更新前、`rollback`→回滚前、`recreate`→重建前、`down`→下线前、`remove`→删除前）
  - 切换到历史归档时：
    - 请求 `GET /api/log-archives/${archiveId}?tail=${tail}` 渲染到同一个 `LogView`，`筛选`、`最近 N 行`、`时间戳`、`下载`（指向 `/api/log-archives/${archiveId}?download=1`）全部可用。
    - 隐藏「实时跟随」和「暂停」控件；在顶部或卡片内展示归档摘要条（原容器短 ID、镜像、归档时间、文件大小；若 `truncated` 给出截断提示）；对 `can.operator` 提供「删除」按钮及二次确认。
  - 切回「实时日志」时，恢复实时 SSE 增量流。
- **项目详情「概览」页（`ProjectDetail.tsx`）**：
  - 请求 `/projects/${id}/log-archives`。当存在历史归档时，在「容器」卡片右上角显示「历史日志 (N)」按钮（项目 `down` 后无存活容器时依然可见）。
  - 点击弹出 `LogArchivesModal`：支持按服务/归档选择、关键词筛选、行数、时间戳切换、下载与删除（`can.operator`）。
- **Docker 资源「容器」页（`DockerResources.tsx`）**：
  - 请求 `/log-archives`。当存在历史归档时，在容器列表右上角显示「历史日志 (N)」按钮，点击复用同一 `LogArchivesModal`，确保独立容器或未纳管 Compose 容器被删除后依然可以回看、下载与清理归档。

## 6. 交由实现者决定的细节

- `internal/server/log_archives.go` 内部的辅助函数划分与归档 ID 生成（复用 `auth.RandomToken`）。
- 归档模态框是否拆为 `web/src/components/` 小组件或在 `ProjectDetail.tsx` 内联实现。
- 单元测试中针对 Docker HTTP 服务的 `httptest.Server` fake 实现方式。
- `20 MiB` 截断发生在单行中途时的行尾换行处理。

## 7. 验证与测试要求

- **后端测试（`go test ./...`）**：
  - SQLite schema 与 v22 迁移测试（含存量库升级）。
  - `parseLogCursor` 与 SSE `Last-Event-ID` / `after` 纳秒级边界去重、续传测试。
  - 归档两阶段（暂存 → 仅提交被替换容器 / 丢弃未变更容器）、空日志跳过、截断标记、保留策略（15 份 / 30 天 / 总大小上限）、孤儿/缺失文件清理、项目权限隔离与下载/删除接口测试。
- **前端测试与构建**：
  - `node --test web/test/api.test.mjs` 新增测试覆盖 `streamLines` 的 `resetOnReconnect: false` 行为及既有默认行为不回退。
  - 补充容器日志游标追踪与状态合并单元测试。
  - `cd web && npm run build`（含 TypeScript 类型检查）零错误通过。
- **端到端验证**：
  - 启动本地编译后的面板（或针对实际路由与 UI 渲染验证）检查实时日志、SSE 重连不清空、历史归档切换/下载/删除以及项目页历史日志弹窗。
