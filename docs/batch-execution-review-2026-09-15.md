# 后续批次执行方案（Batch 1-4 评审与落地细化，v6）

> 日期：2026-09-15。输入：GPT 四批执行计划 + 仓库既有设计文档 + GPT 六/七/八/
> 九/十五轮评审修正。
> 本方案对计划逐批评审（含与代码现状的核对结论），并给出可直接开工的落地设计。
> 对应关系：Task 1.2 ← `intranet-runtime-plan.md` 收尾清单；Task 2.1 ←
> `acceptance-test-design-plan.md`；Task 2.2 ← `test-data-fixture-sandbox-plan.md`
> 与沙箱 V1 架构稿；Task 4.2 ← `enterprise-evolution-and-scale-roadmap.md` §3.1；
> Task 4.1 ← `mattermost-chatops-status-and-roadmap.md` + P1b closeout。
> 3.1（Change Run）与 3.2（Skill Profiles）在 docs/ 下**没有已提交的设计文档**
>（"质量方案 Phase 2/3"出处缺失），§6/§7 是补充设计，评审通过后应回填正式文档。
>
> **十轮状态**：Batch 1（独立 preview origin）与 Batch 2（lease/generator
> 契约）**批准开工**；Batch 3 **未放行**——v5 的"独立 `.git` clone"仍缺两道
> 隔离闸门（十轮 P0×2）+ 锁语义与验证顺序两处矛盾（P1×2）。v6 按 §4.1 补齐：
> clone 净化（`--no-local`/删 remote/拒 alternates/清 env/净化 diff 提取）、
> Apply 复用 gitworktree **跨进程项目 Git 锁**（现 `acquireProjectLock`
> 未导出，需公开）+ proposal 状态 revision-CAS、验证顺序唯一化、V1 明确拒绝
> dirty、proposal 记录限额与脱敏。待复审。
>
> **执行进度（2026-09-15，commit 0f59a381）**：Batch 1 的 §2.0 + §2.1 已落地
> （除 §2.2 运行时收尾验证任务）：`MULTIGENT_PREVIEW_ORIGIN` /
> `MULTIGENT_CONSOLE_ORIGIN` 显式配置 + fail-closed 路由闸门 + 兑换
> （HttpOnly cookie / no-store / no-referrer / Location 无 token / 无效
> token 不回显）+ CORS allowlist（反射已删）+ widget 与
> `__MG_PREVIEW_TOKEN__` 注入删除 + `previewWritePrincipal`（Bearer-only、
> operator 级、审批人匹配、fail-closed、principal 贯穿评论作者）+ Cap 位
> `preview.view`（旧 token 只读兼容）。status 在泄露审计前要求登录。
> 剩余：§2.2 运行时收尾验证、E2E 探测脚本、部署 runbook 更新。
>
> **执行进度（2026-09-15，Task 2.2 Phase 1 落地，commits 1a0d1653 + 38fd88ef）**：
> 前置 Task 0 与沙盒核心已实现——`internal/fixturesandbox`（契约加载/校验、
> kv_records lease 状态机 + payload+revision CAS、内容寻址 artifact 目录、
> schema 漂移 fail-closed 闸门、task-private db provision、CAS 化 reset/
> reclaim、孤儿目录清扫、容器内 generator）；preview engine 增加
> provision/release 钩子（provision 失败 = 预览启动失败，先于 docker；成功
> 注入 APP_DB_PATH env）；API server 每分钟租期回收。模板
> react_go_fullstack 升 1.2.0（SQLite + 确定性 seed + fixtures.json）。验证：
> make test 36 包全绿；确定性 digest 跨库 5 次一致；并发 reset 恰一成功。
> Phase 1 未含：场景选择 UI（Phase 2）、generator 网络白名单收口（已知妥协）、
> 预冻结运维入口。
>
> **执行进度（2026-09-15，commit 7a29ae3d，batch1-rc1 部署 E2E）**：Batch 1
> 代码级验收通过（GPT 14 轮，round-13 全部 P0/P1 关闭）。部署 E2E 在 VM 完成：
> 同一二进制监听 ：27892（console origin），preview origin :27893 由边缘反代
> 转发至后端并**从 socket 重建 X-Forwarded-\***（`httputil.ReverseProxy`
> `Rewrite`+`SetXForwarded`，客户端伪造 XFP 被抹除——伪造 `XFP: https` 的请求
> 通过 gate（若后端信任伪造值会 404），手工伪造 cookie 被 token 校验拒绝
> （401），证明覆盖行为真实生效）。服务端验证结果：console origin
> `/preview/` 302 → preview origin；preview origin 无 token 401；外来 Host
> 404（响应含配置 origin 提示）；CORS 预检 allowlist 命中 → 204+ACAO、
> preview origin → 无 ACAO；真实 share token 兑换 → 302 + Location 无 token
> + `no-store`/`no-referrer` + cookie（`Path=/preview/{task}/; Max-Age=43200;
> HttpOnly; SameSite=Lax`）；兑换 cookie 访问干净 URL → gate 放行、token 校验
> 通过、到达代理后 503 "not running"（预览实例未启动，预期行为）。浏览器侧
> HttpOnly/localStorage 探测未执行（需启动真实预览容器）；HttpOnly 属性已在
> Set-Cookie 字节级确证，localStorage 隔离由 origin 分离保证。遗留：预览
> surface 依赖 Docker 沙箱启动后的完整渲染验证。
>
> **九轮定案（仍然有效）**：Batch 1 采用独立 preview origin（sandbox iframe
> 仅降级开关）；preview origin 来自显式部署配置；CORS allowlist **默认不含
> preview origin**；写端点 Bearer-only；URL token 兑换 HttpOnly cookie（含
> no-store/no-referrer/日志脱敏/SameSite 复核）。九轮同时确认 live 要求登录
> 主体、dirty baseline 需受控快照。

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

