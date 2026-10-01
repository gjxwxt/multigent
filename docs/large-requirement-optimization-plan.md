# 大需求拆分流程优化方案（拆分向导 + 验收物前置 + 发现回流）

> 状态：待用户评审（v1.0，2026-10-02）。
> 性质：master-plan Phase 3 产出（"三中的出个优化方案"）。本方案**衔接既有定稿
> `docs/large-requirement-delivery-module.md`（下称"原方案"）与已落地的 plan 物化
> 机制，不重起炉灶**；定位是原方案 S3-S5 缺口的三块增量补强，每块独立可实施、
> 独立可验收。
> 事实基准：本文全部代码引用核实于 dev @ f0dfb872（2026-10-02）。

---

## 0. 一页总览（TL;DR）

**现状盘点（代码核实，2026-10-02）**：原方案v1.0 写于 9/22（"实施未开始"），
此后引擎侧进展远超文档记载——

| 原方案机制 | 当时代价/缺口 | 当前代码事实 |
|---|---|---|
| scale_gate 结构化裁决（DM3） | 待实施 | ✅ 已落地 `internal/workflow/greenfield.go:219`（`scale_verdict` + `delivery_plan`/`batch_plan` 输出字段，cond 条件边 + 默认边 fail-safe 指 linear） |
| batch_plan→画布 branch 手工衔接（DM4 已知代价） | 人审后手工落实例 | ✅ **已被引擎侧消化**：plan 物化四切片（`plan_store.go`/`plan_materialization.go`/`plan.go`/`plan_branch.go`，c567ef11 等）——contract_review 批准时冻结计划（digest 锚定），parallel_stage 按 frozen plan **每就绪工作包自动派生分支+子任务+worktree+QA 基线**，波次推进（`materializeNextPlanWave`），1:1:1:1 幂等，fail-closed（`planMaterialization=frozen` 时无计划拒绝扇出）。模板描述原文："no canvas branch editing" |
| 需求包资产层（DM1/§4.5） | 唯一新平台能力，待实施 | ❌ **仍缺**：无 `POST /projects/{name}/requirements` 端点；但资料库（三表+绑定）与 @ 引用（542bca49）已生产，需求包可直接复用 |
| 入口体验 | 无 | ❌ CreateTaskDialog 只有 blank/template 两模式；大需求流程入口=知晓 workflow ID + 手工填 KV prompt |

**结论**：引擎核心（裁决→冻结→物化→波次→join）已闭环且有实弹（fp-dedupe、
ias-auth-center），真正的剩余缺口在**三个体验与流程面**——这正是本方案三块增量：

1. **拆分向导（阶段1）**：把"知晓机制的人才能发起大需求交付"产品化为
   CreateTaskDialog 第三模式。S2 时代的 DM4 摩擦已被引擎消化，向导不做
   batch_plan→branch 的衔接（那是引擎的活），只做**编排者工作台**：粘贴需求 →
   平台预裁决（linear/batched 预判）→ 人审编辑交付计划草案 → 以正确的工作流 +
   预填输入发起任务。
2. **验收物前置生成（阶段2）**：向导拆分时即产出"验收合同"（AC 锚点清单 +
   每 WP 的验收标准骨架），随任务 Vars 下发，让 acceptance_test_design 步骤有
   结构化起点——治 overnight 9 缺陷中"口径不清"类。
3. **发现回流（阶段3）**：批次审查（ZCode dynamic workflow，dwfrun-*）的发现
   自动转平台待办；向导留"从上轮 review 导入"入口。

**实施建议**：阶段1 为主体（2-3 天），阶段2 内嵌在阶段1 的计划编辑器（+1 天），
阶段3 独立（1 天）。总 4-5 天。

---

## 1. 引擎现状详述（方案的事实基础）

### 1.1 冻结→物化链路（已生产，零改动）

