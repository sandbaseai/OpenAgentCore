---
title: "运维"
source: docs/getting-started/operations.md
source_hash: ecbfd57ce0369d862014d0525eebf6dd4d860c9eb4a9e12bfcd049e1cc15824e
---

Core 安装运维人员的日常任务：`oac` 命令、服务健康状态、Core 密钥、备份和卸载。节点有[专门的指南](./nodes.md)，[故障排查](./troubleshooting.md)列出了安装程序和访问方面的提示信息。设置见[配置参考](../configuration.md)。

## oac 命令 {#the-oac-command}

每个安装目录都有自己的 `oac` 命令（Windows 为 `oac.exe`）。以安装 Core 的账号运行它；它需要 Docker 访问权限，不需要 root。示例使用默认安装目录 `~/.oac/core`；Windows 上使用 `& "$HOME/.oac/core/oac.exe"` 调用该命令，后接相同参数。使用自定义目录时，替换路径。

| 命令 | 功能 |
| --- | --- |
| `oac install` | 安装，或继续一个在启动前停止的安装；安装脚本会运行它 |
| `oac apply` | 检查数据卷和 `.env`，然后启动服务，并重新创建配置有变化的服务；参见 [oac apply 的工作方式](../configuration.md#how-oac-apply-works)。校验失败时不改动任何服务。成功时打印 `Services are running with the current settings.` |
| `oac core-key [--show]` | 指出 Core 密钥在数据卷中的位置；加上 `--show` 时打印密钥本身 |
| `oac rotate-core-key` | 替换 Core 密钥并重启 Core 和 Web，然后说明如何读取新密钥 |
| `oac uninstall` | 确认后删除安装及其数据；参见[卸载](#uninstall) |

Docker Compose 命令管理服务；针对该安装的 Compose 文件运行它们。`stop` 和 `start` 见[停止与重启](#stop-and-restart)。

| 命令 | 功能 |
| --- | --- |
| `docker compose -f ~/.oac/core/compose.yaml ps` | 展示服务 |
| `docker compose -f ~/.oac/core/compose.yaml logs --tail 200 core` | 展示服务的日志 |
| `docker compose -f ~/.oac/core/compose.yaml down` | 移除容器。数据保留 |

本地凭据和注册解析完成后，Runtime 的 Harness 发现与已认证的引导 HTTP 请求并发执行。两者都成功后才建立连接并发布能力。失败会取消另一项操作并等待其清理。执行器仍由连接生命周期持有。Runtime 启动变更需要重新构建并验证 Runtime 模板。


## Runtime 启动延迟 {#runtime-startup-latency}

daemon 通过 `executor preparation stage` 记录 `stage=workspace`、executor 和 Session ID、毫秒耗时及 `success`。Codex 通过 `codex preparation stage` 记录 `session_plan`、`model_catalog`、`process_spawn`、`rpc_initialize` 和 `verification`，携带所属请求的 trace、耗时及 `success`。这些记录不包含原生错误文本、凭据、配置、模型目录内容或命令输出。未执行的条件阶段表示未观测，不能按零计算。`session_plan` 包含 `model_catalog`；executor 就绪耗时包含工作区准备、adapter 各阶段和传输开销，不应重复相加。失败的 `rpc_initialize` 包含必要的子进程清理。

`model_catalog` 内，`model_catalog_command` 包含原生命令执行、退出和管道排空；只有进程已启动才输出 `model_catalog_cleanup`，它仅测量既有尽力发送的进程组信号。其 `success` 是信号调用的返回结果，进程组已退出也可能为 false；不证明后代已回收，也不改变命令结果。`model_catalog_validation` 包含 JSON、模型、verbosity 及托管 home 校验；`model_catalog_snapshot` 包含快照创建、写入、关闭及绑定 Session plan。失败保持原有拒绝和清理行为。

`rpc_initialize_write`、`rpc_initialize_response` 从同一请求起点累计计时，不能相加，也不是 Core 网络 RTT。仅在匹配响应 ID 时、原生结果解码前记录响应时刻；其 `success` 只表示帧中无原生错误，不代表握手有效。原有 `rpc_initialize` 父阶段记录完整解码握手及失败所需清理。没有匹配响应时不输出响应项。读取协程只捕获时间，不写日志；请求结束后才导出记录，因此日志时间是导出时刻。initialize 的 10 秒期限及 Session 持有的进程生命周期保持不变。

app-server 初始化前仍会校验并固定模型目录。阶段计时用于区分目录准备与原生进程初始化，本身不证明提速。应在相同 Runtime 模板、模型和 Provider 下对比全新及复用 Session，并同时验证持久化回复、用量和首字延迟。Runtime 启动逻辑变更需要重新构建和验收 Runtime 模板；仅替换 Core 不会更新既有沙箱。

## 服务健康状态 {#service-health}

根据不同问题使用这些观察：

| 观察 | 能证明什么 |
| --- | --- |
| `docker compose ps` | 数据库接受就绪检查，Core 和 Web 各自响应自己的 `/healthz` |
| Core `/healthz` | Core 进程存活 |
| 经过认证的 API 读取 | 调用者的密钥适用于该资源 |
| Environment 连接 | Runtime 传输已连接 |
| 已完成的 Turn 及结果 | 任务已记录的结果 |

只有 Web 是监听器；参阅[配置公开地址](./public-address.md#listeners-and-access)。服务健康状态不表示 Harness 或模型可用。执行情况使用 Session、Turn、Items 和 Usage 读取，节点连接、就绪和分配使用 Web 的 **节点** 页面。本地诊断使用安装自身的 Compose 文件：

```sh
docker compose -f "$HOME/.oac/core/compose.yaml" ps --all
docker compose -f "$HOME/.oac/core/compose.yaml" logs --tail 200 core
docker compose -f "$HOME/.oac/core/compose.yaml" logs --timestamps init
```

`init` 服务在初始化完成后退出。其步骤日志见[部署说明](https://github.com/MiniMax-AI/OpenAgentCore/blob/main/deploy/README.md#installation)。

不要将 `docker compose config`、`docker inspect` 或原始日志粘贴到公开问题报告。

## 请求诊断 {#request-diagnosis}

打开 **监控 → 请求日志** 查看近期请求，并复制请求 ID 与 Core 服务日志关联。[请求日志](../../../contracts/agents-api/zh/admin-api.md#request-log)定义范围、隐私及有限的内存保留方式。请求接受了异步 Turn 时，请到 **Session 日志** 查看执行结果；HTTP 接受不等于任务成功。跨重启或 Core 实例的留存使用已有日志采集系统。

### Environment 文件拒绝日志 {#environment-file-rejection-logs}

Environment 文件创建 handler 写出HTTP状态码为400或以上的响应时，会发出一条 `api_request_rejected` 事件。用其中的 `request_id` 关联响应的 `X-Request-Id`；该ID由Core生成，不一定与调用方的请求ID相同。既有日志上下文还会注入 `trace_id` 和 `span_id`；无论请求携带有效的入站trace，还是由Core生成新trace，这两个字段均与响应的 `traceparent` 一致。事件覆盖 `operation=environment_file_create`，路由固定为模板 `/v1/agents/environments/{environment_id}/files`。进入该handler之前被拒绝的请求不在其覆盖范围内。

事件包含 `operation`、`route`、`status`、白名单内的 `code`（未识别的code记为 `unknown`）以及静态 `reason`。reason取值为 `unknown`、`invalid_payload`、`unknown_field`、`invalid_path`、`invalid_base64`、`inline_too_large`、`hosted_environment_provisioning`、`destination_directory` 和 `destination_unsafe`。`unknown` 表示拒绝原因尚未分类；仅凭400不能判定Environment仍在准备中。

事件不记录请求或响应正文、查询字符串、原始URL路径、资源ID、用户文件路径、字段值、任意错误文案或凭据，也不改变HTTP响应和执行行为。使用前应核实已部署的Core版本包含此诊断；缺少事件不能证明成功，诊断信息本身也不能证明文件上传可用。

## 停止与重启 {#stop-and-restart}

计划重启前，先等待活动工作结束：

```sh
docker compose -f ~/.oac/core/compose.yaml stop
docker compose -f ~/.oac/core/compose.yaml start
```

停止 Core 不会停止节点或沙箱。节点服务、microVM 和 Docker 容器继续运行；停止不是回收计算资源的方法。Core 重启不会透明地继续被中断的原生工具调用。重连后查询同一 Session；不要创建新 Session 来重放结果不确定的工作。Session 事件流仅提供实时事件；通过读取 Session、Turn 和 Items 恢复。

Web 重启（包括 `oac apply` 引起的重启）会让所有控制台用户退出登录；参阅[登录](../web/console-server.md#sign-in)。

## Core 密钥 {#core-key}

每个安装有一个管理员凭据，即 Core 密钥。安装程序在 `secrets/web/core.key` 生成以 `oac_admin_` 为前缀、后接 64 个随机小写十六进制字符的密钥。用 `oac core-key --show` 读取；该文件属于容器用户。Core 密钥：

- 用于登录 Web。浏览器获得 HttpOnly 会话 cookie，不持有密钥；
- 通过 `Authorization: Bearer <Core key>` 授权 Core API（`/core/v1`）请求；
- 不授权 Agents API（`/v1`）。应用使用项目 API 密钥，后者也不能调用 `/core/v1`。

请保密。Web 读取 `secrets/web/core.key`。Core 只读取 `secrets/core/core-key-digests.json` 中的 SHA-256。Core 密钥至少 32 字符且不含空白。Web 限制失败登录。

### 用脚本调用 Core API {#script-the-core-api}

在 Core 主机上，此辅助函数在 Core 的网络命名空间中运行 `curl`，并通过 stdin 传入密钥，使其不进入命令行：

```sh
core() (  # core METHOD PATH [JSON body]
  cd ~/.oac/core
  ./oac core-key --show | sed 's/^/Authorization: Bearer /' |
    docker run -i --rm --network "container:$(docker compose ps -q core)" curlimages/curl \
      -fsS -X "$1" "http://127.0.0.1:8091/core/v1$2" -H @- -H 'Content-Type: application/json' ${3:+-d "$3"}
)
```

| 任务 | 命令 |
| --- | --- |
| 列出项目 | `core GET /projects` |
| 创建项目 | `core POST /projects '{"name": "billing-bot"}'` |
| 签发 API 密钥（仅显示一次，字段为 `key`） | `core POST /projects/$PROJECT_ID/keys '{"name": "prod"}'` |
| 撤销密钥 | `core DELETE /projects/$PROJECT_ID/keys/$KEY_ID` |
| 归档项目（撤销全部密钥） | `core POST /projects/$PROJECT_ID/archive` |
| 查看 Harness 及默认模型 | `core GET /harnesses` |
| 设置 Codex 默认模型 | `core PUT /harnesses/codex/model-configuration '{"model": "your-model-id", "model_provider": {"protocol": "responses", "base_url": "https://provider.example/v1", "api_key": "sk-..."}}'` |
| 安装信息，包括 API 基础 URL | `core GET /installation` |

[Core 管理 API](../../../contracts/agents-api/zh/admin-api.md)列出全部路由；错误使用 [Core 错误封装](../../../contracts/agents-api/zh/core-errors.md)。

### 轮换 Core 密钥 {#rotate-the-core-key}

```sh
~/.oac/core/oac rotate-core-key
```

它在初始化容器中更新数据卷内的 `secrets/web/core.key` 和 `secrets/core/core-key-digests.json`，然后重启 Core 与 Web。Core 重启后旧密钥立即失效，控制台会话也会结束；请重新登录并更新脚本。

## 项目和 API 密钥 {#projects-and-api-keys}

在 Web 的 **项目与密钥**，或通过 [Core API](#script-the-core-api)创建项目、签发密钥。项目和密钥行为见 [项目拥有资产](../concepts.md#projects-own-assets)。

轮换应用密钥：

1. 在同一项目签发新密钥。
2. 更新应用以使用它。
3. **撤销** 旧密钥。

**归档** 禁用项目的所有密钥并保留资产。

Core 记录每次公开资源写入所使用的密钥；历史保留策略为 [`OAC_WRITE_AUDIT_RETENTION`](../configuration.md#settings)。

## 备份 {#back-up}

可恢复的备份必须是一组在停止写入期间取得的一致数据。仅数据库转储或 Core 卷导出不包含独立工作区存储及节点计算状态。恢复数据卷时，同时恢复同一份备份中的安装目录，其中保存着写入该数据的版本。

### 单机备份与恢复 {#single-host-backup-and-restore}

如果安装的全部数据都在其 Docker 数据卷中，没有独立工作区存储，也没有需要保留的节点状态，那么数据卷加安装目录就是完整备份。以下流程使用 Unix shell，以及该安装已拉取的 PostgreSQL 镜像。

```sh
cd ~/.oac/core
docker compose stop
project=$(sed -n 's/^COMPOSE_PROJECT_NAME=//p' .env)
image=$(docker compose images --quiet database)
mkdir -p ~/oac-backup
docker run --rm -v "${project}_data:/data:ro" -v ~/oac-backup:/backup --entrypoint tar "$image" czf /backup/data.tar.gz -C /data .
cp -a ~/.oac/core ~/oac-backup/installation
docker compose start
```

要在本机或另一台主机上恢复，先[卸载](#uninstall)仍在使用该项目的安装，然后：

```sh
cp -a ~/oac-backup/installation ~/.oac/core
cd ~/.oac/core
project=$(sed -n 's/^COMPOSE_PROJECT_NAME=//p' .env)
docker compose pull
docker compose up --no-start
image=$(docker compose images --quiet database)
docker run --rm -v "${project}_data:/data" -v ~/oac-backup:/backup:ro --entrypoint tar "$image" xzf /backup/data.tar.gz -C /data
./oac apply
```

带有独立工作区存储或节点的安装需要下面的完整流程。

### 备份内容 {#backup-contents}

- Docker 卷 `<project>_data`，包括 `database/`、`secrets/` 和 `state/`。其中保存项目、密钥摘要、节点、默认模型、加密凭据和执行历史（含大对象）。`secrets/core/credential.key` 必须与匹配的数据库一起保留，否则存储的凭据无法解密。
- 安装目录，包括 `.env`、`compose.yaml`、Compose 覆盖文件和管理命令。
- 当前或保留对象所使用的每个独立工作区命名空间，即使其位于安装卷之外。备份整个根目录，包括 `.oac-storage-root`、对象标识、staging、live 数据、trash 和删除标记。保留数值所有者、权限、链接及扩展属性，包括全部 `user.*` 属性。仅复制 `live/data` 会丢失生命周期证据，并可能使已删除标识复活。[工作区存储](../workspace-provider.md#kernel-nfs-adapter)负责定义布局与所有权要求。
- 对于[配额模式](../workspace-provider.md#quota-mode)命名空间，将承载导出的整个 XFS 文件系统作为一份块级副本备份：存储或 LVM 快照，或已冻结或已卸载设备的镜像。只有这样的副本能保持 project ID、配额限制、inode 标识、文件系统 UUID 和 `oac-workspace-quota` 日志库的一致。文件级复制、`tar`、`rsync` 或 `xfsdump` 会丢失这些信息；此时该命令会拒绝恢复后的对象，而不是在无上限状态下提供它们。
- 各节点状态目录 `/var/lib/oac-node/.oac/nodes/<installation-id>/` 及其提供商存储：Docker 卷或 microsandbox 存储。包含数据库仍然引用的全部计算资源和快照。参见[节点主机故障时](./nodes.md#when-a-node-host-fails)。
- 每个私有 `checkpoint_root` 的全部内容，包括 `.oac-checkpoint-store`、检查点、operation journal、lock 文件与 tombstone，并与匹配的 Core 数据库一起保留。[检查点转移契约](../sandbox-provider.md#checkpoint-transfer)定义其身份、所有权和生命周期。

### 建立停止写入窗口 {#establish-a-stopped-write-window}

1. 在安装入口阻止新应用 input、实时文件访问和管理变更。等待活动执行、初始化、文件操作和原生清理完成。停止其他能够写入同一文件系统的应用或主机进程。
2. 保持 Core 和节点可用，确认每个所属写入方均已停止，且所有 Create、Kill、快照清理及文件系统变更均已结算。节点离线、超时、实例缺失或服务停止均不足以证明这一点。确认挂起会停止源计算资源，但保留的检查点也必须纳入备份。最简单的冷恢复基线是等待符合条件的保留 Session 完成检查点到期清理、allocation 已释放，再按[冷替换契约](../sandbox-provider.md#cold-replacement-after-checkpoint-retention)保留文件系统对象及原生历史。
3. 停止 Core 和 Web，再停止节点控制服务，以防止新的生命周期操作。重新核实精确的原生资源，保持所有写入方停止，直到备份各部分全部完成。**停止 Core 或节点服务不会停止其沙箱。** 节点服务使用 `KillMode=process`；microVM 及其他由提供商管理的计算资源可能继续独立运行。
4. 使用存储平台支持的流程将已停止写入的文件系统中的待写数据落盘，并创建其快照或导出。对于配额模式，在复制其设备期间使用 `xfs_freeze -f` 冻结 XFS 文件系统或将其卸载，完成后解冻。在同一窗口内导出 Core 数据卷、安装目录及所需的节点状态和提供商存储。复制 PostgreSQL 的 `database/` 文件前必须停止 PostgreSQL。一起记录版本、备份时间、组成清单和校验值，并将备份保存在被备份安装之外。
5. 所有复制完成后才恢复服务。先恢复存储，再启动 Core 和节点，验证就绪后开放入口。不要通过重放不确定的原生操作来让备份通过。

当前没有一条命令即可完成且无损的维护排空流程。归档和部署重置会改变 Session 生命周期状态，不能替代保持续接能力的备份暂停。任何写入方或变更仍然未知时，应将备份保留为未完成状态，先解决相应归属。

若在停止写入窗口中进行数据库逻辑复制，仅保持 database 服务运行，执行：

```sh
docker compose -f "$HOME/.oac/core/compose.yaml" exec -T database \
  pg_dump -U oac oac > oac-backup.sql
```

逻辑转储仍然需要匹配的 secrets、state、独立文件系统及所需节点资源。Docker Desktop 的 **Volumes** 导出可用于复制已停止的数据卷，但不会协调这些其他部分。

### 恢复与演练 {#restore-and-rehearse}

恢复期间保持应用入口及 Core/节点服务关闭。暴露恢复后的文件系统前，必须隔离或关闭每一个旧写入方；旧主机不可达不代表已隔离。演练必须使用隔离的安装和存储副本，不能连接原节点，也不能共享其可写命名空间。

启动 Core 前，恢复相匹配的 secrets 和数据库状态、完整的独立文件系统命名空间，以及任何所需的节点标识和提供商存储。保留数据库中的配置和命名空间 UUID、对象标识及删除标记；不要对恢复后的存储执行首次命名空间初始化。恢复配额模式文件系统时，将其块级副本写入后备设备，并以 `prjquota` 挂载到配置的挂载点；绝不要对其运行 `oac-workspace-quota init`，也不要将副本与原文件系统同时挂载，二者具有相同的文件系统 UUID。重新建立配置中的挂载路径和服务 UID，然后从每个参与服务的上下文验证所挂载文件系统、所有权及 xattr。[设置工作区存储](./workspace-storage.md)负责定义部署设置。

冷恢复仅在新计算资源准入后续接具备资格的原生历史。包含保留检查点的备份还依赖匹配的原生 Runtime、节点标识、提供商存储和外部文件系统标识。文件级恢复可能改变 inode 标识，即使文件字节一致，也可能阻止严格检查点恢复。此流程恢复到原有节点标识；其他节点何时可以恢复检查点由[检查点转移](../sandbox-provider.md#checkpoint-transfer)定义。应保留未解决的归属，而不是替换为新沙箱。

恢复依赖全部完整后才启动 database、Core 和节点服务；检查期间保持入口受限。在隔离演练中，验证保留 Session 的文件和具备资格时在新计算资源上的原生续接，确认已删除对象无法重新打开，并检查旧写入方均无法访问恢复后的命名空间。在依赖备份进行恢复前记录结果。

## 卸载 {#uninstall}

先移除节点，参见[移除节点](./nodes.md#remove-a-node)。

```sh
~/.oac/core/oac uninstall
```

Windows 上运行 `& "$HOME/.oac/core/oac.exe" uninstall`。命令会展示 Compose 项目、数据卷、安装目录和安装 ID，在你输入目录名或 `yes` 之前不会删除任何内容。随后它删除容器、网络和数据卷、安装目录以及[安装锁](../configuration.md#installation-directory)（`~/.oac/core.lock`）。Windows 上正在运行的 `oac.exe` 无法删除自身：命令会打印删除其余内容的 `Remove-Item`，在同一目录重新安装之前先运行它。它保留 Docker 镜像，重新安装时不必再次下载，并打印删除镜像的 `docker image rm` 命令。

全部数据随之删除：项目和 API 密钥、Session 历史、存储的凭据和 Core 密钥。要保留数据，请用 `docker compose stop` 停止安装，或先[备份](#back-up)。

卸载不停止沙箱：节点沙箱在节点继续运行，E2B 沙箱在 E2B 继续运行并计费。Core 仍运行时，归档它们的 Session，或[重置部署](nodes.md#change-the-sandbox-configuration)并等待完成。

如果 Core 已不存在，按那里的说明在各节点主机上带 `--force` 运行节点卸载命令，使用 `oac uninstall` 打印的安装 ID，或节点单元名 `oac-node-<installation-id>.service` 中的安装 ID。

如果安装目录被手动删除，其容器会继续运行并占用 Web 端口。不依赖任何安装文件即可找到并移除该项目：

```sh
docker compose ls --all
docker compose -p <project> down --volumes --remove-orphans
```

项目是列表中的 `oac-…` 名称。之后如果 `~/.oac/core.lock` 仍然存在（Windows 上还有 `~/.oac/.core.lock.runtime.lock`），将其删除。

## 执行延迟 {#execution-latency}

通过 `environment input reserved` 日志将提交输入的 HTTP Trace 与 `reservation_id`、`execution_trace_id` 关联。worker 从持久化预留标识派生执行诊断 Trace，观察连接断开或 owner 重启都不依赖进程内 Trace 映射。这是 HTTP Trace 与执行 Trace 之间的明确关联，不是原 HTTP span 的延续。沿 `environment input selected`、`execution preparation requested`、`environment input admitted` 将预留与 Session、Environment、device、准备请求、Executor、Turn 关联。`preparation_request_id` 标识控制准备请求，与 HTTP `request_id` 不同。创建 Session 时附带输入的情况，通过相同 `session_id` 和 `is_initial=true` 将 `session initial input origin` 与 worker 的预留关联；普通和流式创建都支持，不增加 store 查询。

`execution stage` 记录 `stage`、`duration_ms` 与 `status`（`ok`、`error`、`cancelled`、`timeout`）。阶段覆盖输入验证/ownership、预留和等待 admission、执行配置与 admission 提升、生命周期 gate 等待、Runtime 观察和供给、provider create/resume、Environment 初始化。Runtime 连接日志记录已确认观察到的连接变化，不是 socket 建立的精确时间。通过 Environment、allocation、node ID 将后台 Runtime 记录与输入关联；缺少输入 Trace 的后台记录不能视为连续的请求 span。阶段耗时使用本进程单调时钟。`queue_age_ms` 则比较选择时间与数据库预留创建时间，可能受到时钟偏差影响，不能当作单独测量的调度等待。

`execution scheduling observation` 是 worker 本地 reservation 汇总，记录 `owner_observation_id`、reservation/Session 身份、固定状态 observed 标记及从首次候选观测开始的偏移。`eligible_seen` 表示本 owner 首次观察原有就绪检查全部通过，不是资源物理就绪时间。固定配对 `eligible_to_selected_ms`、`selected_to_prepare_ms`、`ready_to_promote_ms`、`promote_to_notify_ms`、`notify_to_handler_ms` 要求同一 observer 内锚点顺序有效且不超过 1800000 ms；缺失、乱序或超界为 null。既有 handler 读取仅可为同 Worker、Session、reservation 标记 `handler_observed`，不增加数据库读取。成功 dispatch 或观察到 cancelled/failed/expired reservation 后释放条目；若 handler 没有读取终态，条目可能保留至 owner 关闭。终态字段冻结、条目释放后，才在所有观测锁外同步输出日志；慢日志 sink 可延迟调用者，但不持有共享观测锁。owner 本地单调 fence 防止在观测完成之前启动的候选页重新打开已释放条目。已有条目仍可使用；旧页中尚未跟踪的条目可能保守跳过，直至新查询页出现；不改变业务调度、不增加查询。这是额外观测缺口，不能据此声称完整候选历史。最多保留 256 个活跃条目、不淘汰；超额只跳过观测，业务继续。`execution scheduling owner observation` 用饱和 uint64 `overflow_count` 与 `overflow_saturated` 报告跳过量。`prefix_observed=false`、`observation_complete=false` 保留候选前缀未观测的事实。outcome 为 `completed/cancelled/owner_lost/failed/unknown`；诊断完成不是执行成功。不能跨 owner ID 合成单调区间，也不能据此推断未观测的数据库、容量或候选分页等待。

`execution_prepare_send` 测量 Core 控制消息发送；`execution_prepare_wait` 测量随后等待 Runtime 执行器就绪或预留进入终态的区间，两者都携带 Session、预留与准备请求 ID。`environment_initialize_commit` 测量初始化完成事务；成功提交后唤醒调度器，已有提示扫描绕过一秒候选扫描间隔，但不绕过准入检查。`input_admission_wait` 增加 `notification_wakeups`、`poll_wakeups` 和 `last_wake_source`（`none`、`notification`、`poll`）；它们统计等待 HTTP handler 实际选中的分支，不代表通知发送或丢失数量。选中轮询不证明漏通知。`runtime_reconcile` 仅对非空 allocation 页记录，测量清单查询及 gate 获取之后的观察批次和随后 pending 供给，附 `allocation_count` 与 `observed_count`；它与单独 provider/观察阶段重叠，不含之前的 gate 等待。这些记录不改变生命周期顺序，也不隔离慢实例观察。

每轮生命周期协调在调用者 Trace 上使用新 span，没有 Trace 时创建后台 Trace。同一 trace/span 关联 `runtime reconciliation started`、`runtime_gate_wait`、`runtime_inventory`、各个 `runtime_observe` 和 `runtime_provision`、`runtime_pending_inventory`、`runtime_pending` 与 `runtime_reconcile_total`，包括 `node_id` 为空的直接 Provider 扫描。`runtime_inventory` 测量每次 allocation 页查询，`refill` 区分可选的回绕页；查询失败时 `allocation_count` 为 null，而不是空页。`runtime_pending_inventory` 测量待处理页查询；`runtime_pending` 包括该查询及返回候选的供给尝试，查询失败时 `candidate_count` 为 null，`attempted_count` 只统计开始尝试数，不代表成功数。`runtime_reconcile_total` 在获取 gate 前开始，每次返回都结束，包括空页和错误；原有非空页 `runtime_reconcile` 保留较窄区间。扫描成功仍可能含失败的单实例观察，需要查看各阶段状态。同轮身份能够证明旧实例观察是否先于 pending 供给；不同轮次仅时间重叠不能证明阻塞。跟踪不改变扫描顺序、公平性或取消语义。

`e2b helper call` 测量已验证的 Go helper 调用，从请求序列化前到调用者返回。固定且可空的字段测量实际调用：`request_marshal_ms` 为请求序列化，`command_setup_ms` 为命令/环境/stdio 配置，`process_start_ms` 为 Start（包括失败），`caller_wait_ms` 为等待完成通知或取消，`process_wait_ms` 为原 Wait 调用，`fence_release_ms` 为存在 fence 时原 child-fence 释放 callback，`observation_parse_ms` 为有界 stderr 过滤与转发，`response_decode_ms` 为响应解码、校验及 trailing-data 检查。Wait 包含剩余子进程执行和输出管道 drain，不只是退出。子进程计时仅经原完成通道传给调用者；未收到完成通知先取消时，Wait/fence/parse/decode 均为 null，即使后台随后排空。不存在 fence 或未到达边界为 null，不是实测零。调用前验证拒绝维持原行为。日志包含已验证 `operation`、`helper_started`、有限状态及 allocation/environment ID，不含路径、输入或原始错误。

仅收到子进程完成通知且 stderr 未超限时，Core 转发最多四条 `e2b helper stage`：`helper_startup` 从 Python main 模块首个单调时钟起，到导入、读取请求及 Provider 构造完成；`helper_execute` 覆盖 `Provider.execute`，包括本地 receipt 锁、SDK 工作及分类结果处理；`helper_response_serialize` 覆盖响应 JSON 编码、校验/大小检查和原有 fallback 编码；`helper_stdout_write` 覆盖原 stdout write 调用返回。序列化或写出成功不表示 provider 执行成功；write 返回也不证明 flush 或父端收到，本轮不新增 flush。已执行但失败的边界记录 `completed=false`，不带错误正文；未执行边界不输出记录。原 `--check` 提前返回路径不补造响应序列化/写出阶段。诊断 sink 失败不会替换权威响应或 stdout 异常。

调用者等待与进程等待重叠，Python 阶段和 create 子阶段与进程等待包络重叠，不能相加。Go 进入 Wait 前子进程可能已开始执行，因此 Wait 并非完整子进程生命周期。Python 首时间戳前的冻结包 loader/解释器工作、最终解释器关闭和阶段间空隙仍未单独测量。残差只能称未覆盖开销，不能称实测启动/退出时间，也不能用独立 check 样本填充。超时仍及时返回，原子进程后台排空，fence 保持至原释放点。仅在原 Wait 和释放完成后读取 buffer。诊断复用调用者 trace/span 与可信 operation；stdout 字节、SDK 顺序、超时/错误优先级及 helper/Runtime 协议均不变。

`control_ready_ms` 从调度、Runtime 就绪和配置组装完成后开始。`start_control_ms` 描述控制确认。`input_to_first_text_ms` 从执行投递前开始，包含 start-control 时间，不是纯模型 TTFT。admission、control、首字区间存在重叠，不要当作独立耗时相加。provider 调用成功表示该操作成功，不等于 daemon 连接完成或 Turn 完成。除非原生适配器提供相应观测，模型请求开始、响应 headers、模型首字、重试仍为不可观测。日志仅记录关联 ID 与有限状态分类，不记录输入内容、凭据或 provider 原始错误。

`runtime transport registered` 记录认证后的 WebSocket 注册，`runtime capability snapshot observed` 记录合法能力声明变化及设备 ID、能力数量。连接注册本身不代表执行就绪。包含可用 Harness 的能力变化会发出合并后的调度提示；Worker 仍检查所有权、容量、生命周期和能力要求，轮询负责兜底。

`journal first text received` 记录 delivery journal 接纳的第一条非空 delta 或原生 output-message 文本，包含结束时 drain。`journal_sequence` 是执行日志序号，不是公开 Session 事件序号。`journal first text persistence` 将首次失败记录为 `unconfirmed`，最终成功返回记录为 `confirmed`，包含尝试次数、append 耗时、接纳到确认耗时（失败为 null）。这是调用方确认时间，不是数据库内部精确 commit 时刻。每次 attempt 的私有 collector 只捕获该准确 source 实际生成的首个公开文本 `event_id`；只有 append 成功返回后才记录 `mapping_status=confirmed`，插入成功但提交未确认仍为候选。未知提交后的幂等 replay 可以确认 journal，但没有新公开映射时明确记录 `mapping_status=unobserved` 与空 event ID，不能因 replay 成功就升级旧候选。后续 SSE 读到同一个 ID 只能证明事件存在，不能补造原 commit 时间。不变的原生消息投影可能不产生公开文本事件。每 Turn 最多一条接纳、一条失败和一条确认，不输出正文或原始错误。

`session stream first text read` 与 `session stream first text output` 观测同一连接内每个 Turn 的首条非空公开文本 delta/done，包含该连接后续的 Turn。每连接最多保留 64 个不同 Turn ID、不淘汰，缺 ID 共用一个独立槽；重复事件、旧 Turn 迟到文本、重复生命周期事件均不重置已选身份。超过上限时仅记录一次 `session stream text observation limit`，明确 `status=unobserved`、`reason=turn_limit`，公开转发照常；首文本与终态对每个身份使用独立 bit；终态不依赖文本，满 64 个 Turn 后已保留 Turn 仍可选择另一类别。`session stream terminal read` 和 `session stream terminal output` 用 `scope=terminal_per_turn_connection` 观测 completed、failed、cancelled，使用相同的真实事件 ID、Write/Flush 边界和 `event_type` 字段；合计最多 260 条边界日志加 1 条超限日志。字段包含真实 `event_id`、`session_event_sequence`、Turn/Session、每次连接新建的 `connection_id`、`start_cursor`、`turn_identity_observed` 及 `scope=first_text_per_turn_connection`；重连不冒充 Turn 全局首文本。`read_at` 是该批数据库读返回时刻，早于批内前序事件写出。输出记录分开列 Write 与 Flush 耗时和状态，未到达的边界为 `unobserved` 与 null。Flush 成功仅表示 `ResponseController.Flush` 返回 nil，不表示下游已收到。短写单独记录，不改变现有行为。`read_to_emit_return_ms` 包含批内前序输出、投影、授权、序列化、Write 和 Flush，不能与子阶段相加。按真实 event ID 关联，不按相邻时间或不同领域的序号配对。GET 仍为 live-only，漏掉的事件不能伪造重放观测。批量阈值、轮询、wire、事务和鉴权行为不变。

`e2b create stage` 将既有模板检查拆为 `template_get_info`（`files.get_info` 调用）和 `template_read_check`（进入并关闭既有 `files.read(format="stream")` 上下文）。每次 create 各最多一条，仅到达时输出时长及 `completed`；失败为 false，缺失表示未观测。read 检查不消费完整文件。这两个子阶段包含在 `template_check` 中，不能相加；helper 随 Core 构建更新，不因此更新 Runtime 模板。

`runtime first heartbeat` 记录 Runtime 每连接首次心跳尝试的 `capability_payload_build_ms` 与 `first_heartbeat_send_ms`，send 包含本地队列/写确认，不等于 Core 收到。Core 的 `runtime first heartbeat observed` 记录首次尝试的 `heartbeat_decode_ms`、`capability_set_ms`、`heartbeat_store_ms`、`capability_hint_ms`；store 包含同步设备心跳调用，仅持久化成功且未删除才触发 hint。失败或关闭仅终结一次，未完成阶段为 null。device ID 提供上下文，但两端 connection ID 属于不同进程，不能证明是同一首帧；后续心跳不重复汇总。

`native turn boundary` 在既有 native Turn 终结边界每 Turn 输出一次。字段为 `run_id`、可空的 `preparation_request_id`/`native_turn_id`、`input_to_start_turn_ms`、`thread_resolve_ms` 及 `thread_mode=start/resume/reuse/unobserved`、`turn_start_write_ms`、`turn_start_ack_wait_ms`、`first_native_text_to_forward_ms`。observed 布尔区分未测与零耗时，forwarded 表示原发送是否成功。ACK 早于 write 返回时 `ack_before_write_return=true` 且 ACK 等待为 null；未见 ACK 保持 unknown。首文本仅选非空 root 文本，可以早于 ACK。这些本地适配器边界不等于模型计算或 Core 接收，所有权确认不是完成。不记录输入、RPC 内容或原生错误。需重建 Runtime 并验证新模板；只更新 Core 无法给既有 sandbox 加上这些观测。

`journal first text persistence` 新增首文本等待首次 flush 的 `batch_wait_ms` 及 `first_flush_trigger=size/timer/function_call/deferred_final`。`append_count`、`append_total_ms`、`append_max_ms`、`append_failed_count`、`append_unknown_count` 跨 attempt 累计。失败与 unknown 可重叠，返回错误不证明已 rollback；首次失败与最终确认的快照不能相加。不新增逐重试日志或 flush。

`session stream first text read` 的 `timing_scope=connection_prefix` 表示 `empty_read_count`、`authority_cache_hit_count` 以及 `poll_wait`、`authority_check`、`list_events` 的 count/total/max 从本连接开始累计，不是 Turn 独占成本，不得跨 Turn 快照相加。`first_text_list_ms` 是包含该文本的实际查询，同页多个事件共享它。`last_empty_to_first_read_ms` 需要真实空页；最近 poll/check/list 区间只是局部边界，不能完整加总，权限缓存命中没有 check 耗时。若连接未观测到文本，仅输出一次 `session stream observation ended`，保留结局及计数，首文本边界为 null。轮询与权限刷新间隔不变。

`executor done received` 是主循环首次接收 done、进入 journal 之前的边界，不是模型完成时刻。`delivery_final_drain` 和 `delivery_final_flush` 分别记录最终 drain 和 flush；flush 包含 `pending_count`、`confirmation` 和 `done_to_cleanup_ms`（收到 done 到清理开始，未观测为 null）。成功空 flush 是 `no_pending`，不是新增提交。仅在 final drain 遇到 done 不补造接收边界；done 后仍可能等待 preparation/function 回执。

已完成且具有本地 workspace 配置（`openai_hosted` 或 `self_hosted`）的 Turn 记录 `artifact_capture_begin`、`artifact_workspace_prepare`、`artifact_export_total`、`artifact_export_staging`、`artifact_workspace_release`、`artifact_capture_total`。Prepare 包含配置/订阅/发送/ready 等待，release 包含发送/ACK。Staging 包含远端 archive 读取、PostgreSQL 大对象写入和 staging 事务，不能当作纯 DB 时间。Capture total 包含失败时 `artifact_failure_cancel_check`。前置失败可记录 `artifact_workspace_cleanup`，`confirmation=unobserved`；其 `status=ok` 仅指清理调用返回，不确认远端 release ACK。`artifact capture skipped` 使用 `turn_not_completed` 或 `workspace_not_local`。environment-none dispatcher 本来不调用 capture，无日志不代表实测零耗时；普通目录浏览不激活此 scope。父子阶段重叠，不能相加。

Artifact capture 使用不可变 `capture_id` 关联原有阶段与有界汇总；普通目录读取和未携带 capture scope 的调用不输出这些汇总。`artifact export reader` 对每个成功注册的 export 输出一条，包含消费失败与清理，并携带 `export_id`；非法请求或注册被拒绝时没有 reader 汇总。`enqueue_ms` 累加 begin/next Send 调用，成功仅指队列接纳，不是 WebSocket 写出。`begin_count` 和 `next_count` 统计发送尝试，包含最后请求 completion；`receipt_wait_count` 统计实际进入的等待。`receipt_wait_ms` 和 `receipt_wait_max_ms` 包含每次回执或中断之前的远端工作、传输与调度，不是纯网络耗时。`chunk_count` 和 `received_bytes` 只统计验证通过的数据块；`delivered_bytes` 统计复制给消费者或最终 drain 的字节，递归读取与小 buffer 不重复计数。`first_data_ms` 和 `completed_receipt_ms` 从 reader 建立起计，未观测到为 null。`completed` 表示收到合法完成回执，不代表 staging 成功。`cancel_enqueue_ms` 仅在清理尝试取消时有值，`cancel_enqueued` 只确认入队。空 tar 仍可能需要数据块与独立 completion 请求，archive EOF 不等于 exporter 完成回执。Reader 区间包含消费者工作和清理，不能再加上其回执等待。

`artifact staging` 仅在 capture scope 与 Session、Turn 匹配时输出一条。`with_staging_ms` 包住原 WithArtifactStaging 调用直到返回，包含事务获取与结束；不是数据库 commit 时刻，扣除子阶段后的剩余也不能叫纯 commit 耗时。`load_environment_ms`、`read_export_ms`、`lock_session_ms`、`load_turn_ms`、`stage_artifacts_ms` 测量原调用，未到达的边界为 null。`put_ms` 累计 `put_count` 次内容写入尝试（没有调用时均为零）；Put 包含远端 export 读取，嵌套在 read-export 内，不是纯 DB 时间。回执等待可能发生于 Put，也可能发生于 archive header/trailer 读取。Staging 的 `confirmation` 仅在外层成功返回时 confirmed；失败、rollback 或未知结果仍为 unconfirmed，即使 staging callback 已完成，也不证明最终 Turn 发布。汇总只记录身份、有限状态、时长和计数，不输出文件路径、内容或原始错误；数量不随文件或 chunk 数增长。读取顺序、锁时机、事务、取消、错误优先级与 Runtime 协议保持不变。

`artifact transport` 为每次 capture 输出一份 Core 快照，把 `capture_id`、`session_id` 和 `turn_id` 映射到既有 `preparation_request_id` 与 `export_id`。其 `connection_id` 标识 Core 本地网关连接。`scope_status=unobserved` 表示观测注册或身份关联达到限制，不改变业务结果。`outbound_queue_wait` 从入队成功后计至出队；`outbound_encode` 和 `outbound_write` 分别覆盖编码及 WebSocket 写调用。`inbound_decode` 和 `inbound_dispatch` 覆盖匹配入站帧的解码和处理器分发。各阶段包含 `_count`、`_total_ms`、`_max_ms`、`_failed_count` 和 `_unknown_count`；没有已完成测量时，总时长及最大时长为 null。`outbound_unfinished_count` 统计 capture 结束时仍在排队或写出的帧。`snapshot_complete=false` 保留部分观测，包括仍在执行的测量或不可用的入队时间戳；Core 不等待诊断补齐。

`runtime workspace transport` 为每个已接纳的 workspace-only preparation 和 export owner 输出有界快照。Runtime scope 包含 Core artifact capture 之外的 workspace 操作，不观测 Harness preparation，单凭 Runtime 记录不能认定它属于 artifact。`scope=preparation` 携带 `preparation_request_id`，`scope=export` 携带 `export_id`。使用这些既有请求身份连接 Core 的 capture 映射。`connection_id` 属于 Runtime 本地，不等于 Core 的连接身份。`identity_valid=false` 将不安全的请求身份留为空值；`connection_mismatch=true` 防止把传输工作归到另一连接。两种情况下都不能用时间邻近替代身份关联。

Runtime 阶段名称为 `receive_dispatch_wait`、`request_queue_wait`、`export_producer`、`export_read`、`export_done_wait`、`prepare_local`、`release_local`、`sender_call`、`outbound_queue_wait`、`outbound_encode` 和 `outbound_write`。接收延迟从解码到达计至 dispatcher 消费，包含接收通道背压；请求队列延迟从请求入队成功后开始。producer 时长覆盖本地导出生产，read 时长覆盖 pipe 读取，done wait 是发送完成结果前既有的 producer 完成等待。本地准备和释放覆盖相应本地工作，sender 与 transport 阶段测量状态或结果发送。各阶段采用 Core 快照的字段后缀，并增加 `_cancelled_count`；null 总时长和最大时长表示没有已完成测量。`read_bytes`、`chunk_count` 和 `local_eof` 表示本地 pipe 消费，不表示 Core 接收或 staging。`transport_observed` 表示匹配的 transport 接纳了观测元数据。`outcome` 与 `snapshot_complete` 描述不同事实：完整观测可以包含失败操作，关闭、暂停发布或在途工作仍属于部分观测。Workspace-only preparation 忽略已过时的状态 revision；终态通知失败不会重新启动已经完成的清理。如果关闭或暂停抑制了发布，owner 在 router 锁外以 `outcome=shutdown` 或 `outcome=suspended` 及 `snapshot_complete=false` 结束快照，即使已经清理的 preparation 记录被移除也如此。这不构成 release acknowledgement。

Core 与 Runtime capture 汇总还测量成功 WebSocket read 返回至 JSON 解码前的 `read_return_to_decode_begin`，Runtime 增加 `inbound_decode`。只有解码后关联既有 scope 的帧才计入；它包含本地解码前工作与调度，不包含阻塞 read，也不是纯网络耗时。各阶段提供 `_overflow`；有界计数饱和，超额或未完成测量保持 unknown，不回绕。

`artifact export reader` 与 Runtime export 汇总增加 `rounds_json` 字符串，内部最多前八轮，字段为 `round_index`、`phase=begin/next`、`offset`、`observed`、固定 `outcome` 与 nullable duration。Core 的 `request_send_to_receipt_dispatch_ms` 从原 Send 调用前到匹配本地回执 dispatch；Runtime 的 `request_read_to_response_write_ms` 从成功 read 返回到原响应 write 返回，终点在获取后续观测锁之前立即保存。count/total/max/unknown 聚合覆盖更多轮，另有 `round_count`、`rounds_truncated`、`rounds_overflow`；计数上限 1000000，duration/total 上限 1800000 ms。未尝试时 Core rounds 为 null，Runtime 为空数组。Core outcome 为 `chunk/completed/unknown`，Runtime 另允许 `ok/error/failed/rejected`。unknown 轮的 duration 为 null。不补造 write-return-to-receipt 区间或新的 completion 请求。这些包络与既有队列、write、receipt 测量重叠，不能隔离单向传输。

Runtime `export_writer` 测量原 pipe writer 调用，记录 count/total/max/failed/cancelled/unknown/overflow 与 `export_writer_observed`，保持原字节、返回 n/error 和背压。区间包含 consumer 消费与本地调度，不是纯阻塞或磁盘时间。`export_producer` 包含 writer 调用。仅完整串行 producer 与完整 writer 测量之差可称 writer 外包络，不能称 filesystem 或 tar CPU 时间。在途快照和饱和仍不完整。这些 Runtime 增量需要重建 daemon 并验证模板，单独部署 Core 不能提供。

这些记录使用进程内单调时长和固定大小汇总，不逐帧输出日志。sender 返回、WebSocket 写调用返回、Core 验证完成 receipt 和 staging 确认是不同边界。异步状态发布不证明 socket 已写出，`local_eof` 不证明对端已接收。未知或未完成测量保持未知；实测 `0.000` 毫秒仅反映精度。父子区间及 producer 与 pipe consumer 区间存在重叠，不能相加。跨进程时间戳用于关联，不构成网络 RTT。Runtime 记录需要对应 daemon 构建；替换 Core 或其 E2B Provider helper 不会升级既有 Runtime 模板。E2B Provider helper 运行在 Core 侧，不内嵌于 Runtime。

`turn_completion` 测量每次原有 `CompleteExecution` 调用及事务返回，记录 `requested_turn_status`、`returned_turn_status`、`attempt`、`confirmation`、`mapping_status` 和真实候选 `event_id`。仅成功时填 returned status，请求状态不证明结果。每 attempt 新建私有 collector，在 `AppendChanges` 按精确 Session、Turn、预期终态类型捕获，独立于首文本 source 投影。候选插入后调用失败仍为 `unconfirmed`，成功只确认该 attempt；即使调用成功，`mapping_status=unobserved` 和空 ID 也不证明公开终态事件存在。`CompleteExecution` 不同于 journal append，不支持幂等 replay：未知提交后重试可能返回 Turn conflict，不能追认旧候选。原 unapplied-input fallback 仍为独立第二次 attempt。Deployment-provider 观测在 completion 返回后，不直接阻塞已提交终态可用性。用真实终态 event ID 关联 commit 和 SSE，不用 journal ordinal 或相邻时间配对。事务、wire、重试和 Runtime 模板要求不变。

Daemon 启动日志记录 `runtime process starting` 和构建版本，`runtime startup stage` 记录各 Harness 探测、bootstrap 和每次连接尝试的本地单调时钟耗时，不输出凭据或原始错误。探测发生在连接注册之前。结合这些边界与已观察到的持久化连接时间，区分启动、周期观察和调度等待。Daemon 观测要求 Runtime 使用带有这些埋点的构建；仅升级 Core 不会升级现有 Runtime 模板。托管启动回执只确认进程已启动，不确认连接或能力就绪。

Codex CLI 可用性探测输出 `process_spawn`、`process_wait` 的 `runtime version probe` 记录。`first_stdout` 从启动尝试开始，计时到 Go 复制协程观察到第一块非空 stdout；`stdout_to_completion` 从该时刻计时到 `Wait` 返回，包含剩余执行和管道排空。无 stdout 时不输出这两项。记录在完成后导出，不允许提前成功：仍以进程退出和输出校验决定可用性。`runtime version probe resources` 使用 `ProcessState` 返回的 OS 进程统计，输出 `user_cpu_ms`、`system_cpu_ms`；平台可能包含已回收后代，无法取得进程统计时省略。缺页和存储字节计数未采集，缺失表示不可观测，不能按零计算，也不能据此认定存储或网络根因。这些区间均不是模型执行。