### 2.0 Task 1.0 预览 origin 隔离（P0，七轮提出、八轮定案：独立 origin 单选）

**威胁**：被预览应用与控制台同 origin（`/preview/{task}/...` 反代），应用脚本
可读 `localStorage["multigent-token"]` 拿到真实用户 Bearer，直接调控制台
API——任何端点级校验都被真实凭据绕过。

**已定决策：独立 preview origin（唯一长期方案；sandbox iframe 只是短期降级
保护，不列入完成定义）**：

1. **preview origin 来自显式部署配置**，不由请求 `Host` 推导（Host 可被伪造
   /配错）。新增部署配置项（如 `MULTIGENT_PREVIEW_ORIGIN`），未配置时预览功能
   fail-closed（任务面板明示"未配置预览 origin"），绝不静默退回同源代理。
2. **控制面 CORS 从反射改为 allowlist**：现状 `withCORS` 把任意请求 Origin
   原样反射进 `Access-Control-Allow-Origin`（server.go:930-933）——独立 origin
   后这会把控制台 API 跨域开放给 preview origin 之外的所有站点。改为：
   **allowlist = {控制台 origin（部署配置）}——默认不含 preview origin**
   （九轮细化）：preview 没有调用控制台控制面 API 的必要，除非未来存在明确、
   只读且单独审计的跨域接口，才逐条加入并记录审计理由。非名单 Origin 一律不回
   CORS 头（保持浏览器同源默认拒绝）。反射逻辑删除，
   `Access-Control-Request-Headers` 同步收敛为固定集合。
3. **URL token 兑换 HttpOnly cookie（八轮第 3 点、九轮细化响应约束）**：
   即使删除 `__MG_PREVIEW_TOKEN__`，分享 URL 的 `?pvt=` 仍可被项目脚本从
   `location.search` 读走外传。兑换流程：
   `GET <preview-origin>/preview/{task}/?pvt=<token>` → 服务端校验 token →
   `Set-Cookie: mg_pvt_<task>=<token>; HttpOnly; Secure; SameSite=Lax;
   Path=/preview/{task}/` → **302 到去掉 pvt 的干净 URL**。此后页面与 JS
   可见 URL 均无 token；JS 永远读不到凭据（HttpOnly）。九轮细化（写入验收）：
   - 兑换响应追加 `Cache-Control: no-store`、`Referrer-Policy: no-referrer`
     （token 不进缓存、不经 Referer 泄漏）；
   - 服务端日志与错误输出**一律脱敏 `pvt`**（含访问日志、审计事件、错误消息
     回显路径——redact 规则与 git 输出脱敏同级）；
   - `SameSite=Lax` 的适用前提：preview origin 与 console 为**同一 schemeful
     site**（同 scheme + 注册域）；部署若跨 site（如独立子域不满足 schemeful
     site 定义，或 iframe 嵌入场景），须重新评估 cookie 发送行为并改用
     `SameSite=None; Secure` 或改走兑换后 302 的顶级导航（当前分享形态即顶级
     导航，Lax 可用；嵌入 iframe 属于部署形态变更，实施时按实际形态复核）。
