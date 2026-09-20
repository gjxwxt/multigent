# 交付模式前置告知方案（流水线外部依赖）

状态：**待评审**。评审目标读者：另一个 agent / 人。请重点复核 §2 事实清单与 §9 未验证项。

来源：本文替换 `.gemini/antigravity/brain/d360b492-1191-41fe-b520-c49148631e8c/implementation_plan.md`（下称"原方案"）。原方案的诊断成立、UI 位置选对，但数据源与推导方式有两处致命缺陷，另有四处描述与源码不符，评审结论见 §3。

---

## 1. 背景

**要解决的问题**：`unified-delivery-pipeline` 的若干节点在语义上依赖 GitLab 远端与 CI，但项目在**未绑定远端**时仍然可以选中这条流水线并启动，且**平台上下两端都不知道这件事**：人看不到，agent 也看不到。

**已确认的现场后果**（不是预测）：
- 未绑定时 `create_pr` 步由 agent 通过内置 skill 完成，而该 skill **明确规定**无远端时把 `pr_url` 写成 `branch: <branch_name>`、`pr_number` 写成 task ID（`internal/builtins/files/skills/git-pr-delivery/SKILL.md` Scenario B）。
- 平台侧 `updateTaskRemoteMR` 遇到 `none` / `branch:` 前缀时是 **`return false` 静默跳过、不记 MR**（`internal/api/workflow_handlers.go:1422-1425`），而 `pr_url` 作为 workflow 步骤输出**没有任何结构校验器**。
- 合起来：这个非 URL 的占位值会沿 `e-create-pr-review` 传给 `pr_review` 人工节点（`internal/workflow/store.go:260`），返工时还会作为 `previous_pr` 回灌给实现步（`store.go:263`）。**这是约定式合法化，不是护栏。**

**两条已定的产品前提**（评审时请勿重开）：
1. **不在平台内做 diff 展示**，人工代码审依赖 preview 实跑 + 跳转远端看 commit。据此，"人工审节点位置放错（排在 PR 之前）"这一改法**明确不做**。
2. 人工闸门在**无 IM 时已有可见提醒**：进入 `human_review` 时无条件写 `Status=awaiting_confirmation` + `Assignee=reviewer`（写失败会中止流转，`internal/api/runtime_workflow_handlers.go:1302-1309`），控制台有 `GET /api/v1/workbench/tasks`（按当前用户过滤，`internal/api/workbench_handlers.go:208`）与未读徽标（`web/src/pages/WorkbenchPage.tsx:1107-1112`）。因此本方案**不含**"新增无 IM 提醒"。

---

## 2. 事实清单（评审者逐条可复核）

