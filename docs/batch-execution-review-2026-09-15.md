# 后续批次执行方案（Batch 1-4 评审与落地细化，v3）

> 日期：2026-09-15。输入：GPT 四批执行计划 + 仓库既有设计文档 + GPT 对本方案
> 的六、七两轮评审修正（v2 吸收六轮 9 项；v3 吸收七轮：同源窃取 P0、live
> 升级、widget 迁移、dirty-worktree 与回滚语义）。
> 本方案对计划逐批评审（含与代码现状的核对结论），并给出可直接开工的落地设计。
> 对应关系：Task 1.2 ← `intranet-runtime-plan.md` 收尾清单；Task 2.1 ←
> `acceptance-test-design-plan.md`；Task 2.2 ← `test-data-fixture-sandbox-plan.md`
> 与沙箱 V1 架构稿；Task 4.2 ← `enterprise-evolution-and-scale-roadmap.md` §3.1；
> Task 4.1 ← `mattermost-chatops-status-and-roadmap.md` + P1b closeout。
> 3.1（Change Run）与 3.2（Skill Profiles）在 docs/ 下**没有已提交的设计文档**
>（"质量方案 Phase 2/3"出处缺失），§6/§7 是补充设计，评审通过后应回填正式文档。
>
> **v3 修订说明**：七轮指出 v2 仍漏一个**更上游的 P0**——预览应用与控制台共享
> 同一 origin，控制台 Bearer token 存 `localStorage["multigent-token"]`
> （web/src/lib/auth.tsx:3,69,139），被代理的项目应用（其代码可被 Agent 修改）
> 的任何脚本都能读走真实用户 Bearer 直接调控制台 API——此时 preview 端点的
> capability 校验被真实凭据绕过。v3 新增 §2.0 origin 隔离决策（Batch 1 的
> 第一个任务，先于一切端点封门）；`preview/live` 升级为登录主体（SSE 原样转发
> Agent 会话输出，无脱敏）；`previewWritePrincipal` 补 widget 迁移语义；Batch 3
> 补 dirty-worktree 基线与原子回滚契约。

---

## 1. 总体评审结论

方向与顺序正确：Batch 1 先封安全洞是对的——**已核实代码确认该洞真实存在，且
分两层**：

- **上游层（七轮新指出的 P0，先于一切端点封门）**：预览应用与控制台共享同一
  origin——`handleTaskPreviewProxy` 把项目应用反代在同 origin 的
  `/preview/{task}/...`（server.go:773 publicMux；preview_handlers.go:798-840），
  而控制台 Bearer token 存 `localStorage["multigent-token"]`（auth.tsx）。被
  预览应用的代码可被 Agent/任务修改，其页面脚本能读走真实用户 Bearer 直接调
  控制台 API——端点级 capability 校验对**真实凭据**无效。必须先做 §2.0 的
  origin 隔离决策。
- **端点层（六轮已核实）**：`preview/chat`（816）、`feedback`（815）、`stop`
  （818）、`status`（819）、`live`（817）全部挂在 publicMux；
  `previewTokenClaims` 只有 `t/p/exp`（preview_token.go:25-29），无能力位。
  feedback 写评论后 `requestTaskAttentionWakeup` 唤醒 Agent 改代码；**live 的
  SSE 把 Agent 会话输出（stdout/stderr 行）原样转发给持分享 token 者**，无
  任何脱敏（preview_handlers.go:706-740）——与 feedback/chat/stop 同级，要求
  登录主体。
- **主体传递缺陷（六轮已核实）**：`previewRequestAuthorized` 自行认证时新
  context 只传内部 `authorizePreviewProject`，handler 手里的 `r` 无认证主体，
  评论作者退化为 `"user"` 字面量。
- **widget 注入面（七轮补充核实）**：preview 代理向被预览应用的 HTML 注入
  `/_multigent_preview/feedback.js` 与 `window.__MG_PREVIEW_TOKEN__`
  （preview_handlers.go:844,1024-1032）；feedback.js 对 live/status/stop/chat
  的 fetch **自动附带 pvt**（feedback.js:33-38,1245,1300,1444）。因此写端点
  "见 pvt 即拒"会把现有登录用户一并 403——必须与 widget 迁移同步设计（§2.1
  第三步）。

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

## 2. Batch 1：安全止血（2.5-3 天；§2.0 是第一任务，先于端点封门）

### 2.0 Task 1.0 预览 origin 隔离（P0，七轮新增；决定 §2.1 的落地形态）

