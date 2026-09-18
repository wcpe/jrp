# 功能规格：Core 独立 SemVer、外部消费验证与 API 兼容政策

> 状态：草拟 · 关联 PRD：FR-28 · 分支：feature/core-version-policy

## 1. 背景与目标

Core 要被第三方 Go 应用当作库消费，就必须有**自己的**版本与兼容承诺；而 jrps/jrpc 产品的版本由根 `VERSION` 表达。ADR-0011 已确立双轨分工：根 `VERSION` 是产品版本唯一真源，`core/VERSION` 是 Core module 版本唯一真源，tag 形式为 `core/vX.Y.Z`。本功能据此把版本真源、兼容政策与外部消费验证落地。

本功能解决的问题：

- 没有独立 module tag，第三方无法用 Go 工具链固定 Core 版本，`replace` 到仓库相对路径的做法在外部不可复现。
- 没有兼容政策，就无法回答「哪些 API 可以改、哪些不能」，也无法给出迁移说明。
- 过早承诺 wire/session 等底层 API 稳定，会把尚未验证的内部形态冻结，阻碍后续修正。

目标：为 Core 建立可验证的独立版本机制、`GOWORK=off` 外部消费验证、以及分层的 API 兼容政策，并明确它与产品版本的职责分工。

使用者：第三方 Go 应用开发者、Core 维护者、发版执行者。所属阶段：P1。

## 2. 需求

- Core 有独立 SemVer 版本，与根 `VERSION` 双轨并存。
- 版本 tag 采用 Go module 要求的子目录 tag 形式，第三方可用标准 `go get` 消费。
- 提供机器可读的版本入口，便于宿主在运行时与诊断信息中读到 Core 版本。
- 提供外部消费验证：一个独立于仓库 `go.work` 的测试 module，在 `GOWORK=off` 下只依赖 Core 公共包即可编译并跑通 FR-25 的垂直切片。
- 制定并文档化 API 兼容政策，含稳定面与不稳定面的划分，以及 v0 与 v1 的不同承诺强度。
- 任何破坏性变更必须提供迁移说明。

- **范围内**：
  - `core/VERSION` 作为 Core 版本真源，tag 格式 `core/vX.Y.Z`。
  - Core 版本常量的公开入口与构建期注入方式。
  - v0 政策：允许在 minor 或 patch 做破坏性变更，但必须写 CHANGELOG 与迁移说明。
  - v1 政策：SemVer 严格生效，破坏性变更只能进 major。
  - 稳定面与不稳定面的清单，以及不稳定面的判定标准。
  - `GOWORK=off` 外部 fixture 验证，包含空 module 缓存外的可复现性检查。
  - 与 `.claude/rules`、`CONTRIBUTING`、相关 ADR 表述的一致性修订。
- **范围外**：
  - 不自动创建或推送任何 git tag；实际发版与打 tag 需要用户明确授权。
  - 不改变根 `VERSION` 的地位：jrps 与 jrpc 二进制版本仍来自根 `VERSION`。
  - 不做自动化发布流水线、制品签名、SBOM 或代理仓库同步。
  - 不做 SemVer prerelease 之外的版本分支策略讨论（例如长期维护分支）。
  - 不定义 `platform/service` module 的版本策略：该 module 属于 FR-29，其版本方案由对应规格处理。
  - 不做 Web 前端包版本管理，不涉及 `apps/web` 与 `packages/*` 的 npm 版本策略。
  - 不定义 P3 数据节点的版本兼容矩阵（ADR-0009）。

## 3. 设计

### 3.1 双轨版本分工

| 版本 | 真源文件 | tag 形式 | 管什么 | 谁关心 |
|---|---|---|---|---|
| Core 版本 | `core/VERSION` | `core/vX.Y.Z` | Core module 的公共 API 与消费兼容 | 第三方 Go 开发者、Core 维护者 |
| 产品版本 | 根 `VERSION` | `vX.Y.Z` | jrps 与 jrpc 二进制、Web 产物、产品变更记录 | 部署者、管理员 |

