# Current handoff

## Start here

Branch: `dev`.
Status: 原型生命周期有界性（合入主干即归档）、真实代码与实时 Preview 为单源真理（SSOT）及设计门主动特批豁免（UI+后端审计）定案并全量落地（ADR `2026-09-18-design-prototype-lifecycle-and-preview-ssot.md`）；方向 C Phase 2（测试数据沙盒 REST API、秒级重置、场景切换与 PreviewDrawer 交互控制台）全量完成并实机部署验证（提交 `6e110b81`，控制台与分布式节点零版本漂移）；生产环境部署与现场实测已全量闭环（方向 E Brownfield 存量只读扫描、阻断识别与 RBAC 闭环；方向 B Batch C-2 真实 GitLab CI runner 证据与 SHA 锚定 14/14 项全绿通过）；交付流水线物理双轨制分工与验收测试规格前置（ATD）必要性判定定案归档 ADR。
Feature Flag `MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS` 维持默认严格关闭 (`false`)，受控灰度具备服务端项目白名单硬门禁。
下一阶段排期：ADR `2026-09-19-review-round-cap-and-ci-ready-gate-hardening.md` 中"评审轮数强制升级"与"ci_ready 服务端复算并四分流"已落地（见 -1.6 / -1.7），剩余两项后置——人工打回归因 `rework`/`rescope` 二分 coupled 与永不重置的 `human_interventions` 计数、`ci_failed` 同项连续失败的 park 规则；三者都需一次带 CI 的真实项目全流程来取证，因此排在该全流程跑通之前不再扩张。随后推进 Greenfield vNext 带 ATD 的真实全流程（含双人 Mattermost 真实环境流转终验）。

Key status & deliverables:
-1.7. **ci_ready 闸门由服务端复算并参与路由（P2 第二步，同一 ADR）**：
   - **缺陷**：`OverallNotReady` 全仓只出现在 `ciready.go` 与 `ci_ready_handlers.go` 两处，工作流引擎从不消费；该步骤类型是 `agent_task`，所以 agent 一句 `mga task step done` 就能带着 `not_ready` 推进——"CI 闸门"是提示词。
   - **接线**：`POST /api/v1/runtime/tasks/{id}/workflow/step/complete`（每个 agent 步骤完成都走的路径）在推进前由服务端复算闸门（`internal/api/ci_ready_gate.go` + `runtime_workflow_handlers.go`）。评估逻辑从 HTTP handler 中行为保持地抽出为 `ciReadyEvaluation(ctx, ciReadyOptions{...})`，init 端点 `seed:true`（沿用补种契约）、闸门 `seed:false` 走 **`ciready.Verify`**——判定绝不写仓库（"平台进程不碰工作区"红线）。
   - **四分流而非一刀切**：`repairable`（仓库形状不合规，400 返工给研发）/ `waiting_pipeline`（HEAD 流水线未终态或尚未出现，**409 且不打扰人**）/ `ci_failed`（流水线终态非 success，400）/ `environment`（远端未绑定或凭据缺失，携带 `needsHuman`）。原三分流在实现时发现必须加第四类，理由与判定见 ADR"实现期修正"。
   - **闸门识别双通道**：`step.Config["platform_gate"] == "ci_ready"`（新模板声明式）∪ `ci_ready` / `ci_ready_gate` 步骤 ID 白名单（**存量已实例化模板同样受保护**，否则新闸门只覆盖新项目）。
   - **失败步骤不被捕获**：agent 上报 `status != completed` 时跳过复算——拦住一个正在报告问题的人会丢掉那份报告。
   - **人工打回重启预算**：`e-code-rework` 的 `review_rounds` 由原样透传改为重置；实测经引擎确定性自增后进入 `implement` 的值是 `1`（全新三轮从第 1 次尝试计起），断言按实测写而不是迁就预想。
   - **自动化验证证据**：`internal/api/ci_ready_gate_test.go`
     - 纯函数表驱动 7 例：ready 放行、仓库缺陷优先于等待、running/无流水线→waiting 且可重试、failed→ci_failed、未绑定→environment 且 needsHuman、not_ready 无失败项仍阻断；并断言"等待类判定绝不需要人"；
     - 真实 run + 真实临时仓库 4 例：未就绪仓库必须拦住、**普通步骤不受闸门影响（热路径回归安全）**、Config 声明的非同名步骤被识别、**闸门不在仓库里留下任何新文件**；
     - HTTP 接线 2 例：`mga task step done` 路径上未就绪返回 400 且 `run.ActiveStepID` 仍停在 `ci_ready_gate`；非闸门步骤返回 200 并推进到下一步；
     - `internal/workflow/reviewer_contract_verification_test.go`：`TestReviewerContractHumanReworkResetsAgentRoundBudget`。
   - **门禁**：`make test` 40 包 0 FAIL；`go build ./...` 通过。
   - **诚实边界与未验证范围声明**：
     - 未做真实 GitLab 联调：`waiting_pipeline` / `ci_failed` 两条分支目前只有构造出的响应对象在纯函数测试里覆盖，未对真实流水线跑过（真机验证需要一次带 CI 的完整任务）；
     - 未实现：`ci_failed` 的"同一失败项连续 2 轮才 park 给人"重复计数；等待/告警状态到控制台与 Mattermost 卡片的透出（现在只在 API 响应体里）；
     - 需运维动作：模板拓扑无变化，但 `e-code-rework` 重置属于已实例化数据之外——**要让线上项目生效必须重新实例化工作流**（`POST /api/v1/workflows`），否则数据库里的旧定义仍是原样透传；
     - 风险面：闸门位于所有 agent 步骤完成的必经路径上，若某项目 `Verify` 误判将卡住该步骤；缓解是响应体带明确 `cause/detail`，且失败上报不被拦截，但仍需在有真实项目上跑一轮才敢说可上线。

