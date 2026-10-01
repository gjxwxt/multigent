# P1.4 join/touched_paths batched 实弹回归 — 收官取证

> 状态：**已收官（2026-10-02 04:1x UTC+8 完成）**。run `wfr-tjdb5b9r` 全 13 步 completed，
> 任务 `t-20261001-4aulcs`（项目 `p14-join-regression`，一次性无远端项目，agent `p13-runner`）。
> 上游依据：docs/master-plan-2026-10-01.md Phase 1 第 1.4 行（缺陷 4 join / 缺陷 7 touched_paths
> 的 batched 实弹复验）。本文只写可复核事实；部署环境细节不入库（见 AGENTS.md §7）。

## 1. 验证结论（逐项）

| 验证点 | 结论 | 证据 |
|---|---|---|
| 计划冻结链（contract_review 批准 = 唯一冻结入口） | ✅ | planId `batch-p14-join-regression-20261001` v1；两轮 fail-closed 400（REUSE 引用文本、integrationPolicy 嵌套对象）后 agent 按评论修正重发成功——rework 环路真实工作 |
| fan-out 材料化（plan-aware 分支） | ✅ | wp-contract 先行（5a2630e 集线基线），wp-store/wp-api 以冻结基线 c0610bf 波次并行；分支名 `feature/wf-wfr-tjdb5b9r-<branchID>` |
| 分支 join（原缺陷 4） | ✅ | 三分支 completed 后 integration_review 正常推进；completionCommit 逐分支验证；join 后 worktree 才清理 |
| no-remote 本地交付证据 | ✅ | wp-store agent 如实报告"未推送、未伪造 origin，平台以 completionCommit 验证本地 tip"；原伪造 origin 行为消失 |
| implementation 线性步交付闸门（**缺陷 #14**） | ❌→已解锁 | root 任务 BranchName/BaseCommit 全空 + 无远端 → 互斥拒绝（6 次 agent run done_failed）；按 agent 路径 A 在任务注册表补 declared branch 后闸门接受 11 commits beyond 冻结基线。**代码修复待做**（见 §3） |
| QA touched_paths 闸门（原缺陷 7，最终验证点） | ✅ | 实弹双向验证：QA 测试提交为 41eda92 → 闸门拒绝（delivery_sha 处树必须干净）；git reset 回 9eb4e1c、测试文件作为未提交工作树变更（声明 delta）→ 接受。基线 delta 语义与脚手架噪声豁免均按设计工作 |
| qa_signoff 风险矩阵闸门（**缺陷 #16**） | ❌→已修复 | agent 写 `risk` 而闸门只读 `risk_level` → 24 行矩阵被 "invalid risk_level \"\"" 拒。已修复（`qaRiskItem.UnmarshalJSON` 加 `risk` 别名 + 回归测试 `RiskAliasAccepted`），交叉编译部署 VM 后实弹通过（含 QA-GATE-PUSH blocked 项的显式 manual waiver） |
| PR/发布/go-live | ✅ | pr_open_and_merge（local_branch 模式分支指针）→ release（真实 go build -ldflags 构造 todoapi v0.1.0 @ 0a82137，health probe healthy）→ go_live_confirm 批准后 run completed |

## 2. 运行时间线（关键节点）

- 15:36 任务创建（write.go 自动种子 MULTIGENT_DELIVERY_CONTRACT requireGitCommit+requirePush）
- 15:41–16:03 scale_gate → contract_batch → 冻结（含两轮 fail-closed 400）
- 16:04–16:18 fan-out 三分支（波次解锁正常）
- 16:18–16:51 implementation 步：agent 完成 TDD 全流程（22/22 -race 绿）但闸门按 #14 互斥拒绝，
  agent 受控探针解码两种配置、发 3 条 admin 升级消息 + 证据文档，按流程记 failed
- 16:51–02:1x 重驱僵局：run 已 reactivate 但调度不派发（ccr 137 + Docker 瞬时不可达 + 调度器
  exec-target 选择缺陷 #17 叠加；期间两次 loop-guard 触发）
- 01:44 按路径 A 修补任务注册表（branchName/baseCommit/baseBranch）→ impl 立即通过
- 01:53 self_review ✅ → 01:55 code_review（人工批准）→ 02:17 qa 步（touched_paths 双向验证）
- 02:47 qa_signoff 首次被 #16 拒 → 本地修复+单测+交叉编译部署 → 02:48 批准通过
- 03:3x–04:1x pr_open_and_merge / release（ccr 137 二次重启后经 manual start 驱动）
- 04:1x go_live_confirm 批准 → **run completed**

