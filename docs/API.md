# 接口契约：JRP

> 本文是 JRP 管理接口的目标契约真源。工程骨架当前端点与尚未交付的 P1 契约分开描述；存在文档不代表已经实现。

## 1. 通用约定

### 1.1 接口分区

- `/healthz`、`/readyz`：工程骨架健康端点。
- `/api/v1`：单管理员 REST 管理接口。
- `/api/v1/events`：SSE 实时事件流。
- `/agent/v1`：jrpc enrollment、配置获取和 WSS 管理通道。

P1 不提供 `/node`、`/cluster`、节点注册、调度或分布式 RPC 端点，也不在请求/响应中预留节点字段。

### 1.2 编码与时间

- REST 使用 UTF-8 JSON；错误使用 `application/problem+json`。
- 时间使用 RFC 3339 UTC 字符串。
- ID 为服务端生成的不透明字符串，客户端不得解析结构。
- 字节数使用整数，持续时间字段明确使用毫秒或秒后缀。

### 1.3 管理认证与 CSRF

- Web 登录成功后使用 `HttpOnly`、`Secure`、合理 `SameSite` 属性的 Cookie session。
- 所有修改状态的管理请求必须验证 CSRF token；只读 GET 也不得产生副作用。
- P1 只有一个管理员，不提供角色或租户字段。
- 正文查看、导出和删除除会话/CSRF 外还必须写审计事件。

### 1.4 jrpc 认证

- enrollment 使用一次性或短期 enrollment 凭据换取客户端身份与独立 token。
- 后续 HTTPS/WSS 请求使用该客户端 token，不使用管理员 Cookie。
- token 只授权当前客户端的 desired state、回执和运行状态，不得访问其他客户端。

### 1.5 分页、过滤与并发控制

- 列表使用游标分页：`limit` 与 `cursor`；响应包含 `items` 和可选 `nextCursor`。
- 默认 `limit=50`，最大值为 200；非法游标返回 400 问题详情。
- 可变资源响应包含 `ETag`，修改请求使用 `If-Match` 或显式 `revision`。
- revision 不匹配返回 409，客户端必须重新读取，不允许最后写入静默覆盖。

### 1.6 幂等与事件

- 创建 token、应用配置等可重试操作接受 `Idempotency-Key`，同一主体与键在有效期内返回同一结果。
- SSE 事件包含 `id`、`type`、`occurredAt`、`data`，支持 `Last-Event-ID` 恢复。
- SSE 仅提供观察能力，不承载配置命令。

## 2. 错误约定

错误媒体类型为 `application/problem+json`：

```json
{
  "type": "https://jrp.invalid/problems/revision-conflict",
  "title": "配置版本冲突",
  "status": 409,
  "detail": "请求基于的配置版本已过期",
  "code": "revision_conflict",
  "requestId": "不透明请求标识"
}
```

字段约定：

- `type`：稳定的问题类型标识，不携带秘密。
- `title`：面向用户的中文摘要。
- `status`：HTTP 状态码。
- `detail`：可安全公开的中文说明，不包含堆栈、SQL、文件路径、内部地址或凭证。
- `code`：稳定机器码。
- `requestId`：用于关联脱敏日志。

常见状态：400 输入非法、401 未认证、403 无权限或 CSRF 失败、404 不存在、409 revision 冲突、413 请求或正文超限、422 配置语义无效、429 速率受限、500 内部错误、503 尚未就绪。

## 3. 当前工程骨架端点

### 3.1 存活检查

- **方法/路径**：`GET /healthz`
- **认证**：无
- **响应**：进程可处理请求时返回 200 和最小状态对象。
- **语义**：只表示进程存活，不表示数据库、监听器或配置已就绪。

### 3.2 就绪检查

- **方法/路径**：`GET /readyz`
- **认证**：无
- **响应**：关键启动检查完成时返回 200；尚未就绪返回 503。
- **语义**：工程骨架阶段只覆盖已接入的依赖，不虚构尚未实现的数据库或协议检查。

除上述端点外，以下 §4 和 §5 均为 P1 目标契约，只有对应 FR 验收通过后才视为已交付。

## 4. P1 管理 REST 契约

### 4.1 首次初始化

