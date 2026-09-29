# 提案：任务说明 @ 引用项目资料（上传与意图解耦）

状态：**v3.1 — 用户实测反馈修订，已实现** · 作者：家祥 + ZCode · 2026-09-29
分支基线：`dev` @ `f74e568f`（资料功能 `016fbbfd` 已合并入 origin/dev 并部署 VM；实现提交 `542bca49`）

> **v3.1 交互修订（用户实测四点反馈，2026-09-29）**：
> 1. **popover 光标定位**：@ 触发的选择面板从光标处弹出（镜像 div 测 caret 坐标），下方空间不足时翻转到光标上方——采纳；
> 2. **取消第二步角色选择**：@ 选中即插入标记并完成绑定，默认 `reference` + 非必读（ZCode 模型：意图由说明文字承载，Agent 拿 manifest 路径按需读内容）。角色/必读仍可在 chip 徽标上事后切换（两者解耦）——采纳，§3.1 的"选中后选角色"步骤移除；
> 3. **资料库 @ 引用不进上传组**：chip 区按来源分组——"上传文件"（本次会话回形针上传）与"引用资料"（资料库 @ 引用）分列，消除"已上传过的文件看起来又要传一遍"的误会——采纳，§3.1 的单一 chip 区改为分组；
> 4. **会话内上传后再 @ 选中**：同一 fileId 去重，chip 保留在上传组、角色不动，仅插入文本标记——维持既有去重语义并明确为验收项；
> 5. **文本标记反向同步**（③的连带）：引用组的绑定以文本 `@显示名` 标记为存在依据，用户删除标记即解除绑定（ZCode 式）；上传组 chip 不依赖文本。§3.4 的"移除 chip 同步移除标记"保留，方向补全为双向。

---

> **v3 修订摘要**（相对 v2，来自 glm 评审）：
> 1. **撤回"同一事务"承诺**：任务落库走 taskstore 的 kv 路径（`putJSON → UpsertRecord`），绑定走 `asset_attachments` SQL 表——两个存储路径无共享事务。改为诚实的三段式：**纯读预校验全部 fileId → AddTask → 逐条插绑定，任一失败补偿删除任务后返 400**。"任务不产生"的准确表述是"产生了再删"；
> 2. **补上第三条泄漏路径（P1②）**：workflow 模板任务的 `StartRunWithInput` 在创建请求内同步执行（write.go:648-666），且 attention 唤醒（:674-681）在 201 响应写出**之前**就已发出——绑定代码必须插在 write.go:648 的 workflow 分支**之前**（即 AddTask 完成后立即插绑），只挡在 autoStart（:668）之前对 workflow 任务无效。UI 的模板分支正走这条路；
> 3. **P3 措辞修正**：node 分发路径的 `taskSpecAssets` 本身 fail-closed（读失败/超限拒绝 enqueue）；竞态下的空列表是"绑定行尚不存在，查询合法返回空"，不是路径吞错。已改正表述；
> 4. **P2 记录在案**：8 上限是 check-then-insert 非原子，但 SQLite 单写锁 + 新任务 ID 外人未知，实际风险趋零，接受不修。

> **v2 修订摘要**（相对 v1，来自 GPT 评审）：
> 1. 新增高优先级风险 R0——前端"创建任务后才逐条绑定"的时序问题，方案改为**创建请求内原子先绑**；
> 2. 输入角色收敛为 `requirement_input` / `reference` 两种，`deliverable` 移出选择（其语义是 Agent 产出回写，phase 2）；
> 3. `required` 与角色解耦；**前端显式传 role + required 两字段，后端默认值逻辑零改动**；
> 4. chip 与文本标记的同步规则明确化；
> 5. IME/光标/候选信息等交互细节补入；文案覆盖全部四种语言（en/ja/zh-CN/zh-TW）。

---

## 1. 项目背景

**Multigent** 是面向团队的人机协作 Agent 操作系统：团队把 Prompt、工具、工作流和人工审核整合为一个协调系统。Agent 在 Docker 沙箱中执行任务，通过 `mga` CLI 与宿主控制台交互；人通过 React 控制台（`web/`）设计工作流、派发任务、审核结果。

**已交付的资料功能（本次提案的地基）**：内容寻址的项目级附件体系，已于 2026-09-29 实现并部署验收。核心模型为三张表：

- `asset_blobs`：字节本体，身份 = SHA-256，相同内容全库只存一份；
- `asset_files`：项目资料库的"文件"条目（显示名 + 指向当前 blob 的指针）；
- `asset_attachments`：文件与任务的绑定关系，带**角色**字段（`internal/db/assets.go:76-83`）。

