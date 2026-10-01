# 总体工作计划（2026-10-01，待用户审批）

> 状态：**待审批**。用户指令原文：先清一里的 1+2+4，3 帮我绑定远端 git；二中先干缺陷问题，
> 再把部署中心二三期结束，之后是三中的出个优化方案。内置浏览器测试。
> 审批通过后按序执行；每个 Phase 结束有验收门，未过不进下一 Phase。

---

## Phase 0 · 一类清账（小尾巴，~1.5h）

| # | 任务 | 做法 | 验收 |
|---|---|---|---|
| 0.1 | MM 卡归档 403 修复 | bot（bot-lina/bot-mira）在 MM System Console 补 `edit_own_posts` 权限（或改用 System Bot token）；代码侧不动 | 内置浏览器：发起部署→批准→旧卡被原地 patch 成已归档态（按钮消失），日志无 403 |
| 0.2 | alex 本人批准实弹 | 用内置浏览器以 alex 身份（用户已登录）点最新卡的批准按钮 | 平台日志 `deploy_approver_allowed` 走指定人分支（非 admin 兜底），pipeline 触发 |
| 0.3 | batch-test1 绑定远端 git | GitLab 上确认/创建 `root/batch-test1` 仓 → `PUT /projects/batch-test1`（repo=gao 路径或 root 路径）→ `POST /api/v1/projects/batch-test1/remote/verify {connectionId: conn-2bc12438…}` | verified_remote_bindings 出现 batch-test1 行；每分钟的 "branches unavailable" 噪音日志消失；部署页分支下拉有数据 |
| 0.4 | 分支推送合并 | ① multigent-integration：`feature/deploy-center-phase1`（领先 32+3 commits，含 B3 全链）推 origin 并合入 dev；② multigent 主仓 dev 的 6 个 docs commit 推 origin | 远端可见；`git log origin/dev` 含 9467e6fa；VM 二进制与 dev HEAD 同源 |

不修（记录在案）：ccr-service 保持 stop；flaky 测试 TestDesignProxyAdmits…（预存）。

## Phase 1 · 缺陷清偿（overnight 9 缺陷路线图收尾，~1 天）

前置事实：`fix/overnight-integration`（fa92b65c，10 commits）已修 7 条且**未进 dev**（含 4 组 merge commits + fa92b65c 补丁）；S2 batched 实跑已在 ias-auth-center/fp-dedupe 验证过该分支的价值。

| # | 任务 | 验收 |
|---|---|---|
| 1.1 | overnight 集成分支合入 dev | merge 零冲突或逐条裁决；`make test` 绿；推 origin/dev |
| 1.2 | 缺陷 #9 残项：membership 不热加载（Mira 类全局 worker 无新项目调度循环） | 行级根因落定（`runtimeSchedulerTargets` 每次实时读 `ListProjects`+`ListProjectMemberships`，怀疑缺口在调度循环仅对"已注册 target"跑——新增项目/成员后无循环）；修复=循环内每 tick 重扫或 membership 变更事件触发重扫；单测覆盖"运行中新增项目→下一 tick 被调度" |
| 1.3 | 缺陷 #2 资产文档死锁链验证 | 在 VM 用一次性项目实弹：任务绑资料库文档+常驻 scheduler → 文档可达（此前永锁）。若 fa92b65c 的 rebind 修复已覆盖则只留证据 |
| 1.4 | 残余两缺陷定性 | 缺陷 4（join 裸仓清理）与缺陷 7（touched_paths）的修复在 1.1 合入后用 batched 实弹复跑验证（复用 batch-test1 或建一次性项目） |

红线：不碰 ias-auth-center 存量数据；VM 部署前备份二进制（沿用 prev-shaid 惯例）。

## Phase 2 · 部署中心二期+三期a（~3-4 天）

范围=proposal 定稿的 M4 + M5 确定性版（三期b 浏览器校验仍另行评审，不做）。

### 2A 二期：部署 AI 抽屉（M4，1.5-2d）
- 右下角抽屉，部署页专属；**纯对话无工具**（与预览 Copilot 的本质区别：不碰代码/写锁）。
- 对话中结构化抽取"发现项"（现象/预期/复现路径），列表可视化。
- 勾选发现项→`POST tasks`（description 附会话摘要+发现项清单，`created_by: deploy-ai`）。
- 复用预览抽屉的 UI 骨架与 OD 的短对话模型（glm-5.3-flash，16k 硬封顶沿用红线）。

### 2B 三期a：部署后自动校验 v1（M5 确定性版，2d）
- 部署成功后：diff commit span → agent 生成冒烟校验规格（method/path/期望状态码/断言 JSON）→ **平台确定性执行**（纯 HTTP，无浏览器）→ 报告。
- 规格随部署单固化（Q7 裁定：存 deploy_request，可重放=回归资产）。
- 失败 → attention 信号 + 自动建 issue 任务。
- 安全：校验目标 host 复用 `deployedAppHealthURL` 的 host 红线（MULTIGENT_DEPLOY_HOST 配置制）。

