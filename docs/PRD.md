# 产品需求文档（PRD）：JRP

> 本文是 JRP 需求与路线图的单一真源，说明做什么以及为什么做。功能的详细工作规格放在 `docs/specs/`，架构实现方式见 `docs/ARCHITECTURE.md`。

## 1. 背景与目标

frp 以配置文件为中心，适合直接部署，但个人与小团队在多客户端、多代理长期运维中仍需自行处理配置分发、变更审计、请求统计、告警通知和无中断变更。JRP 的目标是在保持官方 frpc 互操作能力的同时，以 SQLite 配置真源、自研 jrpc、Web 管理、可观测性和通知形成一套轻量、单机优先的反向代理与 P2P 打洞平台。

一句话价值主张：让个人和小团队用一个可审计、可热更、可观测的控制面管理兼容 frpc 的代理网络，而不依赖重型基础设施。

### 非目标

- 不复制、改写、链接、导入 frp 源码或 Go package。
- P1 不实现 SUDP、TCPMUX、未列明的剩余参数与插件、QQ 通知、HTTPS 终止采集或深度分析。
- P1 不实现数据节点、节点 RPC、节点注册、调度、选举、分布式锁、共享数据库或 HA。
- P1 不实现多管理员、多租户、RBAC、OIDC、PostgreSQL、Redis、消息队列、对象存储或微服务拆分。
- 不把官方 frpc 兼容数据协议扩展成 JRP 私有管理协议。
- 不承诺在未终止 TLS 的 HTTPS 透传场景读取 HTTP 正文。

## 2. 角色

- **个人部署者/管理员**：部署 `jrps`，通过单管理员 Web 管理客户端、代理、通知、采集与运行状态。
- **官方 frpc**：使用兼容数据协议连接 `jrps`，不具备 JRP 私有配置下发能力。
- **自研 jrpc**：通过兼容数据协议建立隧道，并通过独立 HTTPS/WSS 管理通道 enrollment、鉴权和接收版本化配置。
- **被代理服务**：位于客户端侧，由 TCP、UDP、HTTP、HTTPS、STCP 或 XTCP 代理暴露或访问。
- **代理访问者**：通过公网入口或访客模式使用被代理服务，不拥有管理权限。
- **数据节点**：P3 才引入的运行角色，只组合 Core 并接收中心控制面的版本化配置。

## 3. 用户故事

- 作为管理员，我希望官方 frpc 可直接连接 `jrps`，以便平滑接入现有客户端。
- 作为管理员，我希望每个客户端使用独立 token，以便单独吊销、轮换和审计。
- 作为管理员，我希望在 Web 中修改配置并无中断生效，以便已有代理连接不被配置更新切断。
- 作为 jrpc 使用者，我希望客户端自动接收版本化 desired state，以便不再手工分发配置文件。
- 作为管理员，我希望查看代理状态、日志、指标和通知，以便快速定位异常。
- 作为管理员，我希望只对指定 HTTP 代理采集正文并受保留上限约束，以便在风险可控的前提下排障。
- 作为维护者，我希望 Core 与数据库、Web、通知和可执行外壳隔离，以便未来复用于 P3 数据节点。
- 作为其他 Go 应用开发者，我希望通过稳定、无全局副作用的 ServerEngine/ClientEngine 嵌入 JRP 数据面，以便复用协议与代理能力而不携带 jrps、jrpc 或 Web 外壳。
- 作为管理员，我希望 jrps 与 jrpc 能安装为 Linux/Windows 系统服务并在无人登录时自动启动，以便设备重启或进程异常后自动恢复代理能力。

## 4. 功能需求（FR）

