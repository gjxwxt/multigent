# Multigent - Agent 开发与架构指南 (AGENTS.md)

本文档旨在为 AI Agent（以及开发者）提供 **Multigent** 项目的技术架构说明、构建方法、运行流程及协同开发规范。

---

## 0. Agent 运行入口与状态写回

开始任务时，先读取本文件、README 和与当前任务直接相关的源码与测试。
任务状态、决策和验收证据应写入公开且不含环境秘密的项目文档；临时调试输出、
部署地址、凭据、内部网络拓扑和私有运行记录不得提交到公开仓库。

事实优先级：实时 API、Git 和测试结果高于状态摘要；状态摘要高于旧 handoff。无法验证的内容必须明确标记为 `unknown`，不得写成事实。

完成任务前必须更新状态、记录验证证据，并在最终回复中说明仍未验证的边界。不得把现有未跟踪用户文件顺手加入提交。

## 1. 项目简介 (Project Overview)

**Multigent** 是一个面向团队的人机协作 Agent 操作系统（Human-agent collaboration infrastructure）。

它帮团队将 Prompt、工具、工作流及人工审核整合为一个协调协同的操作系统：
- **共享上下文 (Shared Context)**：统一管理 Workspace、Team、Role、Project、Task、Docs、Skills 及工具状态。
- **人机协同工作流 (Visual SOP & Workflows)**：可视化设计工作流，支持人工审核节点与 Agent 自动化任务交接。
- **RBAC 权限控制**：基于角色的访问控制，细粒度隔离用户与 Agent 的数据访问及工具权限。
- **沙箱隔离执行 (Sandbox Execution)**：Agent 运行在 Docker 隔离环境 (`ghcr.io/multigent/multigent/runtime-base`) 中，通过 `mga` 命令行工具安全地与宿主控制台交互。

---

## 2. 技术栈与架构设计 (Tech Stack & Architecture)

### 核心技术栈
- **后端 (Backend)**：Go (1.26+), SQLite (`modernc.org/sqlite` 纯 Go 驱动), Cobra CLI 框架, Gorilla WebSocket.
- **前端 (Frontend)**：React 18, TypeScript, Vite, Tailwind CSS, SVG/Lucide 图标库。
- **打入集成 (Embedding)**：前端静态资源通过 Go `//go:embed` 直接内嵌到 `multigent` 二进制文件中，单文件打包发布。
- **沙箱运行环境 (Sandbox)**：Docker 容器环境，内置 `mga` runtime CLI。

### 核心目录结构
```text
multigent/
├── cmd/
│   ├── multigent/          # 管理端 CLI、Server、Runtime Node Service 与 Workflow 自动化控制入口
│   └── mga/                # Agent 沙箱内使用的 Runtime CLI
├── internal/
│   ├── api/                # REST API 路由、Web 静态资源服务、Workflow Trigger、Identity Provider 及 WebSocket 处理
│   ├── db/                 # SQLite 数据库模型与数据访问层 (Audit, Runtime Nodes, Entitlements)
│   ├── sandbox/            # Docker 容器生命周期管理与镜像准备
│   ├── workflow/           # 工作流引擎、触发器 (Webhook/Cron/Event)、状态节点转换与分支逻辑
│   ├── rbac/               # 权限控制与角色管理
│   ├── secretbox/          # 敏感凭据/模型 API Key 加密存储
│   ├── imbridge/           # 飞书 (Feishu/Lark)、Slack 等 IM 平台对接
│   ├── worker/ & daemon/   # 异步 Worker、Heartbeat 心跳及定时调度器
│   └── builtins/           # 内置 Skill (如 multigent-admin-ops) 与默认模板
├── web/                    # React + Vite 前端控制台源码 (工作流画布、配额指示器、连接管理)
├── Makefile                # 项目构建、打包、部署与测试自动化脚本
├── README.md & INSTALL.md  # 详细说明文档
└── docs/                   # 项目深入设计、工作流账号集成与Getting Started文档
```

---

## 3. 构建与启动指南 (Build & Run Guide)

