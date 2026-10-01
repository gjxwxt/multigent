# 部署审批人（B3）实施计划

> 状态：待评审 ｜ 前置：spec v2（`deploy-center-frontend-refactor-spec-v2.md`）登记的二期任务 B3
> 参考系：工作流 human_review 审批模式 + 现有 MM 部署审批卡链路
> 关联提交：前端重构 c54a5be6 / b644c95c、未接入提示 d56a0aac（multigent-integration @ feature/deploy-center-phase1）

---

## 1. 问题与目标

**现状**：部署发起时只能选"是否需要审批"（`approvalRequired: boolean`），审批卡发到项目绑定频道后，**频道内任何有 operator 权限的人都能点批准**——"需审批"约束的是"有没有人批"，不是"谁来批"。

**目标**：发起部署时可直接指定审批人；审批卡定向送达该人；只有被指定的人（或管理员兜底）能批准/驳回。同时保留"不审批 · 直接部署"。

**对齐 spec v2 B3 原文**（已登记的二期项）：
- `entity.Project.ApprovalRequired` 持久化（batch-4 TODO 已登记，`deploy_handlers.go:343`）
- 创建 body 收 `approverId`
- MM 审批卡定向（DM 或 @）→ 审批策略选择器升级为审批人下拉

---

## 2. 现有链路盘点（已核实源码）

| 环节 | 位置 | 现状 |
|---|---|---|
| 创建部署单 | `internal/api/deploy_handlers.go:170` `createDeployRequest{Branch,SHA,Vars,ApprovalRequired *bool}` | 只有布尔 flag；`deployApprovalRequired` 解析序 = 显式 flag > 项目默认（**TODO batch-4，恒 false**）> 免审批 |
| Approval 落库 | `internal/db/deploy_requests.go:18` `Approval map[string]any` | 现只写 `{required, state}` |
| REST 审批 | `deploy_handlers.go:483` `POST .../deploy/requests/{id}/approve` | 无审批人校验；CAS `pending_approval→approved` 后直通 `triggerDeployPipelineNow` |
| MM 审批卡 | `internal/api/deploy_card.go` `PostDeployApprovalCard` → `threadProjections.ResolveMMTarget(workspaceID, projectID, "")` | 发**项目频道**；按钮 token 2h HMAC（`signDeployCardToken`） |
| 卡片回调 | `internal/api/chatops_handlers.go:220` `handleMattermostActionCallback`（publicMux，但经完整校验链：token HMAC/过期/频道匹配/nonce 防重放/身份绑定） | deploy 分支 `handleDeployApprovalChatopsAction` 只做：项目匹配 + `chatopsDeployOperatorAllowed`（admin / workspace owner·admin / 项目 operator+） |
| 身份映射 | `internal/db/external_identities.go` `ExternalIdentityByExternalID(workspaceID, "mattermost", mmUserID)` | **表已存在且有数据**（VM：alex/admin 两行）；`user_channel_identities` 表含 DM 频道（external_chat_id） |
| 前端成员下拉 | `CreateTaskDialog.tsx:223-254` | `/api/v1/users` 过滤 disabled + 项目 human members（model==='human'）合并 Map 去重，label 用 displayName |

**关键缺口**：全链路没有"审批人"概念；MM 侧平台用户 ↔ 平台账号的映射已就绪，是定向能力的现成地基。

---

## 3. 方案设计

### 3.1 数据契约

**DeployRequest.Approval map 扩展**（不改表结构，复用 `approval_json` 列）：

```json
{ "required": true, "state": "pending_approval", "approverId": "alex", "approverLabel": "Alex (alex)" }
```

- `approverId` = 平台账号 username（与 `CreatedBy`、`chatopsDeployOperatorAllowed` 的 user 体系一致，也与工作流 human_review 的 assignee 同一命名空间）。
- 空 = 不指定审批人 → 维持现状（operator 权限即可批）。
- **不新增 Project.ApprovalRequired 持久化**：batch-4 TODO 登记的是"项目级默认审批策略"。本计划先做单据级审批人（用户直接诉求）；项目级默认作为 P2 项独立排期，避免一次改动横跨 entity 序列化 + 项目设置页 + 兼容迁移三处。

### 3.2 后端改动

1. **创建端点**（`deploy_handlers.go`）：
   - `createDeployRequest` 增加 `ApproverID string \`json:"approverId"\``。
   - 校验：非空时该 username 必须是 workspace 内未禁用账号（`s.users.GetUser` + `Disabled` 检查，与 chatops 权限判定同源）；无效 → 400，**fail-closed 不落单**。
   - 落库 Approval map 写入 `approverId` / `approverLabel`。

2. **审批校验（核心硬约束）**：新增判定函数，REST 与 chatops 两条路共用：

   ```go
   // approverGate: 单据指定了审批人时，仅该人（或 admin 兜底）可过闸。
   // 未指定审批人的单据维持现有 operator 权限模型，行为不变。
   func (s *Server) deployApproverAllowed(req *controldb.DeployRequest, actorUsername string) bool {
       approver := approvalString(req.Approval, "approverId") // 现有 Approval map 的取值辅助
       if approver == "" { return true } // 向后兼容：老单据 + 不指定审批人 = 现状
       if actorUsername == approver { return true }
       u := s.users.GetUser(actorUsername)
       return u != nil && u.Role == RoleAdmin // admin 兜底：审批人休假时不阻塞交付
   }
   ```

   - 接入点 A：`handleApproveDeployRequest` / `handleRejectDeployRequest`（REST，actor = `s.currentUserName(r)`），不过闸 → 403。
   - 接入点 B：`handleDeployApprovalChatopsAction`（`chatops_handlers.go:717` 附近），actor = `platformUserID`（回调链已做身份绑定解析），不过闸 → 沿用现有中文错误文案格式回复"您不是该部署单的指定审批人"。
   - **fail-closed 原则**：approverId 存在但 `GetUser` 异常时按拒绝处理。

