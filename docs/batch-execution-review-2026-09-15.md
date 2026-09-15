# 后续批次执行方案（Batch 1-4 评审与落地细化）

> 日期：2026-09-15。输入：GPT 四批执行计划 + 仓库既有设计文档。
> 本方案对计划逐批评审（含与代码现状的核对结论），并给出可直接开工的落地设计。
> 对应关系：Task 1.2 ← `intranet-runtime-plan.md` 收尾清单；Task 2.1 ←
> `acceptance-test-design-plan.md`；Task 2.2 ← `test-data-fixture-sandbox-plan.md`
> 与沙箱 V1 架构稿；Task 4.2 ← `enterprise-evolution-and-scale-roadmap.md` §3.1；
> Task 4.1 ← `mattermost-chatops-status-and-roadmap.md` + P1b closeout。
> 3.1（Change Run）与 3.2（Skill Profiles）在 docs/ 下**没有已提交的设计文档**
>（"质量方案 Phase 2/3"出处缺失），§6/§7 是补充设计，评审通过后应回填正式文档。

---

## 1. 总体评审结论

方向与顺序正确：Batch 1 先封安全洞是对的——**已核实代码确认该洞真实存在**：
`POST /api/v1/projects/{name}/tasks/{taskId}/preview/chat` 挂在 `publicMux`
（server.go:816），而 `previewTokenClaims` 只有 `t/p/exp` 三个字段
（preview_token.go:25-29），没有任何 scope/cap 概念；`previewRequestAuthorized`
对任何通过 MAC 与过期校验的 preview token 直接放行（preview_token.go:105-108）。
即：拿到分享链接的人可以让 Copilot 改任务工作区代码。这是越权写，P0 定级准确。

需要修正与补充的六点：

1. **在途依赖缺失**：计划没有提 GPT 对竞态面整改的五轮复审。整改已交付
   （8fbdcc47/51f4d034 已推送），不阻塞 Batch 1，但「竞态面已闭合」结论在复审
   确认前维持撤回；若五轮再出 P0，修复优先级高于一切批次。
2. **Task 2.2 改动位置有误**：计划写 `internal/preview/engine.go`、
   `internal/gitworktree/`——后者必须去掉。数据沙盒的资源所有者是 **preview
   lease**（与 30 分钟租期 + 每分钟 reaper 收敛），`gitworktree` 保持纯代码基线
   与不可变提交管理，不挂数据职责（AGENTS.md 六大生命周期解耦红线）。
3. **Task 2.1 遗漏双层体系约束**：模板改完必须**重新实例化**才对新任务可选；
   且按 `acceptance-test-design-plan.md`，目标是 greenfield 的 **vNext 新版本**，
   不得混改 `unified-delivery-pipeline`、不得影响存量实例。
4. **Task 1.1 的"严格匹配 ActorID"需要精化**：审批人匹配只应作用于"让 Copilot
   写工作区"这个动作，且要处理 assignee 为 agent 类型的情形与查询失败的
   fail-closed（详见 §2.3）。
5. **工期偏乐观**：Batch 2 实际 5-6 天（2.2 有两个前置：模板 DB 可重定位、lease
   持久化）；Batch 3 的 3.1 是完整特性（提议持久化 + Diff + UI + 验证管线 +
   回滚），2-3 天不够，Batch 3 约 4-5 天。
6. **缺"不做什么"**：沙箱 V1 不做特性迁移 provision（架构稿 §6.2）、不做 DSL
   造数、不做候选晋级发布（均留 Phase 2/3）；Change Run V1 不做并行多提议
   （同一任务同时至多一个活跃 proposal）。

---

## 2. Batch 1：安全止血（1.5 天）

### 2.1 Task 1.1 预览 Copilot 权限封门

**改动位置**：`internal/api/preview_token.go`、`internal/api/preview_handlers.go`
（+ `internal/api/server.go` 若路由层调整）。

**设计**：

1. **token 加能力位（向后兼容 fail-closed）**：`previewTokenClaims` 增加
   `Cap string`（`preview.view` = 只读浏览）。旧 token 无该字段 → 解码后视为
   空 = 无写能力：分享链接只读语义对存量 token 立即生效，无需失效重发。
   `signPreviewToken` 一律 mint `preview.view`。
