# 范围纪律

## 1. P1 允许范围

- 独立 Core module 与 go.work 编排的 Core、jrps、jrpc。
- wire v1/v2。
- TCP、KCP、QUIC、WebSocket、WSS 连接传输。
- TCP、UDP、HTTP、HTTPS、STCP、XTCP 代理。
- 官方 frpc 兼容与自研 jrpc。
- 单管理员、每客户端 token、jrpc enrollment 与独立 HTTPS/WSS 配置下发。
- jrps/jrpc 各自 SQLite 配置真源和 revision。
- prepare、health-check、atomic publish、drain 无中断热更。
- React Web、日志、监控、Webhook、邮件、审计。
- 明文 HTTP 按需采集，SQLite 元数据、压缩分段正文、30 天和 5 GiB。
- Windows、Linux、macOS 测试与 jrps/jrpc 二进制构建。
- Core 可嵌入门面（ServerEngine/ClientEngine）、Apply、事件订阅、配置构建器、Core 独立版本与 API 兼容政策（FR-25 至 FR-28、FR-32）。
- `platform/service` 系统服务的安装、卸载、启停、重启与状态 CLI，含 Linux systemd 与 Windows SCM（FR-29 至 FR-31）。

## 2. P1 禁止范围

### P2 能力

- SUDP、TCPMUX。
- 未列入 P1 的剩余 frp 参数和插件能力。
- QQ 通知、深度请求/流量分析。
- JRP 终止 TLS 后的 HTTPS 正文采集。
- 未经规格确认的协议升级扩展。

### P3 能力

- `apps/node` 或任何数据节点目录。
- node、cluster、tenant 等占位字段或空模型。
- 节点 RPC、注册、发现、心跳、调度、故障转移。
- 选举、分布式锁、共享数据库、一致性协议。
- HA、多管理员、多租户、RBAC、OIDC。
- PostgreSQL、Redis、消息队列、微服务、外部对象存储。

看到上述能力的提前实现、空接口、空页面、配置项、表、字段或目录时必须删除或停止确认，不得以“为未来预留”为理由保留。

## 3. 不创建空壳

- 不预建 protocol、transport、proxy、nathole 等无真实行为的空包。
- 不定义未来 Engine、NodeClient、ClusterManager 等猜测性接口。
- 不为 P2/P3 创建数据库字段、API 路径、Web 菜单或功能开关。
- 每个新增抽象必须服务当前已批准需求并被实际调用、测试。

## 4. 越界处理

若当前任务依赖后续阶段能力才能完成，停止并说明冲突、最小替代方案和范围影响，等待明确批准。实现优先采用最小直接方案，不做顺手增强或镀金。