### 环境依赖
- **Go**: 1.26+
- **Node.js**: 20+
- **Docker**: 推荐安装并启动（Agent 运行与沙箱测试需要 Docker）

### 常用指令 (Makefile)

1. **一键构建完整应用 (嵌入 Web 控制台)**
   ```bash
   make build
   ```
   *该命令会自动安装前端依赖、构建 Vite 静态文件到 `web/dist`，并编译出二进制文件 `dist/multigent` 和 `dist/mga`。*

2. **启动 Web 控制台 (启动项目)**
   ```bash
   ./dist/multigent start
   ```
   *服务启动后访问：`http://127.0.0.1:27892`*
   - 初始管理员凭据由部署环境配置，不写入仓库文档。

3. **前端热重载开发 (Web Local Dev)**
   ```bash
   # 终端 1：启动后端 API 服务 (监听 27893 端口供 Vite 开发服务器反向代理)
   ./dist/multigent start --addr 127.0.0.1:27893

   # 终端 2：启动前端 Vite 热重载服务 (监听 27891 端口)
   make web-dev
   ```
   *说明：`make web-dev` 仅启动 Vite 前端 HMR 开发服务器，后端 API 服务需要单独在另一个终端启动。*

4. **预热 Docker 沙箱镜像 (Agent Run Prewarm)**
   ```bash
   ./dist/multigent sandbox prepare
   # 国内镜像源预热：
   ./dist/multigent sandbox prepare --region cn
   ```

5. **运行单元测试**
   ```bash
   make test
   ```

---

## 4. Agent 协同与修改规范 (Guidelines for AI Agents)

1. **修改 API 或后端逻辑时**：
   - 新增 API 路由请在 `internal/api/` 下添加，并确保接口鉴权与 RBAC 检查到位。
   - 若添加了控制台支持的参数或指令，请同步在 `cmd/multigent/` 下添加或更新 Cobra 命令。
2. **修改前端页面时**：
   - 页面代码位于 `web/src/pages/`，组件位于 `web/src/components/`。
   - 确保修改后通过 `make web` 校验 TypeScript 与 Vite 构建无报错。
3. **保持提交质量**：
   - 避免直接删改现有的单元测试与接口约定。
   - 修改完代码后，必须运行 `make test` 或 `make build` 进行验证，不可未经验证直接声明成功。

---

## 5. 安全红线 (Security Invariants — 违反即事故)

以下约定由真实事故沉淀而来，**任何修改不得绕过**；细节以源码、测试和公开架构文档为准：

1. **端点鉴权边界**：`publicMux`（server.go）上的路由完全无认证。新增有副作用的端点一律注册到带 `withTokenAuth` 的主 mux 并做 `checkProjectAccess`；必须暴露给预览 iframe 的端点，handler 内必须校验预览签名 token（`internal/api/preview_token.go`），写端点再加频率限制。唯一豁免：GET /api/v1/deploy-verify（只读回查，publicMux 专用签名自证边界，handler 内 HMAC 校验 + 限流，依据 withContextIngestAuth 先例）。
2. **凭据不落盘**：Git remote URL 持久化必须保持纯净（无 token）；凭据只在推送瞬时注入（credential-helper 或运行时注入）。Git 命令输出入库/返回前端前必须 redact（参考 `redactGitOutput`）。
3. **并发写保护**：预览 Copilot 写入受工作流节点写锁约束——判定用 `isTaskAtHumanReviewStep`（human_review 放行，查询失败 fail-closed 保持加锁），不能只看 `task.Status`。
4. **确定性基线**：任务派生必须基于不可变 `baseCommit`（SHA），继承前置任务的 `completionCommit`；禁止以 `origin/main` 的移动引用作为基线。

---

## 6. 核心机制速查 (Key Mechanisms)

改动以下区域前先读懂对应机制，避免破坏既有设计：

