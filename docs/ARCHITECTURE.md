# 架构设计：JRP

> 本文描述系统目标架构与当前稳定边界。功能交付状态以 `docs/PRD.md` 和代码测试为准。

## 1. 定位与边界

JRP 是单机优先、兼容官方 frpc 的反向代理与 P2P 打洞平台。系统由独立 Core、服务端外壳 jrps、客户端外壳 jrpc、React Web 四个稳定边界组成。

- **Core** 只负责数据面和兼容协议能力，不知道数据库、Web、管理 API、通知、CLI 或具体可执行程序。Core 同时是可被第三方 Go 应用嵌入的库，通过 ServerEngine / ClientEngine 门面提供生命周期、配置应用与事件订阅能力。
- **jrps** 装配 Core 与控制面适配器，持有服务端 SQLite、管理 API、通知、采集索引和 Web 静态资源。
- **jrpc** 装配 Core 与客户端适配器，持有客户端 SQLite、enrollment、管理长连接和配置应用协调。
- **React Web** 只调用 jrps 管理 API，生产构建产物嵌入 jrps，不进入 Core。

兼容研究以参考源码仓库的固定 Git 提交 `e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314` 为基线；仓库检出位置由开发环境提供，不属于架构契约。JRP 不依赖、链接、导入或复制 frp 源码。

## 2. 模块与依赖

```text
   github.com/wcpe/jrp/core          （数据面：协议、wire、传输、代理、NAT）
              ▲                    ▲
              │ 单向依赖            │ 单向依赖
   apps/jrps ─┘                    └─ apps/jrpc
       ▲                                 ▲
       │ 单向依赖（FR-29 计划）           │ 单向依赖（FR-29 计划）
   platform/service ◀───────────────────┘
   （系统服务适配，尚未建立）

   apps/web ──生产构建产物──▶ apps/jrps（内嵌）
   packages/ui、devmock、tsconfig、eslint-config ──▶ apps/web
```

- Core 被 `apps/jrps` 与 `apps/jrpc` 单向依赖，自身不反向依赖任何一方。
- `apps/web` 的生产构建产物只嵌入 jrps；Core 与 jrpc 不含前端资源。

强制依赖规则：

1. 根 `go.work` 当前编排 Core、jrps、jrpc 三个 module；`platform/service` 在 FR-29 建立后加入并另行登记。
2. Core 不得导入 `apps/*`，也不得依赖 Gin、GORM、SQLite、Web、通知或 CLI。
3. jrps 与 jrpc 不互相导入；共享数据面能力只能进入 Core。
4. 管理 DTO、数据库模型、会话模型、通知模型和 Web 类型不得进入 Core。
5. `packages/*` 不依赖 `apps/web`；前端共享包保持单向被应用消费。
6. 官方 frpc 与 jrpc 共用兼容数据协议；jrpc 私有管理能力通过独立 HTTPS/WSS 通道承载。
7. `platform/service` 建立后不得导入 Core 或 `apps/*`，不得承载数据面能力；它只提供操作系统服务适配，由 jrps 与 jrpc 单向依赖（FR-29）。

### 2.1 Core 内部能力边界

Core 在相应功能实现时按真实职责形成包，不预建空目录：

- 协议模型与消息校验。
- wire v1/v2 编解码、协商和加密状态机。
- TCP、KCP、QUIC、WebSocket、WSS 传输接入。
- 控制会话、心跳与鉴权。
- 工作连接请求、绑定、复用和生命周期。
- TCP、UDP、HTTP、HTTPS、STCP、XTCP 代理路由。
- 访客连接与 NAT 打洞。
- 压缩、加密、限速等数据变换。

Core 通过稳定端口接收不可变配置快照、发布运行事件和返回应用结果；适配器不能穿透读取或修改 Core 内部状态。

嵌入宿主通过类型化配置构建器在代码里构造不可变配置快照，再交给 Engine；Core 不读取配置文件、环境变量或数据库。Engine 遵循 Start、Shutdown、Done 生命周期：启动成功后接管宿主注入的资源，启动失败资源仍归宿主，关闭后无残留监听器、连接或 goroutine。同一进程可并行运行多个 Engine。Core 不调用 `os.Exit`，不注册全局信号、全局 HTTP 路由或可变单例。

Core 版本与产品版本双轨（ADR-0011）：根 `VERSION` 是产品版本唯一真源，`core/VERSION` 是 Core module 版本唯一真源，tag 形式为 `core/vX.Y.Z`。`core/VERSION` 随 FR-28 交付建立，在此之前 Core 版本仍由根 `VERSION` 表达。

## 3. 数据模型

### 3.1 配置版本

服务端和客户端分别以自己的 SQLite 为持久化真源。核心概念如下：

- `desired revision`：管理员或服务端期望应用的版本，包含不可变配置内容和版本号。
- `active revision`：Core 当前对新连接生效的不可变快照。
- `last-good revision`：最近成功完成准备、健康检查和发布的版本。
- `apply result`：记录准备、发布或排空结果及安全可公开的错误摘要。

SQLite 的 desired 不能冒充 Core active；Core 的内存快照也不能反向成为持久化真源。进程重启后由适配器从 SQLite 恢复 desired，再通过同一应用流程建立 active。

### 3.2 主要实体

