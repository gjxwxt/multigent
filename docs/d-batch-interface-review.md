# D 批窄接口倒置·评审底稿 v2（外部评审意见已消化）

> 2026-10-07 · v1→v2：消化外部评审 9 条声明（逐条对码核实：5 属实/3 部分属实/1 不属实），修正 4 处文档错误，新增 3 个工作项，2 处反驳。核实证据见 `work/license-audit-20261006/review-verdicts.md`（第 47 轮 HANDOFF 有索引）。
> 性质：接口评审 STOP 点材料 v2。v1 的"12 方法/门面缺 ListProjectMemberships/只欠 ErrNotFound"三处表述已证实过时或错误，以本版为准。

## 1. workflow 包依赖面实测（v2 修订）

workflow 包 17,749 行（自有 15,044 / 上游 2,705 plain 口径，-M -C -C 口径 store.go 2,075 + store_test.go 765——**引用数字须标注口径**，plain 与 -M 差异来自 95 行移动重归属）。对旧仓的依赖：

| 依赖 | workflow 实际用到 | 处置 |
|---|---|---|
| `controldb.Store` | **13 个方法**（v1 漏 AgentWorkerByID——store.go:2471 把 s.db 传入 membershipMatchesActor，2491 行调用，grep `s.db.` 不可见） | 新仓最小接口 `WorkflowStore`，kvfacade **13/13 全部已实现**（v1 "门面缺 ListProjectMemberships 需补"是错误——kvfacade.go:682/768 早在 B3 阶段实现，欠的只是测试：两方法现有 0 条测试引用） |
| 签名类型面 | KVWrite(10 处)/KVTxReader(3)/ProjectMembership+Filter(各1)/RecordWithRevision/AgentWorker + `ErrTransitionClaimLost` 哨兵（4 处 errors.Is） | kvfacade 已自带全部 6 类型 + `ErrClaimLost`；接口声明须含类型面 + 哨兵名映射 ErrTransitionClaimLost→ErrClaimLost |
| `entity` | 13 个 Workflow* 类型 **+ entity.Task**（review_resolution.go:90/147 访问 8 字段）**+ PlaybookObjectProvenance**（WorkflowDefinition.Provenance 字段类型，store.go:1384/1512/1517 写 nil；字段行 blame=chenhg5——迁类型或裁字段，C2 拍板） | 按使用面重定义（口径见 §3v2） |
| `errs` | `errs.NotFound` 1 个哨兵 | 新仓 `ErrNotFound` |
| `gitworktree` | SanitizeTaskID | 已在新仓 |

调用点归属（对码核实）：33 个生产调用点中上游 15（45%）/自有 18（55%）——v1 "大部分调用点在上游行"不成立，但 45% 中 13 处集中在定义/运行 CRUD 薄层，接口人审时过一眼语义。

## 2. workflow 对存储的 13 方法清单（33 实测调用点，v2 校准）

| 方法 | 生产调用点 | kvfacade 现状 |
|---|---|---|
| UpsertRecord | 6 | ✅ 已有 |
| ListRecords | 7 | ✅ 已有 |
| GetRecord | 5（含 1 处 Tx 内变体 plan_store.go:385） | ✅ 已有 |
| ListRecordsWithRevision | 4 | ✅ 已有 |
| UpdateRecordIfPayloadAndRevision | 2 | ✅ 已有 |
| DeleteRecord | 2 | ✅ 已有 |
| UpdateRecordIfRevision | 1 | ✅ 已有 |
| RecordRevision | 1 | ✅ 已有 |
| ListProjectMemberships | 1 | ✅ 已实现（kvfacade.go:682），**欠测试** |
| InsertRecordIfAbsent | 1 | ✅ 已有 |
| CommitTransitionGuarded | 1 | ✅ 已有 |
| CommitTransitionGuardedTx | 1 | ✅ 已有 |
| **AgentWorkerByID**（v2 新增） | 1（经参数逃逸路径） | ✅ 已实现（kvfacade.go:768），**欠测试** |