任务运行时的契约：绑定文件经 fail-closed staging 管线落到容器只读挂载 `/mnt/multigent/assets`，并在 Agent 的 prompt 里渲染一份 manifest（每行：`[required|reference] 显示名 → 容器路径 (sha256:…, 大小, MIME)`）。Agent 按路径按需读取，内容不进 prompt。两条执行拓扑（本机直跑 / runtime-node 分发）都在 spec/启动时从控制库解析绑定（`internal/runner/assets_stage.go:78`、`internal/api/runtime_node_handlers.go:1282`）。两条路径对**错误**都 fail-closed（读失败/超 8 个会拒绝启动或 enqueue），但对**竞态下的合法空结果**（绑定行尚不存在）无从设防——这正是 R0 的本质。

**当前的问题——上传与意图之间有空隙**：

1. **角色硬编码**。新建任务弹窗里所有上传的文件创建任务时一律绑定 `role: 'requirement_input', required: true`（`web/src/components/project/CreateTaskDialog.tsx:246`）。用户传一个网站 Icon，Agent 眼里它和需求文档是同一种东西。**意图通道存在（后端支持多角色 + manifest 渲染角色标签），只是 UI 没让用户表达。**
2. **上传与任务说明物理割裂**。上传入口是输入框下方的独立回形针按钮；"上传了这个文件"与"这个文件在任务里扮演什么"之间没有文本关联。换项目弹窗直接清空已传文件，下次还要重传。
3. **复用为零**。每次任务都重传同一批静态资源（Icon、字体、品牌规范），尽管资料库（ProjectAssetsPage）已经存在。

## 2. 目标与非目标

**目标**

1. 在新建任务的**任务说明（prompt）输入框**内支持 `@` 触发文件选择面板，列出**项目资料库**中现有文件，选中即完成绑定；
2. 选中文件时选择**输入角色**：`requirement_input`（需求输入）或 `reference`（参考），并独立设置**必读**开关；
3. 回形针上传保留为 @ 面板内的次要入口（"选不到就现传现引"），主流程是引用资料库；
4. 绑定以后端数据为准（fileId + role + required 调既有绑定 API），文本中的 `@显示名` 仅为用户可读标记，**不做文本解析**。

**非目标（明确不做）**

- 不做 ZCode/Codex 式的斜杠命令系统——8 个附件的规模用不着；
- 不做全文搜索、置顶、预览、文件夹分类——@ 面板只做文件名过滤；
- 不新增 API 路由——既有 9 条资料路由 + 绑定接口现成的 `role`/`required` 字段够用（`assets_handlers.go:324-329`）；
- 不改 Agent 侧 manifest 契约，**Agent 零适配**；
- 不做"反例"新角色——用 `reference` + 用户在说明文字里写明"这是反例，不要照抄"；
- 不引入跨存储事务机制——见 3.5 的补偿式设计。

## 3. 方案设计

### 3.1 交互流

```
用户在 prompt 输入框键入 "@"
  → 弹出轻量 popover（列项目资料库文件，按文件名子串过滤，键盘上下选择）
  → 选中文件 → 行内选择角色（需求输入 / 参考）+ "必读"开关
  → 确认后：
      a) 文本光标处插入 ` @显示名 `（纯显示标记，见 3.4 同步规则）
      b) 弹窗底部 chip 区新增一枚带角色徽标的文件 chip（可移除）
      c) 组件状态记录 { fileId, role, required } 三元组
  → 提交时：创建请求体内携带 assets 字段，服务端在任务启动前完成绑定（见 3.5）
```

- popover 数据源：`GET /api/v1/projects/{name}/assets`（**不带** `includeArchived`，已归档文件不可引用；后端绑定接口对归档文件同样拒绝，`assets_handlers.go:353-356`）；
- 过滤：纯前端文件名子串匹配（`displayName`），不做后端搜索；
- 额度：@ 面板顶部实时显示"还可绑定 N 个"（上限 8，`MaxTaskAttachments`；后端也会拒，双保险）；
- 与回形针共存：回形针上传的文件进 chip 区，角色默认"参考 + 必读关"，可点徽标改；@ 引用与上传收敛为同一种 chip 表示、同一套绑定逻辑；
- 候选信息：每行显示文件类型图标 + 大小 + SHA 前 8 位，避免同名文件误选（同名不同 blob 在资料库中是两个 file 条目，靠 SHA 前缀区分）。

### 3.2 数据流与状态

组件状态从 `UploadedAsset[]` 扩展为：

