# 功能规格：跨平台测试与二进制构建

> 状态：草拟 · 关联 PRD：FR-17 · 分支：feature/cross-platform-release

## 1. 背景与目标

JRP 要在 Windows、Linux、macOS 三种开发与目标环境中保持同一套工程约束：Core 是纯数据面库，jrps 是带内嵌 Web 的服务端外壳，jrpc 是无 Web 的客户端外壳。三者的测试与构建若只在某一个平台上验证，就会在另外两个平台上出现静默漂移——依赖门、构建标签、路径后缀、Web 资源嵌入这几个环节尤其容易只在"常用平台"上碰巧成立。

FR-17 要解决的问题是：把「三个 Go module 在三平台测试通过」和「两个二进制在三平台构建成功」变成可重复、可断言、由 CI 强制的门禁，而不是靠开发者记忆。当前工程已有基础：`scripts/build.mjs` 负责实际编译，`Taskfile.yml` 是跨平台命令真源，`.github/workflows/ci.yml` 已按 ADR-0008 在三平台运行 Go 任务、在 Ubuntu 运行前端与生产嵌入构建。本规格在此基础上补齐缺口并固定验收口径。

使用者是 JRP 维护者与发布执行者。所属阶段 P1（切片 S5）。命令真源与 CI 分工遵循 ADR-0008，Web 嵌入遵循 ADR-0006，本规格不重复其决策正文。

## 2. 需求

- `core`、`apps/jrps`、`apps/jrpc` 三个 Go module 在 Windows、Linux、macOS 上分别执行测试并全部通过。
- 构建产物为两个二进制：`jrps` 与 `jrpc`，统一输出到根 `bin/`，Windows 使用 `.exe` 后缀，其他平台无后缀。
- `jrps` 有两种构建形态，由构建标签区分：带 `webui` 标签时使用 Web 生产构建产物，不带标签时使用回退页面资源。两种形态都不改变管理 API 契约。
- `jrpc` 只有一种形态：不含任何前端资源与前端依赖。
- `task build` 先构建 Web，再生成带 `webui` 标签的生产 `jrps`，最后构建 `jrpc`；`task build:go` 不依赖前端，生成使用回退页面的 `jrps` 与 `jrpc`。
- 产品版本由根 `VERSION` 注入 `jrps` 与 `jrpc` 的构建信息；Core module 版本另由 `core/VERSION` 管理（ADR-0011），两条轨道不同步是合法状态，不得因版本不同步导致构建失败。
- 构建参数固定使用 `-trimpath` 与精简符号表的链接参数，保证产物路径不携带构建机信息。
- CI 分工：Ubuntu 运行 Web 格式检查、lint、类型检查、全量测试与生产嵌入构建；Windows、Linux、macOS 分别运行三个 Go module 的静态检查与测试，并构建两个二进制。
- 缺少 Web 生产资源时，带 `webui` 标签的 `jrps` 构建必须失败并给出明确中文提示，不得静默退化为回退页面。
- 产物必须可自证版本：两个二进制都能输出与根 `VERSION` 一致的产品版本。

- 范围内：
  - 三平台的 Core、jrps、jrpc 测试门禁。
  - 三平台的 `jrps`、`jrpc` 二进制构建（回退形态）与 Ubuntu 的生产嵌入构建。
  - 构建标签、产物命名与后缀、输出目录、版本注入方式。
  - Web 生产资源与回退资源两种嵌入形态的可断言差异。
  - CI 中两 Job 的职责边界与失败阻断。
  - 清理任务对 `bin/` 的覆盖。
- 范围外：
  - 交叉编译矩阵（在一台上产出其他平台产物）与 GOOS/GOARCH 组合矩阵。
  - 制品签名、公证、校验和清单、SBOM 与安装打包（MSI、DEB、PKG、Homebrew、Chocolatey、Scoop）。
  - 自动发布流水线、tag 推送与制品上传；实际发版需用户明确授权。
  - 容器镜像与容器化运行形态。
  - Core module 版本发布与 `core/vX.Y.Z` tag 流程（FR-28）。
  - `platform/service` module 的测试与构建门禁（FR-29 引入时另行接入）。
  - 性能基准与容量压测（属 NFR 性能门禁，由对应功能规格量化）。
  - P3 的数据节点、节点 RPC、选举、分布式锁、共享数据库、HA、多管理员、RBAC、OIDC。

## 3. 设计

### 3.1 构建入口与产物形态

构建只经过 `scripts/build.mjs`，它声明三个目标：

