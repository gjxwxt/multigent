# Current progress

## Completed: IM identity center P0

Commit `9650f924` completed the safe visual grouping slice. The account API exposes only routes the current user may access by project or Agent Worker grant. A bound route remains visible solely so its owner can unbind it; an unbound and inaccessible route is omitted.

Verified locally on 2026-09-08:

- `go test -v ./internal/api -run TestUserIMIdentities_VisibilityRules_ABC`
- `make test`
- `make build`

The VM health endpoint responded successfully, but reports version `dev`; it is not provenance evidence for commit `9650f924`.

## Completed: IM instance association foundation

The control plane now stores an `im_instance_id` on each connection and an `im_instances` record with `admin_attested` provenance. Only a workspace administrator can create an association, attach a Bot connection, or detach it. Account cards prefer that association when grouping; the existing URL-origin grouping remains a display-only fallback.

Verified locally on 2026-09-08:

- `go test ./internal/db ./internal/api -run TestIMInstance`
- `make test`
- `make web`
- `make build`

## Completed: IM instance delivery routing

D6 now accepts a user identity from another Agent connection only if both active connections share the same `admin_attested` IM instance and the recipient still has access to the destination Agent route. For a different Mattermost Bot, it clears the original `ChatID`; the existing Mattermost driver then creates the destination Bot's direct channel from the external user ID. The account page labels this as same-instance reuse rather than a second direct bind.

Verified locally on 2026-09-09:

- `go test ./internal/api ./internal/imbridge -run TestD6OutboundIdentityFallback|TestRuntimeNotify|TestRuntimeChannels|TestMattermost`
- `make test`
- `make web`

## Completed: IM approval identity hardening & atomic binding claim

Commit `d15e7fd` hardened Mattermost identity resolution, binding conflict defense, and ChatOps callbacks:

1. **Mattermost authoritative source**: Ceased writing Mattermost bindings to global `external_identities`; `UserChannelIdentity` is the single source of truth for Mattermost identity claims.
2. **Atomic binding claim**: Added `ClaimMattermostIdentityInScope` with SQLite transaction locking, ensuring mutual exclusion against concurrent double-bind race conditions.
3. **Unified trusted boundary**: Extracted `connectionsShareDeliveryBoundary`, `bindingsShareDeliveryBoundary`, and `getTrustedConnectionIDsForConnection` into `internal/api/im_boundary_helper.go`, shared uniformly across `/mg bind`, D6 notify, and ChatOps approvals.
4. **ChatOps token connection hardening**: Action and dialog callbacks strictly require `peek.ConnectionID`, load secrets for that specific connection, verify signature, and reject empty/tampered tokens immediately.
5. **Scoped user resolution**: `resolvePlatformUserForAction` resolves users strictly within the trusted connection or attested instance scope. Fails closed with security audit if multiple distinct users claim the same external ID in scope. Removed insecure `mmUserName == u.Username` fallback.

Verified locally and on live VM (2026-09-09):
- `go test -v ./internal/db -run TestClaimMattermostIdentity` (PASS)
- `go test -v ./internal/api -run "TestChatops_|TestMattermostSlashBind"` (PASS)
- `make test` (all packages PASS)
- `make web` (TypeScript + Vite PASS)
- `make build` (PASS)
- Deployed to VM `192.168.139.231:27892` and verified live callback intercept for empty ConnectionID.

## Next: IM Slash Command instance gateway convergence

Currently, Slash Commands (`/mg`) in Mattermost register per Bot webhook. Moving to a single unified Slash Command gateway per Mattermost Team/Instance requires routing commands through an instance gateway dispatcher to the appropriate Agent/Project.

## Completed locally: Mattermost ChatOps post-action observability and card semantics

The action callback now emits a shape-only, secret-safe diagnostic stage for every received Mattermost post action. It records only action class and field-presence flags; it never writes action/dialog tokens, request bodies, cookies, bot tokens, or reviewer comments. Post-action failures now use Mattermost's documented `error.message` response and successful one-click approval returns an `update` that removes the original card actions immediately, plus `ephemeral_text` confirmation.