| 编号 | 需求 | 阶段 | 状态 |
|---|---|---|---|
| FR-01 | 建立独立 Core、jrps、jrpc 与 Web 边界，并由 go.work 编排三个 Go module | P1 | 计划 |
| FR-02 | 提供首次初始化和单管理员认证，不引入多管理员或 RBAC | P1 | 计划 |
| FR-03 | `jrps` 接受固定兼容基线下的官方 frpc 连接 | P1 | 计划 |
| FR-04 | Core 支持 wire v1 与 wire v2，具备明确协商、降级和拒绝行为 | P1 | 开发中 |
| FR-05a | 支持 TCP 连接传输（不引入第三方依赖） | P1 | 计划 |
| FR-05b | 支持 WebSocket 与 WSS 连接传输（依赖 `golang.org/x/net`，待批准引入） | P1 | 计划 |
| FR-05c | 支持 KCP 与 QUIC 连接传输（依赖 `kcp-go`、`quic-go`，待批准引入） | P1 | 计划 |
| FR-06a | 支持 TCP、UDP、HTTP、HTTPS 四种代理模式 | P1 | 计划 |
| FR-06b | 支持 STCP 与 XTCP 代理，含访客连接与 NAT 打洞 | P1 | 计划 |
| FR-07 | 每个客户端使用独立 token，支持创建、轮换、吊销和审计 | P1 | 计划 |
| FR-08 | jrpc 通过独立 HTTPS/WSS 管理通道 enrollment 并接收版本化 desired state | P1 | 计划 |
| FR-09 | jrps 与 jrpc 分别以 SQLite 保存配置、revision 与必要运行元数据 | P1 | 开发中 |
| FR-10 | 配置按 prepare、health-check、atomic publish、drain 无中断应用，失败保留 last-good | P1 | 计划 |
| FR-11 | 提供单管理员 React Web 管理客户端、代理、配置版本、采集与通知 | P1 | 计划 |
| FR-12 | 记录请求与运行日志，并对敏感凭证和正文实施脱敏与访问控制 | P1 | 计划 |
| FR-13 | 仅对明文 HTTP 按代理开启正文采集，元数据存 SQLite、正文存压缩分段文件 | P1 | 计划 |
| FR-14 | 提供客户端、代理、连接、流量、错误、配置应用和采集容量监控 | P1 | 计划 |
| FR-15 | 支持 Webhook 与邮件通知，通知副作用在事务提交后触发 | P1 | 计划 |
| FR-16 | 审计安全敏感操作，并执行正文默认 30 天、总量 5 GiB 的保留策略 | P1 | 计划 |
| FR-17 | 在 Windows、Linux、macOS 上测试并构建 jrps 与 jrpc 二进制 | P1 | 计划 |
| FR-18 | 支持 SUDP 与 TCPMUX | P2 | 计划 |
| FR-19 | 补齐兼容基线的剩余参数与插件能力 | P2 | 计划 |
| FR-20 | 支持 QQ 通知和深度流量/请求分析 | P2 | 计划 |
| FR-21 | 支持 JRP 明确终止 TLS 后的 HTTPS 正文采集 | P2 | 计划 |
| FR-22 | 建立协议升级兼容策略与跨版本矩阵 | P2 | 计划 |
| FR-23 | 建立中心控制面与多数据节点拓扑、调度和故障转移 | P3 | 计划 |
| FR-24 | 在分布式需求成立后评估 HA、RBAC、OIDC、外部数据库与对象存储 | P3 | 计划 |
| FR-25 | 提供可嵌入其他 Go 应用的 ServerEngine/ClientEngine，并以 TCP、wire v1、TCP 代理形成首个真实垂直切片 | P1 | 开发中 |
| FR-26 | 支持基于完整不可变快照和 revision 的 Apply，在宿主注入网络资源时完成 prepare、publish、drain 与无中断切换 | P1 | 计划 |
| FR-27 | 提供有界类型化事件订阅与只读状态快照，使嵌入宿主可实时观测并在丢事件后重建状态 | P1 | 计划 |
| FR-28 | 为 Core 建立独立 SemVer、外部模块消费验证和公共 API 兼容政策 | P1 | 计划 |
| FR-29 | 建立独立 `platform/service` module 和 jrps/jrpc 一致的系统服务安装、卸载、启停、重启、状态 CLI | P1 | 计划 |
| FR-30 | 支持将 jrps 与 jrpc 安装为 Linux systemd 系统服务，使用低权限用户、开机启动、失败退避重启和优雅停止 | P1 | 计划 |
| FR-31 | 支持将 jrps 与 jrpc 注册为 Windows SCM 系统服务，使用 LocalService、Automatic 启动、失败恢复动作和优雅停止 | P1 | 计划 |
| FR-32 | Core 提供类型化配置构建器与校验 API，供嵌入宿主构造不可变配置快照；Core 不读取配置文件与环境变量 | P1 | 开发中 |

状态取值为“计划”“开发中”“已交付@版本”。只有验收标准、自动化测试和要求的实机验收均通过后，才能标记为已交付。

## 5. 非功能需求（NFR）

