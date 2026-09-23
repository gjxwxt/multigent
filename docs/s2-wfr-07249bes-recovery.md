# S2 历史 run 恢复方案：wfr-07249bes（待批准后执行）

> 性质：**一次性的历史 run 手工恢复记录**。按评审决定（2026-09-22），此类操作
> 单独记录、不计入 "零手工救场" 验收证据。本文不描述任何新能力。

## 1. 现状快照（恢复前证据，已于 2026-09-22 核实保存）

| 对象 | 状态 | 关键值 |
|---|---|---|
| 父 run `wfr-07249bes` | active @ `parallel_workstreams` | task `t-20260922-7x6tca`（in_progress） |
| branch `workstream_1` | running（应完成） | childTask `t-20260922-2zmm8d`（done_success），childRun `wfr-xx1efjdj`（completed） |
| branch `workstream_2` | running（应完成） | childTask `t-20260922-7t7bgo`（done_success），childRun `wfr-4bng6xlv`（completed） |
| 父 run 步实例 | requirement_draft/requirement_review/scale_gate/contract_batch/contract_review = completed；integration_review 及之后 = pending | — |
| WS-A 产物 | 远端分支 `feat/ws-a-client-lifecycle-63b32fc` @ `67c379d`（二次推送；更早 `a4894fd`） | 契约基线 `63b32fc6b610` |
| WS-B 产物 | 远端分支 `feat/ias-auth-center-ws-b` @ `ca7981c96dd5a068ee4eeb2c0f61ac7ba3e6f95c` | 同上 |
| WS-A agent 工作区 | branch `feat/ws-a-client-lifecycle-63b32fc`，dirty 仅 `.cursor/`、`.mcp.json`、`docs/ci-blocker-2026-09-05-redacted.txt`、`release/`（全部基线期遗留，与完成申报一致） | 可观测 |
| WS-B agent 工作区 | branch `feat/ias-auth-center-ws-b` @ `ca7981c96dd`（与远端 HEAD 一致），dirty 仅 `.cursor/`、`.mcp.json` | 可观测 |

结论：**产物完整且可保留，不需要重跑任何 workstream**。不一致仅在于：两个
branch instance 未落 completed、父 run 未推进（S2-1 缺陷所致，见
`docs/large-requirement-delivery-module.md` §7 S2-1 条目）。

## 2. 正常 API 补报已被实测排除

按评审要求先验证 "正常 API 能否处理"（2026-09-22 实测）：

1. 为 Mira 铸造 runtime token（`POST /api/v1/projects/ias-auth-center/agents/
   Mira/runtime/token`，admin 操作，审计留痕）。
2. `POST /api/v1/runtime/tasks/t-20260922-7t7bgo/workflow/branch/complete`，
   申报完成时已验证的两条测试文件路径。

结果：**400** `workflow branch "Workstream 2" output rejected: touched_paths
does not match the worktree: worktree changes not declared in touched_paths:
.cursor/, .mcp.json, docs/ci-blocker-2026-09-05-redacted.txt, release/`。

原因：两个 worktree 产生于 Fix B 之前，**没有基线文档**，闸门按设计回退到
绝对 status 度量，基线期遗留未跟踪文件无法通过 QA 白名单声明（它们本就不是
QA 交付物）。评审明确禁止为旧任务补造基线（会漂白闸门），因此该不一致状态
**没有正当的 API 路径**可处理，符合评审预设的 "正常 API 无法处理" 分支。

## 3. 恢复操作（最小侵入，待批准后执行）

> 2026-09-23 更新（S2-2.2 后的代码路径变化）：修复轮之后，若部署新二进制
> （≥ 77b9118c…f37e85d7）并补齐基线采集，§2 的 branch/complete 重报将走
> **幂等 re-drive**（崩溃窗口恢复）而非 400 —— 但本 run 的 worktree 无基线
> 且评审禁止补造基线，QA 闸门仍按绝对 status 度量拒绝遗留未跟踪文件，
> 故 §2 的结论不变：该 run 依旧没有正当 API 路径，仍需本节手工恢复。
> 差异仅一处语义：新代码下 failed branch 重报不再触发 re-drive，
> 本恢复只落 completed 账目，与该闸门无交集。

原则：不动 tasks、不动子 run、不动子 run 步实例/输出（这些账目是真实的）；
只把两个 branch instance 推到其应有的终态，然后用**平台自身代码**（store 层
`CompleteAndAdvance`）推进父 run，让 join 语义、事件、integration_review 激活
全部走正常代码路径。

步骤（在 VM 上以临时 Go 程序调用 workflow store 完成，不留 SQL 直改）：

