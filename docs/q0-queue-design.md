# Q0 任务队列设计：并发准入、租约与失败退避

> 状态：已实现（PR-1..PR-3 落地，PR-4 本文档）。设计评审历史见
> `docs/review-brief-for-gpt-q0-queue-2026-09-13.md`（git-excluded）。

## 1. 目标与不变量

同一 Agent（Worker）**同时只执行一个**会修改项目状态的任务；多个项目/多个
Agent 允许并行；重启、超时、失败后可恢复，不永久占槽、不重复执行。

不引入 Redis、消息队列或分布式节点——控制面 SQLite（runtime_runs）+ 文件
taskstore 就是唯一状态存储，一切原子性靠 SQL 条件更新实现。

核心不变量：

1. **单槽**：每个 Worker 同时至多一个"占槽 run"（running 且 lease 未过期且
   `slot_class != 'readonly'`）。判定函数唯一：`db.RunOccupiesWorkerSlot`。
   **queued 不占槽**（但派发门仍挡重复派发）。
2. **同类不重复**：同一派发意图（task / wakeup / workflow step）在
   queued+running 窗口内由部分唯一索引
   `idx_runtime_runs_active_key(workspace_id, run_key) WHERE status IN
   ('queued','running') AND run_key <> ''` 幂等去重；run 终态即释放键。
3. **所有权可验证**：每次 claim/reap 都使 `lease_generation+1`；续租 /
   finish / reap 全部 `WHERE id AND runtime_node_id AND
   lease_generation`，旧持有者的迟到回调一律 0 行生效。generation≤0 直接
   拒绝（fail-closed，不支持新旧 node 混跑）。（fix 轮裁定 1：claim 不再有
   takeover 语义——running run 不进候选，generation 只在 claim queued 行与
   reap 时增长。）
4. **任务级执行凭据**：`task.ActiveRuntimeRunID` 记录"这个任务此刻归哪个
   run"，是 reap 侧任务状态转换的**真栅栏**：清除与 stamp 条件匹配
   （`== 本 run ID`）且共用互斥锁，reaper 与 finish 谁后到谁发现条件不
   成立，天然幂等（fix 轮裁定 3）。
5. **有限退避**：基础设施失败最多自动重试 2 次（每次 5 分钟退避），第 3 次
   封顶为 blocked，等待人类显式 unblock。

## 2. 派发路径与收敛点

四条派发路径全部经 `enqueueRuntimeTaskRun` → `UpsertRuntimeRunIdempotent`：

| 路径 | run_key | 去重语义 |
|---|---|---|
| 调度 tick（heartbeat 到期） | `task:{ws}:{project}:{taskID}` | 同任务排队/执行中即返回已有 run |
| 手动 start（收口 5：任务队列语义） | 同上 | **入队**而非拒绝：agent 已有 queued/running run 时，手动 start 经 run_key 幂等收敛到**同一条 run**（200），不再 409；与 tick 竞争同样收敛。退避/blocked/等待人工确认的门不变（409 + 指引）。本地（无 node）路径保留立即执行语义，由 heartbeat PID 门控 |
| attention / 定时 wakeup | `wakeup:{ws}:{p}:{a}:{intent}`，intent=`scheduled` 或 `attention:{signalIDs}` | 同 intent 唤醒只排一条 |
| workflow 步骤（恢复/手动/触发） | `wf:{ws}:{workflowRunID}:{stepInstanceID}` | 同一步骤实例只排一条；重做复用 step instance |

exec_prompt / fork run 无键（fork 靠 busyAgents 上报 + 槽位判定；exec 每次
独立执行，客户端重试防重留待 Idempotency-Key 需求出现时开放——builder
`RuntimeRunKeyExec` 与 `ValidateIdempotencyKey` 已就绪，格式
`exec:{ws}:{caller}:{p}:{a}:{key}`，绑定 workspace+caller+project+agent，
key 仅 `[A-Za-z0-9._-]`、1..128，日志/审计只落 SHA-256 前 12 位指纹）。

## 3. 槽位与 claim

- **claim 候选严格限定 `status='queued'`**（fix 轮裁定 1）：running run 即使
  lease 已过期也**绝不被 claim 接管**——只有 reaper 在 lease+180s grace 后
  终结它；重试一律通过**新 run**（同 run_key 在旧 run 终态后允许重新入队）。
  旧语义（接管过期 run + generation+1）已废除，A4/A10/A14 测试随语义改写。
- `ClaimRuntimeRun` 事务内先查"当前占槽 run 的 worker key 集合"
  （`runtimeRunsHoldingSlots`），**在 SQL 层过滤**掉已占槽 worker 的候选行
  （fix 轮裁定 4）：队首 worker 被占槽时 claim 继续查找后续可执行 worker，
  不再整轮返回空。同一事务快照内判定，无 check-then-act 窗口。
