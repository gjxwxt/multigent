# Preview Copilot Turn Receipts - 运维与故障排查手册 (Runbook)

本文档面向系统运维与技术支持团队，提供 **Preview Copilot Turn Receipts（预览即时调优回执机制）** 的架构速查、灰度开启、紧急熔断 (Kill-Switch) 与故障恢复标准作业程序 (SOP)。

---

## 1. 架构与事务模型速查

Preview Copilot Turn Receipts 在人工审核阶段（`human_review` 节点）为代码微调提供可追踪、可回滚、原子收编的安全保证：

```
[ 单回合交互 (Turn) ]
  PENDING → EXECUTING → CAPTURING → CAPTURED
     ↑                                   |
     | (回滚)                             ↓ (审核批准收编)
     +--------- REVERTING ←---------------+ COMMITTING (绑定 CommitIntentID + PreCommitSHA)
                   |                          |
       +-----------+                          +--------------------+
       |                                      |                    |
       ↓ (逆向 patch 失败)                     ↓ (Git 提交成功)      ↓ (Git 提交失败)
  REVERT_FAILED (人工排查)               COMMITTED             REVERT_FAILED (人工排查)
```

### 核心机制与不变量 (Invariants)
1. **单回合槽位锁 (TaskSlot)**：每个任务同一时刻仅允许一个单回合（Turn）或审核收编组（CommitIntentGroup）持有写锁。
   - `HolderType = "turn"`：单回合持有，执行完毕后在 `CAPTURED` 释放槽位。
   - `HolderType = "commit_intent"`：整组 `CAPTURED` 回执原子迁移至 `COMMITTING`，锁定槽位。
2. **租期保护隔离 (Lease Invariant)**：
   - 处于 `COMMITTING`、`REVERTING`、`REVERT_FAILED` 的回执以及 `commit_intent` 组**严禁**被租期到期接管（Lease Takeover Forbidden）。
   - 只有单回合处于 `PENDING` / `EXECUTING` / `CAPTURING` 且租期超时后，才允许在同一 DB 事务内标记 `FAILED(lease expired)` 并重置槽位。
3. **收编确定性锚点 (Intent Trailer & PreCommitSHA)**：
   - 审核批准前记录 `PreCommitSHA`，原子事务将全部目标回执绑定 `CommitIntentID` 并置为 `COMMITTING`。
   - Checkpoint commit 写入尾部标注：`Multigent-Commit-Intent: <commitIntentID>`。
   - Git commit 成功后，整组单事务跃迁为 `COMMITTED` 并释放槽位；快照删除在 DB 事务成功后执行（失败时标记 `SnapshotCleanupPending` 供后台恢复重试）。
   - Git commit 失败时，整组单事务跃迁为 `REVERT_FAILED`，快照完整保留，槽位维持加锁阻断并发。

---

## 2. 灰度发布与观察规则 (Gray Release)

回执机制采用**全局硬门 + 项目白名单**双重防御：

### 环境变量配置
| 变量名 | 默认值 | 说明 |
|---|---|---|
| `MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS` | `false` | 全局总开关。`false` 时所有写操作硬阻断，只读视图正常展示。 |
| `MULTIGENT_PREVIEW_TURN_RECEIPTS_PROJECTS` | `""` | 项目白名单（逗号分隔，精确匹配）。空值、`*` 或未列出项目一律禁止写入。 |

### 灰度分阶段操作
1. **只读观察期**（默认）：
   - `MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS=false`
   - 控制台 Preview 面板正常展示 Turn 历史与 Diff，输入框与回滚按钮显示禁用并提示只读。
2. **受控灰度期**：
   - 开启全局开关：`MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS=true`
   - 仅放行指定测试项目：`MULTIGENT_PREVIEW_TURN_RECEIPTS_PROJECTS=demo-sandbox,test-project`
   - 未在白名单中的项目访问写接口统一返回 `409 feature_disabled`，无任何子进程、Git 或评论副作用。
3. **健康检查与审计观察**：
   - 查看审计日志中的 `preview_turn.executed`、`preview_turn.execution_rejected` 事件。
   - 确认无 `preview_turn.rollback_rejected` 或异常增多的 `REVERT_FAILED` 状态。

---

## 3. Kill-Switch 紧急熔断指南

若线上出现异常行为、模型非预期改动或并发写冲突，立即执行全局熔断：

### 熔断步骤
1. **切断写入开关**：
   将启动环境或配置文件中的环境变量置为：
   ```bash
   MULTIGENT_ENABLE_PREVIEW_TURN_RECEIPTS=false
   ```