```ts
type PendingBinding = {
  fileId: string
  displayName: string
  role: 'requirement_input' | 'reference'
  required: boolean
  source: 'upload' | 'library'   // chip 徽标区分来源（信息展示，不影响绑定语义）
}
```

- **前端显式传全两个字段**：绑定请求永远携带 `role` 与 `required`，不依赖后端任何默认值推导。核实结论：绑定接口对空 role 默认 `reference`、`required` 按零值取 `false`（`assets_handlers.go:361-364`、`:384`），并无按角色推导默认值的逻辑；
- **去重**：同一 fileId 重复 @ 选中 → 更新既有 chip 的角色/必读而不是新增；
- **项目切换**：清空 pending 绑定（沿用现有 `onProjectChange` 行为）；
- **角色默认值**：`requirement_input` → required 默认 true；`reference` → required 默认 false。均为前端初始值，用户可改。

### 3.3 角色语义边界（v2 收敛，维持）

**输入侧角色只提供两种**：

| 角色 | required 默认 | 含义 | Agent manifest 标签 |
|---|---|---|---|
| requirement_input | true | 任务的需求/规格本体 | `[required]` |
| reference | false | 参考资料/样例/反例/交付格式样例 | `[reference]` |

- **`deliverable` 移出输入选择**。代码注释明确其语义是"Agent 产出发布回项目资料时的角色（phase 2）"（`internal/db/assets.go:77-78`），需求文档或参考样例不是"交付物"。若要表达"这是交付格式样例"，用 `reference` + 说明文字。
- **反例表达**：`reference` + 说明文字写明"这是反例，不要照抄"。
- **required 与角色解耦**：参考资料也可能是必读的，需求资料也可能只是补充材料——UI 独立提供"必读"开关，角色只决定默认值。
- 已知近似：manifest 标签只区分 required 与否（`internal/assets/assets.go:298-313`），更细的语义区分靠说明文字。不改 manifest 格式。

### 3.4 chip 与文本标记的同步规则

绑定事实源是 **chip + 后端 DB**，文本中的 `@显示名` 是快照式显示标记。同步规则：

1. **移除 chip → 同步移除该选择器插入的 `@显示名` 标记**（按插入时记录的位置/文本匹配），避免"删了引用却仍在绑定"的误解；
2. **用户手工改写/删除文本标记 → 不悄悄解除绑定**（不做反向解析，chip 不动）；任务说明里手写的、与文件同名的文字永远不会触发绑定；
3. **文件改名后旧标记失真**：接受——标记是插入时刻的快照；manifest 给 Agent 的路径由 staging 管线按绑定时的 SHA 生成，不受显示名漂移影响。

### 3.5 绑定时序（v3 关键修正：创建请求内先绑，三路泄漏一次堵死）

**R0 的完整形态（三轮评审后确认，共三条泄漏路径，全部在创建请求的 201 响应之前）**：

| # | 启动源 | 位置 | 说明 |
|---|---|---|---|
| L1 | attention 唤醒 | `write.go:674-681` | assignee 含 "/" 时，创建请求内同步记录信号并请求唤醒，**早于响应写出**（:683） |
| L2 | workflow 同步启动 | `write.go:648-666` | `workflowID != ""` 时 `StartRunWithInput` 在创建请求内同步建 run；首步 agent 节点经 reconcile（`scheduler_manager.go:364`）把任务移交给 agent——UI 的模板分支（`CreateTaskDialog.tsx:389-395, 414-415` 带 `workflowDefinitionId`）正走这条路 |
| L3 | autoStart 直启 | `write.go:668-673` | `body.AutoStart` 为 true 时请求内直启（当前 UI 建任务不传，但 API 层面存在） |

**结论：客户端"创建成功后再绑"永远来不及——唤醒在响应之前已发出。绑定必须进创建请求本身。**

**v3 方案：创建请求体新增可选 `assets` 字段（方案 A 的落地形态）**

```
createTaskBody 增加可选字段：
  Assets []{ FileID string; Role string; Required bool } `json:"assets,omitempty"`

处理顺序（write.go，全部在任何启动源之前）：
  1) 纯读预校验（不改任何状态）：逐个 fileId 校验存在 / 属于本项目与本 workspace /
     未归档 / 角色合法（ValidAssetRole）/ 总数 ≤ 8（含本次全部 assets）。
     任何一条不过 → 400，此刻任务尚不存在，零残留。
  2) 现有 AddTask / AddToInbox 逻辑不变（write.go:619-646）。
  3) AddTask 成功后、workflow 分支（:648）之前：逐条 InsertAssetAttachment。
  4) 任一条插入失败 → 补偿：删除已插成功的绑定行 + 删除任务（DeleteTask）
     + 若已加 inbox 则一并移除 → 400。任务对外的最终表现是"创建失败"。
  5) 全部成功 → 流程继续走现有 L1/L2/L3 三个启动源（此时 manifest 渲染与
     staging 读到的绑定是完整的）。
```

