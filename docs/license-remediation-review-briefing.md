# 许可证处置·外部评审简报（自包含，供无上下文 agent 审阅）

> 2026-10-07 · 写给外部评审者：本文档假设你**对该项目零上下文**。所有背景、术语、决策、证据、待裁决问题都在文中。
> 评审对象：Multigent 许可证处置（license remediation）计划与截至 2026-10-07 的执行成果。
> 你要做的：核实证据是否真实（§6 有复核命令）、判断三个待裁决问题（§5）的方案取舍是否合理、指出被遗漏的风险。

---

## 1. 背景一页纸（为什么要做这件事）

Multigent 是一个 Go + React 的 Agent 操作系统（旧仓：`/Users/imac/Documents/code/github/multigent`，module `github.com/multigent/multigent`）。代码审计发现仓库里混有上游作者（chenhg5、root 初始导入）的代码行，而项目许可证是 **PolyForm noncommercial**。团队（用户 + AI agent 协作开发）决定：**不买商业许可，自研替换**——把自有代码平移/重写进一个全新的仓库，使新仓每一行代码的 git blame 都是自己的（"blame 上游行 = 0" 是全案的核心不变量）。

**红线（法律与技术双重要求）**：
- 不做"改名式重写"（法律上改名不洗白版权，技术上继承旧设计债）——只做"行为兼容的重新实现"：输入是行为规格与测试基线，输出是按我们的标准重新写的代码。
- 审计数字是快照：上游作者在审计后仍在旧仓叠提交，每批开工前必须重测。
- 新仓 git 历史从第一个 commit 起必须纯净：文件复制平移，绝不导入旧 git 历史。

## 2. 已拍板的决策（全部有据可查，按时间线）

| 决策 | 内容 | 出处 |
|---|---|---|
| 询价跳过 | 不买 PolyForm 商业许可，直接自研 | 2026-10-06，v3 文档头部裁决块 |
| A1 违规暴露缓解 | ias-auth-center / RoomFlow 均为本机测试项目、从未承载真实业务 → 无需降级，随旧仓封存 | 2026-10-07 用户裁决 |
| A2 数据迁移 | 新仓**不带历史数据**、不写迁移器；旧库随旧仓封存只读 | 2026-10-07 用户裁决 |
| B2 功能裁剪 | 页面级裁决：一期保留（连接/设置/知识库/工作流/项目/工作台/智能体/任务链路/部署中心/资料库/文件/技能库/登录成员）；二期（协作方案/消息重新设计/审计页）；**两层 OKR + 里程碑全去除**（含存储实体）；站内信人→agent 发送砍掉 | 2026-10-07 用户裁决 |
| 仓库形态 | **单一 monorepo**（`/Users/imac/Documents/code/github/multigent-v2`），不拆多仓 | 2026-10-07 用户采纳建议 |
| 验证节奏 | **一批次执行完 → 立即三道闸验证**（版权闸/技术闸/等价闸），全过才进下一批 | 2026-10-07 用户采纳建议 |
| 测试纪律 | 平移包既有测试原样随迁必须全绿；新代码覆盖不低于被替换面；不许删改断言"变绿" | 用户点名强调 |
| brownfield 包 | 押后至 preview 重建（实测其依赖 preview 包三个类型，与 v3 弱环标注一致） | 2026-10-07 确认 |

## 3. 已完成工作与证据（截至 2026-10-07）

执行主体：主控 agent 编排 + overnight-runner 子 agent 执行 + 主控逐批实地复核（不采信子 agent 自报）。

