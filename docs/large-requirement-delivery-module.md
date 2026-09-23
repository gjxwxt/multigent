# 大需求标准交付模块（Large-Requirement Delivery Module）

> 状态：设计定稿（v1.0，2026-09-22），实施未开始。
> 性质：本模块是平台"标准研发流程"的目标态定义——每个新需求以同一条固化流程从 0 到 1
> 交付。本文是该模块唯一事实源（single source of truth）：背景、决策、阶段计划、验收
> 标准全部在此沉淀，后续任务围绕本文推进，完成后作为 commit 资产入库。
> 关联文档：`docs/auth-center-pilot-command-2026-09-04.md`（上一代指挥模式，本文显式
> 取代其"人工拆批 + 指挥者手工编排"的做法）；`docs/architecture/enterprise-evolution-and-scale-roadmap.md`（roadmap §5/§6 为其后续延伸）。

---

## 0. 一页总览（TL;DR）

**问题**：军标式大需求 SRS（3000+ 行、几十个 Use Case）目前靠"指挥者 + 辅助 Agent
人工拆批"逐批推进。拆批质量依赖临场发挥，流程不可复现，无法固化为平台能力。

**方案**：三层结构固化——

1. **需求包资产层**：需求文档作为项目资产上传入库（不是任务附件），批0 消化产出
   双层产物（结构化摘要 + 索引）供全流程引用；
2. **scale_gate 机制层**：现有 greenfield 流水线在 requirement_review 之后插入一个
   结构化裁决步骤，`linear`（小需求，走原线性路径）/ `batched`（大需求，走
   parallel_stage 扇出）条件路由——模型提议、人审拍板、引擎执行；
3. **双遍验证层**：先在已有 baseline 的项目上 dogfood 验证机制，再开全新空项目
   0→1 全旅程验证，问题清单回灌模板与入口。

**当前引擎能力盘点**（代码核实，2026-09-22）：条件路由（`cond` eq/neq/exists/in）、
`parallel_stage` 扇出/汇聚（joinPolicy all/any）、OD 设计门 fail-closed 输入链、
审阅轮次封顶——**全部现役，本模块零引擎改动**。缺口只有两个：需求包上传端点、
模板变体本身。

---

## 1. 背景与问题定义

### 1.1 触发场景

用户提供了一份 3,284 行 / 156KB 的军标式 SRS（IAS Auth Center，Center 模式）：
33 个 Use Case、27 个 API、7 张 DB 表、19 个错误码、13 种审计事件、36 条验证方法，
自带 UC→API→表→错误码四张映射矩阵。这类文档的特点：

- **拆解已由文档作者完成**（UC 总表 + 验证方法 + 追踪矩阵）——平台不需要再"理解
  并分解"需求，只需要把既有分解映射为批次与分支；
- **单任务上下文装不下**（156KB 进不了任何 prompt，更进不了 OD 的 16,384 token
  硬封顶）；
- **验收标准先于实现存在**——天然适合机器可验证交付。

### 1.2 上一代做法及其局限

`auth-center-pilot-command-2026-09-04.md` 记录了完整的指挥模式：批0 用
agentic 工作流跑全流程，批1-5 由指挥者人工编排、逐批建任务、逐门审批。该模式
取得了真实成果（rc1 交付、E2E 双节点验证、发布链闭环），但三个结构性局限：

| 局限 | 事实依据 | 固化后对策 |
|---|---|---|
| 拆批靠人 | 批1-5 的批次边界由指挥者与辅助 Agent 会话决定，无模板承载 | scale_gate 裁决 + batch_plan 进模板，成为人审对象 |
| 流程不可复现 | 换一个人/换一个 Agent，批次结构就不同 | 模板版本化，同输入同流程 |
| 经验不沉淀 | D1-D9 决策散在指挥文档里，下个项目要重读重学 | 本文档 + 模板内置（见 §3 决策继承表） |

### 1.3 为什么不是另外两条路（决策记录）

**否决"新建带 multi-branch 的初始化 Workflow"**：

- 初始化（模板物化 + 成员/agent 绑定 + rebuild）与需求处理是两个生命周期，混在
  一个 Workflow 里会让初始化重试污染需求状态机；
