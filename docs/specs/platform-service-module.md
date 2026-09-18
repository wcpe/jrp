# 功能规格：platform/service 系统服务模块

> 状态：草拟 · 关联 PRD：FR-29 · 分支：feature/platform-service-module

## 1. 背景与目标

管理员希望 `jrps` 与 `jrpc` 在设备重启或进程崩溃后能自动恢复代理能力，而不需要有人登录后再手工拉起。这要求两个二进制理解所在操作系统的服务管理机制。若把这份能力写进 Core，会破坏"Core 只承载数据面且不依赖任何外壳"的红线；若各自写进 `apps/jrps` 与 `apps/jrpc`，两份实现会在服务语义、参数命名与错误处理上漂移，且 jrps 与 jrpc 互不导入，无法共享。

FR-29 要解决的问题是：建立 Core 与 apps 之外的第三个位置——独立 Go module `platform/service`——统一承载 Linux systemd 与 Windows SCM 的安装、卸载、启停、重启、状态适配，并为 `jrps` 与 `jrpc` 提供一致的一组子命令。

使用者是部署者、管理员与维护 JRP 的工程师。所属阶段 P1（切片 S5）。该 module 不属于 `apps/*`，是平台层。Core 边界遵循 ADR-0002 与 ADR-0003，命令真源遵循 ADR-0008，本规格不重复其决策正文。具体平台的 unit 与 SCM 语义由 FR-30（Linux systemd）与 FR-31（Windows SCM）两份规格定义，本规格只定义它们共同的抽象、CLI 与约束。

## 2. 需求

- 建立独立 Go module：`platform/service` 位于仓库根的平台层目录，不进入 `core`，也不进入 `apps/jrps` 或 `apps/jrpc`。
- `jrps` 与 `jrpc` 提供一致的一组子命令：`install`、`uninstall`、`start`、`stop`、`restart`、`status`，命令名、参数形式、退出码与输出语言一致。
- **服务名固定**：`jrps` 与 `jrpc` 各自使用固定的服务名，不由命令行参数指定，单机上每个二进制只允许一个实例。
- 自启动能力不得进入 Core：Core 依赖图不得出现该 module，Core 不得感知"自己是 systemd 或 SCM 服务"。
- 该 module 不得破坏 Core 边界：它不得把 Gin、GORM、SQLite、Web、通知或管理 DTO 依赖引入自身，也不得反向要求 Core 增加"服务生命周期"接口。
- **最小权限**：Linux 使用明确的现有非 root 用户，Windows 默认 LocalService（FR-30、FR-31 各自落地）。
- **凭据禁令**：服务注册内容、unit 文件内容、进程命令行不得包含 token、密码或通知凭据。
- **数据保留**：`uninstall` 不得删除 SQLite、日志、证书或正文数据；默认数据目录 Linux `/var/lib/jrp/jrps` 与 `/var/lib/jrp/jrpc`，Windows `%ProgramData%\JRP\jrps` 与 `%ProgramData%\JRP\jrpc`，允许以绝对路径覆盖。
- **统一停止语义**：停止请求给进程 30 秒优雅退出期限；正常 `stop` 后不得自动重启；异常终止后按有限退避自动恢复。
- idempotent 语义明确：重复 `install` 与重复 `uninstall` 都有确定结果，不因幂等性缺口把管理员推向脚本化 workaround。
- 权限需求明确：安装与卸载需要管理员或 root 权限，启动与停止需要相应权限；权限不足时返回明确中文错误而非静默部分成功。
- 六个子命令在两个二进制中同名同形地分布，行为差异只来自「谁是服务主体」，不来自实现分支。

- 范围内：
  - `platform/service` module 的目录位置、module 路径、依赖方向与可被谁导入。
  - 统一的六个子命令、参数与退出码语义。
  - 固定服务名与单机单实例约束。
  - 平台适配抽象（探测、安装、卸载、启停、状态查询）与其错误处理契约。
  - install 所需的引导参数：可执行文件路径、数据目录、以及 jrps 的管理监听地址。
  - 最小权限、凭据禁令、数据保留三条 NFR 在 module 层的统一表达。
  - 三平台的构建与测试纳入到该 module 时的方式。