```
greenfield-delivery-pipeline（dev 现役模板）
requirement_draft (pm-agent)
  → requirement_review (human)
    → design_review (human, designGate)
      → scale_gate (pm-agent, agent_task)                    ← 结构化裁决
          ├─ cond scale_verdict=linear   → acceptance_test_design（原路径）
          ├─ cond scale_verdict=batched  → contract_batch
          └─ 默认边（无裁决）             → acceptance_test_design（fail-safe）
contract_batch (developer-agent)：共享契约落基线 + 重发 delivery_plan/requirement_items
  → contract_review (human)：**批准即冻结计划**（PlanFreezeConfigKey="true"，
    plan_freeze.go：校验→写 frozen record 于同一 transition 事务；F1 fail-closed：
    模板声明了 plan 输入但审阅实例没带计划 → 拒绝批准）
    → parallel_workstreams (parallel_stage, planMaterialization=frozen)：
        每就绪 WP → 1 分支（1 子任务+worktree+QA 基线+分支定义快照哈希）
        波次 k+1 挂在 branch-completion 路径自动物化
        失败/跳过 WP 阻塞传递依赖并显式暴露
        join（all）→ integration_review → qa → qa_signoff → …
```

关键代码锚点：

| 机制 | 位置 |
|---|---|
| scale_gate 步骤定义 + 条件边 | `internal/workflow/greenfield.go:219`（步骤）、`:513-566`（三边） |
| 冻结入口（唯一写点，批准即冻结） | `internal/api/plan_freeze.go:34`（isPlanFreezeStep）、`:42`（buildDeliveryPlanForFreeze）、`:224`（preparePlanFreezeTransition） |
| 计划持久化（revision-CAS，digest 根信任） | `internal/workflow/plan_store.go:218`（FreezeDeliveryPlan）、`:490`（ClaimPlanWorkPackage，2min lease） |
| 计划 schema + 校验 + 波次 | `internal/workflow/plan.go:64`（DeliveryPlan）、`:266`（ValidateDeliveryPlan）、`:377`（PlanWaves） |
| 物化引擎（就绪 WP→分支，幂等） | `internal/api/plan_materialization.go:266`（materializeNextPlanWave）、`:2206` 区（ClaimPlanWorkPackage 消费） |
| 计划驱动分支定义 | `internal/workflow/plan_branch.go:83`（PlanBranchFromWorkPackage） |

DeliveryPlan schema（`plan.go:64-86`）——向导编辑器的直接操作对象：

```go
type DeliveryPlan struct {
    SchemaVersion int
    PlanID        string
    Version       int
    RunID         string          // 冻结时钉入，digest 覆盖
    ResolvedAtBaseCommit string   // 信息性；基线由物化路径解析钉死
    RequirementItems []PlanRequirementItem  // {id,text,source}——人批快照，防上游改名漂移
    SharedContract   []PlanContractItem     // {id,artifact,path,owner,evidence}
    WorkPackages     []PlanWorkPackage
    IntegrationPolicy string
    QAPolicy          string
}
type PlanWorkPackage struct {
    ID, Title, Domain, Details string
    DependsOn []string              // 必须先完成的 WP id（波次依据）
    AcceptanceCriteria []string    // 引用 requirement_items id 或 "infra:<name>"——必填≥1
    AgentBinding string             // 与 ActorRole 二选一必填（校验拒绝无 actor 的包）
    ActorRole   string
    ExpectedDelivery []string      // 建议性：重叠警告+验收表，不是闸门
    ContractRefs []string
}
```

### 1.2 任务创建面的既有能力（向导复用，零/微改动）

