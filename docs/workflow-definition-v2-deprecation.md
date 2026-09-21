# 工作流定义 V2 断代与存量兼容策略（Task 0.5）

> 来源：架构精简计划（first-principles reduction）维度二；落地于 2026-09-21。
> 原则：**以定义版本号划分断代线；在途跑完不迁移；存量不强制重实例化；垫片删除三条件。**

## 1. 断代线：DefinitionVersionV2 = 2

- `internal/workflow/gate.go` 中 `DefinitionVersionV2 = 2`。当前所有内置定义与模板实例化产物均为 Version 1。
- **V1（现状，兼容期）**：平台门（platform_gate）与交付步路由走 *marker 优先 + 形状猜测回退*：
  - 网关判定：`platform_gate` 显式标记 → 旧 `designGate` 标志 → 旧 ID 形状（`GateForStep`）。
  - 交付判定：`platform_delivery: pr_review` 显式标记 → 旧 ID/Title 子串（`PullRequestReviewStepMatches(step, defVersion)`）。
- **V2（目标态）**：只认显式声明。`platform_gate`、`platform_delivery`、`requires_remote` 三者必须显式；
  ID/Title 形状猜测全部失效（`PullRequestReviewStepMatches` 在 `def.Version >= 2` 时直接返回 marker 结果）。
- 内置模板（`agentic-software-delivery`、`unified-delivery-pipeline`、SeedDefaults 的 `software-delivery-v1`）
  的 PR 交付三步（create_pr / pr_review / merge(_and)_sync）已全部打上 `platform_delivery: pr_review`，
  并有守卫测试 `TestBuiltinTemplatesDeclareDeliveryMarkers` 锁定：**任何被 legacy 匹配命中的内置步必须带显式标记**。

### 为什么 zh unified 的 merge_sync 曾是证据

`unified-delivery-pipeline` 中文标题"自动合并与主干同步"不含英文 `merge and sync` 子串，且步 ID `merge_sync`
也不是 `merge_and_sync` 的子串——同一模板的中英实例在 legacy 匹配下行为**不一致**（en 的 merge_sync 命中、zh 逃逸）。
这正是形状匹配的脆弱性，显式标记后中英一致。

**行为修正定级（跨模型复审确认过程，2026-09-21）**：zh unified 实例的 merge_sync 步由此开始走交付准备
（`prepareTaskDelivery`）。这是 **bug 修复而非回归**——统一交付流水线的 merge 步本应触发交付准备（en 实例一直如此），
zh 实例因 Title 猜测逃逸而漏掉。定级 P1-accepted；若有 zh 存量定义在 merge_sync 步依赖"跳过交付准备"的旧行为，
需项目负责人重实例化或改用 V2 显式声明前知悉此变化。

## 2. 在途任务（In-flight Runs）跑完不迁移

- 兼容垫片（`store.go` output-whitelist 的 design/qa 豁免）保留至所有旧 Run 达到终态。
- 不做热迁移；引擎对旧定义的行为冻结（表征测试锁定）。

## 3. 存量定义不强制重实例化

- 交付模式（remote verify / delivery mode preflight）对未声明步骤展示"未声明"状态，这是给旧定义的诚实呈现，是特性而非缺陷。
- 升级由项目负责人在控制台自选：重新实例化模板（获得 V2 语义）或维持旧定义。

## 4. 垫片删除三条件（必须同时满足）

`store.go` 的兼容代码（design-gate 严格 `designGate=="true"` 豁免、qa_signoff ID 豁免）仅在以下三条**同时**满足后方可删除：

1. 线上无任何在途未完结的旧版本 Run（可查：`workflow_runs` 中非终态 Run 的定义版本均为 V2）；
2. 所有活跃项目已全部切换至 V2 工作流定义；
3. 连续两个迭代周期无新增旧版本定义实例化（审计日志口径）。

## 5. 版本号提升规范

- 新模板如需 V2 语义：`DefinitionFromTemplate` 目前强制 `Version: 1`；提升版本时需同时改此处与守卫测试，
  并在本文档记录版本演进历史。
- 修改内置模板结构（增删步、改边）时必须 bump 模板 `Version` 并评估存量实例化的重放行为。