- 初始化是三个 API 调用的既定旅程（create → initialize → rebuild），不需要引擎管；
- 扇出的对象是需求分解结果，不是初始化步骤——branch 放初始化里是位置错误。

**否决"主动 Leader Agent 拆批次"**：

- 不可复现：Leader 每次拆的结构不同，流程退化为每次现场即兴；
- 不可控质量：上一代的拆批质量实际靠人工兜底（指挥文档 §3 批次计划由指挥者编写）；
- 重复劳动：SRS 作者已完成拆解，用模型判断覆盖确定性输入是方向性错误。

**采纳"模板变体 + scale_gate 条件分叉"**：流程确定性来自模板，灵活性来自 gate
的结构化裁决字段，可靠性来自人审闸门位置不变。

---

## 2. 固化后的标准旅程（目标态）

每个新需求，从 0 到 1，走完全相同的旅程：

```
① 建项目（react_spring_fullstack 模板）
② 选成员/Agent → 初始化（现有 API：initialize-template → rebuild → 绑定验证）
③ 需求包上传 → 入库 commit（docs/requirements/）→ 成为不可变 baseline
④ 发起交付任务（挂 greenfield 流水线固化版）
⑤ requirement_draft（批0 消化，产出双层产物，见 §4.1）
⑥ requirement_review（人审：批需求 + 批 scale_gate 预判，一次拍板）
⑦ scale_gate 条件路由：
     linear  → 原线性路径（implementation → self_review → code_review → …）
     batched → parallel_stage 扇出（branches = batch_plan 静态声明）
               → integration_review（人审：接缝集成 + 共享契约合并）
               → 汇回原流水线 qa → qa_signoff → release → go_live
⑧ OD 设计门（design_review）按现有 fail-closed 链运行，输入 = 人审批准的摘要
```

其中 ①②③ 是**资产准备段**（不进 Workflow 引擎），④-⑧ 是**交付流水线段**
（引擎编排）。两段之间唯一的耦合点是需求包入库后的 baseline commit。

### 2.1 两段分离的理由

初始化失败的重试（换模板、换绑定）不应回滚需求流程状态；反之需求打回循环
（requirement_review → request_changes）也不应触发重新物化模板。生命周期解耦是
平台六大生命周期解耦原则（HANDOFF §31.11）在本模块的映射。

---

## 3. 设计决策记录（含理由，实施中不得静默推翻）

| # | 决策 | 理由 | 代价（明示） |
|---|---|---|---|
| DM1 | 需求包是**项目资产**，不是任务附件 | 需求包被所有批次引用；附件模型会把它锁进单任务私产 | 需要新增上传端点 + 入库 commit 逻辑（约 1-2 天） |
| DM2 | 批0 产出**双层**：`requirement_draft`（≤8KB 结构化摘要）+ `requirement_index`（UC→章节→文件映射） | 摘要喂 OD（token 硬封顶）和 scale_gate；索引供 dev 批次按 § 号引用原文，避免重复塞全文 | 消化任务的输出 schema 要在 prompt 中强约束 |
| DM3 | scale_gate 是 **agent 步骤但裁决结构化**：`scale_verdict ∈ {linear, batched}` + `batch_plan`（批次清单 + branch 划分 + 共享契约清单） | 模型提议、人审拍板、引擎执行——与平台"机器可验证门禁"哲学一致 | 拆批质量受限于 pm-agent 一次输出的质量，靠 requirement_review 人审兜底 |
| DM4 | parallel_stage 的 branches **实例化时静态声明**（画布可见可改），不做运行时动态 spawn | 引擎不支持动态 branch（这是优点）：批次结构在 run 开始前被人审过，审计/重试/恢复语义完整 | batch_plan 与 branch 声明之间有一步"人审确认后落模板实例"的手工/半自动衔接，v1 接受 |
| DM5 | 共享契约（DDL/错误码/API 骨架）**必须先落库为公共基线**，branch 从该 commit 派生 | worktree 隔离模型下并行分支互相看不见对方代码，契约先行是不冲突的唯一解 | 契约错误会放大到所有 branch，因此契约批不并行、单独走一次人审 |
| DM6 | branch 切分线是**内聚域**（如：密码学链 / 状态机 / UI），不是文档章节号 | 密码学参数（盐前缀、canonical 拼接顺序）分给两个 agent 必对不上；UI 只依赖 API 契约天然独立 | 需要在 batch_plan 输出 schema 中显式要求声明"域内聚依据" |
| DM7 | 小需求走 `linear`，**整条流水线与现状完全一致** | 不为大需求把小需求的流程搞复杂；存量模板零回归 | scale_gate 对小需求多一步轻量裁决（秒级） |
| DM8 | 节点内并发（Claude Code subagent）**允许但限定只读/无副作用调研**；实现类工作以平台 branch 为准 | 节点内并发无平台状态、不可人审、不可恢复，只适合读放大；与 1.4（auth-center D7）一致但边界更严 | 大批内并行度下降，用 branch 数量补偿 |
| DM9 | 需求包上传后**立即自动 commit 入库**，元数据（来源/版本/大小）记入项目 | 上传即不可变 baseline，杜绝"后来又改了需求文档"的漂移 | 大文件入库占用仓库体积；v1 接受（军标文档均为纯文本 Markdown） |
| DM10 | 退出标准：0→1 全旅程**不允许平台外手工救场**；每处忍不住手工的地方记为平台化 todo | 保证固化的是真流程而非"有平台参与的真人流程" | 首轮验证可能因严格止损而中断，允许带 todo 清单终止当轮 |