4. **Copilot 控制 UI 迁出被预览文档**：chat/feedback/stop/live 的 UI 放在
   **认证后的控制台父页面**（AssistantWidget 体系，Bearer 经控制台 origin）；
   删除 preview 代理对 `/_multigent_preview/feedback.js` 与
   `__MG_PREVIEW_TOKEN__` 的注入（preview_handlers.go:844,1024-1032）。
   **共享预览永远没有 Copilot widget**——不是过渡形态，是终态约束。
5. **sandbox iframe 的定位**：仅当部署方**暂时**无法配置独立 origin（如内网
   反代未就绪）时的显式降级开关（单独配置项、启动日志 WARN、任务面板明示
   降级与能力损失）；不在完成定义内，不作为等价交付。

**完成定义**：

- 预览仅从配置的 preview origin 可达；控制台 origin 下访问
  `/preview/{task}/...` 返回 404/重定向到 preview origin；
- 分享 token 仅在 preview origin 生效；URL 中不残留 token（兑换 + 302）；
- 预览文档内执行脚本无法读取任何控制台凭据（E2E：注入探测脚本，断言
  `localStorage.getItem('multigent-token')` 不可达、无 token 于
  location/cookie 可读面）；
- CORS allowlist 生效：名单外 Origin 的跨域预检被拒；
- 共享预览文档中无注入脚本、无 Copilot UI。

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

- **Bearer-only，无过渡双主体**（八轮定案）：v3 的"pvt + Bearer 并存时按
  登录态放行"废除——同源阶段被预览应用可窃取 Bearer，允许窃取者经 operator
  校验等于把上游 P0 结果合法化。迁移顺序强制串行：**先** §2.0 origin 隔离 +
  移除注入 widget，**后**写端点启用 Bearer-only；上线路径上不存在
  "同源 + 可写"窗口。分享 token 对写端点一律 403（无例外分支）；
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
- **Bearer-only**：任何"pvt + Bearer"组合按 Bearer-only 规则判定——仅 pvt
  永远 403，不存在放行分支；
- 回归：带 preview token 的代理/静态资源仍 200；旧格式 token
  （无 Cap）读 200、写 403；409 执行锁与限流行为不变；
- principal 贯穿：feedback/chat 的评论作者 = 登录名（不再出现 `"user"`）；
- `preview/status` 审计结论（已记录，2026-09-15）：响应体为
  `{taskId, busy, agent, startedAt}`——`agent` 仅是 Agent Worker 名称字面量
  （如 `pm`，preview_handlers.go `session.Agent = agentName`），不含路径、
  命令、Agent 会话输出。**无泄露**；同时十三轮 P0 修复后该端点已要求项目
  读级 membership，"放宽给分享 token"无场景价值 → **维持登录制 +
  项目 membership**，不再放宽；
- **origin 隔离 E2E**（§2.0）：兑换 302 后 URL 无 token、cookie HttpOnly、
  预览文档内探测脚本读不到控制台 token、CORS 名单外 Origin 预检被拒、
  控制台 origin 下 `/preview/` 不可达。**部署前置**：反代必须覆盖外部
  X-Forwarded-Proto，后端不可公网直连（十三轮 P0 后 scheme 校验在 gate，
  XFP 不符 = 预览整体 404）。

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

