# 许可证处置·最终方案 v3.0

> 2026-10-06 · 事实基准 `HEAD = 4adfbcc`（含 83 个未提交文件，见 §13）· 三方交叉审计收敛版
> 版本链：v1.0（事实底稿）→ v2.0（三轮对抗审查）→ v2.1（A 组判据修正，仅对话未落盘）→ **本版（v2.1 全部修正 + 两轮逐项 blame/grep 实测复核后的终版）**
> 性质：工程处置方案，**不是法律意见**。正式决策需法务确认。

---

## §0 一句话

**双轨：旧仓立即冻结（停同步 + 存证）；新仓从零建。阶段 0 先建 KV 存储门面，严格净土 5 包平移，
准净土 6 包吃门面跟进；workflow 心脏走窄接口倒置；部署中心垂直切片（带四个前置小活）当第一个商用验证标的；
前端按文件级 blame 审计选择性回收。端点裁剪清单与存储原型并行做前置，在此之前任何工期数字都不作数。
询价信与停同步今天做，成本≈0。**

---

## §1 事实基准（v3.0 全部实测，含 v2.0 未审计的包）

| 对象 | 行数 | 自有 | 上游/非自有 | 处置裁决 |
|---|---:|---:|---|---|
| 全仓（@70cfa058） | 324,556 | 49.2% | 50.8% | v2.0 实测；冻结存证时重跑（方向有利：83 个未提交文件全为自有） |
| `internal/db` | 14,388 | 7,798 | **6,590（45.8% chenhg5）** | **必须重写**，不是平移 |
| `internal/entity` | 2,134 | 270 | **87.3% 非自有**（root 1,212 = 2026-07-14 初始导入 + chenhg5 652） | **必须重定义领域实体** |
| `internal/workflow` | 17,470 | ~15,418 | 1,737 全在 store.go/store_test.go 存储层 | 心脏平移 + 存储层替换（一举两得） |
| `internal/imbridge` | 6,153 | 4,622 | **1,531（24.9%）** | **v3 新纳入审计**：整包不净土，按文件选择性搬 |
| `internal/sandbox` | 2,904 | 1,658 | 1,246（42.9%：chenhg5 846 + root 399） | 非净土， brownfield 的依赖链踩在它上面 |
| `web/src` | 117 文件 | 13,850 | 37,705 + root 14,159 | 逐文件三档处置（§7） |

**root 作者口径警告**：web/src 中 root 有 14,159 行，entity 中 1,212 行 = "Initial Multigent codebase"（2026-07-14）。
存证审计时必须把 root 的身份口径定死（初始导入 vs 后续 VM 提交，逐 commit 看日期可分），否则高自有档的统计会漂移
（同一 >80% 阈值，gaojiaxiang-only 口径得 1 个文件/2,032 行，root 计入自有得 9 个/4,152 行）。

## §2 四条不可动摇的红线（沿用 v2.0）+ 过渡期决策

R1 停止同步上游 · R2 不靠隔离/比例/改名规避 · R3 商用前交付链路上游代码为零 · R4 干净 git 历史新仓。

**R3 的尖锐推论与过渡期决策（v3 升格为显式步骤）**：
PolyForm noncommercial 定义下，**用旧仓承载内部真实业务（ias-auth-center、RoomFlow）现在就在违规区**，
违规暴露按天累计，不等待重建完成、也不等待法务排期。所以：

- **interim 缓解必须先于法务确认做出临时决定**（三选一：停用这类使用 / 买授权买窗口 / 书面记录自担风险继续）；
- 法务确认（§12 步骤 3）是对这个决定的复核与升级，不是它的前置。

## §3 包分组账本（判据：传染链上任何"上游行 > 0"的包都算污染源）

| 组 | 包 | 自有行 | 依赖 | 说明 |
|---|---|---:|---|---|
| **严格净土** | gitworktree / codehost / projecttemplate / doctor / ciready | **9,031** | 仅组内（ciready→projecttemplate） | 直接平移 |
| **准净土** | previewreceipt 5,243 / fixturesandbox 2,490 / changerun 2,257 / brownfield 1,107 / assets 509 / secretbox 407 | **12,013** | 各依赖 db 门面 10–13 个符号 | secretbox 先清 6 行残留；**brownfield 是最弱一环**：依赖 preview（上游重）+ sandbox（42.9% 非自有），实际排期应放在 preview 重建之后 |

**判据纪律（从 v2.1 事故沉淀，适用于一切后续审计）**：
1. 污染源判据一律用"上游行 > 0"，**禁止用比例阈值**——自有占多数但绝对量大的包最容易漏（internal/db 就是案例）。
2. 传染分析必须走**依赖闭包**，per-file / per-package blame 结论**不得外推到闭包**（部署中心切片是第二个案例，见 §7）。

模板资产（projecttemplate 的 .json/.yaml）不在代码 blame 覆盖内，需单独盘点后才能给安全结论。

