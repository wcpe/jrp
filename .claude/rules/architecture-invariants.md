# 架构不变量

> 以下约束来自 `docs/ARCHITECTURE.md` 和已接受 ADR。违反任一条即为架构漂移；确需改变时先新增 ADR 取代原决策，再同步规则与架构文档。

## 1. 独立兼容实现

- 兼容研究以参考源码仓库的固定 Git 提交 `e1f1d6ab1b591ec9b596f4b1b6a81cd2960c6314` 为基线；本机检出位置不得写入项目文档、规则或协议契约。
- 禁止复制、改写、链接、导入 frp 源码或 Go package。
- 禁止复用参考仓库的注释、错误文案、目录结构、NAT 策略表或测试组织。
- Core 依赖图不得出现 `github.com/fatedier/frp`；兼容性以 JRP 自有规格和黑盒证据建立。

## 2. Core 与外壳边界

- `github.com/wcpe/jrp/core` 是独立 Go module，只承载协议、传输、控制会话、工作连接、代理、访客、NAT 和数据变换。
- Core 禁止依赖 Gin、GORM、SQLite、Web、通知、CLI、`apps/*` 或管理 DTO。
- `apps/jrps` 与 `apps/jrpc` 只单向依赖 Core，彼此不得导入。
- React Web 只调用 jrps 管理 API，生产资源只嵌入 jrps，不进入 Core 或 jrpc。
- `packages/*` 只被 `apps/web` 消费，不得反向依赖应用。

## 3. 控制协议边界

- 官方 frpc 与 jrpc 共用兼容数据协议。
- jrpc enrollment、desired state 和回执只走独立 HTTPS/WSS 管理通道。
- 禁止向官方兼容消息中加入 JRP 私有管理命令。
- P1 不定义数据节点协议、节点字段或 RPC。

## 4. 配置与运行真源

- jrps 与 jrpc 各自的 SQLite 是本地 desired configuration 和 revision 的持久化真源，禁止共享数据库文件。
- Core active snapshot 是不可变运行状态，不得冒充持久化真源。
- desired、active、last-good revision 必须分开表达和审计。
- 配置应用固定为 prepare、health-check、atomic publish、drain；发布前失败保留旧 active/last-good。
- 数据目录、SQLite 路径、管理 API 根监听是仅有的引导参数，变更需要重启。

## 5. 数据面隔离

- Core 热路径不得查询 SQLite、调用 Webhook/邮件、等待 Web 或管理事务。
- 关闭正文采集时，数据转发路径不得访问 SQLite 请求记录。
- 运行事件通过有界通道交给适配器；慢消费者不得无限阻塞数据面。
- 通知在业务事务提交后通过 outbox 触发，禁止事务内远程调用。

## 6. 技术与部署约束

- P1 使用 Go 多 module、Gin、GORM、纯 Go SQLite 驱动，以及 React、TypeScript、Vite、Ant Design、TanStack Router/Query、MSW。
- 根 Taskfile 是 Go 相关任务的命令真源；前端包由根 pnpm + Turbo 编排，根 `turbo.json` 与 `pnpm-workspace.yaml` 的 catalog 段落是前端任务与依赖版本的受约束文件；Makefile 只转发，不维护第二套逻辑（ADR-0013）。
- P1 禁止 PostgreSQL、Redis、消息队列、微服务、共享数据库和对象存储。
- 根 `VERSION` 是 jrps、jrpc 和产品的唯一版本真源；Core 作为可消费 module 另有独立 SemVer，真源为 `core/VERSION`（FR-28 建立前的过渡期由根 `VERSION` 表达），tag 形式为 `core/vX.Y.Z`（ADR-0011）。

## 7. 安全与采集约束

- 凭证和 HTTP 正文按已接受风险明文静态存储，不得声称已加密。
- API、日志、UI 和审计必须脱敏；正文查看、导出、删除必须审计。
- HTTP 正文默认关闭，只对明文 HTTP 逐代理开启；P1 HTTPS 透传不采集正文。
- 正文元数据存 SQLite，正文存压缩分段文件，默认 30 天且总量不超过 5 GiB。

## 红线

出现以下任一情况必须停止：导入或复制 frp；Core 引入外壳/数据库/Web/通知依赖；jrps 与 jrpc 互相导入；把私有管理消息塞入兼容协议；混淆 desired 与 active；用重启冒充非引导配置热更；引入被禁重型基础设施；提前实现 P3 节点；声称明文数据已加密；静默违背已接受 ADR。
