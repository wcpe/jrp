# JRP

> 面向个人与小团队、兼容官方 frpc 的高性能反向代理与 P2P 打洞平台，以数据库驱动配置、无中断热更、可观测性和通知能力简化运维。

## 状态

- 产品版本：`0.1.0`
- 初始化日期：2026-07-16
- 仓库：`github.com/wcpe/jrp`
- 阶段：初始化工程骨架；P1 功能按 `docs/PRD.md` 逐项交付
- 许可：MIT

当前版本建立独立 Core、`jrps`、`jrpc` 与 Web 的工程边界。功能是否已交付以 PRD 状态、测试与实际代码为准，不因文档中存在目标契约而视为已经实现。

## 架构一览

Core 是独立 Go module，不属于 `jrps`、`jrpc` 或 Web 外壳。两个可执行程序只能单向依赖 Core；Web 构建产物仅嵌入 `jrps`。

```text
 官方 frpc                         jrpc 管理通道
     │                           HTTPS / WSS
     │ frpc 兼容数据协议               │
     ▼                                ▼
┌──────────────────────────────────────────────┐
│ Core：协议、wire v1/v2、传输、代理、访客、NAT │
│ github.com/wcpe/jrp/core                     │
└───────────────▲──────────────────▲───────────┘
                │                  │
          单向依赖 Core       单向依赖 Core
                │                  │
       ┌────────┴────────┐  ┌──────┴───────┐
       │ jrps 外壳/控制面 │  │ jrpc 客户端外壳 │
       │ API、SQLite、通知 │  │ SQLite、配置应用 │
       └────────▲────────┘  └──────────────┘
                │
       ┌────────┴────────┐
       │ React Web 管理台 │
       │ 生产资源嵌入 jrps │
       └─────────────────┘
```

P3 的目标拓扑是“中心控制面 + 多数据节点”。P1 只保持 Core 可独立组合的边界，不创建空节点、节点 RPC、注册、选举或一致性协议。

## 规划能力

### P1

- 兼容官方 frpc，支持 wire v1/v2。
- 连接传输：TCP、KCP、QUIC、WebSocket、WSS。
- 代理类型：TCP、UDP、HTTP、HTTPS、STCP、XTCP。
- 自研 `jrpc`，支持 enrollment、版本化配置下发与每客户端 token。
- `jrps` 与 `jrpc` 分别以 SQLite 作为配置持久化真源。
- 配置采用 prepare、health-check、atomic publish、drain，实现已有连接不中断的热更。
- 单管理员 Web 管理、日志、监控、Webhook 与邮件通知。
- HTTP 正文按代理选择性采集；元数据进入 SQLite，正文进入压缩分段文件，默认保留 30 天且总量不超过 5 GiB。

### 后续阶段

- P2：SUDP、TCPMUX、剩余参数与插件、QQ 通知、深度分析、HTTPS 终止采集、协议升级兼容。
- P3：中心控制面、多数据节点、调度与故障转移；之后再评估 HA、RBAC、OIDC、外部数据库和对象存储。

## 目录结构

```text
core/             独立 Core Go module
apps/jrps/        服务端、控制面与 Web 嵌入外壳
apps/jrpc/        自研轻量客户端外壳
apps/web/         React 管理台
packages/         Web 共享包
platform/         平台适配 module（FR-29 计划，尚未建立）
docs/             需求、架构、接口、协议、运维与 ADR
.claude/rules/    项目治理与防漂移规则
```

本仓库按语言分层编排：根 `go.work` 编排 `core`、`apps/jrps`、`apps/jrpc` 三个 Go module；根 pnpm workspace 与 Turbo 编排 `apps/web` 与 `packages/*`（ADR-0013）。Core 不依赖 Gin、GORM、SQLite、Web、通知或 CLI；`jrps` 与 `jrpc` 互不导入。

## 文档导航

- 需求：[`docs/PRD.md`](docs/PRD.md)
- 架构：[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- 接口：[`docs/API.md`](docs/API.md)
- 协议：[`docs/PROTOCOL.md`](docs/PROTOCOL.md)
- 运维：[`docs/OPERATIONS.md`](docs/OPERATIONS.md)
- 安全：[`SECURITY.md`](SECURITY.md)
- 决策：[`docs/adr/`](docs/adr/)
- 功能规格：[`docs/specs/`](docs/specs/)
- 演进与维护：[`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md)
- 变更史：[`CHANGELOG.md`](CHANGELOG.md)

## 快速开始

工程骨架完成后，以根 Taskfile 为跨平台命令真源，Makefile 仅转发同名目标：

```bash
task bootstrap
task test
task build
```

预期构建产物为 `jrps` 与 `jrpc` 两个二进制。当前阶段不得把未实现的协议、数据库或分布式能力当作可用功能。

## 兼容性与独立实现

兼容性研究以参考源码仓库的固定 Git 提交 `e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314` 为基线；仓库检出位置属于开发环境，不进入项目契约。JRP 仅通过公开行为、独立规格与黑盒互操作测试建立兼容性，不复制、改写、链接、导入 frp 源码或 Go package。

## 约定

贡献前请阅读 [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) 和 [`.claude/rules/`](.claude/rules/)。提交信息使用中文 Conventional Commits，任何用户可见变更必须同步文档与 CHANGELOG。

## 许可

本项目采用 [MIT License](LICENSE)，版权归 `wcpe` 所有。
