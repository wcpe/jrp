# 功能规格：Core 类型化配置构建器与校验 API

> 状态：草拟 · 关联 PRD：FR-32 · 分支：feature/core-config-builder

## 1. 背景与目标

Core 要成为可被第三方 Go 应用嵌入的库，宿主必须在代码里构造配置，再由 Engine 消费。PRD §8 把「配置构建器」定义为 Core 公共 API 中构造并校验不可变配置快照的类型化入口，只接受宿主在代码中传入的值，不读取文件或环境变量；FR-32 把这一条固化为独立需求。

本功能解决的问题：

- 宿主无法用类型安全的方式表达服务端与客户端配置，只能靠 map 或字符串拼装，错误在运行时才暴露。
- 非法配置（端口越界、缺鉴权、代理名重复、目标地址非法）若以 panic 或未定义行为表现，会让嵌入宿主的进程整体崩溃，违反 NFR「嵌入安全性」。
- 若 Core 自己读取配置文件、环境变量或数据库，数据面就会反向依赖运行环境，破坏 ADR-0002 与 ADR-0003 确立的边界。

目标：为嵌入宿主提供**一套类型化、可组合、可校验、产出不可变值**的配置构建入口，覆盖 ServerEngine 与 ClientEngine 两侧；所有校验失败以明确的错误值返回，绝不 panic。

使用者：第三方 Go 应用开发者（直接消费 Core 公共包）、jrps 与 jrpc 适配器（把 SQLite desired 转换为 Core 快照）。所属阶段：P1。

## 2. 需求

- 提供服务端配置与客户端配置两类构建入口，两侧都可用选项函数组合。
- 构建结果是不可变值：构建完成后没有 setter，读取切片与映射返回副本，Snapshot 进入 Core 时再深复制一次。
- 提供独立校验 API，宿主可在交给 Engine 之前预检；构建函数在返回前自动执行同一套校验。
- 每类非法配置返回可判定、可分类的错误值，可通过 `errors.Is` 与 `errors.As` 识别，携带稳定字段路径与安全可公开的消息。
- 错误消息、日志与状态快照不得回显 token、密码或其他凭证原文。
- 公共接口只暴露标准库类型与 Core 自有值类型。
- Core 测试与实现不得读取文件、环境变量或数据库。

- **范围内**：
  - 服务端配置：监听端点、wire 版本、客户端凭证集合、代理绑定集合、心跳与超时参数。
  - 客户端配置：客户端标识、服务端端点、鉴权材料、本地代理集合、心跳与超时参数。
  - 构建器选项函数、不可变值语义、独立校验 API、分类错误模型。
  - 随 FR-25 首个垂直切片启用的取值集合：TCP 传输、wire v1、TCP 代理。
  - 数量与长度上限的边界校验，防止无界分配。
- **范围外**：
  - 不读取 toml、ini、yaml、json 等任何配置文件，不提供 `LoadFile`、`Parse` 之类的反序列化入口。
  - 不读取环境变量，不提供环境变量回退。
  - 不做远程配置拉取、不做 SQLite 或文件持久化、不做配置版本迁移与 diff。
  - 不包含正文采集开关、通知、Web、CLI、日志目标、数据目录等外壳配置；配置持久化真源仍在 jrps/jrpc 各自的 SQLite（ADR-0004）。
  - 不提供 `AddProxy`、`RemoveProxy`、`UpdatePort` 等增量修改 API；配置变更只能重建完整快照后由 FR-26 的 Apply 应用。
  - 不实现 wire v2、KCP、QUIC、WebSocket/WSS 传输，以及 UDP、HTTP、HTTPS、STCP、XTCP 代理；相应取值由 FR-04、FR-05a/05b/05c、FR-06a/06b 的规格随其实现一并加入，不在本功能预留空枚举。
  - 不提供插件 SPI、模板渲染、变量替换或表达式求值。

## 3. 设计

### 3.1 包布局与依赖方向

