# 功能规格：ServerEngine 与 ClientEngine 双侧门面（首个真实垂直切片）

> 状态：草拟 · 关联 PRD：FR-25 · 分支：feature/core-engine-facade

## 1. 背景与目标

JRP 的数据面能力此前只在仓库内部使用，第三方 Go 应用无法复用。PRD §3 明确提出「作为其他 Go 应用开发者，我希望通过稳定、无全局副作用的 ServerEngine/ClientEngine 嵌入 JRP 数据面」；FR-25 要求提供可嵌入的双侧门面，并以 TCP、wire v1、TCP 代理形成首个真实垂直切片。

本功能解决的问题：

- 宿主想要的是「连上服务端完成穿透」或「自己开放服务接收连接」两种角色，而不是重写一个 frp 客户端。
- 库如果调用 `os.Exit`、注册全局信号或全局路由、持有可变单例，就无法在同一宿进程中安全使用，违反 NFR「嵌入安全性」。
- 如果先把协议实现完再回头补门面，接口会迁就内部实现；采用「门面优先」，先用垂直切片反向驱动协议的最小必要能力。

目标：确立 ServerEngine 与 ClientEngine 的生命周期契约、资源所有权转移规则、多实例并行约束，并用 TCP 传输 + wire v1 + 一个 TCP 代理跑通端到端真实数据流。

使用者：第三方 Go 应用开发者；jrps 与 jrpc 外壳在后续迁移中成为参考宿主。所属阶段：P1。

优先级说明：本次确认为「门面优先」，即先定 Engine 公共接口，再用它驱动 FR-04、FR-05a、FR-06a 的最小协议实现，而不是先实现全部协议再补封装。

## 2. 需求

- ServerEngine 与 ClientEngine 都要提供：宿主可用 Core 类型化配置（FR-32）构造不可变值来创建 Engine。
- 生命周期固定为 `Start`、`Shutdown`、`Done`，并提供最终错误读取入口。
- 启动成功后 Engine 接管宿主注入的网络资源；启动失败时资源仍由宿主持有并可用。
- Shutdown 后不存在残留监听器、连接或 goroutine；重复 Shutdown 安全。
- 同一进程可并行运行多个 Engine，互不干扰，不使用进程级全局状态。
- 首个垂直切片必须真实闭环：ClientEngine 与 ServerEngine 进程内互联，TCP 传输、wire v1 控制会话、一个 TCP 代理，端到端双向字节流可验证。
- 外部消费可验证：测试应用在 `GOWORK=off` 条件下只依赖 Core 公共包即可编译并跑通闭环。
- 所有日志通过宿主注入的 `*slog.Logger` 输出；日志不构成业务契约。

- **范围内**：
  - `Start`、`Shutdown`、`Done`、`Err` 四个生命周期入口，以及对应的状态约束。
  - 宿主注入 `net.Listener` 与 dialer 的资源所有权规则。
  - 一个 TCP 传输 + wire v1 控制会话（登录、心跳）+ 一个 TCP 代理端到端转发。
  - 多实例并行约束与嵌入安全约束的代码级验证。
  - `GOWORK=off` 外部 fixture 验证路径。
- **范围外**：
  - 不实现 wire v2、KCP、QUIC、WebSocket、WSS 传输（FR-04、FR-05b、FR-05c）。
  - 不实现 UDP、HTTP、HTTPS、STCP、XTCP 代理与 NAT 打洞（FR-06a 剩余部分、FR-06b）。
  - 不提供配置热更与 Apply：首次配置由 FR-32 构建器产出、并以首次 snapshot 建立 active；带 revision 的变更属于 FR-26。
  - 不提供事件订阅与只读状态快照：属于 FR-27。
  - 不提供 jrps/jrpc 外壳迁移、SQLite、Web、通知、CLI、采集、jenrollment 管理通道与 `/agent/v1`。
  - 不定义插件 SPI 或代理/传输的可插拔注册机制。
  - 不定义数据节点接口、节点 RPC、集群、租户、RBAC、OIDC 等 P3 能力（ADR-0009）。
  - 不重连策略以外的自愈能力；不做跨 Engine 的服务发现。

