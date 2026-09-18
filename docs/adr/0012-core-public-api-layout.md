# ADR-0012：Core 公共包布局与嵌入契约

## 状态

已接受

## 背景

FR-25 至 FR-28 把 Core 从"仅供 jrps/jrpc 使用的内部 module"变成"可被第三方 Go 应用嵌入的库"，但随之产生了若干必须由正式决策固定的问题，否则各功能规格会各自假设、互相固化：

1. **公共包布局未定**：FR-25 假定 `core`（配置值类型）、`core/server`（ServerEngine）、`core/client`（ClientEngine）三层，但这一布局尚未登记为决策，任何后续规格都可能采用不同划分，导致包路径漂移。
2. **last-good revision 的持有方未定**：FR-26 假定 Core 内存持有并回传宿主，FR-09 假定 SQLite 持久化并审计，FR-27 已在状态快照字段中把"Core 持有"当作既成事实。三份规格对同一概念给出不同归属。
3. **嵌入契约需要单一入口**：生命周期、资源所有权、错误处理、版本与稳定性承诺分散在多份规格中，第三方开发者没有一处权威说明。

本决策不引入新能力，只固化已确认的方向并裁决上述待定项，使后续规格有统一依据。

## 决策

### 1. 公共包布局

采用三层布局，仅有这三个公共导入路径：

| 路径 | 职责 | 导出内容 |
|---|---|---|
| `github.com/wcpe/jrp/core` | 配置值类型与构建器、版本查询、跨 client/server 共享的值类型（如 Engine 状态） | 配置构建入口、校验、版本常量、`Version()` |
| `github.com/wcpe/jrp/core/server` | 服务端嵌入门面 | `Engine`、`New`、选项、错误哨兵 |
| `github.com/wcpe/jrp/core/client` | 客户端嵌入门面 | `Engine`、`New`、选项、错误哨兵 |

- 依赖方向严格为 `core/server`、`core/client` → `core` → 标准库。反向依赖禁止，`core` 不得导入两个门面包。
- 协议、会话、传输、代理、NAT 等实现置于 `core/internal`。**`internal` 类型不得出现在任何公共签名中**。
- 公共签名只允许标准库类型与 Core 自有值类型，不得泄漏 Gin、GORM、SQLite、quic-go、kcp-go 等第三方实现类型（承 ADR-0002、ADR-0003）。
- 构造函数返回具体类型（如 `*server.Engine`），不预先抽取抽象接口。需要替身的宿主自行声明最小接口——不为尚未存在的需求创建猜测性抽象（承 `scope-discipline.md` 第 3 节）。
- P1 不新增第四个公共包；后续若有必要，按同一规则新增 ADR 扩展。

### 2. revision 归属裁决

desired、active、last-good 的归属统一如下，**此前的分歧以本表为准**：

| 概念 | 运行态归属 | 持久化归属 | 写入方 | 用途 |
|---|---|---|---|---|
| desired revision | Core 不知道 | jrps/jrpc 各自的 SQLite（唯一真源） | 只有控制面适配器 | 表达管理员期望，驱动 Apply，供审计 |
| active revision | Core 内存 | 外壳在 SQLite 保存 Apply 结果记录 | 只有 Core 的 publish 步骤 | 当前对新连接生效的快照版本 |
| last-good revision | Core 内存 | 外壳在 SQLite 保存 Apply 结果记录 | 只有 Core 的 publish 成功路径 | 回滚目标；最近一次成功的版本 |

裁决要点：

- **运行态与持久化记录是两件事，不是双真源**。Core 内存中的 active/last-good 服务于运行时决策（拒绝过期 revision、确定回滚目标）；SQLite 中的记录服务于审计与重启后展示。**真源性质只属于 desired**。
- Core **持有运行态 last-good**，因为回滚决策发生在 Core 侧，Core 必须知道"上一个成功版本是什么"。若改由宿主持有，Core 的 Apply 入口需要额外的 last-good 入参，反而把运行态知识泄漏到调用方。
- 外壳保存的是 **Apply 结果记录**（含 revision、阶段、成功与否、可公开的错误摘要），供审计、UI 展示与重启后重建。**这不等同于把 Core 内存快照持久化**——Core 不知道记录的格式，也不读取它。
- 仍然成立且不可违反：**Core active 不得冒充持久化真源**；**SQLite desired 不得冒充 Core active**。ADR-0004 的边界不被本裁决削弱。
- 进程重启后：外壳从 SQLite 恢复 desired，经同一 Apply 流程重建 Core 的 active 与 last-good 运行态。恢复逻辑在外壳。