- 两者独立递增，不允许引用对方或试图保持同步；一次发版可以只升其中一个。
- Core 版本变更由 Core 公共面变化驱动；产品版本变更由用户可见的产品行为变化驱动。
- jrps/jrpc 的 `go.mod` 首个 Core tag 发布后依赖具体 Core 版本而非伪版本，并删除相对路径 `replace`；仓库内本地开发由根 `go.work` 覆盖，这不是阻挡 Go 工具链验证的外部手段。
- 双轨分工由 ADR-0011 确立：根 `VERSION` 是产品版本唯一真源，`core/VERSION` 是 Core module 版本唯一真源。已接受 ADR 正文不可修改，因此 ADR-0002 后果节的处理方式为补述引用 ADR-0011，而非改写其正文。

### 3.2 版本常量与运行时可读

- `core/VERSION` 文件为唯一文本真源；构建脚本把它注入到 Core 的版本常量（与 jrps/jrpc 注入产品版本的方式保持一致）。
- 公共包提供读取入口，返回不可变值：

```go
info := core.Version()
// info.Module、info.SemVer、info.Commit
```

- 版本信息读取不得依赖环境变量或配置文件；未注入构建信息时有确定默认值（例如 SemVer 取自 VERSION 文件，commit 为空字符串）。
- 第三方在诊断信息中输出 Core 版本时，Core 不强制格式；提供宿主可读的字符串表达即可。

### 3.3 tag 与消费方式

- Core 是仓库子目录 module `github.com/wcpe/jrp/core`，Go 要求其子目录 module 的 tag 带子目录前缀，形式为 `core/vX.Y.Z`（例如次版本号不同的两个 tag 分别为 `core/v0.1.0` 与 `core/v0.2.0`；具体取值由发版流程从 `core/VERSION` 派生）。
- 第三方消费：

```text
go get github.com/wcpe/jrp/core@core/vX.Y.Z
```

- Core 版本号 SemVer 语义：
  - MAJOR：不兼容的公共 API 变更。
  - MINOR：向后兼容的能力新增。
  - PATCH：向后兼容的问题修复。
- v0 阶段不遵循「minor 必须向后兼容」：见 §3.4。
- 首个真实垂直切片完成并通过全部验收后发布首个 v0 tag（对应 PRD NFR「版本兼容」：首个真实垂直切片发布 v0，全部 P1 与外部嵌入验收稳定后才进入 v1）。

### 3.4 兼容政策

**稳定面（v1 起严格保护，v0 期间尽力保护）**：

- Engine 生命周期：`New`、`Start`、`Shutdown`、`Done`、`Err`。
- 资源注入选项与所有权转移规则。
- Apply 入口与 `Deployment`/`Result`/`Stage` 语义。
- 修订三态的只读查询与语义。
- 事件订阅 `Subscribe`/`Events`/`Close` 与 `State` 只读快照。
- 配置构建器与校验 API、`ConfigError` 错误码集合。
- 错误哨兵：`ErrAlreadyStarted`、`ErrNotStarted`、`ErrStopped`、`ErrApplyInProgress`、`ErrConfigInvalid`。

**不稳定面（v1 发布前不做兼容承诺，可随 minor 变更）**：

- wire v1/v2 的帧、消息、编解码结构体、golden frame 位置与内部常量。
- 控制会话与心跳的状态机内部形态、时间参数默认值。
- 工作连接的建立、绑定与池化内部结构。
- 代理的内部路由实现、转发缓冲与变换链。
- NAT 打洞候选交换内部消息与策略表。
- 传输层内部结构（即使传输本身作为取值在稳定面被引用，其包结构不算）。
- 内部包 `core/internal/**` 全部内容：任何路径都不构成公共 API。

不稳定面的判定补充：

- 出现在公共包中但属于上述条目的符号，需在文档与包注释中显式标注「v1 前不稳定」，避免宿主误用。
- 宿主若确实需要 wire 层能力，应在 Core 之外自行实现；Core 不为第三方提供 wire 扩展点。首版不开放插件 SPI。