- 范围外：
  - OpenRC、SysV init、runit、launchd、supervisord、容器编排与任何第三方守护工具。
  - macOS 自启动（launchd 或 Login Item）：FR-17 只要求 macOS 上测试与构建通过，不要求自启动。
  - 用户级服务（`systemd --user` 或 Windows 用户会话服务）。
  - 自动生成服务账户或运行身份的创建。
  - 配置自愈、自动升级、看门狗式的进程心跳与业务健康探测。
  - 任何 Web 管理界面、HTTP 接口或远程服务控制面。
  - P3 的数据节点、节点 RPC、选举、分布式锁、共享数据库、HA、多管理员、RBAC、OIDC。

## 3. 设计

### 3.1 module 位置与依赖方向

```text
core                      （纯数据面，不得依赖下方任何 module）
apps/jrps  ──┐
             ├──▶ platform/service   （平台适配，Go 标准库 + 平台 API）
apps/jrpc  ──┘
```

- `platform/service` 是仓库内的独立 Go module，module 路径位于 `github.com/wcpe/jrp` 之下，由根 `go.work` 编排；它**不属于** `apps/*`。
- 依赖方向单向：`apps/jrps` 与 `apps/jrpc` 可以导入它；它不得导入 `core`，也不得导入 `apps/*`。
- 它不持有业务知识：只知道"如何把一个可执行文件路径注册为某平台的服务"，不知道 jrps 与 jrpc 的差别。"哪个二进制、哪个数据目录"由上层的 apps 通过参数传入。

**为什么不进 Core**：Core 是被第三方 Go 应用嵌入的数据面库，禁止依赖 Gin/GORM/SQLite/Web/通知/CLI，也禁止承担外壳职责；自启动属于部署形态而非数据面能力，进入 Core 会让第三方宿主继承操作系统服务依赖，违背嵌入安全性 NFR。

**为什么不进 apps**：jrps 与 jrpc 互不导入，若各自实现会在服务名、幂等性、退避策略与错误文案上漂移；把共享部分下沉到第三个 module，两个外壳只保留各自的引导参数差异，且这份能力未来也可被其他需要自启动的外壳复用而不牵动 Core。

### 3.2 平台适配抽象

module 内部按平台提供实现，向上暴露一组语义确定的动作：

| 动作 | 语义 | 平台实现 |
|---|---|---|
| 探测 | 判断当前平台是否存在可用的服务管理器 | Linux 检测 systemd system scope；Windows 检测 SCM 可用；其他平台返回不支持 |
| 安装 | 写入 unit 或注册 SCM 服务，但不启动 | FR-30 的 unit 写入与启用；FR-31 的 SCM 注册与失败动作配置 |
| 卸载 | 停止并移除注册，**保留数据目录与数据文件** | 两平台均不触碰数据目录 |
| 启动 / 停止 / 重启 | 对已注册服务执行控制 | 两平台均沿用各自的 30 秒停止期限 |
| 状态查询 | 返回可判定的运行状态与可选的主进程标识 | 两平台分别查询（FR-30：systemctl 语义；FR-31：SCM 状态语义） |

抽象只描述动作语义，不描述具体命令或 API 名称；平台差异留在平台实现内，不向上泄漏。不支持的平台返回"不支持"错误，由上层输出中文提示，绝不静默降级为"前台运行"。

### 3.3 CLI 契约

`jrps` 与 `jrpc` 各自的子命令保持一致：

| 子命令 | 行为 | 幂等性 | 成功退出码 |
|---|---|---|---|
| `install` | 注册服务并启用开机启动；已存在且与期望一致时不失败 | 幂等 | 0 |
| `uninstall` | 尝试停止后移除注册，**不删除数据** | 幂等 | 0 |
| `start` | 启动已注册服务 | 非幂等但可判定：已在运行时不判为失败 | 0 |
| `stop` | 请求优雅停止，30 秒内未退出才强制终止 | 已在停止时不判为失败 | 0 |
| `restart` | 依 stop 再 start 的顺序执行 | — | 0 |
| `status` | 输出中文状态与退出码：运行中、已停止、未安装分别对应不同码 | 只读 | 0 |