**诚实的原子性表述（v2 的"同一事务"作废）**：任务落库走 taskstore 的 kv 路径（`DBStore.AddTask → putJSON → UpsertRecord`，`dbstore.go:31-49, 813-818`），绑定走 `asset_attachments` SQL 表（`InsertAssetAttachment`）——两个存储路径之间**没有共享事务**，本方案不引入事务机制。实际语义是**"预校验前移 + 失败补偿删除"**：校验阶段挡住绝大多数失败（文件不存在/跨项目/已归档/超限/非法角色），真正可能走到补偿的只剩插入期间的 IO 错误，而补偿路径把对外表现收敛为"任务不存在"。窗内极端情况（补偿删除自身失败）会留下孤儿任务，属于可接受的低概率残留——补偿逻辑必须写，且要打错误日志。

**P2（接受不修）**：现有绑定接口与新建路径的 8 上限都是 check-then-insert 非原子。SQLite 单写锁 + 新任务 ID 在创建响应前外人不可知，实际风险趋零。文档记录，不改代码。

**fail-closed 语义**：创建请求携带 assets 时，预校验失败 → 400、任务不存在；插入失败 → 补偿删除后 400、任务不存在；成功 → 任务照常启动且绑定完整。回形针上传失败 → 阻断提交并报错（文件入库是绑定的前置，不再"toast 后继续"）。

### 3.6 @ 触发与 IME 细节（验收级）

- 触发：光标前一字符为 `@` 且其前为行首/空白（避免命中邮箱中间态）；触发后持续跟踪 `@` 之后的查询串直到空白/删除 `@`；
- IME：compositionstart/compositionend 期间不触发选择、不插入标记（中文输入法下 `@` 常与候选窗共存）；
- 光标：popover 打开时方向键在候选间移动，`Enter` 选中，`Esc` 关闭；焦点始终留在 textarea；
- 选中文本替换：插入标记前若 textarea 有选区，先折叠光标到选区终点再插入；
- 空资料库：面板显示"资料库为空，去资料页上传或直接点回形针"。

### 3.7 前端改动面

| 文件 | 改动 |
|---|---|
| `web/src/components/project/CreateTaskDialog.tsx` | @ popover、角色+必读选择、chip 区改造（角色徽标 + 来源徽标）、`PendingBinding` 状态、提交时携带 `assets` 字段 |
| `web/src/components/project/AssetMentionPopover.tsx`（新） | 纯展示组件：文件列表 + 过滤 + 键盘导航 |
| `web/src/locales/{en,ja,zh-CN,zh-TW}/common.json` | @ 面板与角色/必读文案，四种语言全补 |
| `internal/api/write.go` | createTaskBody 增加可选 `assets`；:646 与 :648 之间插入预校验 + 绑定 + 补偿（约 50 行） |
| `internal/api/write_test.go`（或就近测试文件） | 原子绑定成功/各失败分支用例（含补偿删除断言） |

## 4. 验证计划

1. **单测（后端）**：创建请求带 assets 的成功路径与各失败分支（文件不存在/跨项目/已归档/超 8 个/非法角色/插入失败触发补偿）——断言失败时**任务已被补偿删除**（查询不到）且 inbox 无残留；成功时绑定行完整且角色正确；
2. **构建**：`make web` + `make test`；
3. **部署后浏览器验收**（VM，操作真实 UI）：
   - 资料库先上传 2 个文件（一个 .md 需求、一个 .png Icon）；
   - @ 引用 .md 选"需求输入+必读"、@ 引用 .png 选"参考"→ 创建任务 → `GET …/tasks/{id}/assets` 断言两条绑定 role/required 正确；
   - 任务跑起来后查看 prompt manifest：`.md` 行带 `[required]`，`.png` 行带 `[reference]`；
   - 回形针上传的文件默认"参考"且可改角色/必读；
   - **时序断言（v3 扩充：两条路径都测）**：① 空白任务带 assignee 创建（走 L1 唤醒路径）→ run 的 prompt manifest 必须含全部绑定文件；② **workflow 模板任务创建（走 L2 同步启动路径）→ 首步 agent run 的 manifest 同样必须完整**——这是 v2 漏测的路径；
   - 边界：@ 引用已归档文件（面板不可见）、绑定满 8 个后第 9 个（前后端都拦）、IME 输入中键入 @ 不触发面板、移除 chip 后文本标记同步消失、**创建请求携带非法 fileId → 400 且任务不存在（补偿验证）**。