| 能力 | 位置 | 向导用途 |
|---|---|---|
| `BaseTaskID` → 继承 completionCommit 作基线 | `internal/api/write.go:457-467`（baseTask 无 completion 快照→409） | 多任务编排时的依赖串接（阶段3 导入用） |
| 创建时资产绑定 `assets[]`（fileId/role/required） | `write.go:315`（taskCreateAssetBinding）+ `:690`（同步 StartRunWithInput 前生效） | 需求包（资料库文件）随创建绑定 role=requirement_input |
| `@ 引用` 已在对话框 | CreateTaskDialog AssetMentionPopover（542bca49） | 同上，UI 已有 |
| prompt KV → 起步步骤输入 | `write.go:1381`（workflowInitialInputsFromPrompt：仅起步步声明的字段可被 KV 注入） | ⚠️ 只覆盖 requirement_draft 步的 `request`/`context`；delivery_plan 草案注入需小扩（§3.2） |
| 工作流选择 + actor 绑定 + autoStart | CreateTaskDialog 既有（blank/template 模式） | 第三模式直接复用骨架 |

### 1.3 与原方案的差异表（本方案相对 v1.0 的修正）

| 原方案条目 | 当前状态 | 本方案处置 |
|---|---|---|
| §4.2 scale_gate 在 requirement_review 之后 | 实际在 design_review 之后（设计先行再裁决，避免大设计白做） | 尊重现实；向导预判不改变流程位置 |
| DM4"人审确认后手工/半自动落实例" | 已被 plan 物化四切片消化（自动） | 向导**不再做** branch 物化衔接；§5.1 的 v2 预案标记已兑现 |
| §4.5 需求包独立端点+入库 commit | 未实施 | **降级为复用资料库**：上传到 assets + `@ 引用`绑定 role=requirement_input；不做 `docs/requirements/` 入库 commit（DM9 的"入库即不可变基线"由资料库 SHA-256 内容寻址等价承载，且避免双存储） |
| S3（1-2 天端点+两处 UI） | — | 缩水为"向导内嵌资料选择器"，见 §4 |
| S4 0→1 验证 / DM10 零救场 | 未跑 | 本方案批准后，S4 用向导走（向导即 S4 的第一手验证） |

---

## 2. 阶段1：拆分向导（大需求编排工作台）

### 2.1 问题定义

现在发起一个大需求交付，操作者必须：
1. 知道 `greenfield-delivery-pipeline` 这个 workflow 的存在与语义；
2. 把 156KB 需求贴进 prompt 或先手动传资料再 @ 引用；
3. 知道 scale_gate 的裁决发生在流程中段，以及 contract_review 冻结的是什么；
4. 拆分质量完全托付给 run 中段的 pm-agent 一次输出 + contract_review 人审兜底。

原方案把这当成"可以接受的体验"，因为重心在机制层。但机制层已经闭环，入口
体验成了新的最短板：**模型在 run 中段现场拆分**，而操作者往往在任务创建时
就已知道该怎么拆（毕竟原方案 §1.1 说"拆解已由文档作者完成"）。

### 2.2 方案：CreateTaskDialog 第三模式 `batch`

对话框模式扩展为 `blank | template | batch`。三步向导：

**Step A · 需求输入**（复用既有组件）
- 粘贴需求文本（大 textarea）或从项目资料库选文件（复用 AssetMentionPopover，
  绑定 role=requirement_input required=true，走 `assets[]` 创建绑定）；
- 若粘贴文本超阈值（>64KB），提示先存资料库（一键上传）——大文本不进 prompt，
  消化由流程中段的 requirement_draft 步骤完成。

**Step B · 预拆分（平台确定性预处理 + 单次 LLM）**
- 新端点 `POST /api/v1/projects/{name}/delivery/preplan`（§3.1）：
  输入=需求文本/资料 fileIds；输出=`DeliveryPlan` 草案（不落库、不冻结）+
  `scaleHint`（linear/batched + 理由）+ requirement_items 锚点清单；
- 模型=项目 agent 模型账号（与 deploy-verify generate 同源：resolveDesignModelProvider，
  默认 glm-5.3-flash），16k 硬封顶沿用 OD 红线，`allowDesignRequest("preplan:"+project)`
  限流；