| 目标 | 模块目录 | 入口包 | 构建标签 | 产物 |
|---|---|---|---|---|
| `jrps` | `apps/jrps` | `./cmd/jrps` | `webui` | `bin/jrps`（Windows 为 `bin/jrps.exe`） |
| `jrps-fallback` | `apps/jrps` | `./cmd/jrps` | 无 | `bin/jrps`（Windows 为 `bin/jrps.exe`） |
| `jrpc` | `apps/jrpc` | `./cmd/jrpc` | 无 | `bin/jrpc`（Windows 为 `bin/jrpc.exe`） |

`jrps` 与 `jrps-fallback` 输出到同一文件名，代表同一二进制的两种资源形态，不能同时存在；先构建哪个就保留哪个，这一点必须在文档中如实说明，不得宣称一次构建同时产出两种 `jrps`。

带 `webui` 标签的目标在 Web 生产资源缺失时直接失败并提示先执行 Web 构建；不带标签的目标不依赖前端产物。

### 3.2 构建标签与资源嵌入

`apps/jrps/internal/webui` 用构建标签区分两个实现：生产实现嵌入 Web 生产构建目录，回退实现嵌入仓库内的回退页面目录。两者对外暴露同一文件系统入口，因此上层路由不变，测试可以对"两种形态都返回可用页面"做断言。

该标签区分是既有设计：回退形态用于纯 Go 场景与 CI 的快速通道，不代表生产形态，发布产物必须是带 `webui` 标签的 `jrps`。

### 3.3 版本注入与双轨

产品版本唯一文本真源是根 `VERSION`，由构建脚本读取并通过链接参数注入 `apps/jrps` 与 `apps/jrpc` 各自的构建信息变量。二进制通过 `version` 子命令或 `--version` 参数输出该版本。

Core module 版本另有唯一文本真源 `core/VERSION`，tag 形式为 `core/vX.Y.Z`（ADR-0011）。本规格只消费产品版本轨道：不得把 Core 版本并入产品版本注入，也不得以"版本必须对齐"为由阻断构建。FR-17 的验收中凡涉及版本，限定为产品版本。

### 3.4 命令真源与 CI 分工

根 `Taskfile.yml` 是跨平台命令唯一真源，`Makefile` 只转发同名目标，CI 调用 Task 任务而不复制命令（ADR-0008）。相关任务：

- `task test:core`、`task test:jrps`、`task test:jrpc`：分模块测试。
- `task test:jrps:webui`：带 `webui` 标签的 jrps 测试，验证生产嵌入形态。
- `task build:go`：不依赖前端，构建回退 `jrps` 与 `jrpc`。
- `task build:web`、`task build:jrps`、`task build:jrpc`、`task build`：生产路径。`task build:jrps` 的内部顺序是先 Web、再带标签测试、再构建。
- `task lint:go`：格式化检查、三个模块 `go vet` 与依赖门检查。
- `task clean`：清理 `bin/` 与前端产物目录。

CI 两个 Job：

| Job | 平台 | 内容 |
|---|---|---|
| `go` | ubuntu、windows、macos 三平台矩阵 | `workspace:sync`、`lint:go`、`test:go`、`build:go` |
| `web-production` | ubuntu | 前端依赖安装、`workspace:sync`、`fmt:check`、`lint`、`typecheck`、`test`、`build` |

三平台矩阵保证 Core、jrps、jrpc 在每个目标平台都能编译与通过测试，并产出两个二进制；Ubuntu Job 额外覆盖前端全量检查与生产嵌入构建。任一 Job 失败阻断合并。

### 3.5 平台差异处理

平台差异只出现在三处，且都由构建脚本或任务层吸收：Windows 的 `.exe` 产物后缀、Windows 上包管理器命令的调用方式、以及各平台 Node/pnpm 与 Go 工具链版本固定。Go 源码本身不得为构建引入平台专属分支或平台专属依赖。

### 3.6 依赖门

依赖门脚本在 Core 上检查禁止依赖（frp、Gin、GORM、SQLite、`apps` module），并禁止 jrps 与 jrpc 互相导入。FR-17 要求该门在三平台都通过：任一平台因依赖解析差异引入禁止依赖都视为失败。

## 4. 任务拆分

