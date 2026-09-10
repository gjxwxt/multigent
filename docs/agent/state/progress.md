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
