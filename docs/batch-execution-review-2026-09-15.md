# 后续批次执行方案（Batch 1-4 评审与落地细化，v2）

> 日期：2026-09-15。输入：GPT 四批执行计划 + 仓库既有设计文档 + GPT 对本方案
> v1 的六轮评审修正（9 项全部吸收，见各节标注）。
> 本方案对计划逐批评审（含与代码现状的核对结论），并给出可直接开工的落地设计。
> 对应关系：Task 1.2 ← `intranet-runtime-plan.md` 收尾清单；Task 2.1 ←
> `acceptance-test-design-plan.md`；Task 2.2 ← `test-data-fixture-sandbox-plan.md`
> 与沙箱 V1 架构稿；Task 4.2 ← `enterprise-evolution-and-scale-roadmap.md` §3.1；
> Task 4.1 ← `mattermost-chatops-status-and-roadmap.md` + P1b closeout。
> 3.1（Change Run）与 3.2（Skill Profiles）在 docs/ 下**没有已提交的设计文档**
>（"质量方案 Phase 2/3"出处缺失），§6/§7 是补充设计，评审通过后应回填正式文档。
>
> **v2 修订说明**：v1 的止血范围不完整（漏 preview/feedback、preview/stop）、
> 认证主体传递设计有误、权限语义用错（checkProjectAccess 是读访问）；Batch 2/3
> 的 QA-only lease、generator 执行环境、确定性判据、Change Run 机制、黑名单
> 覆盖面、Skill Profile 来源六处契约按 GPT 六轮评审收紧。在途依赖已更新：
> 竞态面五轮复审**已通过**（范围：workflow 状态转换，不含外部副作用 exactly-once）。

---

## 1. 总体评审结论

方向与顺序正确：Batch 1 先封安全洞是对的——**已核实代码确认该洞真实存在，且
比 v1 认定的更大**：

- `preview/chat`（server.go:816）、`preview/feedback`（815）、`preview/stop`
  （818）、`preview/status`（819）、`preview/live`（817）**全部挂在 publicMux**；
  `previewTokenClaims` 只有 `t/p/exp`（preview_token.go:25-29），无任何能力位。
- **feedback 与 chat 同级危险**：feedback handler 写任务评论后调用
  `requestTaskAttentionWakeup`（preview_handlers.go:455 一带）唤醒 Agent 修改
  代码——外部分享链接持有人虽不能直接驱动 Copilot，却能间接触发代码修改。
  **stop 则可让持 token 者停止任意会话**。止血范围必须覆盖全部 preview 控制
  端点，不能只封 chat。
- `previewRequestAuthorized` 自行认证时（preview_token.go:115-128），新 context
  只传给内部 `authorizePreviewProject`，**原 handler 拿到的 `r` 没有认证主体**——
  两个 handler 的评论作者因此退化为 `"user"` 字面量（preview_handlers.go:452、
  550 一带）。授权、审计、评论作者必须拿到显式 principal。

需要修正与补充的六点（v1 观点，v2 保留）：

1. **在途依赖**：竞态面整改 GPT 五轮复审**已通过**（范围：workflow claim →
   guarded commit 状态写入不重复；**不等同于**外部副作用 exactly-once）。
   Batch 1 无阻塞。
2. **Task 2.2 改动位置**：资源所有者是 preview/执行沙箱 lease，`gitworktree`
   保持纯代码基线（六大生命周期解耦红线）。
3. **Task 2.1 双层体系**：greenfield vNext 新版本 + 重新实例化；不碰 unified
   与存量实例。
4. **工期**：Batch 2 约 5-6 天、Batch 3 约 4-5 天。
5. **"不做什么"**：沙箱 V1 不做特性迁移 provision、DSL 造数、候选晋级发布、
   前端抽屉；Change Run V1 同一任务至多一个活跃 proposal。
6. **开工顺序**：按 GPT 六轮建议——Batch 1（1-3 项修正后）先开工；Batch 2/3
   的 4-9 项契约（§4-§8）确认后再动工。

---

## 2. Batch 1：安全止血（2 天；范围按六轮评审扩大）

### 2.1 Task 1.1 预览控制面权限封门（范围 = 全部 preview 控制端点）

**改动位置**：`internal/api/preview_token.go`、`internal/api/preview_handlers.go`。

**第一步：preview 端点能力矩阵**（先盘点后动手，防再漏）：