统一约束：

- 服务名固定且不可通过参数覆盖；同一台机器上每个二进制只允许一个服务实例，重复安装同一二进制不得产生第二个服务。
- install 只接受引导级参数：可执行文件路径、数据目录（可缺省为平台默认）与必要的监听地址。**不接受 token、密码、通知凭据或任何业务配置**。
- 输出到 stdout 的信息与错误到 stderr 的信息均使用简体中文；成功与失败都给出下一步可执行的提示。
- 权限不足时返回特定退出码与中文错误，不做部分注册：安装失败后不得留下半个 unit 或半个 SCM 注册项。

### 3.4 安全边界

- **凭据禁令的可执行检查**：install 写入的注册内容与进程命令行，只能包含可执行文件路径、数据目录、监听地址与显式允许的引导参数。测试须对生成的注册内容做字符串断言：不得出现任何疑似口令、token 或秘密字段的内容。
- **最小权限**：Linux 使用 install 时指定的现有非 root 用户；Windows 默认 LocalService。module 层拒绝把 root 或 LocalSystem 作为默认，也不自动创建账户。
- **数据保留**：uninstall 只删除注册本身。module 层的数据删除 API 一律不提供；数据目录清理属运维人工动作，由 OPERATIONS 记录的方式执行。

### 3.5 停止、退避与重启策略

- 优雅停止期限统一为 30 秒：进程收到停止请求后在该期限内自行收尾，超过则被强制终止。
- 正常 `stop` 与 `uninstall` 产生的停止不得触发自动重启。
- 异常终止（崩溃、被杀）后由服务管理器按有限退避自动恢复；退避次数与间隔上界在 FR-30、FR-31 中具体化，module 层只约束"有限且由平台原生机制表达，不自己写重试循环"。

### 3.6 构建与测试

- 该 module 加入根 `go.work`，并在 Windows、Linux、macOS 三平台进行测试与构建；新增任务入口必须按 ADR-0008 先加到根 Taskfile，再由 Makefile 与 CI 调用，不得在 CI 内写独立命令。
- 依赖门检查须覆盖该 module：它的依赖图不得出现 Core 禁止清单中的任何条目，也不得出现 `core` 与 `apps/*`。
- 平台专属实现用构建约束隔离，确保 Windows 上不编译 systemd 实现、Linux 上不编译 SCM 实现；两侧共用语义由可跨平台运行的测试覆盖（如 CLI 参数解析、幂等判定、退出码映射、凭据过滤）。

## 4. 任务拆分

- [ ] 先写失败测试：六个子命令的参数解析与退出码映射、重复 install/uninstall 的幂等性、单机单实例约束（重复安装不产生第二服务）、命令行与注册内容中不含凭据字段的断言、uninstall 后数据目录与 SQLite 文件仍存在的断言（初始为红）
- [ ] 建立 `platform/service` Go module 与根 `go.work` 编排，用构建约束隔离 Linux 与 Windows 实现
- [ ] 定义 §3.2 的平台适配动作与其不支持平台的明确错误
- [ ] 定义服务元数据与固定服务名，实现单机单实例约束
- [ ] 实现 CLI 共享层：install、uninstall、start、stop、restart、status 的参数、中文输出与退出码映射
- [ ] 实现 install 的引导参数校验与凭据字段过滤，禁止任何口令类参数进入注册内容与命令行
- [ ] 在 jrps 与 jrpc 中接入这六个子命令，使命令形态与文案一致，差异只来自各自引导参数
- [ ] 补足幂等语义：重复 install/uninstall/start/stop 的确定结果
- [ ] 在依赖门脚本中加入对该 module 的依赖图断言，确保其不依赖 core 与 apps
- [ ] 新增 Taskfile 任务入口并在三平台运行测试与构建，Makefile 只转发同名目标
- [ ] 同步 PRD、ARCHITECTURE、OPERATIONS、SECURITY、CHANGELOG 中受影响内容

## 5. 验收标准

自动化：

