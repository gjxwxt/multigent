# IM 任务线程内 @提及 → 任务裁决关联与回复路由 (IM Mention Task-Thread Binding)

> 状态：设计定稿 v1（2026-09-19）。来源：任务 t-20260919-sgv20t 实测事故——
> Agent 在任务线程升级"ci_ready 需人工裁决 A/B/C"，用户在 Mattermost 任务线程内 @机器人回复
> "A"，消息送达但被当作闲聊（agent 回复"收到～你发了个a"到别处），最终 agent 未经裁决自行
> 重塑 CI 并完成任务。本文档与实现同一 commit 提交。

## 1. 问题定义

任务线程是任务在 Mattermost 中的投影（Live Task Card 根帖 + 线程内更新）。用户在该线程内
@机器人 发送的内容天然是对该任务的协调输入（裁决、补充说明、优先级调整），但当前平台把它
当作无任务坐标的普通 IM 信号处理：

1. **入站信号不知道任务**：`recordIMAttentionSignal` 写入的 refs 只有
   `{bindingId, project, agent, chatId, messageId, chatType, messageType}`，
   没有 taskId / threadRoot。wakeup prompt 里 agent 看到的是"有人在频道里说了 A"，
   而非"用户对任务 X 的裁决为 A"。
2. **回复不进线程**：agent 后续主动回复走 `mga notify send --to source`，源信息从
   wakeup prompt 的 Source/Refs 行解析（`runtimeNotifySourceFromPrompt`），
   只能带回 message.id 级别的回复目标；用户在任务线程根帖下留言时，回复虽然能落到
   根帖附近，但没有"任务线程"语义，容易散落到主频道或 DM。

**实测证据（t-20260919-sgv20t）**：入站 "a" 正常记录为 attention signal 并注入 04:35
wakeup prompt（传输层全通），但 refs 无 taskId，agent 无法将其与待裁决的 ci_ready 关联，
判定为打招呼并回复到错误位置。

## 2. 现状盘点（代码事实，2026-09-19 @ dev）

| 环节 | 位置 | 现状 |
|---|---|---|
| 任务线程投影 | `internal/db/task_thread_projections.go` | `(provider, channel_id, root_post_id) → task_id` 映射已持久化，`TaskThreadProjectionByRoot` 查询现成 |
| 线程根帖 | `internal/imbridge/mattermost.go` ParseEvent | 入站消息已解析 `RootID`（Mattermost 线程内所有回帖 root_id 恒指线程根帖） |
| 出站线程路由 | `internal/api/runtime_notify_handlers.go:1287` `resolveTaskThreadTarget` | 已完整实现：wakeup 任务的 `MULTIGENT_WAKEUP_TARGET_TASK_ID` → `ActiveTaskThreadProjection` → root_id 回帖；taskId 客户端值必须与可信任务上下文一致（防注入） |
| wakeup 目标任务解析 | `internal/api/scheduler_attention.go:316` `attentionWakeupTargetTask` | 只统计 `SourceKind=="task"` 的信号；IM 信号（`im_message`）带 taskId 时不参与 |
| 入站信号记录 | `internal/api/agent_channel_events.go:2071` `recordIMAttentionSignal` | refs 不含 taskId/rootPostId |
| 信任分级 | `attentionSignalTrust` | authenticated_user + authorized 已实现 |

结论：**出站回复线程化、wakeup 目标任务、prompt 注入、防注入校验全部现成，缺的只是
入站信号与任务的绑定这一环**。补上它，下游全自动生效。

## 3. 方案

### 3.1 入站绑定：信号锚定任务（核心改动）

`recordIMAttentionSignal` 在写 refs 前，若 `message.RootID != ""`，用
`(provider, chatId, rootId)` 反查 `TaskThreadProjectionByRoot`；命中且投影属于同一
project 时，refs 增补：

```json
{ "taskId": "<投影的 task_id>", "threadRoot": "<root_post_id>" }
```

约束：
- 只查 `status='active'` 投影（ByRoot 已内置）——任务结束线程关闭后，同线程的后续 @
  退化为普通信号，不复活旧任务上下文；
- 反查失败（未命中/DB 错误）静默降级为现状行为，**绝不因关联失败丢弃信号**；
- taskId 来自服务端投影表，不接受客户端声明，无注入面。

### 3.2 wakeup 目标任务：纳入 IM 信号（解锁线程回复）