- worker key：`worker/{agentWorkerID}`，legacy membership 行退化为
  `{project}/{agent}`（与 node 上报 busyAgents 的键格式一致）。
- fork 豁免在**入队时**判定并持久化 `slot_class`：session 能力集完全落在
  平台固定只读类别（inspect/log/list/read/status）→ `readonly`，其余一切
  （含解析失败、mode-only、空集）→ `normal`（fail-closed）。claim/槽位判定
  只读这一列，不再动态解析。
- **busyAgents 豁免限定 `slot_class='readonly'`**（fix 轮裁定 6）：node 上报
  的 busy 列表只对 readonly fork 候选豁免；normal fork / task 候选照常被
  busy 列表挡住，过期的 busy 上报不能造成同 worker 双 bookings。
- node 端 claim 响应携带 `leaseGeneration`；续租/完成/失败请求必须回传，
  不传或传旧值 → 409。node 的续租循环收到"lease lost"冲突即取消 run
  context，agent 停止为已不属于自己的 run 工作。

## 4. 租约与 reaper

- lease 90s；node 每 30s 在同一 tick 做 heartbeat + 续租（强耦合是设计
  前提："lease 在续" ⇔ "node 活着"）。
- **workspace 级 reaper 单例**：`StartWorkspaceScheduler` 里 `sync.Once`
  启动，60s 一轮；**唯一判据**
  `status='running' AND lease_expires_at < now-180s`——不查 node 心跳表
  （fail-closed 的第二判据会把 reaper 饿死）。宽限从 **lease 过期时刻**
  起算：node 死后感知延迟 ≈ lease(90s) + grace(180s) ≈ **270s**。
- 强杀 = `failed(error_code=lease_expired)` + generation+1 + 条件清空 task
  token + 审计 `runtime_run.reaped`；UPDATE 自带 lease+generation 条件，
  与续租/finish 竞态时输家自动 0 行。
- **reap 后任务状态转换走栅栏**（fix 轮裁定 2 + 收口 3）：reap 成功后通过
  `transitionReapedTask` 推进任务——**必须匹配 task token**（`fix 轮裁定
  3`：`ActiveRuntimeRunID == run.ID` 才允许动任务状态，token 已被新派发
  接管则不碰）；非 workflow 任务纳入统一 infra 退避/blocked 逻辑
  （lease_expired 计入 streak），**绝不留下 in_progress 卡死任务**；
  workflow 任务**显式进入 workflow 引擎失败/rework 机制**：分支任务走
  `completeRuntimeWorkflowBranch(..., "failed")`，普通任务走
  `CompleteAndAdvance(..., "failed")`——active step 实例、workflow run、
  task 全部落到确定终态，不停留 in_progress
  （`TestReaperFailsWorkflowStepThroughReworkPath`：真实 workflow run 过期）。
- **token 是真栅栏**（fix 轮裁定 3）：stamp 与 clear 共享一把进程内互斥锁
  （`runtimeTaskTokenMu`），旧的 finish/清理不可能穿插到新派发的
  read-check-write 之间——"旧 finish 与新 enqueue"竞态下，旧 finish 清掉
  新 token 的窗口被消除（`TestOldFinishVsNewEnqueueTokenFence` 10 轮）。
- **统一栅栏临界区**（收口 1+2）：`fencedTaskTransition` 是"移动 run 正在
  执行的任务"的唯一入口——同一互斥锁持有期内完成 读取 task → 校验 token →
  mutate → 单次持久化（含 token 释放，一次写落地）→ 返回。finish 路径
  （`applyFencedFinishTransition`）与 reaper（`transitionReapedTask`）都走
  它；双方都不得"先无条件 finalize 再清 token"、不得在栅栏外裸调退避。
  mutate 是**纯内存三态决策**（`fenceDecision`）：`Apply`（状态 + 归档 +
  token 释放一次写落地）/ `Skip`（无可转换状态，释放 token）/
  `Retry`（workflow 引擎等旁路写失败——什么都不持久化，token 保留给下轮
  replay）。**持久化失败 = 什么都没写、什么都没放**（收口 4d）：存内 task
  与 replay 方读到的完全一致，重放永不重复计数。
- **失败恢复 = 同轮 sweep replay**（收口 4d）：`clearStaleTaskRuntimeToken`
  在释放指向已终态 run 的 token 前，**先经栅栏重放该 run 的任务转换**
  （reaped → 统一退避；succeeded → 归档成功；failed → 退避或 done_failed；
  workflow → CompleteAndAdvance("failed")），转换成功后才清 token 并审计。
  reaper 单轮内的顺序：栅栏转换 →（转换持久化失败时）sweep replay 兜底 →
  pass-wide 全量扫描。真实路径测试：HTTP finish 写失败 token 保留 → sweep
  重放归档成功；reaper 写失败同轮收敛 pending+NotBefore（streak 恰 1，不
  双计）；恢复 pass 对已收敛任务为 no-op。