**跨版本承诺强度**：

| 阶段 | 稳定面 | 不稳定面 | 变更时的义务 |
|---|---|---|---|
| v0 | 尽力兼容 | 无承诺 | 任何破坏性变更都需要 CHANGELOG 条目与迁移说明 |
| v1 起 | 严格 SemVer | 仍无承诺 | 稳定面破坏只能进 major；minor 新增向后兼容能力 |

### 3.5 迁移说明

- 每次 Core 版本变化必须在 CHANGELOG 中单独成段，注明受影响的 API 与替换方式。
- 破坏性迁移说明必须包含：变更前示例代码、变更后示例代码、以及宿主需要同步调整的点。
- v1 发布说明必须列出「稳定面最终清单」与「从 v0 最新版升级到 v1 的迁移步骤」。
- 迁移说明属于入库文档，不得只放在临时目录或提交信息里。

### 3.6 外部消费验证

```text
.tmp/external-core-consumer/     # 未入库的临时验证目录
├── go.mod                        # 模块路径为外部独立路径
│                                 # require github.com/wcpe/jrp/core <版本>
└── main_test.go                  # 只导入 core、core/server、core/client
```

验证步骤：

1. 在临时目录创建独立 Go module，其 `go.mod` 不含 `replace`，直接依赖被验证的 Core 版本（首个 tag 发布前可用本地绝对路径 replace 作为临时替代，并在报告中说明该替换仅是发行前过渡）。
2. 设置 `GOWORK=off` 运行 `go mod download`、`go build ./...`、`go test ./...`，确保不读取仓库根 `go.work`。
3. 外部 fixture 完成 FR-25 的垂直切片闭环：ServerEngine + ClientEngine、TCP 传输、wire v1、一个 TCP 代理的端到端数据流。
4. 依赖图断言：`go list -deps` 的输出中不含 Gin、GORM、SQLite、`apps/*`、`github.com/fatedier/frp`，也不含 `core/internal` 路径。
5. 在干净的 module cache 场景下重复一次下载与编译，确认无可用的本地 workspace 隐含依赖。
6. Windows、Linux、macOS 三平台各跑一次，确保 Core 无平台专属隐含依赖。

临时验证目录与产物只放 `.tmp/`，不入库；CI 中如需常态化，应在先有 ADR 与 Task 入口后接入，不得临时插入。

## 4. 任务拆分

- [ ] 先写失败测试：在 `.tmp/` 建立外部消费 fixture 并使其首次运行为红——断言它在 `GOWORK=off` 下能完成 FR-25 垂直切片闭环、`go list -deps` 无禁用依赖、且能在干净 module cache 下编译；同时编写 Core 版本常量的失败测试（初始为红）
- [ ] 在 ADR-0002 后果节与 ARCHITECTURE、CONTRIBUTING、architecture-invariants 中补述 ADR-0011 的双轨分工（不改写已接受 ADR 正文），消除版本真源歧义
- [ ] 新增 `core/VERSION` 并改为 Core 版本唯一文本真源，实现版本常量与构建期注入
- [ ] 撰写 `docs/CORE_API.md`：公共面清单、生命周期、资源所有权、Apply、事件、错误模型与版本政策
- [ ] 在代码中标注不稳定面（包注释与导出符号注释），把「v1 前不稳定」写成可执行检索的约定
- [ ] 补齐外部 fixture 为绿：首个 tag 发布前用临时 replace，发布后切换到真实版本
- [ ] 更新 jrps/jrpc 的 `go.mod`：首个 tag 后依赖真实 Core 版本并删除相对 replace，本地由 go.work 覆盖
- [ ] 同步 ARCHITECTURE、CONTRIBUTING、architecture-invariants、CHANGELOG 中受影响表述，消除版本真源冲突
- [ ] 运行 `task test:core`、`task lint:go`、依赖门检查与三平台外部消费验证
- [ ] 打 tag 需用户明确授权，不在本任务内自动执行；PRD 中 FR-28 状态在全部验收通过后变更

## 5. 验收标准

正常路径：

