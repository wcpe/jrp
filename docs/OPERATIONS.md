# 运维手册：JRP

> 本文描述 JRP 的部署、升级、备份恢复、回滚和排障边界。当前版本为 `0.1.0` 初始化工程骨架，未交付能力不得按本文目标步骤视为可用。

## 1. 部署

### 1.1 运行形态

P1 推荐单机运行 `jrps`，由其承载兼容数据入口、管理 API、嵌入式 Web、服务端 SQLite、正文分段和通知适配器。每台受管机器按需运行 `jrpc`，并持有自己的 SQLite。

不需要 PostgreSQL、Redis、消息队列或对象存储。P1 不部署数据节点或分布式控制面。

### 1.2 数据目录

为 jrps 和每个 jrpc 分配独立、仅运行账户可读写的数据目录。目录至少容纳：

- SQLite 主文件及 WAL/SHM 伴随文件。
- HTTP 正文压缩分段文件与索引一致性所需元数据。
- 必要运行日志和安全审计输出。
- 本地证书、证书指纹信任状态和凭证材料。

凭证和正文可能明文落盘。文件系统权限和备份介质是主要防线，详见根 `SECURITY.md`。

### 1.3 构建与命令

根 Taskfile 是跨平台命令真源，Makefile 只转发同名任务。工程骨架完成后使用：

```bash
task bootstrap
task workspace:sync
task test
task build
```

分别验证时使用 `task test:core`、`task test:jrps`、`task test:jrpc`、`task test:web`。`task build` 先构建 Web，再生成带 `webui` 标签的生产 `jrps` 和 `jrpc`；`task build:go` 无需前端即可生成使用回退页面的 `jrps` 与 `jrpc`。二进制统一输出到根 `bin/`，Windows 使用 `.exe` 后缀，版本均由根 `VERSION` 注入。

### 1.4 首次启动目标流程

1. 创建专用低权限运行账户与数据目录。
2. 生成或安装管理 HTTPS 证书；自签名场景记录并核对证书指纹。
3. 启动 jrps 并访问 `/healthz`、`/readyz`。
4. 执行单管理员初始化，保存恢复材料，不把凭证写入命令历史、日志或仓库。
5. 在 Web 中创建客户端和独立 enrollment/token。
6. 启动 jrpc，通过 HTTPS/WSS enrollment；核对服务端证书指纹。
7. 创建代理并观察 desired、active、last-good revision 与应用结果。

具体 CLI 和参数只有在对应 FR 交付后才可执行；数据目录、SQLite 路径和管理 API 根监听是引导参数，变更需要重启。

FR-09 已交付的引导参数：

- `jrps [--listen 地址] [--data-dir 目录] [--database 路径]`。
- `jrpc run [--data-dir 目录] [--database 路径]`。

`--data-dir` 缺省为 Linux `/var/lib/jrp/{jrps,jrpc}`、Windows `%ProgramData%\JRP\{jrps,jrpc}`；`--database` 缺省为数据目录下的 `jrps.db` 或 `jrpc.db`。两侧数据库互相独立，指向对方数据库文件或同一路径时进程拒绝启动并在日志中给出中文原因。

FR-02 的管理面 TLS 引导参数（已实现并通过实机验收，交付状态待 S2 阶段统一标记）：

- `jrps --tls-cert 证书 --tls-key 私钥`：同时提供即启用 HTTPS 监听，管理面 TLS 由 jrps 内建终止。
- 两个参数必须成对提供：缺一个即按启动失败处理，不会静默降级为明文——静默降级会让管理员误以为自己在用 HTTPS。
- 启动日志记录证书 SHA-256 指纹，自签名场景据此与浏览器提示的指纹逐段核对；证书文件不可读或不含 PEM 块时拒绝启动。
- 不提供这两个参数时管理面以明文 HTTP 监听（`--listen` 缺省绑定 `127.0.0.1`），该形态仅适用于本机开发。

FR-10 的引导参数重启清单（已交付，规格 §3.5）：

