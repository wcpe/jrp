# ADR-0006：采用 React 管理台并嵌入 jrps

## 状态

已接受

## 背景

JRP 需要单管理员 Web 管理，但不希望引入独立 Web 部署单元或让前端依赖进入 Core。开发阶段仍需可测试的 API 模拟与共享 UI 约束。

## 决策

管理台采用 React、TypeScript、Vite、Ant Design、TanStack Router、TanStack Query 和 MSW。前端作为独立 pnpm app 开发，生产构建产物嵌入 jrps；共享包限定为 `ui`、`devmock`、`tsconfig`、`eslint-config`。

## 理由

- 独立前端应用保持开发、类型检查和测试工具链清晰。
- 嵌入 jrps 简化单机部署和版本配套。
- TanStack Router/Query 提供明确路由与服务端状态边界。
- MSW 只服务开发和测试，避免把模拟逻辑带入生产。

## 后果

- Core 和 jrpc 不包含前端资源或前端依赖。
- `packages/*` 不得反向依赖 `apps/web`。
- Web 只通过 jrps API 操作状态，不直连 SQLite。
- jrps 与 Web 作为同一产品版本发布，避免 API/静态资源版本错配。
- P1 只实现已交付能力页面，不创建 P2/P3 空页面。

## 备选方案

- **独立部署 Web 服务**：增加部署与版本协调成本，P1 不采用。
- **服务端模板页面**：难以满足计划中的交互和状态管理，拒绝。
- **将 Web 资源放入 Core**：破坏 Core 边界，拒绝。