### 3. 嵌入契约要点

以下为跨 FR-25 至 FR-28 的统一约定，细节以各功能规格为准：

- **生命周期**：`New`（纯内存构造）→ `Start(ctx)`（接管资源）→ `Shutdown(ctx)`（幂等，排水后释放）→ `Done()`（完全停止后关闭）。停止后不可重启。
- **资源所有权**：Start 成功后 Engine 接管宿主注入的资源；Start 失败资源仍归宿主；Shutdown 后无残留监听器、连接或 goroutine。
- **错误处理**：使用可 `errors.Is` 判断的哨兵错误；返回给宿主的错误中不含 token、密码、Authorization 或正文原文。
- **嵌入安全**：Core 不读取环境变量、配置文件或数据库；不调用 `os.Exit`；不注册全局信号、全局 HTTP 路由或可变单例；同一进程可并行运行多个 Engine。
- **版本与稳定性**：按 ADR-0011 双轨。v1 之前不承诺 wire、session、传输内部类型的稳定性；稳定面清单在 `docs/specs/core-version-policy.md` 维护。
- **public API 变更**：凡改变上述任一条的改动，须新增 ADR 取代或扩充本决策，不得在功能规格中局部推翻。

## 理由

- **三层布局最小且够用**：共享值类型必须有地方放，`core` 是最自然的位置；两个门面天然分属 server/client，且互不依赖。三层能表达全部已确认需求，第四层目前没有真实驱动力。
- **具体类型优先于接口**：当前尚未出现第二个 Engine 实现，抽取接口只会增加无测试的抽象面。Go 的惯例是"接受接口、返回结构体"，宿主需要替身时自行声明最小接口即可。
- **last-good 归 Core 而非宿主**：回滚是运行时行为，决策权应在掌握当前状态的一方。让宿主传参会在每次 Apply 时重复传递 Core 已经知道的信息，且容易传错。
- **区分运行态与持久化记录**：化解 FR-09/26/27 分歧的关键在于认识到二者目的不同——前者服务决策，后者服务审计。明确这一区分后，"谁是真源"的答案唯一且不变：只有 desired。
- **契约集中登记**：第三方开发者需要一处权威说明。分散在四份规格里会导致任何一份更新时其他三份不同步。

## 后果

- `docs/specs/core-engine-facade.md`（FR-25）、`core-snapshot-apply.md`（FR-26）、`core-event-subscription.md`（FR-27）、`sqlite-configuration-store.md`（FR-09）中的布局与 last-good 假设**自本决策起具有依据**，不再是待定项。
- FR-26 第 6 节"Core 是否持有 last-good 需正式确认"与 FR-27 的快照字段注释应改为引用本决策。
- FR-25 第 6 节"公共包布局尚未登记为 ADR"应改为引用本决策；剩余的"Core 公共 API ADR"待决项收敛为本 ADR。
- 新增第四个公共包或改变三层依赖方向，必须新增 ADR。
- 后续规格描述 Active/last-good 时，必须区分"运行态"与"持久化记录"两种语境，不得混用。

## 备选方案

- **单一 `core` 包容纳全部内容**：导入路径更短，但 ServerEngine 与 ClientEngine 的类型同处一个包会显著扩大单个包的导出面，且 client 使用者被迫看到 server 符号——不利于嵌入宿主按需理解 API，拒绝。
- **按能力分更多包**（如 `core/proxy`、`core/wire` 也对外）：初期看似"面向未来友好"，但 P1 尚无第三方需要单独消费这些能力，且会把本应属于 `internal` 的实现面固化成公共契约，为将来重构背上兼容负担，拒绝。
- **last-good 完全由宿主持久化**：表面上更贴近"Core 不管持久化"，但 Core 的回滚决策需要该值，改为每次 Apply 由宿主传入会把运行态知识泄漏给调用方并引入传错风险，拒绝。
- **不对 last-good 做裁决，留待实现时定**：会让 FR-27 继续持有"Core 持有"的既成假设，而 FR-09 继续持有"外壳持有"的假设，两份规格在同一概念上无法对齐，拒绝。
