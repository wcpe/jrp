# 功能规格：Linux systemd 系统服务

> 状态：草拟 · 关联 PRD：FR-30 · 分支：feature/linux-systemd-service

## 1. 背景与目标

部署在 Linux 上的 `jrps` 与 `jrpc` 需要在无人登录时自动启动，并在进程崩溃后自动恢复，否则一次重启或一次崩溃就会让整个代理网络失去入口。管理员不想为这件事手写 unit 文件：手写意味着权限、重启策略、停止期限和数据目录都要靠人记住，而且容易在命令行里塞进凭据。

FR-30 要解决的问题是：让 `jrps` 与 `jrpc` 的 Linux 自启动成为一条可重复执行的命令，且生成的 unit 满足固定的安全与运行语义——system scope、明确的现有非 root 用户、开机启用、`Restart=on-failure`、有限退避、30 秒停止期限、优雅停止、不含任何凭据、卸载不删数据。

本规格是 FR-29 的平台落地之一：服务抽象与 CLI 契约由 `platform/service` module 规格定义，本规格只定义 Linux systemd 侧的具体形态。使用者是部署者与管理 JRP 的工程师。所属阶段 P1（切片 S5）。Core 边界遵循 ADR-0002 与 ADR-0003，命令真源遵循 ADR-0008，本规格不重复其决策正文。

## 2. 需求

- 支持将 `jrps` 与 `jrpc` 安装为 **systemd system scope** 服务，不是 user scope。
- 使用**明确的现有非 root 用户**运行：由 install 指定一个已存在的用户名；不自动创建服务账户，不默认使用 root，用户不存在或为 root 时返回明确中文错误。
- 开机启用：安装时启用开机启动，使主机重启后无人登录也能自动运行。
- `Restart=on-failure`：进程异常终止后由 systemd 自动重启；正常 `stop` 不得触发重启。
- 失败退避：自动重启按有限退避执行，不得无限即时重启；退避上界明确且写入 unit。
- 停止期限 30 秒：停止请求给进程 30 秒优雅退出时间，超时由 systemd 强制终止。
- 优雅停止：进程收到终止信号后自行收尾（停止接收新连接、按既有生命周期关闭），不靠 SIGKILL 完成正常流程。
- unit 文件与 ExecStart 命令行**不得包含 token、密码或通知凭据**。
- 数据保留：uninstall 不删除 SQLite、日志、证书或正文数据；默认数据目录 `/var/lib/jrp/jrps` 与 `/var/lib/jrp/jrpc`，允许绝对路径覆盖。
- 服务名固定，单机每个二进制只允许一个实例。
- 六个子命令（install、uninstall、start、stop、restart、status）与 FR-29 一致。
- 数据目录不存在时安装过程创建它并置为服务用户可读写；已存在时不改变其既有权限与内容。
- 不支持 systemd 的 Linux 环境返回明确中文错误，不静默降级。

- 范围内：
  - systemd **system scope** unit 的生成、写入、启用与移除。
  - 现有非 root 用户的指定与校验，以及数据目录属主与权限的处理。
  - `Restart=on-failure`、有限退避、30 秒停止期限与优雅停止的 unit 表达。
  - install、uninstall、start、stop、restart、status 在 systemd 上的实现。
  - 卸载保留数据与日志：保留 SQLite、正文、证书、运行日志与审计输出。
  - 与 `platform/service` module 的对接方式。
- 范围外：
  - **systemd user scope**（`systemd --user`、linger）：首版不做。
  - **OpenRC、SysV init、runit、supervisord、monit 等第三方守护工具与任何非 systemd 初始化系统**。
  - macOS 与 Windows 的自启动形态（Windows 见 FR-31）。
  - 自动创建服务账户、自动分配 UID/GID 或自动配置 sudo。
  - 容器编排、k8s/systemd-nspawn 等运行形态。
  - cgroup 资源限制、CPU/内存配额、以及 seccomp/AppArmor/SELinux 策略的编写与下发。
  - 日志转发到 journald 之外的集中式日志收集与轮转策略编排。
  - 单元文件的手工模板分发、Ansible/Chef/Puppet 配方与配置管理集成。
  - 服务自愈之外的业务健康探测、自动升级与看门狗。
  - P3 的数据节点、节点 RPC、选举、分布式锁、共享数据库、HA、多管理员、RBAC、OIDC。

## 3. 设计

### 3.1 与 platform/service 的关系

Linux 实现位于 `platform/service` module 内，由构建约束隔离，只编译到 Linux。它实现 FR-29 定义的动作：探测、安装、卸载、启停、状态查询。上层 `apps/jrps` 与 `apps/jrpc` 只传入引导参数（可执行文件路径、数据目录、监听地址、运行用户），不感知 systemd 细节。

依赖方向不变：`platform/service` 不导入 `core` 与 `apps/*`；`core` 不依赖该 module。

### 3.2 unit 文件形态

安装写入一个 system scope unit，位置遵循 systemd 的 system unit 目录约定，文件名由固定服务名派生。关键指令与取值：