`attentionWakeupTargetTask` 扩展：`SourceKind=="im_message"` 且 refs.taskId 非空的
信号与 task 信号同等参与"唯一任务"归集；多个不同任务并存时维持 fail-closed（返回空，
不猜）。存在性校验（`s.ts.GetTask`）复用现有逻辑——taskId 必须真实存在于任务存储，
防止指向已删除任务的悬垂引用。

命中后 `MULTIGENT_WAKEUP_TARGET_TASK_ID` 落到 wakeup 任务 vars，
`resolveTaskThreadTarget` 即可将 agent 的 `--to human` / `--to source` 回复路由进
任务线程根帖（现有第 8-11 步校验全部沿用）。

### 3.3 prompt 指令增强（让 agent 知道坐标）

wakeup prompt 的 AttentionHint（中英双语）追加一句：信号带 taskId 时，说明这是用户在
该任务线程内的协调输入（裁决/补充/澄清），应结合该任务当前状态解读；回复默认发回任务
线程（现有 `--to source` 机制自动落线程，无需 agent 额外动作）；任务 ID 见 Refs 行。

不注入任务全文——agent 已有 `mga` 工具按 taskId 自取上下文，避免 prompt 膨胀与双源。

### 3.4 优先级分层（呼应"会话内优先"）

- 任务线程内 @（命中 3.1）：signal Priority 已是 high，保持；Reason 细分为
  `im_task_thread_mention`，wakeup 调度侧天然先于普通信号处理；
- 主频道 / DM @：维持现状（`im_group_mention` / `im_direct_message`，normal/high）。
  不在本改动引入新调度权重，避免行为漂移；分层收紧留待有真实调度冲突证据再做。

### 3.5 明确不做（范围外）

- **裁决词表解析**（把 "A"/"approve" 自动转成 decision 提交）：裁决语义属于工作流
  人工审核节点，有专门的卡片交互通道（`mga workflow decision submit` / 审批卡片）。
  线程自由文本与结构化裁决之间的映射交给 agent 结合任务上下文判断，平台不猜。
- **跨 provider 通用化**：飞书/Lark 的 thread 语义不同（root_id 不稳定），本期仅
  Mattermost；飞书侧留待其线程投影落地后按同模式接入。
- **主频道回复的强制改道**：用户在主频道 @ 时尊重提问位置，回复不强行进线程。

## 4. 安全与红线核对

- **端点不变**：无新增 HTTP 端点；入站仍走 HMAC 签名的 `/api/v1/im/{provider}/events`。
- **无凭据面**：refs 只存 taskId/rootPostId（平台内部标识），无秘密。
- **注入防御**：taskId 全部来自服务端投影表与任务存储校验，客户端不可指定
  （与 `resolveTaskThreadTarget` 第 5 步的 trusted-task 校验同源同理）。
- **fail-open 信号、fail-closed 路由**：信号关联失败不丢消息；线程回复的目标校验
  （channel 归属、project 归属、IM 实例一致）维持既有强校验。
- **并发**：投影反查为只读单行查询；UpstreamThreadProjection 写入路径
  （LiveCard 创建）不变。

## 5. 验证计划

1. **单测**（`internal/api`）：
   - 线程内 @ + 投影存在 → refs.taskId/threadRoot 正确、Reason=`im_task_thread_mention`；
   - 无 RootID / 投影未命中 → 行为与现状完全一致（回归保护）；
   - IM 信号带 taskId 参与 `attentionWakeupTargetTask`：单一任务命中、多任务 fail-closed、
     悬垂 taskId 被存在性校验剔除。
2. **全量回归**：`make test`。
3. **部署与行为级验证**（VM，遵循 HANDOFF 坑 19/25/30）：
   交叉编译 linux/amd64 → stdin 管道部署 → `systemctl restart multigent` →
   `GET /api/v1/health` version 核对 → 实际在 react-component 任务线程 @机器人
   发送协调消息 → 验证 attention signal refs 含 taskId、agent wakeup prompt 呈现
   任务坐标、回复落在线程内。

## 6. 与后续工作的关系

- 本改动是"agent 决策分层"（事实归 agent / 策略签核归人）的前置通道：只有裁决消息
  能带着任务坐标到达 agent，"人签核"才有时效价值。
- ciready profile 分层（检查子集 + 豁免留痕）为独立改动，见会话记录；不阻塞本篇。