| 机制 | 位置 | 要点 |
|---|---|---|
| 预览引擎与租期回收 | `internal/preview/engine.go` | 30 分钟租期 + 后台 reaper 每分钟回收；`.multigent/runtime.json` 契约 fail-closed 校验 |
| Git Worktree 隔离 | `internal/gitworktree/worktree.go` | 项目锁串行化 git 操作；`sanitizeTaskID` 防路径穿越；快照失败必须阻断清理（防丢未推送工作） |
| 审核自动提交 | `internal/api/workflow_handlers.go` `commitAndPushReviewChanges` | 人工审核 approve 时收编 Copilot 工作区改动为 checkpoint commit；push 失败写任务评论告警，不静默 |
| 启动自愈扫描 | `internal/api/workflow_handlers.go` `recoverActiveWorkflowRuns` | 重启后 3s 自动恢复停在 agent 节点的 active run；永不自动恢复 human_review；150ms 节流 |
| 六大生命周期解耦 | `internal/api/`, `internal/gitworktree/`, `internal/preview/`, `internal/workflow/` | 任务、代码基线、Worktree、预览会话、容器和远程同步各自独立，禁止混用单一状态 |
| 工作流双层体系 | `internal/workflow/store.go` | 代码内置 `Templates()`（只读目录）→ 经 `POST /api/v1/workflows` 实例化落库才可供任务选用；改模板后必须重新实例化才能在 UI 生效 |
| 统一交付流水线 | 模板 ID `unified-delivery-pipeline` | 13 步闭环，核心是编码后的 Agent 初审闸门（独立 reviewer-agent、实测验证、`review_rounds` 三轮封顶）；发布步 CI 触发为 best-effort（无权限如实填 none） |
| CI/CD 基线与 ci_ready 闸门 | `internal/ciready/` + 模板 `.gitlab-ci.yml`（模板 1.1.0） | 确定性优先：补种与十项校验全是纯函数，Agent 在 init v2 的 `ci_ready` 步骤只执行 `mga ci ready`（引擎无系统步骤类型才借道 agent_task）；端点 `POST /api/v1/runtime/ci-ready` 以 HEAD SHA 流水线为客观证据；`apk add --no-cache` 即 fail（每 job 重下 docker CLI）；release tag 必须从含优化 yml 的 main 切 |
| 设计确认闸门与 OD 执行治理 | `internal/api/design_handlers.go`, `internal/api/od_client.go` | 继承澄清需求全文 + 框定 UI 原型角色；OD 项目唯一约束自动清理；OD 容器 `--read-only` 且单次硬封顶 16,384 tokens，严禁接入未调校的长思考链模型（`qwen3.8-27b` 必截断），默认使用实测兼容工具调用的 `glm-5.3-flash`（2.8k tokens 快速落盘） |

**runner tags 决策**：GitLab 的 `tags:` 字段参与 job 调度，必须与目标 runner 的注册标签一致；不要未经验证参数化或删除模板中的标签。不同部署环境的 runner 标签属于部署配置，不写入本项目的私有运维记录。

---

## 7. 部署与验证环境 (Deployment Context)

- 生产地址、部署主机、服务端口、凭据、容器名称和 systemd 路径属于部署环境配置，
  不得写入公开仓库。请使用部署环境中的私有 runbook。
- 修改代码后至少运行 `make test` 或 `make build`，并按目标环境的私有 runbook
  做行为级验证。
- 交叉编译产物写入 `dist/`、`work/` 或外部临时目录均可，但这些产物必须被
  `.gitignore` 忽略，不能作为源码提交。

<!-- aoci:begin -->
## AOCI 仓库认知

AOCI 为本仓库维护一个稳定、可版本化、可增量更新的仓库级认知层，供模型跨任务复用对系统的理解。

`aoci.txt` 是面向模型的结构化认知索引。它以每个受管理文件、数据库表或其他受管理对象一条独立 Entry 的方式，用符号标签与 F/R/A/S 语义表达对象的核心职责、重要关系、对外契约，以及理解或修改系统时必须知道的非显然约束和设计决策。

Header、目录段和全部 Entry 共同组成完整仓库索引，可以覆盖前端、后端、配置、数据库结构及其他受管理内容。受管理内容发生变化时，通常只需维护受影响的认知条目，不需要重新生成整个索引。

