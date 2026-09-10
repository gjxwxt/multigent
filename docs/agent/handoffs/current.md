# Current handoff

## Start here

Branch: `feat/chatops-live-card-and-d6`.
Status: Phase 1 安全缺陷闭环修复、ChatOps 本地级联清理与 Phase 2 企业级模板均已完成并通过全量验证，End-to-End Pilot 生命周期及伪造输入防御测试全绿。

Key status & deliverables:
1. **ChatOps Channel Automation & Cascade Cleanup (产品边界明确)**:
   - Strictly scoped to IM instance; 409 conflict intercept prevents channel hijacking; link mode strictly finds existing channels (404 if missing).
   - Bot channel invite failures tracked (`status: error`, target suppressed); partial failures recoverable via UI retry.
   - Added `GET /api/v1/projects/{name}/channels` and project settings ChatOps status card with retry recovery button.
   - **级联清理产品边界明确 (Cascade Cleanup & Product Boundary)**：
     - `internal/api/delete_handlers.go` 中项目删除时自动级联清理本地 `project_memberships`、`project_channel_links` 与 `agent_channel_bindings`（由 `TestDeleteRoleTeamAndProjectRequireWorkspaceAdmin` 严格验证）。
     - **产品设计边界声明**：级联清理仅删除 Multigent 本地关联记录与授权关系，**明确保留 Mattermost 远端真实频道供安全审计与历史追溯**，避免误删导致外部团队沟通记录丢失。
2. **Project Initialization Alignment & Acceleration**:
   - Fixed AgentDir vs workspace split; single materialization prevents file duplication; Docker sandbox aligned with workspace root.
   - Initialization workflow v5 streamlined to 3 deterministic steps (`ready` -> `sync` -> `ci_ready`), cutting run duration from 38m to <3m.
3. **Phase 1 设计快照与 QA 门禁（3 项 P0 + 2 项防御加固已彻底闭环）**:
   - 流程拓扑已调整为前置 QA 门禁（`code_review -> qa -> qa_signoff -> pr_open_and_merge -> release`），防止坏代码进 `main`；
   - 契约字段在自审、代码审、QA 审与返工边之间传递；
   - **✅ 已闭环的 P0 安全修复与深度防御**：
     1. **审批绕过入口彻底关闭（含伪造路径防御）** (`internal/api/workflow_handlers.go:664`)：
        - 服务端在进入设计闸门审批判定时，强制 `delete(outputs, "approved_design_snapshot_path")` 与 `delete(outputs, "approved_design_html")`，绝不信任客户端传入的快照路径与内容，只能由服务端抓取 OD 成功后写入；
        - 空项目 ID、无快照且无豁免理由时 fail-closed 返回 400 Bad Request（`TestDesignReviewApprovalFailsClosedWithoutReferenceOrWaiver`）；
        - 伪造 `approved_design_project_id` + 伪造 `approved_design_snapshot_path` 均强制 fail-closed 返回 400（`TestDesignReviewApprovalFailsClosedWithForgedSnapshotPath` 验证通过）。
     2. **跨项目任务 ID 越权读取快照已拦截** (`internal/api/design_gate_snapshot.go:177`)：引入 `projectTaskResourceGuard`，校验调用者访问权的同时严格校验 `taskId` 必须归属于当前 `projectName`，跨项目越权直接返回 404（`TestDesignSnapshotPreviewEndpoint` 验证通过）。
     3. **同源 HTML 原型 XSS 彻底隔离** (`internal/api/design_gate_snapshot.go:246`)：快照静态资源端点强制注入严格 CSP 沙箱响应头（无 `allow-same-origin`）：`Content-Security-Policy: sandbox allow-scripts allow-forms; default-src 'self' data: blob: https: 'unsafe-inline' 'unsafe-eval'; frame-ancestors 'self'`，配合 `X-Content-Type-Options: nosniff` 与 `Referrer-Policy: no-referrer`，使得原型在不透明 `null` origin 中执行（`TestDesignSnapshotPreviewEndpoint` 验证通过）。
     4. **快照写入路径安全与原子发布** (`internal/api/design_gate_snapshot.go:141`)：
        - 严格校验每个 OD 工件文件路径为纯净相对路径，任何 `..` 目录遍历尝试均被阻断拦截（`TestCaptureDesignGateSnapshotRejectsPathTraversal` 验证通过）；
        - 使用临时目录（`os.MkdirTemp`）完整抓取写入所有文件和 `manifest.json` 后，再通过 `os.Rename` 原子发布到正式快照目录，杜绝半成品快照目录残留。
     5. **QA 风险-覆盖矩阵严格校验** (`internal/api/workflow_handlers.go:825`)：
        - 强制校验矩阵中每项必须有非空且唯一的 `item_id`（空 ID 或重复 ID 均返回 400）；
        - 严格校验 `manual_waivers` JSON 格式，解析失败直接硬失败返回 400，拒绝静默忽略；
        - 严格校验 `risk_level`（high/medium/low）与 `status`（passed/failed/blocked/waived/unexecuted/skipped）枚举合法性，拒绝未知枚举；
        - 拒绝无证据的高风险通过项：`risk_level == "high"` 且 `status == "passed"` 时必须具备非空 `evidence`（`TestQASignoffGateValidation` 全部 9 项子用例全绿验证）。