| # | 断言 | 证据 | 确定度 |
|---|---|---|---|
| F1 | `verifiedBinding()` 未绑定时返回 `ok=false` 且**不报错**；`verifiedGitLabHost()` 未绑定时返回**硬错误** | `internal/api/remote_binding_service.go:134-148`；`:160-162` | 实读 |
| F2 | 项目记录上的 `RemoteProvider`/`RemoteConnection` 是**客户端可写的显示字段，不证明任何事**；平台判定一律走已验证绑定 | `internal/api/ci_ready_handlers.go:164-165`（注释原文）；键为 `(workspaceID, projectName)` | 实读 |
| F3 | 无绑定且未声明 remote-required 时，`ci_ready_gate` 的流水线证据**整体跳过**（no-op），只剩静态检查 | `ci_ready_handlers.go:171-174, 192-199`（`localOnlySkip`） | 实读 |
| F4 | 但 13 项静态检查里的 `baseline_files` **仍然要求** `.gitlab-ci.yml` + `deploy/Dockerfile` + `deploy/compose.yml` 存在 | `internal/ciready/ciready.go:118-131` | 实读 |
| F5 | agent 提示词里**完全没有**远端绑定状态 | `internal/runner/runner.go:892-994`；grep `RemoteProvider\|remoteBound\|RemotePipelineRequired` 在 `internal/runner` 零命中 | 实读（grep） |
| F6 | `BuildTaskPrompt` 在**控制台侧**执行，不在运行节点上 | 调用方 `internal/api/runtime_node_handlers.go:490` | 实读 |
| F7 | 无 origin 时 `commitAndPushReviewChanges` **整块跳过：不写评论、不报错、返回 nil**；push 失败才写一条回滚锚点评论并返回 nil | `internal/api/workflow_handlers.go:1368-1369, 1380-1407` | 实读 |
| F8 | `remoteSyncStatus` 会诚实记 `not_applicable`，但 **`web/` 里零个消费者** | `workflow_handlers.go:1486-1490`；`internal/api/task_remote_sync.go:36-38`；grep `remoteSync` 在 `web/` 零命中 | 实读（grep） |
| F9 | `unified-delivery-pipeline` 实际 **13 步**；`AGENTS.md` 写的"12 步闭环"已过期 | `internal/workflow/store.go:1017-1031`（计数 13）；`AGENTS.md:144` | **实测**（`sed -n '1017,1031p' \| grep -c 'tmplStep('` = 13） |
| F10 | `gitworktree` 的 `PushTag` **无生产调用方**，而模板描述称存在"平台打标机制" | `internal/gitworktree/worktree.go:891`（仅测试引用）；`internal/workflow/store.go:978` | 实读 |
| F11 | `web/package.json` 的 scripts 只有 `dev/build/lint/preview`，**无 vitest**；`web/src` 下**零个** `.test.ts(x)` | `web/package.json`；find 结果 | **实测** |
| F12 | 存在可切换的无远端依赖模板：`tdd-review-loop`、`agentic-bug-triage-loop`、`hotfix-deploy-pipeline` | `internal/workflow/store.go` `Templates()` 注册表 | 实读 |
| F13 | IM 桥不配**不会**卡住 agent 派发：通知钩子在流转提交之后、panic-recovered goroutine 内、返回 void | `internal/api/task_thread_projection_hooks.go:43-48, 138-143`；`runtime_workflow_handlers.go:1504-1566` | 实读 |
| F14 | 没有任何 stale `human_review` 扫描 / 超时 / 升级机制；唯一 TTL 是 2h 的 chatops action token（IM-only） | 子 Agent 审计 + `docs/concepts/workflow-collaboration-accounts.md:282`（"超时后升级"仅为设计想法） | **子 Agent 结论，我未逐行复验** |
| F15 | 沙箱镜像有 python3/ripgrep/nodejs，**无 Playwright 浏览器** | `docker/runtime-base/Dockerfile:78-87` | 实读 |

> F14 与"react-components 那次交付实际停在哪个节点"是本方案仅存的两个未复验事实，见 §9。

---

## 3. 对原方案的评审结论

**采纳**：
- 诊断（缺前置告知）成立；
- 告知时机选在"人做决定的那一刻"（新建任务弹窗）成本最低；
- "一键切换到无远端依赖模板"可行（F12）。

**否决（两处致命）**：

1. **数据源用错。** 原方案前端读 `project.remoteProvider / remoteConnection`——正是 F2 里平台自己判定为"不证明任何事"的字段。后果不是偶发不准，而是**告警卡与真闸门互相打脸**：显示字段被写成 `gitlab` → 卡片绿；实际无已验证绑定 → `ci_ready_gate` 按 `cause=environment` 拦住（`internal/api/ci_ready_gate.go:90-93`）。用户会读成"平台自己骗自己"。
2. **推导方式错。** 原方案用 step ID + 字段名白名单猜依赖。两个问题：其清单里的 `pr_merge` **不存在**（真实 ID 是 `merge_sync` / `pr_open_and_merge`，grep 零命中），说明清单系脑补；更本质的是工作流**可自建**（`POST /api/v1/workflows` 接受任意 steps/edges），任何改名（`open_mr`）都会让红卡永久不出现。**会静默放行的告警比没有告警更糟**，它制造"已检查过"的信心——这正是本项目在拆的软闸门模式。

**四处与源码不符**（按原方案文字）：