## §4 阶段 0 — KV 存储门面（新增阶段；v2.0/v2.1 都漏的隐性前置）

实测准净土 4 包的 db 符号清单，门面真实宽度：

- **KV 记录 CRUD + 修订守卫**：GetRecord / UpsertRecord / DeleteRecord / ListRecords / ListRecordsWithRevision / UpdateRecordIfRevision / UpdateRecordIfPayloadAndRevision / InsertRecordIfAbsent / KVWrite / KVTxReader
- **claim 语义已进入准净土**：changerun 用 `CommitRecordWritesGuarded`、previewreceipt 用 `...Tx` 变体——门面不是纯 KV，**第一批 claim 语义在阶段 0 就要落地**
- **workspace**：UpsertWorkspace / Workspace（四包共用）
- **两个特殊面**：fixturesandbox 用裸 SQL（Exec/Query/QueryRow，schema 级耦合）→ 门面加受控查询出口或包内小改造；assets 用一串 typed helper（AssetAttachment/AssetBlob/AssetFile/AssetRoleReference…）→ 重指向 raw records 或实现

**关键性质：门面 ⊆ 阶段 2 的 RunRepository**——workflow 实测用 db 20+ 个方法（上述全部 + CommitTransitionGuarded/Tx + membership/worker），一次投入两阶段复用。
预算：门面约 800–1,200 行（在 §9 阶段 2 的 2,200–3,200 行预算内前置支出）。

## §5 阶段 1 — 严格净土平移 + 领域实体重定义

- 5 个严格净土包直接迁入；新仓重定义 domain entity（entity 87.3% 非自有已实测，"抛弃冗余字段"裁决成立）。
- **验收判据**：`git log` 无上游 commit；迁入文件 blame 上游行 = 0。
- 准净土 6 包在阶段 0 门面落地后跟进（先清 secretbox 6 行残留；brownfield 押后至 preview 重建）。

## §6 阶段 2 — 窄接口倒置，攻克 workflow 心脏

（沿用 v2.0 §3 阶段 2 全部内容，补充实测）：

1. workflow 对 db 的调用面 **20+ 方法 + 6 个类型**（AgentWorker/Workspace/ProjectMembership/RecordRevision/KVTxReader 等）——窄接口在新仓重新声明，不复制 db.Store 签名集合；
2. db 45.8% 上游 ⇒ 存储实现是**重写**；`CommitTransitionGuarded`（db/workspaces.go:523 + Tx :590）语义是 claim 持有者校验的批量事务（租约/fencing），不是 revision 乐观锁；
3. 1,737 上游残留集中在 store.go/store_test.go，替换存储层与清零残留是同一件事；
4. **估算维持 2,200–3,200 行**（门面前置后，剩余为 claim/transition 实现收尾 + 并发/过期/重入边界测试）；
5. imbridge 对 workflow 的 6 个类型 import（见 §7）在阶段 2 一并断裂。

## §7 阶段 3 — 部署中心验证切片 + 端点裁剪 + 前端

### 7.1 部署中心切片 = 第一个商用验证标的（v2.1 提案采纳，v3 修正其边界）

文件级事实（实测）：deploy 后端（internal/api/deploy_* + internal/db/deploy_requests* + imbridge/deploy_card_bridge*）
共 6,260 行（含测试），blame = 6,231 gaojiaxiang + 29 未提交，**零上游**；未 import workflow/sandbox/preview。

但**闭包不干净**（判据纪律第 2 条的应用），切片真实边界 = 文件平移 + 四个前置小活：

| # | 前置活 | 实测依据 |
|---|---|---|
| 1 | deploy handlers → imbridge：**只按文件搬 deploy_card_bridge（235 行）**，imbridge 整包 24.9% 上游不随迁 | 全包 blame 实测 |
| 2 | **断裂 imbridge→workflow 的 6 个类型 import**：ApprovalParameterPreview / KindEvidence / KindHumanDecision / KindInheritedContract / ResolutionHumanOptional / ReviewResolutionPreview——审计后挪独立类型包 | grep 实测全部为类型/常量，无行为依赖 |
| 3 | entity 重定义 + db 门面（§4/§5 已排） | §1 实测 |
| 4 | **前端 4 个共享依赖必须重写**（不能直接搬）：use-api.ts（43 root+9 chenhg5）/ api.ts（111 chenhg5）/ Toast.tsx（74 root）/ ConfirmDialog.tsx（136 chenhg5）——ProjectDeployPage **架构可搬、文件不可直接搬** | 逐文件 blame 实测 |

**切片验收**：Task → 部署申请 → 审批（含 IM 卡片）→ 部署执行 → 确定性验证，全链在新仓跑通。

### 7.2 端点裁剪清单（全案最大不确定性，不能跳）

