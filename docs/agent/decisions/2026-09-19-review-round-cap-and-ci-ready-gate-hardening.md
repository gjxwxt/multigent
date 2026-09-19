# 评审轮数封顶强制执行与 ci_ready 闸门硬化

## 背景事实（本轮实测，非推断）

1. **"三轮封顶"此前不封顶**：`maxReviewRounds = 3`（`internal/workflow/store.go:3163`）只在返工边确定性自增（`:3219-3227`），出边条件只看 `self_review_verdict`（`:1047-1048`）。达 3 轮后模型继续报 `issues_fixed` 时，循环照常进行，而计数被 clamp 在 3——即"上报值显示已到顶、行为上仍在空转"。
2. **引擎无法用模板条件表达轮数上限**：`compareWorkflowValue` 仅支持 `eq / neq / exists / in`（`store.go:3137-3159`），无数值比较。因此"≥3 时走 escalate"不可能靠声明式出边实现。
3. **`ci_ready` 是两层混合闸门**：
   - 第一层：13 项纯静态文件检查（`internal/ciready/ciready.go`：lockfile / `.gitlab-ci.yml` 可解析 / runner tags / gradlew / compose healthcheck …），不发网络请求；
   - 第二层：`pipeline_evidence`（`internal/api/ci_ready_handlers.go:145-165`）**观察**当前 HEAD 的 GitLab 流水线是否 `terminal && status == success`，不触发流水线；
   - 且远端未绑定又未开 `MULTIGENT_CI_REMOTE_PIPELINE_REQUIRED` 时该项检查**完全不追加**（`localOnlySkip`），此时 `overall: ready` 的含义是"没查"，不是"查过且通过"。
4. **`ci_ready` 此前不参与路由**：`OverallNotReady` 只出现在上述两个文件中，工作流引擎不消费；该步骤类型是 `agent_task`（`store.go:1022`），所以 `mga task step done` 无论 `not_ready` 与否都能推进。所谓"确定性闸门"实际是提示词约束。

## Decision

1. **轮数上限由引擎强制执行，不靠模板、不靠模型自觉**：评审步骤提交 `issues_fixed` 且确定性计数已达上限时，服务端把**路由**改写为 `escalate`，并在案卷中留下可解释痕迹（"平台按轮数上限强制升级，agent 原判为 issues_fixed"）。agent 的原始判断不被篡改、不被隐藏，只是不再被允许决定路径。
2. **人工打回必须归因二分，两个计数器分开**：
   - `rework`（验收标准不变，修实现）→ 重置 agent 侧 `review_rounds`，回到 `implement`；
   - `rescope`（方向/范围错、超出批准范围、画蛇添足）→ 回到需求澄清节点，**不消耗也不重置**任何评审轮数；
   - 新增单调递增、永不重置的 `human_interventions`，只统计真人介入次数。第二次、第三次人工打回通常意味着契约错了而不是代码错了，必须能被独立看见。
3. **升级案卷由服务端拼装，禁止模型在升级那一刻现写总结**：拼接每轮的路由词元、结构化 `escalation_case` 条目、以及每轮的 `base..head` diff 区间（依赖 `GET /api/v1/projects/{name}/tasks/{taskId}/diff`）。
4. **严重度词表复用既有 `risk_level: high|medium|low`**（QA 准出闸门已在严格校验该枚举），不引入 P0..P4 第二套分类。
5. **`ci_ready` 结果参与路由，且失败原因三分类处置**：服务端在步骤完成路径上**自己复算**（不信任 agent 上报），按原因分流：
   - `repairable`（仓库形状不合规：缺 lockfile、yml 非法、tags 不匹配）→ 确定性返工给研发 agent，附逐项修复指引；
   - `waiting_pipeline`（HEAD 的流水线仍在运行或尚未触发）→ 任务停在闸门并保持"等待中"，**不打扰人工**，由后续轮询/webhook 重新校验；
   - `ci_failed`（流水线 terminal 非 success）→ 先返工；同一失败项连续 2 轮仍在 → 才 park 给人并通知审批人。
6. **人类不是重试按钮**：只有真人独有判断（范围、方向、风险接受）流向人；可自动修复与仅需等待的一律由引擎处置。

## 实现期修正（编码时发现，已按此落地）

