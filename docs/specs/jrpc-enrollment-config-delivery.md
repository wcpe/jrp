# 功能规格：jrpc 注册与配置下发

> 状态：草拟 · 关联 PRD：FR-08 · 分支：feature/jrpc-enrollment-config-delivery

## 1. 背景与目标

官方 frpc 使用自己的配置、不支持 JRP 私有配置下发，管理员必须手工分发配置文件。JRP 自研客户端 `jrpc` 需要一条独立的管理通道完成注册（enrollment）、鉴权和版本化配置接收，让管理员只在服务端维护 desired state。

本规格定义 `jrpc` 从 enrollment 到应用回执的完整闭环，并明确一条硬边界：**JRP 私有管理能力只走独立 HTTPS/WSS 管理通道，禁止向官方兼容数据协议加入任何 JRP 私有管理命令**。官方 frpc 与 jrpc 共用兼容数据协议仅用于建立数据隧道。

使用者是运维 `jrpc` 的管理员与 `jrpc` 进程自身。属于 P1 阶段。管理通道契约见 `docs/API.md` §5，边界裁定见 ADR-0003，配置真源见 ADR-0004，无中断应用见 ADR-0005，本规格不重复其正文。

## 2. 需求

- `jrpc` 首次启动时以管理员发行的 enrollment 凭据向 `POST /agent/v1/enrollments` 注册，换取自己的客户端身份与独立 token。
- 服务端响应包含客户端 ID、独立 token、管理 WSS 地址、服务端证书指纹信息和当前 desired revision 摘要。
- enrollment 凭据是一次性或短期的；凭据无效、过期、客户端已绑定、版本不受支持与速率受限都必须有明确错误。
- 后续所有管理请求使用该客户端 token，不使用管理员 Cookie；token 只授权本客户端的 desired state、回执与运行状态。
- `jrpc` 经 `GET /agent/v1/desired-state` 获取版本化 desired state，支持 ETag/revision 条件请求，无新版本返回 304。
- 断线重连时 `jrpc` 携带本地已知 revision，服务端只下发必要版本；服务端不得在每次重连时全量重发未变化的版本。
- `jrpc` 把 desired state 持久化到自己的本地 SQLite 后，进入与服务端相同的 prepare、health-check、atomic publish、drain 流程，再通过 `POST /agent/v1/apply-results` 回报结果。
- 应用回执包含 revision、阶段、成功/失败、脱敏错误摘要与本地 active/last-good revision；重复回执幂等；客户端不得回报其他客户端或未知 revision。
- 管理长连接 `GET /agent/v1/connect` 升级为 WSS，用于服务端通知 desired revision 变化与 token 状态、客户端回报在线状态与应用进度；它不承载代理数据，不复用或扩展官方 frpc 数据消息。
- 重连采用受限退避：避免服务端重启或轮换后产生连接风暴。
- 冲突处理：当本地 revision 与服务端 desired 不一致、或服务端版本早于本地已知版本时，以服务端当前 desired 为准重新应用，并由服务端以新 revision 表达，客户端不得自行翻回旧版本。
- 客户端 token 明文与 desired state 存于 `jrpc` 本地 SQLite；进程命令行与服务注册不得包含 token。

- 范围内：
  - enrollment 全流程与错误分类。
  - 独立 HTTPS/WSS 管理通道上的 desired state 获取、条件请求与 304。
  - 断线重连、revision 携带、退避与状态重建。
  - 客户端本地持久化后进入与应用服务端相同的四阶段流程，并回报 apply result。
  - 管理长连接的用途边界与心跳。
  - token 隔离与冲突处理。
- 范围外：
  - **配置文件作为配置真源**：已被 ADR-0004 拒绝；本功能不读取、不生成、不导出 jrpc 配置文件，也不支持以文件覆盖服务端下发内容。
  - **向官方兼容数据协议加入任何 JRP 私有管理命令**：官方 frpc 与 jrpc 的兼容数据协议只做数据隧道。
  - 新增 CLI 管理命令：enrollment 相关动作由 jrpc 启动流程与管理 API 完成，不新增面向管理员的 token/enrollment 子命令。
  - 官方 frpc 的配置下发：官方 frpc 不参与本通道。
  - 多租户、跨客户端批量下发模板、按分组的分批灰度发布。
  - 数据节点注册、节点心跳、调度与故障转移。
  - 客户端到客户端的直接控制通道。