- **唯一入口**：`jrps init` 本地 CLI；P1 不提供远程 HTTP 初始化端点。
- **启用条件**：只允许在目标 SQLite 尚无管理员记录时执行；成功后该初始化路径永久关闭。
- **凭据输入**：默认通过交互式标准输入读取管理员密码，禁止把密码放入普通命令参数；自动化方式由对应功能规格定义受限标准输入或一次性环境变量。
- **原子性**：数据库迁移、管理员创建与初始化完成标记必须在同一事务中提交；失败不得留下可登录的半初始化状态。
- **重复执行**：已完成初始化时返回明确错误，不得重置管理员、覆盖密码或绕过现有会话安全策略。
- **CSRF 边界**：初始化发生在本机 CLI，不建立浏览器会话，因此不适用 Web CSRF；首次成功登录后所有修改请求仍按 §1.3 执行。

### 4.2 管理员会话

- `POST /api/v1/session`：验证管理员凭据并建立 Cookie session。
- `DELETE /api/v1/session`：注销当前会话。
- `GET /api/v1/session`：返回当前会话的脱敏信息和 CSRF 协议所需状态。

不得返回密码、密码派生材料、完整 Cookie 或服务端会话密钥。

### 4.3 客户端

- `GET /api/v1/clients`：列出客户端、连接状态和 revision 摘要。需管理员会话。**P1 不分页**（单管理员场景下客户端数量可控），因此不提供 `limit`/`cursor`，响应只有 `items`——与 §1.5 的通用分页约定此处不适用。
- `POST /api/v1/clients`：创建客户端。需会话与 CSRF，成功返回 201。客户端标识由服务端生成；响应同时返回**一次**独立 token 明文与**一次** enrollment 凭据明文，此后不可再取。
- `GET /api/v1/clients/{clientId}`：读取单个客户端详情。需会话；只返回掩码与元数据。
- `PATCH /api/v1/clients/{clientId}`：修改允许的管理元数据。（待交付）
- `POST /api/v1/clients/{clientId}/tokens:rotate`：轮换独立 token。需会话与 CSRF；响应返回**一次**新 token 明文，旧 token 立即失效。客户端已吊销时返回 **409**（客户端存在且可读，只是处于不可操作的终态；报 404 会把管理员引向"ID 写错了"）。
- `POST /api/v1/clients/{clientId}/tokens:revoke`：吊销 token 并使后续鉴权失败。需会话与 CSRF；吊销不可恢复，且同事务作废该客户端所有未使用的 enrollment 凭据。
- `POST /api/v1/clients/{clientId}/enrollment-credentials`：重新发行一次性 enrollment 凭据，成功返回 201。需会话与 CSRF。**客户端已吊销时返回 409**：向终态客户端发行凭据只会产出一张永远无法兑换的死凭据。

客户端响应字段：`id`、`name`、`maskedToken`（`****` 加摘要前缀）、`enrollmentState`、`connectionState`、`desiredRevision`、`activeRevision`。创建与轮换响应额外含 `token`，创建响应额外含 `enrollmentCredential`。

所有 token 响应只允许在创建、轮换与兑换时返回一次完整值，后续只返回掩码和元数据。**负向契约**：读取接口、日志、审计与问题详情均不得出现完整 token 或凭据明文；客户端 token 只用于 `/agent/v1` 管理通道，不能登录管理 API。

> 实现说明：`tokens:rotate` 与 `tokens:revoke` 的注册形态受 gin 限制——同一路径段不允许既有两个冒号通配符、也不允许通配符与静态段共存（注册时会 panic）。因此该组子动作由 catch-all 捕获后在处理器内切分，**对外 URL 与本节契约逐字一致**，客户端无需感知。

### 4.4 代理与配置 revision