-1.6. **Agent 初审轮数上限由服务端强制执行（P2 第一步，ADR `2026-09-19-review-round-cap-and-ci-ready-gate-hardening.md`）**:
   - **缺陷**：`maxReviewRounds = 3` 此前只做确定性自增（`internal/workflow/store.go` `incrementReviewRounds`），出边条件只看 `self_review_verdict`。实测：第 3 轮仍报 `issues_fixed` 时 run 回到 `implement` 继续第 4 轮，而计数字段被 clamp 显示为 `3`——提示词承诺的"三轮封顶"在行为上不存在。
   - **为何不能靠模板条件修**：`compareWorkflowValue` 只支持 `eq / neq / exists / in`，无数值比较，模板无法声明 `review_rounds >= 3`。因此把裁决放进引擎而不是放进拓扑声明。
   - **实现**：`applyReviewRoundCap`（`internal/workflow/store.go`）在**路由求值用的副本**上，把已达上限的 `issues_fixed` 改写为 `escalate`；`chooseNextEdge` 单点接入，两条转移路径同时受益。判定只认平台自己维护的计数——`review_rounds` 缺失或非数字时**不改写**（越权猜测会误伤正常路由）。实例与事件上持久化的仍是 agent 自己提交的词元，Summary 追加 `[平台裁决] …（agent 原判：issues_fixed）`，让人分清"模型主动升级"与"平台接管决定"。
   - **自动化验证证据**：`internal/workflow/reviewer_contract_verification_test.go`
     - `TestReviewerContractRoutingForcesEscalationAtCap`：红→绿（修复前实测落到 `implement`）——第 3 轮报 `issues_fixed` 必须落到 `code_review`、run 活跃步骤为 `code_review`、留存的 agent 原判仍为 `issues_fixed`、Summary 含"平台裁决"；
     - `TestReviewerContractRoutingIssuesFixedBeforeCapStillReworks`：第 2 轮返工仍按 agent 意愿回 `implement`（证明不是无条件剥夺路由权）；
     - 既有 `TestReviewerContractRouting*` 四条（pass / rework / 主动 escalate / 非法词元拒绝）全部未改未弱化。
   - **门禁**：`make test` 40 包 0 FAIL（含 `internal/workflow`、`internal/api` 全量回归）。
   - **诚实边界与未验证范围声明**：
     - 未做真实模型联调：本轮只证明引擎在给定输入下的路由行为，未验证 reviewer-agent 在提示词与新行为之间的长期稳定性与打回率变化（这正是 ADR 要求用一次真实全流程跑通来取证的部分）；
     - 未实现（ADR 内已裁定、尚未编码）：人工打回归因 `rework` / `rescope` 二分、永不重置的 `human_interventions` 计数、升级案卷服务端拼装、`ci_ready` 结果参与路由与 `repairable / waiting_pipeline / ci_failed` 三分流；
     - 模板未改拓扑，因此**无需**重新实例化工作流即可生效（与 `rescope` 出边那部分不同，后者需要重新实例化）。

-1.5. **人工审核闸门显示真实 diff（P1，评审发现项："审批人在签自述"）**:
   - **缺陷**：审核面板只渲染 agent 自己声明的 `step.InputFields`，全平台没有 commit 区间的只读 diff 端点（唯一 diff 是 Copilot 预览轮次），`code_review`/`pr_review` 的人类看到的实际是"作者写的关于自己的文本"。
   - **能力层（`internal/gitworktree/diff.go`，纯只读）**：`Manager.DiffCommits(dir, base, head)` 返回 `{base, head, files[{path,oldPath,status,additions,deletions,binary}], patch, truncated, note}`。两侧强制 `^[0-9a-f]{7,40}$`（`ValidateCommitSHA`），refs / `HEAD` / `sha^` / `sha..sha` / `--output=…` 一律拒绝；先 `cat-file -e <sha>^{commit}` 确认对象存在（`ErrUnknownCommit` 供上层映射 404）；整段读取持有项目 Git 锁，避免与 worktree 建立/快照/清理交错。
   - **顺带修掉一个"被测试认证的假抽象"**：`SanitizedDiffArgs` 此前把 `--no-ext-diff/--no-textconv` 放在子命令**之前**，任何真实调用都会 `未知选项` 退出 129；它只有一个"字符串包含"断言的测试在为形状背书，生产代码从未调用过。现改为把 diff 专用开关插入子命令之后，并由 `DiffCommits` 的真实执行覆盖。
   - **端点（`internal/api/task_diff_handlers.go`，注册在主 mux 非 publicMux）**：`GET /api/v1/projects/{name}/tasks/{taskId}/diff`，复用 `projectTaskResourceGuard`（鉴权 + 跨项目 404）；默认区间取任务不可变的 `BaseCommit..CompletionCommit`（符合"确定性基线"红线），可显式传参但必须是 hash；worktree 已回收时回落项目 checkout 的对象库。
   - **控制台（`web/src/components/task/TaskDiffEvidence.tsx`）**：审核面板新增"代码变更"区块——文件数与 ± 行、逐文件状态（added/modified/deleted/renamed 含旧路径）、`base..head` 短哈希、可展开的行级着色补丁、截断时琥珀色告警；无提交记录/无本地 checkout 时显示可解释的空态而非报错弹窗。中英文 locale 与 `workflows.detail.changeDiff` 同步补齐。
   - **自动化验证证据**：
     - `internal/gitworktree/diff_test.go`：真实两提交仓库的 ± 行与补丁内容、add/delete/rename 三态、非法 revision 七类全拒、同 SHA 显式空、**仓库自带 `* diff=evil` + `diff.evil.external=/bin/touch` 探针文件未被创建**（宿主未被执行外部 diff 驱动）；
     - `internal/api/task_diff_handlers_test.go` 6 项：真 diff 200、5 类注入参数 400 且不泄漏内容、缺基线 409、未知提交 404、跨项目 404、未认证经完整路由 401；
     - `make test` 40 包 0 FAIL；`npx tsc -b` 退出 0 零输出；`make web` 与 `make build` 通过（dist/multigent 30M 内嵌控制台）。
   - **诚实边界与未验证范围声明**：
     - **未在真实浏览器点击验证**：本地库无带 `BaseCommit/CompletionCommit` 的真实任务，面板视觉效果与展开手感未经真人走查（编译与类型已通过，渲染未验证）；
     - 未接入：IM 审核卡片（Mattermost 侧仍只有自述字段）；`qa`/`self_review` 等 agent 步骤未强制引用 diff；
     - 未处理：单巨型提交的行级上限（当前 `MaxDiffLines=5000`、`MaxDiffBytes=400KB`、`MaxDiffFiles=200`，超限只给文件清单与说明）。

