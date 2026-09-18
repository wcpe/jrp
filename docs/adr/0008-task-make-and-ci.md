# ADR-0008：以 Task 统一命令并由 Make 和 CI 调用

## 状态

已被 [ADR-0013](0013-pnpm-turbo-orchestration.md) 收窄为 Go 相关任务的编排决策；本文其余内容对 Go 任务继续有效

## 背景

JRP 同时包含多个 Go module 和 pnpm workspace，需要在 Windows、Linux、macOS 上获得一致的格式化、测试和构建入口。若 Taskfile、Makefile 与 CI 各自维护逻辑，会快速漂移。

## 决策

根 Taskfile 是跨平台命令与依赖关系的唯一真源；Makefile 只转发同名目标；GitHub Actions 调用 Task 任务并在三平台验证 Core、jrps、jrpc，在主前端环境验证 Web 与生产嵌入构建。

## 理由

- Task 提供跨平台、可组合的命令描述。
- Make 保留常见入口但不产生第二套实现。
- CI 与本地调用同一任务，降低环境漂移。
- 多 module 分项测试可快速定位边界问题。

## 后果

- Taskfile 提供 bootstrap、workspace sync、格式化、lint、类型检查、分组件测试、总测试、分组件构建、总构建、开发启动和清理任务。
- Makefile 不写独立 shell 逻辑，只转发给 `task`。
- CI 覆盖 Windows、Linux、macOS 的三个 Go module 与两个二进制构建。
- Core 依赖门检查禁止 frp、Gin、GORM、SQLite 和 apps module。
- 工具版本固定；静态检查与测试失败均阻止合并。

## 备选方案

- **只用 Make**：Windows 原生体验和跨平台 shell 语义不稳定，拒绝。
- **CI 内复制全部命令**：与本地流程形成双真源，拒绝。
- **每个 module 自定义入口**：缺少统一产品验证，拒绝。
