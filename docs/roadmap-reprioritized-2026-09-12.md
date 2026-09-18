# Multigent 中长期任务重排（基于 2026-09-12 真实状态）

> 依据：GPT 规划 + 仓库最新文档（`intranet-runtime-plan.md`、`runtime-p15-acceptance-2026-09-12.md`、`runtime-p15-canary-2026-09-12.md`、`acceptance-test-design-plan.md`、`greenfield.go`、`current.md`）+ 工作树 P0 实现核查。
> 结论：GPT 规划方向正确，但**信息滞后约两天**——它基于 9-10 的 handoff 快照，未反映 9-12 已完成的运行时治理与 P0 实现现状。

---

## 0. 现状核查结论（与 GPT 认知的差异）

| 项 | GPT 认知 | 真实状态（2026-09-12） |
|---|---|---|
| 内网运行时 P0 | "进行中，不应提前宣布完成" | **代码已基本实现**（工作树）：registries/network 强类型配置 + 严格解析、`multigent runtime probe`、flock 并发锁均已落地；待验证/部署/审计 |
| JDK 21 镜像（GPT 称 P1） | 未做 | **已完成并验收**：`runtime-jvm21:2026.9.1`，JAVA_TOOL_OPTIONS 代理注入、Gradle 真实链路、三态 profile、inventory、审计、回滚全闭环 |
| 存量项目运行时治理 | 未提 | **已 canary 回填**：todo-api=base、api-key-hub=jvm21，真实 Preview + agent sandbox 验收通过 |
| `current.md` | 指出其过期 | 确认：停在 9-10 ChatOps，未反映 9-12 运行时工作 |

---

## 1. 重排后的执行顺序

### 第 1 步：内网运行时 P0 收尾（zcode 进行中 → 验收 + 部署 + 审计）
P0 代码已实现，剩余是**收尾而非开发**：
- 全量 `make test` + `make build` 门禁；
- 本机 `multigent runtime probe` 功能验证（配错端口 → FAIL 非零退出码）；
- **空配置回归**：BuildArgs 输出与改前一致（硬性要求）；
- **严格解析的部署风险**：审计 VM 上 `/opt/multigent/data/` 的 conf 是否有历史遗留未知键，避免启动失败；
- 变更清单输出、由用户审计后提交（plan 明确"不提交"）。
- 完成后：P0 收口，内网部署前提就绪。

### 第 2 步：Reviewer Prompt 增强 + 前置验收测试设计（改动小、收益高，可同批）
- **Reviewer 增强**（GPT 第 4 点，最被低估）：从"要求 Reviewer 写另一套方案"改为"建立独立预期行为基线"。Reviewer 先读需求/设计/base SHA/head SHA/测试规格，不看 Diff 列出应满足行为、数据流、权限边界、高风险点、应有测试切面；再看 Diff 对照。输出强制结构化（问题重述/验收标准/独立预期/Diff 证据/必须修复/非阻断/测试缺口/approve|request_changes|escalate）。限制：不读研发思考链、不改业务代码、跑不了测试标 `unverified`、三轮封顶转人工。
- **前置验收测试设计**（GPT 第 5 点）：`acceptance-test-design-plan.md` 已设计好，按 Batch A/B/C 实施。排在 Reviewer 之后或同批——先让 Reviewer 能审测试质量，再把 QA 测试规格前移到设计确认后。

### 第 3 步：双人 Mattermost 真实验收（GPT 第 3 点）
- 两个真实用户分别承担需求/代码/QA 审批；验证 @提醒、卡片点击、权限、打回、重发、任务 Thread、重启恢复。
- 现有记录对"真实回调是否都已验证"存在新旧证据不一致，值得完整收官一次。
- 注意：`current.md` 已闭环 DEFECT-C3（多 Bot 绑定冲突）、审批打回 400、静默拒绝反馈等，验收应基于最新代码。

### 第 4 步：Brownfield 存量仓库接入完整走通（GPT 第 2 点）
- 运行时/依赖这一半已由 canary 推进；补齐：已有 GitLab 仓库拉取、分支基线、私有依赖、已有 CI、凭据、构建与回推。
- 对应 `enterprise-evolution-and-scale-roadmap.md` 的 P0 五步就绪流水线（只读探测 → 受控基线 → 最小验证 → 就绪报告 → 权限准入）。

### 第 5 步：并发与资源治理（GPT 第 8 点）
- 至少两个项目、每项目两个任务并发压测：容器配额、排队、缓存、Docker 资源回收、重启恢复、跨项目消息串线。
- 对应四层限额模型（Workspace/Project/Agent/Node）。

### 第 6 步：IM 实例级网关与跨 Bot 身份复用（GPT 第 7 点）
- 单实例单 `/mg` 网关、用户绑定一次多 Bot 动态私聊、稳定实例标识。
- ChatOps 从"试点能用"走向"多项目多 Bot 可运营"。

### 第 7 步：大需求拆分 / Goal 树产品化（GPT 第 9 点，最后）
- 保留"人工审批批次计划 + 明确依赖 + 不允许无限派生"规范；
- 等存量接入、并发隔离、测试证据稳定后再做任务树 UI 与自动编排。

---

## 2. 与 GPT 顺序的差异说明

| 位置 | GPT | 重排 | 原因 |
|---|---|---|---|
| 第 1 | 内网运行时收口 | 内网运行时 P0 **收尾**（非开发） | P0 代码已实现，只剩验收/部署/审计 |
| 第 2 | 双人 Mattermost | Reviewer 增强 + 前置测试 | 改动小、收益高，且是后续 QA 质量的地基 |
| 第 3 | Brownfield | 双人 Mattermost | 保持，但基于最新代码 |
| 第 4 | Reviewer + 前置测试 | Brownfield | 运行时治理已推进，补齐 CI/凭据/回推 |
| 其余 | 并发 → IM → Goal 树 | 并发 → IM → Goal 树 | 一致 |

---

## 3. 风险与提醒

1. **P0 严格解析的部署风险**：VM conf 若有历史遗留未知键会启动失败，部署前必须先审计（plan §4.3-9）。
2. **P0 空配置回归**是硬性要求，别被"代码写完了"误导，未验证不得声称完成（plan §5）。
3. **`current.md` 会持续过期**：重要结论应写 HANDOFF 或独立验收文档，不要只留在会话里。
4. **Reviewer 增强的边界**：不要把"我会用另一种架构实现"本身当问题；阻断问题必须带"违反什么/文件行号/影响/最小修复/缺失验证"。
5. **JDK 镜像未推送 registry**：`runtime-jvm21:2026.9.1` 仅本地构建（无 registry digest），生产推广前需固化镜像构建与版本化记录（验收记录 §5-1 已提示）。