| 端点（均现挂 publicMux） | 现状 | 目标：分享 token | 目标：登录用户 |
| --- | --- | --- | --- |
| 应用代理/静态资源/`preview/live` | token 或登录均可读 | 仅 `preview.view`（只读浏览） | 可读 |
| `preview/status` | token 可读 | **默认不开放**——先审计响应体是否泄露 Agent 输出/内部路径，确认无泄露再决定；审计前一律要求登录 | 可读 |
| `preview/feedback` | token 即可写评论 + **唤醒 Agent 改代码** | **403** | 登录 + operator 权限 + 审批人校验 |
| `preview/chat` | token 即可驱动 Copilot | **403** | 同上 |
| `preview/stop` | token 即可停会话 | **403** | 登录 + operator 权限 |

**第二步：token 能力位（向后兼容 fail-closed）**：`previewTokenClaims` 增加
`Cap string`（`preview.view`）。旧 token 无该字段 → 视为无写能力，存量分享
链接立即降级为只读，无需失效重发。`signPreviewToken` 一律 mint `preview.view`。

**第三步：写端点专用 helper，显式返回 principal**（修正 v1 设计错误）：
不能在 `previewRequestAuthorized` 之后检查 `ctxUserKey`——该函数自行认证时只把
新 context 用于内部 `authorizePreviewProject`，handler 手里的 `r` 没有认证主体
（现状评论作者因此退化为 `"user"` 字面量）。新增：

```go
// previewWritePrincipal 返回经认证的请求与主体；分享 token、匿名、
// 认证失败一律 401/403 并返回 false。
func (s *Server) previewWritePrincipal(w http.ResponseWriter, r *http.Request, project, taskID string) (*http.Request, previewPrincipal, bool)
```

- 拒绝携带 preview token 的请求（写端点不认分享 token，403）；
- 自行认证后构造 `r = r.WithContext(ctx)` **返回给 handler**——授权判定、
  审计记录、评论作者全部使用返回的 principal（username），不再有 fallback；
- principal 缺失（username 为空）→ fail-closed 拒绝。

**第四步：权限语义升级（fail-closed）**：

- `checkProjectAccess` 是**读访问**（= `canAccessProject`），不能作为 Copilot
  写授权。写端点用 `checkProjectOperator`/`canOperateProject`（auth.go:1142/1132
  已存在）；
- 叠加审批人校验：任务停在 human_review 步骤且 assignee 为 human 时，要求
  `principal.username == step.ActorID`；assignee 为 agent 或无 assignee 时退回
  operator 判定（与审批端点口径一致）；
- **fail-closed 三态**：工作流查询失败 → 拒绝；项目无法解析（非 `current`
  兜底后仍为空）→ 拒绝；Actor 类型不符且无法确认操作权 → 拒绝；
- 任务 `InProgress` 且非 human_review：维持现有 409 执行锁；只读快照维持
  `previewInstanceReadOnly` 409；chat 限流 `allowPreviewChat` 不变。

**测试**（单元 + httptest 集成，覆盖矩阵每一格）：

- feedback/chat/stop × {匿名、仅分享 token、登录无 operator、登录 operator
  非审批人、审批人登录} 五态断言（401/403/200）；
- 回归：带 preview token 的代理/静态资源/`preview/live` 仍 200；旧格式 token
  （无 Cap）读 200、写 403；409 执行锁与限流行为不变；
- principal 贯穿：feedback/chat 的评论作者 = 登录名（不再出现 `"user"`）；
- `preview/status` 审计结论记录在案（泄露 → 保持登录制；无泄露 → 可放宽，
  单独 commit 说明）。

### 2.2 Task 1.2 内网运行时 P0 审计收尾（验证任务，非开发）

逐项打勾 `intranet-runtime-plan.md` 收尾清单：

- [ ] 全量 `make test` + `make build` 门禁；
- [ ] 空配置回归：BuildArgs 输出与改前逐字节一致（硬性要求）；
- [ ] `multigent runtime probe` 本机功能验证（配错端口 → FAIL 非零退出码）；
- [ ] VM 上 `/opt/multigent/data/` 配置严格解析自检：无历史遗留未知键导致启动
  失败（部署细节走私有 runbook，仓库文档只用占位符）；
- [ ] 变更清单输出，交用户审计。

完成定义：intranet-runtime-plan §6 的勾选全部落地 + 验收记录写入 HANDOFF。

---

## 3. Batch 2：质量铁三角（5-6 天）

### 3.1 Task 2.1 前置验收测试 Agent（约 2.5 天）

按 `acceptance-test-design-plan.md` Batch A/B/C 执行，此处只列落地要点与
计划原文的差异：