## 3. 设计

### 3.1 公共包与依赖方向

- 公共入口：
  - `github.com/wcpe/jrp/core/server` —— ServerEngine。
  - `github.com/wcpe/jrp/core/client` —— ClientEngine。
  - 配置值类型复用 `core`（FR-32）；随本功能一并引入的仅必要时的最小值类型（如 Engine 状态）。
  - 协议、会话、资源所有权等实现置于 `core/internal`，`internal` 类型不得出现在公共签名中。
- 依赖方向：`core/server`、`core/client` → `core` → 标准库；反向依赖禁止；`core` 不导入 `core/server`、`core/client`。
- 公共签名只允许出现标准库类型与 Core 自有类型；不得出现 Gin、GORM、SQLite、quic-go、kcp-go 或 `internal` 类型（ADR-0002、ADR-0003 的边界约束，此处不重复正文）。
- 构造函数返回具体类型 `*server.Engine` 与 `*client.Engine`，不预先抽取抽象接口；需要替身测试的宿主自行声明最小接口，避免为尚未存在的需求创建猜测性抽象。

### 3.2 生命周期状态机

```text
New ──Start──▶ Running ──Shutdown──▶ Stopped
  │               │                     ▲
  └──Start失败────┘                     │
                  └──意外异常 ───────────┘
```

- `New(cfg, opts...) *Engine`：纯内存构造，不绑定端口、不启动 goroutine、不返回 error（配置合法性由 FR-32 的构建器保证；Engine 内部再校验一次并在 `Start` 返回错误，用于防宿主绕过构建器零值构造）。
- `Start(ctx context.Context) error`：绑定/接管资源并进入运行。只能成功一次；重复调用返回 sentinel 错误 `server.ErrAlreadyStarted`。
- `Shutdown(ctx context.Context) error`：幂等。停止接收新连接，按排水上限等待活动连接结束，随后释放全部资源。超出 `ctx` 期限返回 `context.DeadlineExceeded` 包装的错误，但资源仍进入强制释放流程并随后关闭 `Done`。
- `Done() <-chan struct{}`：完全停止后关闭。未 Start 时调用返回的行为明确：Stopped 之前为永不关闭的 channel；不得返回 nil。
- `Err() error`：返回导致 Engine 停止的最终错误；正常 Shutdown 后返回 nil。只在 `Done` 已关闭后读取结果稳定。
- Engine 不允许停止后重启：`Stopped` 状态下 `Start` 返回 `server.ErrStopped`。

### 3.3 资源所有权规则

- 宿主通过网络资源选项注入 `net.Listener` 与 dialer：

```go
eng := server.New(scfg,
    server.WithListener(ln),
    server.WithLogger(logger),
)
eng.Start(ctx)
```

- 所有权转移点固定为 **Start 成功返回那一刻**：
  - Start 成功：宿主注入的 listener 归 Engine 所有，宿主不得再 `Accept` 或 `Close`；Engine 在 Shutdown 时关闭它。
  - Start 失败：listener 仍归宿主，Engine 不关闭、不遗留半绑定状态；宿主可自行 Close 或重试。
- 未由宿主注入的资源（例如连接到上游服务端所需的出站连接）：Engine 自行创建、自行关闭，不要求宿主管理。
- 若 Start 中途部分资源已创建而后失败，Engine 必须释放自己创建的那部分，且不触碰宿主注入的资源。
- Shutdown 完成后：无监听套接字、无出站连接、无残留 goroutine；该承诺由 goroutine 泄漏检测测试强制。

### 3.4 嵌入安全约束

Core 必须满足并在测试中验证：