- 存在 `core/VERSION` 且为 Core 版本唯一真源；`core.Version()` 返回的 SemVer 与该文件一致，并有明确的 commit 字段。
- 临时目录中的外部 fixture 在 `GOWORK=off` 下只依赖 Core 公共包完成下列动作：配置构建 → 创建 Engine → Start → ClientEngine 与 ServerEngine 通过 TCP + wire v1 + 一个 TCP 代理完成端到端数据流 → Apply → 事件订阅 → Shutdown，全部通过。
- `go list -deps` 在外部 fixture 上的输出不含 Gin、GORM、SQLite、`apps/*`、`github.com/fatedier/frp` 与 `core/internal` 路径。
- `docs/CORE_API.md` 中存在稳定面与不稳定面的完整清单，且不稳定面清单覆盖 §3.4 列出的全部条目。

边界：

- 干净 module cache 场景下外部 fixture 仍能下载、编译并通过测试（首个 tag 发布前使用临时 replace，并在验证报告中显式说明）。
- Windows、Linux、macOS 三平台各通过一次外部消费验证。
- jrps 与 jrpc 仍以根 `VERSION` 作为二进制版本来源，构建产物的版本号与 Core 版本不同步也是合法状态。
- 版本信息读取不依赖环境变量或配置文件；未注入 commit 时返回空字符串，不得 panic。

错误路径：

- 稳定面出现签名变更而版本号未相应提升时，由 API 兼容政策审查环节拦截；门禁至少覆盖对稳定面清单中导出符号的增删与签名变化检查。
- 外部 fixture 若意外导入了 `core/internal` 或 `apps/*`，编译必须失败（Go 自身的 internal 规则与 module 边界共同保证），并由门禁脚本对外部 fixture 的导入列表做显式断言。
- `core/VERSION` 与 `core.Version()` 返回值不一致时测试失败。
- 任何使 Core 依赖图出现 Gin、GORM、SQLite、`apps/*` 或 `github.com/fatedier/frp` 的改动，都会被依赖门检查拦截并有测试或脚本输出为证。
- 迁移说明缺失：任何 Core 版本变更若没有对应 CHANGELOG 条目，文档同步审查不通过。

文档一致性：

- `architecture-invariants.md`、ADR-0002 后果节与 `CONTRIBUTING` 中「唯一版本真源」的表述已修订为产品与 Core 双轨分工，无残留冲突。
- CHANGELOG 中存在独立的 Core 版本段落，列明版本变化与迁移方式。
- Windows、Linux、macOS 三平台 `task test:core` 与 `task lint:go` 通过。

## 6. 风险与待定

- **版本号的单一事实来源**：真源是 `core/VERSION` 文件，tag 形式 `core/vX.Y.Z` 由发版流程从该文件派生；长期文档中不得再出现第二处硬编码的 Core 版本号。
- **双轨分工的约束来源**：ADR-0011 是双轨决策的单一依据。长期文档中凡提及版本真源，必须区分产品版本与 Core module 版本；已接受 ADR 正文不可改写，需要调整时按 ADR-0011 规定的补述方式处理。
- **首个 tag 的授予时机**：首个 tag 应在 FR-25 垂直切片完成并通过外部消费验证后发布，且必须由用户明确授权。若在切片尚未稳定时提前发布，会冻结未经充分验证的接口形态。
- **v0 阶段破坏性变更的频率管理**：v0 允许破坏，但频繁破坏会消耗宿主信任。建议把「v0 破坏次数」作为进入 v1 的观察指标之一，具体阈值需另行确认。
- **v1 进入条件需可判定**：PRD NFR 只给了「全部 P1 与外部嵌入验收稳定后」，缺少具体判定清单。本功能的 follow-up 需要列出可勾选的 v1 进入条件；在明确之前不得宣布 v1。
- **不稳定面标注的可持续维护**：仅靠注释难以长期保持准确。应考虑后续引入 API diff 工具做门禁，但那属于新依赖与新 Task 入口，需要先履行依赖与流程批准，不在本功能擅自引入。