- **模板**：新建 `greenfield-delivery-pipeline` vNext（版本 +1），节点
  `acceptance_test_design`（agent_task，qa-agent）插在 `design_review` 与
  `implementation` 之间；下游 implementation/self_review/code_review/qa/
  qa_signoff 按文档 §4.4 表更新输入输出字段与 Prompt。改模板后**必须重新
  实例化**才对新任务可选（双层体系）；不触碰 unified 与存量实例。
- **输出契约**：`test_spec_doc_id` / `test_spec_manifest`（结构化 JSON）/
  `test_spec_summary`。每条 Case 最小字段集按文档 §4.3（case_id、ac_id、
  risk_level、automation_level、execution_type、预期结果等）。
- **manifest 确定性校验**：workflow 层纯函数校验（非空、合法 JSON、case_id
  唯一、AC 引用/风险/预期结果必填）——参照 ci_ready 的"确定性优先"先例：
  能用纯函数校验的不借道 agent。
- **返工回流**：`qa_signoff → implementation` 复用 e-qa-rework 模式；失败项
  （failed/blocked/unexecuted）+ 规格引用无损汇总进 `review_comments`。
- **测试**：文档 §8 六项（模板结构、manifest 校验、端到端含 QA 打回、QA 写
  权限边界、前端展示、make 全绿）。

### 3.2 Task 2.2 测试数据沙盒 V1（约 3-3.5 天；契约按六轮评审收紧）

**前置 Task 0（必须，约 0.5 天）——模板 DB 可重定位**：旗舰模板
`react_go_fullstack` 当前**没有数据库**（纯 JSON API + 内嵌前端）。给它加：
环境变量可重定位的 SQLite 路径 + `db:seed:baseline` / `db:seed:scenario`
脚本 + `server/data/` gitignore。否则 V1 上线零个真实可用项目。

**核心实现**（资源所有者 = 执行沙箱 lease；**不动 gitworktree**）：

1. **契约加载器**：`.multigent/fixtures.json` 校验（engine 仅 sqlite；
   `relativePath` 拒绝绝对路径/`..`/symlink 跳出；argv 非空、超时封顶、glob
   标准化排序后摘要）。
2. **artifact catalog + lease 持久化（kv_records）**：`PreviewInstance` 目前是
   Engine 内存 map，重启靠容器 label `Reconcile` 重建——**lease 必须落库**。
   artifact 存本地受控目录（MULTIGENT_DATA_DIR 下）+ 元数据入 kv_records；
   对象存储留适配器接口不做实现。
3. **两种 lease 类型，禁止空 ID 逃逸**（六轮修正）：v1 的"QA-only lease
   `previewLeaseId` 可为空"与"lease 是资源所有者"矛盾——无预览引用的 lease
   无人回收。改为：QA/无预览场景创建独立的 **`ExecutionSandboxLease`**（与
   preview lease 同构：有自己的 ID、状态机、租期、dataDirectory、到期回收），
   同样落 kv_records、同样进 reaper。每个数据目录都归属于一条**有 ID、可
   追溯、可回收**的 lease。
4. **lease 状态机用跨进程 CAS**（六轮修正）：reset/回收的并发防护不是单进程
   mutex——多 console/多进程共享 SQLite 时必须以 lease 行的
   `state + revision` CAS（复用 transition-claim 同款 payload+revision 原语）
   做状态迁移仲裁：`active → resetting → active`、`active → reclaiming →
   removed`，CAS 失败即让位。单进程 mutex 只作为进程内优化，不作为正确性依据。
5. **generator 在一次性 sandbox 容器内执行**（六轮修正，安全红线）：项目契约
   给出的 `argv` **不得由 Go 主机进程直接执行**——那会把项目脚本执行权带到
   宿主机。generator 在无生产凭据、网络受限的一次性容器中运行（复用沙箱镜像
   与网络策略；不挂载 Docker socket、不注入模型 key、不透传宿主 env），产出
   artifact 后再挂载给 preview。容器内执行同样有超时与输出上限。
6. **provision**：冻结 baseCommit 时解析 fixtureVersion → artifactDigest；
   校验完整性/大小上限/模板指纹后复制为 task-private.db；同目录管理 WAL/SHM；
   注入环境变量指向私有库；执行 scenario（固定 seed、UTC、固定 locale）。
7. **fail-closed on schema 漂移**（六轮修正）：V1 不支持特性迁移——worktree
   的 schema fingerprint 与冻结 artifact 的 `templateSchemaFingerprint` 不一致
   时，**明确拒绝 provision 并拒绝启动预览**（报"需要发布兼容的新 fixture"），
   绝不悄悄用基线数据启动一个 schema 对不上的应用。
