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
3. **所有权可验证**：每次 claim/takeover/reap 都使 `lease_generation+1`；
   续租 / finish / reap 全部 `WHERE id AND runtime_node_id AND
   lease_generation`，旧持有者的迟到回调一律 0 行生效。generation≤0 直接
   拒绝（fail-closed，不支持新旧 node 混跑）。
4. **任务级执行凭据**：`task.ActiveRuntimeRunID` 记录"这个任务此刻归哪个
   run"；清除必须条件匹配（`== 本 run ID`），reaper 与 finish 谁后到谁发现
   条件不成立，天然幂等。
5. **有限退避**：基础设施失败最多自动重试 2 次（每次 5 分钟退避），第 3 次
   封顶为 blocked，等待人类显式 unblock。

## 2. 派发路径与收敛点

四条派发路径全部经 `enqueueRuntimeTaskRun` → `UpsertRuntimeRunIdempotent`：

| 路径 | run_key | 去重语义 |
|---|---|---|
| 调度 tick（heartbeat 到期） | `task:{ws}:{project}:{taskID}` | 同任务排队/执行中即返回已有 run |
| 手动 start | 同上 | 与 tick 竞争收敛到同一条 run |
| attention / 定时 wakeup | `wakeup:{ws}:{p}:{a}:{intent}`，intent=`scheduled` 或 `attention:{signalIDs}` | 同 intent 唤醒只排一条 |
| workflow 步骤（恢复/手动/触发） | `wf:{ws}:{workflowRunID}:{stepInstanceID}` | 同一步骤实例只排一条；重做复用 step instance |

exec_prompt / fork run 无键（fork 靠 busyAgents 上报 + 槽位判定；exec 每次
独立执行，客户端重试防重留待 Idempotency-Key 需求出现时开放——builder
`RuntimeRunKeyExec` 与 `ValidateIdempotencyKey` 已就绪，格式
`exec:{ws}:{caller}:{p}:{a}:{key}`，绑定 workspace+caller+project+agent，
key 仅 `[A-Za-z0-9._-]`、1..128，日志/审计只落 SHA-256 前 12 位指纹）。

## 3. 槽位与 claim

- `ClaimRuntimeRun` 事务内先查"当前占槽 run 的 worker key 集合"
  （`runtimeRunsHoldingSlots`），候选行若其 worker 已占槽则整个 claim 返回
  无工作——同一事务快照内判定，无 check-then-act 窗口。
- worker key：`worker/{agentWorkerID}`，legacy membership 行退化为
  `{project}/{agent}`（与 node 上报 busyAgents 的键格式一致）。
- fork 豁免在**入队时**判定并持久化 `slot_class`：session 能力集完全落在
  平台固定只读类别（inspect/log/list/read/status）→ `readonly`，其余一切
  （含解析失败、mode-only、空集）→ `normal`（fail-closed）。claim/槽位判定
  只读这一列，不再动态解析。
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
  与续租/finish/takeover 竞态时输家自动 0 行。
- **跨存储对账**：run 在 SQLite、task 在文件存储，无法同事务。方向一
  （run 成功、task 写失败）：不回滚 run，靠 run_key 挡重复，token 由后续
  写补偿。方向二（task 残留、run 已终态）：reaper 周期内的
  `clearStaleTaskRuntimeToken` 扫描收敛 + `task.runtime_token_orphan` 审计。
  对账收敛点是 reaper 周期，不要求强一致。
- **生命周期**：重复 Start 幂等；`ShutdownGracefully` 在 `controlDB.Close()`
  **之前** `stopRuntimeReaper()` 等 loop 退出。
- **部署 SOP（Q0 不支持新旧 node 混跑）**：drain（等全部 running run 自然
  finish 或人工处理）→ 停全部 node → 升控制台 → 升 node。generation≤0 的
  续租/finish 请求一律拒绝。

## 5. 失败退避与人类兜底（D4）

- 计数只认服务端受控封闭集合
  `{spec_fetch_failed, workspace_prepare_failed, agent_prepare_failed,
  executor_failed, lease_expired, agent_run_failed}`；不采信 agent 自报
  businessFailure（该标记不存在）。workflow 步骤失败与人工取消不进计数。
- streak 1-2 → 任务回 pending + `NotBefore=now+5m`；成功清零；编辑任务无
  副作用。
- streak=3 → 任务 blocked（active 列可见）+ 人类负责人通知：
  收件人 = 任务创建者（仅当真实人类用户）∪ 项目 manager memberships；
  逐人按其在项目 IM 绑定下的外部身份映射 DM（复用 runtime notify 的身份
  模型）；无映射者降级为任务评论 @提及；全体无映射 → 评论 + 审计。
  **禁用 agent attention，禁用 workspace-admin 广播。**
- 派发侧门：调度选择跳过 blocked；手动 start 对 blocked 返回 409（附
  unblock 指引），对退避中任务返回 409（附剩余时间）。
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
  A1 50 并发 claim 单赢家；A2 同 Worker 单槽；A3 跨 Worker 并行；A4 接管
  generation+1 + 旧持有者回调失效；A5 未过期不接管；A6-A9 run_key 幂等/
  迁移；A10 同 node 旧 generation 全链失效 + generation=0 拒绝；A11/A12
  readonly 豁免 / normal 占槽（含 fail-closed）；reap 条件更新 + cutoff。
- **api 层**（`internal/api/runtime_slot_test.go`）：A13 reaper-vs-finish
  10 轮并发（终态恰一次、token 至多清一次）；A14 takeover-vs-finish 10 轮
  （run 不复活、token 不被旧值覆盖）；B9 槽位语义；B17 queued 不占槽但
  重复派发收敛同 run；token 生命周期；reaper pass 单元；单例生命周期；
  旧 generation finish 409。
- **PR-3**（`internal/api/task_infra_backoff_test.go`）：B5 退避、B6 封顶
  + 评论兜底、B7 成功清零、B8 派发门 + unblock、B11 workflow 排除、
  B12 业务失败不计、B14 编辑不清零、封闭集合单元测试。
- 端到端 C1-C7（双 worker 并行、串行、kill -9 后 ~270s 收割、正常续租不
  误杀、重启新 generation、生命周期、部署演练）在 VM 部署时验证。

## 8. 已知取舍

- `agent_run_failed` 同时覆盖业务失败与 agent 崩溃：误计业务失败的代价只是
  多一次 5 分钟退避、3 次封顶后有人工 review 兜底，不会漏处理。mga 增加
  业务失败显式上报通道后再细分移出（独立评审）。
- 部分唯一索引迁移对历史空键行无约束（A9 覆盖）；上线前可用 temp-DB 演练。
- reaper 不做 node 版本兼容；升级必须 drain（Claude 裁定：现在做多 node
  兼容是过度工程）。