- 三项引导参数——数据目录、SQLite 路径、管理 API 根监听——变更需要重启，且必须在文档中标注；除此之外的全部 P1 配置项变更都映射到一次无中断应用流程（prepare → health-check → publish → drain），不存在"停止旧进程后重启进入新配置"的分支。
- 数据面控制入口属于**引导身份**：监听地址、端口、传输方式（`tcp`/`websocket`/`wss`/`kcp`/`quic`）与 WSS/QUIC 的证书、私钥任一变化都需要重启，应用流程会以「控制入口身份变化，需要重启」明确拒绝并保留旧 active。缺省端口 7200、缺省传输 `tcp`。
- 控制入口参数此前会被应用流程**静默忽略**（引擎只在启动时建立控制入口），现已改为显式拒绝，避免真源与运行态不一致。
- 控制监听端口被占用属于启动失败：进程拒绝启动并在日志中给出中文原因，不降级为"只有管理面没有数据面"的半可用状态。

### 1.5 健康检查

- `/healthz`：进程存活。
- `/readyz`：已接入的关键启动检查通过。

不得把 `/healthz` 成功等同于配置已应用或代理可用。生产监控还需观察控制会话数、代理状态、配置应用失败、工作连接、磁盘容量、正文容量、通知失败和 SQLite 写入延迟。

## 2. 升级

1. 阅读 CHANGELOG、PRD 状态和迁移说明，确认目标版本兼容性。
2. 备份 SQLite 主文件、WAL/SHM、正文分段和证书/指纹状态。
3. 在隔离环境执行 Core、jrps、jrpc、Web 测试和配置迁移演练。
4. 先升级 jrps，再按兼容矩阵滚动升级 jrpc；官方 frpc 继续按固定兼容契约验证。
5. 检查 `/readyz`、desired/active/last-good revision、客户端重连和代理流量。
6. 保留上一版本二进制与升级前备份，直到观察期结束。

应用配置使用无中断热更，但二进制升级不自动等于无停机。P1 单机 jrps 进程升级可能造成控制连接重连，必须在发布说明中如实标注；不得宣称已具备 P3 HA。

## 3. 数据备份与恢复

### 3.1 备份范围

一致性备份必须同时覆盖：

- jrps/jrpc SQLite 主文件与可能存在的 WAL、SHM。
- HTTP 正文压缩分段文件。
- 正文分段索引、保留状态和审计数据。
- TLS 证书、私钥和证书指纹信任状态。
- 版本信息与部署参数记录。

日志可按合规需求单独备份，但不得替代审计数据。备份中可能包含明文凭证和正文，权限不得低于生产数据目录。

### 3.2 备份方法

优先使用 SQLite 在线备份 API 或经过验证的事务一致性快照。若使用文件级复制，应先进入应用提供的备份协调流程或安全停写，确保主文件、WAL/SHM 与正文索引处于一致边界。不得只复制 `.db` 而忽略未 checkpoint 的 WAL。

正文分段与 SQLite 索引应共享备份批次标识或检查点。备份完成后校验文件清单、大小、校验和和可读性，并按组织策略加密备份介质；项目本身不提供静态加密保证。

### 3.3 恢复

1. 停止目标进程并隔离写流量。
2. 保存当前损坏现场用于排障。
3. 恢复同一批次的 SQLite、WAL/SHM、正文分段和安全材料。
4. 检查文件所有者与最小权限。
5. 以恢复版本启动，检查 SQLite 完整性和正文索引引用。
6. 验证 `/healthz`、`/readyz`、管理员登录、客户端重连、revision 与代表性代理。
7. 对缺失正文段执行可审计的孤儿/缺失处理，不伪造内容。

### 3.4 恢复演练

每个正式版本前至少在隔离目录恢复一次最近备份。演练需验证 SQLite 完整性、WAL 处理、正文分段可读性、证书/指纹状态、管理员登录和代表性代理；实机结果由用户确认。

## 4. 回滚

- **配置回滚**：读取历史 revision 内容，创建新的 desired revision，再走 prepare、health-check、atomic publish、drain；不得修改历史 revision 或直接篡改 active。
- **二进制回滚**：停止新版本，恢复上一版本二进制；若存在不可逆数据迁移，必须同时恢复升级前一致性备份。
- **Web 回滚**：Web 与 jrps 二进制一起发布，不能单独放置与 API 契约不匹配的静态资源。
- **失败原则**：若恢复/回滚验证不通过，保持服务隔离并继续修复，不以删除失败测试或跳过检查完成回滚。