---

## 4. 机制详细设计

### 4.1 批0 双层产物 schema（DM2）

`requirement_draft`（喂 OD 与 scale_gate，硬上限 8KB）：

```yaml
meta:           # 文档编号/版本/密级
business_flows: # 核心业务流程（主线级，如 注册→心跳→超时→归档）
pages:          # 页面清单（供 OD 原型）
roles:          # 角色与权限
states:         # 关键状态与异常（空态/错误/告警）
non_goals:      # 范围外（文档自标"不涉及"项显式列出）
```

`requirement_index`（供 dev 批次引用，无上限）：

```yaml
ucs:            # 每个 UC：id / 名称 / 章节号 / API / 涉及表 / 验证方法行号
error_codes:    # 错误码清单指针
artifacts:      # API 表 / DB 表 / 审计事件 的原文位置
```

### 4.2 scale_gate 步骤定义

- 类型 `agent_task`，actor `pm-agent`，位于 requirement_review 之后；
- 输入：`approved_requirement`（人审批准）+ `requirement_index` + 需求包路径；
- 输出（结构化，写进 OutputFields 供条件路由消费）：
  - `scale_verdict`: `linear` | `batched`
  - `batch_plan`: 批次/branch 清单，每项含 {branch_id, title, actorRole, 内聚域声明,
    inputFields（来自共享契约产物）, 预估 UC 集合}；
- 条件路由（引擎现役 `cond`）：
  - edge A: `cond{field: scale_verdict, op: eq, value: linear}` → `implementation`（原步骤）；
  - edge B: `cond{field: scale_verdict, op: eq, value: batched}` → `contract_batch` → `parallel_stage`；
  - 默认边指向 linear（裁决缺失时 fail-safe 走老路，绝不静默并行）。

### 4.3 大需求分支结构（batched 路径）

```
contract_batch (agent_task, 单独一批不并行)
    产出: 共享契约 = DDL + 错误码枚举 + API 骨架(OpenAPI) + 技术栈骨架
    ↓ human_review（契约评审——DM5 的强制闸门）
parallel_stage (joinPolicy: all)
    ├─ branch: crypto-chain    （4.1 生成工具 + 自检 + 导入验签 + 防篡改，举例）
    ├─ branch: instance-domain （注册/心跳/超时/归档 + 弹性配额原子 SQL，举例）
    └─ branch: console-ui      （Vue3 页面，契约 = API 骨架，举例）
    ↓ integration_review (human_review: 接缝集成 + 共享契约合并核对)
    ↓ 回到原流水线 qa → qa_signoff → pr/release → go_live（零改动）
```

> 注：以上 branch 划分是 ias-auth-center SRS 的映射示例，batch_plan 按每个需求
> 实际产出；DM6 的内聚原则不变。

