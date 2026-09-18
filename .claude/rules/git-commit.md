# Git 提交规范

## 1. 授权

- 只有用户在当前对话明确要求时才执行 `git commit` 或 push。
- 禁止未经授权使用 worktree、恢复用户修改或改写提交。
- 禁止跳过 hooks；禁止对已推送提交 amend 或强推主分支。

## 2. 提交信息

标题和正文必须使用简体中文；Conventional Commits 的 type 与 scope 使用英文小写。

格式：

```text
type(scope): 中文描述
```

允许的 type 包括 `feat`、`fix`、`refactor`、`docs`、`chore`、`test`、`build`、`ci`、`perf`、`style`。scope 使用 `core`、`protocol`、`transport`、`jrps`、`jrpc`、`web`、`api`、`capture`、`notification`、`build`、`ci`、`docs` 等实际模块或能力域。

正文说明为什么改和关键取舍，不逐行复述差异。禁止任何 AI、工具、协作者自动生成签名或尾注。

禁止用阶段词代替具体改动，例如“完成 P1”“MVP 第一阶段”“本次迭代”。提交应描述具体功能或修复。

## 3. 最小提交粒度

- 验证门通过才提交；需要实机/互操作验收时，用户确认前不得声称完成并提交。
- 每个提交独立可测试、可构建，只对应一个功能、修复、重构或文档主题。
- 不混入无关格式化、临时调试、过程文件、凭证或大型二进制。
- 暂存时按文件精确添加，先检查所有 staged 与 untracked 文件。

## 4. 文档入库边界

应入库：README、CHANGELOG、SECURITY、PRD、ARCHITECTURE、PROTOCOL、API、OPERATIONS、ADR、功能规格、CONTRIBUTING、项目规则和正式 GitHub 模板。

严禁入库：`.tmp/` 下实施计划、探索笔记、抓包、临时脚本、过程报告、AI 助手笔记；`.env`、凭证、SQLite、WAL/SHM、正文分段、日志、覆盖率、构建产物和依赖目录。

## 5. 提交前检查

1. 运行受影响的 Core、jrps、jrpc、Web 测试与静态检查。
2. 运行 `git diff` 审查每一行改动。
3. 检查 Core 依赖门和独立兼容边界。
4. 检查文档同步、CHANGELOG 和版本真源。
5. 检查暂存区没有用户无关修改或敏感文件。