4. **Phase 2 企业级全栈工程模板（已验收通过）**:
   - 现代企业级样板：Java 21 + Spring Boot 3.3.3 + Gradle + React 18 SPA + Vite + Tailwind CSS。
   - **严格禁止依赖锁文件静默降级**：
     - `Makefile` 的 `install` 目标显式要求 `web/package-lock.json`，缺失时直接退出报错，杜绝重新引入未锁定依赖。
   - **POSIX 权限与 Gradle Wrapper 确定性内置**：
     - `templateFileMode` 强制 `gradlew` 与 `*.sh` 拥有 `0755` 执行权限；
     - 内置并 Git 追踪 Gradle 8.9 distribution wrapper jar（`server/gradle/wrapper/gradle-wrapper.jar`）。
   - **真实自动化测试与 TDD 规范落地**：
     - 前端补齐 RTL + Vitest（7/7 通过），修复全局 `globalThis.fetch` 保证 `tsc -b --noEmit` 0 报错；
     - 后端提供 MockMvc 切片测试、异常统一拦截与 JUnit 5 隔离测试；
     - 两个内置模板（`react_spring_boot` 与 `react_go_fullstack`）均配齐 `AGENTS.md`、`CLAUDE.md` 与 `docs/`（`architecture.md`, `api-spec.md`, `tdd-guide.md`）。
   - **自动化受控 CI 集成测试**：
     - `TestReactSpringBootMakeInstallFailsWithoutLockfile`：验证缺少 lockfile 时 `make install` 立即阻断。
     - `TestReactSpringBootControlledCIIntegration`：实测执行 `Materialize` -> `make install` -> `make verify`（含 `doctor`, `lint`, `test`, `build`），全程自动化跑通。
5. **Phase 3 End-to-End Pilot 全链路验证 (`TestPilotGreenfieldDeliveryPipelineFullLifecycle`)**:
   - 全程演练 11 步 Greenfield Delivery Pipeline（需求澄清 -> 需求快审 -> 设计确认闸门 -> 实现编码 -> Agent 初审 -> 人工代码审核 -> QA 测试与风险矩阵 -> QA 准出签核 -> 开 PR 并合并 -> 首版发布 -> 上线确认）；
   - 覆盖设计闸门空输入阻断、伪造快照路径输入阻断以及特批豁免放行；
   - 覆盖 QA 准出闸门高风险未测项拦截与 per-item manual waiver 特批放行；
   - 覆盖项目删除时本地 `project_channel_links` 与 `agent_channel_bindings` 的级联清理断言。
6. **统一项目创建载荷与原子级联初始化 (Unified Project Creation & Atomic Provisioning)**:
   - 后端 `POST /api/v1/projects` 原生支持一体化载荷：接收 `name` (Key)、`description`、`workerIds`、`memberUsernames` 及 `channel` 配置；
   - 原子化完成项目脚手架建立、Agent Team 成员绑定（`project_memberships`）、工作区成员权限同步及 ChatOps 频道接入；若频道创建遭遇冲突或异常，自动回滚已建项目与本地关系，杜绝僵尸项目并支持原地修正重试；
   - 前端新建项目弹窗（`CreateProjectDialog`）交互重构：将项目标识 `Key`（严格要求 `[a-zA-Z0-9-_.]`）与业务名称 `Description`（支持中文业务描述并在卡片展现）清晰区分，并配齐实时正则校验反馈；
   - API 异常透传（`web/src/lib/api.ts`）：`localizedAPIErrorMessage` 优先展示服务端具体的校验与业务原因，彻底消除因通用 `validation_failed` 掩盖真实输入错误的问题。

## Non-negotiable boundaries

- QA 门禁必须严格位于主干合并之前（`code_review -> qa -> qa_signoff -> pr_open_and_merge`）。
- 模板依赖安装必须 100% 依赖确定性 lockfile，严禁在 lockfile 缺失时自动降级为动态拉取。
- 快照文件提供端点必须校验任务所属项目，且对原型 HTML 内容实行沙箱隔离。
- `approved_design_snapshot_path` 和 `approved_design_html` 严禁信任客户端请求输入，必须由服务端抓取生成。
- `gradlew` 必须保持 `0755` 权限且必须内置 `gradle-wrapper.jar`。
- 项目标识 `Project.Name` 严格受 `validateWorkspaceObjectName` 约束，只能包含英文、数字、`-`、`_` 与 `.`，严禁包含中文与空格；若包含频道开通配置，开通失败必须原子回滚项目。
- **工作流人机角色严格解耦**：`agent_task`（自动化步骤）与 `human_review`（人工审核闸门）严禁复用相同的 `actorRole`（如禁止同时使用 `owner-engineer`）。自动化步骤统一使用 agent 后缀角色（`developer-agent`、`reviewer-agent`、`release-agent`、`qa-agent`、`pm-agent`），人工审核闸门统一使用责任人角色（`owner-engineer`、`product-owner`、`qa-owner`）。

