# 演进与维护指南

> 本文规定 JRP 文档如何随代码演进、ADR 如何迭代、新需求如何落地，目标是防止需求、架构、范围和实现漂移。

## 1. 黄金法则：文档即代码

- 文档与代码在同一仓库、同一次变更中一起修改。
- 一个变更没有把受影响文档更新到一致，就不算完成。
- 同一事实只保留一个权威来源，其他位置使用链接引用。
- 兼容性实现必须保持独立，不复制或导入 frp 源码、注释、错误文案和测试组织。

## 2. 文档地图

| 文档 | 权威内容 | 更新时机 | 入库 |
|---|---|---|---|
| `docs/PRD.md` | 需求、范围、优先级、状态与验收 | 需求增删改或交付时 | 是 |
| `docs/specs/功能名.md` | 命中检查项的功能工作规格 | 开发该功能时 | 是 |
| `docs/ARCHITECTURE.md` | 模块、数据、机制与依赖现状 | 架构变化时 | 是 |
| `docs/PROTOCOL.md` | Core 兼容协议与互操作契约 | 协议行为变化时 | 是 |
| `docs/adr/*` | 重大决策的原因与后果 | 做出或取代架构决策时 | 是 |
| `docs/API.md` | 管理与 Agent 对外契约 | 接口变化时 | 是 |
| `docs/OPERATIONS.md` | 部署、升级、备份、回滚与排障 | 运维方式变化时 | 是 |
| `SECURITY.md` | 信任模型与敏感数据风险 | 安全模型变化时 | 是 |
| `CHANGELOG.md` | 用户可见变更 | 每个用户可见变更 | 是 |
| `.tmp/实施计划.md` | 当前里程碑 | 实施期间 | 否 |

活文档长期维护并入库；过程计划和临时证据只放 `.tmp/`，不得提交。

## 3. ADR 生命周期

- 状态流转为“提议中 → 已接受 → 已弃用或已被取代”。
- 已接受 ADR 的决策正文不可修改。
- 决策变化时新增下一个编号的 ADR，在新 ADR 中说明取代关系；旧 ADR 只修改状态行并链接新 ADR。
- 编号永久递增，不删除、不复用、不补洞。
- 新技术、架构模式、长期边界或对已有决策的推翻必须写 ADR；日常实现细节不写 ADR。

理解当前系统优先阅读 `docs/ARCHITECTURE.md`；ADR 用于追溯“为什么”。

## 4. 变更工作流

```text
1. 更新 PRD：登记需求、阶段和状态
2. 通过 spec 检查：需要时创建功能规格
3. 对齐 ARCHITECTURE、PROTOCOL 和已有 ADR
4. 架构决策变化时新增 ADR
5. 测试先行并实现最小改动
6. 运行 Core、jrps、jrpc、Web 相关测试与静态检查
7. 同步 API、OPERATIONS、SECURITY 和 CHANGELOG
8. 完成实机/互操作验收后再标记交付
```

验证门必须包含受影响组件：Core 测试、jrps 测试、jrpc 测试、Web 测试；跨边界变更需同时通过全部相关测试。真实 frpc 互操作、网络/NAT、二进制运行或浏览器场景需要实机确认时，自动化测试不能替代用户验收。

## 5. 防漂移检查清单

- [ ] 改动可追溯到 PRD 需求或明确缺陷。
- [ ] 未复制、导入或链接 frp 源码；参考仓库与提交未漂移。
- [ ] Core 未依赖 Gin、GORM、SQLite、Web、通知、CLI 或 apps module。
- [ ] jrps 与 jrpc 未互相导入，官方兼容协议与 jrpc 管理通道未混合。
- [ ] 未提前创建 P2/P3 功能、字段、目录、接口或抽象。
- [ ] 新增/修改行为有测试，Core、jrps、jrpc、Web 中受影响部分全绿。
- [ ] 真实 frpc、网络、NAT、浏览器或跨平台验收已按需求执行并确认。
- [ ] PRD、ARCHITECTURE、PROTOCOL、API、OPERATIONS、SECURITY 与 CHANGELOG 已同步。
- [ ] 新架构决策有 ADR；取代旧决策时保留历史正文。
- [ ] 凭证、正文和隐私数据未进入源码、提交、日志或错误响应。
- [ ] `git diff` 已审查，无无关格式化、调试输出或过程文件。

## 6. 分支与提交

采用 GitHub Flow：

- `main` 始终保持可发布，改动经短生命周期分支和 PR 合入。
- 分支使用 `feature/`、`fix/`、`refactor/`、`hotfix/` 前缀。
- 回滚优先 `git revert`，不重写已推送历史。
- 根 `VERSION` 是产品唯一版本真源，构建把同一版本注入 `jrps` 与 `jrpc` 二进制；Core 作为可消费 module 另有独立 SemVer，真源为 `core/VERSION`（FR-28 建立前的过渡期由根 `VERSION` 表达，ADR-0011）。
- 提交信息使用中文 Conventional Commits，禁止 AI 签名；一次提交只做一件事且必须通过验证门。

示例：

```text
feat(protocol): 支持 wire v2 协商
fix(reload): 修复监听器准备失败时的资源泄漏
docs(architecture): 更新配置版本状态机
```

## 7. 测试与质量

- 修改功能代码前先运行相关测试，确认基线通过。
- 新增或修改业务逻辑必须同步测试，覆盖正常、边界和关键错误路径。
- 禁止注释、跳过或删除失败测试来获得绿色结果。
- 当前稳态门禁为 Go 的 `gofmt`、`go vet`、依赖边界检查与模块测试，以及 Web 的 Prettier、ESLint、TypeScript 类型检查与 Vitest。
- `golangci-lint`、`goimports`、`govulncheck`、`pnpm audit`、竞态检测和浏览器级测试属于后续增强，需单独批准、固定版本并接入同一 Task 入口后才能列为强制门禁。
- Core 依赖门、wire/加密状态机、传输、工作连接、NAT、热更、SQLite revision/outbox、采集旁路、脱敏和 Windows 文件语义属于高风险门。

## 8. 发布与可交付物

版本号只在正式发版时修改。发版需要：

1. Core、jrps、jrpc、Web 测试和静态检查全部通过。
2. Windows、Linux、macOS 构建验证通过。
3. `jrps` 与 `jrpc` 二进制版本一致，均来自根 `VERSION`；Core module 发版另打 `core/vX.Y.Z` 标签。
4. CHANGELOG 的未发布内容切为带日期的版本段。
5. 对应 FR 在全部验收通过后标记为 `已交付@版本`。
6. 需要实机/集成确认的项目已由用户确认通过。

P1 单机发布不得宣称具备数据节点、HA、RBAC、OIDC 或静态加密能力。

## 9. 功能规格检查

新增功能命中以下任一条件时，先复制 `docs/specs/_template.md` 创建规格：

1. 新数据模型、表或 Schema。
2. 新 API、命令、事件或配置项。
3. 改动跨越两个及以上模块。
4. 需要新增或取代 ADR。
5. 其他需求依赖该功能。
6. 涉及并发、事务、锁或状态机。

小型缺陷、纯重构或依赖升级通常不单独创建规格，但仍需测试、文档同步和 CHANGELOG 判断。

## 10. AI 协作

AI 代理与人类协作者遵守同一套 `.claude/rules/`。项目侧不安装 `.claude/skills/`；流程能力由使用者环境提供，仓库只保留项目特定规则和长期文档。