- **定位红线：预拆分是"草案生成器"，不是裁决者**。scale_gate 仍是流程内的正式
  裁决步骤；预拆分产物只作为 scale_gate 的可选输入加速其收敛（草案不注入也会
  正常走）。这保持 DM3"模型提议、人审拍板、引擎执行"的语义不变——只是把
  "提议"从 run 中段提前到创建时，且人审前置到发起时。

**Step C · 计划编辑与发起**
- 计划编辑器（新组件 `DeliveryPlanEditor`）：树形/表格编辑 WorkPackages——
  增删改 WP、编辑 DependsOn（环检测复用 `PlanWaves` 的校验）、为每个 WP 填
  AcceptanceCriteria（从 requirement_items 勾选）、选 AgentBinding（项目 agent
  下拉）、ExpectedDelivery 路径声明；
- 编辑器内实时跑 `ValidateDeliveryPlan` 的等价前端校验（schema 约束在 §1.1，
  前端做浅校验，深校验留给冻结时 fail-closed）；
- "发起交付"→ `POST /api/v1/projects/{name}/tasks`（既有端点），body：
  `workflowDefinitionId: "greenfield-delivery-pipeline"`、`assets[]`（需求包）、
  `vars.delivery_plan_draft: <压缩 JSON>`（§3.2 的新注入面）、prompt（含粘贴
  摘要 + KV 行）；
- 发起后任务卡片显示计划摘要（WP 数/波次数/agent 绑定），链接到工作流画布。

### 2.3 UI 原型（文本示意）

```
┌─ 新建任务 ──────────────────────────────────────────────┐
│ [常规] [从模板] [大需求交付]                             │
│ ──────────────────────────────────────────────────────── │
│ ① 需求来源   ◉ 粘贴文本   ○ 从资料库选择 [+上传]        │
│ ┌──────────────────────────────────────────────────────┐ │
│ │ (156KB 军标 SRS 粘贴区 / 已选: center模式-服务端.md)  │ │
│ └──────────────────────────────────────────────────────┘ │
│ ② 预拆分      [生成计划草案]  ← 单次 LLM，~10s          │
│    预判: batched · 5 个工作包 · 3 个波次 · 契约 3 项     │
│ ③ 交付计划（可编辑）                                     │
│ ┌──────────────────────────────────────────────────────┐ │
│ │ WP-1 契约与骨架     agent: FP Dev C   波次1           │ │
│ │   AC: req-3, req-7  交付面: api/, db/                 │ │
│ │ WP-2 密码学链       agent: FP Dev C   波次2  依赖:WP-1│ │
│ │   AC: req-1, req-2                                    │ │
│ │ WP-3 实例域         agent: FP Dev C   波次2  依赖:WP-1│ │
│ │   AC: req-4..req-6                                    │ │
│ │ [+ 添加工作包]   [校验计划 ✓]                         │ │
│ └──────────────────────────────────────────────────────┘ │
│ 执行 Agent: [FP Dev C ▾]  ☑ 立即启动                    │
│                              [取消]  [发起交付]          │
└──────────────────────────────────────────────────────────┘
```

### 2.4 不做什么（边界）

- **不做** batch_plan→branch 的实例衔接（引擎已自动）；
- **不做**流程外建任务树替代工作流——batch 模式发起的仍是**一个**挂 greenfield
  工作流的任务，拆分物是计划草案不是独立任务；多任务编排（每 WP 一个平级任务）
  是另一条路径（brownfield + BaseTaskID 串接），不在本方案范围；
- **不做**计划强制注入：scale_gate 输入字段里 `delivery_plan` 是 optional，
  草案经 vars 注入只是加速；裁决仍是 gate 自己的输出。

---

## 3. API 增量（全部挂主 mux + checkProjectAccess）

### 3.1 `POST /api/v1/projects/{name}/delivery/preplan`

- 请求：`{ source: "text"|"assets", text?, fileIds?: [] }`
- 鉴权：withTokenAuth + checkProjectAccess（读性质，无需 operator 门；
  但 LLM 调用有限流 `allowDesignRequest("preplan:"+project)`）；