| 指令 | 取值 | 理由 |
|---|---|---|
| `Description` | 中文描述，标明是 jrps 还是 jrpc | 便于运维识别 |
| `After` | 网络就绪目标 | 代理能力依赖网络可用 |
| `User` | install 指定的现有非 root 用户名 | 最小权限 NFR |
| `Group` | 该用户所属主组或同一显式组 | 与数据目录权限一致 |
| `WorkingDirectory` | 数据目录 | 引导参数之一 |
| `ExecStart` | 可执行文件绝对路径 + 引导参数 | 不含任何凭据 |
| `Restart` | `on-failure` | 异常终止后恢复，正常停止不重启 |
| `RestartSec` | 明确的有限秒数 | 退避起点 |
| `RestartSteps` / `RestartMaxDelaySec` | 有限步数与上界 | 有限退避，不无限即时重试 |
| `TimeoutStopSec` | 30 秒 | 停止期限 |
| `KillSignal` | 终止信号（可被进程优雅处理） | 优雅停止 |
| `KillMode` | 进程级 | 只终止服务主进程，不误伤同组进程 |
| `StartLimitBurst` / `StartLimitIntervalSec` | 有限次数与窗口 | 配合有限退避 |

`ExecStart` 只携带：可执行文件绝对路径、`--data-dir`（数据目录）与必要的监听地址引导参数。**不得**携带 token、管理员密码、Webhook secret、SMTP 密码或任何通知凭据。测试对写入的 unit 全文做断言：不得出现任何类口令字段与类 token 值。

### 3.3 运行用户与数据目录

- install 必须显式指定一个**已存在**的非 root 用户；缺省不带用户（不以 root 兜底），用户为 root 或不存在时返回明确中文错误。
- 数据目录缺省为 `/var/lib/jrp/jrps`（jrps）与 `/var/lib/jrp/jrpc`（jrpc），允许绝对路径覆盖。
- 目录不存在时创建，并把属主置为该服务用户、权限设为仅该用户可读写；目录已存在时保留既有内容与权限，只确保服务用户可读写。
- 数据目录内至少容纳 SQLite 主文件与伴随文件、正文压缩分段、运行日志与审计输出，以及本地证书与指纹信任状态（与 OPERATIONS 数据目录一节一致）。

### 3.4 停止与恢复语义

- `stop` 由 systemd 发出可处理的终止信号，进程在 30 秒内自行收尾；超过 `TimeoutStopSec` 才强制终止。
- 优雅停止期间进程按自身生命周期收尾：停止接收新连接、按既有排空语义处理已有连接、释放监听与资源。
- 正常 `stop`、`restart`、`uninstall` 触发的停止**不得**触发自动重启——`Restart=on-failure` 的语义本身就是不把正常退出视为失败。
- 异常终止（非零退出、被信号杀死、崩溃）触发按 `RestartSec` 与退避步数递增的有限退避重启；达到启动限制窗口的上界后 systemd 停止重试，进入失败态，由 status 可判定。
- 退出码语义：进程以成功状态码退出时不重启；以失败状态码或被信号终止时重启。进程不得用"假装成功退出"来规避恢复，也不得用"故意失败退出"来强制重启。

### 3.5 状态查询

`status` 返回可判定状态并映射到 FR-29 的统一退出码：未安装、已安装运行中、已安装已停止、失败态。失败态（如触发启动限制）必须可区分于"已停止"，并给出中文提示与下一步建议。

### 3.6 安装与卸载流程

安装：

1. 校验权限足以写入 system unit 目录并控制系统服务。
2. 校验运行用户存在且非 root。
3. 校验或创建数据目录并配置属主与权限。
4. 渲染 unit 文件（不含凭据），写入 system unit 目录。
5. 重新加载 systemd 配置，启用开机启动。
6. 不自动启动：安装后状态为"已安装已停止"，由管理员显式 `start`。

卸载：

1. 若服务存在则请求停止（沿用 30 秒期限）。
2. 禁用开机启动并移除 unit 文件。
3. 重新加载 systemd 配置。
4. **不删除数据目录及其中的任何文件**，并在输出中明确提示数据已保留及其路径。

安装失败时不残留半个 unit：写入失败需清理已写入文件，使同一服务可再次 install。

## 4. 任务拆分

- [ ] 先写失败测试：unit 内容断言（system scope、User 非 root、`Restart=on-failure`、`TimeoutStopSec` 为 30 秒、有限退避指令存在、ExecStart 不含凭据字段）、运行用户为 root 或不存在时报错、数据目录缺省值、uninstall 后数据文件仍存在（初始为红）
- [ ] 实现 systemd 探测与不支持环境的中文错误
- [ ] 实现 install：运行用户校验、数据目录创建与属主权限设置、unit 渲染与写入、重新加载与开机启用
- [ ] 在 unit 渲染层加入凭据字段过滤，禁止口令类参数进入 `ExecStart`
- [ ] 实现 uninstall：停止、禁用、移除 unit、重新加载，并保留数据目录
- [ ] 实现 start、stop、restart 与 status，含失败态与已停止态的可区分退出码
- [ ] 实现幂等：重复 install、重复 uninstall、start 已运行、stop 已停止的确定结果
- [ ] 在 jrps 与 jrpc 中接入 Linux 侧子命令，命令形态与 FR-29 一致
- [ ] 补齐日志：安装、卸载、启停与状态查询输出简体中文，含下一步提示
- [ ] 运行该 module 测试、`go vet` 与依赖门检查；在三平台 CI 中确保 Linux 侧代码可编译
- [ ] 同步 PRD、ARCHITECTURE、OPERATIONS、SECURITY、CHANGELOG 中受影响内容