- `platform/service` 是独立 Go module，被根 `go.work` 编排；`jrps` 与 `jrpc` 可导入它。
- 依赖图断言通过：Core 依赖图中不含 `platform/service`；`platform/service` 依赖图中不含 `core`、`apps/*`、frp、Gin、GORM、SQLite。
- 六个子命令在 jrps 与 jrpc 上的名称、参数形式、退出码映射与中文输出一致；未安装、已安装运行中、已安装已停止三种情形的退出码可区分。
- 幂等：连续两次 `install` 不失败且不产生第二个服务；连续两次 `uninstall` 不失败；`stop` 已停止的服务不判为失败；`start` 已运行的服务不判为失败。
- 凭据过滤：构造一组含类口令、类 token 与类通知秘密字段的参数，断言它们不出现在生成的注册内容与最终命令行中，且命令返回明确错误。
- 数据保留：install 后生成数据文件（SQLite 主文件与日志），uninstall 后数据目录与这些文件仍然存在且内容可读。
- 不支持的平台返回明确"不支持"中文错误与非零退出码，不降级为前台运行。
- 安装失败（例如权限不足）后不残留半个注册项，同一服务再次 install 可成功。
- 构建约束生效：Windows 上不编译 systemd 实现，Linux 上不编译 SCM 实现。
- 三平台的该 module 测试通过；`task lint:go` 与依赖门检查通过。

边界与错误路径：

- install 缺少可执行文件路径时返回明确中文错误；数据目录缺省时落到平台默认目录（Linux `/var/lib/jrp/jrps` 与 `/var/lib/jrp/jrpc`，Windows `%ProgramData%\JRP\jrps` 与 `%ProgramData%\JRP\jrpc`）。
- 传入非法绝对路径或以相对路径充当数据目录时有确定结果与中文提示。
- status 在未安装时不 panic，返回"未安装"状态的确定退出码。
- macOS 上执行自启动子命令返回明确的"不支持"中文提示，不尝试 launchd。

实机与评测（需用户确认）：

- **实机验收依赖 FR-30 与 FR-31 的真实主机结果**：在本规格之上，Linux systemd 主机与 Windows 主机分别完成 install → status → start → status → restart → status → stop → status → uninstall 全流程，每一步核对中文输出与退出码；最终确认 uninstall 后 SQLite、日志、证书与正文数据仍然存在。
- **正常 stop 后不得自动重启**：stop 后等待若干分钟，由用户确认进程未被拉起。
- **异常终止后自动恢复**：由用户在 FR-30、FR-31 的实机验收中确认。本规格不单独重复该项验收。
- 上述 Linux 与 Windows 结果需用户在真实主机上确认通过；容器或 CI 中的模拟不能替代。

## 6. 风险与待定

- **风险**：`platform/service` 被误当成 Core 的一部分或 apps 的一部分，导致依赖方向反转。缓解方式是依赖门脚本对该 module 单独做依赖图断言，并在文档中固定依赖方向图。
- **风险**：macOS 上运行 SERVER_MACHINE 无自启动支持，管理员可能误以为 `task build` 产物自带自启动。缓解方式是 macOS 上明确返回不支持，并在 OPERATIONS 中标注 macOS 首版需人工拉起。
- **风险**：install 参数逐渐膨胀，把业务配置逐步带进服务注册，最终在命令行里出现凭据。缓解方式是 install 只接受引导级参数，任何新增参数都要先证明它不是业务配置。
- **风险**：30 秒停止期限对于大流量排空可能偏紧，运维会用 kill 绕过，反而破坏优雅停止。缓解方式是在 OPERATIONS 中记录排空预期并让进程在该期限内自行决定数据面收尾节奏。
- **待定**：该 module 是否有独立版本号。它与 Core、产品都不在同一条双轨内，`core/VERSION` 只管 Core module 版本，产品版本管 jrps/jrpc；`platform/service` 若未来被外部消费，需要另立版本策略，当前未决定，不预先承诺。
- **待定**：`status` 是否需要输出更丰富的运行信息（进程标识、运行时长）。当前只要求可判定的状态与区分的退出码，扩展需另行确认。
- **待定**：是否需要 `--data-dir` 之外的配置文件路径类引导参数。当前只有数据目录、SQLite 路径与管理 API 根监听属于引导参数，新增需确认不破坏"引导参数变更需重启"的既有约定。