| 原方案 | 实际 | 证据 |
|---|---|---|
| 「统一交付流水线共 12 步闭环」 | 13 步；"12 步"来自已过期的 `AGENTS.md:144` | F9 |
| `ci_ready_gate` "读取 `.multigent/runtime.json` 契约，核查测试/构建/lint 及健康端点" | 该步跑 13 项**静态仓库检查**，不读 runtime.json（那是 preview 引擎的契约）；且原方案**漏掉平台侧复算**：服务端在 step-complete 前重算并按 `repairable/waiting_pipeline/ci_failed/environment` 四分流拦截 | `internal/ciready/ciready.go:112-446`；`internal/api/ci_ready_gate.go` + `7684add7` |
| `create_pr` "会轮询等待 MR 关联的 GitLab CI 流水线" | 平台**没有**建 MR / 轮询的代码路径（轮询只存在于 `mga ci ready --wait`）；该步是 agent + skill，且 skill 教它填 `branch:` | §1；`internal/api/ci_ready_handlers.go`（`wait_seconds`） |
| 把 `qa` 列为一项前置依赖 | QA 工具链可运行性是项目/镜像属性，**无法从工作流定义推导**；保留即假数据 | F15 |

**验证计划不成立**：`workflow-prerequisites.test.ts` 无法执行（F11）。

---

## 4. 核心概念：交付模式（delivery mode）

把"这个项目这次能交付到哪一步为止"变成**平台已知事实**，而不是靠人脑记、靠 agent 猜。两个来源合流成一个判定：

- **供给侧**：项目是否有已验证远端绑定（F2 的 `verifiedBinding()`，不是显示字段）。
- **需求侧**：工作流的哪一步**声明**自己需要远端（新增 `step.Config` 标记，沿用 `platform_gate` 的既有约定，`internal/api/ci_ready_gate.go:17,160`）。

两者在**两个消费点**上各说一次同样的话：给 agent 的提示词（§5.1，止血）、给人的控制台卡片（§5.3，早告知）。推导规则**只有一份，在后端**。

---

## 5. 方案分层

### 5.1 第一层 · 提示词注入交付事实（最高收益，最小改动）

**目标**：agent 在 `create_pr` / `merge_sync` / `release` 这类节点上，**不再因为不知道绑定状态而烧轮数或编造引用**。

**改动点**：
- `internal/runner/runner.go` `workflowPromptContext`（`:892` 起）：该函数**已经**打开控制库并解析 `workspaceID`（`:903-909`），只需追加一次 `controlDB.VerifiedRemoteBindingFor(workspaceID, project)`（`internal/db/remote_binding.go:81`），把结论渲染成一段事实块，与现有 workflow/step/fields 上下文并列注入。
- 文案按三种情形分叉：
  - 有已验证绑定：给出 `path_with_namespace`（不含凭据），说明"远端可用，可创建 MR 并等待流水线"。
  - 无绑定 + 非 remote-required：明说"本项目**未**绑定已验证远端。本次交付终点是**任务分支**：不要尝试创建 MR、不要等待流水线；按 `branch:<name>` 约定上报，并如实标注无法远端验证的部分。"
  - 无绑定 + remote-required：明说"该步将以 `cause=environment` 被平台拦住，请**直接升级给人**，不要重试。"
- **同步修改内置 skill**（`internal/builtins/files/skills/git-pr-delivery/SKILL.md` Scenario B）：把"无条件填 `branch:`"改成"仅在提示词声明无远端时填 `branch:`，且必须在输出中显式标注'非真实 MR'"。否则 skill 与提示词两套指令并存。

**测试**：`internal/runner/` 新增一例（现有 `reviewer_prompt_contract_test.go` / `runner_prompt_test.go` 已是同形态先例），断言三种情形各自的关键词在场/不在场。**必须包含一条反向断言**：无绑定时提示词里**不出现**"创建 Merge Request"这类指令性措辞。

**已知边界**：
- 提示词是派发时渲染的，**run 中途新增绑定不会回溯影响已下发的 spec**；下一步派发时自愈。需在文档与本节明示，不得声称实时。
- `workflowPromptContext` 在 `controldb.OpenDefault()` 失败时 `return ""`（**fail-open**，现有行为）。这意味着极端情况下 agent 拿不到任何上下文——本层不改变该行为，但要在 §9 记为已知风险。

