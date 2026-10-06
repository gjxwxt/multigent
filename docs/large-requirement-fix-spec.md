# 大需求交付：缺陷修复规格（v1.1）

> 来源：`docs/large-requirement-split-explainer.md` 的红队审查 + 执行方对抗核查。
> 事实基准：dev @ 364cd2d4（2026-10-02）。本文件是**执行规格**，不是设计讨论。
> 每条含：缺陷 → 修法 → 涉及函数 → 测试点 → **陷阱裁定**（红队判定的实施地雷，必读）。

## 修订记录

### v1.1（2026-10-02）—— 对抗核查三处修订，含一处 v1.0 硬伤

| # | 修订 | 性质 |
|---|---|---|
| R1 | **P1-B 断言错误**：v1.0 称"撤销 defer ⇒ 下游回 Ready ⇒ join 放行"。实际撤销置回 `failed`，failed 传染语义是 **Blocked**（`plan.go:473-474`）⇒ join 依旧拒绝。已改为完整出路链（撤销 → retry → 完成），文案由 `undo a defer` 改为 `retry the deferred work` | **v1.0 硬伤**，已核实并更正 |
| R2 | **P0-A 传染漏了传递路径**：v1.0 只提直接边（`plan.go:471-472`），漏了 `default` 分支的传递边（`:478-481`）。已改为**在循环最前面按 kind 过滤**（contract 型边直接 `continue`）⇒ 两处自动同时覆盖；测试表补传递用例 | 缺陷补漏 |
| R3 | **P0-B 判定集校准**：去掉 `requirement_items`（它是需求快照不是计划，会误伤只接收快照的静态阶段）。判定集收紧为 `delivery_plan` / `batch_plan`（复用 `plan_freeze.go:54-55`），并要求实施前对着现存定义核真实 InputFields | 风险收敛 |
| R4 | 行号漂移修正：`chooseNextEdge` `:3587`→`:3588`、`planDriven` `:122-123`→`:122-124` | 无害 |

---

## 0. 陷阱总表（先看这个，否则会出事）

| # | 条目 | 陷阱 | 裁定 |
|---|---|---|---|
| T1 | P0-A | `PlanDigest = SHA256(CanonicalDeliveryPlanJSON)`（`plan.go:265-272`），canonical 含 `DependsOn`（`plan.go:227`）。改 `DependsOn` 类型 ⇒ canonical 变 ⇒ **存量冻结计划 digest 全部失配**（`plan_store.go:445` recompute 比对）⇒ `LoadFrozenPlanForRun` 报错 ⇒ `planStageBranches` planRefusal ⇒ **存量运行中的交付全炸** | **禁止改 `DependsOn` 类型。** 新增平行字段，canonical 仅在显式声明时纳入 |
| T2 | P0-B | 执行方倾向写"parallel_stage + 存在 plan 输入声明 ⇒ 强制 frozen"，但括号里写成"删除 `!planDriven` 静态回退分支"——**两者不等价**。无条件删除会让所有 legacy 静态并行阶段（无冻结计划概念的老项目）全部拒绝物化 | **必须是条件式。** 判定依据 = `Config=frozen` **OR** InputFields 声明了 plan 字段 |
| T3 | P1-B | 状态机**没有**"回退到前序步骤"的原语（只有固定配对的 rework 边，`store.go:256-273`）。"退回 contract_review" 无法直接实现；且 join 被拒后 deferred 比例不会自行下降 ⇒ **永久死锁** | **不做状态回退。** 改为"拒绝 join + 阻塞链可见 + 提供撤销 defer 操作" |

---

## P0-A. `dependencyKind` —— 堵 defer 传染把未执行包算作已处置

### 缺陷

`stageJoinAllowedByPlan`（`store.go:2329-2352`）遍历工作包时：

```go
for _, wp := range version.Plan.WorkPackages {
    if deferredSet[wpID] { continue }        // ← 跳过状态检查
    if states[wpID] != PlanWPStateCompleted { return false, nil }
}
```