AOCI 提供系统架构、对象职责、重要关系、对外契约和关键约束的高密度视图。

### 工作原理

AOCI 采用“模型生成、模型读取”的认知闭环。

Header、Entry 和 Curation 语义的创作只按当前机器签发的 Plan 与实时 Guide 执行；由 Host 模型基于当前绑定证据独立完成。

Entry 的语义必须来自模型对真实证据的理解。不得仅依据路径、文件名、扩展名、AST、符号列表、依赖扫描、正则、固定模板或规则引擎推导、预填、拼接或改写索引语义。

对 Fresh Bootstrap，只按当前机器签发的 Plan 和实时 Guide 执行。当它们要求创作时，Host 模型创作 Root、Meta、Tag 和 F/R/A/S，提供 authoring-run 声明，并把它绑定到 Plan、Evidence 与完整 Candidate。不得要求 AOCI 填写 `origin=host_model`、制造 Receipt 或把程序生成的 Framework 当作语义。本文件不自行重建 Onboarding 流程。内部批次不是用户决策；只有遇到既有批准边界或真实的安全、漂移、CAS、Recovery 条件才停止。

### 最小使用入口

- `aoci_rules`：取得当前AOCI版本的会话运行合同。
- `aoci_overview`：建立或恢复本仓库的完整认知。
- `aoci_maintain`：受管理对象达到最终稳定状态后检查认知是否需要维护。
- `aoci_update_entry`：提交与当前证据和源码摘要绑定的完整语义更新批次。
- `aoci_report`：仅当当前布局和工具状态支持时，在证据不足、无法可靠生成语义时登记待办，不猜写。

其他MCP工具、CLI命令、参数和专项流程，以当前工具说明、Guide和 `--help` 返回内容为准，不在本文件中重复完整手册。

本区块只规定仓库接入、认知使用和收尾原则。`aoci_rules` 承载当前会话合同，Guide实时输出承载当前Plan的执行顺序与停点，工具Schema、Spec和Validator承载机器结构与判据；Prompt、Description、README和静态文档不能覆盖这些机器事实。

### 建立、生成和恢复认知

1. 每个新的 Agent Run 开始时，应先判断：

   - 本仓库是否已经存在可用的完整AOCI索引；
   - 当前上下文中是否已有与本仓库根、当前索引版本和当前AOCI服务相匹配，并且模型仍可可靠使用的完整仓库认知。