1. **三分流实际是四分流**：`环境型` 需要独立成一类（`gateCauseEnvironment`：远端未绑定 / 凭据缺失）。把它归进 `repairable` 会让 agent 对着"只有管理员能修"的问题反复重试自己的步骤——正是本项目要避免的静默空转。该类判定携带 `needsHuman: true`。
2. **闸门只判定、不修复**：`ciready.Ensure` 会**补种缺失的 CI 基线文件**（写仓库），而 `ci_ready` 步骤完成时的服务端复算绝不能写工作区（既有红线："平台进程不碰仓库工作区"，契约落盘归沙箱内的 agent）。因此 init 端点继续 `Ensure`，闸门走 `ciready.Verify`；`ciReadyOptions.seed` 就是这个分界，二者共用同一套判定与证据装配代码，避免"两套校验"。
3. **闸门识别必须兼容存量实例化模板**：只认 `step.Config["platform_gate"]` 的话，数据库里已有的工作流（无该配置）会继续凭自述通过，等于新闸门只保护新项目。故同时保留 `ci_ready` / `ci_ready_gate` 步骤 ID 白名单，配置声明优先。
4. **"人打回即重置三轮"落在 attempt 1 而不是 0**：返工边自带确定性自增，映射 `"review_rounds": "0"` 经引擎后进入 `implement` 的值为 `1`——语义正确（人工打回买到的是全新三轮，从第 1 次尝试计起），实测确认后写进断言，而不是把断言改成迁就实现前的猜测。
5. **阻断的可重试性必须区分**：`waiting_pipeline` 返回 409（等待，不该叫人），`repairable` / `ci_failed` 返回 400（要改东西），`environment` 携带 `needsHuman`。若一律 400，就把"等 CI"伪装成了"你做错了"。
6. **失败步骤不被闸门捕获**：agent 上报失败（`status != completed`）时闸门跳过复算——拦住一个正在报告问题的人，只会丢掉那份报告。

## 尚未实现（本 ADR 已裁定、按依赖顺序后置）

- 人工打回归因 `rework` / `rescope` 二分与**永不重置**的 `human_interventions` 计数：这两件必须一起交付——只加计数器而不让"第二次人工打回"改变路由，就是本项目反复出现的那种"clamp 但不阻断"。它涉及模板新增出边，需经 `POST /api/v1/workflows` 重新实例化才生效。
- `ci_failed` 下"同一失败项连续 2 轮仍未过才 park 给人"的重复计数（需要跨步骤完成保存失败项计数）。
- 把 `waiting_pipeline` 的等待状态与 `needsHuman` 的告警接到控制台 Nodes/任务视图与 Mattermost 卡片文案上（当前只在 API 响应体里携带 `cause / retryable / needsHuman`）。

## Reason

- 这个平台存在的理由是"独立证据优先于模型自述"（Reviewer 独立基线契约、ATD 契约锚点、SHA 漂移硬拦截都基于此）。轮数上限若只写在提示词里，就等于在唯一需要确定性兜底的地方重新引入了它要防的失效模式：模型自报的轮数已经不可靠（生产曾出现第 2 轮上报第 1 轮），而模型自报的"我再试一轮"同样不可靠。
- 把"是否升级"的权力留在模型手里，后果不是多跑一轮，而是**人的注意力被安排在最没价值的位置**：三轮空转之后人拿到的是一堆彼此矛盾的自述。
- 打回归因二分是会计问题而非流程问题。让"重新定方向"消耗"代码返工额度"，会同时污染两个信号：轮数失去"实现质量"含义，方向错误也被伪装成实现不达标。
- `ci_ready` 的三分类是现场缺陷驱动：`not_ready` 一词混合了"仓库不合格"与"CI 还在跑"，而人只能点"开始"重试。后者会让人对着仍在运行的流水线反复点按钮，正是本项目已修过的"权限不足被静默丢弃 → 用户体感机器人假死"那一类事故的镜像版本。
- 复用 `risk_level` 而不是新增 P0..P4：一个产品里两套严重度词汇，最终成本是审批人需要记住哪套词在哪个节点有效。

## Cost to reverse

1. 轮数强制升级若日后被证明过严（例如合法的第 4 轮修复），回退方式是**提高 `maxReviewRounds` 常量**或给特定模板放宽，而不是取消服务端改写；取消改写等于回到"闸门靠提示词"。
2. `human_interventions` 与 `rescope` 出边是模板拓扑变更：需要经 `POST /api/v1/workflows` 重新实例化才在控制台生效（工作流双层体系约定）。回退需同时回退模板与已实例化数据，故新字段一律声明为 optional，不要求人手填。
3. `ci_ready` 参与路由后，存量项目的历史任务可能在升级瞬间被判定 `not_ready` 而停在闸门。缓解：阻断只作用于**新的一次步骤完成**，不追溯改写已完成的 run；并且三分类中的 `waiting_pipeline` 不产生人工待办。
4. 严禁反向操作：不得为了让单测通过而把服务端复算降级为"仅记录不阻断"（这会让硬化后的闸门重新变成装饰）。