3. **MM 卡定向**（`deploy_card.go` `PostDeployApprovalCard`）：
   - 指定了审批人时，先查 `ExternalIdentityByExternalID` 反向：`ListExternalIdentities(filter{WorkspaceID, Provider:"mattermost", UserID: approverId})` 拿 MM user id，卡片文本首行插 `@mmuser`（频道内 @ 高亮）。
   - **DM 直发不做在本期**（spec v2 B3.2）：`user_channel_identities` 表虽有 DM 频道数据，但 DM 发帖需要 bot 与用户先有直接频道，且失败回退路径复杂；@ 提及在项目频道内已能强提醒，且保留了"审批人不在时 admin 可见可兜底"的透明性。DM 作为二期增强。
   - 卡片上标注指定审批人（"指定审批人：Alex"），无人可 @ 时（无 MM 绑定）如实显示用户名文本，不静默降级为无标注。

### 3.3 前端改动（DeployConsole.tsx）

**形态**（对齐工作流 human_review 的"指定到人"语义 + CreateTaskDialog 成员下拉模式）：

审批策略 select 升级为**双控件**：

```
┌ 审批策略 ────────────────────────────┐
│ [ 不审批 · 直接进入部署 ▾ ]           │   ← 现有 select 保留三态中的两态
│   需审批 · 审批人批准后自动触发       │
└─────────────────────────────────────┘
┌ 审批人（选"需审批"时显示）────────────┐
│ [ 不指定 · 项目内可审批成员均可 ▾ ]    │   ← 新增成员 select，首项空值
│   Alex (alex)                        │
│   Admin (admin)                      │
└─────────────────────────────────────┘
```

- 成员数据源复用 CreateTaskDialog 模式：`useApiJson('/api/v1/users')` 过滤 disabled，与项目 human members 合并去重，label 用 displayName。
- 选"不审批"时审批人控件隐藏、`approverId` 不随 payload 提交。
- `ProjectDeployPage.confirmBody` 弹窗文案追加"指定审批人：{label}"。
- 台账行（列表页）在审批列展示审批人短名。

**i18n**：`approvalApproverLabel` / `approvalApproverAny` / `confirmApproverSuffix` 等约 4 个 key，双语落 zh-CN + en。

### 3.4 验收与测试清单

**后端单测**（`deploy_handlers_test.go` / `chatops_handlers_test.go`）：
1. 创建带 approverId 的单：Approval map 含 approverId；无效 approverId → 400。
2. REST approve：指定审批人时本人过闸、他人 403、admin 兜底过闸。
3. chatops approve：同上三态（platformUserID 维度）。
4. 未指定审批人的单：REST/chatops 行为与现状完全一致（回归保障）。
5. 老单据（approval_json 无 approverId）：不指定即放行。

**前端**：`make web` 通过；部署页手测——免审批不显示审批人控件 / 需审批+指定人 → payload 带 approverId / confirm 文案含审批人。

**实弹验收**（VM）：指定 alex 发起需审批单 → MM 项目频道卡片带 @alex → admin 点批准成功、alex 点批准成功、第三账号被拒 → 台账显示审批人。

---

## 4. 分期与工作量

| 阶段 | 内容 | 工作量 |
|---|---|---|
| P1 后端 | 创建契约 + approverGate 双接入点 + 单测 | 1 ~ 1.5 天 |
| P1 前端 | 双控件 + i18n + payload/confirm/台账 | 0.5 天 |
| P1 联调 | MM 卡 @ 审批人 + VM 实弹三账号验收 | 0.5 天 |
| P2（本期不做） | `entity.Project.ApprovalRequired` 项目级默认（batch-4 TODO）；DM 定向；审批人离岗代理/转派 | 另立计划 |

**合计 P1：2 ~ 2.5 天。**

---

## 5. 风险与边界

1. **@ 提及不等于强制**：MM @ 只是提醒，真正的强制在 `approverGate` 服务端校验（token 校验链之后的硬闸）。文案上不要承诺"只有审批人能点按钮"，按钮人人可见、点了才被拒。
2. **审批人无 MM 绑定**：卡片退化为文本标注（不 @）；REST 端仍可审批。落单时校验账号存在已挡住大部分拼写错误，MM 绑定缺失如实展示。
3. **approverId 用 username 而非内部 ID**：与 `CreatedBy`、chatops actor、工作流 assignee 同一命名空间，避免引入第二套身份换算；账号删除场景由 admin 兜底覆盖。
4. **向后兼容**：`approverId` 空值语义 = 现状，存量单据与存量调用方（CLI/API 脚本）零破坏。
5. **公共端点边界不碰**：`/api/v1/im/mattermost/actions` 仍在 publicMux（MM 服务器无法带平台 token），安全依赖既有校验链 + 本计划新增的 approverGate，不新增 publicMux 路由。