- 配置类型、构建器选项与错误模型位于 Core 根包 `core`（导入路径 `github.com/wcpe/jrp/core`），作为公共入口。
- 具体实现可位于 `core/internal/config`，公共包只转发类型与函数，`internal` 类型不得出现在公共签名中。
- 依赖方向严格单向：配置包只依赖标准库（`net/netip`、`time`、`errors`）；不依赖 `core/server`、`core/client`，也不依赖 `apps/*`、Gin、GORM、SQLite、Web、通知、CLI。
- 公共签名中出现的类型只有三类：标准库类型（`netip.AddrPort`、`time.Duration`、`string`、`int`、`error`）、Core 自有值类型（`Transport`、`WireVersion`、`ProxyType` 等具名字符串枚举）、Core 自有结构体（字段全部为上述类型）。
- 不泄漏 `net.Listener`、`net.Conn` 等接口类型作为配置字段；宿主注入的网络资源属于 FR-25 与 FR-26 的 Engine 与 Deployment 层，不进入配置值。
- 边界与依赖规则依 ADR-0002（多 module 与 Core 边界）、ADR-0003（控制面与数据面分离）、ADR-0004（SQLite 配置真源）；本功能不重复其决策正文。

### 3.2 API 形态

客户端配置：

```go
cfg, err := core.NewClientConfig(
    core.WithClientID("client-a"),
    core.WithServerEndpoint(core.ServerEndpoint{
        Address:   netip.MustParseAddrPort("127.0.0.1:7000"),
        Transport: core.TransportTCP,
        Wire:      core.WireV1,
    }),
    core.WithClientAuth(core.TokenAuth{Token: token}),
    core.WithTCPProxy(core.TCPProxy{
        Name:       "ssh",
        LocalAddr:  netip.MustParseAddrPort("127.0.0.1:22"),
        RemotePort: 6000,
    }),
    core.WithHeartbeat(30*time.Second),
)
if err != nil {
    // 宿主自行决定日志与退出；Core 不打印、不退出
    return err
}
```

服务端配置：

```go
scfg, err := core.NewServerConfig(
    core.WithListen(core.BindEndpoint{
        Address:   netip.MustParseAddrPort("0.0.0.0:7000"),
        Transport: core.TransportTCP,
    }),
    core.WithWire(core.WireV1),
    core.WithClientCredential(core.ClientCredential{
        ClientID: "client-a",
        Token:    token,
    }),
    core.WithTCPProxyBinding(core.TCPProxyBinding{
        Name:       "ssh",
        ClientID:   "client-a",
        RemotePort: 6000,
    }),
)
```

约束：

- `NewClientConfig` 与 `NewServerConfig` 返回不可变配置值，**构建时即执行完整校验**，校验失败返回 `*core.ConfigError` 包装的错误，不返回半构造对象。
- 独立校验入口 `cfg.Validate() error` 返回同一套错误模型，供宿主在 Apply 前预检；`Validate` 无副作用，可重复调用。
- 选项函数只做赋值，不做校验；校验集中在构建末尾一次完成，保证错误顺序确定。
- 选项可叠加：`WithClientCredential`、`WithTCPProxy`、`WithTCPProxyBinding` 可多次调用；名称冲突由校验发现，不由选项 panic。
- 取值枚举为具名常量（`core.TransportTCP`、`core.WireV1`、`core.ProxyTypeTCP`），宿主不得用裸字符串字面量构造，避免拼写漂移。
- 不支持的枚举值不是「预留」，而是校验失败：宿主传入 `core.WireV2` 在 wire v2 交付前不存在该常量；常量一旦存在即代表对应能力已按各自 FR 交付。

### 3.3 不可变语义

- 配置结构体的字段全部私有，只有读取方法（`cfg.ClientID()`、`cfg.Proxies()` 等）。
- 读取集合类字段返回深复制的新切片；读取结构体字段返回值副本；字符串与标量按值返回。
- 构建器在接收选项时即对入参切片做一次复制，宿主后续修改自己的切片不影响已构建的配置。
- FR-26 的 Snapshot 在接收配置值时再深复制一次，形成双重隔离；配置值本身不持有 `net.Listener`、`*slog.Logger` 或任何可变共享对象。

### 3.4 校验规则