- **提议持久化（含限额与脱敏，十轮补充）**：kv_records（
  `preview_change_proposals`，key [project, taskID, proposalID]）：actor
  （显式 principal，同 Task 1.1）、原始请求、补丁、Diff、状态、验证结果、
  时间戳。**同一任务同时至多一个活跃 proposal**（V1 不做并行）。**记录内容
  约束**：
  - 原始请求/patch/diff 各设大小上限（建议请求 16KB、patch/diff 256KB，超限
    截断并标记 truncated，完整内容不落 kv_records）；
  - **敏感信息脱敏后才入库**：用户粘贴的内容可能含 token/凭据/内网地址——
    入库前跑与 git 输出 redact 同级的扫描（key=value 模式、常见 token 前缀、
    `Authorization`/`Bearer` 头），命中即打码；kv_records 是持久层，不能把
    凭据原样永久写入；
  - 状态字段迁移走 revision-CAS（见 Apply 锁语义）。
- **隔离产出 = 净化后的独立 clone，不是普通 `git clone`**（九轮推翻 linked
  worktree；十轮补两道真实隔离闸门）：`git worktree add` 的 linked worktree
  共享 common dir（九轮）；而**普通本地 `git clone` 也不安全**（十轮实测）——
  它硬链接源仓库对象并保留指向源仓库的 `origin`，Agent 在 clone 内
  `git push origin ...` 就能在源仓库创建 ref；"无 remote 凭据"不等于
  "无可写 remote"。v6 净化契约：
  1. **clone 方式**：`git clone --no-local <src> <dst>`（禁用硬链接/对象共享；
     备选 bundle 导出再解包），clone 参数里拒绝任何 alternates 引用
     （`--shared`/objects/info/alternates 出现即拒）；
  2. **交给 Agent 前删除全部 remote**（`git remote remove` 逐个删，断言
     `git remote` 为空——从根上消除 push 到源仓库的通道）；
  3. **运行环境净化**：Agent 执行环境的 env 清空 `GIT_DIR`、`GIT_COMMON_DIR`、
     `GIT_INDEX_FILE`、`GIT_OBJECT_DIRECTORY`、`GIT_ALTERNATE_OBJECT_DIRECTORIES`
     等 Git 指针变量（防经环境变量重定向到父仓库）；
  4. **平台从不可信 clone 提取 diff 必须净化 Git 配置**：Agent 可写 clone 的
     `.git/config`，配置里的 `diff.external`/`textconv` 驱动会在宿主执行
     **任意程序**。平台提取 diff 时使用 `git -c core.fsmonitor= -c core.hooksPath=/dev/null
     diff --no-ext-diff --no-textconv ...`（禁外部 diff 驱动与 textconv、禁
     hooks；且 `HOME`/`GIT_CONFIG_*` 指向空配置目录），绝不用裸 `git diff`；
  5. 私有临时 ref 保活 baseline（同 v5）；Agent 只拿 clone；平台提取 patch 后
     删除 ref 与 clone（幂等）；
  6. 验收测试新增：clone 内 `git remote` 为空且 `git push` 无处可去；源仓库
     对象无硬链接共享（对 clone 内新建对象 `stat` nlink 校验或用
     `--no-local` 后对象计数差断言）；clone config 被写入恶意
     `diff.external` 后，平台提取 diff 不执行外部程序（用探针脚本断言未触发）。
- **proposal 基线：V1 只接受 clean 主 worktree**（七轮提出 dirty 建模；十轮
  定案拒绝 dirty，见"验证顺序唯一化"一节）：clone 只含 `baseCommit`，看不到
  主 worktree 的未提交变更。**V1 契约：`git status --porcelain` 非空 → 明确
  报"请先提交或暂存当前变更"，不静默继续，只测试拒绝路径**。以下
  alternate-index 协议**仅作为未来 dirty 模式的独立设计评审输入**（V1 不实施、
  不测试）：
  1. 临时快照**不得触碰真实 index**——`git add -A` 会直接改写真实 index
     （v3 草案的语义错误），且 `write-tree` 产出的是 tree 不是 commit，
     `git worktree add` 无法稳定使用。正确实现用**临时 `GIT_INDEX_FILE`**：
     a. 以 alternate index 执行 `GIT_INDEX_FILE=<tmp> git read-tree <base>`；
     b. `GIT_INDEX_FILE=<tmp> git add -A`（只写 alternate index，真实 index
        不动）；
     c. `GIT_INDEX_FILE=<tmp> git write-tree` 得到 tree；
     d. `git commit-tree <tree> -p <base> -m "baseline"` 创建临时 commit；
     e. **临时 commit 立即由私有 ref 保活**
        （`refs/multigent/proposals/<proposalID>/baseline`——九轮修正：裸挂
        commit 会被 `git gc` 回收，clone 完成前必须 ref 可达）；
     f. 独立 clone 从该 ref 创建（见"隔离产出"契约第 3-5 点）；
        patch 基线、preimage 校验、Diff 展示全部以 `baselineCommit` 为准；
     g. proposal 终态后删除私有 ref 与 clone（对象由 gc 自然回收）；全程真实
        HEAD、分支、index 零移动（测试断言）。