**威胁**：被预览应用与控制台同 origin（`/preview/{task}/...` 反代），应用脚本
可读 `localStorage["multigent-token"]` 拿到真实用户 Bearer，直接调控制台
API——任何端点级校验都被真实凭据绕过。

**决策（推荐顺序）**：

1. **独立 origin/subdomain（推荐目标态）**：Preview 以独立 origin 提供
   （如 `preview.<console-host>` 或独立端口）；分享 token 仅在 preview origin
   生效；控制台 cookie/localStorage 天然对 preview 文档不可见。落地：
   preview 代理改写为按 Host/子域路由（部署面 + server 路由判断），分享链接
   生成改为 preview origin 绝对 URL。控制台与 preview 的 API 仍同进程。
2. **Copilot 控制 UI 迁出被预览文档**：chat/feedback/stop 的 UI 放在**认证后的
   控制台父页面**（现有 AssistantWidget 组件体系），以 Bearer 经控制台自身
   origin 调用；**不再向被预览应用的 document 注入 feedback.js 与
   `__MG_PREVIEW_TOKEN__`**（删除 preview_handlers.go:844,1024-1032 注入点，
   `/_multigent_preview/feedback.js` 退役）。
3. **短期过渡（若独立 origin 暂不可行）**：预览 iframe 使用**不带
   `allow-same-origin` 的 sandbox 属性**——应用文档落入 opaque origin，
   读不到控制台 localStorage。**必须专项验证应用能力损失**（应用自身
   localStorage/cookie/Service Worker 失效、同源 fetch 凭据语义变化），对
   不兼容的应用在任务面板明示降级原因；验证结论记录在案后才可作为过渡态。

**完成定义**：三选一落地 + 测试证明——preview 文档内执行脚本无法读到控制台
token（E2E：向预览页注入探测脚本，断言 `localStorage.getItem('multigent-token')`
不可达）；分享 token 仅绑定 preview 面。

### 2.1 Task 1.1 预览控制面权限封门（依赖 §2.0 的形态决策）

**改动位置**：`internal/api/preview_token.go`、`internal/api/preview_handlers.go`。

**第一步：preview 端点能力矩阵**（先盘点后动手，防再漏；`live` 按七轮升级）：

| 端点（均现挂 publicMux） | 现状 | 目标：分享 token | 目标：登录用户 |
| --- | --- | --- | --- |
| 应用代理/静态资源 | token 或登录均可读 | 仅 `preview.view`（只读浏览） | 可读 |
| `preview/status` | token 可读 | **默认不开放**——先审计响应体是否泄露 Agent 输出/内部路径，确认无泄露再决定；审计前一律要求登录 | 可读 |
| `preview/live` | token 可读，**SSE 原样转发 Agent 会话输出，无脱敏**（handlers.go:706-740） | **403**（同 feedback/chat/stop：要求登录主体——Agent 输出含路径、命令、可能的凭据回显，比 status 更危险） | 可读 |
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

- **"拒绝携带 preview token 的请求"须与 widget 迁移同步**（七轮修正）：现状
  `feedback.js` 对 chat/live/status/stop 的 fetch **自动附带 pvt**（1245/
  1300/1444 行），若直接"见 pvt 即 403"，现有**已登录用户**也会被拒。因此
  本 helper 的拒绝语义必须分两种：
  - 请求**仅凭** preview token 可认证（无 Bearer/会话）→ 403（分享面无写权）；
  - 请求同时带 preview token **和**有效登录态（widget 迁移过渡期）→ 认登录态，
    放行到 operator 校验。
  配套迁移（§2.0 第 2 项落地后自然收敛）：共享预览不注入 Copilot widget；
  认证控制台的控制面请求走**控制台自身 origin + Bearer**（AssistantWidget
  体系），不再依赖注入的 feedback.js。迁移完成前的过渡期内，两种主体并存
  以登录态优先；迁移完成（feedback.js 退役）后删除 pvt 旁路。
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

