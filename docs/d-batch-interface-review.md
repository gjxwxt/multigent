# D 批窄接口倒置·评审底稿（STOP 点材料）

> 2026-10-07 · 上位：`docs/license-remediation-execution-plan.md` §6 / 证据：audit-full.json + 源码 grep 实测
> 性质：**接口评审 STOP 点的决策材料**——按施工图 §0.5-4，接口清单落盘后须人审，通过才继续 D 批实现。本文档只提取事实与方案，不做最终裁定。

## 1. workflow 包依赖面实测（D 批的全部对象）

workflow 包 17,749 行（自有 15,044 / 上游 2,705，上游全在 store.go+store_test.go 存储层）。对旧仓的依赖只有四个：

| 依赖 | 上游行 | workflow 实际用到 | 处置 |
|---|---|---|---|
| `controldb.Store`（internal/db） | 巨量（db 包整体不净土） | **12 个方法**（§2 清单） | 换成新仓最小接口 `WorkflowStore`，kvfacade 满足 |
| `entity`（13 个 Workflow* 类型） | workflow.go 218 行中 197 上游 | 13 个类型 + 5 个 ID 构造器 | 类型定义**按真实使用面重定义**进新仓 entity（C2 产物，见 §3） |
| `errs` | 63 行全上游 | **只用 1 个哨兵** `errs.NotFound` | 新仓自定义 `ErrNotFound`，一行的事 |
| `gitworktree` | 0（净土，已平移） | `SanitizeTaskID` 1 个调用 | 已在新仓，直接 import |

**结论：D 批的接口倒置面比 v3 预估更窄**——workflow 的存储调用是标准 KV 门面语义（UpsertRecord/GetRecord/ListRecords/修订守卫），B3 kvfacade 已全部具备；真正的设计工作在 §3 的实体重定义，不在存储接口。

## 2. workflow 对存储的 12 方法清单（grep 实测频率）

| 方法 | 调用次数 | kvfacade 现状 |
|---|---|---|
| UpsertRecord | 7 | ✅ 已有 |
| ListRecords | 7 | ✅ 已有 |
| ListRecordsWithRevision | 6 | ✅ 已有 |
| GetRecord | 4 | ✅ 已有 |
| UpdateRecordIfPayloadAndRevision | 2 | ✅ 已有 |
| DeleteRecord | 2 | ✅ 已有 |
| UpdateRecordIfRevision | 1 | ✅ 已有 |
| RecordRevision | 1 | ✅ 已有 |
| ListProjectMemberships | 1 | ⚠️ 门面无——需补（membership 三表查询）或接口位 |
| InsertRecordIfAbsent | 1 | ✅ 已有 |
| CommitTransitionGuarded | 1 | ✅ 已有（claim 语义） |
| CommitTransitionGuardedTx | 1 | ✅ 已有（Tx 变体） |

12 个中 11 个 kvfacade 直接满足，1 个（ListProjectMemberships）需补实现+测试（预计 <100 行）。

## 3. entity Workflow 类型面（C2 与 D 批的交界，评审重点）

workflow 引用 13 个类型（按使用频率）：

```
WorkflowField(291) / WorkflowStep(142) / WorkflowRun(55) / WorkflowEdge(54) /
WorkflowDefinition(44) / WorkflowStepInstance(40) / WorkflowPosition(34) /
WorkflowActorBinding(33) / WorkflowTemplate(21) / WorkflowBranch(20) /
WorkflowBranchInstance(19) / WorkflowStepEvent(18) / WorkflowEdgeCondition(12)
+ ID 构造器 5 个（NewWorkflow*ID / NewTaskID）
```

旧定义在 entity/workflow.go（218 行，197 上游）。**方案**：C2 重定义时按上述使用面反推字段——workflow 包用到的字段全保，纯上游遗留字段（OKR/Milestone 关联等已裁剪域的引用）不进新 schema。具体字段级清单在 C2 执行时生成（需逐类型 grep 字段引用，属实现工作非评审前置）。

## 4. D 批实施序（评审通过后执行）

1. 新仓 `internal/workflow/contract.go`：声明 `WorkflowStore` 接口（= §2 的 12 方法最小集）+ `ErrNotFound` 哨兵
2. kvfacade 补 ListProjectMemberships（先补测试再补实现）
3. entity Workflow 13 类型重定义落新仓（C2 同步产出字段级清单）
4. workflow 包平移：自有 15,044 行原样迁（import 换路径），store.go 上游 2,705 行**不迁**——其逻辑由 kvfacade + 接口接线替代
5. 行为等价：旧仓 store_test.go 中自有断言为基线移植（上游行>0 的测试段不迁，语义靠 kvfacade 自身测试背书）
6. imbridge→workflow 6 类型 import 断裂处理（挪独立类型包，E 批前置）

**夜间/子 agent 边界（施工图 §6 原文）**：只做到第 1-3 步（接口声明+类型定义+门面补缺），第 4 步起等白天人审接口清单后才继续。

## 5. 评审要回答的三个问题（用户/白天评审拍板）

1. `WorkflowStore` 接口 12 方法集是否认可？（替代方案：直接暴露 kvfacade Facade 具体类型——更省事但耦合具体实现，不推荐）
2. entity Workflow 13 类型"按使用面重定义"的原则是否认可？（替代：原样平移 workflow.go 197 上游行——违反判据纪律，不推荐）
3. store_test.go 基线移植口径：自有断言迁、上游段不迁——是否认可？（替代：全迁则把上游测试语义带入新仓，含上游行；不迁则等价背书全靠 kvfacade 测试——弱一档）