## Evidence

- `internal/api/project_handlers_test.go`:
  - `TestHandleCreateProject_Validation`: PASS (非法字符、中文字符、空格与前缀点严格拦截，0 漏放)
  - `TestHandleCreateProject_UnifiedPayload`: PASS (验证单次请求原子完成项目创建、Agent Worker 绑定及工作区成员授权)
  - `TestHandleCreateProject_WithChannelProvisionAndRollback`: PASS (频道创建冲突时自动回滚项目目录与全部 DB 授权关系)
- `internal/api/pilot_greenfield_test.go`:
  - `TestPilotGreenfieldDeliveryPipelineFullLifecycle`: PASS (0.33s，11 步全流程、伪造路径拦截、安全闸门与删除清理全绿)
- `internal/api/delete_handlers_test.go`:
  - `TestDeleteRoleTeamAndProjectRequireWorkspaceAdmin`: PASS (包含本地 channel links 与 agent bindings 级联清理断言)
- `internal/api/design_gate_snapshot_test.go`:
  - `TestDesignReviewApprovalFailsClosedWithoutReferenceOrWaiver`: PASS (空引用/无豁免阻断)
  - `TestDesignReviewApprovalFailsClosedWithForgedSnapshotPath`: PASS (伪造快照路径反向测试通过)
  - `TestCaptureDesignGateSnapshotRejectsPathTraversal`: PASS (OD 工件路径遍历拦截通过)
  - `TestCaptureDesignGateSnapshotRejectsAbsolutePaths`: PASS (绝对路径 /etc/passwd 拦截通过)
  - `TestCaptureDesignGateSnapshotFailureLeavesNoHalfBakedArtifacts`: PASS (中途抓取失败临时目录彻底回收且无半成品)
  - `TestGetDesignSnapshotRejectsSymlinkEscape`: PASS (符号链接逃逸任务目录拦截通过)
- `internal/api/design_gate_pipeline_test.go`:
  - `TestDesignSnapshotPreviewEndpoint`: PASS (CSP sandbox 响应头、nosniff 与跨项目访问 404 拦截)
  - `TestQASignoffGateValidation`: PASS (全部 9 项严格校验子用例全绿)
- `internal/projecttemplate/template_test.go`: 全部 9 个单元与集成测试通过：
  - `TestReactSpringBootControlledCIIntegration`: PASS (实测 `make install` -> `make verify` 全链路全绿)
  - `TestReactSpringBootMakeInstallFailsWithoutLockfile`: PASS (实测拦截无锁安装)
- `make build`: 产出 `dist/multigent` 与 `dist/mga`；前端 `web` 目录 `npm run build` 0 报错。
- `git diff --check`: 退出码 0，零代码与格式缺陷。
- **运行环境实测验证**:
  - VM 二进制部署后查询 `/api/v1/version`，精确返回 `{"ok":true,"version":"101d7dda"}`。
  - 实测创建带成员的真实项目 `pilot-live-test` 成功并完成权限赋权；测试完成后已成功清理。
- **结构化提交演进 (Logical Commits)**:
  - `ce93dce` fix(security): Phase 1 安全门禁严密加固与反向测试闭环
  - `e6268e69` feat(im): 频道绑定管理与项目删除级联清理
  - `0e70a534` feat(template): 新增 React+Spring Boot 模板及初始化与 CI 确定性基线
  - `d1f1f46d` fix(web): 新建项目弹窗展示全部成员的实际 IM 绑定状态
- `101d7dda` fix(project): unify project creation payload, atomic provisioning and transparent validation

## 2026-09-10 ChatOps approval and order MVP evidence

- **审批回调已在真实 Mattermost 环境闭环验证**：旧代码审核卡片点击后先正确触发 CAS 保护并补发当前版本卡片；新卡片显示 `指定审批人：@admin`，点击批准后回写“人工审核已完结”，并推进工作流。
- **任务索引兼容性修复**：历史任务记录可能把可空时间字段持久化为 `""`（如 `FinishedAt`、`ArchivedAt`），导致 Go JSON 反序列化失败并误报 `task_missing`。`entity.Task.UnmarshalJSON` 现将空的可选时间兼容为 `nil`，并有 `internal/entity/task_json_test.go` 回归测试。
- **工作流身份与任务定位修复**：审批人绑定按步骤实例隔离，避免 `owner-engineer` 角色同时用于 Agent 节点和人工代码审核节点；`findTaskInProject` 增加项目任务记录索引回退，覆盖人工审核改变 assignee 后的查找场景。
- **真实 order MVP 结果**：任务 `t-20260909-879ipa` / 工作流 `wfr-p2ubljke` 已 `done_success` / `completed`，9/9 步骤完成。`pr_open_and_merge` 合并 SHA 为 `4d2a358e7710f45d455fdd8bb615482de4bb4db1`，release 产出 `v0.1.0`，GitLab Pipeline `#1041` 的部署作业通过健康检查并验证 `/api/tickets/export.csv`。
- **恢复性证据**：release Agent 曾因等待 Tag 流水线时结束会话、未提交结构化 `step done` 而失败；创建数据库备份后定向恢复任务，启动自愈成功接管 release，未重跑前置实现和审批节点。
- **当前部署**：VM 服务已更新至提交 `a0b3cef3`，health 返回正常；本地工作区 clean，全量 `go test ./...` 与 `git diff --check` 通过。