| 批次 | 内容 | 交付 | 验证 |
|---|---|---|---|
| S0 存证 | 全仓审计重测 + root 口径冻结 + tag `license-audit-baseline` | 1,273 文件实测：自有 41.8%；与 v3 §1 对拍零漂移；发现 DEV-1（secretbox 上游实为 161 行非 6 行） | 旧仓 work/license-audit-20261006/ 四份证据文档 |
| B1 审计工具 | tools/license-audit/{blame-authors, closure-scan, verify-clean, endpoint-crosswalk} | 4 个 Go 工具 + 对拍测试（已知污染包 vs 已知干净包） | 旧仓 go test ./tools/... 绿 |
| B2 端点底稿 | 463 注册端点 vs 316 前端字面量交叉 | 四档：前端调用 309 / 零调用 74 / 平台内部 74 / websocket 6；页面级裁决已投影为端点级裁剪草稿 | endpoint-crosswalk.md + trim-list-draft.md |
| B3 存储原型 | internal/store/kvfacade（KV CRUD+修订守卫+claim 语义） | 827 行 + RunRepository 15 方法 + -race 并发测试 | go test ./internal/store/... 绿 |
| C1 净土平移 | gitworktree/codehost/projecttemplate/doctor/ciready 5 包（9,031 行）直接平移 | 新仓 8 commits；模板资产 87 文件/8,106 行单独盘点 | 主控复核：作者唯一、build/vet/test 5 包绿、逐包 diff（剔 import 行）=0 |
| C1.5 准净土 | kvfacade 平移 + **secretbox 行为兼容重写**（上游 161 行，唯一重写件）+ assets/fixturesandbox/changerun/previewreceipt 4 包接门面 | 新仓 6 commits；4 个最小接口契约（TaskAttachmentSource/RecordStore/ProposalStore/ReceiptStore）；kvfacade 本体零改动 | 主控复核：11/11 包 test -count=1 绿、secretbox 旧格式向量 10 测试 PASS、BlobStoreRoot env 链等价 PASS |
| D 批前置 | workflow 包依赖面提取 | 12 个存储方法清单（11 个 kvfacade 已备）+ 13 个 entity 类型清单 + 评审问题（§5） | docs/d-batch-interface-review.md |

**新仓现状（multigent-v2，12+ commits）**：11 包 / 约 21,000 行，全部测试绿，git log 作者唯一（gaojiaxiang），零上游行。

**关键文档**（均在旧仓）：
- `docs/license-remediation-final-v3.md` — 审计终裁（上位依据）
- `docs/license-remediation-execution-plan.md` — 施工图（批次+STOP 点+§0.5 已裁决条款）
- `docs/rebuild-page-priority-plan.md` — 页面重建优先级方案（含消息页面实测：40,042 条消息中 97.5% 是 agent 心跳机器信令，人读仅 1.5%）
- `docs/d-batch-interface-review.md` — D 批接口评审底稿
- `work/license-audit-20261006/` — 审计证据区（audit-full.json / closure-full.json / endpoint-crosswalk.md / trim-list-draft.md）

## 4. 执行纪律（如何防止执行走样）

1. **STOP 点显式化**：需要人裁决的地方（A1/A2/B2 裁剪/D 批接口评审/C2 schema 定稿）在施工图标 `⏸ STOP`，agent 执行到点必须停，夜间自主执行绝不越过。
2. **批次可回滚**：一批 = 一组 commit，失败回滚不连带。
3. **每批三道闸**：版权闸（verify-clean：blame 上游行=0）→ 技术闸（build/vet/test 全绿）→ 等价闸（与旧仓行为基线对比）。
4. **主控复核独立于执行 agent**：执行 agent 的自报不作为验收证据，主控用命令实地重跑。
5. **发现预扫外依赖即刹车**：C1.5 中 brownfield 因发现 preview 依赖被 agent 主动刹停，押后处置——纪律被真实执行过一次。

## 5. 三个待裁决问题（D 批接口评审 STOP 点，需要评审意见）

### 问题 1：workflow 的存储依赖，用"接口"还是"具体类型"？

**背景**：新仓 workflow 包（17,749 行，其中 15,044 行自有）需要读写存储。旧仓直接 import 一个巨大的 `db.Store` 接口（1,073+ 行定义，上游污染重）。新仓有两个选择：

- **方案 A（推荐）**：在新仓声明一个窄接口 `WorkflowStore`，只含 workflow 实际用到的 **12 个方法**（UpsertRecord/GetRecord/ListRecords/修订守卫写入等，逐个 grep 实测过）。已验证现有 kvfacade（存储原型）满足其中 11 个，只差 1 个（ListProjectMemberships，预计 <100 行补齐）。
- **方案 B**：workflow 直接 import kvfacade 的具体类型 `Facade`。省一次接口声明，但 workflow 从此锁死在这个存储实现上——将来换存储/做测试桩都要动 workflow 本体。

**推荐理由**：窄接口是 Go 惯例（"接受接口，返回具体类型"），成本只是 12 行声明；收益是 workflow 与存储解耦，测试可以喂内存桩。方案 B 省的那点事不值得。

### 问题 2：13 个 Workflow 实体类型，"按使用面重定义"还是"平移上游定义"？

**背景**：workflow 用到 13 个实体类型（WorkflowField/Step/Run/Edge/Definition/StepInstance/Position/ActorBinding/Template/Branch/BranchInstance/StepEvent/EdgeCondition，含 5 个 ID 构造器）。这些类型的定义目前在旧仓 `internal/entity/workflow.go`——218 行里 **197 行是上游代码**。把它们原样复制进新仓 = 把上游版权行带进新仓，直接违反"blame 上游行=0"不变量。