## 5. 风险与开放问题

| # | 风险/问题 | 处置 |
|---|---|---|
| R0 | ~~创建后才绑定的启动竞态~~ | **v3 已修复**：绑定进创建请求，插在全部三条泄漏路径（L1/L2/L3）之前；失败补偿删除 |
| R0' | 补偿删除自身失败的孤儿任务 | 接受：预校验前移后仅剩 IO 错误可触发；补偿失败打错误日志，人工清理 |
| R1 | 文本 `@显示名` 与实际绑定漂移 | 接受并定义同步规则（3.4） |
| R2 | 角色语义滥用 | 角色收敛为两种输入角色（3.3） |
| R3 | 与另一协作线程同改 `CreateTaskDialog.tsx` / `write.go` / locales 冲突 | 开工前 `git fetch` 确认基线 |
| P2 | 8 上限 check-then-insert 非原子 | 接受：SQLite 单写锁 + 新任务 ID 创建响应前外人不可知，风险趋零；记录不改 |
| Q1 | @ 是否也在 description 框生效 | **v1 只做 prompt 框**——绑定语义属于 prompt/执行域。评审时定 |
| Q2 | `assets` 字段是否兼容"创建后补绑" | 现有逐条绑定接口保留不动：创建时原子绑（新）、创建后补绑（旧，用于已有任务） |

## 6. 改动量估算

后端 ~60 行（write.go 预校验 + 绑定 + 补偿）+ 单测；前端 ~300 行（两个组件 + 四语言文案）。无迁移、无新路由、无 Agent 侧适配、无跨存储事务机制。

## 附 A：外部评审（GPT）意见采纳记录

| 评审意见 | 核实结论 | 采纳 |
|---|---|---|
| 绑定时序竞态（R0，高优先级） | 属实；创建请求不传 autoStart 但 assignee 唤醒是异步的、间隔无契约 | ✅ 采纳，升级为创建请求内先绑（v3 进一步覆盖三路泄漏） |
| deliverable 不应作为输入角色 | 属实：代码注释明确其语义是 phase 2 的产出回写（assets.go:77-78） | ✅ 采纳，输入侧收敛两种 |
| required 与角色解耦；后端无按角色推导的默认值 | 属实：空 role 默认 reference、required 取零值 false（assets_handlers.go:361-364,384） | ✅ 采纳：前端显式传两字段，后端默认值逻辑零改动 |
| chip/文本标记同步规则 | 合理 | ✅ 采纳（3.4） |
| IME/光标/候选信息/四语言 | 合理（locales 实为四份） | ✅ 采纳（3.6/3.7） |

## 附 B：外部评审（glm）意见采纳记录

| 评审意见 | 核实结论 | 采纳 |
|---|---|---|
| P1① "同一事务"是空头支票 | **属实**：任务落库走 `DBStore.putJSON → UpsertRecord`（kv 路径，dbstore.go:31-49,813-818），绑定走 `InsertAssetAttachment`（asset_attachments 表），无共享事务 | ✅ 采纳：改为"纯读预校验前移 + AddTask + 逐条插入 + 失败补偿删除"，文档明确"任务不产生"实为"产生了再删"（3.5） |
| P1② workflow 同步启动是第三条泄漏路径 | **属实**：`StartRunWithInput` 在创建请求内同步执行（write.go:648-666），attention 唤醒 :674-681 亦在 201 响应前发出；UI 模板分支带 workflowDefinitionId 正走此路 | ✅ 采纳：绑定插在 write.go:648 之前（AddTask 后立即）；验证计划补 workflow 创建路径的时序断言（§4.3） |
| P2 8 上限 check-then-insert 非原子 | 属实（assets_handlers.go:365-373）；风险趋零判断合理 | ✅ 记录在案，不修（3.5 / §5） |
| P3 "静默得到空列表"措辞不当 | **属实**：taskSpecAssets fail-closed 注释明确（runtime_node_handlers.go:1277-1288）；竞态空结果是合法查询结果非吞错 | ✅ 采纳：§1 与 R0 表述已改正 |
| 方案 B（客户端兜底）追不上三条泄漏路径 | 与核实结论一致 | ✅ 已从方案中移除方案 B，只保留方案 A |
