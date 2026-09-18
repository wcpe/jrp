## 变更说明
<!-- 说明改了什么、为什么；关联 PRD FR、功能 spec、ADR 或 Issue。 -->

## 类型
- [ ] feat 新功能
- [ ] fix 修复
- [ ] refactor 重构
- [ ] revert 回滚
- [ ] docs / chore / test / build / ci / 其他

## 影响范围
- [ ] Core
- [ ] jrps
- [ ] jrpc
- [ ] Web
- [ ] 管理 API / Agent API
- [ ] 协议兼容
- [ ] SQLite / 采集 / 通知
- [ ] 文档与治理

## 防漂移自检
- [ ] 方向一致：已阅读相关 PRD、ARCHITECTURE、PROTOCOL 与 ADR，未静默违背既定决策
- [ ] 独立实现：未复制、改写、链接或导入 frp 源码、注释、错误文案、目录或测试组织
- [ ] 架构边界：Core 未依赖 Gin、GORM、SQLite、Web、通知、CLI 或 apps module
- [ ] 协议边界：官方兼容数据协议与 jrpc 私有 HTTPS/WSS 管理通道保持分离
- [ ] 范围合规：未夹带 P2/P3 能力、节点空壳或猜测性抽象
- [ ] 测试：受影响的 Core、jrps、jrpc、Web 测试与静态检查通过
- [ ] 实机验收：需要官方 frpc、网络/NAT、浏览器或跨平台验收时已记录结果
- [ ] 安全：未泄露凭证、正文或隐私数据；明文存储风险表述真实
- [ ] 文档同步：受影响的 PRD、ARCHITECTURE、PROTOCOL、API、OPERATIONS、SECURITY 已更新
- [ ] 架构决策：如有长期取舍，已新增 ADR；取代旧决策时未修改旧正文
- [ ] CHANGELOG：用户可见变更已记入未发布段
- [ ] 提交规范：中文 Conventional Commits、无 AI 签名

## 验证证据
<!-- 列出执行的命令、关键结果和必须的手动/实机验收。 -->

## 破坏性变更与迁移
<!-- 如有对外 API、协议、配置、数据或运维方式的破坏性变更，写明影响和迁移；否则填“无”。 -->
