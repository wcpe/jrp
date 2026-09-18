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
