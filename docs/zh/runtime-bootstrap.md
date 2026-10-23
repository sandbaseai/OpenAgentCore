---
title: "Runtime 引导"
source: docs/runtime-bootstrap.md
source_hash: ea492d111c289832559930079861c512fd518f85b0886f7b3d6a93f33f752773
---

Runtime 引导协议连接沙箱提供商与其启动的托管 Runtime。提供商向 Runtime 交付一个引导文件，该文件是此次启动中 Runtime 唯一的认证输入。

| 属性 | 内容 |
| --- | --- |
| 调用方 | 沙箱提供商。Go 提供商使用 [`runtime_bootstrap.go`](https://github.com/MiniMax-AI/OpenAgentCore/blob/main/services/core/internal/sandbox/runtime_bootstrap.go) 中的 `sandbox.Bootstrap.RuntimeConnection()` 构造输入；SDK helper 原样转发序列化对象。 |
| 实现方 | Runtime，以及[参考实现](#reference-implementations)中列出的提供商 |
| 代码 | [`internal/runtimebootstrap`](https://github.com/MiniMax-AI/OpenAgentCore/blob/main/internal/runtimebootstrap/bootstrap.go)：类型与验证器 |
| 传输 | 一个 JSON 文件，其绝对路径传给 `oac-daemon connect --bootstrap-file` |
| 版本规则 | 精确的 `version`（[启动输入](#launch-input)） |

## 职责与就绪状态 {#responsibilities-and-readiness}

| 参与方 | 拥有 | 绝不 |
| --- | --- | --- |
| 沙箱提供商 | 账户、挂载和工作区；交付该文件；Runtime 的资源与 Environment 绑定配置；以无特权 Runtime 账户启动 daemon | 读取或写入 Runtime 的私有认证存储 |
| Runtime | 验证输入、认证和连接 | 为同一次启动接受第二个认证来源 |

启动成功仅证明交付完成：经过认证的连接、已准备的能力和执行就绪是 [Core–Runtime 协议](runtime-protocol.md) 下的独立观测；[沙箱提供商协议](sandbox-provider.md#four-distinct-readiness-facts) 列出各自证明的事实。

自托管执行器和运维人员供应的设备通过其他方式获取 daemon 身份；[机器连接 API](../../contracts/agents-api/zh/machine-api.md#credentials) 列出所有凭据来源。它们都进入同一 Runtime 执行循环。

## 启动顺序 {#startup-order}

```mermaid
sequenceDiagram
    participant P as Sandbox Provider
    participant R as Runtime daemon
    participant C as Core
    Note over P: Write the bootstrap file
    P->>R: start oac-daemon connect --bootstrap-file PATH
    Note over R: Validate the file, read the credential into memory
    R->>C: Connect over the Core-Runtime protocol
```

## 启动输入 {#launch-input}

将一个 JSON 对象交付到普通文件中，该文件仅允许 Runtime 账户和可信资源供应进程读取（托管 Linux 上权限为 0600），并传入其绝对路径：

```sh
oac-daemon connect --bootstrap-file /home/runtime/runtime-bootstrap.json
```

| 字段 | 含义 |
| --- | --- |
| `version` | 精确的引导版本 `runtimebootstrap.Version` |
| `core_url` | 以 `/api/v1` 结尾的 HTTP(S) 机器 API 基址，不含凭据、查询或片段 |
| `device_id` | Core 签发的 daemon 身份的规范非零 UUID |
| `harness` | 会话固定选择的 Harness 标识；托管 Runtime 只探测并注册此 Harness |
| `credential` | Core 签发的非空 daemon 凭据，不含空白或 NUL |

解码器拒绝未知、重复、缺失和大小写别名字段，拒绝其他版本及超过 `runtimebootstrap.MaxBytes`的文档。错误不包含提交的值。文件缺失或格式错误时，daemon 在连接前失败。

该文件是此次启动唯一的认证输入：daemon 拒绝将其与自托管注册选项组合使用，并将凭据读入内存而不保存到存储的 profile。凭据不进入命令参数、环境变量或回执。提供商为进程重启保留该文件，仅在明确清理自己拥有的资源时删除。

## 超时与限制 {#timeouts-and-limits}

| 数值 | 适用对象 | 超出时 |
| --- | --- | --- |
| 16 KiB（`runtimebootstrap.MaxBytes`） | 引导文档 | 解码器拒绝该文档，daemon 在连接前失败。 |

Core 从分配所属会话派生 `harness`。组合镜像可以包含多个 Harness，但托管 Runtime 只探测所选 Harness；未知、缺失或不可用的选择会失败，不探测其他 Harness。启动协议版本为 2，Core、提供商辅助程序和 Runtime 镜像必须配套部署；安装不支持从另一发行版升级。自托管安装仍探测已安装的 Harness 集合。

Runtime 验证输入，并负责认证与连接。启动成功仅证明交付完成：经过认证的连接、已准备的能力和执行就绪是 [Core–Runtime 协议](runtime-protocol.md) 下的独立观测；[Sandbox Provider 指南](sandbox-provider.md#four-distinct-readiness-facts) 列出各自证明的事实。

## 参考实现 {#reference-implementations}

| 提供商 | 交付方式 |
| --- | --- |
| Docker | 将文件写入 Runtime 拥有的 home volume。 |
| microsandbox、E2B | 在启动同一命令之前交付文件。 |

## 托管暂停控制 {#hosted-suspension-control}

私有托管暂停/唤醒控制文件的路径在 `internal/runtimebootstrap/bootstrap.go` 中以 `SuspendControlFile` 定义。Core 恢复逻辑和原生 Go adapter 读取该值；E2B helper 契约生成器将其投射到模板构建器。托管启动准备私有目录并提供 `OAC_RUNTIME_DAEMON_SUSPEND_PID_FILE`，以启用 Runtime 暂停。这是随软件包发布的协议设置。共享 Sandbox Provider 注册负责空闲和保留默认值；adapter 负责自身原生租约超时。

## 验证 {#verification}

`go test ./internal/runtimebootstrap ./apps/daemon/internal/cli` 覆盖输入契约、凭据来源互斥规则和重启行为。提供商测试验证交付与文件权限，不依赖 Runtime 的私有存储。
