# ADR-0002：采用多 Go module 与独立 Core 边界

## 状态

已接受

## 背景

协议、传输、控制会话、工作连接、代理、访客和 NAT 属于可复用数据面能力；SQLite、Web、通知、CLI 和进程装配属于产品外壳。若所有能力位于一个 module，Core 容易反向依赖控制面，未来 P3 数据节点也难以独立组合。

## 决策

建立三个独立 Go module：`github.com/wcpe/jrp/core`、`github.com/wcpe/jrp/apps/jrps`、`github.com/wcpe/jrp/apps/jrpc`，由根 `go.work` 编排。jrps 与 jrpc 单向依赖 Core，彼此不导入。

## 理由

- module 边界能用 Go 工具链直接验证依赖方向。
- Core 可在不携带 Gin、GORM、SQLite、Web 或通知的情况下测试和复用。
- jrps 与 jrpc 可以按各自运行环境装配适配器，不污染协议层。
- P3 数据节点可以创建新的外壳 module 并组合 Core，而无需拆解现有服务端。

## 后果

- Core 禁止依赖 `apps/*`、Gin、GORM、SQLite、Web、通知和 CLI。
- 管理 DTO、数据库模型和 Web 类型不得进入 Core。
- 根 `VERSION` 是产品唯一版本真源，三个 module 在 P1 不维护独立产品版本；Core 作为可消费 module 另有独立 SemVer，见 ADR-0011。
- 跨 module 改动必须分别运行 Core、jrps、jrpc 测试和依赖门检查。
- P1 不创建 `apps/node` 或空 Engine/RPC 接口。

## 备选方案

- **单一 Go module**：初期更简单，但无法形成可靠的 Core 边界，拒绝。
- **将 Core 作为 jrps 内部包**：阻碍 jrpc 和未来数据节点复用，拒绝。
- **立即拆成微服务**：超出单机产品需求并引入运维复杂度，拒绝。