- **Apply 锁语义 = gitworktree 跨进程项目 Git 锁 + proposal revision-CAS**
  （十轮推翻 v5 的锁设计）：v5 写的"申请写锁（isTaskAtHumanReviewStep +
  previewSessions 互斥）"不成立——`isTaskAtHumanReviewStep` 只是一次只读查询
  （preview_handlers.go:1072），`previewSessions` 只是单进程、按 task 的聊天
  会话 map（preview_handlers.go:517），两者都不能与既有审核链路的
  `git add/commit/push`（workflow_handlers.go:899，受 `acquireProjectLock`
  保护）串行。v6 契约：
  1. **复用 gitworktree 的跨进程项目锁**：公开 `acquireProjectLock`
     （worktree.go:88 现为小写未导出，改为导出的 `AcquireProjectLock` 或提供
     包装方法），覆盖 Change Run 的**全部** Git 触及区间——临时 ref 创建、
     preimage 检查、apply、postimage 记录、回滚、与审核提交（审核链路已在锁内，
     两者天然互斥串行）；
  2. **proposal 状态迁移 revision-CAS**：`AwaitingApproval → Applying` 用
     kv_records 的 payload+revision CAS（transition-claim 同款原语）——防双击
     与跨控制台并发确认；CAS 失败即"已被其他操作者接管"；
  3. **拿锁后复核**：进入锁内第一步重查 human-review 状态与 proposal 当前态
     （CAS 只保证状态机独占，不替代锁内业务复核）；不满足即释放锁并返回冲突。
  4. 验收测试：并发两个 Apply 请求（跨 goroutine 模拟跨控制台）恰一个成功；
     Apply 与审核 approve 并发时严格串行（锁序无死锁）；双击确认只产生一次
     状态迁移。
- **验证顺序唯一化（十轮 P1）**：v4 曾一处写"主 worktree apply 后跑
  lint/build"、另一处要求写源码的验证先在 clone——自相矛盾。V1 定为**唯一
  顺序**：
  ```text
  clone 内 apply → clone 内全部验证（lint/build/测试，含可写源码的）
    → 拿项目锁 + 状态复核（CAS+锁内）
    → 主 worktree apply（纯 apply，无任何验证步骤）
    → 记录 postimage → 完成
  ```
  主工作区**只允许纯 apply**——任何失败都发生在主工作区之外，永远不落入
  主工作区回滚路径；主工作区的 `git apply -R` 仅用于人工触发的撤销（见下）。
- **回滚 = 受控逆向 + postimage 校验**（七轮提出、八轮 index 语义、九轮
  postimage、十轮维持）：
  - **回滚前校验 postimage**：`git apply -R` 假设文件仍处于 apply 后状态——
    若人工/其他流程在 apply 与回滚之间改过文件，反向 patch 会把**别人的修改
    一起覆盖**。回滚前逐 touched path 校验当前内容仍等于 **postimage**
    （apply 时在锁内记录的文件哈希）；不等于 → 该路径拒绝自动回滚，proposal
    标记"需人工介入"，不静默覆盖；
  - clean 形态下 `git apply` 不经 index、`git apply -R` 对称还原工作区文件，
    回滚后按 worktree 既有快照约定做完整性核验（**快照失败必须阻断**）；
  - 禁止 `git checkout -- .` / `git reset --hard` 一类无差别回退（会吞掉
    用户与 Agent 的并行未提交工作）；回滚动作与结果记入 proposal 记录。