- 不调用 `os.Exit`，也不让 panic 经宿主调用栈传播到宿主进程退出路径。
- 不注册全局信号处理器（`signal.Notify` 属于宿主职责）。
- 不注册全局 HTTP 路由或全局默认 ServeMux 处理器。
- 不持有可变全局单例；包级变量只允许不可变常量与只读查表。
- 所有超时、退避、缓冲规模来自配置值与 Core 常量，不来自环境变量。
- 日志通过 `server.WithLogger` / `client.WithLogger` 注入的 `*slog.Logger`；未注入时丢弃日志，绝不退化为 `fmt.Print` 或标准输出直写。

同一进程多 Engine 并行：

- 两个 ServerEngine 绑定不同端口后可同时运行、互不影响；两个 ClientEngine 连接同一个或不同 ServerEngine 亦互不影响。
- 多实例之间不得共享 goroutine 池以外的可变状态；禁止按角色或名称做进程级去重。
- 测试用例在同一进程内至少同时运行两组 server+client，验证数据流、状态与日志相互独立。

### 3.5 垂直切片范围与数据流

```text
应用进程
├─ ServerEngine（注入 127.0.0.1:7000 listener，wire v1，TCP 代理 ssh:6000）
│     ▲ TCP 控制连接 + TCP 工作连接（wire v1）
└─ ClientEngine（注入 dialer，本地目标 127.0.0.1:22，代理名 ssh，远程端口 6000）
```

数据流：`外部访客 → 服务端 6000 → 工作连接 → ClientEngine → 本地 127.0.0.1:22 → 原路返回`。

- 控制会话覆盖：连接建立 → wire v1 选择 → 登录与鉴权 → 心跳。
- 代理覆盖：一个 TCP 代理，含双向字节流、半关闭、任意长度分片。
- wire v1 的最小消息集合仅包含支撑上述时序所需的登录、心跳、工作连接；其余消息族不属于本功能。
- 本切片不涉及 TLS、加密协商与压缩；相关能力待各自 FR 引入。
- 传输只实现 TCP（零第三方依赖，对应 FR-05a）。

### 3.6 错误契约

- 全部错误为 sentinel 值或包装 sentinel 的错误，宿主用 `errors.Is`、`errors.As` 判断，禁止依赖错误字符串。
- 本次引入的最小 sentinel 集合：`ErrAlreadyStarted`、`ErrNotStarted`、`ErrStopped`。
- 脱敏约束：`Err()` 返回的错误不得包含 token、密码、Authorization 或完整正文。

## 4. 任务拆分

- [x] 先写失败测试：在 `core` 内编写端到端失败测试（server+client 进程内互联、TCP + wire v1 + 一个 TCP 代理双向字节流），以及 Start 失败不关闭宿主 listener、Shutdown 后无 goroutine 泄漏、重复 Shutdown 幂等、重复 Start 返回 sentinel、多 Engine 并行互不影响；编写 `GOWORK=off` 外部 fixture 验证脚本用例（初始为红）
- [x] 确定并登记 Core 公共包布局（已由 ADR-0012 完成，无需另立 ADR）
- [x] 实现 `core/server` 与 `core/client` 的 New、Start、Shutdown、Done、Err 与内部最低限度状态机
- [x] 实现资源所有权转移：Start 成功接管宿主 listener，Start 失败保留给宿主，Shutdown 全部释放
- [x] 实现 TCP 传输接入与 wire v1 编解码的最小必要部分，只覆盖本切片时序
- [x] 实现控制会话登录与心跳，以及一个 TCP 代理的端到端转发
- [x] 实现可选 `*slog.Logger` 注入，保证日志脱敏且不成为业务契约
- [x] 补齐 `GOWORK=off` 外部 fixture：临时目录中的独立 Go module 只依赖已发布或本地相对路径的 Core 公共包，编译并运行闭环
- [x] 运行 `task test:core`、`task lint:go`、`go test -race ./...` 与依赖门检查
- [x] 同步 CHANGELOG 与受影响长期文档；PRD 中 FR-25 状态在全部验收通过后变更