- 注册端点 475 个，前端实际调用 130 个；"45"是 45–60 **待定区间**，不是定数；130→45 = 砍 65% 功能面，是产品级决策，逐一标注 保留/延后/废弃 之后才算数。
- **WebSocket 单列**（agent_channel_ws + events：2,854 上游 / 600 自有）——人机协作平台的实时通道不是 REST 端点，且恰是上游最集中的区域之一。

### 7.3 前端：审计后选择性回收，不是整仓弃写

逐文件 blame 实测（117 文件；自有 = gaojiaxiang，上游 = chenhg5/root/chicken）：

| 档 | 文件 | 行数 | 处置 |
|---|---:|---:|---|
| 纯自有 | **36** | 9,272 | 直接平移（与 v2.1 审计 36 文件完全吻合；行差 116 = 未提交行口径） |
| 混同 | 81 | 41,046 | 其中的自有行（≈2,800）审计后个案回收；**37,000+ 上游行重写才是前端工作量大头**——不要被"只弃 2,830"安慰 |

部署中心前端（*deploy* 路径 2,122 行）是纯自有主力的代表切片；设计闸门/澄清循环等自有 UI 按同法逐文件审计回收。

## §8 阶段 4 — 切换与归档

- 新仓跑通 Task → Workflow → Worktree → Deploy 完整闭环；
- 行为等价测试基线 + 双轨切换方案（v2.0 已补）+ **数据迁移决策（v2.1 新增，v3 保留为显式步骤）**：
  生产库约 578MB，schema 是旧 entity 形状；"抛弃 entity 冗余字段"= schema 变 = 存量数据进不了新仓。
  要么写迁移器，要么明说"新仓不带历史"。**要保住 ias-auth-center / RoomFlow 的状态数据，迁移器不是可选项。**
- 切换后旧仓封存只读。

## §9 工作量账本

| 项 | 估算 |
|---|---:|
| 阶段 0 门面 | 800–1,200（在阶段 2 预算内前置） |
| 阶段 2 存储（含门面） | 2,200–3,200 |
| 阶段 3 后端保留端点 + WS + 基础设施 | 10,000–12,000 |
| 阶段 3 前端 | ~10,000 |
| **合计新写** | **20,000–28,000** |

**工期纪律（v2.1 三条纪律，v3 全文适用）**：⑤端点裁剪清单与⑥存储原型两个前置产出出来之前，**任何工期与人力数字都是猜的，不承诺**。两个前置互相独立，**并行跑**。

## §10 询价决策：算，不要站队（沿用 v2.0 §6）

询价成本 ≈ 0（一封邮件，chg80333@gmail.com）；报价 ≤ 自研总投入 10% → 买（同时覆盖过渡期窗口）；
> $50k → 自研。**无论结论如何询价都该做**。"坚决一分钱不给"是把手段当目的——1,000 美元买几个月合法窗口，是最便宜的时间。

## §11 禁止事项（v2.0 §7 + v3 增补）

沿用：改名式重写 / 隔离规避 / 继续同步上游 / 换 LICENSE / 未出裁剪清单就砍功能 / 跳过等价基线就切换。

**v3 增补**：
- ❌ 未过**依赖闭包**就宣布"零污染切片/零依赖平移"（per-file blame 结论不得外推——A 组与部署中心切片两次案例）；
- ❌ 使用**口径未冻结**的审计数字（作者集合、root 身份、未提交行）做分组或排期决策。

## §12 七步执行序（v3 终版）

| # | 步骤 | 时点 |
|---|---|---|
| 1 | **发询价信**（是否提供商业授权/定价/范围：可否修改、闭源、SaaS 转售） | 今天，≈0 成本 |
| 2 | **停同步上游 + 83 个未提交文件落提交 + 重跑 audit + 旧仓快照存证 + root 口径冻结** | 今天 |
| 3 | **R3 interim 决策（不等法务）+ 法务确认 R3** | 本周 |
| 4 | **数据迁移决策**（迁移器 or 不带历史） | 本周 |
| 5 | **端点级裁剪清单**（130 逐一标注） | ∥ 与 6 并行 |
| 6 | **存储原型**（门面 + RunRepository 最小用例，用真实数字替换估算） | ∥ 与 5 并行 |
| 7 | **阶段 1 修正版开工**（5 包平移 + 领域实体；部署中心切片做验证标的） | 6 出真实数字后 |

## §13 未验证边界

- 我不是律师。PolyForm 在中国法下的可执行性、"内部业务系统使用"认定，需法务确认。
- 本版行数基于当前工作树（**83 个未提交文件**，含本会话的投影/凭据/自愈修复）；步骤 2 存证落提交后需重跑 audit，数字会漂移（方向有利）。
- imbridge/lark 子包未逐文件审（只审了整包与 deploy_card_bridge）；模板资产 .json/.yaml 未 blame；上游是否有明码标价授权页未核实。
- 高自有档的文件数对作者口径敏感（1 vs 9 个文件），冻结口径前不得用于排期。
- 工期未估算——见 §9 纪律。
