# ADR-0004：以 SQLite 作为配置持久化真源

## 状态

已接受

## 背景

JRP 需要为个人和小团队提供版本化配置、审计和断电恢复，同时避免 PostgreSQL、Redis、消息队列等外部基础设施。服务端和客户端都需要本地持久化，但 Core 不能在数据热路径访问数据库。

## 决策

jrps 与 jrpc 分别使用 SQLite 作为本地配置和 revision 的持久化真源；管理适配层使用 Gin、GORM 与纯 Go SQLite 驱动。适配器把 desired revision 转换为 Core 不可变 active snapshot，Core 不依赖任何数据库组件。

## 理由

- SQLite 满足单机、低运维和事务需求。
- 独立数据库避免 jrps/jrpc 共享文件或网络数据库形成隐式耦合。
- desired、active、last-good revision 可明确表达持久状态与运行状态的差异。
- 纯 Go 驱动有利于 Windows、Linux、macOS 交叉构建。

## 后果

- SQLite desired 不得冒充 Core active，Core 内存状态也不得反向成为持久化真源。
- 配置、凭证和必要元数据按已接受风险明文静态存储；不得声称已加密。
- API、日志、UI 和审计必须脱敏，数据目录和备份是信任边界。
- 通知采用事务 outbox，事务提交后再执行外部副作用。
- 关闭正文采集时 Core 数据热路径不得访问 SQLite。
- P1 不引入 PostgreSQL、Redis、共享数据库或消息队列。

## 备选方案

- **配置文件作为唯一真源**：无法满足集中版本、审计与 jrpc 下发，拒绝。
- **PostgreSQL**：对目标部署过重，留待 P3 需求成立后评估。
- **内存状态为真源**：重启丢失且难以审计，拒绝。