- **规模基线**：单机目标支持 200 个在线客户端和 2,000 个代理；超出基线需重新压测和容量规划。
- **数据面隔离**：关闭正文采集时，Core 数据热路径不得访问 SQLite；管理和统计写入通过有界通道与批处理隔离。
- **性能门禁**：在相同机器、相同传输和相同代理场景下与固定参考实现做黑盒吞吐、延迟和资源占用对比；基线与容差在对应功能 spec 中量化。
- **可用性**：非引导配置更新不得切断已有代理连接；准备失败不得替换 active 或 last-good revision。
- **保留策略**：HTTP 正文默认关闭；开启后默认保留 30 天，总量上限 5 GiB，任一限制先到即清理。
- **安全**：凭证和正文允许明文静态存储，但 API、日志、UI 和审计必须脱敏；正文访问必须审计。
- **可移植性**：Windows、Linux、macOS 均须通过 Core、jrps、jrpc 测试并构建两个二进制。
- **可维护性**：Core 不依赖 Gin、GORM、SQLite、Web、通知或 CLI；jrps 与 jrpc 互不导入。
- **嵌入安全性**：Core 不读取环境变量、配置文件或数据库，不调用 `os.Exit`，不注册全局信号、全局 HTTP 路由或可变单例；同一进程可并行运行多个 Engine。
- **公共 API 边界**：Core 公共接口只暴露标准库类型与 Core 自有值类型，不泄漏 Gin、GORM、SQLite、QUIC/KCP 第三方实现类型或 `internal` 类型。
- **版本兼容**：Core 使用独立 SemVer；首个真实垂直切片发布 v0，全部 P1 与外部嵌入验收稳定后才进入 v1。
- **系统服务边界**：自启动能力位于独立 `platform/service` module，不进入 Core；首版只支持 Linux systemd system scope 与 Windows SCM，不支持 OpenRC、SysV、用户级服务或第三方守护工具。
- **最小权限**：Linux 服务必须使用明确的现有非 root 用户，Windows 默认使用 LocalService；服务注册、unit 和进程命令行不得包含 token、密码或通知凭据。
- **数据保留**：服务卸载不得删除 SQLite、日志、证书或正文数据；默认数据目录使用 Linux `/var/lib/jrp/{jrps,jrpc}` 与 Windows `%ProgramData%\\JRP\\{jrps,jrpc}`，允许绝对路径覆盖。
- **版权边界**：只以公开行为、独立规格和黑盒证据实现兼容，不复用参考源码、注释、错误文案、目录或测试组织。
- **可观测性**：日志使用中文分级信息，指标不得包含 token、密码、Cookie、Authorization 或完整正文。

## 6. 验收标准

- Core、jrps、jrpc 是独立 Go module，根 `go.work` 可统一编排，Core 依赖图不存在 Gin、GORM、SQLite、Web、通知、apps module 或 frp package。
- 官方 frpc 在固定参考基线下通过 wire v1/v2、FR-05a/05b/05c 连接传输和 FR-06a/06b 代理模式的黑盒互操作矩阵；涉及真实网络/NAT 的结果需用户确认实机通过。FR-05b 与 FR-05c 的部分在其第三方依赖获批后执行，依赖未批准期间不计入 P1 验收口径。
- jrpc enrollment、token 隔离、desired revision 下发、断线恢复和冲突处理具备自动化测试与实机验证。
- 配置更新在新资源准备成功后原子发布；长连接和活动流在 drain 期间不中断；失败场景保留旧版本。
- Web 能完成单管理员登录、客户端/代理管理、配置应用、状态查看、采集管理和通知管理，权限与 CSRF 测试通过。
- 关闭采集时数据面无 SQLite 访问；开启时仅采集可见明文 HTTP，压缩分段、30 天和 5 GiB 清理策略通过边界测试。
- Webhook、邮件、审计、日志和指标的成功、失败、重试、脱敏与容量边界均有测试。
- Windows、Linux、macOS 的 Core/jrps/jrpc 测试通过，Web 测试和生产构建通过，生成 `jrps` 与 `jrpc` 二进制。
- 安全文档与实际行为一致，不声称凭证或正文已静态加密。
- 外部测试应用在 `GOWORK=off` 条件下只依赖 Core 公共包，即可嵌入 ServerEngine/ClientEngine 并完成 TCP、wire v1、TCP 代理闭环。
- 配置构建器对每类非法配置返回明确错误而非 panic；Core 测试不得读取文件或环境变量。
- Engine 遵循 Start、Shutdown、Done 生命周期；启动成功后接管宿主注入资源，启动失败仍由宿主持有，关闭后无残留监听器、连接或 goroutine。
- 完整 Snapshot/Revision 应用失败不得替换 active；监听变化时已有流保持连续，并覆盖并发 Apply、超时、回滚与资源释放测试。
- 事件订阅缓冲有界且不得阻塞数据面；溢出后宿主可依据重同步事件和只读状态快照恢复一致视图。
- Core 独立版本、module tag、迁移说明和 API 兼容政策可验证；v1 发布前不得承诺尚未稳定的底层 wire/session API。
- jrps 与 jrpc 在 Linux systemd 和 Windows SCM 下均可执行 install、uninstall、start、stop、restart、status，服务名固定且单机每个二进制只允许一个实例。
- Linux unit 使用现有非 root 用户、system scope、开机启用、`Restart=on-failure`、30 秒停止期限，并通过真实 systemd 主机重启验收。
- Windows 服务使用 LocalService、Automatic Start、失败退避恢复动作、Stop/Shutdown 控制码和 30 秒停止期限，并通过真实 Windows 主机重启验收。
- 异常终止后服务按有限退避自动恢复，正常 stop 后不得自动重启；卸载后数据目录和数据文件保持不变。