`deferredSet` 来自 `EvaluatePlanProgress` 的传染（`plan.go:470-475`）。传染不区分依赖性质，因此：

> wp-B 只依赖 wp-A 的**接口定义**（契约，落盘即满足），A 被 defer 后 B 也被判 deferred 并跳过检查——**B 从头到尾没跑过**，join 照样放行，集成评审拿到缺件产物。

### 修法（按 T1 裁定：不碰 `DependsOn` 类型）

在 `PlanWorkPackage`（`plan.go:107-127`）**新增平行字段**：

```go
// DependsKind maps a dependency work package id to the kind of dependency.
// "contract": the dependency only has to land its contract artifact (schema /
// interface definition / error codes) — deferring it does NOT defer the
// dependent. "artifact": the dependent needs the real deliverable — deferring
// it defers the dependent. Absent key = "artifact" (pre-existing semantics).
DependsKind map[string]string `json:"dependsKind,omitempty"`
```

**为什么用平行 map 而不是把 `DependsOn` 升级成对象形态**：
- `DependsOn []string` 保持不变 ⇒ JSON 反序列化零 breaking change；
- canonical 里 `DependsKind` 仅在非空时纳入 ⇒ **旧计划的 canonical JSON 逐字节不变 ⇒ digest 不变 ⇒ 存量冻结计划不受影响**；
- 省掉"双形态 unmarshal"的分支代码和它的测试。

配套改动：

1. **canonical（`plan.go` `canonicalizeDeliveryPlan` 附近，参照 `:227` 的 `sortedTrimmed` 写法）**：
   仅在 `len(DependsKind) > 0` 时写入，key 按字典序排序输出，空值剔除，未知 kind 视为校验错误。
2. **校验（`ValidateDeliveryPlan`，`plan.go:281+`）**：
   - kind 只允许 `contract` / `artifact`，其他值拒绝；
   - `DependsKind` 的 key 必须是本包 `DependsOn` 里的 id，否则拒绝（防孤儿声明）；
   - 与 `PlanWaves`（`:392`）的环检测共用同一份依赖图——**kind 不改变依赖图拓扑，只改变传染语义**，波次计算不受影响。
3. **传染（`EvaluatePlanProgress`，`plan.go:462-505`）** —— **两处传染路径都要盖住，只改一处会漏**：

   **语义前提（决定实现方式，务必先读懂）**：contract 依赖指向的是**共享契约基线**——`contract_batch` 阶段提交、并行场开始之前就已固定的基线 commit。它**与上游包是否被执行无关**：B 跑过、跑失败、甚至从未物化，B 的契约都已经在基线里了。因此 contract 型的边**根本不参与传染计算**，而不是"参与计算但按 kind 分叉"。

   **实现**：在 `for _, dep := range deps[id]` 循环最前面按本条边的 kind 过滤——

   ```
   if DependsKind[dep] == "contract" { continue }   // 这条边不参与传染
   ```

   过滤放在最前面，下面**两处自动同时覆盖**（这是选它而不是"分叉两处"的原因——改一处漏一处的风险太高）：
   - **直接边** `case PlanWPStateDeferred: deferredSet[id] = true`（`plan.go:471-472`）；
   - **传递边** `default` 分支的 `else if deferredSet[dep] { deferredSet[id] = true }`（`plan.go:478-481`）。

   一个包有多条 artifact/缺省依赖时：只要**任一条**被 defer 或被判 blocked，即传染（保守取值，fail-closed）。
4. **join 守卫**：`deferredSet` 的构成随之改变，`store.go:2334` 的 `continue` 语义自动收窄——**无需改 join 代码**。

### 测试点（必须全绿）