## 3v2. entity Workflow 类型面（15 项，v2 修订）

13 个 Workflow* 类型（清单同 v1：Field/Step/Run/Edge/Definition/StepInstance/Position/ActorBinding/Template/Branch/BranchInstance/StepEvent/EdgeCondition）+ 5 个 ID 构造器，**另加两项（评审发现，已核实）**：
- **entity.Task 子集**（review_resolution 路径访问 8 字段：BaseCommit/BranchName/CompletionCommit/CreatedAt/ID/IntegratedCommit/RemoteSyncCommit/UpdatedAt）
- **PlaybookObjectProvenance**（WorkflowDefinition.Provenance 字段类型）——C2 决策点：迁类型 or 裁 Provenance 字段

**重定义口径（v2 修正，最严重的一条评审意见已采纳）**：不能纯后端 grep 裁字段——实测 `WorkflowStep.Position` 后端零读取，但 WorkflowBoard.tsx 54 处引用 position.x/y 计算连线几何、画布拖动回写；Edge 的 SourceHandle/TargetHandle 同理。**Go 编译器兜不住 JSON 层**，按纯后端裁剪 = 编辑器保存后画布塌陷、连线手柄丢失（行为级回归）。正确口径：

```
保留字段 = 后端引用 ∪ 范围内前端编辑契约（WorkflowBoard/编辑器的读写字段） ∪ 资产字段
```

产出物：drop-list ADR（逐字段 keep/cut + 依据）+ **金样往返测试**（golden round-trip：真实 workflow 定义 JSON 进出新 schema 逐字节保持 Position/Handle 等前端契约字段）。

## 4v2. D 批实施序（v2 修订）

1. 新仓 `internal/workflow/contract.go`：`WorkflowStore` 接口（13 方法）+ 6 签名类型 + `ErrNotFound` + ErrTransitionClaimLost→ErrClaimLost 映射
2. kvfacade 补 ListProjectMemberships/AgentWorkerByID 测试（实现已在，各补 2-3 条表驱动）
3. entity 15 项类型重定义（§3v2 口径）+ drop-list ADR + 金样往返测试
4. workflow 包平移：自有 15,044 行原样迁；store.go 上游 2,075 行（-M 口径）不迁。**上游构成（v2 实测）**：状态机核心 StartRun/CancelRun/ReconcileRunCurrentAssignee/annotateRunCurrentAssignee 等 513 行 45% 上游（最危险块）；产品内容 679 行上游（SeedDefaults 134 / softwareDeliveryText 156 / 5 个 legacy 模板 389）——**模板正文迁移需产品决策**（哪些模板进新仓默认种子）；unified-delivery-pipeline 与 hotfix 模板主体自有（94%/97%，各余 6 行上游辅助闭包需重写）
5. 行为等价：**自有测试按函数粒度移植**（对码实测 27 函数：纯自有 13/纯上游 10/混合 3；plain 与 -M 口径会翻转 1 个函数的分类，移植前钉死口径）。**状态机等价背书主承担者不是 store_test.go**（其自有部分不覆盖状态机），而是 workflow 包其余 30 个净土测试文件——transition_idempotency_test.go 740 行全自有、28 处 StartRun/CancelRun/CompleteAndAdvance/Join 引用，branch_gate_test 13 处、deferred_join_ruling_test 10 处。v1 担心的"状态机无迁移测试"经核实不成立。
6. imbridge→workflow 6 类型 import 断裂处理（E 批前置）

**新增工作项（评审产出）**：
- **新仓内容级对账验收**：verify-clean 读目标仓自身 blame，对无上游历史的新仓**结构上恒真**（同义反复），检测不了改名式平移。新仓验收门升级为双闸：verify-clean（形式）+ **provenance manifest**（内容级：每文件记录来源 commit SHA + 与旧仓 bundle 的 diff 审计，重写件附相似度检查）。施工图 §0.5 三道闸已同步修订。
- 模板正文产品决策清单（SeedDefaults/legacy 模板逐个 keep/cut）