2. **preview/chat 拒绝分享 token**：handler 在 `previewRequestAuthorized` 之后
   增加一层 `requirePreviewChatUser`：请求若仅凭 preview token 通过（无控制台
   登录态）→ 401/403。登录判定复用 `ctxUserKey`（withTokenAuth 语义），不新开
   认证路径。
3. **RBAC + 审批人匹配（fail-closed）**：
   - 登录用户须通过 `checkProjectAccess`（项目写权限）；
   - 任务停在 human_review 步骤且该步骤 assignee 为 human：要求
     `currentUser.Username == step.ActorID`，否则 403；
   - assignee 为 agent（或无 assignee）：退回项目 RBAC 判定（与现有审批端点
     口径一致）；
   - 工作流查询失败 → 拒绝（与 `isTaskAtHumanReviewStep` 的 fail-closed 约定
     一致）；
   - 任务 `InProgress` 且非 human_review：维持现有 409（执行锁）不变；
     非 `InProgress` 的只读快照：维持 `previewInstanceReadOnly` 409 不变。
4. **纯浏览面不受影响**：GET 预览代理、静态资源仍可用 preview token（这是
   分享链接的存在意义）。

**测试**（单元 + httptest 集成）：

- 四态矩阵：匿名无 token 401；仅 preview token 403；登录但非责任人/无项目权限
  403；责任人登录 200。
- 回归：带 preview token 的 GET 预览资源仍 200；旧格式 token（无 Cap）对写端点
  403、对读端点 200；409 限流与执行锁行为不变。

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

### 3.2 Task 2.2 测试数据沙盒 V1（约 3-3.5 天）

**前置 Task 0（必须，约 0.5 天）——模板 DB 可重定位**：旗舰模板
`react_go_fullstack` 当前**没有数据库**（纯 JSON API + 内嵌前端）。给它加：
环境变量可重定位的 SQLite 路径 + `db:seed:baseline` / `db:seed:scenario`
脚本 + `server/data/` gitignore。否则 V1 上线零个真实可用项目。

**核心实现**（资源所有者 = preview engine，**不动 gitworktree**）：

1. **契约加载器**：`.multigent/fixtures.json` 校验（engine 仅 sqlite；
   `relativePath` 拒绝绝对路径/`..`/symlink 跳出；argv 非空、超时封顶、glob
   标准化排序后摘要）。
2. **artifact catalog + lease 持久化（kv_records）**：`PreviewInstance` 目前是
   Engine 内存 map，重启靠容器 label `Reconcile` 重建——**lease 必须落库**
   （key：workspace/project/task/leaseID，值含 artifactDigest、scenario、
   baseCommit、state、dataDirectory、expiresAt），dataDirectory 由不可预测
   lease ID 派生、限定在平台 sandbox root 内。artifact 存本地受控目录
   （MULTIGENT_DATA_DIR 下）+ 元数据入 kv_records；对象存储留适配器接口不做实现。
3. **provision**：冻结 baseCommit 时解析 fixtureVersion → artifactDigest；
   校验完整性/大小上限/模板指纹后复制为 task-private.db；同目录管理 WAL/SHM；
   注入环境变量指向私有库；执行 scenario（固定 seed、UTC、固定 locale）。
   **QA-only lease**：`previewLeaseId` 允许为空，否则 QA 无 preview 时无处挂靠。
4. **引擎集成点**：`engine.go:327/334` 的 `npm run seed || true`（正是沙箱方案
   点名批判的吞错行为）替换为幂等 provision 调用；失败 fail-closed 不启动预览。
5. **reset/回收**：reset = 停写 → 删目录 → 从冻结 digest 重建（每 lease 一把
   锁防并发 reset）；reaper 沿用 30 分钟租期 + 每分钟扫描，**先停容器再删
   目录**；重启后按持久化 lease 归账孤儿目录，幂等。
6. **验收**：方案 §8 五项（同基线 5 次确定性、并发隔离、reset 复原、生命周期
   物理清理、生产 CI 交付物扫描无沙箱数据）。"100ms 重置"按架构稿口径记为
   条件性 p95 SLO，不写进完成定义。