1. `Store.CompleteBranchAndMaybeAdvance` 不可用（其第一步会因
   `run.ActiveStepID` 校验外的输出闸门再次拒绝）。改为直接复刻 join 落账：
   - `BranchInstancesForStep(runID, "parallel_workstreams")` 读出两个 instance；
   - 按 §1 快照的产物证据填充 `Status="completed"`、`Summary`（含分支名 + SHA）、
     `OutputValues`（`branch_summary` = 分支@SHA + 基线锚点；`touched_paths`
     = 完成时已验证的两条测试路径，仅 WS-B 有 QA 增量，WS-A 填其申报值）；
   - `SaveBranchInstance` 落账。
2. 对父 task `t-20260922-7x6tca` 调 `Store.CompleteAndAdvance(project, taskID,
   "parallel_workstreams 汇聚（S2-1 手工恢复）", aggregateJSON, aggregate,
   "completed")` —— 与引擎 join 成功时执行的完全相同的推进调用：
   `parallel_workstreams` 步实例置 completed、记录事件、按模板条件边激活
   `integration_review`（human 步，admin 代行审批）。
3. 结果核验（只读）：父 run active @ `integration_review`、两个 branch
   completed、步实例/事件完整。
4. 本文追加 "执行记录" 小节：时间、操作者、逐步输出、核验结果。

### 3.1 前提条件（全部满足才允许执行）

- **版本前提**：VM 二进制 ≥ `c1a058ff`（含 S2-2/S2-2.1/S2-2.2/S2-2.3 全部语义：
  幂等 re-drive、failed-branch 闸、交付面拆分、分支任务度量归属）。旧二进制上
  本方案不成立（Store 行为不同）。执行前以二进制内 `ErrQABaselineTampered`
  字符串存在性 + `multigent version` 核验。
- **前置状态备份**：步骤 1 执行前备份两个 branch instance 行与父 run 行的
  JSON 快照（API 只读导出，写入本目录 `recovery-backup-<date>.json`）。恢复
  出现意外时以快照为依据人工评估，快照本身不回写。
- **并发控制**：执行窗口内暂停对该项目的新任务入队（admin 操作），执行前
  重新核验父 run 仍为 active@parallel_workstreams 且两个 instance 仍为
  running；任一前置状态漂移即中止。

### 3.2 部分失败语义（诚实陈述）

- **步骤 1 与步骤 2 之间存在持久化窗口**：步骤 1 落账两个 branch instance
  为 completed 后、步骤 2 `CompleteAndAdvance` 执行前若中断，系统处于
  "branches completed + run 仍 active@parallel" 的中间态。该状态与崩溃窗口
  一致：新二进制下（幂等 re-drive 语义）重放步骤 2（同参数再次调用
  `CompleteAndAdvance`，它内部会校验 active 步并推进）即可收敛；若不收敛
  则**停止并报告，不引入第三种写法**。
- 步骤 1 内部分失败（第一个 instance 落账成功、第二个失败）：按已落账状态
  继续补第二个 instance（幂等重试），两次都成功才进步骤 2；无法落账即中止，
  已落账的 completed 与产物事实一致，不回滚。
- **不存在"失败零写入"承诺**：任何一步失败都可能留下部分落账；每次落账都
  与产物事实一致且可幂等重试收敛，这是本方案的安全依据，而不是零写入。

### 3.3 负责人与通知验证

- 执行负责人：admin（人审代行者同源）。
- 步骤 2 的 `CompleteAndAdvance` 正常激活 `integration_review`（human 步，
  受让人按模板绑定）；执行后必须核验：integration_review 步实例状态 =
  active/待审、admin 侧收到待审任务（任务列表可见 + 注意力信号触发）。
  核验不过 = 恢复未完成，停止并报告。
- 全程逐命令记录到 §5 执行记录（时间、操作者、输出、核验结果）。

### 3.4 回退

不回退分支产物、不改子 run。中间态收敛失败时保持现场（快照 + 逐步输出），
等人工决策；本方案不含破坏性回退路径。

## 4. 恢复后的 S2 剩余验证

恢复只解决 "回到正确轨道"；S2 验收（评审提高后的目标）仍要求：

- [ ] integration_review 人审通过（admin 代行，审批记录留痕）；
- [ ] **两个分支合入同一集成候选 SHA**（integration 步真实合并 + 冲突处理）；
- [ ] 独立验证（qa 步）针对该候选执行（基线增量闸门首秀）；
- [ ] 父流程推进至 qa_signoff 及之后，无重复任务、无悬挂 branch。

以上任一步失败即暴露下一个缺陷，按 S2-1 同样流程（定位 → 评审 → 修复 → 回归）。

## 5. 执行记录

（待批准后填写）