- [ ] 先写失败测试：断言三平台产物文件名与后缀（Windows 带 `.exe`）、带 `webui` 目标在缺少 Web 生产资源时非零退出并输出中文提示、`--version` 输出与根 `VERSION` 一致、两种 `jrps` 形态的路由返回不同资源但契约一致、依赖门在新增禁止依赖时失败（初始为红）
- [ ] 固化 `scripts/build.mjs` 三个目标的产物命名、后缀与输出目录行为，并补齐缺少资源时的中文失败提示
- [ ] 为 `jrps` 的两种形态各加一个可断言的测试入口（生产资源与回退资源）
- [ ] 补齐 `jrps` 与 `jrpc` 的版本输出路径，使 `version` 子命令与 `--version` 参数结果一致且都来自根 `VERSION`
- [ ] 在 CI 的 Go 矩阵 Job 中确保三平台都执行 lint、分模块测试与两二进制构建
- [ ] 在 CI 的 Ubuntu Job 中确保前端格式、lint、类型检查、测试与生产嵌入构建全链路执行
- [ ] 让 `task clean` 覆盖 `bin/` 与 `apps/jrps/internal/webui/dist`，并验证清理后 `task build` 可重建
- [ ] 在三平台各执行一次 `task test`、`task build` 与 `task build:go`，记录产物清单与版本输出
- [ ] 同步 PRD、ARCHITECTURE、OPERATIONS、CHANGELOG 中受影响内容，并把"产品版本与 Core 版本双轨"写入构建相关表述

## 5. 验收标准

自动化（CI 门禁）：

- Windows、Linux、macOS 三平台的 Go Job 全部通过：`workspace:sync`、`lint:go`、`test:go`、`build:go` 均成功。
- 三平台各自产出两个二进制，文件名与后缀断言通过：Windows 为 `jrps.exe` 与 `jrpc.exe`，Linux 与 macOS 为 `jrps` 与 `jrpc`，均位于根 `bin/`。
- Ubuntu 的 Web Job 通过：`fmt:check`、`lint`、`typecheck`、`test`、`build` 全链路成功。
- 带 `webui` 标签的 jrps 测试通过；不带标签的 jrps 与 jrpc 测试通过。
- 依赖门在三平台均通过：Core 依赖图无 frp、Gin、GORM、SQLite 与 `apps` module；jrps 与 jrpc 无互相导入。

构建形态：

- 缺少 Web 生产资源时，`jrps` 目标构建失败并返回非零退出码，输出中文提示要求先构建 Web；`jrps-fallback` 目标在同一状态下构建成功。
- 两种 `jrps` 形态启动后都能提供管理入口且 HTTP 契约一致，差异只体现在静态资源来源。
- `jrpc` 的构建输入与依赖中不含前端资源或前端依赖。

版本：

- 两个二进制输出的产品版本与根 `VERSION` 内容一致。
- Core 版本与产品版本不同步时构建仍然成功，构建脚本不读取 Core 版本。
- 产物构建使用 `-trimpath`，产物中不含构建机绝对路径。

边界与错误路径：

- 根 `VERSION` 为空或缺失时构建失败并输出明确中文错误，不产出半成品二进制。
- `task clean` 执行后 `bin/` 与 Web 内嵌产物目录被清理；随后 `task build` 可完整重建。
- 未知构建目标时脚本输出用法提示并退出，不进入编译。

实机（需用户确认）：

- 在 Windows、Linux、macOS 三台真实机器上分别执行 `task test` 与 `task build`，确认两个二进制可运行、`--version` 与各平台根 `VERSION` 一致，`jrps` 可启动并返回健康检查响应；其中 macOS 与 Windows 的结果依赖用户在本机确认，CI 结果不能替代本机确认。
- 在 Ubuntu 上确认 `task build` 产出的 `jrps` 带有 Web 生产资源而非回退页面，由用户在浏览器打开管理入口确认。

## 6. 风险与待定

- **风险**：生产嵌入构建当前只在 Ubuntu 的 CI Job 中验证，Windows 与 macOS 上的带标签构建未纳入门禁，可能出现标签相关代码只在一端被修改。缓解方式是标签两侧实现共用同一文件系统入口并由两侧测试共同覆盖，避免平台专属分支。
- **风险**：`jrps` 与 `jrps-fallback` 输出到同一文件名，误以为一次构建产出两种形态会导致发布回退页面版本。缓解方式是在构建输出信息中明确标注本次构建是否带 `webui` 标签。
- **风险**：三平台 Node/pnpm 与 Go 版本若不固定，依赖解析结果会漂移。缓解方式是沿用 CI 中已固定的工具版本，并在本地使用同一版本。
- **风险**：`bin/` 与前端产物目录若被误提交会造成仓库污染。缓解方式是清理任务覆盖这些目录，交付前核对工作区状态。
- **待定**：是否在 Windows 与 macOS 的 CI Job 中也执行一次带 `webui` 标签的构建，以验证构建标签的平台无关性；当前只有 Ubuntu Job 覆盖生产嵌入路径。
- **待定**：`platform/service` module 建立后是否并入三平台测试矩阵，由 FR-29 的规格决定接入方式，本规格不预先改变 CI 结构。
- **待定**：产物签名、校验和清单与打包形态属于发布工程范畴，当前未立项，不在本规格内承诺。