## 5. 排障

### 5.1 客户端无法连接

- 核对传输类型、地址、端口、TLS/WSS 和证书指纹。
- 检查客户端 token 是否属于该客户端、是否已轮换或吊销。
- 检查 wire v1/v2 选择和兼容错误类别。
- 使用 request ID/客户端 ID 查询脱敏日志，禁止输出完整 token。

### 5.2 配置未生效

- 比较 desired、active、last-good revision。
- 查看失败发生在 prepare、health-check、publish 还是 drain。
- 检查监听冲突、参数校验和资源上限。
- 不通过重启掩盖非引导配置的热更缺陷。

### 5.3 代理中断或性能下降

- 按传输、代理类型、工作连接、并发、吞吐、延迟和错误率分层定位。
- 检查工作连接池、goroutine/连接泄漏、磁盘和 CPU。
- 确认 Core 热路径没有 SQLite、通知或无界正文缓冲。
- 与固定参考基线做同机同场景黑盒对比，不比较不同配置结果。

### 5.4 SQLite 写入异常

- 检查磁盘空间、权限、WAL、锁等待和批处理队列。
- 不手工删除 WAL/SHM；先停止写入并按 SQLite 一致性流程处置。
- 通知副作用必须来自已提交 outbox，不因数据库重试重复发送。

### 5.5 正文容量异常

- 检查采集是否仅对指定 HTTP 代理开启。
- 核对 30 天和 5 GiB 两个限制及清理任务状态。
- 比较 SQLite 索引与分段文件，处理孤儿段或缺失引用并记录审计。
- HTTPS 透传不存在可读正文，不应产生正文记录。

### 5.6 通知失败

- 区分 DNS、TLS、鉴权、超时、4xx、5xx 和速率限制。
- 检查重试次数、退避和死信状态，避免无限重试。
- 日志只显示目标掩码和安全错误摘要，不显示 SMTP 密码、Webhook secret 或消息中的敏感正文。

## 6. 官方 frpc 接入的实机验收批次

FR-03 的设备无关部分已由自动化门禁覆盖（黑盒矩阵、链路夹具、客户端离线观测）；**真实网络的部分只能在实际部署环境执行**，命令与回填项如下（执行器全部参数可查 `node scripts/compat/real-network.mjs --help`）。

### 6.1 前置条件

- 目标环境已运行 jrps，且控制入口按目标传输可达（`wss` 需证书链可从执行机验证；`kcp`/`quic` 需 UDP 可直通）。
- 已存在该客户端的代理条目，且其入口端口在服务端空闲。
- 自签证书情形需准备 CA 证书文件，并确认证书含目标主机名的 SAN。

### 6.2 第一步：只读预检

只探查执行条件，不启动客户端、不注册代理、不需要凭据：

```bash
node scripts/compat/real-network.mjs --server=<控制入口主机:端口> --transport=<tcp|websocket|wss|kcp|quic> --preflight-only
```

预检结论落 `.tmp/compat-real/<日期>/preflight/preflight.json`。**预检通过不等价于验收通过**，只说明具备执行条件。

### 6.3 第二步：正式批次

```bash
node scripts/compat/real-network.mjs \
  --server=<控制入口主机:端口> --transport=all --wire=both \
  --token=<数据面 token> --entry-port=<服务端代理入口端口> \
  --entry-host=<代理入口主机> \
  --tls-ca=<CA 证书路径> --tls-server-name=<证书中的主机名>
```

| 参数 | 说明 |
|---|---|
| `--server` | 控制入口 `主机:端口`，必填 |
| `--transport` | `tcp`/`websocket`/`wss`/`kcp`/`quic`，`all` 表示全跑 |
| `--wire` | `v1`/`v2`，`both` 表示两个都跑 |
| `--token` | 数据面 token，必填（预检模式不需要） |
| `--entry-port` | 服务端为该代理分配的入口端口，必填 |
| `--entry-host` | 代理入口主机；与控制入口不同主机时必填 |
| `--tls-ca`、`--tls-server-name` | WSS/HTTPS 自签证书时的 CA 与 SAN 主机名 |
| `--duration` | 每用例观察时长（秒，默认 20），观察期内断言心跳稳定 |
| `--entry-release-wait` | 用例间等待入口端口释放的上限（秒，默认 40） |