2. 仓库已经存在可用的完整索引，但当前Run没有可靠完整认知时，先调用 `aoci_rules`，再调用 `aoci_overview`。

   完整认知仍可靠时直接复用。局部不确定本身不要求机械重读系统全貌。

   本Run从已知Host上下文压缩恢复时（包括宿主注入的压缩摘要），必须把此前模型认知视为不可靠。压缩handoff不得保留或摘要正式Whole-Index，也不得保留或摘要任何Overview Header、Entry、Chunk、Challenge或Attestation正文；只能保留安全续接所需的receipt身份、未完成write或Recovery状态，以及立即重载指令。复制进handoff的Whole-Index语义或receipt不能证明恢复后模型的当前认知可靠。若当前上下文已无法可靠保留运行合同，先调用 `aoci_rules`。继续业务任务前，使用 `refresh_reasons=["context_compaction"]` 和新的 `refresh_event_id` 调用普通完整Whole-Index `aoci_overview`（不设置 `check_only` 或设为false）；不得使用 `check_only` 或认知probe。原样跟随每个 `next_cursor` 直到 `completed=true`，确认交付，并且只基于新交付正文提交一次Attestation。完成这次新的完整传输后，即使Attestation为partial或fail也消费该generation，并按既有合同继续source-bound任务，不再自动调用第二次Overview。

   AOCI可以针对 `context_compaction`、项目 `cognition_refresh_threshold` 下的机器 `semantic_threshold` 或主要 `phase_transition` 提供checkpoint与认知状态事实。只需要这些紧凑事实时使用 `check_only=true`；这些事实只向Agent提供建议，不替模型决定是否需要系统全貌。

   Agent显式调用普通 `aoci_overview`（未设置 `check_only` 或为false）时，只要能形成一致的CognitionSet，AOCI必须完整交付请求scope。不得因为已有receipt、阈值未达到或没有待处理刷新原因而抑制正文。正式认知Dirty或Stale时仍交付正文，但必须标记不可靠。存在未决恢复或无法形成一致snapshot时失败关闭，不返回混合正文。

   普通Overview返回 `continuation_required=true` 时，必须原样提交 `next_cursor` 并自动继续到 `completed=true`。不得询问用户、开始业务任务或给出阶段性系统结论。Host截断、缺块、重复、乱序、cursor失败、Index变化或`chunk_tokens`变化时停止本次认知链。Attestation完成前不得用Memory、源码、Spec、`aoci.txt`、历史会话、scope、search或Entry读取修补或补充Whole-Index认知。Challenge ordinal是正式Entry序列中的1-based位置；Header内容、注释、空行、Section/Overview/Chunk Marker、Receipt与Metadata均不计数，Chunk Receipt ordinal使用同一序列。Attestation必须原样回绑本次Challenge发布的当前`index_sha256`、`entry_sequence_sha256`与`entry_count`；旧Index、旧Entry序列、旧数量或旧Attestation均无效。完整链结束后只正式提交一次既有模型认知Attestation；同一响应只允许一次不改变语义答案的JSON Schema或字段格式修正。对象、Tag或F不匹配即失败且认知吸收不确定，不得语义重试或旁路补答。首次认知失败时还不得执行Root/Meta、Migration、全局布局或其他未重新绑定的系统级决策。上下文压缩刷新若传输完整、认知身份不变、治理对齐且没有Recovery或第三方冲突，即使Attestation为partial或fail也消耗该refresh generation，并继续原任务，不再自动重读Overview。`system_mastery_percent`只自评系统框架——架构、职责、强关系、稳定外部契约以及高熵安全和维护约束——不表示完整实现或运行实况知识；机器索引覆盖率必须分开。默认只向用户输出由本次真实覆盖率、Challenge、块数、Token和掌握度生成的规定成功或失败一句话。Host截断时提示用户把 `overview_delivery.chunk_tokens` 设置为更小的合法值后重新开始，不得自动修改。

   加法认知等级必须与严格证明字段分开解释。`delivery_verified`表示已加载Index且Host交付已确认，但完整认知验证仍未完成；应表达为“已加载且交付已验证”，不得描述为“没有认知”或“没有理解系统”。`cognition_verified`要求Attestation通过（Challenge至少80%的ordinal完全正确且对象身份至多失手一处），`cognition_governed`还要求治理对齐。通用完整读取失败句只用于真实交付故障。

   当Overview响应包含可选`cognition-state/v2`投影时，必须分别解释各维度。其Level止于`model_cognition_usable`；`strict_attestation_verified`、`governance_aligned`与`current_system_cognition_reliable`都是独立状态，绝不参与该Level。ordinal、对象身份、Tag或核心F不匹配可以导致严格Attestation失败，而模型认知仍然可用；不得仅凭这种不匹配就宣称模型没有理解系统。只有`current_system_cognition_reliable=true`允许无保留地声称当前完整系统认知可靠。投影缺失时继续使用上述Legacy解释。

   普通的只读审计、分析、检查、不修改代码或不提交、不push，不自动等于严格零写入，也不改变上述认知有效性判断。Codex Memory和历史Skill只能辅助恢复经验、用户偏好与调查方向，不能替代与当前仓库根、索引摘要、AOCI服务身份和认知范围匹配的当前认知收据；项目AGENTS和当前AOCI身份在AOCI状态上优先于历史Memory。

   只有用户明确禁止Ledger、元数据、`.aoci`运行资产及任何文件写入时，才按严格零写入处理。若必要的认知建立与该边界冲突，必须报告冲突并请求用户裁决或建议使用隔离副本，不得静默以Memory替代当前仓库认知。