### 4.4 OD 原型输入链（复用现有机制，无代码改动）

现状代码链（已核实）：`designGateRequirement()` 严格取 `approved_requirement` /
`requirement_draft` 字段 → `designGatePrompt()` 全文注入 `<requirement>` 标签 →
OD 容器（单次 16,384 token 硬封顶，默认 glm-5.3-flash）→ 产出原型 → design_review
人审冻结 → `approved_design_snapshot_path` 注入 implementation。

固化的唯一新增约束：**OD 的输入 = requirement_review 批准的 `requirement_draft`
摘要**（DM2 双层产物的第一层），原文与索引不进 OD。这消除"提取还是自己填"的模糊：
提取（机器）→ 批准（人）→ 原型化（机器），三段链各有 owner。

### 4.5 需求包资产层（唯一的新平台能力）

- `POST /api/v1/projects/{name}/requirements`（multipart）：存储 + 自动 commit 入
  `docs/requirements/` + 元数据记录（来源/版本/字节量/sha256）；
- 任务创建体（`internal/api/write.go` WriteTaskBody）新增 `requirementRef` 字段：
  从已入库资产选择引用（下拉），不做二次上传；
- 前端：项目详情页"需求包"区（上传/列表/预览）+ 创建任务对话框的引用选择器；
- 安全约束沿用平台既有惯例：端点挂主 mux（token 鉴权 + checkProjectAccess），
  入库前文件内容不进任何日志。

---

## 5. 实施计划（五个阶段，每阶段有独立验收）

| 阶段 | 内容 | 验收标准 | 预估 |
|---|---|---|---|
| S1 模板机制 | greenfield 模板加 scale_gate + 两条条件边 + contract_batch/parallel_stage 变体 + 测试 | linear 路径回归零变化；batched 路径单测覆盖扇出/汇聚/契约注入；`make test` 绿 | 1 天 |
| S2 Dogfood 机制验证 | 在 ias-auth-center（已有 baseline + 批0 产物）上发起真实 run 走 batched 路径 | 真实扇出 ≥2 branch 并汇聚；契约产物进公共基线；问题清单回灌模板 | 0.5 天 |
| S3 需求包资产层 | §4.5 的端点 + 入库 commit + 前端两处 UI | 上传→commit→任务引用选择全链路可用；未鉴权访问被拒 | 1-2 天 |
| S4 全新 0→1 验证 | 全新空项目完整走 §2 旅程（react_spring_fullstack 模板） | DM10 退出标准：零手工救场；todo 清单产出 | 0.5-1 天 |
| S5 固化收编 | 问题修复 + 模板版本号 + 本文档状态更新为"已实施" | 全部阶段证据入 `work/evidence/large-requirement-module/`；文档随 commit 入库 | 依发现 |

S1→S2 先行的理由：机制层不动 UI/API，改完立即可验；S3 动 API 与前端，晚做不阻塞
机制验证。S4 必须在 S3 之后（0→1 旅程依赖需求包上传入口）。

### 5.1 预注册风险（实施前已知，验收时逐条核对）

| 风险 | 缓解 | 残余处置 |
|---|---|---|
| 扇出需要 ≥2 空闲 agent 槽位 | 单机双 agent（Lina+Mira）即可满足两 branch；更多 branch 需第二 runtime node（roadmap 分布式议题） | batch_plan 裁决时把"可用槽位"作为 branch 数量上限输入 |
| jvm/spring 技术栈冷依赖慢（上一代最大时间税） | 模板 jvm21 profile 已 canary 验证；契约批内完成骨架搭建 | 阶段验收不设时间门槛，只验产物 |
| batch_plan 与画布 branch 声明的衔接（DM4 的已知代价） | v1 允许"人审确认后手工/半自动落实例"；S4 观察 | 若高频摩擦，v2 考虑 batch_plan→实例定义的自动物化端点 |
| requirement_draft 8KB 对超大需求不够 | 索引层兜底细节；摘要只承载流程/页面/角色/状态 | 允许 OD 门输入另附 pages 专节 |

---

## 6. 验收与追溯