- **跨存储对账**：run 在 SQLite、task 在文件存储，无法同事务。方向一
  （run 成功、task 写失败）：不回滚 run，靠 run_key 挡重复，token 由后续
  写补偿。方向二（task 残留、run 已终态）：**每轮 reaper pass 全量扫描**
  `sweepStaleTaskRuntimeTokens`（Claude 评审偏离 1 的落实：不再只在 reap
  路径顺带清理），token 指向已终态/不存在 run 的任务一律清理 +
  `task.runtime_token_orphan` 审计。对账收敛点是 reaper 周期，不要求强一致。
- **生命周期**：重复 Start 幂等；`ShutdownGracefully` 在 `controlDB.Close()`
  **之前** `stopRuntimeReaper()` 等 loop 退出。
- **部署 SOP（Q0 不支持新旧 node 混跑）**：drain（等全部 running run 自然
  finish 或人工处理）→ 停全部 node → 升控制台 → 升 node。generation≤0 的
  续租/finish 请求一律拒绝。

## 5. 失败退避与人类兜底（D4）

- 计数只认服务端受控封闭集合
  `{spec_fetch_failed, workspace_prepare_failed, agent_prepare_failed,
  executor_failed, lease_expired}`（fix 轮裁定 6：`agent_run_failed` 移出——
  agent 级失败可能是业务结果，被计方不能持有计数器）。不采信 agent 自报
  businessFailure（该标记不存在）。workflow 步骤失败与人工取消不进计数。
  `agent_prepare_failed` 保留在集合内：它由 runtime node 自身的 prepare
  阶段（`cmd/multigent` runtime_node.go）在任何 agent 业务逻辑之前发出，
  可证明属于平台侧基础设施失败（计划原列 5 个码 + 该码 = 实现的 5 码集合，
  此处定稿）。
- streak 1-2 → 任务回 pending + `NotBefore=now+5m`；成功清零；编辑任务无
  副作用。**reaper 的 lease_expired 强杀同样计入该逻辑**（fix 轮裁定 2）。
- streak=3 → 任务 blocked（active 列可见）+ 人类负责人通知：
  收件人 = 任务创建者（仅当真实人类用户）∪ 项目 manager memberships；
  逐人按其在**当前项目 IM 绑定**下的外部身份映射 DM（fix 轮裁定 5：身份
  解析以 binding 的 `ChannelBindingID` 过滤，钉死 connectionId 与
  imInstanceId——双 Mattermost 实例下绝不误取另一实例的身份；本绑定下无
  映射者降级为任务评论 @提及）；全体无映射 → 评论 + 审计。
  **禁用 agent attention，禁用 workspace-admin 广播。**
- 派发侧门：调度选择跳过 blocked；手动 start 对 blocked 返回 409（附
  unblock 指引），对退避中任务返回 409（附剩余时间）；对 agent 已有
  queued/running run 的任务**入队收敛**（见 §2 收口 5），不再 409。
- **解除封锁唯一通道**：
  `POST /api/v1/projects/{name}/tasks/{taskId}/unblock`
  （checkProjectManager + 审计 `task.unblock`）→ 任务回 pending、streak
  清零。前端二次确认仅为 UX 增强。

## 6. 审计动作

| action | 时机 |
|---|---|
| `runtime_run.enqueue` | 入队（既有） |
| `runtime_run.reaped` | reaper 强杀（含 node/task/lease 过期时刻/宽限/新 generation） |
| `task.runtime_token_orphan` | 清理指向已终态 run 的残留 token |
| `task.blocked` | 封顶进入 blocked（含收件人与投递结果明细） |
| `task.unblock` | 人工解除封锁（含 actor） |

## 7. 测试矩阵与覆盖

- **db 层**（`internal/db/runtime_slot_test.go`、`run_key_test.go`）：
  A1 50 并发 claim 单赢家；A2 同 Worker 单槽；A3 跨 Worker 并行；
  A4（fix 1 改写）过期 lease 的 running run 对 claim 不可见、reap 后新 run
  才可领取；A5 未过期不接管；A6-A9 run_key 幂等/迁移；A10（fix 1 改写）
  同 node 旧 generation 全链失效 + generation=0 拒绝；A11/A12 readonly
  豁免 / normal 占槽（含 fail-closed）；**fix 4** 占槽后 claim 跳到下一
  worker；**fix 6** busyAgents 豁免限 readonly（normal fork / task 候选照挡）；
  reap 条件更新 + cutoff。