- 行为：装配 prompt（需求文本或经资料库 staging 的内容）→ 单次 LLM →
  按 `ValidateDeliveryPlan` 同 schema 解析（失败 fail-open 返回错误，不落库）；
- 响应：`{ scaleHint, plan: DeliveryPlan, requirementItems: [...] }`；
- 不落库：草案只活在响应里，落库发生在任务创建（vars）与冻结（contract_review）。
  无状态端点，重放安全。

### 3.2 草案注入面：`delivery_plan_draft` 任务 Var（平台唯一小改动）

`workflowInitialInputsFromPrompt` 只把 prompt KV 注入**起步步**（requirement_draft）
声明的输入——`approved_requirement`/`requirement_draft` 等中段字段注入不进去。
方案：**新增确定性透传**——任务创建时若 `vars["delivery_plan_draft"]` 非空且
目标工作流含 scale_gate 步骤，则把该值经边映射链写入 scale_gate 实例的可选输入
`delivery_plan`（greenfield.go:229 已声明该 optional 输入字段，无需改模板）。

实现位置：`StartRunWithInput` 后的一次实例补写（或 step 激活时 vars 查找），
约 30-50 行 + 回归测试。fail-open：注入失败只记日志，流程照走（草案本就是
加速项）。替代方案（更重）：把 scale_gate 的输入声明改为 `$vars.delivery_plan_draft`
支持——但字段映射系统目前无 vars 源，改动面更大，不取。

### 3.3 不新增端点清单

- 任务创建：复用 `POST tasks`（assets/vars/workflowDefinitionId 全部现役）；
- 计划冻结/物化/join：全部既有；
- 资料上传：复用 `POST /projects/{name}/assets`。

---

## 4. 阶段2：验收物前置生成（内嵌向导，不单独立项）

原 master-plan 表述："拆分时按任务类型预生成 qa 豁免单骨架/ci_ready 检查项/
验收 runbook"。核实后**收敛为更小的实切**：

### 4.1 现状事实

- greenfield 流水线已有强验收合同链：acceptance_test_design（ATD）步在编码前
  产出 test_spec_manifest（case_id/ac_id/risk/automation/execution/expected_result，
  硬规则禁止"待观察"），qa 步按 manifest 独立复核，qa_signoff 按矩阵逐项豁免；
- ci_ready 闸门是 ci_ready 步的确定性检查（`mga ci ready`，十项校验），
  与需求内容无关（基线文件/CI 配置），**不需要**按需求预生成；
- overnight 9 缺陷中"口径不清"的根因是当时无 ATD 步（线性 13 步模板），而非
  缺预生成机制——unified-delivery-pipeline 已补 ATD 等价物。

### 4.2 收敛后的设计

预生成物 = **WP 级 AC 锚点 + 验收标准骨架**，恰是 DeliveryPlan 已有的
`AcceptanceCriteria []string`（引用 requirement_items id）+ `ExpectedDelivery`。
因此阶段2 = 向导计划编辑器里的三件事（无新 API）：

1. **AC 勾选器**：为每个 WP 从 requirement_items 勾选 AC（编辑器强制非 infra
   包 ≥1 条——与 `ValidateDeliveryPlan` 一致）；
2. **验收标准文本骨架**：勾选后自动生成 WP 的 AC 骨架文本（Given/When/Then
   占位 + source 章节号引用），存进 WP.Details——contract_review 人审时
   reviewer 看到的计划自带验收锚点；
3. **期望交付面声明**：ExpectedDelivery 编辑（路径 glob 提示），供物化后的
   重叠警告与验收表（引擎现役消费，advisory 不做闸门——尊重引擎裁定）。

qa 豁免单/ci_ready 检查项**不预生成**：前者由 qa_signoff 按实际矩阵现场签
（预生成会成为先验包袱），后者与需求无关。这是对 master-plan 原文的诚实修正。

---

## 5. 阶段3：发现回流

### 5.1 问题定义