8. **引擎集成点**：`engine.go:327/334` 的 `npm run seed || true`（沙箱方案
   点名批判的吞错行为）替换为幂等 provision 调用；失败 fail-closed 不启动预览。
9. **reset/回收**：reset = CAS 拿到 resetting 态 → 停写 → 删目录 → 从冻结
   digest 重建 → CAS 回 active；reaper 沿用 30 分钟租期 + 每分钟扫描，
   **先停容器再删目录**；重启后按持久化 lease 归账孤儿目录，幂等。
10. **验收与确定性判据**（六轮修正）：确定性比较**规范化的逻辑数据导出**——
    固定表清单、固定列序、固定排序的行集导出（如逐表 `SELECT * ORDER BY pk`
    的规范化文本/哈希），**不比较 SQLite 文件字节**（页布局、WAL、header 计数
    都不是稳定判据）。其余验收沿用方案 §8：同基线 5 次导出一致、并发隔离、
    reset 复原、生命周期物理清理、生产 CI 交付物扫描无沙箱数据。"100ms 重置"
    记为条件性 p95 SLO，不写进完成定义。

**V1 明确不做**：特性分支迁移 provision、DSL 造数、候选晋级发布、前端悬浮
抽屉（Phase 2）。

---

## 4. Batch 3：受控变更与 Skill 治理（4-5 天）

### 4.1 Task 3.1 受控 Change Run（约 3 天，含 UI）

> 无既有设计文档，以下为补充设计（v2 按六轮评审收紧为机制而非词），评审通过
> 后回填 docs。

**状态机**：

```text
Proposed → AwaitingApproval → Applying(验证) → Applied
                    │                └→ VerificationFailed(回滚)
                    └→ Rejected
```

- **提议持久化**：kv_records（`preview_change_proposals`，key
  [project, taskID, proposalID]）：actor（显式 principal，同 Task 1.1）、
  原始请求、补丁、Diff、状态、验证结果、时间戳。**同一任务同时至多一个活跃
  proposal**（V1 不做并行）。
- **隔离产出 = shadow worktree 机制，不是 Prompt 约束**（六轮修正）：当前
  Agent 对 worktree 有真实写权限，靠 Prompt 说"别写"不可验证。机制：为 proposal
  创建 **shadow worktree**（同一 baseCommit 的临时 worktree，或 `git worktree`
  + 临时分支），Copilot 在 shadow 内产出改动；平台对 shadow 做
  `git diff` 提取 patch。用户在预览抽屉看的是 **shadow 的 Diff**，主 worktree
  在批准前不被触碰。
- **Apply 机制**：确认后申请写锁（`isTaskAtHumanReviewStep` fail-closed +
  `previewSessions` 互斥）→ 校验 **preimage hash**（patch 基于的文件内容哈希
  与主 worktree 当前内容一致，不一致 = worktree 已漂移，拒绝并要求重新生成）→
  在**主 worktree** 应用 patch → 按 runtime.json 契约跑 lint/build（超时封顶）。
  失败回滚：worktree 快照回退（快照失败必须阻断——沿用 gitworktree 防丢未推送
  工作的既有约定）。
- **收编触发点与人工审核显式对齐**（六轮修正）：Apply 成功**不做**泛化
  `git add -A` 自动 checkpoint/push。改动以未提交工作区状态存在，收编
  （checkpoint commit / 收入审核提交）只发生在既有的人工审核 approve 链路
  （`commitAndPushReviewChanges`），触发点不变、审计归属不变。
- **高风险路径黑名单（确定性校验，不走模型判断）**：对 patch 触及的**每一条
  路径**（新增、删除、重命名的源与目标、symlink 创建）做规范化（clean path、
  解 `..`、大小写按平台策略）后匹配黑名单；**二进制 patch 一律拒绝**（无法
  审查 Diff）。黑名单默认集合（项目契约可收紧、不可放宽）：
  `.gitlab-ci.yml` 及其他 CI 配置（`.github/workflows/`）、`migrations/`、
  `Dockerfile*`、`.multigent/`（fixtures.json/runtime.json）、凭据与环境文件
  （`.env*`、`*credentials*`、`*.pem`、`*.key`）、Agent 指令文件
  （`AGENTS.md`、`CLAUDE.md`）、Skill 文件（`.agents/skills/`）、deploy/ 目录、
  Git 元数据操作（`.git/` 下任何路径、submodule/modules 变更、hooks 目录）。
- **与 Task 1.1 的关系**：1.1 管"谁能发起"（登录 + operator + 审批人），Change
  Run 管"发起后怎么落地"（shadow → 审 → apply → 验证），两层闸门串联。
