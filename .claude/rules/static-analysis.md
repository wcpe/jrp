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

当前合并门禁仅包含已经落地的事实：

- Go：`gofmt`、`go vet`、依赖边界检查和三个 module 的测试。
- Web：Prettier、ESLint、TypeScript 类型检查和 Vitest。
- 构建：Windows、Linux、macOS 的 fallback Go 构建，以及 Ubuntu 的完整 Web 与生产二进制构建。
- 本地和 CI 调用同一 Task 入口；CI 不维护第二套命令逻辑。

格式、静态检查、类型检查、依赖门、测试或构建失败时不得合并。任何禁用规则或基线调整必须有明确理由和可追溯变更。

## 5. 后续增强

`golangci-lint`、`goimports`、`govulncheck`、`pnpm audit`、竞态检测和浏览器级测试尚未作为当前稳态强制门禁。引入这些工具需要单独批准、固定版本、补齐跨平台验证，并接入根 Taskfile 后再更新本规则。