## 3. 设计

### 3.1 通道与边界

两条完全独立的通道：

| 通道 | 路径 | 承载 | 认证 |
|---|---|---|---|
| 兼容数据协议 | Core 兼容入口 | 数据隧道、控制会话、工作连接、访客与 NAT 时序 | 客户端自身的兼容协议鉴权 |
| 管理通道 | `/agent/v1` HTTPS/WSS | enrollment、desired state、apply result、在线状态 | 一次性 enrollment 凭据 → 客户端独立 token |

硬边界：管理通道消息不得进入兼容数据协议；兼容协议消息不得被用于承载管理命令。服务端在管理通道上不接受任何伪装成兼容协议消息的管理请求。这一条同时是架构红线（见 `.claude/rules/architecture-invariants.md` 第 3 节）。

jrpc 管理通道使用 HTTPS/WSS；自签名场景支持证书指纹固定，证书更换必须经显式信任更新。

### 3.2 enrollment 流程

```text
管理员在 Web 创建客户端 → 发行 enrollment 凭据
        │
        ▼
jrpc 启动（本地无身份）→ POST /agent/v1/enrollments（凭据 + 身份材料 + 版本与平台摘要）
        │
        ├── 成功 → 保存客户端 ID 与 token 到本地 SQLite → 记录当前 desired revision
        │
        └── 失败（无效/过期/已绑定/版本不支持/限流）→ 中文明确错误 + 退避重试，不自行伪造身份
```

- enrollment 成功后本地 SQLite 持有客户端身份、token 与已知 revision；后续启动不再 enroll，除非管理员重新发行凭据。
- 版本与平台摘要用于服务端判断支持性，不作为鉴权材料。
- token 摘要服务端的持久化形态见 FR-07 规格。

### 3.3 desired state 获取与断线恢复

- 轮询入口 `GET /agent/v1/desired-state` 携带 `If-None-Match` 或本地 revision；服务端无新版本返回 304，有新版本只返回该客户端的完整不可变 desired state 与 revision。
- WSS 长连接在 desired revision 变化时推送通知；客户端收到通知后拉取或接收版本摘要，避免长连接直接承载大配置体。
- 断线重连携带本地已知 revision 与客户端身份；服务端据此决定下发必要版本。
- 重连退避有上限并带抖动，避免服务端重启后集中重连；退避期间已有代理隧道不受影响。
- 长连接心跳用于在线状态与失效检测；心跳丢失按断线处理，不直接判定客户端下线并立即清理配置。

### 3.4 本地应用与回执

`jrpc` 收到新 desired revision 后的顺序固定：

1. 持久化 desired state 到本地 SQLite（ADR-0004 的客户端侧真源）。
2. 经 Core 配置构建入口得到不可变快照。
3. 走 prepare → health-check → atomic publish → drain（与 `jrps` 同一阶段语义，见 FR-10 规格）。
4. 每个阶段结束回报 apply result；重复回报同一 revision 与阶段幂等。
5. publish 前失败保留本地 active 与 last-good，并回报失败与安全可公开的错误摘要。

服务端校验回执的 revision 与阶段属于该客户端；越权或未知 revision 被拒绝并审计。服务端展示客户端侧的 active/last-good 与最近应用结果，供管理员排障。

### 3.5 冲突处理

| 情形 | 处理 |
|---|---|
| 本地 revision 早于服务端 desired | 拉取新版本并按流程应用 |
| 本地 revision 晚于服务端记录 | 以服务端当前 desired 为准；若内容不一致，服务端以新 revision 表达，客户端按新版本应用 |
| 服务端 desired 内容校验失败 | 客户端回报 422 类失败摘要，保留 last-good，不修改本地 active |
| 回执 revision 未知或不属于该客户端 | 服务端拒绝并审计，客户端重新拉取 desired state |
| token 被轮换或吊销 | 管理请求 401；客户端退避重试并在日志中给出中文原因，已有隧道不受管理通道失败影响 |

### 3.6 安全与可观测

- 管理通道不得返回其他客户端信息；问题详情不含堆栈、文件路径或内部地址。
- token 只经 Authorization 头或等价头传递，不进入 URL 查询串与日志。
- 所有 enrollment、下发、回执与鉴权失败都有中文日志与（服务端侧）审计事件，日志不含完整 token。
- 管理通道处理不得阻塞数据面；下发与回执的写入通过有界通道与批处理隔离。