| 类别 | 触发条件 | 错误码 | 说明 |
|---|---|---|---|
| 空配置 | 客户端未设置服务端端点；服务端未设置监听端点 | `CodeIncomplete` | 必填字段缺失 |
| 空配置 | 服务端未配置任何客户端凭证 | `CodeIncomplete` | 无凭证的服务端无法完成登录鉴权 |
| 端口越界 | 端口为 0、负数或大于 65535 | `CodePortOutOfRange` | 含监听端口、远程端口、本地目标端口；不支持端口 0 自动分配 |
| 缺鉴权信息 | 客户端未设置鉴权材料；凭证集合中 Token 为空；ClientID 为空 | `CodeMissingAuth` | 错误消息只说明缺失字段，不回显 token |
| 代理名重复 | 同一配置内两个代理同名 | `CodeDuplicateProxyName` | 服务端按全局唯一判定，客户端按本客户端唯一判定 |
| 目标地址非法 | `AddrPort` 未指定 IP（`!IsValid`）、IP 为未指定地址而场景要求明确主机、端口为 0 | `CodeInvalidAddress` | 不代替宿主解析字符串；宿主应使用 `netip.ParseAddrPort` 处理不可信输入 |
| 未支持取值 | 传输、wire 版本或代理类型取值为空或不在已交付常量集合内 | `CodeUnsupportedValue` | 给出受支持的取值列表，不猜测宿主意图 |
| 越权绑定 | 服务端代理绑定引用了不存在的 ClientID | `CodeUnknownClient` | 与缺鉴权分开表达 |
| 数量与长度超限 | 代理条目数、客户端凭证数超过 Core 常量上限；名称长度为空或超过上限 | `CodeLimitExceeded` | 上限由 Core 常量定义，不提供宿主可调参数，避免无界分配 |
| 时间参数非法 | 心跳间隔、超时为负值 | `CodeInvalidDuration` | 零值表示使用 Core 默认常量；负值明确报错 |

补充规则：

- 校验按固定顺序执行并**收集全部问题**：一次返回包含所有错误的聚合错误，可用 `errors.As` 取出 `*core.ConfigError` 列表，便于宿主一次修完。
- 错误消息使用中文，包含稳定字段路径（例如 `proxies[1].remotePort`、`clients[0].token`），不含绝对路径、不含凭证原文。
- `ConfigError` 实现 `Error() string`、`Unwrap() error`，并提供 `Code`、`Field`、`Message` 读取方法；`core.ErrConfigInvalid` 作为可被 `errors.Is` 匹配的哨兵。
- 字符串匹配不构成稳定契约，宿主必须靠错误码判断。

### 3.5 与下游功能的衔接

- 本功能产出的 `ClientConfig` 与 `ServerConfig` 是 FR-26 中完整 Snapshot 的配置内容；revision、宿主注入资源和 Apply 语义不属于本功能。
- FR-25 的 `server.New`、`client.New` 接收本功能产出的配置值，并各自再执行一次 `Validate`，防止宿主绕过构建器直接零值构造。
- 采集开关、通知目标、审计等外壳概念不进入配置值，jrps 适配器把 SQLite 中的这些字段留在自己的模型里（ADR-0003）。

## 4. 任务拆分

- [x] 先写失败测试：为 §3.4 每一类校验规则编写表驱动用例，断言返回 `*core.ConfigError` 且错误码与字段路径正确，断言不 panic；同时编写不可变性测试（修改宿主入参切片后配置值不变、读取集合后改动返回值不影响配置值）与「Core 包内无 os.Getenv/os.Open 调用」的依赖门测试
- [x] 定义配置值类型、枚举常量与数量上限常量，字段全部私有并配套读取方法
- [x] 实现选项函数与 `NewClientConfig`、`NewServerConfig`，构建末尾执行全量校验并聚合错误
- [x] 实现 `Validate` 与错误模型（哨兵、错误码、`ConfigError`、`Unwrap`），保证消息脱敏
- [x] 实现深复制与不可变语义，覆盖切片、嵌套结构体与凭证字段
- [x] 运行 `task test:core`、`task lint:go` 与依赖门检查，确认 Core 依赖图无 Gin、GORM、SQLite、`apps/*` 与 frp
- [x] 同步 CHANGELOG 与受影响的长期文档；PRD 中 FR-32 的状态在全部验收通过后再变更

