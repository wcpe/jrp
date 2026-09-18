# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布版本

### 新增

- 初始化 JRP 的产品需求、架构、接口、协议、运维与安全文档。
- 建立独立 Core、`jrps`、`jrpc`、React Web 和未来中心控制面的边界决策。
- 建立 ADR、功能规格、贡献指南、项目规则及 GitHub 协作模板。
- 固定官方 frpc 兼容研究基线与独立实现约束。
- 建立 MIT 许可证和单一产品版本真源。
- 清理正式文档中的开发机器绝对路径，以固定 Git 提交表达兼容研究基线。
- 拆分连接传输与代理需求：FR-05 拆为 FR-05a/05b/05c，FR-06 拆为 FR-06a/06b。
- 新增 FR-32，要求 Core 提供类型化配置构建器与校验 API 供嵌入宿主使用。
- 新增 ADR-0011，确立产品版本与 Core module 版本双轨：根 `VERSION` 管产品，`core/VERSION` 管 Core module。
- 为 FR-02 至 FR-17、FR-25 至 FR-32 编写 27 份功能规格，覆盖 P1 全部需求。
- 新增 ADR-0012，裁定 Core 公共包三层布局、revision 归属与嵌入契约，收敛 last-good 持有方的分歧。
- PRD 新增 §7.1，把 P1 中 FR-01 之外的 27 条需求按依赖序切成 S1 至 S5 五个执行切片及一个依赖待批准的后置组。
- 取证并修正 wire v1 帧格式：载荷长度字段为 8 字节有符号网络字节序，原文档记为 4 字节。
- 新增 ADR-0013，前端包改由根 pnpm + Turbo 编排，Go 任务仍由 Taskfile 编排；ADR-0008 相应收窄为 Go 任务编排决策。
- 引入 pnpm catalog 统一前端第三方依赖版本，并把「包内不得写死版本号」纳入依赖边界检查。
- 全量审核文档并修复：`core/event` 并入根包以符合 ADR-0012、last-good 归属表述、PRD 验收口径、P1 允许范围补入 FR-25 至 FR-32、端点权威归属、`platform/service` 与 `core/VERSION` 的时态。
- 架构文档补入 `platform/service` 作为 Core 与 apps 之外的第三个位置，并新增对应依赖规则。
- 依赖边界检查新增 Core 源码扫描：禁止 `os.Getenv`、`os.Open`、`os.ReadFile` 与 `database/sql`，保证 Core 不读取环境变量、文件或数据库（只约束生产源码，不约束测试）。
- 交付 FR-04 wire v1/v2 编解码：v1 帧编解码、带回放能力的预读版本判定、v2 帧头与 hello 协商、降级与拒绝判定表、加密/压缩状态机、有界缓冲池与统一拒绝出口。
- 补充 FR-04 分层测试：字节层黄金帧、边界长度、畸形输入、状态机迁移、v1/v2 同端口共存与模糊测试入口。
- 修正 `docs/PROTOCOL.md` 的 wire v2 契约：经黑盒取证确认版本魔数为 7 字节 `FRP\x00\x02\r\n`（原文档未记录取值），并补入 §3.1 已登记消息类型表。
- 修正 `docs/specs/wire-v1-v2-codec.md` §3.4 的 hello 结构描述：能力集合按 `capabilities` 与 `selected` 嵌套分组承载，原规格未记录该结构。
- 落地 FR-09：`jrps` 与 `jrpc` 各自建立独立 SQLite 持久化层（GORM + `github.com/glebarez/sqlite` 纯 Go 驱动，CGO-free），含事务化迁移、追加写约束与 `--data-dir`、`--database` 引导参数。
- FR-09 中 desired、active 与 last-good 分列表达：只有 desired 是持久化真源，另两者保存 Apply 结果记录，恢复时拒绝把 active 冒充真源。
- FR-09 中请求元数据走有界批处理通道，采集关闭时不进入落库路径；通知采用事务 outbox，外部副作用只在事务提交后触发。
- FR-09 中 token 只以摘要落库，读取视图与审计一律掩码。
- OPERATIONS 补入 FR-09 已交付的 `--data-dir`、`--database` 引导参数与两侧数据库独立性约定。
- 交付 FR-25 首个真实垂直切片：`core/server` 与 `core/client` 双侧 Engine 门面，TCP 传输 + wire v1 控制会话 + 一个 TCP 代理端到端闭环；生命周期为 New、Start、Shutdown、Done，Start 成功接管宿主资源、失败保留给宿主，Shutdown 后无残留监听器、连接或 goroutine。
- 修复 FR-25 的 Engine 异常可观测性：控制与心跳失败路径统一记录首个异常错误并关闭 Done，`Err()` 不再恒为 nil；Shutdown 保持 Err() 为 nil。
- 修复 FR-25 双端 Start 并发竞态：认领启动位时原子置为 starting，失败回滚到 idle，重复启动返回哨兵错误。
- 修复 FR-25 排水超时语义：`waitDrained` 超限返回可判定的超时错误，不再与正常排空混同为 nil；已过期的 ctx 直接返回其错误。
- 修复 FR-25 桥接泄漏：配对成功的桥接纳入 WaitGroup 并追踪连接，Shutdown 按排水上限等待，不再依赖对端关闭。
- 新增 `task test:core:external` 外部消费验证：独立 module 在 `GOWORK=off` 下只依赖 Core 公共包跑通端到端闭环。
- 修复 Engine 正常 Shutdown 后 `Err()` 非 nil：异常记录仅在运行期生效，Shutdown 不再被连接关闭引发的读错误污染。
- 修复排水语义：Shutdown 先停监听、再按排水上限等待数据桥接自然结束，活动流不再在关闭瞬间断裂；控制连接与未配对暂存连接立即释放。
- 修复 Start 失败错误不可判定：监听器不可用时包装哨兵 `ErrNotStarted`，宿主可用 `errors.Is` 判断。
- 修复 wire 协商上限未回灌：协商达成后收紧 v2 读取器上限，超限消息帧按协商值拒绝而非按实现上限放行。
- 补齐 FR-25 边界测试：任意长度与跨分片传输、未 Start 就 Shutdown、未注入 logger 不写标准输出、并行 Engine 独立启停、半关闭 EOF 传播、注入 logger 生效。
- 收紧空洞断言：重复 Start 与停止后重启改用 `errors.Is` 判定哨兵；错误脱敏测试改为构造真实拒绝路径。
- 依赖门补入 Web 框架与通知库两类禁用模式。
- FR-09 规格移除在线备份验收项：该能力不在 PRD 需求列表内，属独立后续需求。
- 收紧 SQLite 数据库文件权限：Linux 等平台的数据库与 WAL/SHM 由驱动默认 0644 改为 0600，符合「数据目录权限仅限运行账户」的验收要求。
- S1 切片交付：FR-32 配置构建器、FR-04 wire v1/v2 编解码、FR-09 SQLite 配置存储通过验收并标记为已交付@0.1.0。
- FR-25 通过全部 18 条验收标准：PRD §4 状态改为「已交付@0.1.0」，规格 §4 的十项任务全部勾选。
- FR-25 取证记录：Core 全量测试、`-race` 全量测试、`task lint:go` 依赖门与外部消费 `GOWORK=off` 验证均通过；三平台 CI（`Go / ubuntu-latest`、`windows-latest`、`macos-latest`）全绿。
- 补 FR-32 两处测试缺口：服务端绑定名的正向边界（单字节与恰为上限构建成功），以及取值类错误必须给出受支持取值列表的断言。
- wire 层模糊测试接入 CI：新增 `task test:fuzz` 入口与独立 CI job，三个 fuzz 入口各跑 10 秒。
- CI 升级 action 版本至 Node 24 运行时：checkout v7、setup-go v7、setup-node v7、pnpm/action-setup v6，消除 Node.js 20 弃用警告。
- CI 的文档路径忽略范围由 `.claude/rules/**` 扩至 `.claude/**`。
- README 补入 CI、许可、Go 与 pnpm 徽章，登记 S1 已交付能力，并更新快速开始为当前可用命令。
- 交付 FR-02 首次初始化与单管理员认证：`jrps init` 在本机一次性建立唯一管理员凭据，密码只经交互式标准输入或一次性环境变量读取，禁止命令参数形态；管理员记录与初始化标记在同一事务提交，重复执行返回中文错误且不覆盖密码。
- FR-02 建立首个 `/api/v1` 路由面与中间件挂载点，并实现会话端点：`POST`/`GET`/`DELETE /api/v1/session` 分别建立、查询与注销会话；未初始化时 `/api` 前缀一律返回 503，`/healthz` 与 `/readyz` 保持可用。
- FR-02 会话采用 `HttpOnly`、`Secure` 与 `SameSite=Strict` Cookie，服务端只保存会话令牌摘要；修改请求必须校验 CSRF token，缺失或错误返回 403 且不产生副作用。
- FR-02 管理员密码使用标准库 PBKDF2-HMAC-SHA256 派生，盐与派生参数随记录保存；登录失败不区分用户不存在与密码错误，连续失败达到阈值后限流，登录、登出与失败尝试均写入审计事件且不含凭据材料。
- FR-02 新增 `admin_initialized`、`admin_login`、`admin_logout`、`admin_login_failure` 四个审计动作常量，与既有审计常量块保持同一命名风格。
- 新增 `core/internal/transport` 传输层（FR-05a）：把 FR-25 内联的 TCP 实现抽取为正式传输抽象，提供带用途标记与建链时间的连接句柄、监听句柄的接管与释放、Accept 错误的临时/致命分类、强制超时的拨号器、工作连接池与待命连接空闲回收，零第三方依赖。
- 传输层删除两处硬编码：双侧引擎写死的 `drainTimeout = 10s` 改为取自配置快照的排水上限，服务端访客监听地址写死的 `127.0.0.1` 改为继承配置的监听端点地址族。
- 修正 FR-25 无超时拨号：工作连接拨号不再传 `context.Background()` 且不再使用零值 `net.Dialer{}`，拨号超时一律来自配置快照，不可达地址按超时返回而不悬挂。
- 新增工作连接池：客户端按代理占用池槽位，达到上限立即拒绝并累加可观测事件计数，不无限等待；服务端待命工作连接按空闲上限回收最早的暂存连接。
- 配置快照新增排水上限、工作连接池上限与待命空闲上限三组参数（含默认值与 Core 常量上界），并删除 FR-25 客户端未使用的 `WithDialer` 选项。
- 修复带 `webui` 构建标签的测试在常规测试中被漏检：`NewRouter` 增加选项参数后，仅该标签下的用例编译失败，而它此前只在生产构建链路执行，本地测试与常规 CI 都测不到。现把 `test:jrps:webui` 纳入 `test:go`，使本地 `task test` 即可覆盖。
- 修复连接重置测试的拨号竞态：服务端接受后立刻重置与客户端拨号无同步，负载高时会在拨号阶段即失败。改为先确认连接建立再重置，并用变异验证确认断言强度未降低。
- 修正连接终止测试对错误类型的过度指定：对端以 RST 终止时，不同内核与负载下本地可能读到 ECONNRESET 或 EOF，区分 RST 与 FIN 在 CI 上不稳定。改为断言被测目标本身（关闭幂等 + 句柄不可再写＝资源已释放），并更名准确反映语义。
- 交付 FR-06a 四种代理：Core 新增 `core/internal/proxy` 代理层，TCP、UDP、HTTP、HTTPS 共用同一套生命周期（注册校验 → 资源创建 → 接收流量 → 停止接收 → 排空），并把 FR-25 内联的 TCP 代理重构为挂靠 FR-05a 连接抽象，不再保留第二套实现。
- FR-06a 注册校验固定为四级顺序（字段合法性 → 权限 → 端口与域名/路径冲突 → P1 范围），失败不留半注册监听器或路由；新增 `CodePortConflict`、`CodeRouteConflict`、`CodeInvalidRoute` 三个稳定错误码。
- FR-06a 目标地址越权校验：四种服务端绑定新增必填的 `AllowedTargets` 允许集合（空集合视为允许任意转发，因此按校验失败处理），客户端在工作连接声明中上报实际目标地址，越权即拒绝该工作连接且不接入数据面。
- FR-06a UDP 代理按对端地址会话化管理：会话有空闲上限并在超时后回收，会话数达上限时新对端被拒绝并计数，数据报超上限被丢弃并计数，不按声明长度无界分配。默认值为会话空闲 60s、会话上限 8、数据报上限 1400 字节（规格 §6 标为待定，按最小可用选取，真实网络下的合适取值仍需用户实机确认）。
- FR-06a HTTP 代理支持多代理共享同一入口端口：路由规则定为「主机名精确匹配（不区分大小写）+ 路径最长前缀匹配」，未匹配返回不回显内部路由表的 404；正文采集只保留逐代理开关且不实现存储，落盘属于 jrps 适配器。
- FR-06a HTTPS 代理 P1 为 TLS 透传：绑定类型不含也不得出现证书、私钥或正文采集字段，Core 不终止 TLS、不解密、不生成正文记录。