批次审查（ZCode dynamic workflow，run `dwfrun-*`，36 发现 P1×7）的产物只存在
ZCode 会话工件里；9/28 修复验证显示 34/36 未修复且**无回流机制**——发现不会
自动变成平台内的可追踪待办。

### 5.2 设计

**方向A（平台侧，本方案采纳）**：批次审查报告落库 + 一键转任务。

- 批次审查 run 结束后，把结构化发现清单（findings[]：id/title/severity/evidence/
  suggested_fix）作为 JSON 附件存入项目资料库（复用 POST assets，displayName
  `batch-review-<date>.json`）；
- 新端点 `POST /api/v1/projects/{name}/review-findings/import`（operator 门）：
  body=发现 JSON，服务端逐条映射 `createProjectTaskFromBody`（与 deploy-ai
  转任务同管道：severity≥high→bug/P2，其余→chore/P0，`created_by: review-import`，
  labels `["batch-review"]`），响应含每条→taskId 映射；
- 前端：项目设置页"审查发现"区块（列表+全选+导入按钮）；部署 AI 抽屉的
  findings→tasks 前端交互（DeployAIDrawer）可复用其勾选模式。

**方向B（向导入口，轻）**：batch 模式 Step A 加"从上轮 review 导入"——若项目
存在 `batch-review-*.json` 资料，提供复选把未修复发现作为非目标/约束注入需求
上下文（文本拼装，无新 API）。

**不做**：ZCode 侧自动推送（越出平台边界，且 dynamic workflow 的报告格式
未冻结——先人工导出 JSON 再导入，跑通一轮回看是否值得自动化）。

---

## 6. 实施计划与验收

| 阶段 | 内容 | 验收 | 预估 |
|---|---|---|---|
| W1 拆分向导 | 3.1 preplan 端点 + 3.2 草案注入 + batch 模式三步 UI + DeliveryPlanEditor | 内置浏览器：粘贴 3k 行需求 → 草案 5 WP → 编辑依赖 → 发起 → run 起步步拿到资料绑定、scale_gate 收到草案 → make test/make web 绿 | 2-3 天 |
| W2 验收物前置 | 编辑器 AC 勾选 + 骨架文本 + ExpectedDelivery | contract_review 页面 reviewer 可见 WP 级 AC 锚点；ValidateDeliveryPlan 拒绝空 AC 包 | 1 天 |
| W3 发现回流 | 5.2 import 端点 + 设置页区块 + 向导导入入口 | 内置浏览器：样例 findings JSON 导入 → 任务逐条落库（正确 type/priority/labels）→ 从上轮导入注入需求上下文 | 1 天 |
| W4 0→1 验证 | 用向导在新项目完整走原方案 §2 旅程（替代原 S4） | DM10：零手工救场；todo 清单产出；问题回灌 | 0.5-1 天 |

依赖：W1 内 3.2 与 3.1 可并行；W2 依赖 W1 编辑器；W3 完全独立可提前；W4 依赖 W1-W2。

## 7. 风险与明示不做

- **不做**：需求包独立存储与 `docs/requirements/` 入库 commit（资料库等价承载）；
  多任务平级编排路径；ZCode 侧自动回流；qa 豁免单/ci_ready 预生成（§4.2 修正）；
  三期b 浏览器校验（用户已定后置）。
- **风险**：preplan 单次 LLM 对超大需求（>3000 行）的输出质量——草案失败
  fail-open（返回错误让用户手填/跳过），不阻塞发起；16k 封顶保证不烧钱。
- **风险**：vars 注入面（3.2）触碰工作流输入链——用 optional 字段 + fail-open
  控制爆炸半径，回归测试覆盖"无 vars 零影响/有 vars 正确到达 scale_gate/非法
  JSON 丢弃"三态。
- **红线继承**：preplan/import 端点全部主 mux + checkProjectAccess（import 加
  operator 门）；需求文本不进日志；LLM 输出 16k 封顶；模型账号走项目 agent
  既有凭据（无新凭据面）。