- `GET /api/v1/proxies`、`GET /api/v1/proxies/{proxyId}`：查询代理。
- `POST /api/v1/proxies`：创建代理并生成新的 desired revision。
- `PATCH /api/v1/proxies/{proxyId}`：修改代理并生成新的 desired revision。
- `DELETE /api/v1/proxies/{proxyId}`：删除代理并生成新的 desired revision。
- `GET /api/v1/config-revisions`：查询 desired、active、last-good 与版本列表（最近 20 个，含 `revision`、`creator`、`origin`、`changeSummary`、`createdAt`；不含内容本体）。需管理员会话。
- `GET /api/v1/config-revisions?revision={n}`：查询指定版本的应用结果（`results`，逐阶段 `phase`/`succeeded`/`errorDetail`/`occurredAt`）。`revision` 非法返回 400。
- `POST /api/v1/config-revisions/{revision}:apply`：触发 prepare、health-check、publish、drain。需管理员会话与 CSRF。受理返回 202（`{"revision":n}`），应用在后台异步执行，最终结果经 `?revision={n}` 查询观察；引擎未装配返回 503。并发与过期的同步 409 语义见实现说明。
- `POST /api/v1/config-revisions/{revision}:restore`：以历史内容创建新的 desired revision（201 返回新版本号），不直接修改历史版本；写入 `restore_apply` 审计。版本不存在返回 404。需管理员会话与 CSRF。

应用请求成功受理可返回 202，最终结果通过资源状态和 SSE 事件观察。冲突使用 409，语义无效使用 422。

> 实现说明（FR-10 jrps 批次）：代理 CRUD 端点（`/api/v1/proxies`）随 FR-11 交付，本批 `/api/v1/config-revisions` 三个端点先行为可用。`:apply` 的过期检查在后台执行（与读取 desired 同一临界区），受理响应恒为 202，过期与并发失败经日志与应用结果观察；严格的同步 409 需编排层预检接口，随 FR-11 统一收紧。`:apply` 与 `:restore` 的注册形态受 gin 限制，由 catch-all 捕获后切分，对外 URL 与契约逐字一致。

### 4.5 状态、日志与指标

- `GET /api/v1/status`：服务、客户端、代理、连接与容量摘要。
- `GET /api/v1/logs`：按时间（`from`/`to`，RFC 3339）、等级（`level`，四级枚举）、组件（`component`）、事件名（`event`）与关联 ID（`clientId`/`proxyName`/`requestId`）过滤脱敏日志；`limit` 与 `cursor` 分页。未认证返回 401，非法参数返回 400 问题详情；查看动作写入审计（`log_view`，只含条件摘要与条数）。响应项字段：`id`、`occurredAt`（UTC RFC 3339）、`level`、`component`、`event`、`message`、`clientId`、`proxyName`、`requestId`、`revision`（可选字段为空省略）。请求日志随 FR-13 采集适配层接入（采集关闭时该通道为空）。
- `GET /api/v1/metrics`：供 Web 使用的结构化指标摘要；监控抓取格式另由实现 spec 明确。
- `GET /api/v1/events`：SSE 实时事件。

### 4.6 HTTP 采集

- `GET /api/v1/captures`：分页查询请求元数据，不默认携带正文。
- `GET /api/v1/captures/{captureId}`：查询单条元数据。
- `GET /api/v1/captures/{captureId}/body`：流式读取正文并记录审计。
- `DELETE /api/v1/captures/{captureId}`：删除索引与正文引用并记录审计。
- `GET /api/v1/capture-policy`、`PUT /api/v1/capture-policy`：管理默认关闭、30 天和 5 GiB 策略。契约与策略值归属见 `docs/specs/audit-and-retention.md`。
  - 读取需管理员会话；修改需会话与 CSRF（`X-CSRF-Token`），变更写入审计。
  - 响应字段：`captureEnabled`（布尔）、`retentionDays`（整数）、`maxTotalBytes`（整数，字节）、`auditRetentionDays`（整数）、`updatedAt`（RFC 3339 UTC）。
  - `PUT` 请求体必须提供全部四项策略值（不接受部分提交），越界或缺失返回 400 且不产生部分变更。
  - 边界：正文保留天数 1–365，正文总量上限 1 MiB–1 TiB，审计保留天数 30–3650。
  - 并发控制：策略是单行整体对象，`PUT` 用**最后写入生效**语义，暂不提供 `If-Match`/`ETag`。这与 §1.5 对可变资源的通用约定不同，是 P1 的已知简化：单管理员场景下并发覆盖的实际风险低，而变更审计完整记录前后值，事后可追溯。若引入多管理员或外部自动化写入，须补齐条件写入。

未采集、已清理或不可见的正文返回 404/410 的稳定问题码；API 不应把压缩分段文件路径暴露给客户端。

### 4.7 通知与审计