- **方案 A（推荐）**：按真实使用面重定义——逐类型 grep workflow 包实际访问了哪些字段，只保留用到的；未用字段（尤其指向已裁剪域如 OKR/Milestone 的引用）不进新 schema。漏字段的风险由编译器兜底（workflow 一编译就会报 undefined field）。
- **方案 B**：平移原文件再"洗"作者信息。**明确排除**——这属于改名式重写红线。

**推荐理由**：A2 已裁决不带历史数据，schema 没有存量兼容约束，正好按真实使用面设计干净 schema；编译器使漏字段风险可控。

### 问题 3：workflow 的行为等价测试基线，怎么移植？

**背景**：D 批要证明"新仓 workflow 与旧仓行为等价"。旧仓 `store_test.go` 是现成基线，但它 2,705 行上游行全部集中在 store.go + store_test.go——测试文件本身也混有上游代码段。

- **方案 A（推荐）**：store_test.go 中**自有作者写的断言**原样移植为新仓测试；上游段的测试不迁，其语义背书由 kvfacade 自己的测试（含 -race 并发测试）承担。
- **方案 B**：测试文件全量平移。会带上游行进新仓，破坏版权不变量——**不可行**。
- **方案 C**：完全不迁测试，等价性全靠 kvfacade 测试。版权干净但等价背书弱一档（workflow 业务语义层的测试缺失）。

**推荐理由**：A 是版权与等价性之间的可行平衡点。风险点（评审者请重点审）：自有断言与上游段的**逐行归属判定**是否可靠——工具链已有 blame-authors 可逐行标注，但边界行（如自有代码引用上游 helper）需要人抽检。

## 6. 评审者的复核方法（验证证据真实性）

```bash
cd /Users/imac/Documents/code/github/multigent-v2
git log --format='%an %ae' | sort -u          # 应只输出 gaojiaxiang 一行
go build ./... && go vet ./...                 # 应零输出
go test -count=1 ./...                         # 应 11 包全 ok（-count=1 禁缓存）
go test -count=1 -v ./internal/secretbox/      # 重点看 TestLegacySealedValuesRemainOpenable

# 旧仓五包与新仓逐字节对比（除 import 路径）：
for pkg in gitworktree codehost projecttemplate doctor ciready assets changerun fixturesandbox previewreceipt; do
  n=$(diff -r /Users/imac/Documents/code/github/multigent/internal/$pkg \
             /Users/imac/Documents/code/github/multigent-v2/internal/$pkg 2>/dev/null \
      | grep -E '^[<>]' | grep -v 'multigent/internal\|multigent-v2/internal' | wc -l)
  echo "$pkg: $n"
done   # assets/changerun/fixturesandbox/previewreceipt 有预期差异（接口契约替换+注释，见 §3）；五净土包应为 0

# 审计证据：
python3 -c "import json; d=json.load(open('/Users/imac/Documents/code/github/multigent/work/license-audit-20261006/audit-full.json')); print(d['totals'])"
```

**抽检建议**：① secretbox 重写的兼容向量测试是否真的用"旧算法生成的字节"解密（读 `internal/secretbox` 测试源码）；② C1.5 四包的接口契约文件（contract.go 等）是否真的最小、没有偷偷抄上游 db 包实现；③ trim-list-draft 的投影逻辑抽 3 个端点核对页面归属。

## 7. 诚实边界（评审者应知道的未验证/未做项）

1. **B3 是原型**：kvfacade 尚未承接旧仓 workflow 的真实负载（那是 D 批的事）；RunRepository 15 方法是"实测调用面"但非全部生产路径。
2. **新仓未 push**：全部成果在本地（用户保留 push 决定权）；无远端备份，单盘故障会丢工作。
3. **brownfield 未处置**：随 preview 重建路径，preview 本身也是大件（v3 §3 弱环），排在 E/F 批附近。
4. **消息页面重设计**、**C2 schema 字段级清单**、**D 批 store_test 归属抽检**——都是"方案已定、实现未动"状态。
5. **法务边界**：执行者不是律师；"行为兼容重写 + blame=0"是工程界的干净做法，但不构成法律意见。
6. **DEV-1 教训未终结**：上游作者仍可能继续在旧仓叠提交，每批切换前 verify-clean 重测的纪律必须保持到 G 批切换完成。