-1. **运行节点 ambient claim 凭据越界收口（P0 安全，评审发现项）**:
   - **缺陷**：`ClaimRuntimeRun` 的候选集含 `desired_runtime_node_id = ''`，仅按 `workspace_id` 隔离；而 run spec 的 env 合并了项目级环境变量与模型 Provider key（`runtimeProviderEnvForAgent`）。同 workspace 内任意持有效节点 token 的机器，轮询领取即可被动获得其它项目的凭据；join token 兑换出的节点凭据 `ExpiresAt` 被置空（长期有效）。
   - **裁定（fail-closed 且可解释）**：未指派 run 的 ambient 领取只在 **workspace 内 ≤1 个非 disabled 节点** 时开放；一旦出现第二台在线节点，未指派 run 一律不派发给"先问的节点"，必须由 `AgentWorker.DefaultRuntimeNodeID` 显式定址。`ambientClaimAllowed`（`internal/db/runtime_nodes.go`）。
   - **停摆不得静默**：领取返回空且确有 parked 未指派 run 时，响应携带 `notice`，说明滞留数量、在线节点数与解除方式（绑定 worker）——沿用"权限不足必须给文字反馈"的既有红线，避免第二台机器接入后被误判为节点假死。`ambientClaimNotice`（`internal/api/runtime_node_handlers.go`）。
   - **自动化验证证据**：
     - 红→绿：`internal/db/runtime_node_claim_scope_test.go`（`TestClaimRuntimeRunAmbientClaimRequiresSingleNode` 修复前实测 node-a 领到了 `proj-secret` 的未指派 run，修复后拒绝；`TestClaimRuntimeRunDisabledNodeDoesNotCount`）；
     - 边界：`internal/api/runtime_node_claim_scope_test.go`（`TestRuntimeNodeClaimHoldsUnaddressedRunFromSecondNode`：两节点均不得领取、被拒的 spec 响应不含凭据字段、定址后仅该节点可领取）；
     - 既有契约保护：`runtime_nodes_test.go` / `runtime_slot_test.go` 的租约与 generation 断言**未删除未弱化**，仅把第二个节点登记为 disabled，使其继续经由租约规则而非放置规则拒绝；
     - `go test ./internal/... ./cmd/...` 37 包全绿、0 FAIL；`go build ./...` 通过。
   - **诚实边界与未验证范围声明**：
     - 未实机验证：真实双节点环境的领取/心跳行为，以及现有单节点部署在升级后确实零行为变化（按 `liveNodes <= 1` 分支推定）；
     - 未处理（另立任务）：spec 携带长期凭据本身、节点 token `ExpiresAt` 置空、`ScopesJSON` 存而不校验、hostname 自报可合并节点身份；
     - 未做：Nodes 管理页对 "parked runs" 的可视化（当前只在 claim 响应中给出原因）。

-0.7. **原型生命周期有界性、代码与 Preview 单源真理 (SSOT) 及设计门主动特批豁免 (ADR & UI 交付)**:
   - **核心架构判定定案 (`docs/agent/decisions/2026-09-18-design-prototype-lifecycle-and-preview-ssot.md`)**：
     - **原型生命周期有界性**：OpenDesign 原型生命周期严格截至对应任务分支合入主干（PR merge），合入即归档，不留陈旧基线；
     - **单源真理 (SSOT)**：真实代码与沙箱 Preview 抽屉为唯一真理（结构性理由：代码包含原型无法表达的状态、路由、权限与异常降级行为），彻底免除反向同步原型的技术债负担；存量项目接入（Brownfield）直接以自身代码拉起 Preview，不碰 OpenDesign；
     - **MVP 三场景 Loop**：0 $\to$ 1 走 `greenfield`（带 OD 一次定调）；存量接入走 `brownfield`（无 OD）；日常 1 $\to$ N 走 `unified` + Preview 抽屉验收。
   - **控制台设计门主动透出特批豁免 (`web/src/components/design/`, `locales/`)**：
     - `DesignChoiceCard.tsx`：扩展 `DesignSource = 'existing' | 'generate' | 'waive'`，引入 `ShieldAlert` 图标与高对比琥珀色告警选中态；
     - `DesignSourceChoiceModal.tsx`：并排展示 3 选 1 卡片（生成、已有、特批豁免设计），选中豁免时动态展开必填理由输入框与审计说明；
     - `DesignGateFlow.tsx`：实现 `confirmWaiverDirect(reason)`，携带 `design_waiver_reason` 与 `design_waived: 'true'` 提交审批；
     - 国际化支持：中英文 locale 同步补齐 `choice.waive`、`waiverDirectPlaceholder` 与 `autoComments.directWaived`。
   - **后端自动化单测回归与安全防护**：
     - `internal/api/design_gate_snapshot_test.go`：新增 `TestDesignReviewApprovalSucceedsWithDirectWaiver`，严格校验无 OD 项目引用时携带合法理由直接放行、输入透传至下一阶段、并在任务审计日志留下完整操作痕迹；
     - `make test` 全仓库 52 个包 100% PASS，`npm run build` 前端编译零告警通过。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**：
     - 已验证：前端 TypeScript 编译、多语言完整性、后端 API 鉴权/豁免放行/任务日志记入单测、全量回归无破坏；
     - 待验证：在真实浏览器中点击该豁免卡片并提交，确认控制台界面的视觉体验与手感。

