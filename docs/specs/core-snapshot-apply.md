# 功能规格：完整不可变 Snapshot/Revision 的 Apply（prepare → publish → drain）

> 状态：草拟 · 关联 PRD：FR-26 · 分支：feature/core-snapshot-apply

## 1. 背景与目标

配置变更若通过重启或「先关旧监听再开新监听」生效，会切断已有代理连接；若准备过程失败就直接替换 active，又会把配置错误放大为线上故障。ADR-0005 已把非引导配置的应用固定为 `prepare → health-check → atomic publish → drain` 状态机，FR-10 是 jrps/jrpc 外壳侧的同名要求。FR-26 解决的是**这一状态机在 Core 公共 API 上的具体形态**：宿主提交完整不可变快照与 revision，Core 在保持已有连接与监听入口连续的前提下完成无中断切换。

本功能解决的问题：

- 增量式配置 API（AddProxy/RemoveProxy）会形成第二套配置真源，与「SQLite desired」和「Core active」的分工冲突；必须只接受完整快照。
- 未变化入口若被"关闭再重建"，端口会在切换窗口内消失、backlog 里的待接入连接一并丢失；必须明确入口的复用条件与所有权交接时机。
- 准备阶段失败若污染 active，就会同时失去旧版本与新版本；必须有可验证的保留承诺。

目标：把 ADR-0005 的状态机落成 Core 的 Apply 公共 API，明确 revision 三态语义、未变化入口的复用与交接方式、并发与超时行为，以及失败后的资源释放与回滚路径。

使用者：jrps 与 jrpc 适配器的配置编排层；需要热更的第三方嵌入宿主。所属阶段：P1。

## 2. 需求

- Apply 只接受**完整**配置快照，不提供任何增量修改入口。
- 快照不可变：进入 Core 时深复制，Core 内部不修改宿主持有的值。
- 状态机四步固定：prepare → health-check → atomic publish → drain。
- publish 之前任一步失败，必须释放本次新建资源并保留旧 active 与 last-good，不得出现半生效状态。
- 已有连接与活动流在切换期间保持连续；drain 期间旧资源不再接收新流量。
- 明确 desired、active、last-good 三个 revision 的语义与归属，Core 不冒充持久化真源。
- 并发、超时、取消、回滚与资源释放都有确定行为与可判定错误。

- **范围内**：
  - Deployment 概念：revision + 不可变 snapshot。
  - Apply 的四步状态机、每步的成功与失败语义。
  - 入口标识对齐：未变化入口复用同一监听套接字并交接所有权，新增入口接管，移除入口排空。
  - 单飞语义：同一 Engine 同一时间只允许一个 Apply 进行中。
  - drain 上限与强制释放，以及 publish 后 drain 异常的处置。
  - Apply 结果（阶段、成功/失败、安全可公开的错误摘要）返回给宿主。
  - 首版在 FR-25 定义的 TCP + wire v1 + TCP 代理能力范围内验证；FR-25 交付前本功能不具备验证前提。
- **范围外**：
  - 不读取 SQLite、不写 apply 历史表、不做 desired 持久化与审计落库；持久化与审计属于 jrps/jrpc 适配器（ADR-0004）。
  - 不实现自动回滚到「上一个版本」的隐式行为；回滚由宿主创建并应用**新的** desired revision 后走同一状态机（ADR-0005 后果节）。
  - 不实现引导参数热更：数据目录、SQLite 路径、管理 API 根监听变更仍需重启；控制会话身份（服务端端点、客户端标识、令牌）同理，发生变化时明确拒绝而不是静默沿用旧连接。
  - 不实现宿主注入网络资源：访客入口一律由 Core 按快照自建，`Deployment` 不接受宿主注入的 `net.Listener`。宿主注入与托管语义属独立后续需求（见 §6）。
  - 不实现 UDP 入口的会话参数热更：参数变化须一并更换该代理端口（见 §3.4）。
  - 不实现配置 diff 算法、可视化对比、版本图谱或历史版本查询 UI。
  - 不实现事件发布与只读状态快照：属于 FR-27（本功能只定义「apply 结果」这一返回值，不定义事件通道）。
  - 不实现 P3 的多节点一致性、选举、分布式锁或共享配置（ADR-0009）。
  - 不实现 wire v2、KCP/QUIC/WebSocket/WSS 与非 TCP 代理的热更验证。

