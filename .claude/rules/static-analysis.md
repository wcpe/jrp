# 代码风格与静态检查

## 1. Go 稳态门禁

适用于 Core、jrps、jrpc，当前由根 Taskfile 统一执行；前端包由根 pnpm + Turbo 编排（ADR-0013）：

- `gofmt` 负责格式化，`task fmt` 写入格式，`task fmt:check` 与 `task lint:go` 检查格式。
- `go vet ./...` 分别检查三个 Go module。
- `scripts/check-dependencies.mjs` 检查 Core 的 `go.mod`、`go list -m all` 和 `go list -deps`，并检查 jrps 与 jrpc 的导入方向。
- `task test:core`、`task test:jrps`、`task test:jrpc` 执行模块测试，`task test:go` 汇总执行。

禁止在代码中零散关闭检查。确需豁免时应集中说明中文原因，并将范围缩到最小。

## 2. Web 稳态门禁

适用于 React/TypeScript workspace：

- Prettier 负责格式化与格式检查。
- ESLint flat config 负责语法、React、Hooks、导入和无用代码检查。
- 项目 `typecheck` 任务使用 `tsc --noEmit` 执行类型门禁。
- Vitest、React Testing Library 与 MSW 覆盖当前组件和 API 状态测试。

生产代码禁止 `console.log`；MSW 仅允许进入开发和测试依赖图。

## 3. 依赖与架构门

CI 和本地 `task lint:go` 必须检查：

- Core 的 `go.mod`、模块图与包依赖图不包含 frp、Gin、GORM、SQLite 或任何 apps module。
- jrps 与 jrpc 不互相导入。
- `packages/*` 不反向依赖 `apps/web` 对应的 `@jrp/web` package。
- Makefile 只转发 Task，不复制命令实现。
- 根 `VERSION` 是 jrps 与 jrpc 构建版本的唯一真源；Core module 版本真源为 `core/VERSION`（FR-28 建立前的过渡期由根 `VERSION` 表达，ADR-0011）。

## 4. 强制门禁

当前合并门禁包含已经落地的事实：

- Go：`gofmt`、`go vet`、依赖边界检查和三个 module 的测试。
- Go 增强：`goimports`、`golangci-lint`、`govulncheck`（见 §5 的版本约束）。
- Core 竞态检测：`go test -race`。
- 代码扫描：CodeQL（Go，见 §5 的构建模式约束）。
- Web：Prettier、ESLint、TypeScript 类型检查和 Vitest。
- 构建：Windows、Linux、macOS 的 fallback Go 构建，以及 Ubuntu 的完整 Web 与生产二进制构建。
- 本地和 CI 调用同一 Task 入口；CI 不维护第二套命令逻辑。

`lint:enhanced` 汇总 goimports、golangci-lint 与 govulncheck，与 `lint:go` 分开：这三个工具都需先安装，而 `lint:go` 位于三平台矩阵与生产构建两个 job 内，并入会让每个 job 重复安装。

格式、静态检查、类型检查、依赖门、测试或构建失败时不得合并。任何禁用规则或基线调整必须有明确理由和可追溯变更。

## 5. 工具版本与已知约束

工具版本固定在 `Taskfile.yml` 的 `tools:install` 内，升级时同步更新该处与本节的记录。固定版本而非跟随 latest：让门禁结果不依赖安装时间，并避免新版工具要求更高的 Go 工具链而迫使 CI 额外下载。

| 工具 | 版本 | 约束 |
| --- | --- | --- |
| `golangci-lint` | v2.12.2 | 配置见根 `.golangci.yml`，按 module 分别运行 |
| `goimports` | `golang.org/x/tools` v0.49.0 | 经 `scripts/lint-imports.mjs` 执行 |
| `govulncheck` | `golang.org/x/vuln` v1.7.0 | 按 CI 的 Go 版本选择 |
| CodeQL | action v4 | Go 不支持 `build-mode: none` |

**govulncheck 的版本与工具链耦合**：v1.8.0 要求 Go ≥ 1.26，而 CI 使用 1.25.x，届时会被迫下载 go1.26 工具链。故选 v1.7.0。

**govulncheck 的标准库漏洞判定依赖当前工具链**：补丁版本落后会报出与代码无关的告警。CI 的 `static-analysis` 与 `codeql` job 因此把 `go-version` 显式写到 `1.25.13`，其余 job 沿用 `1.25.x`。升级 Go 版本时须重新扫描确认。

**CodeQL 的 Go 构建模式**：Go 只支持 `autobuild` 或 `manual`，不支持 `none`。本仓库是 `go.work` 多模块工作区，CodeQL 对 Go workspace 的支持仅见于 extractor 源码，官方文档未给出保证，且 autobuild 在依赖解析失败时会尝试改写 `go.mod`/`go.sum`——故用 `build-mode: manual` 并显式执行 `go build ./core/... ./apps/jrps/... ./apps/jrpc/...`。仓库根不是 module，`go build ./...` 会直接失败。

**依赖漏洞的处置**：`govulncheck` 报出的可调用漏洞须修复（升级依赖或收紧 Go 补丁版本），不得靠豁免关闭。标准库漏洞优先用升级 Go 补丁版解决，第三方漏洞优先升级直接依赖。

## 6. 收紧路径与当前豁免

`.golangci.yml` 以宽松规则集起步，启用 `errcheck`、`govet`、`ineffassign`、`staticcheck`、`unused`。当前豁免及其理由（均集中写在该配置内，代码中不得零散添加忽略指令）：

- 测试文件的 `errcheck`：`defer conn.Close()` 是 Go 惯用写法，该错误无可断言之处；生产代码不豁免。
- `build.go` 的 `withCopiedTargets` 判为 unused：属误报，`unused` 未识别泛型类型约束的方法满足关系。已用行为用例验证别名防护生效，宿主改动切片不会影响已构建的配置值。
- `client/engine.go` 的 `log`/`logger` 判为 unused：客户端侧日志事件尚未落地，而 `docs/specs/core-engine-facade.md` 已约定 `WithLogger` 注入点，属规格未落地而非可删代码。公共面保留，代码内已标注待办；接入日志事件后移除该豁免。

尚未纳入的增强项：`pnpm audit`、浏览器级测试，以及 `errcheck` 在测试外的进一步收紧。引入时仍按「单独批准、固定版本、补齐跨平台验证、接入根 Taskfile 后再更新本节」处理。