-0.6. **方向 C Phase 2：测试数据沙盒 REST API、秒级重置与 PreviewDrawer UI (Commit `6e110b81`)**:
   - **核心引擎契约扩展与状态机完备 (`internal/fixturesandbox`)**：
     - `provisioner.go`：新增 `TaskStatus`（查询契约、引擎、相对存储、当前场景、可选场景清单、Lease ID、重置次数与租期）、`ResetTask`（CAS 驱动重置任务私有 DB）与 `SwitchScenario`（切换并冻结不同场景的数据集基准）；
     - `artifact.go`：修复 `Store.Reset` 返回值，确保原子递增后的最新 `Lease` 结构（包含递增后的 `ResetCount`）正确回传调用方；
     - 纯函数单测：`go test -race -v ./internal/fixturesandbox/...` 全量 25/25 PASS。
   - **REST API 端点实现与安全防护 (`internal/api/fixture_sandbox.go`, `server.go`)**：
     - `GET /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox`：由主 mux 注册（`withTokenAuth`），`checkProjectAccess` 鉴权，无契约工程优雅降级返回 `{"hasContract":false}`，绝不阻塞任务预览；
     - `POST /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox/reset`：严格执行 `checkProjectOperator` RBAC 门禁，调用 CAS 重置引擎恢复至不可变快照，写入结构化审计日志 `task.fixture_sandbox.reset`；
     - `POST /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox/scenario`：严格执行 `checkProjectOperator`，对未在契约中声明的非法场景严格 fail-closed 拦截（HTTP 400），写入结构化审计日志 `task.fixture_sandbox.switch_scenario`；
     - 端点集成测试：`internal/api/fixture_sandbox_test.go` 覆盖未认证 401、访客（Viewer）403、执行者（Operator）200、非法场景拦截 400、无契约降级 200 与任务不存在 404，全量 PASS。
   - **控制台交互集成与双向重载联动 (`web/src/components/task/PreviewDrawer.tsx`)**：
     - 在预览抽屉顶部工具栏优雅内嵌“🎭 数据沙盒”控制面板（仅在契约工程可见，无契约工程静默隐藏）；
     - 提供当前场景标识、测试场景切换单选列表、相对存储路径只读展示（严防宿主绝对路径泄漏）；
     - 提供“一键重置沙盒 (<100ms)”操作，点击后秒级抹除写污染并自动重载预览 iframe 呈现纯净数据；
     - 包含重置次数徽章（`↺N`）与剩余租期动态显示；
     - 完整接入中英文双语国际化词条（`web/src/locales/`）。
   - **全量构建、回归测试与实机部署**：
     - 前端编译：`npm run build` TypeScript 与 Vite 零告警通过；
     - 全量回归：`make test` 52 个包 100% PASS (0 failure, 0 races)；
     - 一键构建：`make build` 完整内嵌静态资源产出单文件二进制；
     - 实机零漂移部署：交叉编译后同步至控制面（`linux/amd64`）与运行节点 `ubuntu-node-2`（`linux/arm64`），双方版本均为 `6e110b81`，心跳实时在线；实测未认证 401 拦截与无契约任务 `{"hasContract":false}` 正常降级。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**：
     - 已闭环验证：单元测试、API 鉴权门禁、前端构建、双端部署与版本对齐；
     - 待后续在契约工程真实预览场景中由真人通过浏览器界面走查场景切换手感。

-0.5. **真实环境部署与现场实测全闭环 (Live Deployment & Verification, Commit `a7741433`)**:
   - **双节点零漂移部署与心跳验证**：
     - 控制面（`amd64`）：服务正常运行，`/api/v1/version` 精确返回 `{"ok":true,"version":"a7741433"}`；
     - 运行节点（`arm64`）：`multigent-runtime-node.service` 重启成功，版本精确返回 `multigent a7741433`；
     - 拓扑一致性：控制面数据库 `runtime_nodes` 验证节点状态为 `online`，版本 `a7741433`，心跳实时刷新，双端无版本漂移。
   - **方向 E Brownfield 存量项目现场实测**：
     - 实测存量项目（`api-key-hub`，Java 21 Spring Boot + Gradle + React 18 / Vite / NPM）：纯函数扫描精确识别双服务架构、Gradle Wrapper 权限及锁文件完整性，评估为 `status: "ready"`，推导合法 `.multigent/runtime.json` 契约；
     - 实测缺失锁文件项目（`todo-api`, `ias-auth-center`）：精确阻断并报告 2 项 `SeverityBlocking` 缺陷，给出精准修复建议（`go mod tidy`、`npm install --package-lock-only`），符合 Fail-Closed 预期；
     - RBAC 门禁实测：未认证请求严格返回 401；项目访客（Viewer）调用严格返回 403 `project_operator_required`；项目执行者（Operator）调用返回 200 正常就绪报告。
   - **方向 B Batch C-2 真实 GitLab CI Runner 流水线证据与 CLI 实测**：
     - 通过 `POST /api/v1/projects/{name}/remote/verify` 完成项目远端只读校验并固化绑定；
     - 颁发携带 `task.use` 的 Agent 运行时 Token，调用 `POST /api/v1/runtime/ci-ready?wait_seconds=10`：14/14 项检查全绿通过；
     - 关键闸门 `pipeline_evidence` 成功命中 HEAD 提交的真实 GitLab CI Runner 流水线（Pipeline 1042），精确提取 `build:backend`、`build:frontend`、`test:backend`、`lint:backend` 4 项作业成功状态；
     - 沙箱 CLI 实测：在运行环境中直接执行 `mga ci ready --wait 10`，结果 100% 吻合。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**：
     - 本次已实地验证 CI Runner 真实证据提取、Brownfield 存量扫描与 RBAC 闸门；
     - Greenfield vNext 带 ATD 的从零端到端流水线（包含前置 OpenDesign 原型设计门、ATD 用例生成与双人审批）留作下一步综合验收。