3. 仓库没有可用的完整索引，或当前只有最小骨架、Header不完整、Entries未完成、必要Curation尚未裁决时，如果需要建立正式完整AOCI索引，先取得 `aoci_rules`，然后进入当前AOCI Guide。由Guide依据仓库真实状态决定下一阶段并完成必要安全步骤。

   `aoci_maintain` 不替代索引建立流程。

   不在本文件中自行重建或硬编码完整索引生成状态机。

4. 在长程任务中，模型负责保留当前认知收据并正确使用刷新门禁：

   - Host报告上下文压缩或模型已知系统全貌丢失时，执行上述强制 `context_compaction` 重载规则；AOCI不能自行推断Host事件；
   - 进入真正的主要阶段时声明 `phase_transition`，不得把函数、测试运行或小步骤当作阶段；
   - 在有用的稳定检查点通过 `check_only=true` 取得机器语义计数；
   - 除已知压缩的强制重载外，由Agent判断当前任务是否需要再次显式获取指定scope或完整Overview；
   - 在维护和对齐完成前，保留AOCI报告的Dirty或Stale可靠性状态。

### 任务收尾与认知维护

5. 纯只读问答、分析、版本核验，或没有产生受AOCI管理对象变化的任务，不需要调用维护工具。当前AOCI版本是任意`aoci_overview` check_only或`aoci_maintain`响应里的`cognition_receipt.mcp_service_version`；二进制路径是项目`.mcp.json`里的`command`，CLI不必在PATH上。

6. 发生受AOCI管理对象变化时，待其达到本次任务的最终稳定状态后，只调用一次 `aoci_maintain`。不要在每次中间修改后逐文件维护。

7. 若维护结果返回真实语义候选，Host 模型必须基于每个候选绑定的对象和必要证据，独立创作完整标签与F/R/A/S更新。通过 `aoci_update_entry` 一次提交当前机器签发批次的完整候选集合，同时原样保留每项 `source_sha256`、`candidate_id` 与对应domain批次身份。`max_entries`只限制单次请求和原子事务，不限制logical plan、Whole-Index或Managed Scope。`remaining`非零时，在当前批次成功Apply后重新调用Maintain并从新preimage继续；绝不能为满足transport上限缩减Index覆盖或自行截取返回批次。

   没有足够证据且当前布局支持 `aoci_report` 时，使用它而不猜测、套用模板或为消除待办而生成缺乏证据的认知。

8. 必须遵守工具返回的结构化状态和安全边界：

   - `repair_required`：只修复明确命中的候选，再重新提交当前机器签发的完整批次；
   - `stopped`：结束当前写入尝试并检查 `failed_step`、错误、正式写入证据与Recovery。auto模式下，已证明零写入则记录closure并重新Plan；完整Intent和可证明postimage则Resume；策略要求Rollback且preimage可证明则精确恢复后重新Plan。只有证据不足、第三方正式字节冲突、需要审批或外部动作，或命中其他真实安全边界时，才停止整个用户任务；
   - 冲突、审批、人工裁决、权限和安全信号不得忽略；
   - 已经对齐后不得重复维护或重复写入；`refresh_ready_for_overview` 是checkpoint事实，由Agent决定是否为下一阶段请求普通完整Overview。

   维护完成后如果又修改了任何受管理对象，之前的维护结果失效，应在新的最终稳定状态重新完成收尾。

9. 用户只限制业务文件范围，但没有明确禁止仓库托管资产时，AOCI托管资产可以在收尾阶段为保持认知一致而更新，并应在审计和提交中与业务文件区分。

   用户明确禁止修改 `aoci.txt`、`.aoci`、元数据或任何额外文件时，以用户限制为准，不得写入，并如实报告剩余不一致。

### 专项流程

初始化、完整索引生成、Header生成、Entries生成、数据库结构索引、Curation、人工评审和故障恢复，只按当前AOCI Guide或工具在对应阶段返回的指令、命令和安全停点执行。

不预加载、不猜测，也不自行重建这些专项流程。平台调用方式、请求格式、批次上限、审批规则、索引格式细节和恢复步骤由对应Guide、工具说明、模型Prompt和CLI帮助按需提供。
<!-- aoci:end -->