## 5. 验收标准

正常路径：

- 外部 fixture 应用在 `GOWORK=off` 条件下，仅依赖 Core 公共包，完成「ClientEngine ↔ ServerEngine，TCP + wire v1 + 一个 TCP 代理」的数据闭环：外部连接服务端入口端口，字节流经隧道抵达客户端本地目标并原路返回，内容逐字节一致。
- Start 成功后 `Done` 未关闭、`Err()` 返回 nil；宿主可正常读写两个方向的流。
- Shutdown 后 `Done` 关闭，`Err()` 返回 nil，服务端入口端口不再可连接。

边界：

- 同一进程并行运行两组 server+client Engine，端口不同，两组数据流同时成功且互不影响；再启停其中一组，另一组不受影响。
- 半关闭：一端关闭写方向后，另一端仍能读完剩余数据，随后感知 EOF。
- 大块分片传输与任意长度边界（0 长度写、单字节、跨分片）数据一致。
- 未注入 logger 时 Engine 正常运行且不向标准输出直写日志。

错误路径：

- 重复 Start：第二次调用返回 `ErrAlreadyStarted`，不产生第二个监听器。
- Start 失败（例如注入的 listener 已被宿主关闭）：返回包装 sentinel 的错误；宿主注入的 listener 仍归宿主所有，宿主可再次 Close 而不 panic；Engine 创建的临时资源已释放。
- 未 Start 就 Shutdown：安全返回，不 panic；`Done` 行为符合 §3.2 声明。
- 重复 Shutdown：幂等返回，不 panic，不二次关闭资源。
- Stopped 后 Start：返回 `ErrStopped`。
- Shutdown 期间仍有活动流：在排水上限或 `ctx` 期限之前保持连续；超限后强制释放并返回可判定的错误。
- 任何错误路径下 `Err()` 与错误消息中不含 token、密码、Authorization 或正文原文。

资源与并发：

- Shutdown 完成后 goroutine 数量回到 Start 前基线（由泄漏检测断言），无残留监听套接字与出站连接。
- `go test -race ./...` 在 Core 上通过。
- Core 源码中不出现 `os.Exit`、`signal.Notify`、`http.Handle` 系列调用与可变包级变量；该断言由静态检查或 grep 门禁覆盖。
- Windows、Linux、macOS 三平台 `task test:core` 与 `task lint:go` 通过。

## 6. 风险与待定

- **公共包布局已由 ADR-0012 裁定**：本规格采用的 `core/server`、`core/client` → `core` 三层布局、依赖方向、具体类型优先于接口，以及 revision 归属，均以 ADR-0012 为准；变更需新增 ADR。版本策略部分由 ADR-0011 覆盖。
- **根 VERSION 与 Core 版本双轨**：已由 ADR-0011 确立——根 `VERSION` 管产品二进制，`core/VERSION` 管 Core module，tag 形式为 `core/vX.Y.Z`。长期文档提及版本真源时必须区分两者，不得再使用无修饰的"唯一版本真源"表述。
- **协议最小集合的边界**：本切片只实现支撑 client↔server TCP 隧道的 wire v1 消息。哪些消息属于 FR-04 的完整范围而哪些属于本切片，需在实现时严格划界，避免本功能悄悄成为协议第二实现路径。
- **dialer 注入形态**：ClientEngine 是否需要宿主注入 dialer（而非 Core 自行拨号）影响公共签名。当前假定提供 `client.WithDialer`，若实现阶段认为出站连接应完全由 Core 管理，应删除该选项以保持最小公共面。
- **Engine 是否需要 Err 入口**：`Err()` 与 `Done()` 的组合是本规格下的最小观测能力，FR-27 将在此之上提供有界事件订阅与只读状态快照。若 FR-27 先行，`Err()` 可能被其状态快照取代，届时应按 ADR 统一裁减，避免两套观测机制并存。