-0.4. **交付流水线物理双轨制分工与验收测试规格前置 (ATD) 必要性判定 (ADR 2026-09-18)**:
   - **双轨制物理隔离架构确立**：
     - **日常快速迭代流水线 (`unified-delivery-pipeline`)**：保持精简高效（澄清 $\to$ 澄清审 $\to$ 编码 $\to$ 初审 $\to$ CI 闸门 $\to$ 代码审 $\to$ 合并 $\to$ 后置 QA $\to$ 发布；此处为关键主干摘要，完整 13 步拓扑以 `internal/workflow/store.go` 为准）；坚决不插入前置测试规格设计（ATD）节点与前置设计门，杜绝日常开发阻力与模型上下文膨胀。
     - **全新工程交付流水线 (`greenfield-delivery-pipeline` vNext)**：针对从零起跑、高保真交付的新工程，完整保留 `design_review`（OpenDesign 高保真原型门）与 `acceptance_test_design`（ATD 验收测试规格前置设计门）。
   - **ATD 契约锚点与稳定性归因**：
     - 确认 ATD 节点产出的 `test_spec_manifest` 由纯函数强类型校验器（`internal/workflow/testspec.go`）硬门禁把关（占位词拦截、字段校验），向研发提供机器可读测试目标，杜绝大模型在无基线时写“假测试”（断言颠倒）；
     - 正确归因 `t-20260910-pu8mhs` 成功经验：它验证了“设计确认 + 独立初审 + 合并前 QA 闸门”骨架的有效性，而 ATD 是给骨架增加事前契约保障。
   - **下一步实证方案**：
     - 将 Batch C-2（真实 GitLab CI runner 联调）与 Greenfield vNext 带 ATD 的从零交付结合，实地获取手感稳定性与往返打回率的客观数据。
   - **ADR 归档与诚实验证声明**：
     - ADR 路径：[`docs/agent/decisions/2026-09-18-delivery-pipeline-duality-and-atd-necessity.md`](file:///Users/imac/Documents/code/github/multigent/docs/agent/decisions/2026-09-18-delivery-pipeline-duality-and-atd-necessity.md)；
     - 验证说明：本提交为纯文档（docs-only），无代码面变动，未重复运行全量测试套件；最新代码提交 `e27b8eae` 经全量单测验证 100% PASS。
-0.3. **方向 E Brownfield 存量仓库受控接入与就绪门禁 (P0, 5 条裁决修订全闭环)**:
   - **纯函数探测与评估引擎 (`internal/brownfield`)**:
     - **多语言栈零副作用探测**：`Detect` 纯函数只读扫描 Node (npm/pnpm/yarn/bun)、JVM (Gradle/Maven/wrapper 权限)、Go (go.mod/go.sum)、Python (poetry/pipfile/requirements)、Rust (Cargo) 技术栈、构建脚本、CI/CD 与容器编排；
     - **分层确定性就绪判定（P2 修订 4）**：强制锁文件生态（npm/go/rust）缺锁文件或 Java 缺 wrapper/无执行权限时，判定为 `SeverityBlocking` (`not_ready`) 并给出可解释修复指引；Python `requirements.txt` 无哈希判定为 `SeverityWarning`（弱确定性），给出建议但不阻断存量接入；
     - **契约推导与 Conformance 校验（P2 修订 5）**：`SynthesizeRuntimeSpec` 依据探测结果自动推导合法的 `.multigent/runtime.json` 契约（前端/后端服务目录、启动命令、端口、健康检查与超时）；在单测中严格通过 `preview.LoadRuntimeSpec` 反解析与字段校验。
   - **6 步受控就绪流水线 (`internal/workflow/brownfield.go`, `brownfield-onboarding-v1`)**:
     - **受控流转拓扑**：`readonly_scan -> baseline_pin -> verify_build -> evaluate_readiness -> human_signoff -> materialize_contract`；
     - **沙箱契约落盘与零触碰仓库原则（P1 修订 1）**：服务端在 `evaluate_readiness` 步骤仅产出结构化 `readiness_report` 与 `synthesized_runtime_json` 工件；人工审核确认后，由 `materialize_contract` 步骤在 **Docker 沙箱内** 由 Agent 落盘 `.multigent/runtime.json` 并提交基线 commit，100% 恪守“平台进程不碰仓库工作区”架构红线；
     - **沙箱隔离构建红线（P1 修订 3）**：`verify_build` 严格限定在 Docker 容器沙箱内执行（采用 `Detector` 推荐的 `ProfileBase` 或 `ProfileJVM21`），严禁在平台主进程或控制面宿主机执行任意构建脚本；
     - **审核规则合规**：经 `TestHumanReviewGateOutputsAreNeverHandRequired` 回归约束，`human_signoff` 步骤的 `approved_runtime_json` 标记为 `Optional: true`，杜绝要求人类手填产物。
   - **端点路由与 RBAC 严格分级（P1 修订 2）**:
     - `POST /api/v1/projects/{name}/brownfield/scan` 注册在主 mux（`withTokenAuth`）并强门禁 `checkProjectOperator`（未认证 401，Viewer 403 `project_operator_required`，Operator / Manager 200 正常扫描）；
     - `POST /api/v1/runtime/brownfield/evaluate` 注册在 `runtimeMux`（校验 `task.use` capability）。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**:
     - 引擎纯函数、状态机流转与 API 权限已全部由本地自动化单测通过；但在真实内网环境下涉及企业私有 Nexus/Maven 401 凭据缺失与外部中间件（MySQL/Redis）依赖的实际排查，需在首个真实 Brownfield 业务仓库试点时做场景闭环。
   - **自动化验证证据**:
     - `go test -race -v ./internal/brownfield/...`：7/7 PASS；
     - `go test -race -v ./internal/workflow -run 'TestBrownfield'`：2/2 PASS；
     - `go test -race -v ./internal/api -run 'TestBrownfield'`：3/3 PASS；
     - `go test -race ./internal/workflow/...`：全量 PASS（17.18s）。
-0.2. **方向 B 前置验收测试设计 Batch B / Batch C-1 及平台全链路接缝闭环**:
   - **真实代码与提交事实核实 (Audit & Reality Reconciliation)**：
     - 核查确认 Batch B 全部条目（commits `5acbe42e`, `981153fd`, `75bdfdff`, `50e61e88`）与 Batch C-1 本地真实仓库试点（commit `62340a12`，`internal/api/batch_c_pilot_test.go`）均已在 `dev` 分支合入并有完备单测保障；
     - 纠正了 `docs/acceptance-test-design-plan.md` 中滞后的“Batch B/C 未开始”状态，对齐代码与提交历史客观事实。
   - **Batch B 关键能力确认**：
     - **B-a 返工可追溯**（commit `5acbe42e`）：`qa_signoff` 打回时服务端自动把测试矩阵中 `failed`/`blocked`/`unexecuted` 项聚合为结构化 `qa_rework_items`（逐项富化 `manifest.expected_result`），经 `e-qa-rework` 送返 `implementation`；同时在 `review_comments` 前追加人类可读失败清单，即使审批人留空也不会丢失失败上下文；
     - **B-b QA checkpoint 权限与白名单门禁**（commits `981153fd`, `75bdfdff`, `50e61e88`）：QA 步骤声明 `touched_paths`，服务端校验严格执行白名单控制（仅限测试源码/夹具/探针，严禁碰触业务代码与 CI/配置）；`verifyQATouchedPathsAgainstWorktree` 强比对真实 worktree 的 `git status --porcelain -z`，双向严格一致且 fail-closed；
     - **全链路研发映射**：`test_implementation_evidence`（Case ID $\to$ 测试文件/结果映射）从实现节点贯通初审、代码审、QA 到打回返工。
   - **Batch C-1 本地真实仓库试点 (`internal/api/batch_c_pilot_test.go:TestBatchCPilotGreenfieldVNextOverRealRepo`)**：
     - 基于真实 git 仓库 fixture 驱动 Greenfield vNext 全生命周期，验证验收测试规格在真实工程结构下的流转。
   - **Batch C-2 CI Runner 流水线证据与 SHA 强锚定验证 (`internal/api/batch_c_pilot_test.go:TestBatchC2RunnerPipelineEvidenceAndSHAAnchor`)**：
     - 验收测试方案 §7 Batch C 条目 5 自动化闭环：验证 CI Runner 执行流水线与 QA 矩阵证据严格锚定到不可变的同一完成提交 SHA：
       1. **正向锚定通过**：Pipeline SHA == Worktree Completion SHA $\to$ `pipeline_evidence` 判定通过，提取 Runner 执行的 Job 详情（如 `build:backend`, `test:backend`）；
       2. **SHA 漂移硬拦截（Fail-Closed 1）**：Worktree 产生未推送或未构建的新 Commit $\to$ 立即 400 阻断（`no pipeline observed for current HEAD`），杜绝坏代码偷跑；
       3. **Runner 作业失败硬拦截（Fail-Closed 2）**：CI 流水线执行失败 $\to$ 立即判定 `overall: not_ready`（`terminal status required: success`）。
   - **平台级自动化接缝全闭环验证 (`internal/api/runtime_workflow_seam_test.go`, `TestRuntimeWorkflowSeam_GreenfieldVNextPromptAndStepDone`)**：
     - 针对此前 Reviewer 切片时留下的未闭环边界（`BuildTaskPrompt` 运行时渲染 $\to$ `mga task step done` HTTP 完成端点 $\to$ 引擎状态机 $\to$ 研发返工提示词携带上下文），实现单一全自动化集成测试：
       - `acceptance_test_design`：`runner.BuildTaskPrompt` 动态渲染步骤契约 $\to$ 非法 manifest 被服务端 400 拦截 $\to$ 合法 manifest 经 HTTP `POST /api/v1/runtime/tasks/{id}/workflow/step/complete` 成功入库并推进；
       - `implementation`：`BuildTaskPrompt` 校验收到结构化 `test_spec_manifest`，必须提供 `test_implementation_evidence` $\to$ HTTP 提交证据推进；
       - `self_review`：`BuildTaskPrompt` 校验收到测试证据与规格 $\to$ HTTP 提交 `self_review_verdict: "pass"` 推进；
       - `code_review`：人工审核批准推进；
       - `qa`：`BuildTaskPrompt` 校验收到研发证据与测试规格 $\to$ QA 尝试篡改业务源码（`server.go`）被 HTTP 400 fail-closed 拦截 $\to$ 合法探针测试（`tests/revocation_probe_test.go`）匹配真实 git porcelain 成功推进；
       - `qa_signoff`：留空 comments 提交打回 $\to$ 服务端 `enrichQARejectionComments` 自动富化失败清单，生成带 `expected_result` 的结构化 `qa_rework_items`；
       - 返工 `implementation`：`runner.BuildTaskPrompt` 验证研发 Agent 提示词精准注入 `qa_rework_items`、富化后的打回审阅意见及原始规格基线。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**：
      - **诚实边界与现场实测闭环 (Honest Boundaries & Live Verification)**：
        - **Batch C-2（真实环境部署级联调）**：已在部署环境中由实测闭环（见 §-0.5）。通过 `POST /api/v1/projects/{name}/remote/verify`、`POST /api/v1/runtime/ci-ready?wait_seconds=10` 及 `mga ci ready --wait 10` 成功联动真实 GitLab CI Runner，提取 Pipeline 1042 与 4 项 Job 成功证据；
        - **Roadmap 第 1 步已在真实 VM 环境完成实证闭环**。
   - **自动化验证证据**：
     - `go test -race -v ./internal/api -run 'TestBatchC'`：PASS (10.84s, 2/2 tests passed, 0 races)；
     - `go test -race -v ./internal/api -run 'TestRuntimeWorkflowSeam_GreenfieldVNextPromptAndStepDone'`：PASS (0 races, 3.88s)；
     - `make test`：全仓库 52 个包 100% PASS (0 failure, 0 races)。
-0.1. **Reviewer 独立基线契约 (`de917d62`) 真实验证切片全量闭环**:
   - **活库基线与断代核实**：确认 SQLite 数据库（`~/.multigent/multigent.db`）中 `workflow_runs` 表由于环境重建历史运行记录为 0；为防范“只改模板文本、无实测运行闭环”的断代风险，插队完成真实执行切片。
   - **工作流状态机四路流转全覆盖 (`internal/workflow/reviewer_contract_verification_test.go`)**：
     - **Pass 路由**：评审员按独立预期基线完成比对，提交单一词元 `pass`，准确进入下一阶段 `ci_ready_gate`；
     - **Rework 打回与平台确定性计数**：评审员识别缺陷提交 `issues_fixed`，工作流准确退回 `implement`；平台在 `rework` 边上强制执行确定性自增（`review_rounds`: 1 $\to$ 2），杜绝模型漏报自身轮数导致死循环；缺陷报告准确透传至 `review_comments`；
     - **Escalate 达限升级**：达到 3 轮上限后提交 `escalate`，工作流准确升级至人工 `code_review`，并原子传递结构化 `escalation_case` JSON 案卷；
     - **Fail-Closed 词表防御**：针对自然语言结论（如 `"approved"` / `"LGTM"`）实施硬拦截并报错，任务严格停留在 `agent_self_review` 步骤，拒绝非法放行。
   - **执行时提示词契约下发验证 (`internal/runner/reviewer_prompt_contract_test.go`)**：
     - 在中英双语（`zh-CN` / `en`）模板下，通过 `runner.BuildTaskPrompt` 验证评审员 Agent 任务提示词完整注入 7+1 条款、前置输入与必填输出规范。
   - **真实大模型「带负荷」审查实测验证 (Live Model Load Verification)**：
     - 搭建包含 AC-1 ~ AC-4 的令牌撤销任务，并在 Diff 中故意埋入 2 处真实缺陷（403 替代 401 破坏契约、Bearer Token 明文日志泄漏）及单测断言掩盖；
     - 调动真实 Reviewer Agent 执行审查，5 大严苛指标全达标：
       1. **独立基线**：正文第 1 部分完全独立推演行为、数据流、边界与测试切面，不被作者实现带跑偏；
       2. **区间逐条比对**：锁定 Base/HEAD SHA 区间逐一比对 AC-1 ~ AC-4；
       3. **缺陷捕获**：超额抓出 403 违约、明文 Token 泄漏、端点漏写及单测反向掩盖错误 4 处缺陷；
       4. **单一词元**：严格输出单一词元 `"issues_fixed"`，无自然语言污染；
       5. **案卷结构化**：输出标准的 4 项包含契约、行号、影响、最小修复与缺失验证的标准 JSON 案卷。
   - **诚实边界与未验证范围声明 (Honest Boundaries & Caveats)**：
     - **模型侧单测 vs 平台端到端**：本次测试确证了“大语言模型能高度遵从该契约并精准识别缺陷”，但平台级自动化接缝（`BuildTaskPrompt` 运行时渲染 $\to$ Runner SSE $\to$ 容器内 `mga task step done` $\to$ 引擎自动解析并推进）未在单个 live loop 串联。接缝闭环纳入 Batch C 真实项目试点；
     - **“亲自重跑构建”验证形态**：模型在虚假断言下正确以 `unverified` 拒绝放行，但“真跑构建无误后予以放行”的正面形态未被实测；
     - **Roadmap 第 1 步状态保持「待验证」**：`runtime_probe_test.go` 单测通过，但本交付无真机 probe 联调与 VM conf 审计证据，不得从清单划掉。
   - **自动化验证证据**：
     - `go test -race -v ./internal/workflow -run 'TestReviewerContract'`：4/4 PASS；
     - `go test -race -v ./internal/runner -run 'TestReviewerPromptContract'`：2/2 PASS；
     - 真实模型实测产物 JSON 原件留存并经审查核准；全包回归 `go test ./internal/workflow ./internal/runner`：0 报错。
-0. **方向 A 独立工具箱 Phase 0 收口与价值审计 (commits `37f610cb`, `893b62e6`, `fc5b8742`)**:
   - **Phase 1 挂起决策**：依据 zcode 审查意见，Phase 0 降级手册已打通 6 步手工发版路径；当前单团队单部署场景下开发 `mgt` CLI 成本收益倒挂，且容易脱离核心业务质量进入工程舒适区；正式挂起 Phase 1，待分布式拆分或出现真实发布阻断时触发。
   - **零代码平台离线降级手册整改**：§1.1 修复表格中 `$CI_COMMIT_TAG` 说明；§1.2 交付可用的 `tools/ciready-check` 工具；§3.3 纠正缓存卷挂载点为真实 `/tmp/multigent-cache` 体系并注明 fallback 链对应关系。
   - **选型与规划基线锁定**：`docs/roadmap-reprioritized-2026-09-12.md`、`docs/standalone-toolbox-decoupling-plan.md`、`docs/test-data-fixture-sandbox-plan.md` 已提交入 `dev` 分支，决策依据永久纳入代码历史。
   - **零代码平台离线降级手册整改与参数真实化**：
     - §1.1 修复表格中被转义吞掉的 `$CI_COMMIT_TAG` 规则门禁说明；
     - §1.2 废弃不可执行的 `go run -e` 伪命令，落地实测可行的内置工具 `tools/ciready-check/main.go`（带单测 `main_test.go` 全 PASS），提供无服务直调 `ciready` 检查与 `--seed` 补全能力；
     - §3.3 纠正缓存卷挂载点与环境变量体系为平台真实的 `/tmp/multigent-cache`（遵从 p15 canary 教训，防 root 权限踩坑），补齐 labels、linked worktree 父仓库挂载说明，并明确注明命令与平台 `engine.go:417-434` 运行时 fallback 链的自适应对应关系。
   - **诚实验证说明**：规划文档与 Runbook 初始三笔提交为纯文档（通过 `internal/ciready` 单元测试与真实活库 0 运行记录核实）；本次整改新增 `tools/ciready-check` 经自动化测试验证通过（`go test -v ./tools/ciready-check` PASS）。
   - **后续插队行动**：实测确认 `de917d62`（Reviewer 独立基线契约）在 SQLite 数据库中真实运行记录为 0；在投入 Batch B/C 编码前，插队半天进行真实运行验证切片。
0. **Preview Copilot Turn Receipts 切片 B（已正式放行）**:
   - **事务性回执与 Group-Slot 单事务原子化**：`internal/previewreceipt` 支持状态机 `CAPTURED → COMMITTING → COMMITTED / REVERTING → REVERTED / REVERT_FAILED`；`Prepare`、`Finalize`、`Abort`、`Recover` 均在单 DB 事务原子批处理中完成；租赁防御杜绝租期超时误抢占。
   - **人工审核收编与不可变基线保障**：审核通过时自动收编，生成携带 `Multigent-Commit-Intent` Trailer 的 Checkpoint Commit；启动自愈按行严格校验 Intent Trailer，准确区分已提交与未提交。
   - **真实生产执行链路与沙箱严格隔离断言**：`previewDefaultAgentRunner` $\to$ `multigent exec` $\to$ `runner.Runner` $\to$ `runenv.DockerProvider` $\to$ `sandbox.BuildArgs` $\to$ 真实 Docker 容器；严格拒绝任何 `ExtraVolumes`、`CredentialMounts`、`docker.sock` 与宿主目录挂载，卷挂载数量严格等于 1（仅隔离 clone 目录挂载为 `/workspace:rw`）。
   - **零泄漏审计与生产 Runbook**：输出 `docs/runbook-preview-turn-receipts.md`，彻底排除破坏性命令（`reset --hard` / `clean -fd`），明确 Fail-Closed 永久加锁与无自动恢复接口的逐路径处置 SOP。
   - **发布审批状态**：用户已确认放行 Batch 5.1。当前处于受控灰度就绪状态。

0.1. **Preview Copilot Guarded Skill Profiles (Task 3.2 & Batch 3.2.1, 完整性与权限硬化闭环)**:
   - **服务端受控技能白名单与固定基线摘要**：预置 4 类技能 Profile (`ui-polish`、`a11y-remediation`、`responsive-layout`、`form-logic`)，`DefaultTrustedBuiltinSkillDigests` 固化官方 SHA-256 基线；`ResolveSkillProfileGuidance` 强制比对磁盘实际计算摘要与基准清单，摘要不匹配或未配置基准时严格 400 Bad Request fail-closed 阻断注入。
   - **Skill 修改接口 RBAC 硬化**：`PUT /api/v1/skills/{name}` 补齐 `checkCurrentWorkspaceAdmin`，普通登录用户调用直接 403 Forbidden (`workspace_admin_required`) 拦截，杜绝越权修改 Profile 依赖的 Skill 内容。
   - **脚本执行中立化安全红线**：预览 Copilot 严禁将技能执行附件（`.sh`）挂载进沙箱或执行；仅提取纯声明式 Markdown 规范指引注入提示词；技能目录下添加未授权脚本会自动破坏摘要导致加载阻断。
   - **全链路前端可视化集成**：`PreviewDrawer.tsx` 动态加载 Profile 列表，提供直观的技能药丸徽标切换、快捷操作自动匹配 Profile、用户消息徽标实时展示生效 Profile。
   - **自动化验证证据**：`TestPreviewSkillProfiles` + `TestPutSkillPrompt` (10/10 PASS)、回归套件 (23.28s PASS)、`npm run build` (PASS)、`make test` (PASS)、`make build` (PASS)。

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
7. **设计确认闸门执行治理与模型适配 (Design Gate Execution Governance & Model Alignment)**:
   - **需求对齐与角色边界**: `designPendingPrompt` 强制读取同任务前置节点产出的 `approved_requirement` / `requirement_draft` 全文，替代 v0 原始粗糙 Prompt；明确框定为 UI 原型专家，限制在 OD 沙箱内产出单页 HTML/Mock 数据，严禁越界修改业务代码或写生产后端。
   - **项目唯一约束自愈**: `handleDesignStart` 增加自动清理机制；若 OD 容器内存在同名孤儿空项目（`proj_mg_{taskID}`），先执行删除自愈再创建，杜绝 `UNIQUE constraint failed: projects.id`（502）冲突。
   - **容器只读与 16k Token 硬封顶治理**:
     - OpenDesign 运行于 `--read-only` 根只读容器内，内置 BYOK 适配层（`byok-opencode.js`）硬编码 `DEFAULT_OUTPUT_TOKEN_LIMIT = 16_384`。
     - 深度思考模型（如 `qwen3.8-27b`）在面对 5 万字系统规范与需求时，思考链输出可达 5.4 万字（耗尽 16,384 tokens），在完成思考调用 `bash` 写文件前被上游推理服务以 `reason: length` 截断，导致 `no_artifact` 失败。
     - 全量实测内网网关（CCR）可用模型，明确选型策略：
       - **默认模型：`glm-5.3-flash`**：思考链极克制（~90 tokens），单次仅消耗 2,891 tokens 即可通过 `bash` 成功落盘完整动效/双模态原型，耗时 15 秒，Anthropic 协议与工具调用 100% 兼容。
       - **备用模型：`qwen3.6-35b`**：消耗 1,167 tokens，耗时 9 秒，但版式丰富度略逊。
       - **禁用模型：`deepseek-v4-flash`**（网关未对齐工具调用，`tool_calls: null`）与 **`qwen3.8-27b`**（未受控思考链耗尽 16k tokens 截断）。
     - `internal/api/od_client.go:odDefaultModel` 已正式切换为 `glm-5.3-flash`。

## Non-negotiable boundaries

- QA 门禁必须严格位于主干合并之前（`code_review -> qa -> qa_signoff -> pr_open_and_merge`）。
- 模板依赖安装必须 100% 依赖确定性 lockfile，严禁在 lockfile 缺失时自动降级为动态拉取。
- 快照文件提供端点必须校验任务所属项目，且对原型 HTML 内容实行沙箱隔离。
- `approved_design_snapshot_path` 和 `approved_design_html` 严禁信任客户端请求输入，必须由服务端抓取生成。
- `gradlew` 必须保持 `0755` 权限且必须内置 `gradle-wrapper.jar`。
- 项目标识 `Project.Name` 严格受 `validateWorkspaceObjectName` 约束，只能包含英文、数字、`-`、`_` 与 `.`，严禁包含中文与空格；若包含频道开通配置，开通失败必须原子回滚项目。
- **工作流人机角色严格解耦**：`agent_task`（自动化步骤）与 `human_review`（人工审核闸门）严禁复用相同的 `actorRole`（如禁止同时使用 `owner-engineer`）。自动化步骤统一使用 agent 后缀角色（`developer-agent`、`reviewer-agent`、`release-agent`、`qa-agent`、`pm-agent`），人工审核闸门统一使用责任人角色（`owner-engineer`、`product-owner`、`qa-owner`）。
- **设计门模型选型与 Token 边界**：OpenDesign 容器内单次输出被 `byok-opencode.js` 硬限制为 16,384 tokens（容器 `--read-only` 无法动态修改）。设计门严禁接入思考链未调校或会无界膨胀消耗超过 10,000 tokens 的纯思考模型；选型模型必须在 CCR 下实测具备完整的 Anthropic Messages 格式 Function/Tool Calling 能力（首选 `glm-5.3-flash`）。

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
- `internal/api/od_client.go`: `odDefaultModel = "glm-5.3-flash"` (commit `0b8363b4`)
- `docs/agent/decisions/2026-09-10-design-gate-model-and-token-limits.md`: ADR 完整记录选型与截断排查
- CCR 网关实测证据：
  - `glm-5.3-flash`: output_tokens 2891, stop_reason tool_use, duration 15s (PASS, 完整生成富交互动画原型)
  - `qwen3.6-35b`: output_tokens 1167, stop_reason tool_use, duration 9s (PASS, 完整生成 HTML 模板)
  - `qwen3.8-27b`: output_tokens 16384, stop_reason length, reasoning 54101 chars, artifactCount 0 (FAIL, 思考链耗尽上限截断)
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