- **V1 明确拒绝 dirty baseline（十轮 P1 二选一）**：v4/v5 同时写"只接受
  clean"与"dirty 用 alternate-index 建基线"，两头都要。V1 定案：**只接受
  clean 主 worktree**（`git status --porcelain` 非空 → 明确报"请先提交或
  暂存当前变更"，不静默继续）；**只测试拒绝路径**；alternate-index +
  `commit-tree` 的 dirty 基线协议（前述 a-e 步骤保留在文档中作为未来设计
  输入）留到独立设计评审后再启用。
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
  Run 管"发起后怎么落地"（隔离 clone → 审 → apply → 验证），两层闸门串联。
- **UI**：预览抽屉展示隔离 clone 产出的 Diff + 确认/拒绝；流式进度复用现有
  previewSessions SSE 通道。
- **测试**：黑名单各形态（新增/删除/重命名/symlink/二进制/路径穿越规范化）、
  **clone 净化**（`git remote` 为空且 push 无处可去、无 alternates/硬链接
  共享、恶意 `diff.external` 下平台提取 diff 不执行外部程序、env Git 指针
  变量清空）、**隔离边界**（Agent 视角在 clone 内执行 update-ref/worktree
  管理，断言主仓库引用与元数据零变化）、**dirty 拒绝**（porcelain 非空 →
  明确报错、零写入）、**锁与状态机**（并发两 Apply 恰一成功、Apply 与审核
  approve 严格串行、双击只迁移一次、锁内复核失败即释放）、preimage 漂移拒绝、
  **clone 先行验证**（可写源码的验证命令不触碰主 worktree，主工作区只有纯
  apply）、**回滚 postimage 校验**（apply 后文件被人工改动 → 拒绝自动回滚
  并标记人工介入）、proposal 记录脱敏与限额、proposal 串行化、1.1 权限矩阵
  在入口同样生效、apply 后收编仍只由审核链路触发。

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
| 1 | **1.0 独立 preview origin**（CORS allowlist 默认不含 preview origin + cookie 兑换含 no-store/no-referrer/日志脱敏/SameSite 复核）+ 1.1 控制面封门 + 1.2 运行时收尾 | 3 天 | **十轮维持批准开工** |
| 2 | 2.1 验收测试 Agent + 2.2 数据沙盒 | 5-6 天 | **十轮维持批准，与 Batch 1 并行开工** |
| 3 | 3.1 Change Run + 3.2 Skill Profiles | 4-5 天 | **十轮未放行，待复审**：§4.1 已按两项 P0（clone 净化、跨进程锁 + CAS）与三项 P1（验证顺序唯一化、V1 拒绝 dirty、记录脱敏限额）改写完毕，复审通过后开工 |
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
- **预览面与控制台凭据必须 origin 隔离（八轮定案）**：preview origin 来自
  显式部署配置（不由 Host 推导，未配置 fail-closed）；CORS 只允许部署配置
  名单（禁止反射任意 Origin）；分享 token 经 URL 兑换为 preview-origin
  HttpOnly cookie 后从 URL 移除；预览面永不注入携带凭据的脚本、永不提供
  Copilot widget；写端点 Bearer-only，不存在 token+登录双主体过渡态。
- 不 rebase、不 force push；work/、dist/ 产物不入库；正式文档不含部署细节。

---

## 执行进度（2026-09-15，commit 4daec299，Batch 3 第一层收口）

审核提交路径（`commitAndPushReviewChanges`）已全部收编：跨进程项目 Git 锁
（`AcquireProjectLock`，与 worktree manager 同锁）覆盖 status→add→commit→push
全窗口；除 push 外全部 git 调用以新增导出的 `SanitizedGitEnv` 运行（GIT_* 重
定向/HOME/XDG/凭证面剥除 + GIT_CONFIG_NOSYSTEM=1 强制），且全部调用前置
`-c core.fsmonitor= -c core.hooksPath=` 配置中和——Agent 可写 config 不再可能
在宿主执行任意程序。新增导出 `ProjectRootForWorktree`。测试：毒化 config 探
针不执行且 commit 落库、外部持锁期间审核提交被排除、并发提交串行无丢失、
SanitizedGitEnv 再注入拒绝。make test 全绿。

Task 3.1 完整 Change Run（proposal 状态机 + 隔离 clone 接入 agent 面 +
SanitizedDiffArgs diff 提取 + 黑名单 + 回滚校验）仍未开工，待复审放行。