**夜间/子 agent 边界**：同 v1——只做第 1-3 步，第 4 步起等人审。

## 5. 评审问题（✅ 三问已定案 2026-10-07，用户委托按推荐执行）

1. ✅ **定案**：`WorkflowStore` 13 方法 + 6 签名类型 + `ErrNotFound`/`ErrClaimLost` 双哨兵接口集——按推荐（方案 A 窄接口）执行。
2. ✅ **定案**：entity 15 项按"后端 ∪ 前端契约 ∪ 资产"口径重定义 + drop-list ADR + 金样往返测试——按推荐执行。Position/SourceHandle/TargetHandle 等前端契约字段 JSON tag 必须逐字节保持。
3. ✅ **定案**：测试按自有函数粒度移植（plain 口径冻结后计数）+ 30 个净土测试文件为状态机等价背书主承担者——按推荐执行。

**附带产品决策（评审新发现，同日定案）**：store.go 上游行中的模板/文案内容处置——
- `unifiedDeliveryPipelineTemplate`（主体自有 94%）与 `hotfixDeployPipelineTemplate`（97%）：**平移**，各余 6 行上游辅助闭包（normalizeTemplateLocale/localizedTemplateText/field）重写。
- `SeedDefaults`（上游 134 行）、`softwareDeliveryText`（上游 156 行）、5 个 legacy 模板（上游 389 行）：**不平移**。legacy 模板多数引用已裁剪域（OKR/里程碑，B2 裁决）本已是死内容；新仓默认种子 = 自有 authored 的 greenfield 流水线模板族（17 步 unified-delivery-pipeline 等），minimal re-author 的默认文案随实现走。旧仓已实例化的 workflow run 是冻结快照，A2 已裁决不带历史，无兼容负担。

**STOP 级裁决①（2026-10-07，用户）：✅ 选 A——平移函数内的上游行必须零残留**。D 批相似度审计实锤"申报重写实为函数粒度平移"（98 函数带入 1,275 上游行：实质 ≈778 + 语法 ≈507），用户裁决对含上游行的函数执行真正语义重写（clean-room：读旧实现理解语义 → 全新实现 → 相似度检查复核 <50% 行同 → 31 测试文件全绿不删断言）。纯自有函数的逐字节平移不受影响（自有代码复制合法）。验收 = 上游行残留对账归零 + 逐函数相似度报告落 provenance-manifest。

**D2 终裁（2026-10-07，用户确认）：✅ 四类不可消除面验收口径成立，D2 关闭**。73 函数洁净室重写完成（相似度 73/73 <50%），实质残留 298 行全部落入：契约签名 58 / Go 惯用法 204 / 结构体形状 31 / 路由算子词表 5——版权保护表达而非惯用法/接口形状/数据契约，四类无替代表达。被测试断言钉住的一条 error string 子串（"it advances only through its branch join"）按运营契约措辞豁免。D 批 + D2 全案关闭，进 E 批。

## 6. 对评审者的反驳（2 条，附证据）

- **声明 4**（"12 方法大部分调用点在上游行，先冻结=固化旧设计债"）：33 调用点逐行 blame 实测上游 15/33=45%，未过半；其中 13 处是定义/运行 CRUD 薄层（语义稳定），plan_store/qa_baseline_store 调用点全自有。接口以"实测调用面"定稿不等于固化设计债——D 批后续由 30 个净土测试文件锁定行为，接口演化有安全网。
- **声明 7 后半**（"纯自有里 6 个含 2-18 行移动自上游代码"）：对 6 个模板/契约测试函数做 -M 与 plain 交叉对比，moved 行数全部为 0，两口径判定一致（全自有）。方向相反的差异倒是存在：全文件 -M 比 plain **多**判 41 行上游（766 vs 725）——-M 在此文件上偏保守而非偏宽松。