- `GET /api/v1/notification-targets`：列出通知目标，需管理员会话。响应为 `{"items": [...]}`，秘密只以 `maskedSecret` 掩码出现。
- `POST /api/v1/notification-targets`：创建目标，需会话与 CSRF；成功返回 201。目标标识由服务端生成。
- `PATCH /api/v1/notification-targets/{targetId}`：更新目标，需会话与 CSRF。**秘密留空表示保留原值**，避免管理员改名字时被迫重输密码，也避免"未填写即清空"。
- `DELETE /api/v1/notification-targets/{targetId}`：删除目标，需会话与 CSRF，成功返回 204。删除时该目标的在途待发送记录一并转入 `discarded`，不静默丢失。
- `POST /api/v1/notification-targets/{targetId}:test`：发送显式测试通知，不伪造业务事件，需会话与 CSRF。投递失败返回 502，且同样写入审计（结果记为失败）。
  - 请求体字段：`name`（必填）、`type`（`webhook` 或 `email`）、`enabled`（缺省为 `true`）、`secret`（只在创建或更新时接收）、`webhookUrl`、`smtpHost`、`smtpPort`、`smtpFrom`、`smtpTo`（数组）、`smtpSecurity`（`starttls` 或 `none`，缺省为 `starttls`）。
  - Webhook 地址必须使用 HTTPS，不得内嵌凭据；SMTP 收件人上限 20。
  - 响应字段：`id`、`name`、`type`、`enabled`、`maskedSecret`、`summary`、渠道配置字段、`createdAt`、`updatedAt`。**不返回明文字段 `secret`**。
  - 校验失败返回 400 且一次给出全部违规项；目标不存在返回 404；**目标已停用返回 409**（停用的含义就是不接收通知，业务投递路径同样拒绝停用目标，两条路径语义保持一致）；渠道未启用返回 503。被拒绝的测试通知同样写入审计，结果记为 `denied`。
- `GET /api/v1/notification-deliveries`：分页查询通知投递结果，需管理员会话。只读，不要求 CSRF。
  - 用途：供运维与 Web 通知页查看发送结果，尤其是失败终态的失败次数、最后脱敏错误摘要与停止时间（FR-15 §3.3、§5）。
  - 过滤参数：`targetId`、`eventType`（事件类型）、`status`（`pending`/`sending`/`sent`/`retrying`/`failed`/`discarded`，封闭枚举）、`stopped`（取 `true` 时只返回已停止重试的 `failed` 与 `discarded`，用于把"仍在重试"排除在"最终失败"之外）。
  - 过滤值必须是封闭枚举内的取值；非法值返回 400，不静默忽略。
  - 分页遵循 §1.5：`limit` 默认 50、最大 200；`cursor` 为不透明字符串，非法游标返回 400。排序为最新的记录在前。
  - 响应字段：`items` 数组（每项含 `id`、`eventId`、`targetId`、`eventType`、`status`、`attempts`、`lastError`、`nextAttemptAt`、`stoppedAt`、`createdAt`、`updatedAt`）与可选 `nextCursor`。`lastError` 在写入时已脱敏，不含堆栈、秘密或内部地址。
  - **负面契约**：不返回投递载荷原文的完整字段，也不提供删除投递记录的接口；投递记录由保留策略统一清理。
- `GET /api/v1/audit-events`：分页查询审计事件，需管理员会话。
  - 过滤参数：`from`/`to`（RFC 3339 时间范围，闭区间）、`action`（动作枚举）、`objectType`（对象类型枚举）、`result`（`success`/`failure`/`denied`）。
  - 过滤值必须是封闭枚举内的取值；非法值返回 400，不静默忽略。
  - 分页遵循 §1.5：`limit` 默认 50、最大 200；`cursor` 为不透明字符串，非法游标返回 400。
  - 响应字段：`items` 数组（每项含 `id`、`occurredAt`、`actorType`、`actorId`、`action`、`objectType`、`objectId`、`result`、`context`、`requestId`）与可选 `nextCursor`。
  - **负面契约**：P1 不提供审计导出接口，也不提供审计删除接口。审计事件不可被管理员经 API 抹除；写入后不可修改。

通知目标中的秘密只在创建/更新时接收，读取时必须掩码。

### 4.8 通知投递契约

通知内容由 outbox 在业务事务提交后投递，外部副作用不在事务内发生。