## 7. 分期（路线）

- **第一期（P1）**：建立可单机运行、兼容官方 frpc、由 SQLite 与 Web 管理、可无中断热更的完整平台。
- **第二期（P2）**：补齐高级兼容能力、通知渠道、深度分析和 HTTPS 终止采集。
- **第三期（P3）**：演进为中心控制面与多数据节点，并在需求成立后评估企业级身份、HA 和外部存储。

需求所属阶段只以 §4 表为准。下表是 P1 内部的执行切片，用于确定开发顺序与并行边界，不改变阶段归属。

### 7.1 P1 切片

切片顺序遵循「先地基、后上层」：每片内的条目互不依赖，可并行开发；下一片必须等上一片落定后才能开始。

| 切片 | 包含 FR | 条数 | 前置 | 说明 |
|---|---|---|---|---|
| S1 | FR-32、FR-04、FR-09 | 3 | 无 | 配置构建、wire 编解码、SQLite 持久化 |
| S2 | FR-25、FR-05a、FR-06a、FR-02、FR-15、FR-16 | 6 | S1 | Engine 门面与首个垂直切片、TCP 传输、四种代理、认证、通知、审计 |
| S3 | FR-26、FR-27、FR-03、FR-06b、FR-07、FR-10、FR-13、FR-12 | 8 | S2 | Apply 与事件订阅、日志、官方 frpc 接入、STCP/XTCP、token、无中断热更、正文采集 |
| S4 | FR-08、FR-11、FR-28、FR-14 | 4 | S3 | enrollment 与配置下发、Web 管理台、Core 版本政策、监控 |
| S5 | FR-29、FR-30、FR-31、FR-17 | 4 | S4 | 系统服务 module、Linux systemd、Windows SCM、三平台构建 |
| 后置 | FR-05b、FR-05c | 2 | 依赖批准 | WebSocket/WSS 与 KCP/QUIC 传输，第三方依赖获批后启动 |

- FR-01（多 module 工程骨架）不在切片内：它已在工程初始化阶段达成，待验收后另行判定状态。
- FR-05b 与 FR-05c 已登记但处于依赖待批准状态，不计入 S1~S5 的完成度口径。
- FR-12 依赖 FR-27 提供的有界事件通道，故与 FR-27 同属 S3；其日志等级、脱敏矩阵与查询接口本身不依赖 FR-27。
- 切片不替代验收：每个 FR 仍须独立通过其验收标准才可标记交付。

## 8. 术语表

- **Core**：独立 Go module，承载协议、传输、控制会话、工作连接、代理、访客和 NAT 能力。
- **jrps**：服务端与控制面可执行外壳，装配管理 API、SQLite、通知、采集和嵌入式 Web。
- **jrpc**：JRP 自研客户端，可接收服务端版本化配置；不是官方 frpc 的分支。
- **desired revision**：SQLite 中管理员期望应用的配置版本。
- **active revision**：Core 当前对新连接生效的不可变配置快照版本。
- **last-good revision**：最近一次成功准备并发布的配置版本。
- **wire v1/v2**：官方兼容数据协议的两种线上封装版本。
- **引导参数**：数据目录、SQLite 路径、管理 API 根监听；变更需要重启。
- **ServerEngine / ClientEngine**：Core 面向嵌入宿主的服务端与客户端稳定运行门面，负责生命周期、配置应用和数据面资源管理。
- **Deployment**：提交给 Engine 的完整不可变配置单元，由 revision、snapshot 与本次新增或变化的宿主资源组成。
- **Subscription**：有界类型化事件订阅；慢消费者不得阻塞数据面，溢出时要求宿主通过状态快照重新同步。
- **配置构建器**：Core 公共 API 中构造并校验不可变配置快照的类型化入口；只接受宿主在代码中传入的值，不读取文件或环境变量。
- **platform/service**：Core 之外的独立 Go module，为 jrps、jrpc 提供 systemd 与 Windows SCM 的安装、运行和状态适配。
- **系统服务**：由操作系统在无人登录时管理的 jrps/jrpc 进程，负责开机启动、停止控制和异常恢复，不等同于 P3 数据节点。