### 5.2 第二层 · 后端声明 + 派生只读字段（数据契约正确性）

**目标**：让"这一步需要什么"和"这个项目有什么"都成为**后端可答的问题**，前端退化为纯渲染器。

**改动点**：
- `entity.WorkflowStep.Config` 新增约定键 `requires_remote`（值 `"true"`），打在确实需要远端的模板步骤上：`unified-delivery-pipeline` 的 `create_pr`、`merge_sync`、`release`；`greenfield-delivery-pipeline` 的 `pr_open_and_merge`、`release`。判定函数与 `ciReadyGateStep` 同构（`internal/api/ci_ready_gate.go:151-166`）。
- 项目详情响应（`GET /api/v1/projects/{name}`）新增**后端派生只读**字段 `remoteBindingVerified bool`（由 `verifiedBinding()` 计算，F2）。**前端禁止再读 `remoteProvider`/`remoteConnection` 做判定。**
- 由于 `requires_remote` 位于 definition 内，`GET /api/v1/workflows` 与 `GET .../tasks/{id}/workflow` 天然带出，无需新端点。

**测试**：模板结构测试（断言这些步骤带标记、且**本地型模板不带**）+ 一个 handler 测试断言 `remoteBindingVerified` 来自已验证绑定而非显示字段（构造"显示字段有值、无已验证绑定"的项目，断言为 `false`——这条正是钉死原方案缺陷的回归）。

**边界**：改模板需**重新实例化**（`POST /api/v1/workflows`）才对线上项目生效；已实例化的旧 definition 无此标记，将被判为"不要求远端"。这与 `ci_ready_gate` 当时遇到的问题是同一类，必须显式记录，并考虑沿用其**ID 白名单兜底**策略（`ci_readyGateStepIDs`，`ci_ready_gate.go:25`）以免存量模板静默失去保护。

### 5.3 第三层 · 控制台前置告知（纯 UX，按第二层产物渲染）

**目标**：人在建任务时就看见"哪几步会因为没有远端而降级"。

**改动点**：`web/src/components/project/CreateTaskDialog.tsx` 读取所选 definition 的 `requires_remote` 步骤 + `remoteBindingVerified`，渲染警示卡。**不含任何推导逻辑**（原方案的 `analyzeWorkflowPrerequisites` 白名单猜测法整体不采纳）。
- 警示文案必须逐节点说明**后果类型**，不停留在"需要 GitLab"：`create_pr` → 产出非 MR 的 `branch:` 占位；`release` → 平台无打标能力（F10），实际靠 agent 自报。
- 提供 F12 的模板一键切换。
- **不自建工作流的漏判**由"未声明即视为不要求"承担，并在卡片上如实标注"该流程未声明远端依赖（自建流程需自行确认）"——**宁可显示不确定，也不显示假绿**。

**测试**：无自动化（F11 前端无测试基建）。引入 vitest 属独立决策，**禁止夹带**在本改动中。

---

## 6. 明确不做

- 不做平台内 diff 展示，不挪人工代码审节点（§1 产品前提）。
- 不新增无 IM 的人工提醒（§1 前提 2：已有 workbench 待办 + 徽标）。
- 不在前端猜依赖（原方案 §Proposed Changes 的推导函数）。
- 不读可伪造显示字段做判定（F2）。
- 不在本改动里引入前端测试框架。
- 不放宽 `internal/workflow/testpaths.go` 白名单。

---

## 7. 执行顺序与前置

顺序：**5.1 → 5.2 → 5.3**。理由：5.1 直接止血（阻止编造与空烧轮数）且零契约变更；5.2 是数据正确性地基，做完 5.3 才会薄；先做 5.3 会把一条猜出来的规则固化成产品语义。