### Workflow ActorRole Configuration Matrix (新项目交付模板角色标准矩阵)

为彻底杜绝人机角色混淆导致的“未绑定具体用户”及审批阻断（403 `reviewer_authorization_failed`），全系统工作流模板严格执行以下职责解耦标准（以 `greenfield-delivery-pipeline` 为基准）：

| 步骤标识 (Step ID) | 步骤性质 (Type) | 标准角色 (ActorRole) | 绑定对象类型 | 默认指派参考 | 说明 |
|---|---|---|---|---|---|
| `requirement_draft` | `agent_task` | `pm-agent` | Agent | Mira / Lina | 澄清业务诉求与范围 |
| `requirement_review` | `human_review` | `product-owner` | Human | admin / alex | 人工快审需求方向 |
| `design_review` | `human_review` | `product-owner` | Human | alex | OpenDesign 原型设计确认闸门 |
| `implementation` | `agent_task` | `developer-agent` | Agent | Mira | 核心编码与本地验证 (原混用 `owner-engineer` 已修正) |
| `self_review` | `agent_task` | `reviewer-agent` | Agent | Lina | 独立初审 (原混用 `owner-engineer` 已修正) |
| `code_review` | `human_review` | `owner-engineer` | Human | admin | 人工代码审核与风险核验 |
| `qa` | `agent_task` | `qa-agent` | Agent | Lina | 风险-覆盖矩阵与测试用例执行 |
| `qa_signoff` | `human_review` | `qa-owner` | Human | alex | 人工准出签核 (含特批豁免) |
| `pr_open_and_merge` | `agent_task` | `developer-agent` | Agent | Mira | 开 MR 并在 CI 通过后合并 (原混用 `owner-engineer` 已修正) |
| `release` | `agent_task` | `release-agent` | Agent | Mira | 打 Tag、部署与健康探测 (原混用 `owner-engineer` 已修正) |
| `go_live_confirm` | `human_review` | `product-owner` | Human | admin | 验收线上部署并正式归档交付 |

### 关键工程经验沉淀 (Key Operational Behaviors & Patterns)

1. **审批卡片多重安全拦截与 CAS 乐观锁防重放**：
   - 按钮点击先经 CAS（校验 `token_version` 与 `token_hash`），旧卡片被 `review_cas_stale` 拦截并重发最新卡片；
   - 步骤必须处于 open 状态且步骤绑定必须为 `human`（`workflowReviewActorTypeIsHuman`），非人类直接判定为越权；
   - 审批人必须是指定 ActorID 或具备该项目管理权限的平台用户（如 admin），否则严格返回 403。
2. **任务持久化与可选时间字段容错机制**：
   - 历史任务记录中可选时间（`FinishedAt`、`ArchivedAt` 等）存为 `""` 空字符串曾导致 Go 反序列化崩溃、误报 `task_missing`；
   - `entity.Task.UnmarshalJSON` 现自动规整为 `nil`，此模式应贯彻于所有含可选时间字段的模型。
3. **分层消息投递与看板防抖（ChatOps Tiered Notification）**：
   - 常规 Agent 步骤（S0）：仅原地 Patch 更新 Live Card 根帖（1.5s 防抖），不发 Thread 消息；
   - 里程碑（S1）、错误告警（S2）与人工审核（S3）：主动向 Thread 推送卡片，完成后原子就地更新为已归档只读态。

### Follow-up observations

- 任务根帖在中途曾出现“Completed/55%”这类历史投影与真实工作流状态不一致，最终完成时已刷新为 100%/9/9；建议后续单独加一条投影状态机回归测试，确保恢复和人工审批后的根帖不会提前显示终态。
- 发布 Agent 等待外部 CI 时必须保持会话直到提交结构化输出；平台最好提供 release 节点级重试/续接入口，避免只能依靠人工恢复任务状态。

## 2026-09-10 Enterprise Evolution Roadmap, Duration Bugfix & Mattermost Audit

