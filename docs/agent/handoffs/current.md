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