**V1 明确不做**：特性分支迁移 provision、DSL 造数、候选晋级发布、前端悬浮
抽屉（Phase 2）。

---

## 4. Batch 3：受控变更与 Skill 治理（4-5 天）

### 4.1 Task 3.1 受控 Change Run（约 3 天，含 UI）

> 无既有设计文档，以下为补充设计，评审通过后回填 docs。

**状态机**：

```text
Proposed → AwaitingApproval → Applying(验证) → Applied
                    │                └→ VerificationFailed(回滚)
                    └→ Rejected
```

- **提议持久化**：kv_records（`preview_change_proposals`，key
  [project, taskID, proposalID]）：actor、原始请求、agent 产出的补丁、Diff、
  状态、验证结果、时间戳。**同一任务同时至多一个活跃 proposal**（V1 不做并行）。
- **流程**：Copilot 收到修改请求 → agent 在隔离产出模式下只生成补丁、不直接
  写工作区 → 平台渲染 Diff 展示给用户 → 确认后 Apply：申请写锁
  （`isTaskAtHumanReviewStep` fail-closed 判定 + `previewSessions` 互斥）→
  应用补丁 → 按 runtime.json 契约跑 lint/build（超时封顶）→ 成功收编为
  checkpoint；失败回滚（worktree 快照回退，**快照失败必须阻断**——沿用
  gitworktree 防丢未推送工作的既有约定）。
- **高风险路径黑名单（确定性校验，不走模型判断）**：`.gitlab-ci.yml`、
  `migrations/`、`Dockerfile`、`.multigent/`、`deploy/` 默认拒绝自动改，
  proposal 直接 rejected；项目契约可收紧、不可放宽。
- **与 Task 1.1 的关系**：1.1 管"谁能发起"（权限），Change Run 管"发起后怎么
  落地"（受控应用），两层闸门串联。
- **UI**：预览抽屉 Diff 视图 + 确认/拒绝；流式进度复用现有 previewSessions
  SSE 通道。
- **测试**：黑名单拒绝、无锁 Apply 拒绝、验证失败回滚完整性、proposal 串行化、
  1.1 权限矩阵在 Change Run 入口同样生效。

### 4.2 Task 3.2 Skill Profiles 受控挂载（约 1.5 天）

- **服务端 allowlist**：skill 元数据入 kv_records（内容 SHA256 digest、适用
  技术栈标签、启停状态）；仅 admin 可写。挂载时校验 digest——不匹配即拒绝。
- **Intent Profile**：`ui-polish`、`a11y-remediation` 等预设 profile 映射到
  allowlist 内 skill 集合；预览抽屉勾选 → chat 请求携带 profiles → 组 Prompt
  时注入对应 skill 内容。
- **边界**：profile 只能引用 allowlist 条目，禁止前端内联注入任意 Prompt 文本
  （缩小提示注入面）；未勾选 profile 的请求行为与现状一致。

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
| 1 | 1.1 权限封门 + 1.2 运行时收尾 | 1.5 天 | 无（立即开工） |
| 2 | 2.1 验收测试 Agent + 2.2 数据沙盒 | 5-6 天 | Batch 1（1.1 的 403 语义是 2.2 QA 链路的前提） |
| 3 | 3.1 Change Run + 3.2 Skill Profiles | 4-5 天 | Batch 1（3.1 依赖 1.1 的登录态判定） |
| 4 | 双人终验 / Brownfield / 自愈 | 验收类，穿插 | 对应批次完成 |

在途：GPT 五轮复审竞态面整改——通过则恢复「竞态面已闭合」结论并更新文档；
出 P0 则修复优先于一切批次。

## 7. 每批次完成定义（红线对照）

- `make test` + `make build` 全绿；行为级验证按目标环境私有 runbook。
- 新端点一律注册在带 `withTokenAuth` 的 mux + `checkProjectAccess`；确需暴露给
  预览 iframe 的，handler 内校验预览签名 token，写端点加频率限制。
- 凭据不落盘（remote URL 纯净、输出 redact）；并发写保护用
  `isTaskAtHumanReviewStep` fail-closed；任务派生基于不可变 baseCommit。
- 不 rebase、不 force push；work/、dist/ 产物不入库；正式文档不含部署细节。