| 用例 | 断言 |
|---|---|
| contract 依赖被 defer | 下游**不被**传染，仍在 Ready，可物化；join 在下游未完成时仍拒绝 |
| **传递传染**（v1.1 补，必测） | C `-contract→` B，B 因自身的 artifact 依赖 A 被 defer ⇒ 断言 **C 不被传染**（B 的契约在共享基线里，与 B 执行与否无关）。此用例专门防"只改直接边、漏掉 `plan.go:478-481` 传递边" |
| artifact 依赖被 defer | 下游传染 deferred（回归，行为不变） |
| 缺省（无 `dependsKind`） | 行为与改动前**逐字节一致** |
| digest 兼容性 | 用改动前的计划 JSON 冻一条记录，改动后 `LoadFrozenPlanForRun` **仍能通过 digest 校验** |
| 孤儿 kind | `dependsKind` 的 key 不在 `dependsOn` 中 ⇒ 校验拒绝 |
| 非法 kind | `kind: "whatever"` ⇒ 校验拒绝 |
| 混合依赖 | 一条 contract + 一条 artifact，artifact 侧被 defer ⇒ 传染（fail-closed） |

---

## P0-B. plan-driven 升不变量 —— 补信任链缺失的那条线

### 缺陷

`planDriven(step)`（`plan_materialization.go:122-124`）读 `step.Config["planMaterialization"] == "frozen"`。这是 `25445bcf` 引入的**配置项，不是不变量**。

`planStageBranches`（`:133-145`）在 `!ok && !planDriven` 时返回 `(nil, nil, nil)` ⇒ 调用方回落到模板的**硬编码静态分支** `workstream_1/2`（`greenfield.go:301-325`，自带 `workstream-agent-1/2` 的 ActorRole）⇒ **未经批准的身份面被物化**。

暴露面：
- `25445bcf` 之前实例化的工作流定义，其 `parallel_stage` 无该 config；
- `PUT /workflows/{id}` 允许编辑定义，人在画布上编辑可能丢掉 config；
- 收敛只靠 `ensurePlatformWorkflowDefinitions`（`server.go:439`），且只覆盖 platform-owned 定义。

### 修法（按 T2 裁定：条件式，双管）

**管 1 —— 入口判定改为"声明即强制"**

`planStageBranches` 的 `!ok` 分支改为：

```
planDeclared := planDriven(step)
                || step.InputFields 声明了计划字段（delivery_plan / batch_plan）
if !ok {
    if planDeclared {
        return planRefusal("parallel stage %q declares plan inputs but run %s has no frozen plan; ...")
    }
    return nil, nil, nil   // 纯 legacy 静态并行，保留回退
}
```

判定字段集**直接复用**冻结侧已有的字面量（`plan_freeze.go:54-55` 的 `{"delivery_plan", "batch_plan"}`）——抽成共享常量，**不要新写一份**。

**不要**把 `requirement_items` 加进判定集（v1.1 修订）：它是**需求快照而非计划**，出现在它上面会误伤只接收快照的静态阶段。

**实施前**对着现存定义核一遍真实 InputFields：默认模板 `parallel_workstreams` 的 InputFields 实际是 `contract_artifacts` + `approved_requirement`（`greenfield.go:291-294`），**两个都不在判定集里**——它靠的是 Config 路径。最终裁判是"纯 legacy 静态并行回归"测试，不是字段清单。

**管 2 —— 启动扫描存量实例**

在 `ensurePlatformWorkflowDefinitions`（`workflow_handlers.go:1959`）同批启动流程里加一道扫描：

- 遍历所有 workspace/project 的**已实例化**工作流定义；
- 找出 `type == "parallel_stage"` 且 InputFields 含 plan 字段但 `Config["planMaterialization"] != "frozen"` 的定义；
- 动作：`log.Printf` 告警 + **自动补 config**（与 platform-owned 收敛同一事务语义）；
- 幂等：已补过的直接跳过。

### 测试点

| 用例 | 断言 |
|---|---|
| plan-driven 且无冻结计划 | planRefusal，**不**回落静态分支 |
| 声明了 plan 输入但 Config 缺 frozen，且无冻结计划 | planRefusal |
| 纯 legacy 静态并行（无 plan 输入声明）无冻结计划 | 回落到静态分支（**回归保护，防炸存量**） |
| 启动扫描 | 缺 config 的定义被补上，重复启动不重复写 |
| 回归 | 现有 `plan_materialization_test.go` 12 个用例全绿，语义无漂移 |

