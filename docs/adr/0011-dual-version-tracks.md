# ADR-0011：产品版本与 Core module 版本双轨

## 状态

已接受

## 背景

JRP 起初只有产品形态：`jrps` 与 `jrpc` 两个二进制，版本由根 `VERSION` 注入，ADR-0002 据此把根 `VERSION` 定为产品唯一版本真源。FR-25 至 FR-28 确立 Core 可被第三方 Go 应用作为库消费后，出现了第二个版本消费方：第三方应用依赖的是 Core module 的公共 API 兼容性，而不是 `jrps`/`jrpc` 二进制的版本号。

两者的变更节奏不同：产品版本随 Web、SQLite、通知、CLI 等外壳能力推进；Core 版本只随公共 API 兼容性推进。若强行共用根 `VERSION`，会出现两种失真——Core 公共 API 未变却因外壳改动而升版，或 Core 发生破坏性变更却因外壳未发版而无版本可表达。

ADR-0002 后果节中「根 `VERSION` 是产品唯一版本真源」的表述本身仍然成立，但需要明确它只约束产品版本，不排斥 Core 作为可消费 module 拥有独立 SemVer。

## 决策

采用双轨版本，两条轨道各自只有一个文本真源：

| 轨道 | 真源 | tag 形式 | 覆盖对象 | 消费方 |
|---|---|---|---|---|
| 产品版本 | 根 `VERSION` | `vX.Y.Z` | `jrps` 与 `jrpc` 二进制、Web 产物、产品变更记录 | 部署者、管理员 |
| Core module 版本 | `core/VERSION` | `core/vX.Y.Z` | Core 公共 API 与消费兼容性 | 第三方 Go 开发者、Core 维护者 |

- Core 使用独立 SemVer。首个真实垂直切片发布 `core/v0`；全部 P1 能力与外部嵌入验收稳定后才进入 `core/v1`。
- v1 之前不承诺底层 wire、session、传输内部类型的稳定性；稳定面与不稳定面在 `docs/specs/core-version-policy.md` 中列举，随 Core 版本演进更新。
- 产品版本与 Core 版本不同步是合法状态，不得为了"对齐"而人为抬升任一版本号。

## 理由

- Go 要求子目录 module 的 tag 带子目录前缀，`core/vX.Y.Z` 是工具链原生支持的形式，不需要额外发布机制。
- 版本真源仍是文件而非硬编码常量，与既有构建注入方式一致，不引入第二套机制。
- 把 SemVer 承诺限定在 Core 公共 API，使"版本号变化"对第三方开发者有明确含义；产品版本继续按产品节奏发布，不受库兼容性约束拖累。
- v0 阶段不承诺底层稳定性，避免过早为 wire 与 session 内部结构背上长期兼容负担。

## 后果

- 新增 `core/VERSION` 作为 Core 版本唯一文本真源；长期文档不得出现第二处硬编码的 Core 版本号。
- Core 提供版本查询入口（如 `core.Version()`），返回 SemVer 与 commit；版本信息不得来自环境变量或配置文件。
- `docs/ARCHITECTURE.md`、`.claude/rules/architecture-invariants.md`、`.claude/rules/static-analysis.md` 与 `docs/CONTRIBUTING.md` 中的"唯一版本真源"表述须限定为产品版本，并补述 Core 独立版本轨道。
- 发版流程需分别处理两条轨道：产品发版打 `vX.Y.Z`，Core 发版打 `core/vX.Y.Z`；重大变更须同时更新迁移说明。
- Core 的公共 API 兼容政策成为长期约束，破坏性变更只能随 major 版本发布。

## 备选方案

- **Core 跟随根 `VERSION` 统一发布**：最简单，但版本号无法表达 Core 公共 API 的兼容性，第三方消费方无法据此判断升级风险，拒绝。
- **只定政策不做 tag 发布**：省去发布流程，但第三方无法通过 `go get` 锁定版本，与 FR-28"外部模块消费验证"的验收要求冲突，拒绝。
- **为每个 Core 子包单独发版**：版本数量爆炸且子包间兼容性难以表达，超出当前需求，拒绝。