- **api 层**（`internal/api/runtime_slot_test.go`）：A13 reaper-vs-finish
  10 轮并发（终态恰一次、token 至多清一次）；A14（fix 1 改写）
  claim-vs-finish-vs-reap：claim 找不到 running run、旧 finish 收敛或被拒、
  reap 后新 run 为唯一重试路径；**fix 2** reap 推任务进退避/封顶（不留
  in_progress）；**fix 3** token 栅栏（fenced reap 不碰新 run 的任务）+
  "旧 finish vs 新 enqueue" 10 轮；**偏 1** 无 reap 的 pass 全量清理孤儿
  token；**收口 3** 真实 workflow run 过期 → step/run/task 落定 failed；
  **收口 4** 真实路径竞态（`handleRuntimeNodeRunComplete` HTTP finish vs 新
  enqueue 10 轮、reaper vs 新 enqueue 10 轮、写失败 token 保留 + sweep
  replay 恢复）；**收口 5** 手动 start 在 queued/running 期间入队收敛同
  run（200，不 409、不重复建 run、token 完好）；B9 槽位语义；B17 queued
  不占槽但重复派发收敛同 run；token 生命周期；reaper pass 单元；单例生命
  周期；旧 generation finish 409。
- **PR-3**（`internal/api/task_infra_backoff_test.go`）：B5 退避、B6 封顶
  + 评论兜底、B7 成功清零、B8 派发门 + unblock、B11 workflow 排除、
  B12 业务失败不计、B14 编辑不清零、封闭集合单元测试（含 agent_run_failed
  不计数）；**fix 5** IM 身份限定到项目绑定（双实例不误取、无映射降级）。
- 端到端 C1-C7（双 worker 并行、串行、kill -9 后 ~270s 收割、正常续租不
  误杀、重启新 generation、生命周期、部署演练）在 VM 部署时验证。

## 8. 收口轮裁定（2026-09-13 执行栅栏收口）

1. **统一 task-runtime fenced transition**（收口 1+2）：见 §4。finish 与
   reaper 共用 `fencedTaskTransition`；持久化失败保留 token，恢复靠同轮
   sweep replay（§4"失败恢复"）。
2. **workflow reaper 显式进入失败/rework 机制**（收口 3）：见 §4。真实
   workflow run 过期测试断言 step/run/task 不停留 in_progress。
3. **手动 start 产品语义 = 任务队列**（收口 5）：Q0 把任务队列定义为唯一
   状态源（§2 表格本就声明"手动 start 与 tick 收敛同一条 run"），旧实现
   的 `hasActiveRuntimeRun` → 409 是 Q0 前立即执行语义的残留，与文档矛盾。
   已改为：node 路径手动 start 一律入队，由 run_key 幂等去重；退避/blocked/
   awaiting 门不变；本地路径保留立即执行（heartbeat PID 门控）。UI 文案与
   i18n（`tasks.taskQueued` / `tasks.agentAlreadyRunning`）同步为入队语义。
4. **Claude 标注一（fix 1 语义大改，产品语义已确认）**：claim 不再接管
   过期 running run——终止权从"新派发换机续跑"转移到"reaper reap 后重跑"，
   这是有意的产品语义而非隐式回归。理由：接管要求新 claim 继承旧 run 的
   generation 与半途状态，跨机续跑的失败面（内存态丢失、fork 会话失效）
   远大于重跑；重跑路径的语义由 run_key 幂等与 fenced transition 完整保证。
   改写后的 A4/A10/A14 断言已按新语义校验（claim 不见 running、
   generation 只在 claim queued 与 reap 增长、reap 后新 run 是唯一重试路径）。
5. **Claude 标注二（错误码集合换血，勿拿旧表对）**：实现的封闭集合是
   `{spec_fetch_failed, workspace_prepare_failed, agent_prepare_failed,
   executor_failed, lease_expired}`（§5）。与最初计划枚举的 5 码不同：
   `agent_run_failed` 换成了 `agent_prepare_failed`。理由：前者无法区分
   业务失败与平台崩溃（被计方不能持有计数器），后者由 node prepare 阶段
   在任何 agent 业务逻辑之前发出，可证明平台侧。GPT 校验时以本节集合为准，
   不要对照旧计划表。

## 9. 已知取舍

- ~~`agent_run_failed` 同时覆盖业务失败与 agent 崩溃~~（fix 轮裁定 6 废除）：
  `agent_run_failed` 已移出封闭集合——agent 级失败回退为业务结果，走
  done_failed 归档，不进退避计数。若未来 mga 增加可证明的平台侧执行器失败
  码，可单独评审加回。
- 部分 unique index 迁移对历史空键行无约束（A9 覆盖）；上线前可用 temp-DB 演练。
- reaper 不做 node 版本兼容；升级必须 drain（Claude 裁定：现在做多 node
  兼容是过度工程）。
- token 栅栏的互斥锁是**单控制台进程内**强一致；run/task 跨存储整体仍只在
  reaper 周期收敛（多控制台实例部署不在当前架构内）。