2. **热重启 / 重启 Multigent 服务**：
   - 重启后，写端点（`POST .../preview/chat`、`POST .../preview/turns/{turnId}/rollback`）立即对所有项目返回 `409 feature_disabled`。
   - 前端 Preview Copilot 自动切回纯只读模式（历史 Turn、Diff 保持可读，输入框与回滚按钮硬门锁定）。
3. **影响评估**：
   - 熔断**完全不影响**正常工作流运行、任务状态流转及人工审核审批推进。
   - 未完成的写请求将被安全拦截，不会污染任何 Git 分支或 Worktree。

---

## 4. REVERT_FAILED 故障排查与人工处置 SOP

当回执进入 `REVERT_FAILED` 时，表明逆向补丁应用失败、Git 合并冲突或收编提交失败。系统对任务槽位实行 **Fail-Closed 永久加锁**：在此状态下，系统**严禁任何新的 Turn 写入或租赁超时接管**，杜绝并发覆写与代码污染。同时，系统**不暴露任何自动恢复接口**，必须由开发或运维人员人工介入排查。

### 排查步骤
1. **获取故障回执信息**：
   通过审计日志或 API 查看该任务的最新回执：
   - 检查 `status`（`REVERT_FAILED`）及 `failureReason` 错误原因。
   - 记录 `turnId`、`baselineCommit`、`preCommitSHA`、`commitIntentId`。
2. **检查物理快照目录**：
   快照物理保存在项目 Git 根目录下的 `.multigent/turns/<taskId>/<turnId>/snapshot/`：
   ```text
   .multigent/turns/<taskId>/<turnId>/snapshot/
   ├── <modified_files...>      # 该回合修改时的完整文件快照
   ```
   快照完整保留，绝不被自动删除。
3. **查看当前 Worktree 状态**：
   在任务对应的 Worktree 目录下执行非破坏性检查：
   ```bash
   git status --porcelain
   git diff
   git log -n 3 --oneline
   ```
   确认是否有未提交的手工修改或冲突残留。

### 人工处置方案
> [!CAUTION]
> **严禁执行全局破坏性命令**（例如 `git reset --hard HEAD` 或 `git clean -fd`），否则会误删同一 Worktree 内其他未提交的手工修改或未跟踪的工作文件。

- **场景 A：人工确认放弃该回合修改并还原代码**
  1. 依据快照与 Git diff，逐文件确认受影响的文件路径：
     ```bash
     # 逐路径比对工作区与快照
     diff -u .multigent/turns/<taskId>/<turnId>/snapshot/<path_to_file> <path_to_file>
     ```
  2. 针对受影响的文件，使用精准的单文件命令还原至基线版本：
     ```bash
     git checkout HEAD -- <path_to_file>
     ```
  3. 若该回合产生了新增文件，逐个确认后手工删除对应的新增文件：
     ```bash
     rm <path_to_unwanted_new_file>
     ```
  4. 确认工作树恢复整洁且无冲突后，由具备管理权限的运维人员在数据库或后台人工解除任务槽位占用。
- **场景 B：审核收编时由于 Git 冲突失败**
  1. 检查 `failureReason` 中的冲突提示。
  2. 人工在 Worktree 中解决代码冲突并完成提交：
     ```bash
     git add <conflicted_files...>
     git commit -m "chore(review): resolve conflict for task <taskId>"
     ```
  3. 再次在工作流界面点击“通过审核”，系统识别干净工作树后将自动完成任务流转。

---

## 5. 关键审计事件速查表

| 事件名称 (Action) | 触发时机 | 记录字段 (AfterJSON) |
|---|---|---|
| `preview_turn.executed` | 单回合在隔离沙箱内执行并捕获成功 | `project`, `taskId`, `turnId`, `receiptId`, `status` |
| `preview_turn.execution_rejected` | 回合执行被权限、白名单、非人审节点或冲突拒绝 | `project`, `taskId`, `errorCode` |
| `preview_turn.rolled_back` | 回合逆向 patch 成功并恢复工作区 | `project`, `taskId`, `turnId`, `status` |
| `preview_turn.rollback_rejected` | 回滚被白名单、并发冲突、漂移或未找到拒绝 | `project`, `taskId`, `turnId`, `errorCode` |
| `preview_turn.cancelled` | 操作员主动终止预览或中断执行会话 | `project`, `taskId`, `reason` |
| `preview_turn.review_committed` | 人工审核通过，已捕获回执整组收编写入 Checkpoint Commit | `project`, `taskId`, `commitIntentId`, `commitSha`, `turnIds`, `receiptIds` |

> [!IMPORTANT]
> 审计事件严格遵守脱敏与隐私不变量：绝不包含用户 Prompt 原文、DisplayDiff、OperationalPatch、原始 Git/Docker 命令输出或敏感凭据。
