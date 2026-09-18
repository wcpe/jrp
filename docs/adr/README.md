# 架构决策记录（ADR）

记录 JRP 的重大架构决策。理解系统当前形态先阅读 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)，需要追溯取舍原因时再查对应 ADR。

| 编号 | 决策 | 状态 |
|---|---|---|
| 0001 | [独立实现官方 frpc 兼容能力](0001-independent-frpc-compatibility.md) | 已被 ADR-0010 取代 |
| 0002 | [采用多 Go module 与独立 Core 边界](0002-multi-module-core-boundary.md) | 已接受 |
| 0003 | [分离控制面、数据面、可执行外壳与 Web](0003-control-data-and-web-boundaries.md) | 已接受 |
| 0004 | [以 SQLite 作为配置持久化真源](0004-sqlite-configuration-source.md) | 已接受 |
| 0005 | [采用无中断配置热更状态机](0005-zero-interruption-reload.md) | 已接受 |
| 0006 | [采用 React 管理台并嵌入 jrps](0006-react-embedded-web.md) | 已接受 |
| 0007 | [分离 HTTP 采集元数据与正文存储](0007-http-capture-storage.md) | 已接受 |
| 0008 | [以 Task 统一命令并由 Make 和 CI 调用](0008-task-make-and-ci.md) | 已被 ADR-0013 收窄为 Go 任务编排 |
| 0009 | [P3 采用中心控制面与数据节点方向](0009-central-control-future-nodes.md) | 已接受 |
| 0010 | [采用可移植的独立兼容基线](0010-portable-compatibility-baseline.md) | 已接受 |
| 0011 | [产品版本与 Core module 版本双轨](0011-dual-version-tracks.md) | 已接受 |
| 0012 | [Core 公共包布局与嵌入契约](0012-core-public-api-layout.md) | 已接受 |
| 0013 | [以 pnpm + Turbo 编排前端包并保留 Taskfile 编排 Go](0013-pnpm-turbo-orchestration.md) | 已接受 |

每条 ADR 使用“状态、背景、决策、理由、后果、备选方案”结构。已接受 ADR 的正文不可修改；决策变化时新增 ADR 取代旧决策，旧文件只更新状态与取代链接。编号永久递增，不删除、不复用。