## 5. 验收标准

自动化（可在 Linux 宿主机或具备 systemd 的环境中验证）：

- unit 文件写入 system unit 目录，服务名固定；重复 install 不产生第二 unit。
- unit 内容断言通过：`User` 为指定的现有非 root 用户且不等于 root；`Restart=on-failure`；存在有限退避的 `RestartSec` 与步数/上界指令；`TimeoutStopSec` 为 30 秒；`KillSignal` 为可被优雅处理的信号；`KillMode` 为进程级。
- 凭据断言通过：unit 全文与 `ExecStart` 中不含类口令、类 token 或通知秘密字段；尝试通过 install 传入这类参数时返回明确中文错误而非写入 unit。
- 数据目录：缺省值正确（jrps 为 `/var/lib/jrp/jrps`，jrpc 为 `/var/lib/jrp/jrpc`）；传入绝对路径时使用该路径；目录属主为服务用户且权限仅该用户可读写。
- 卸载保留数据：uninstall 前生成 SQLite 主文件、日志文件与一个正文分段文件；uninstall 后这些文件与数据目录均存在且内容可读，输出中提示数据保留路径。
- 幂等：连续两次 install 成功且只有一个 unit；连续两次 uninstall 成功；start 已运行与 stop 已停止不判为失败。
- status 的未安装、运行中、已停止三种状态有不同退出码与中文输出。
- 运行用户为 root 或不存在时 install 失败并给出明确中文错误，且不写入 unit。
- 安装失败后无残留 unit，再次 install 可成功。
- 该 module 在 Linux 上的测试通过；`go vet`、依赖门检查通过；Windows 与 macOS 上 Linux 侧代码不参与编译。

**真实 systemd 主机重启验收（需真实主机，需用户确认）**：

> 以下各项不能在容器内替代：容器中的 systemd 行为与真实开机流程不同，必须由用户在真实 Linux 主机上执行并确认。

1. 在真实 Linux 主机上以 root 权限为 `jrps` 执行 install，指定一个已存在的非 root 用户；确认 unit 写入 system 目录且开机已启用。
2. 执行 `start`，确认进程以指定用户运行（通过进程属主确认非 root），确认服务状态为运行中。
3. 为 `jrpc` 重复步骤 1 与 2，确认两个服务可并存且各自只有一个实例。
4. **执行真实主机重启**：重启后不登录任何会话，由用户确认两个服务均自动运行，代理能力可用。
5. 确认重启后进程属主仍为非 root 用户，数据目录内容与重启前一致。
6. 执行 `stop`，确认 30 秒内完成优雅停止；由用户确认停止后等待若干分钟进程未被自动拉起。
7. 模拟异常终止（杀死服务主进程），由用户确认服务在有限退避后自动恢复运行，且重启次数有上界。
8. 执行 `restart`，确认中间态可判定且最终为运行中。
9. 执行 `uninstall`，确认服务被移除，且 SQLite、日志、证书与正文数据目录内容保持不变。
10. 用户确认上述全部步骤通过后，FR-30 才可进入交付判定。

## 6. 风险与待定

- **风险**：管理员为省事直接用 root 运行，使最小权限形同虚设。缓解方式是 install 明确拒绝 root，并在 OPERATIONS 中写明应使用哪个现有用户。
- **风险**：30 秒对大量长连接排空偏紧，运维可能改用 kill 绕开优雅停止。缓解方式是让进程在该期限内自行决定收尾节奏，并在文档中说明超时后的强制终止后果。
- **风险**：unit 被运维手工修改后与本规格生成的内容漂移，重新 install 会覆盖。缓解方式是 uninstall/install 前提示差异，不在无人确认时静默覆盖。
- **风险**：不支持 systemd 的发行版上管理员仍期望自启动。缓解方式是明确返回不支持并引用范围外说明，不做静默降级。
- **风险**：启动限制窗口达到上界后服务停在失败态，管理员可能误以为"服务装坏了"。缓解方式是 status 明确区分失败态与已停止，并给出中文下一步提示。
- **待定**：退避的具体秒数、步数与启动限制窗口取值。当前只约束"有限且由 systemd 原生指令表达"，具体数值需在规模基线（200 客户端 / 2000 代理）场景下实测后定稿。
- **待定**：是否需要在 unit 中声明依赖的具体目标名称（网络就绪目标）。取决于目标发行版的 systemd 版本，需与实测结果共同确认。
- **待定**：日志是走 journald 还是文件。当前要求日志保留在数据目录且不因卸载被删，具体落盘方式需与日志规格（FR-12）对齐后定稿。