自动化断言：登录成功、代理注册成功、入口回显逐字节一致、观察期心跳稳定、客户端重启后重新登录。证据与报告落 `.tmp/compat-real/<日期>/<传输>-<wire>/`（`result.json` 与 `report.md`），**不进入版本库**。

### 6.4 需由执行者回填的项

`report.md` 已列出下列清单，自动化无法证明，必须人工观察后填写：

- 网络拓扑：是否经过 NAT／防火墙，NAT 类型与端口映射方式。
- 反向代理：升级/隧道是否成功，软件与版本，空闲断链行为。
- 传输质量：观测到的延迟、丢包、抖动与回环基线的差异。
- 跨运营商／跨地域：是否明显退化或重连。
- 跨平台：客户端与服务端的操作系统与架构。
- 其他异常：日志中的非预期错误（附片段）。

### 6.5 反向代理侧的两条硬要求（已用真实 nginx 1.28.3 实测）

反向代理可放在控制入口之前，但必须满足两点，否则表现为间歇掉线或握手中断：

1. **透传升级头**：WebSocket 传输要求反代显式转发 `Upgrade` 与 `Connection`，否则握手停在 400。

   ```nginx
   location /~!frp {
     proxy_pass http://127.0.0.1:7200;
     proxy_http_version 1.1;
     proxy_set_header Upgrade $http_upgrade;
     proxy_set_header Connection $connection_upgrade;  # 由 map 决定：有 Upgrade 用 upgrade，否则 close
     proxy_read_timeout 60s;                          # 见第 2 条
   }
   ```

2. **空闲读超时必须大于客户端心跳间隔**（官方默认心跳 30s，nginx 默认 `proxy_read_timeout` 60s）。实测对照：心跳 30s 时同一窗口内反代零超时、服务端会话事件恒为 1；把心跳调到 90s 后，nginx 在 60s 空闲处切断升级连接（`upstream timed out … while proxying upgraded connection`），官方客户端随即用**同一 runID** 自动重连，服务端侧表现为一次会话接管（会话事件 1 → 2）。即：心跳间隔调大时，必须同步调大反代的空闲超时，否则控制连接会被周期性重建。

**已实测的三种前置拓扑**（均由官方 frpc 与 jrps 真实运行）：

| 拓扑 | nginx 配置 | 结果 |
|---|---|---|
| L7 明文反代 | `server { listen 7201; location / { proxy_pass http://127.0.0.1:7200; … } }` | WebSocket 升级透传；websocket 传输 16/20 通过、4 项 blocked（`heartbeat-relay`、`work-conn-rejected` 依赖裸 TCP 帧，仅 tcp 传输下可执行）、0 失败 |
| L7 终结 TLS | `listen 7443 ssl;` + 证书，后端仍为明文 WebSocket | jrps 只监听明文 websocket，frpc 以 `wss` 连前置；登录与入口回显双 wire 4/4 通过 |
| L4 透传 | `stream { server { listen 7202; proxy_pass 127.0.0.1:7200; } }` | TCP 传输全部用例双 wire 20/20 通过 |

L7 终结 TLS 时客户端与服务端协议不同（frpc 用 `wss`，jrps 用 `websocket`），这是常见形态：证书只装在前置。若反代只做 L4 透传，则前端协议与后端一致。

### 6.6 批次内的已知边界

- **KCP 不在批次内**：官方 frpc v0.70.0 在 `transport.protocol = "kcp"` 下不发出任何数据报（三种配置组合零流量、对照官方 frps 亦无连接，`frpc verify` 判定配置合法），已按基线客户端限制登记；JRP 侧 KCP 由引擎端到端用例与 jrps 级登录覆盖。
- **客户端被强杀后的入口行为**：流传输（tcp/websocket/wss）约 0.5 秒内停止接受新连接；QUIC 需等会话空闲回收（约 30 秒，短于控制会话 90 秒失活窗口），期间新访客受配对中心暂存上限约束。
- **入口端口释放**：UDP 载体（KCP/QUIC）在客户端被强杀后释放慢于 TCP，因此用例之间默认等待至多 40 秒；短于该值会造成下一个用例的端口冲突误判。