## 3. 设计

### 3.1 revision 三态语义

| 概念 | 归属 | 含义 | 谁能写入 |
|---|---|---|---|
| desired revision | jrps/jrpc 各自的 SQLite | 管理员或服务端期望应用的配置版本 | 只有控制面适配器 |
| active revision | Core 内存 | 当前对新连接生效的不可变快照版本 | 只有 Core 的 publish 步骤 |
| last-good revision | Core 内存（外壳保存 Apply 结果记录，见 ADR-0012） | 最近一次成功完成 prepare、health-check 与 publish 的版本 | 只有 Core 的 publish 成功路径 |

不可混淆规则（承 ADR-0004、ADR-0005，此处不重复正文）：

- **Core active 不得冒充持久化真源**：Core 不提供「把 active 写回数据库」的入口，也不保证进程重启后 active 仍在。
- **SQLite desired 不得冒充 Core active**：宿主不得把 desired 版本号直接当作「已生效版本」上报给用户；生效事实只能来自 Core 返回的 Apply 结果与状态查询。
- 进程重启后由适配器从 SQLite 恢复 desired，再通过同一 Apply 流程建立 active；这段恢复逻辑在适配器侧，不在 Core。
- last-good 只在 publish 成功时更新；prepare 或 health-check 失败不触碰 last-good。

### 3.2 Deployment 与快照不可变性

```go
type Deployment struct {
    Revision uint64        // 单调递增，由宿主（SQLite 侧）分配
    Snapshot core.Snapshot // 完整不可变配置快照（由 FR-32 构建器产出）
}
```

res, err := eng.Apply(ctx, dep)
```

- `Snapshot` 是**完整**配置：包含全部客户端、全部代理、全部传输与鉴权，不接受局部补丁。
- Apply 入口对 `Snapshot` 做深复制；返回值中的状态也不与 Core 内部共享底层数组。
- `Revision` 由宿主分配并单调；Core 只做「与当前 active 比较」：等于 active 视为幂等成功，小于 active 视为过期并拒绝（避免乱序重放），大于则执行切换。
- **入口标识对齐**：每个访客入口有一个在配置内唯一的标识（TCP、HTTPS、UDP 用代理名，共享 HTTP 入口用端口派生的登记名）。标识与绑定地址都未变的入口在新旧 revision 中是**同一个监听套接字**，由 prepare 直接接管而不重建；标识消失的入口进入 drain；新增标识的入口在 prepare 阶段新建。

### 3.3 四步状态机

```text
Apply 入口
  │
  ├─ 0. validate：快照结构校验（复用 FR-32 校验）+ revision 比较
  │     失败 → 返回 ApplyError{Stage: Validate}，active 不变
  │
  ├─ 1. prepare：构造新监听器、会话、路由与依赖资源
  │     · 不影响 active，不关闭任何旧资源
  │     · 未变化绑定资源直接复用，不重建
  │     失败 → 释放本步新建资源，返回 ApplyError{Stage: Prepare}
  │
  ├─ 2. health-check：验证新资源可用（端口绑定成功、路由可解析、必要依赖可达）
  │     失败 → 释放本步与 prepare 新建资源，返回 ApplyError{Stage: HealthCheck}
  │
  ├─ 3. atomic publish：一次性把 active 指针切到新快照
  │     · 新连接只观察新版本；切换对已有连接不可见
  │     · 成功后更新 active 与 last-good
  │
  └─ 4. drain：旧版本中被移除或替换的资源停止接收新流量
        · 已有流继续到自然结束或排空上限
        · 超上限 → 强制释放，进入告警路径（不回切 active）