## 4. 任务拆分

- [ ] 先写失败测试：用 A 客户端 token 拉取 B 客户端 desired state 被拒绝；回执未知 revision 被拒绝；伪造管理消息进入兼容协议被拒绝。
- [ ] 实现 `/agent/v1/enrollments` 与一次性 enrollment 凭据校验、已绑定拒绝与错误分类。
- [ ] 实现 jrpc 侧本地持久化客户端身份、token 与已知 revision。
- [ ] 实现 `GET /agent/v1/desired-state` 的条件请求、304 与只下发必要版本。
- [ ] 实现 `GET /agent/v1/connect` 的 WSS 长连接、心跳与 desired 变化通知。
- [ ] 实现 jrpc 重连退避、revision 携带与状态重建。
- [ ] 实现 jrpc 本地四阶段应用流程并与服务端共用阶段命名。
- [ ] 实现 `POST /agent/v1/apply-results` 的幂等回执与服务端校验。
- [ ] 实现 token 轮换/吊销后管理通道的 401 与客户端退避。
- [ ] 补齐证书指纹固定与自签名场景的处理路径。
- [ ] 运行 jrpc、jrps 测试、竞态检测与构建；同步 PRD、ARCHITECTURE、API、PROTOCOL、OPERATIONS、SECURITY、CHANGELOG 中受影响内容。

## 5. 验收标准

- 自动化：jrpc 用一次性 enrollment 凭据成功注册并得到独立 token；凭据二次使用被拒绝；对已绑定客户端重复 enrollment 被拒绝。
- 自动化：客户端 token 只能获取本客户端 desired state；用其他客户端 token 请求被拒绝且不泄露对方存在性。
- 自动化：无新版本时 desired state 请求返回 304；有新版本时只返回必要版本内容。
- 自动化：断线重连携带本地已知 revision，服务端不下发未变化版本；模拟连续断线 N 次后最终收敛到服务端当前 desired。
- 自动化：jrpc 收到新 revision 后写本地 SQLite、走完四阶段并回报 apply result；重复回报同一 revision 与阶段幂等。
- 自动化：本地 prepare 或 health-check 失败时，jrpc 保留本地 active 与 last-good 并回报失败摘要，已有隧道继续可用。
- 自动化：服务端下发的新版本在本地应用成功后，服务端能看到该客户端的 active revision 与最近应用结果。
- 自动化：token 轮换与吊销后，管理通道请求返回 401，客户端进入退避且数据隧道不受管理通道失败影响。
- 自动化：管理通道不接受任何以兼容数据协议形态承载的管理命令；兼容协议消息中不存在 JRP 私有管理字段。
- 自动化：日志、审计、问题详情与 URL 中均不含完整 token。
- 边界：enrollment 限流、超长版本摘要、未知代理类型、空 desired state、服务端重启期间的连接风暴均有确定行为。
- 实机（需用户确认）：在一台机器上启动 jrps 与一个官方 frpc、一个 jrpc；确认官方 frpc 仅用兼容协议建立隧道且不受管理通道影响；在 Web 中修改该 jrpc 的代理配置，确认 jrpc 自动接收新 revision、本地无中断应用并回报成功；随后断开管理网络一段时间再恢复，确认 jrpc 自动重连并补齐期间的版本变更；最后吊销该客户端 token，确认其管理通道失败而原有隧道在最后一次应用后仍按预期运行。

## 6. 风险与待定

- **风险**：管理长连接断开期间 desired 可能多次变更，客户端重连后需要处理版本跳跃。缓解方式是重连只取服务端当前 desired，跳过中间版本，并以 revision 单调性保证不回退。
- **风险**：错误配置被下发到全部 jrpc 会造成批量失败。缓解方式是 publish 前失败保留 last-good，且在 Web 上展示每个客户端的应用结果（FR-11）。
- **风险**：证书更换后指纹不匹配会导致客户端无法连接。缓解方式是显式信任更新流程与明确中文错误。
- **风险**：重连退避参数不当可能在服务端重启后造成瞬时压力。缓解方式是退避上限加抖动，并在压测中验证。
- **待定**：管理长连接推送与轮询的优先级（以推送为主、轮询为兜底）需在实现前定稿。
- **待定**：客户端上报运行状态与健康摘要的字段集合需与 FR-14 指标定义对齐后写入本文档。