- **UI**：预览抽屉展示 shadow Diff + 确认/拒绝；流式进度复用现有
  previewSessions SSE 通道。
- **测试**：黑名单各形态（新增/删除/重命名/symlink/二进制/路径穿越规范化）、
  preimage 漂移拒绝、无锁 Apply 拒绝、验证失败回滚完整性、proposal 串行化、
  1.1 权限矩阵在入口同样生效、apply 后收编仍只由审核链路触发。

### 4.2 Task 3.2 Skill Profiles 受控挂载（约 1.5 天）

- **以现有 Skill 文件资产为唯一来源，不另起 kv catalog**（六轮修正）：Skill
  本就是文件型资产，且平台 formatter 会复制附带脚本并赋予 `.sh` 可执行权限——
  另建一套 kv 目录会制造两个真相来源。digest 对 **`SKILL.md` 与全部附件**计算
  整体 SHA256（任一附件变更即失配），存于现有 Skill 元数据旁；挂载时校验。
- **Intent Profile**：`ui-polish`、`a11y-remediation` 等预设 profile 是
  **allowlist 内 skill ID 的命名集合**（服务端定义），预览抽屉勾选 → chat
  请求携带 profile 名 → 服务端解析为 skill 集合、校验整体 digest 后注入。
- **执行附件默认禁用**（六轮修正）：preview profile 场景下，skill 的可执行
  附件默认不进入沙箱（或要求额外 runtime capability 才挂载）——预览调优
  不应获得任意脚本执行面。前端仍只传 profile 名，禁止内联注入任意 Prompt
  文本（缩小提示注入面）。

---

## 5. Batch 4：团队化与存量接入（验收为主，穿插进行）

- **4.1 Mattermost 双人真实流转终验**：两个真实账号互审、打回、重试、@通知、
  卡片交互、重启恢复全闭环。验收任务：基于最新代码（P1b closeout 已修
  DEFECT-C3 等），输出验收记录。
- **4.2 Brownfield 存量接入**：按 enterprise-evolution §3.1 五步就绪流水线
  （只读探测 → 受控基线 → 最小验证 → 就绪报告 → 权限准入）。运行时治理一半
  已由 canary 回填覆盖，本任务补齐：已有 GitLab 仓库拉取、私有依赖、既有 CI
  识别、凭据（不落盘红线）与回推。
- **4.3 系统垃圾自愈**：孤儿 worktree 扫描清理（复用/参照 delete_handlers 的
  孤儿处理）+ Gradle 共享缓存卷挂载（缓存卷参数属部署配置，进私有 runbook，
  仓库文档占位符）。

---

## 6. 排期与在途依赖

| 批次 | 内容 | 修正后工期 | 前置 |
| --- | --- | --- | --- |
| 1 | 1.1 preview 控制面封门 + 1.2 运行时收尾 | 2 天 | 无（立即开工） |
| 2 | 2.1 验收测试 Agent + 2.2 数据沙盒 | 5-6 天 | Batch 1；§3.2 的 3/4/5/7 项契约（lease 模型、CAS、容器执行、fail-closed 漂移）确认 |
| 3 | 3.1 Change Run + 3.2 Skill Profiles | 4-5 天 | Batch 1；§4 的 shadow worktree、preimage、黑名单、Skill 来源契约确认 |
| 4 | 双人终验 / Brownfield / 自愈 | 验收类，穿插 | 对应批次完成 |

在途：竞态面整改 **GPT 五轮复审已通过**（范围：workflow claim → guarded
commit 状态写入不重复；不等同于外部副作用 exactly-once——引用结论必须携带
范围限定）。

## 7. 每批次完成定义（红线对照）

- `make test` + `make build` 全绿；行为级验证按目标环境私有 runbook。
- 新端点一律注册在带 `withTokenAuth` 的 mux + `checkProjectAccess`；**写端点
  授权至少 `checkProjectOperator`（checkProjectAccess 是读语义）**；确需暴露给
  预览 iframe 的，handler 内校验预览签名 token，写端点加频率限制，且认证主体
  必须显式返回给 handler（授权、审计、评论作者共用，禁止 `"user"` fallback）。
- 凭据不落盘（remote URL 纯净、输出 redact）；并发写保护用
  `isTaskAtHumanReviewStep` fail-closed；任务派生基于不可变 baseCommit。
- 项目提供的脚本/命令一律在无生产凭据、网络受限的沙箱容器内执行，禁止 Go
  主机进程直接 exec 项目契约给出的 argv。
- 不 rebase、不 force push；work/、dist/ 产物不入库；正式文档不含部署细节。