---

## P1-B. join 的 deferred 过半闸 —— 含死锁出路

### 缺陷

10 个包 defer 9 个，join 照样放行，集成评审拿到 1 个包的成果。

### 修法（按 T3 裁定：不做状态回退）

在 `stageJoinAllowedByPlan`（`store.go:2303-2353`）的 mixed-mode 守卫**之后**加一道纯函数闸门：

```
deferredRatio = len(progress.Deferred) / len(plan.WorkPackages)
coveredReq    = 存在某个 requirement item，其全部引用者都在 Deferred 中
if deferredRatio > 0.5 || coveredReq { return false, nil }
```

**但拒绝 join 必须同时给出路**，否则永久死锁（deferred 比例不会自行下降）：

1. **拒绝时写根任务 attention + 阻塞原因**（复用 blocked 的可见性语义）：
   文案要可操作，例如
   `"delivery plan X: 7 of 10 work packages deferred (>50%); join blocked — retry the deferred work or abandon this round"`。
2. **新增"撤销 defer"操作**（`plan_branch_actions.go`，与 `deferPlanBranch:260` 对称）：
   - 前置：`inst.Status == PlanWPStateDeferred`；
   - 动作：实例状态回 `failed`，清除 defer 摘要里的 `deferred; ` 前缀，保留原始失败原因，写审计评论；
   - 传染是每次评估现算的，**无需手工级联**。
3. **明确不做**：不新增"回退到 contract_review"的状态跳转。

### ⚠️ 撤销 defer 单独**不构成**出路（v1.1 修正 —— v1.0 此处断言错误）

v1.0 写的是"撤销 defer ⇒ 下游从 Deferred 回到 Ready，join 恢复可放行"。**这是错的**（对抗核查指出，已核实）：

撤销把实例置回 `failed`，而 `failed` 的传染语义是 `blocked = true`（`plan.go:473-474`，这是**有意设计**）⇒ 下游进 **Blocked** 而非 Ready ⇒ `len(Blocked) > 0` ⇒ **join 依旧拒绝**。

撤销 defer 的真实作用是**解除过半闸**（该包退出 Deferred 桶 ⇒ 比例下降），把包从"已处置"挪回"待处置"。**完整出路链**：

```
撤销 defer → 实例变 failed（显式暴露）→ 人 retry → completed → 下游回 Ready 并物化 → 完成后 join 放行
```

或：废弃本轮、重开一个 run。文案因此写 `retry the deferred work`，不写 `undo a defer`。

### 明确不做

不新增"回退到 contract_review"的状态跳转。deferred 过半说明**这一轮的计划本身有问题**，正确动作是废弃本轮、重开一个 run，而不是把旧 run 倒回去重冻结（改计划要人重新批准，语义会乱）。这一点应写进文档 §9 边界说明。

### 测试点

| 用例 | 断言 |
|---|---|
| deferred 5/10（=50%，不 >50%） | join 放行（边界取严格大于） |
| deferred 6/10 | join 拒绝 + 根任务有 attention + 阻塞原因可读 |
| 某 req item 的全部引用者都 deferred（即使总数未过半） | join 拒绝 |
| 撤销 defer（v1.1 修正） | 实例回 failed；该包**退出 Deferred 桶**（比例下降，过半闸解除）；下游从 Deferred 转 **Blocked**；join **仍拒绝** |
| 撤销 → retry → 完成（完整出路链） | 下游回 Ready 并物化，完成后 join 放行 |
| 撤销 defer 的非法状态 | 非 deferred 实例 ⇒ 409 |

---

## 其余四项（不需要独立规格，顺手做）