> **实现进度**：投递侧（渠道、重试、租约、发送循环）与写入侧均已就绪。已接入写入的业务动作：通知目标的创建、更新与删除，以及管理员登录连续失败达到限流阈值；写入与业务结果同事务。事件按广播投递给全部启用目标（订阅与分级路由属 P3），不发给被变更的目标自身。删除事件不指向目标——删除是硬删除，指向已消失的目标会让记录以"投递失败"收场。配置应用结果（FR-10）与代理变更（FR-11）的写入点随对应 FR 补齐。第 4.8 节描述的是目标契约。

- **Webhook 请求体**（UTF-8 JSON）：`eventId`、`eventType`、`occurredAt`（RFC 3339 UTC）、`payload`（已脱敏摘要）。
- **Webhook 签名**：目标配置了 `secret` 时携带 `X-JRP-Signature` 头，值为 `sha256=` 加请求体的 HMAC-SHA256 十六进制。未配置秘密时不发送该头（不伪造来源证明）。
- **响应归类**：2xx 为成功；429 与 5xx 判为可重试；3xx、4xx 判为确定性失败（本渠道不跟随重定向）。
- **邮件**：主题为 `[JRP] {事件类型} {RFC 3339 时间}`，非 ASCII 按 RFC 2047 编码；正文为 UTF-8 中文，只含脱敏摘要。
- **重试**：可重试失败按有界递增退避重新入队并叠加抖动，达到上限后进入失败终态并记录停止时间。确定性失败不消耗重试次数。
- **租约**：发送中记录带租约，租约到期后允许重新投递，用于进程崩溃后的恢复；抢占是原子条件更新，同一记录不会被并发投递。

## 5. P1 jrpc 管理契约

### 5.1 Enrollment

- **方法/路径**：`POST /agent/v1/enrollments`
- **认证**：不需要客户端 token——enrollment 凭据本身就是入场券。这也是 `/agent/v1` 下唯一无需 token 的端点。
- **请求**：enrollment 凭据、版本与平台摘要。客户端标识由服务端在创建时生成，不由客户端提交（避免伪造他人标识）。
- **响应**：客户端 ID、独立 token（**明文只此一次**）、管理 WSS 地址、服务端证书指纹信息和当前 desired revision 摘要。
- **错误**：凭据无效/过期/已使用、客户端已吊销、版本不受支持、速率受限。
  - 被拒绝的兑换写入审计（结果记为 `denied`），审计只记原因类别、不回显凭据值。
  - 凭据无效、已过期与已被使用返回**完全相同**的 401 问题详情，不区分原因——区分它们会让攻击者据此判断凭据是否曾经存在。并发兑换同一凭据只有一个成功，由存储层的条件更新保证。
  - 已吊销客户端的凭据一律被拒：吊销会作废该客户端所有未使用的凭据，否则它们就是一条绕过吊销的路径。
- **交付状态**：凭据的发行与兑换已交付（FR-07）；本端点的响应中「管理 WSS 地址、证书指纹、desired revision 摘要」三项取决于 FR-08 与 FR-26，**enrollment 限流与「客户端已绑定」拒绝**随 FR-08 补齐。

### 5.2 Desired state

- **方法/路径**：`GET /agent/v1/desired-state`
- **认证**：客户端 token。
- **条件请求**：支持 ETag/revision；无新版本可返回 304。
- **响应**：只包含当前客户端的不可变 desired state 和 revision。

### 5.3 应用回执

- **方法/路径**：`POST /agent/v1/apply-results`
- **请求**：revision、阶段、成功/失败、脱敏错误摘要、active/last-good revision。
- **约束**：重复回执幂等；客户端不得回报其他客户端或未知 revision。

### 5.4 管理长连接

- **方法/路径**：`GET /agent/v1/connect`，升级为 WSS。
- **用途**：服务端通知 desired revision 变化、token 状态与控制面心跳；客户端回报在线状态与应用进度。
- **边界**：不承载代理数据，不复用或扩展官方 frpc 数据消息。

## 6. 安全与审计要求

- 所有正文读取、token 管理、配置变更和通知目标变更必须审计。
- 管理接口不得返回完整秘密；问题详情不得泄露内部实现。
- 输入必须限制集合大小、字符串长度、端口、域名、URL、帧和正文大小。
- 破坏性接口必须具备明确对象、revision/ETag 和幂等边界。