- 管理员与会话：P1 单管理员、安全 Cookie 会话、CSRF 状态。
- 客户端：身份、独立 token 摘要与生命周期、连接状态、desired/active revision。
- 代理：所属客户端、类型、入口、目标、传输与安全参数、采集开关。
- 配置版本：不可变内容、创建者、创建时间、应用状态和 last-good 关系。
- 通知目标与 outbox：Webhook/邮件配置、待发送事件、重试状态。
- 请求元数据：代理、时间、方法、主机、路径摘要、状态、大小、耗时和正文段引用。
- 正文分段索引：文件段、偏移/条目、压缩后大小、保留截止时间和删除状态。
- 审计事件：主体、动作、对象、结果、时间和脱敏上下文。

凭证和 HTTP 正文按已接受风险明文静态存储；API、日志、UI 和审计必须脱敏。正文不存入 SQLite 大字段，而写入压缩分段文件。

## 4. 接口

- **兼容数据协议**：官方 frpc 与 jrpc 连接 jrps，支持 wire v1/v2、控制会话、工作连接、访客与 NAT 时序。
- **管理 REST**：`/api/v1`，供 Web 和管理员自动化使用。
- **实时事件**：SSE，发布状态、配置应用和通知结果等只读事件。
- **jrpc 管理通道**：`/agent/v1` 下的 enrollment 与 WSS 长连接，传递版本化 desired state 和回执。
- **健康检查**：工程骨架的 `/healthz` 与 `/readyz`，不要求管理员会话。

P1 不定义任何数据节点 API、节点字段或分布式 RPC。详细契约见 `docs/API.md` 与 `docs/PROTOCOL.md`。

## 5. 关键机制

### 5.1 无中断配置应用

配置应用遵循固定状态机：

1. **prepare**：校验 revision，构造新监听器、会话、路由和依赖资源，不影响 active。
2. **health-check**：验证新资源可用、端口绑定成功、必要依赖可达。
3. **atomic publish**：一次性把 active 指针切换到新不可变快照，新连接只使用新版本。
4. **drain**：旧资源停止接收新流量，但已有流和连接继续到自然结束或排空上限。

任一步在 publish 前失败都释放新资源并保留旧 active/last-good。publish 后的排空异常进入告警和审计，不回切已被新连接使用的版本，除非正式回滚流程创建并应用新的 desired revision。

只有数据目录、SQLite 路径和管理 API 根监听是引导参数，变更需要重启；其他 P1 配置不得以自动重启冒充热更。

### 5.2 控制面与数据面隔离

- Core 热路径不查询 SQLite，不调用 Webhook/邮件，不等待 Web 或管理事务。
- 管理适配器把 SQLite 配置转换成 Core 不可变快照；Core 只返回结果和事件。
- 运行事件通过有界队列交给指标、日志、审计和通知适配器；队列满时按事件等级执行明确降级，不能阻塞数据转发。
- 通知使用事务 outbox，业务事务提交成功后再发送外部副作用。

### 5.3 jrpc 配置下发

jrpc enrollment 后持有独立客户端身份与 token，通过 HTTPS/WSS 管理通道接收版本化 desired state。断线重连时携带本地已知 revision，服务端只下发必要版本；jrpc 在本地 SQLite 持久化后再进入相同的 prepare/publish/drain 流程并回报结果。

官方 frpc 不参与该管理通道，只使用兼容数据协议和自身配置。

### 5.4 HTTP 采集

- 默认关闭，逐代理开启。
- P1 只采集 JRP 可见的明文 HTTP；HTTPS 透传不解析正文。
- 请求元数据批量写 SQLite，正文流式压缩写入分段文件，避免整段加载内存。
- 默认保留 30 天，总量上限 5 GiB；清理过程维护索引和文件的一致性。
- 关闭采集时数据面不触发 SQLite 请求记录路径。

## 6. 部署

P1 为单机部署：

```text
浏览器 ──HTTPS── jrps（管理 API + Web + Core）── 互联网入口
                         │
                         ├── SQLite
                         ├── 正文压缩分段文件
                         └── Webhook / 邮件

官方 frpc / jrpc ──兼容数据协议── jrps
jrpc ──HTTPS/WSS 管理通道── jrps
```

jrps 与 jrpc 各自拥有数据目录和 SQLite，不共享数据库。生产环境以专用低权限账户运行，管理入口使用 TLS；自签名场景支持证书指纹固定。

P3 演进为中心控制面 + 多数据节点：中央持有数据库、用户、配置和 Web，数据节点只组合 Core 并接收版本化配置。P1 不定义其网络协议、注册模型、表结构或空接口。

## 7. 关键裁决与不做项

- 可移植的独立兼容基线：ADR-0010（取代 ADR-0001）。
- 产品版本与 Core module 版本双轨：ADR-0011。
- 多 module 与 Core 边界：ADR-0002。
- 控制面、数据面、外壳和 Web 边界：ADR-0003。
- SQLite 配置真源：ADR-0004。
- 无中断热更：ADR-0005。
- React Web 嵌入：ADR-0006。
- HTTP 采集存储：ADR-0007。
- Task、Make 与 CI：ADR-0008。
- 未来中心控制面与数据节点：ADR-0009。

当前不做 Redis、PostgreSQL、消息队列、微服务、多管理员、RBAC、OIDC、外部对象存储或任何 P3 节点空壳。