- 本模块自身的追溯单元 = 阶段（S1-S5）；每阶段在本文"执行日志"追加记录 + 证据
  路径，commit message 引用本文（`docs/large-requirement-delivery-module.md`）。
- 被交付项目（如 ias-auth-center）的追溯单元 = batch_plan 的 branch/批次 + 现有
  工作流门的审阅记录，不在本文重复。
- 模块完成的定义：S1-S5 全部关闭，且"下一个新需求"能够由非设计者按 §2 旅程走通。

---

## 7. 执行日志（倒序追加）

- 2026-09-23（S2-2.3 修复轮，用户评审第 1+2 项）：用户对 12f1fe94/f37e85d7 的
  四项评审中先关闭两项代码问题：①P0 急切白名单——join 闸门在
  checkBranchQAGate 里直接调 ValidateQATouchedPaths，交付分支声明业务文件
  在 delivery 语义生效前就被拒绝（低层测试 TestDeliveryCheckpointAllows
  BusinessFiles 直调 verifyWorktreeDeltaAgainstDeclaration 绕过了正式入口，
  全绿但入口仍坏）。契约拆分：ValidateTouchedPathFormat（通用：格式 +
  禁区 CI/部署/凭据/agent-config，两种检查点都拒绝）vs
  ValidateQATouchedPaths（QA 角色：通用 + 测试工件白名单，仅线性 QA）；
  交付白名单仍由 verifyDeclared Direction 3 的 surface flag 控制。②P1
  度量归属——join 前无合并，父工作树不可能持有分支 delta；闸门改为度量
  分支子任务自身工作树 + 自身捕获时基线：precheck 键 t.ID，权威 join gate
  新增 deliveryTaskID 参数（taskID 保持 run 查找句柄）。正式入口契约测试
  TestPrecheckUsesBranchWorktreeNotParentWorktree 重写为双工作树（分支编辑
  vs 父树噪音）+ 每任务基线、编辑前捕获。收尾补正向全链测试
  TestBranchJoinHTTPBusinessDeliveryEndToEnd（df3e19cb+fa618213）：业务
  server.go 经生产 HTTP 入口完成 join，核验子任务归档、子 run 终态、
  branch 实例记录业务申报、父 run 汇聚推进。本轮权限范围澄清：通用校验
  禁止 CI/部署/凭据/agent-config 是当前工作包的权限边界，不是所有研发
  任务的永久规则；需要改这些文件的需求走受控授权步骤。独立复审（真
  claude-sonnet-4-6）首遍 request_changes（1 P1 + 2 P2），9a87bbc8 全部
  关闭：①P1 双分支隔离契约测试 TestPrecheckTwoBranchesIsolateWorktrees
  AndBaselines——三目录（父+A+B）各自基线编辑前捕获，A/B 未申报编辑按名
  拒绝且互不泄漏、诚实申报独立通过；②P2 mustUploadQABaseline 时序陷阱
  文档化（编辑后重捕把脏树洗进权威基线）；③P2 CompleteBranchAndMaybe
  Advance 文档写明生产 taskID != deliveryTaskID 不变式。第二遍复审
  approved。教训：fixture helper 的隐式时序依赖必须显式文档，否则下个
  测试作者必然踩中（本轮契约测试就因此假通过过一次）。

- 2026-09-23（S2-2.2 修复轮，独立复审）：对 77b9118c+09cb58e5 最终代码的真
  claude-sonnet-4-6 独立复审返回 request_changes（1 P0 + 2 P1 + 3 P2），全部
  修复于 12f1fe94：①P0 崩溃窗口 re-drive 竞态——并发重报的 CAS 败者把
  CompleteAndAdvance 拒绝当错误上抛（幂等 200 变 500），现败者重读 run，
  已离开 join 步或终态即判定胜者完成了 re-drive，降级为零写回放；②P1
  re-drive 是成功路径恢复——任一 branch failed 时 stage 即失败，completed
  branch 重报只回放记录结果，绝不推进（workflowBranchAnyFailed 闸）；③P1
  embedded 分支 fail-closed 区分"契约真不存在"与"父侧读取瞬时故障"，后者
  返回可重试错误而非 4xx 判决；④P2 补边界测试（failed branch 重报不推进、
  交付面下 Direction 1/2 仍生效仅 Direction 3 抑制）+ tamper 错误携带
  expected/actual digest。教训：并发幂等路径的失败语义与成功语义同等重要，
  CAS 败者的错误必须分类处置而非一律上抛。