- **企业级演进架构路线图已沉淀**：
  - 详细设计见架构文档：[`docs/architecture/enterprise-evolution-and-scale-roadmap.md`](file:///Users/imac/Documents/code/github/multigent/docs/architecture/enterprise-evolution-and-scale-roadmap.md)。
  - **核心设计共识**：
    - 确立“集中式单活控制面（API/调度/审计/SQLite）+ 分布式运行节点（Docker/Agent 水平外挂）”的务实拓扑，坚决避免在控制面盲目拆微服务引发的分布式一致性灾难。
    - **P0 存量仓库接入**：建立 5 步只读探测与基线验证流水线，实施“失败但可解释（Fail-closed / not_ready）”原则，严禁直接改动 `main`，通过后固化 `.multigent/runtime.json`。
    - **P0 内网 ChatOps 验收**：制定 `doctor --im` 双向探测规范，消除权限不足静默丢弃（提供友好反馈），解决 DEFECT-C3 多绑定冲突。
    - **P1 四层限额治理**：落实 Workspace / Project / Agent / Node Quota 并发与容器资源配额，严密覆盖同项目多任务与跨项目多任务两组基准测试。
    - **P1 测试质量治理**：建立研发 TDD -> 独立 QA 审用例 -> Signoff 追溯矩阵三层防线，高风险未测项必须显式 Waiver 特批。
    - **P2 大需求拆分后置**：降级为“一页纸设计案 + 单批次审批推进”的规范流程，先禁用自动无穷递归派生子任务。

- **多阶段任务“实际耗时显示 1m”根因修复**：
  - **现象**：任务 `t-20260910-62lk48` 运行中前端动态展示真实历时（~50m），任务完成后弹窗与卡片耗时突变为 `1m`。
  - **根因**：`cmd/multigent/scheduler.go` 在调度执行第 3 阶段（`ci_ready`）时硬编码执行了 `task.StartedAt = &now`，无条件洗掉了最初的启动时间戳（`02:02:50` -> `02:51:02`），导致任务完结时 `FinishedAt - StartedAt` 仅剩 89 秒。
  - **修复**：`cmd/multigent/scheduler.go` 改用幂等的 `entity.ApplyStatusTimestamps(task, prev, now)`，并在 `internal/entity/task_timestamps_test.go` 中补充多步骤流转保留原有 `StartedAt` 的回归单测，全量测试已通过。

- **Mattermost `proj-api-key-hub` 频道会话审计与诊断**：
  - **Live Card 原地更新正常**：主帖 `thsdt1g3kbrx8mg4hybi359rwy` 原地更新至 100% 完结，历史旧卡片已安全软删除（`deleteat > 0`），未产生刷屏垃圾帖。
  - **异常发现 1（用户提问被静默拒绝）**：用户 `alex` 在 Thread 询问 `@bot-lina 为什么这个初始化任务干的这么慢，卡点在哪`，因其在项目角色为 `viewer`（只读访客），被后端 `userCanOperateAgentInWorkspace` 判定权限不足（`rejected: permission_denied`）。系统静默丢弃未给任何文字回复，用户体感为机器人假死。
  - **异常发现 2（通道多实例告警 DEFECT-C3）**：频道内同时绑定了 Lina、Mira、Nora 三位 Agent，其中 Mira 与 Nora 共享了相同的 AppId（`h4notz95xif8iehx4z88yyt6ka`），触发系统持续产生 `selecting matches[0]` 降级告警。

## 2026-09-10 ChatOps RBAC, DEFECT-C3 Elimination & Permission Feedback Delivery

- **新建项目弹窗成员角色选择与默认执行者 (Operator)**:
  - **前端交互 (`web/src/components/project/CreateProjectDialog.tsx`)**：
    - 展开「项目成员」手风琴后，为每个被勾选的项目成员提供角色下拉选择器（执行者 `operator`、查看者 `viewer`、项目管理者 `manager`）。
    - 勾选非创建者成员默认赋予「执行者 (`operator`)」，创建者固定展示「创建者 · 管理员」徽章不可取消。
    - 提交请求时向后端发送结构化 `members: [{ username, role }]` 并保留 `memberUsernames` 兼容性。
  - **后端支持 (`internal/api/project_handlers.go`)**：
    - `handleCreateProject` 接收 `members` 参数并规范化校验角色；如果老客户端仅提供 `memberUsernames`，默认角色一律分配为 `operator`（彻底废弃原 `"member"` 导致被降权为 `viewer` 的设计）。
    - 单测覆盖：`internal/api/project_write_rbac_test.go` (`TestProjectCreate_MemberRolesAndDefaults`)。

- **DEFECT-C3 多 Agent 绑定路由冲突彻底消除**:
  - **根因**：`provisionProjectChannelCore` 为频道内 Agent 分配连接时，若缺少独立连接会 fallback 到第一个可用连接，导致 Mira 与 Nora 共享相同 BotID，引发 WebSocket 事件分发多重匹配与告警。
  - **修复 (`internal/api/project_channel_handlers.go` & `internal/api/agent_channel_events.go`)**：
    - 实施两阶段独占分配（Pass 1 专用名优先，Pass 2 空闲独占），同一频道内每个 Bot 连接最多绑定 1 个 Agent；多余 Agent 跳过绑定并在 warnings 中显式提示。
    - `matchChannelEventBindings` 增加路由消歧保护：当同 BotID 存在历史残留绑定时，按 AgentID 与连接名一致性消歧。
    - 单测覆盖：`internal/api/agent_channel_events_test.go` (`TestMatchChannelEventBindings_DisambiguatesDefectC3`)。

- **Live Card 完结时耗时与标题归零彻底修复**:
  - **根因**：`CloseTaskThread`（`internal/imbridge/task_thread_projection_service.go`）在关闭任务时直接调用 `patchLiveCardDirect`，漏传了真实 `ElapsedSeconds` 与 `TaskTitle`，导致最终主帖卡片被刷成 `< 1m` 且标题丢失。
  - **修复**：`CloseTaskThread` 引入 `CloseTaskOptions`，在任务流转至 `Done` 时将真实运行耗时与任务标题显式透传写入归档卡片。
  - **单测覆盖**：`internal/imbridge/task_thread_projection_live_card_test.go`。

- **越权与未绑定操作明确文字反馈**:
  - **文字消息拦截 (`internal/api/agent_channel_events.go: acceptIMMessage`)**：
    - 权限不足时回复：“⚠️ 您在项目「%s」仅拥有只读权限（Viewer），无法唤醒 Agent 或下发操作指令。如需协作，请联系项目负责人为您分配执行者（Operator）或管理者权限。”
  - **卡片按钮回调拦截 (`internal/api/agent_channel_events.go: acceptIMInteractionCallback`)**：
    - 未绑定聊天账号时返回 ephemeral 消息提示先绑定账号；拥有只读权限时返回 ephemeral 消息明确说明只读无权审批。
  - **人工审核审批人回退展示 (`internal/api/task_thread_projection_hooks.go`)**：
    - 审核卡片在步骤实例未产生时，回退至 `run.ActorBindings` 与任务创建者/负责人，避免卡片显示“当前步骤未绑定具体用户”。

- **端到端部署与验收证据 (Verification Evidence)**：
  - **自动化 UI 与 API 测试**：执行 Playwright 脚本成功在 Web 控制台创建项目 `proj-rbac-5511`，勾选成员 `alex`，确认角色下拉框默认选中为 `operator`。
  - **SQLite 数据库验证**：`/opt/multigent/data/.multigent/multigent.db` 中 `alex` 在 `proj-rbac-5511` 的角色成功落库为 `operator`，`admin` 为 `manager`。
  - **通道与绑定验证**：Lina 绑定专用连接 `conn-4632f10ef701e6fa0174e723`（Bot `fq19z958...`），Mira 绑定专用连接 `conn-1bec7de71cae54f406a042d3`（Bot `h4notz95...`），1:1 独占分配，VM journalctl 日志中 DEFECT-C3 告警完全消除（0 告警）。

## 2026-09-10 Code Review Invariants & Hardening Delivery

- **成员角色合约、非法角色拦截与回滚补偿**:
  - **后端创建者强制锁定** (`internal/api/project_handlers.go`)：创建者无论客户端传何角色，后端强制重写锁定为 `ProjectRoleManager`。
  - **非法角色强校验拦截**：`role` 非 `viewer|operator|manager` 时直接返回 HTTP 400 `ErrCodeValidationFailed`（例如传入 `adminish` 返回 400），禁止静默降级为默认角色。
  - **频道创建失败回滚补偿**：若频道创建阶段出错（`pErr != nil`），立即执行对 `s.users` 已写入该项目授权的补偿清理（采用 `make([]projectAccess, 0)` 解决 `UpdateUser` nil 切片被忽略陷阱），杜绝项目创建中断时的孤儿权限悬挂。
  - **前端传参清理 (`web/src/components/project/CreateProjectDialog.tsx`)**：移除冗余的 `memberUsernames` 键，仅发送清晰语义的 `members: [{ username, role }]`。
  - **回归单测**：`internal/api/project_write_rbac_test.go` (`TestProjectCreate_MemberRolesAndDefaults`, `TestProjectCreate_ChannelFailureRollsBackUserAssignments`) PASS。

- **DEFECT-C3 确定性分配、专属拉群与入口 Fail-Closed**:
  - **确定性 1:1 分配** (`internal/api/project_channel_handlers.go`)：`sortedAgentNames` 按字母序排序；连接匹配采用 `connectionMatchesAgent` 精准比对 Profile `botName/displayName/username/agentId`，避免泛模糊子串误伤；仅邀请分配成功的 Bot 入群；未分配独立连接的 Agent 跳过并标记 `status=partial`。
  - **Bot 进群失败防御**：若 Mattermost 拉 Bot 入群失败（403），不为其生成 `AgentChannelTarget` 且不计入 `boundAgents`，明确记录于 `failedAgents`。
  - **启动自愈清理历史重复**：`healAgentChannelBindingsAndIdentities()` 针对同一 `(chat_id, bot_id)` 下的多余活跃绑定自动标为 `unbound`，重启服务自动清理历史脏数据。
  - **入口多候选验签与 Fail-Closed** (`internal/api/agent_channel_events.go`)：多绑定候选时逐一校验 HMAC 签名；若出现多个跨项目合法候选且皆合法，严格 Fail-Closed (401)，根除 `matches[0]` 盲选安全漏洞。
  - **单测覆盖**：`project_channel_handlers_test.go` (`TestProvisionProjectChannel_Success`, `TestProvisionProjectChannel_BotFailureTracking`) PASS。

- **Live Card 耗时冻结**:
  - **耗时计算统一** (`internal/api/task_thread_projection_hooks.go`)：使用 `int(entity.TaskElapsed(t, time.Now()).Seconds())`，完结状态（`FinishedAt` 已设）严格冻结耗时，后续时间流逝耗时不再继续增长。
  - **回归单测**：`internal/entity/task_timestamps_test.go` (`TestTaskElapsed_FreezesOnCompletion`) PASS。

- **Mattermost Action 错误反馈与权限拦截**:
  - **错误文字弹窗** (`internal/api/chatops_handlers.go`)：`writeMattermostActionError` 输出增加 `"ephemeral_text": message`，确保 Mattermost 客户端收到清晰的错误提示弹窗。
  - **审批人说明合规** (`internal/imbridge/task_thread_projection_service.go`)：工作流人工审核无指定人类审批人时，文案显示为“`审批处理：待项目管理员认领`”，禁止将任务创建人误表述为“指定审批人”。
  - **审批权限强校验** (`internal/api/runtime_workflow_decision_handlers.go`)：workflow decision 与 Action 回调校验用户具备全局 `admin` 或项目至少 `operator` 角色；Viewer 无论是否在项目中均严格拒绝推进工作流（403）。
  - **回归单测**：`internal/api/chatops_handlers_test.go` (`TestMattermostActionCallback_RejectionScenarios`) 覆盖未绑定身份、Viewer 权限拒绝、过期 Token、CAS 409 冲突四类场景，全量断言工作流状态未被非法篡改或推进。

- **生产部署与端到端实测验证证据**:
  - **服务部署**：Linux amd64 产物编译部署至 VM 并重启 `multigent` 与 `multigent-mattermost-bridge` 服务。
  - **自愈日志确认**：服务启动即刻触发自愈并准确清理了 3 条历史重复绑定：
    - `deactivated duplicate binding chan-3f2b208a5d75052dad579cad (OrderCollab/Lina) on channel 9iotdnrgd7dtdggo8u96fsabja bot fq19z958a78stdxutq1ixbxsdw (DEFECT-C3)`
    - `deactivated duplicate binding chan-9649d1d0d024472fa7c8966f (OrderCollab/Mira) on channel 9iotdnrgd7dtdggo8u96fsabja bot h4notz95xif8iehx4z88yyt6ka (DEFECT-C3)`
    - `deactivated duplicate binding chan-b75213f3e362a5156ec5688f (api-key-hub/Nora) on channel en7zkc7s1b8nmxeqaf49yyy5fy bot h4notz95xif8iehx4z88yyt6ka (DEFECT-C3)`
  - **UI 项目创建实测**：Playwright 脚本在 Web 控制台创建项目 `proj-rbac-3511`，添加成员 `alex`，确认其默认角色为 `operator`。
  - **SQLite 落库验证**：数据库 `users.projects_json` 确认 `admin` 强制授予 `manager`，`alex` 正确记录为 `operator`。
  - **非法角色 API 防御实测**：curl 提交 `role: "adminish"` 返回 `HTTP 400 Bad Request`，`code: "validation_failed"`, 验证通过。

- **Mattermost ChatOps 审批打回 (Reject) 400 路由不匹配缺陷修复与闭环**:
  - **根本原因**: Mattermost 弹窗提交处理函数 (`chatops_handlers.go`) 硬编码传递 `decision = "rejected"`，而工作流引擎定义及边转移规则唯一定义为 `cond("decision", "eq", "request_changes")`，且决策归一化未覆盖 `rejected`，导致出边匹配失败抛出 400。此外，审核步骤定义了批准产物字段，打回时引擎仍过度校验必填；`isDialogAction` 未包含 `reject` 导致打回失败后重试被防重放拦截（`action_replay_blocked`）。
  - **修复措施**:
    1. `chatops_handlers.go`: `tokenData.Action == "reject"` 显式规范化为 `decision = "request_changes"`；将 `reject` 纳入 `isDialogAction`，失败或超时重试时自动补发新卡片。
    2. `review_resolution.go` & `workflow_handlers.go`: `ResolveApprovalOutputs` 与 `normalizeWorkflowReviewDecision` 统一支持 `reject`、`rejected`、`needs_changes`、`rework` 自动归一化为 `"request_changes"`。
    3. `store.go`: `workflowConditionMatches` 评估 `decision` / `review_decision` 时对两端进行语义归一化（`approve` / `request_changes`）；`normalizeWorkflowOutputValues` 在打回决策时豁免批准类产物的必填校验。
  - **验证证据**:
    - 单测覆盖：`TestMattermostDialogSubmit_RejectRework` 与 `TestMattermostActionCallback_FailedRejectDialogReissuesCurrentCard` 全部 PASS。
    - 全量单测：`go test ./internal/...` 40+ 个包 100% PASS。
- **Agent 通用通知 (`mga notify send`) 智能收归任务 Thread 策略与实现交付 (2026-09-10)**:
  - **核心痛点与目标**: 消除 Agent 执行任务向频道通报时独立发顶级消息引起的群聊刷屏与看板割裂问题。支持 `--thread auto | task | channel` 三模式，确保任务通知智能归入任务根看板 Thread。
  - **关键安全红线与设计决议 (Decisions & Architecture)**:
    1. **Wakeup 任务与业务目标任务身份脱节修正 (P0)**:
       - 调度器（`cmd/multigent/scheduler.go` 与 `internal/api/scheduler_attention.go`）在生成 wakeup 任务时向任务 Vars 注入 `MULTIGENT_WAKEUP_TARGET_TASK_ID` 与 `MULTIGENT_WAKEUP_PROJECT`。
       - 服务端通知端点直接读取该任务变量获取可信业务目标任务，彻底解决 RunID 指向 wakeup 任务导致的任务身份脱节。
    2. **同项目跨任务防串线**:
       - 客户端传 `--task` 时，必须与服务端解析出的当前目标任务一致；传同项目其他任务直接返回 400 阻断，杜绝污染其他任务 Thread。
    3. **已知任务冲突禁止降级**:
       - 一旦识别出当前任务，若目标频道与看板频道不一致，`auto` 与 `task` 均返回 400 拦截，防止任务小结误发到无关群组。
    4. **IM 实例链式强校验**:
       - 沿 `binding.ConnectionID -> connection.IMInstanceID -> GetProjectChannelLink -> link.ChannelID == target.ChatID == projection.ChannelID` 严格校验，跨 IM 实例严格拒绝。
    5. **参数冲突与私聊保护**:
       - `--to source --thread task` 互斥参数直接返回 400。
       - 私聊 DM 目标在 `auto` 模式下保持直发，在 `task` 模式下返回 400 拦截。
    6. **去冗余前缀**:
       - 入 Thread 成功的回复消息，服务端自动省略 `[Workspace] [project]` 前缀，保持 Thread 内对话自然流畅；顶级消息保留前缀。
  - **验证证据 (Verification Evidence)**:
    - `internal/api/runtime_notify_handlers_test.go`: 15 项全量单测矩阵通过（覆盖非法枚举、参数冲突、wakeup 目标任务推导、fail-closed 零发帖、同项目跨任务拦截、频道不匹配拦截、跨实例拦截、无上下文 auto 降级、closed 投影降级、显式 channel 顶级发送、私聊直发保护、去前缀、跨 Agent 回复同一 Thread、防伪造 TaskID 拦截、跨项目 Worker 渠道自动优先匹配）。
    - 调度器多信号隔离单测：`internal/api/scheduler_attention_worktree_test.go` 验证批次含多个不同任务时 fail-closed 不注入单一任务且不挂错 worktree。
    - 任务保留变量防御单测：`internal/api/task_vars_reserved_test.go` 验证 API 禁止客户端注入 `MULTIGENT_WAKEUP_TARGET_TASK_ID`。
    - 仓库级全量回归：`make test`（全仓库 40+ 包）100% PASS。
    - 代码质量检查：`git diff --check` 退出码 0，零代码与格式缺陷。
    - 生产部署与真实环境验证 (Live VM Verification)：
      - 编译带 commit 戳 Linux amd64 二进制热部署至 VM，重启 `multigent.service` 与 `multigent-mattermost-bridge.service`，健康检查返回 `{"ok":true,"version":"0071f74a-dirty"}`。
      - 真实任务 `t-20260910-pu8mhs`（频道 `#proj-api-key-hub` `en7zkc7s1b8nmxeqaf49yyy5fy`，Root Post `xgz3fhzbrbntjgno5h57pner8a`）实测验证通过：
        1. `mga notify send --thread task`: 精确挂入任务看板 Thread (`root_id: xgz3fhzbrbntjgno5h57pner8a`, `externalReply: true`, `externalSent: true`)。
        2. `mga notify send` (默认 auto 模式): 自动识别当前任务上下文并智能挂入看板 Thread。
        3. `mga notify send --thread channel`: 显式作为频道顶级消息广播发送。
        4. 防伪造 task ID 测试：传递伪造 task ID 严格返回 HTTP 400 Bad Request 拦截。
        5. 无任务上下文时：`--thread task` 严格 400 拦截；`--thread auto` 安全降级为顶级消息 (`threadFallback: true`)。