## 5. 验收标准

正常路径：

- 用 §3.2 的两段示例分别构造客户端与服务端配置，构建成功且 `Validate` 返回 nil；读取得到的代理列表与凭证数量与输入一致。
- 重复调用 `Validate` 结果一致且无副作用。
- 构建后修改宿主持有的原始切片，配置值内容不变；读取配置值的代理切片并修改返回值，再次读取仍为原值。

边界：

- 心跳与超时字段为零值时采用 Core 默认常量，构建成功。
- 代理条目数恰为上限时构建成功，超过上限一条即返回 `CodeLimitExceeded`（测试引用 Core 导出的上限常量，不硬编码数值）。
- 端口取 1 与 65535 构建成功；取 0 与 65536 返回 `CodePortOutOfRange`。
- 代理名长度为 1 与上限值时构建成功；空名返回 `CodeLimitExceeded`，超长名同样返回该错误码（上限同样引用 Core 导出常量）。

错误路径（每类均返回错误而非 panic，且 `errors.Is(err, core.ErrConfigInvalid)` 为真）：

- 未提供服务端端点、未提供监听端点、服务端无客户端凭证 → `CodeIncomplete`。
- 监听端口 0、远程端口 65536、目标端口 -1 → `CodePortOutOfRange`，字段路径指向具体字段。
- 客户端无鉴权材料、凭证 Token 为空、ClientID 为空 → `CodeMissingAuth`，错误消息中出现字段路径但不出现 token 原文。
- 同名代理重复出现 → `CodeDuplicateProxyName`，字段路径指向重复条目的下标。
- `netip.AddrPort` 零值、端口为 0 的地址 → `CodeInvalidAddress`。
- 传输或 wire 取值为空字符串或不在已交付常量集合内 → `CodeUnsupportedValue`。
- 代理绑定引用不存在的 ClientID → `CodeUnknownClient`。
- 心跳为负、超时为负 → `CodeInvalidDuration`。
- 一次构造中同时存在端口越界、缺鉴权与重名三类问题 → 聚合错误中包含全部三类，错误码与字段路径各自正确。

依赖与门禁：

- Core 依赖图穷举不含 Gin、GORM、SQLite、Web、通知、`apps/*` 与 `github.com/fatedier/frp`。
- Core 生产源码中不存在 `os.Getenv`、`os.Open`、`os.ReadFile` 或任何数据库驱动导入；该断言由依赖门脚本扫描非 `_test.go` 的 Go 源码覆盖。测试文件不受此约束，因为测试读取自身 `testdata` 属正当行为且不进入二进制产物。
- `task test:core` 与 `task lint:go` 在 Windows、Linux、macOS 均通过。

## 6. 风险与待定

- **包布局已由 ADR-0012 裁定**：配置类型与构建器位于 Core 根包 `core`，与 `core/server`、`core/client` 构成三层公共布局。本规格的包位置不再随后续 ADR 变动；如需新增第四个公共包，须新增 ADR 并按其修订本规格。
- **配置上限常量的取值需量化**：代理条目数、客户端凭证数、名称长度上限目前只确定「由 Core 常量定义且宿主不可配置」这一原则，具体数值应在实现时结合 NFR 规模基线（200 客户端、2000 代理）确定并写入代码常量与 CHANGELOG。
- **地址表达方式**：本规格选用 `netip.AddrPort` 以保证值语义与可比较性，代价是宿主需自行解析字符串。若实现阶段发现大量宿主场景要求字符串地址，需评估是否补充解析辅助函数，但不得引入 `net.Listener` 之类的接口字段。
- **聚合错误的呈现形式**：`errors.Is` 与 `errors.As` 能识别单条 `ConfigError`，聚合多条错误时的遍历方式需在实现时定型，并作为公共契约的一部分写入文档。
- **凭证在配置值中的存在形式**：本规格允许配置值持有 token 明文（NFR 明确凭证可明文静态存储），但错误、日志与后续 FR-27 的状态快照必须脱敏。若安全评审要求 Core 不接受明文 token 而改由宿主注入鉴权回调，需新增 ADR 后调整。