The callback continues to require the signed token action to match `context.action`, before action-session creation. This adds no permission bypass and preserves token verification, trusted-instance identity resolution, reviewer authorization, Dual CAS, and session claims.

Human-review cards now carry the active human step's actor binding. They mention a Mattermost username only when the stored binding has a safe `externalUsername` on the same active IM instance; otherwise they show a non-mention fallback. Zero-input cards now also expose a review-summary dialog, and edit dialogs show an explicit review summary even when there are no pending parameters.

Verified locally on 2026-09-10:

- `go test ./internal/api -run 'Chatops|Mattermost|Interaction|Workflow' -count=1`
- `go test ./internal/imbridge -count=1`
- `git diff --check`

Deployment and the live callback's actual rejection stage remain unverified. A live click after deployment is required to distinguish an unreachable callback from one of the now-logged fail-closed stages; no task, database, or external approval was changed during this work.

## Completed: OpenDesign 设计门执行治理与模型适配 (commit `0b8363b4`)

1. **输入契约与角色边界**: `designPendingPrompt` 强制使用已澄清的 `approved_requirement` / `requirement_draft` 全文，替换原始粗糙 Prompt；强制注入 UI 原型专家角色边界，限制在 OD 沙箱内产出单页 HTML/Mock 原型，严禁越界修改生产代码。
2. **唯一约束自愈**: `handleDesignStart` 增加自动清理机制，若 OD 容器内存在无产物的同名孤儿空项目（`proj_mg_{taskID}`），先执行删除自愈再创建，杜绝 `UNIQUE constraint failed: projects.id`（502）。
3. **16k Token 截断根因定位与模型切换**:
   - OpenDesign 容器采用 `--read-only` 根只读模式运行，内置 BYOK 适配层（`byok-opencode.js`）硬编码 `DEFAULT_OUTPUT_TOKEN_LIMIT = 16_384`。
   - `qwen3.8-27b` 面对全量规范时思考链输出高达 5.4 万字（耗尽 16k tokens），在写文件前被上游推理服务截断（`reason: length`），触发 `no_artifact`。
   - 全量实测内网网关模型后，将设计门默认模型切换为 `glm-5.3-flash`（思考链极克制 ~90 tokens，单次消耗 2,891 tokens 即可落盘完整高保真原型，耗时 15 秒，工具调用 100% 稳定）。
4. **验证证据**:
   - `internal/api/od_client.go`: `odDefaultModel = "glm-5.3-flash"`
   - `go test -v ./internal/api -run TestDesign` 全绿 (PASS)
   - 交叉编译 `dist/multigent-linux-amd64` 并热部署到 VM，服务状态 healthy。

## Completed: Preview Copilot Turn Receipts 切片 B (commit `b114bd9b`)

1. **事务性回执与 Group-Slot 生命周期**：
   - 实现了 `internal/previewreceipt`：包含 `TurnEngine` 与 SQLite CAS `Store`，支持状态机 `CAPTURED → COMMITTING → COMMITTED / REVERTING → REVERTED / REVERT_FAILED`。
   - `Prepare`、`Finalize`、`Abort`、`Recover` 全生命周期实行单 DB 事务原子批处理（`CommitRecordWritesGuardedTx`），彻底消除了多回执收编裂脑与半终态。
   - 实现了租赁防御（Lease Defense）：`COMMITTING`、`REVERTING`、`REVERT_FAILED` 及 `commit_intent` 组即便超时也严禁新 Turn 接管。
2. **人工审核收编与不可变基线保障**：
   - `workflow_handlers.go`：在人工审核通过时调用 `commitAndPushReviewChanges`，统一以 `PreCommitSHA` 和 `CommitIntentID` 生成携带 `Multigent-Commit-Intent` Trailer 的 Checkpoint Commit，本地提交/回执失败严格阻断流转。
   - 启动自愈扫描（`RecoverStaleReceipts`）：精准按行匹配 Intent Trailer，区分已提交与未提交，安全释放或补齐快照清理。