- 2026-09-23（S2-2.1 修复轮，自查发现）：提交后自查探针发现 S2-2 的 ⑤ 决策
  （branch join = 交付增量免白名单）只写在注释里，代码未落地——
  verifyDeclaredAgainstReal 的 Direction 3 无条件对 real delta 跑测试工件白名单，
  分支交付业务代码（server.go）在 join 处被结构性拒绝，S2-1/S2-2 要解的死锁仍在。
  修复：检查点种类上移到度量面（qaBaselineSurface.deliveryDelta），Direction 3
  仅对 legacy 线性 QA 检查点生效；checkBranchQAGate 标记交付面。回归
  TestDeliveryCheckpointAllowsBusinessFiles 双向锁定（业务交付过 join、同 delta
  过线性 QA 仍拒）。教训：注释与代码语义必须由测试锁定，不能靠评审叙述。

- 2026-09-22（S2-2 修复轮）：S2-1 补丁经 GPT 复核与独立跨模型复审（真
  claude-sonnet-4-6）双通道 request_changes，6 项发现收敛后全部修复：

  - **P0-1 基线信任模型重定**：权威基线只存在于控制平面（kv_records
    `qa_baselines`）；worktree 副本（manifest 0644 可写，金丝雀而非保护层）仅
    用于恢复与篓改检出。worktree 副本 digest 不匹配 → `ErrQABaselineTampered`
    fail-closed；manifest 在而控制平面记录丢失 → `ErrQABaselineLost`
    fail-closed；无 manifest 的老 worktree → 绝对 status 回退（依旧严格）。
    二次采集前滚动旧副本，采集上传失败不残留副本。
  - **P0-2 预检 worktree 解析修正**：branch join 预检与落账闸门统一用
    `workflowRootTaskIDVar`（父任务）解析 worktree，不再误用子任务自身 ID。
  - **P1-1 预检契约源修正**：branch 契约从**冻结的 branch instance
    OutputFields**（fan-out 时快照）读取，回退父 run 定义快照；embedded
    multi-step branch 无契约即拒（fail-closed）——消除“预检过、join 拒”的
    错契约窗口。
  - **P1-2 幂等 re-join**：已终态 branch instance 的重报零写返回记录结果；
    已归档子任务的重报路由到 `resumeArchivedBranchJoin` 续跑 join——重试
    不再被陈旧拒绝卡死，也不得借重报重新推进父 run。
  - **P2 加固**：fingerprint 覆盖权限位/symlink/special 文件；
    `unquoteGitPath` 严格三位八进制校验；交付增量（branch join）与线性 qa
    步白名单两套度量面彻底分离（QA 编辑业务文件依旧必拒）。
  - 回归：`internal/workflow/qa_baseline_regression_test.go` 重写至信任模型
    （9 项）；新增 `internal/api/runtime_branch_precheck_contract_test.go`
    （5 项）；join HTTP 套件修正 fixture 语义（子任务 Vars 携带父 run ID——
    与生产 fan-out 对齐）并按 P1-2 契约更新重复上报断言。全仓 `go test ./...`
    / `go vet` / web `tsc` 全绿。