- feedback/chat/stop/**live** × {匿名、仅分享 token、登录无 operator、登录
  operator 非审批人、审批人登录} 五态断言（401/403/200）；
- **过渡期双主体**：带 pvt + 有效登录态的请求（现状 widget 形态）必须按登录态
  放行到 operator 校验，不因 pvt 存在而 403；仅 pvt 无登录态 → 403；
- 回归：带 preview token 的代理/静态资源仍 200；旧格式 token
  （无 Cap）读 200、写 403；409 执行锁与限流行为不变；
- principal 贯穿：feedback/chat 的评论作者 = 登录名（不再出现 `"user"`）；
- `preview/status` 审计结论记录在案（泄露 → 保持登录制；无泄露 → 可放宽，
  单独 commit 说明）；
- **origin 隔离 E2E**（§2.0）：预览文档内探测脚本读不到控制台 token。

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
  创建 **shadow worktree**（`git worktree add` 临时分支），Copilot 在 shadow
  内产出改动；平台对 shadow 做 `git diff` 提取 patch。用户在预览抽屉看的是
  **shadow 的 Diff**，主 worktree 在批准前不被触碰。
- **proposal 基线 = 主 worktree 的真实状态，dirty 必须显式建模**（七轮修正）：
  shadow 以 `baseCommit` 创建时看不到主 worktree 的未提交变更——而 Copilot 要
  改的往往正是这些状态。契约二选一，按序尝试：
  1. **主 worktree 必须 clean**（`git status --porcelain` 为空）才接受
     proposal；不 clean 时向用户明确报"请先提交或暂存当前变更"，不静默继续；
  2. 用户显式选择"以当前未提交状态为基线"时：先创建**受控临时快照**
     （`git add -A` + `git write-tree` 的**只写对象、不动 HEAD/分支/索引**的
     临时树），把该树 SHA 记为 proposal 的 `baselineCommit`，shadow 从它创建；
     patch 基线、preimage 校验、Diff 展示全部以 `baselineCommit` 为准。该快照
     属 proposal 私有，随 proposal 生命周期回收，绝不移动主 worktree 的任何
     Git 状态。
- **Apply 机制（原子实现）**：确认后申请写锁（`isTaskAtHumanReviewStep`
  fail-closed + `previewSessions` 互斥）→ 校验 **preimage hash**（patch 基于
  `baselineCommit` 的文件内容哈希 == 主 worktree 当前内容哈希；不一致 = 已漂移，
  拒绝并要求重新生成）→ 在**主 worktree** `git apply --index` 应用 patch →
  按 runtime.json 契约跑 lint/build（超时封顶）。
- **验证语义与回滚 = 受控 preimage/patch 逆向，不是"快照回退"一词**（七轮
  修正）：
  - **验证不改主 worktree 源码**：验证命令若可能写源码（lint --fix 一类），
    一律先在 **shadow worktree** 内对 patch 验证；主 worktree 只在 shadow 验证
    通过后才 apply——主 worktree 的 apply 路径本身不产生"验证失败要回滚源码"
    的情形；
  - 主 worktree apply 后的残余失败（构建环境差异等）：用 **`git apply -R` 对
    原 patch 精确逆向**（preimage 已在 apply 前锁定），回滚后按 worktree 既有
    快照约定做完整性核验（**快照失败必须阻断**——沿用 gitworktree 防丢未推送
    工作的约定）；禁止 `git checkout -- .` / `git reset --hard` 一类无差别
    回退（会吞掉用户与 Agent 的并行未提交工作）；
  - 回滚动作与结果（成功/失败/核验输出）记入 proposal 记录。
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
  **dirty-worktree 两条路径**（clean 强制、显式 baselineCommit 快照——断言主
  worktree HEAD/分支/索引零移动）、preimage 漂移拒绝、无锁 Apply 拒绝、
  **shadow 先行验证**（可写源码的验证命令不触碰主 worktree）、回滚 =
  `git apply -R` 精确逆向且不吞并行未提交工作、proposal 串行化、
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
| 1 | **1.0 origin 隔离** + 1.1 控制面封门 + 1.2 运行时收尾 | 2.5-3 天 | 1.0 形态决策（独立 origin vs sandbox iframe）确认后开工 |
| 2 | 2.1 验收测试 Agent + 2.2 数据沙盒 | 5-6 天 | Batch 1；lease/generator 契约（§3.2 第 3/4/5/7 项）已按七轮确认 |
| 3 | 3.1 Change Run + 3.2 Skill Profiles | 4-5 天 | Batch 1；dirty-worktree 与回滚语义（§4.1）确认 |
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
- **预览面与控制台凭据必须 origin 隔离**：被预览应用的文档不可见控制台
  token 存储（localStorage/cookie）；预览面禁止注入携带凭据的脚本（widget
  走认证后的控制台父页面）。
- 不 rebase、不 force push；work/、dist/ 产物不入库；正式文档不含部署细节。