## 3. 缺陷登记（本次新增/更新，编号接续 overnight 9 缺陷）

| # | 缺陷 | 根因位置 | 状态 |
|---|---|---|---|
| #10 | plan REUSE 标记：contract_batch 输出 "REUSE_FROM_SCALE_GATE: …" 引用文本而非 JSON，冻结消费本步 OUTPUT 拷贝 fail-closed 400 | greenfield.go contractBatchDesc 措辞可再收紧 | 未修（rework 环路可自愈，观察项） |
| #11 | agent-home 误当 git root：无仓库项目 resolveProjectGitRoot 遍历 agents/*/ 找 .git | preview_handlers.go resolveProjectGitRoot | 未修 |
| #12 | integrationPolicy 类型未文档化：planSourceJSON 期望 string，agent 惯性写嵌套对象 | plan_source.go（可加模板措辞/示例） | 未修（rework 可自愈） |
| #13 | runner gate 与平台完成双轨观测不一致：runner 级 gate 在容器退出后翻 done_failed，平台侧 precheckBranchJoinGate 已落 branch instance done_success | runner.go applyDeliveryContractGate 时序 | 未修（观测问题，不阻塞 join） |
| #14 | **无远端线性步互斥拒绝**：root 线性步 BranchName/BaseCommit 全空 → (a) feature 分支上报 branchName 空 fail；(b) HEAD 在 main → "no commit beyond frozen base"。local_branch 模式两条件互斥 | delivery_contract.go:154 ValidateGitDeliveryEvidence no-remote 分支 + write.go 契约种子不区分线性/分支步 | **已实弹解锁（任务注册表补 declared branch）**；代码修复方向：线性步无 BranchName 时以 completionCommit/HEAD rev-parse 为本地证据（agent 建议 B），待修 |
| #15 | 调度 loop-guard 中断 wakeup cycle：同一任务单周期反复出队→整 cycle 报错停止（sleep 中断后续任务） | cmd/multigent/scheduler.go:1397 | 未修（防护本身有效，但中止面过大） |
| #16 | **qa_signoff risk 别名缺失**：agent 写 `risk`，闸门 qaRiskItem 只读 `risk_level` → 24 行有效矩阵被拒 | workflow_handlers.go qaRiskItem.UnmarshalJSON | **已修复**（risk 别名 + RiskAliasAccepted 回归测试），已部署 VM 实弹验证 |
| #17 | **多项目 worker exec-target 饥饿**：selectSchedulerExecutionTarget 只在 cycle 开始选一次 execProject（scheduler.go:1066），runAllPendingTasks 循环内 nextPendingTask 只查该 project（:1202）；attention wakeup 每 tick 自播种 pending 任务（priority 0）恒赢 priority 1 的跨项目任务 → 其它 membership 项目任务饿死 | cmd/multigent/scheduler.go | 未修；绕过=manual start。修复方向：循环内每次出队重新选 target，或 attention 任务不参与跨项目 priority 竞争 |

既有遗留：approval_json.state 不回写；ccr-service 137 崩溃循环（本次实弹中两次 OOM 退出，均 docker
start 恢复；根因未查）；`make build` 产出本机架构二进制，部署需 `GOOS=linux GOARCH=amd64
CGO_ENABLED=0 go build`（本次已验证该路径；Mach-O 误部署表现为 posix_spawn: no such file or
directory，恢复备份即回滚）。

## 4. 部署与回滚记录

- 部署：交叉编译 ELF（45,329,579 字节）替换 `/opt/multigent/bin/multigent`，
  multigent + multigent-mattermost-bridge 同二进制均重启；console 200 验证。
- 回滚档：`multigent.bak-44de4843-prev-riskfix-<ts>`（44de4843 版本）；另有一个误部署的
  Mach-O 已改名 `multigent.broken-macho-*` 存档。VM 部署前备份惯例已遵守。
- 本地分支 `dev`（44de4843-dirty + #16 修复）—— #16 修复尚未提交，待随缺陷修复批一起提交。

## 5. 未验证边界

- #14 的**代码级**修复未做（本次以数据层解锁实弹；线性步闸门对无远端项目的默认行为仍未修）。
- #17 的代码修复未做；manual start 绕过路径已验证可用。
- qa 触及的 `resolveQABaselineSurface` 篡改分支（kv_records 基线被改 → fail-closed）未在本轮
  实弹覆盖（本轮验证的是"声明 delta 接受/带外提交拒绝"正向语义）。
- ccr 137 根因仍未查（本轮仅重启恢复 ×2）。
