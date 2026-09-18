# ADR-0013：以 pnpm + Turbo 编排前端包并保留 Taskfile 编排 Go

## 状态

已接受

## 背景

JRP 的代码库由两类工作区组成：pnpm workspace 承载 `apps/web` 与 `packages/*`，`go.work` 承载 `core`、`apps/jrps`、`apps/jrpc`。ADR-0008 为此确立了「根 Taskfile 是命令唯一真源、Makefile 只转发」的编排方式。

随着前端包数量增加，ADR-0008 的方式暴露出两个具体问题：

1. **重复声明同类依赖导致版本漂移**：`react`、`typescript`、`eslint`、`@types/react` 等依赖在 `apps/web`、`packages/ui`、`packages/devmock` 中各自声明版本号。升级时需要逐包修改，漏改即产生同一依赖的多版本共存。
2. **任务依赖靠手工维护**：根的 lint、typecheck、test 脚本用 `pnpm --filter` 逐一列举包名。新增包必须记得同时改根脚本，否则该包不进入检查范围。

同时，前端包之间已存在真实的构建拓扑（`packages/ui`、`packages/devmock` 被 `apps/web` 消费），需要按依赖顺序执行并复用未变化的结果。

Go 侧的情况不同：`go.work` 与 Go 工具链已提供跨 module 的依赖解析与内容寻址构建缓存，`go test` 的缓存命中粒度比任务级缓存更细。为 Go 任务再套一层任务编排没有收益，反而形成两套缓存语义。

## 决策

按语言分层编排，两种工具各自负责擅长的部分：

| 层 | 编排工具 | 覆盖范围 | 职责 |
|---|---|---|---|
| JS/TS | 根 pnpm + Turbo | `apps/web`、`packages/*` | 构建拓扑编排、任务缓存、依赖版本统一 |
| Go | Taskfile | `core`、`apps/jrps`、`apps/jrpc` | module 测试、静态检查、二进制构建 |
| 转发 | Makefile | 全部 | 只把同名目标转发给 `task` |

### 1. Turbo 作为前端任务编排入口

- 根 `turbo.json` 定义 `build`、`typecheck`、`lint`、`test`、`dev` 任务与依赖关系。其中 `typecheck`、`lint`、`test` 均声明 `dependsOn: ["^build"]`，保证被依赖包的产物先就绪。
- 根的 `package.json` 脚本改为 `turbo run <task>`，不再逐个列举 `--filter` 包名。新增包只要在 workspace 内并声明同名脚本即自动进入管道。
- Turbo 的本地任务缓存对未变化输入复用结果。

### 2. pnpm catalog 统一依赖版本

- `pnpm-workspace.yaml` 的 `catalog` 段落是前端第三方依赖版本的唯一真源。
- 各包以 `"catalog:"` 引用，不在包内写死版本号。
- `workspace:*` 用于仓库内包互引，不受 catalog 影响。
- `peerDependencies` 表达兼容范围而非具体版本，保持普通范围写法，不使用 `catalog:`。

### 3. Go 侧编排保持 ADR-0008 的形态

- Taskfile 仍是 Go 相关任务的唯一真源，Makefile 继续只做转发。
- Go module 不纳入 Turbo 管道，不创建与 Go 工具链重复的缓存层。
- 根 `go.work` 与各 `go.mod` 的职责不变。

### 4. 边界约束不变

本决策不改变任何既有的架构边界：Core 仍不依赖外壳、数据库、Web 或通知；jrps 与 jrpc 仍互不导入；Web 生产构建产物仍只嵌入 jrps。Turbo 与 catalog 只影响前端包如何被编排和声明依赖，不影响它们的依赖方向。

依赖方向检查脚本需扩展为同时校验前端包边界，避免新增包绕过约束。

## 理由

- **按语言分层比强行统一更贴合实际**：Go 与 JS 的包管理、缓存与依赖解析机制不同，用一套工具覆盖两者必然在其中一侧产生冗余。
- **Turbo 解决的是真实痛点**：依赖版本漂移与手工维护 filter 列表都会随包数量增长而放大，catalog 与自动拓扑排序分别消除了这两类问题。
- **不给 Go 套任务缓存**：Go 的构建缓存粒度细于任务级缓存，叠加编排只增加一层可能误报的失效判断。
- **改动面可控**：前端编排集中在一个 `turbo.json`、一个 catalog 段落与根脚本，不触碰 Go module 结构与既有 ADR 确立的依赖方向。

## 后果

- 前端任务的推荐入口变为根 `pnpm build`、`pnpm lint`、`pnpm test`、`pnpm typecheck`（内部走 Turbo）。
- 新增前端包不再需要修改根脚本；只要包在 workspace 内并有同名脚本即可进入管道。
- 前端第三方依赖升级只需改 `pnpm-workspace.yaml` 的 catalog 一处。
- `turbo.json` 与 `pnpm-workspace.yaml` 成为前端编排的受约束文件，修改需同步本决策。
- ADR-0008 中「根 Taskfile 是跨平台命令与依赖关系的唯一真源」的表述被本决策收窄为「Go 相关任务的真源」；ADR 正文不改写，其状态标注为被本决策取代。
- CI 中前端 Job 与 Go Job 的职责不变，前端步骤的脚本名称保持兼容。
- Core 依赖门检查需同时覆盖前端包边界，避免新增包绕过既有限制。

## 备选方案

- **保持纯 pnpm 手工编排**：包数量继续增长后，依赖版本漂移与 filter 列表漏项会持续发生，且无任务缓存，拒绝。
- **Turbo 同时编排 Go 任务**：需要在 Turbo 中表达 Go module 依赖并调用外部命令，与 Go 工具链自带的构建缓存形成两层语义；任务级缓存的失效判断也不如 Go 内容寻址精确，拒绝。
- **用 Taskfile 编排前端任务**：前端包之间的构建拓扑与时序依赖无法由 Taskfile 直接表达，需手写脚本模拟，等于重复实现 Turbo 的能力，拒绝。
- **迁移到 Nx 等其他编排器**：能力重叠且需要引入新的插件生态，当前规模不需要，拒绝。