- 2026-09-22（S2-1 修复轮）：S2 dogfood 于 parallel_workstreams 汇聚处暴露引擎缺陷，
  经评审批准实施两项修复（本文件 §4.5 闸门语义随之修订，后续实现以本条为准）：

  **缺陷链（证据：Mira 会话日志 + DB + 源码比对）**：

  1. **完成路由缺口**（Fix A）：branch 子任务走 `mga task step done` →
     `handleRuntimeWorkflowStepComplete` 缺少 branch 分流 → 以子 run 身份
     `CompleteAndAdvance` 先落账（子 run completed、任务 done_success），然后
     branch join 的 QA 闸门 fail-closed 拒绝（`.cursor/`、`.mcp.json` 等平台
     脚手架被要求声明）→ 400 返回时终态已落账、agent 已退出 → branch instance
     永远停在 running，joinPolicy=all 永不满足。**失败后的状态一致性缺失**。
  2. **闸门度量面错误**（Fix B）：touched_paths 真实变更闸门度量绝对 git status，
     平台物化脚手架与基线期遗留未跟踪文件被误计为任务交付，诚实完成被拒绝。

  **修复（commit 见 git log S2-1）**：

  - Fix A：branch join QA 闸门提取为共享校验（`Store.PreviewBranchQAGate` +
    `NormalizeWorkflowOutputValuesForPreview`），step/complete 端点在 branch
    子任务**终态步**（child 定义无出边的当前步）持久化前预检同一闸门——拒绝时
    零写入、任务保持 in_progress、branch 保持 running，agent 可修正重报；中间步
    与线性路径完全不受影响。回归测试：
    `internal/api/runtime_branch_precheck_test.go`（预检五态）、
    `internal/api/runtime_branch_join_http_test.go`（HTTP 端到端：拒绝后零终态、
    修正重报成功、重复上报不重复推进、线性任务零影响）。
  - Fix B：worktree 物化时（`gitworktree.ensureWorktree`，agent 启动前的受保护
    时点）记录内容指纹基线（`.multigent/qa_baseline.json`，SHA-256 per path）；
    闸门改度**任务交付增量**（相对基线的新增/内容变化/删除），同路径再修改与
    已提交变更均可检出（GPT 边界意见采纳：纯路径集合会漏检这两类）；损坏基线
    fail-closed 不降级；无基线的老 worktree 保持原绝对 status 语义（依旧严格）。
    回归测试：`internal/workflow/qa_baseline_regression_test.go`（脚手架误报
    消除、已提交变更、同路径再修改、删除、损坏基线、无基线回退六项）。

  **评审明确排除的方案**（记录防止回潮）：不扩大目录豁免清单（清单会爬行且违背
  确定性匹配）；不以完成时状态补造基线（会漂白闸门本要捕获的变更）；历史脏文件
  不自动认定为合法交付内容。

  **S2 验收状态：未通过**（评审决定）。wfr-07249bes 两个 workstream 产物已确认
  完整（WS-A: `feat/ws-a-client-lifecycle-63b32fc` @ 67c379d；WS-B:
  `feat/ias-auth-center-ws-b` @ ca7981c96dd；契约基线 63b32fc6b610），且两个
  agent 工作区 git 状态与完成申报一致（worktree 仍可观测），但汇聚未发生，且
  "两个分支进入同一集成候选 SHA"（§6 提高的验收目标）尚未发生。历史 run 恢复
  方案见 `docs/s2-wfr-07249bes-recovery.md`；恢复操作不计入验收证据。

  **架构方向修订（评审意见，指导后续 S 阶段）**：大需求编排从 "scale_gate 选择
  线性/并行" 提升为 "批准的交付计划驱动分批执行"（规划与审批 → 冻结计划 →
  物化任务 → 按依赖分批执行 → 集成验收）；scale_gate 保留为入口判断。首批验证
  目标调整为：冻结交付计划 → 自动物化第一批任务 → ≥2 个工作包正确执行并集成为
  同一候选 → 本批验收通过后启动下批 → 失败/变更可从明确位置恢复。本文 §4 后续
  将按此修订（当前先完成可靠性修复，不在本次补丁中重写编排架构）。

- 2026-09-22：v1.0 设计定稿。三轮会话收敛：①引擎能力盘点（parallel_stage/cond 路由/
  OD 输入链全部现役，零引擎改动）；②否决两条备选路线并记录理由（§1.3）；③补齐需求
  包资产层与 OD 输入标准两个缺口定义（DM1/DM2/DM9）；④确认双遍验证路径（S2/S4）。
  设计依据全部来自当日代码核实：`internal/workflow/greenfield.go`（模板与条件边）、
  `internal/workflow/store.go:3101`（chooseNextEdge/条件路由）、
  `internal/api/runtime_workflow_handlers.go:1283`（parallel 激活）、
  `internal/api/design_handlers.go:943`（designGatePrompt 全文注入链）、
  `web/src/components/workflow/WorkflowBoard.tsx:259`（前端三节点类型支持）。