- 5.1 前置：无。可独立提交。
- 5.2 前置：决定存量已实例化 definition 是否需要 ID 白名单兜底（见 §5.2 边界）；需要一次重新实例化窗口。
- 5.3 前置：5.2 已上线，否则前端无数据可读。
- 与既有排期关系：本方案不依赖、也不阻塞 `docs/runbook-gate-acceptance-2026-09-19.md` 的闸门真机验收；但 **5.2 的 `remoteBindingVerified` 判定测试应复用同一批 fixture**，避免两套绑定桩数据。

---

## 8. 验收（行为级，不接受"编译通过"）

5.1：
- 在一个**无已验证绑定**的项目上跑到 `create_pr`，取运行时实际下发的 prompt（`multigent log` / agent 会话记录），断言"未绑定 + 交付终点是任务分支"这段事实**在**、"创建 MR"指令**不在**。
- 断言 agent 输出的 `pr_url` 带显式"非真实 MR"标注。

5.2：
- `GET /api/v1/projects/{name}` 返回 `remoteBindingVerified`；构造"显示字段填了 gitlab、实际无绑定"的项目，断言为 `false`。
- `multigent workflow show <def> --format json` 里远端依赖步骤带 `requires_remote`，本地模板不带。

5.3：
- 浏览器实点（不接受仅 API 200）：未绑定项目选统一交付流水线 → 红卡出现且列出**具体节点 + 后果类型**；切 `tdd-review-loop` → 转为轻量提示；已验证绑定项目 → 显示就绪。
- 自建一个把 `create_pr` 改名的工作流 → 卡片必须显示"未声明"告警，**不得显示就绪**。这一条是本方案与原版的关键差异，必须真点一次。

---

## 9. 未验证项（请评审者重点看这里）

1. **F14**（无 stale review 扫描/升级）来自子 Agent 审计，我未逐行复验。
2. **实际事故链未取证**：原方案引用任务 `t-20260919-5pednj` 与项目 `test-react-component`（称 `remoteProvider` 为 null），但**这台机器上查不到该库记录**——本地只有 `react-components`（`work/repos/react-components`，远端指向 `localhost:8083`），其唯一 AI 痕迹是 2026-07-30 的单个提交 `d53b6b9`、至今未合入主干。**"该任务实际停在哪个节点、报了什么错"仍是 unknown**，而它是本方案最强输入。执行前应从内网部署侧取该 run 的 step 实例与错误原文补进 §1。
3. `wf-g837rxka` 这个**已实例化 definition** 的实际步骤数未核（本地无该库），13 步是代码模板数，不等于该定义实例。
4. 5.1 的提示词长度影响：新增事实块会占用上下文预算，未实测对各模型档位步骤的影响（历史上 `glm-5.3-flash` 在长思考链下有过截断事故）。
5. 分布式路径未单独验证：F6 表明提示词在控制台侧构建，但我未覆盖"运行节点离线/远端 DB 分离"部署形态下 `controldb.OpenDefault()` 解析到哪。若该形态存在，5.1 的事实块可能取错库——**必须在实现前确认**。

## 10. 连带影响（评审通过后需一并处理，否则会留下互相矛盾的记录）

- **`ecd7a9c6` 的 diff 面板已无消费方**：§1 前提 1 确定"不在平台内做 diff 展示"，且人工审节点不挪，则 `web/src/components/task/TaskDiffEvidence.tsx` + `GET /api/v1/projects/{name}/tasks/{taskId}/diff` + `internal/gitworktree/diff.go` 三者都没有调用方。建议整块删除，**只保留** `SanitizedDiffArgs` 的修复（那是独立真 bug：flag 拼在子命令之前导致 git exit 129）。
- **runbook 证据点 ② 随之失效**：`docs/runbook-gate-acceptance-2026-09-19.md` 的 ② 写的是"diff 证据面板浏览器实点"。要么改成"preview 实跑 + 跳转远端看 commit"的等价人工验证，要么删掉该格——不能留在脚本里当验收项。
- **`AGENTS.md:144` 的"12 步闭环"过期**（F9），需改为 13 步。与本方案无耦合，可单独修。
- **内置 skill 与提示词不得并存两套指令**：见 §5.1，Scenario B 必须同步改，否则等于给了 agent 一个"合规撒谎"的出口。