**验收（内置浏览器为主）**：
1. 部署页四状态切换正常（实弹复走）；抽屉打开→描述现象→发现项入列→转任务→任务出现在项目列表；
2. 部署成功后校验规格生成→执行→报告展示；人为改坏一个端点→校验 fail→attention+自动任务；
3. `make test`/`make web` 全绿；单测覆盖规格 schema 校验与 HTTP 执行器。

## Phase 3 · 大需求拆分流程优化方案（先方案后实施，方案 0.5d）

衔接既有定稿 `docs/large-requirement-delivery-module.md`（S1-S2 机制已实施并有 ias-auth-center/fp-dedupe 实跑），**不重起炉灶**，增量补三块：

1. **拆分向导（阶段1）**：CreateTaskDialog 升级为"大需求向导"——粘贴需求 → 走 scale_gate 产 batch_plan → 人审编辑任务树 → 批量建任务+依赖自动串 baseCommit/completionCommit。把 S2 时代"人工衔接 batch_plan→画布 branch"的已知摩擦（DM4 代价）产品化。
2. **验收物前置生成（阶段2）**：拆分时按任务类型预生成 qa 豁免单骨架/ci_ready 检查项/验收 runbook，让 batched 派发即带验收合同（overnight 9 缺陷中"口径不清"类的治本）。
3. **发现回流（阶段3）**：batch 审查 workflow 的发现自动转待办（治"34/36 未修复无回流"），向导留"从上轮 review 导入"入口。

产出：方案文档（含 UI 原型、API 增量、与既有 S1-S5 的差异表）→ 用户评审 → 批准后排实施。

---

## 执行顺序与依赖

```
P0（清账）→ P1（缺陷，依赖 P0.4 分支合并）→ P2（二三期，独立可并行）→ P3（方案）
```
P2 与 P1 无代码耦合，若用户希望提前看到 AI 抽屉可并行。全程内置浏览器实弹验收；
每 Phase 收官更新记忆 + 在本文件追加执行日志。

## 风险与明示不做

- 不做：三期b（浏览器校验/自动 merge，用户已定后置）、监控告警专案（本轮不排）。
- VM 上 docker/MM 基建已稳定（watchdog 冤杀已根除）；若再出现容器异常按 vm-forensics-playbook 处理，不影响计划主线。
- P2 的 AI 抽屉若 OD 短对话模型实测不稳（工具调用兼容），降级为纯表单+LLM 摘要，不阻塞转任务主链路。

---

## 执行日志

- **2026-10-02 · master-plan 全部 Phase 收官**：
  - **P0 清账**：MM bot 权限/alex 指定人批准/batch-test1 绑远端/分支推送合并全过（详见
    HANDOFF 对应章节与记忆 batch-test1-deploy-chain、deploy-approver-b3-delivery）。
  - **P1 缺陷清偿**：#16（qa 矩阵 risk 别名）修复部署（d3670c29）；#14 实弹解锁（代码修复
    方向已记录 verdict §3）；#17 饥饿缺陷登记（manual start 绕过）；run wfr-tjdb5b9r 13 步
    completed，verdict 见 docs/p14-join-regression-verdict.md。
  - **P2A 部署 AI 抽屉**：31edd902 实现+VM 部署+内置浏览器全链验收（三端点+抽屉+findings
    转任务；探针任务已清理）。
  - **P2B 三期a 确定性 HTTP 校验**：f0dfb872 三端点+自动链，三路实弹（成功/手动失败/自动转
    任务 t-20261001-4l6whq）+浏览器验收全过；沉淀完毕（HANDOFF P2B 章节 + 记忆
    deploy-verify-delivery）。t-20261001-4l6whq 是真实 bug 任务（未知接口 404 检查因应用
    返回 500 失败），留给用户裁决归档或让 agent 自然走完。
  - **P3 优化方案**：docs/large-requirement-optimization-plan.md（f69923f1）已产出待用户
    评审。要点：引擎侧 plan 冻结/物化/波次已在 dev 闭环（DM4 手工衔接已被消化），方案
    增量=①CreateTaskDialog batch 模式+preplan 端点+草案 vars 注入 ②验收物前置收敛为
    计划编辑器内 AC 勾选/骨架/交付面声明（对 master-plan 原文"qa 豁免单/ci_ready 预生成"
    做了诚实修正：豁免属 qa_signoff 现场、ci_ready 与需求无关）③审查发现 import 端点+
    向导导入入口。预估 4-5 天，批准后排实施。
