# 规则索引

本目录规则约束 JRP 仓库中的人类与 AI 协作，核心目标是防止架构、范围、决策、文档和质量漂移。项目规则优先于通用规则。

| 规则 | 约束 |
|---|---|
| [architecture-invariants.md](architecture-invariants.md) | Core、外壳、控制面、数据面与真源边界 |
| [scope-discipline.md](scope-discipline.md) | P1/P2/P3 范围与禁止空壳 |
| [decision-alignment.md](decision-alignment.md) | 改动前对齐 PRD、架构、协议与 ADR |
| [doc-sync.md](doc-sync.md) | 文档与实现同一变更同步 |
| [testing-and-quality.md](testing-and-quality.md) | 验证门、高风险测试和质量底线 |
| [static-analysis.md](static-analysis.md) | Go 与 Web 格式、静态分析和漏洞门禁 |
| [comments.md](comments.md) | 注释和日志语言 |
| [config-files.md](config-files.md) | 运行配置命名、注释与敏感项 |
| [git-commit.md](git-commit.md) | 中文提交与文档入库边界 |

完整演进流程见 [`../../docs/CONTRIBUTING.md`](../../docs/CONTRIBUTING.md)。项目侧禁止创建 `.claude/skills/`。