| 项 | 做法 | 实施提示 |
|---|---|---|
| **P1 补位高频 tick** | reconciler 拆独立 30s 扫描，只扫 active 且 plan-driven 的 run，调 `materializeNextPlanWave`；**不动** Tier1 的 15min stall 判据 | 幂等已由 `planBranchIdempotencyKey`（`plan_materialization.go:266`）保证，直接复用即可。**必须验证**：tick 成功物化后是否会 bump `run.UpdatedAt`——若会，将掩盖真 stall，使 Tier1 永不触发 |
| **P1 降级留痕** | scale_gate 走默认边（`greenfield.go:557` `e-scale-default-linear`）时，在 run 上记一条 step event + 根任务评论"无裁决，已降级串行" | `chooseNextEdge`（`store.go:3588`）是**纯函数、无 server 上下文**，留痕必须在调用方做，不要试图改这个函数 |
| **P2 派发失败告警** | dispatch 静默 skip 时在根任务落 attention 记录 | `c6a60996` 已修 agentBinding 前缀问题，但"解析不到 ⇒ skip"仍静默，这是同一类 |
| **P2 文档勘误** | `docs/large-requirement-split-explainer.md`：§3 agentBinding 改写为"仅校验非空"；§6.5 删掉"测试运行证据"半句；§8 问题 3 重写为"解析不到 ⇒ 静默 skip" | §3 那处正在误导外部审查者，优先改 |

---

## 执行顺序建议

1. **P0-A** —— 真 bug，且改动面最小、风险最低（平行字段）；
2. **P0-B** —— 信任地基，但**必须先跑 legacy 回归**（管 1 的条件式判定写错会炸存量）；
3. **P1-B** —— 依赖"撤销 defer"新操作，工作量最大；
4. 其余四项随手。

每一步都要 `go build ./...` + `go test ./internal/workflow/ ./internal/api/`，并按私有 runbook 做行为级验证。

> ⚠️ **第 2 步的"先跑 legacy 回归"不是修饰语**。T2 陷阱的唯一防线就是它：管 1 的条件式判定一旦写成无条件，所有 legacy 静态并行阶段会当场拒绝物化，而单测可能全绿（因为测试计划都带 plan）。**legacy 静态并行回归用例必须先于管 1 落地。**

---

## 本轮不做清单（收尾时必须显式记录，不许悬空）

下列项目**不是"已解决"，是"有意识不做"**。执行者收尾写 HANDOFF 时必须逐条列出并注明状态，否则下一个人会误以为已完成：

| 项 | 状态 | 备注 |
|---|---|---|
| **缺口 3：并发闸只闸物化、不闸资源** | **双向遗漏，本轮不做** | 已被双方队列同时漏掉（红队提出 → 执行方压 P2 → 规格未收进清单）。真实暴露面：**已物化的包在 retry / 重驱时不受 `planMaxParallelMaterialization` 约束**。该 VM 实测并发 2 从未 OOM，故不阻塞本轮——但**必须记录**，不能当作已解决。若后续放开并发数，此项先做 |
| §8 问题 6：retry/defer 并发打到同一分支实例的 CAS 验证 | 本轮不做 | 未深挖 `update` 路径的 revision 语义；最便宜的加固是补一个并发测试（两 goroutine 同时 retry 同一 branch，断言只有一个成功、预算只 +1） |
| L1 自动重试 / 自动失败分类 | 明确不做 | 本轮只做"人选类别、系统推荐" |
| L2 Leader（多 Agent 协调者） | 明确不做 | 前置条件：本轮机制 + 独立设计评审通过 |
| 归档/删除的级联 | 明确不做 | 只有取消级联 |
| 挂起自动处置 | 明确不做 | Tier 2 只告警不杀 |
| "等待外部"状态机 | 明确不做 | 失败类别里留了语义位（environment），状态机本身不做 |

---

*规格版本 v1.1（2026-10-02）。陷阱 T1/T2/T3 由红队审查判定并经代码核实坐实，实施前请先读懂这三行；v1.1 的 R1/R2/R3 由执行方对抗核查指出并经复核属实，其中 **R1 是 v1.0 的事实错误**，已更正。*