3. **严格沙箱隔离与生产链路 Docker 验证**：
   - 隔离预览运行专有配置（`IsolatedPreview: true`）：拒绝 `ExtraVolumes`、自动凭据挂载、Docker socket 及宿主目录（`WorktreeParentMount`、用户 bin、Cursor 二进制）。
   - 沙箱硬断言：卷挂载数量严格等于 1，且唯一挂载目标只能是 `/workspace:rw`。
   - 生产链路真实 Docker 测试（`TestPreviewTurn_DockerSandboxExecution_RealContainer`）：通过 `previewDefaultAgentRunner` $\to$ `multigent exec` $\to$ `runner.Runner` $\to$ `runenv.DockerProvider` $\to$ `sandbox.BuildArgs` $\to$ 真实 `alpine:3.21` 容器，验证代码修改、回执生成、Checkpoint 提交与快照物理清理。
4. **服务治理与零泄漏审计**：
   - 服务端项目白名单硬门禁（`MULTIGENT_PREVIEW_TURN_RECEIPTS_PROJECTS`）。
   - 产出 [`docs/runbook-preview-turn-receipts.md`](file:///Users/imac/Documents/code/github/multigent/docs/runbook-preview-turn-receipts.md)，明确非破坏性排查恢复 SOP。
   - 零泄漏结构化审计日志覆盖执行、拒绝、回滚、取消及收编事件。
5. **验证证据**:
   - `MULTIGENT_RUN_DOCKER_INTEGRATION=1 MULTIGENT_PREVIEW_DOCKER_TEST_IMAGE="alpine:3.21" go test -race -v ./internal/api -run 'TestPreviewTurn_DockerSandboxExecution_RealContainer'`: PASS
   - `go test -race -v ./internal/sandbox -run 'TestBuildArgs_IsolatedPreview'`: PASS
   - `go test -race -v ./internal/runner -run 'TestIsolatedPreviewRun_FailClosedRejection'`: PASS
   - `go test -race -count=1 ./internal/previewreceipt/...`: PASS
   - `go test -race -count=1 ./internal/api -run 'Test(WorkflowReview_Commit|ReviewCommit|PreviewTurn|PreviewOrigin)'`: PASS
   - `make test` & `make build`: PASS

## Completed: Preview Copilot Guarded Skill Profiles (Task 3.2)

1. **服务端受控技能画像白名单 (Curated Allowlisted Profiles)**:
   - 定义 4 组预置 Skill Profiles：`ui-polish`、`a11y-remediation`、`responsive-layout`、`form-logic`；
   - 未在白名单内的 Profile 严格 Fail-Closed（返回 400 Bad Request）。
2. **内置技能与 SHA-256 完整性摘要保护 (Integrity Digest)**:
   - 内置 `modern-web-guidance` 与 `a11y-debugging` 核心专业工程规范；
   - `ComputeSkillDigest` 计算全目录文件 SHA-256，防御文件篡改并提供版本溯源指纹。
3. **脚本执行中立化防线 (Script Neutralization Invariant)**:
   - 严格落实架构红线：预览 Copilot 严禁挂载或执行脚本附件（`.sh`），仅提取纯声明式 Markdown 指引注入提示词。
4. **端点与前端交互集成 (API & Web Console)**:
   - 暴露 `GET /api/v1/projects/{name}/tasks/{taskId}/preview/profiles` 端点；
   - `PreviewDrawer.tsx` 支持技能药丸徽标（Chips）单点切换、快捷操作自动附带画像、消息气泡清晰呈现当前生效技能。
5. **验证证据**:
   - `go test -race -v ./internal/api -run 'TestPreviewSkillProfiles'`: PASS (6/6)
   - `go test -race -count=1 ./internal/api -run 'Test(WorkflowReview_Commit|ReviewCommit|PreviewTurn|PreviewOrigin)'`: PASS
   - `cd web && npm run build`: PASS (TypeScript 0 错误)
   - `make test`: PASS (全仓库 40+ 包)
   - `make build`: PASS (二进制全量嵌入完成)