```

关键承诺：**publish 之前的任何失败都释放本次新建资源，并完整保留旧 active 与 last-good。** 该承诺是 ADR-0005 的直接后果，本功能不另立规则。

### 3.4 未变化入口的复用与所有权交接

访客入口由 Core 按快照创建（不是宿主注入），因此复用的难点不在"谁拥有"，而在**如何在不停掉端口的前提下把入口交给下一代**。关闭再重建会让端口在切换窗口内消失，backlog 里待接入的连接也会一起丢掉，正是「无中断」要避免的。

| 阶段 | 新建入口 | 复用的入口（标识与绑定地址都未变） | 被移除或替换的入口 |
|---|---|---|---|
| prepare | 绑定端口，资源填入新代 | **接管上一代的同一个监听套接字**，不重绑 | 不触碰，仍在服务 |
| publish 成功 | 归新代所有 | 所有权在交接那一刻转移给新代 | 转入待排空 |
| publish 前失败 | 释放本步新建的全部入口 | **不释放**，仍归上一代 | 保持服务 |
| drain | 不适用 | 不适用 | 停止 Accept／Suspend 后，已有连接跑到自然结束，随后关闭 |

约定：

- 流入口的交接分两步且顺序固定：**先让旧代停止接收，再启动新代的接收循环**。反过来的话两个循环会同时挂在同一个监听器上，同一个端口上会出现新旧两套配置并存。
- 停掉旧代的 Accept 不为关闭套接字：把 Accept 的截止时间设为已过去的时刻令其立即以超时返回，旧代据此退出循环；交接完成后必须清空该截止时间，否则新代的 Accept 会一直立即超时，入口表现为「在监听却收不到连接」。
- 该窗口内到达的连接留在内核 backlog 里，由新代的循环接走，不丢连接。
- UDP 入口没有 Accept 循环，改用「停止接收但不回收会话」的挂起语义：套接字随入口归新代，已有会话继续服务到自然结束。
- UDP 的会话参数（空闲上限、会话上限、数据报上限）在入口构造期固定。因此这三项也构成复用条件：**参数变了但端口没变**时既不能复用（参数无法生效）也不能重绑（旧入口仍占端口），按 `Stage: Prepare` 明确拒绝并说明原因，而不是让它表现为端口占用这种与原因无关的失败。
- drain 结束后关闭被替换入口时只调用一次 `Close`，对已关闭资源不重复关闭导致 panic。
- Core 不得在持有资源表锁时执行网络 IO 或等待 drain；drain 的等待在锁外进行。

### 3.5 并发、超时与取消

- **单飞**：同一 Engine 同一时间只允许一个 Apply 进行中；并发调用返回可用 `errors.Is` 判断的 `ErrApplyInProgress`，不排队、不静默合并。
- 排队与 latest-wins 策略不由 Core 决定，由宿主控制面负责。
- `ctx` 取消或超时：Apply 立即中止当前阶段，按「publish 前失败」规则释放资源并保留旧版本，返回包装 `context.Canceled` 或 `context.DeadlineExceeded` 且带阶段的错误。
- drain 阶段不受 Apply 的 `ctx` 期限约束：publish 已成功，切换不可撤销；drain 使用独立的排水上限，由快照中的排水时限或 Core 常量决定，取较小者。
- Apply 返回后再发起下一次 Apply：允许；并发窗口由单飞约束保证串行。

### 3.6 回滚语义

- **隐式回滚到旧快照被禁止**：publish 成功后旧版本已不再接收新连接，回切会让新连接二次中断并破坏版本归属。
- 正式回滚流程：宿主在 SQLite 创建**新的** desired revision（内容等同目标旧版本），通过同一 Apply 提交；Core 无法区分这是「回滚」还是「新版本」，因此行为一致且可审计。
- publish 之后 drain 阶段失败：不回切 active；通过 Apply 结果把「发布成功但排空异常」暴露给宿主，由宿主记录 apply result 并触发告警与审计。

### 3.7 Apply 结果与错误模型

```go
type Result struct {
    Revision     uint64   // 本次应用的 revision
    Previous     uint64   // 切换前的 active revision
    Stage        Stage    // 终止阶段：最后一次成功推进的阶段（Applied 表示 publish 完成，Drained 表示 drain 完成）或首个失败阶段（Validate / Prepare / HealthCheck）
    Changed      int      // 本次新增与变化的资源数
    Drained      int      // 本次排空的资源数
    Error        error    // 安全可公开的错误摘要原因，成功为 nil
}
```

- `Result` 与错误都不得包含 token、密码、Authorization 或正文原文。
- 错误使用 sentinel + 阶段标记：`errors.Is(err, core.ErrApplyInProgress)`、`errors.As(err, &applyErr)` 后读 `applyErr.Stage`。
- 字符串匹配不构成稳定契约。

## 4. 任务拆分

- [x] 先写失败测试：编写「prepare 失败后 active 与 last-good 不变且新资源已释放」的用例，以及「切换监听资源后已有流保持连续、新连接进入新 revision」的用例；补充并发 Apply 返回 `ErrApplyInProgress`、ctx 超时保留旧版本、drain 超上限强制释放、重复 Shutdown、快照深复制无别名共五组失败测试（初始为红）
- [x] 定义 Deployment、Result 与阶段枚举
- [x] 实现 validate 与 revision 比较（幂等、过期拒绝、单调递增）
- [x] 实现 prepare：构造新资源、复用未变化入口、不触碰 active
- [x] 实现 health-check 与失败路径的资源回滚（释放本步与 prepare 新建资源）
- [x] 实现 atomic publish：指针原子切换、active 与 last-good 更新、新连接版本归属
- [x] 实现 drain：旧资源停止接收新流量、已有流跑到自然结束或排空上限、结束后一次性关闭资源
- [x] 实现单飞约束、ctx 取消/超时语义与错误模型
- [x] 暴露当前 active 与 last-good revision 的只读查询，供宿主区分 desired 与 active
- [x] 运行 `task test:core`、`task lint:go`、`go test -race ./...` 与依赖门检查
- [x] 同步 CHANGELOG 与受影响长期文档；PRD 中 FR-26 状态在全部验收通过后变更

## 5. 验收标准

正常路径：

- 首次 Apply 建立 active：返回 `Stage: Drained`，active 与 last-good 均等于本次 revision，新连接使用新配置。
- 二次 Apply（仅修改代理端口）：新连接进入新端口，旧端口在新 revision 发布后不再接受新连接；发布前已建立的连接与活动流保持连续，数据传输不中断且内容完整。
- revision 等于当前 active 的重复 Apply 返回幂等成功，不改变资源，不中断现有连接。
- 复用场景：入口标识与绑定地址都未变的入口在新旧两个 revision 中是**同一个监听套接字**，未被重建。
- 新增代理：已有代理原样保留（入口地址不变、已建立的活动流连续、仍接受新连接），新代理的入口可用。
- 换 UDP 端口：新端口接管成功，旧端口随旧代关闭并可被再次绑定。

边界：

- 空代理集合的 snapshot：Apply 成功，旧代理全部进入 drain 并按上限排空。
- 排空上限到达：超限后强制释放剩余连接，返回含「发布成功但排空异常」标记的结果，active 仍为新版本。
- 无在途流时排空立即收敛，结果不得标记为排空异常（控制连接跨代存活，不计入代的排空等待）。
- drain 期间新连接只进入新版本，不得被旧资源接受。
- 未变化资源复用：一组代理在两次 Apply 中保持不变时，其连接不受任何影响。
- 复用入口的交接窗口内到达的连接不丢：内核 backlog 会保留它们，由新代的接收循环接走。
- UDP 会话参数变化而端口未变：`Stage: Prepare` 明确拒绝并说明原因，active 不变。

错误路径（每类均返回错误而非 panic，且 active 与 last-good 保持不变）：

- 快照结构非法（端口越界、代理名重复、缺鉴权等，复用 FR-32 校验）→ `Stage: Validate`。
- prepare 阶段失败（例如新端口被占用、依赖资源不可用、UDP 参数变更但端口未变）→ `Stage: Prepare`，本次新建资源已完整释放，被接管的入口仍归上一代，宿主可修正后重试。
- health-check 失败 → `Stage: HealthCheck`，prepare 与本步新建资源全部释放，旧服务继续可用。
- 过期 revision（小于当前 active）→ 拒绝并带阶段标记，不改变 active。
- 并发 Apply → 第二个调用返回 `ErrApplyInProgress`，不排队、不破坏进行中的切换。
- Apply 期间 `ctx` 取消或超时 → 中止并释放新资源，保留旧 active 与 last-good，返回可 `errors.Is` 判定 context 原因且带阶段的错误。
- drain 阶段失败或超上限 → 不回切 active；结果标记为排空异常，由宿主记录并告警。

状态与资源：

- 通过只读查询可分别读到 active 与 last-good revision；失败路径下两者都与变更前一致。
- 传入的 snapshot 在 Apply 后被宿主修改，Core 内 active 不受影响（深复制生效）；返回的状态对象与 Core 内部无切片或映射别名。
- 全程无 goroutine 泄漏；`go test -race ./...` 通过；Shutdown 后无残留监听器与连接。
- Apply 结果、错误与日志中不含 token、密码、Authorization 或正文原文。
- Windows、Linux、macOS 三平台 `task test:core` 与 `task lint:go` 通过。

## 6. 风险与待定

- **last-good 归属已由 ADR-0012 裁定**：Core 持有运行态 last-good 供回滚决策使用，外壳在 SQLite 保存 Apply 结果记录供审计与重启后展示；真源性质只属于 desired，不因本裁决改变。
- **排空上限的取值来源**：候选为快照字段或 Core 常量。快照可配会扩大公共面和为宿主提供「无限排空」的误用空间，建议 Core 常量优先；最终选择应在实现时定型并写入长期文档。
- **revision 过期拒绝的策略**：本规格选择「小于 active 直接拒绝」。若宿主存在多控制面并发下发的场景需重新评估；P1 单机单管理员下该策略是确定的，但需与 jrps/jrpc 的 desired 下发时序对齐验证。
- **宿主注入网络资源属独立后续需求**：本规格曾把"宿主注入 listener"当作核心难点（含托管标记 `CoreOwned`）。实现阶段确认该模型与 FR-25 的装配方式（Core 按快照自建访客入口、控制监听器经 `New(WithListener)` 注入且跨代复用）不兼容，落地它需要改造 FR-25 的装配面并同步 jrps/jrpc 适配器。本版据此收窄为「Core 自建入口的复用与交接」（见 §3.4），宿主注入与托管语义拆为独立需求，与外壳装配改造一并评估。NFR 侧无遗留：Core 当前不接受任何宿主注入的 `net.Listener`，因此不存在"半生效"的所有权承诺。
- **UDP 入口的会话参数热更**：当前要求参数变化时一并更换端口。根治需要在不重绑的前提下迁移套接字与会话，或让入口参数可在线更新；两者都会给 UDP 入口增加一套状态机，是否值得取决于实际运维需求。
- **drain 与 Apply 的 `ctx` 解耦**：本规格规定 drain 不受 Apply `ctx` 约束。若宿主要求「Apply 返回即代表全部完成」，需改为同步等待 drain，但那会让 Apply 时长不可控；该取舍需在实现前明确。
