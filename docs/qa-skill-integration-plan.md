# QA Skill 工具包采纳方案（外部 ming-qa / `qa-skill`）

## 1. 状态与结论

**状态**：评估完成。**暂不整体挂载**；阶段 1 可立即执行且不依赖该工具包一行代码，阶段 2/3 有硬前置（见 §6）。

**评估对象**：本地下载的 `qa-skill` 仓库（MIT，未纳入本仓库）。8 个 skill = 1 个编排器 `quality-assurance-agent` + 7 个阶段 skill（context-profiler / risk-analyzer / testcase-designer / test-script-generator / test-runner / code-reviewer / report-generator），实际干活的是 5300+ 行 `scripts/qa_agent.py`（Python 3.9+，仅标准库）。产物落在目标仓库的 `.qa-agent/`（根目录在 `qa_agent.py:5303`、`:5706` 与 `qa_core/project_manifest.py:23` **硬编码**，不可配）。

**一句话结论**：它的价值不在"节点分工"——那套我们已经有，而且比它更严；价值在**执行机器**（我们只有提示词，没有机器）和**跨任务回归语料**（我们完全没有）。

## 2. 背景：我们已经有什么

第三方方案把研发期切成三节点（前置意图冻结 / 研发内循环 / 后置验收防线）。对照本仓库现状：

| 它主张的能力 | 我们的现状 | 证据 |
|---|---|---|
| 前置意图冻结、用例驱动 | **已有**：`greenfield-delivery-pipeline` vNext 12 步含 `acceptance_test_design`，产出 `test_spec_doc/manifest/summary`，manifest 由纯函数强类型校验器硬门禁（拒占位词） | `internal/workflow/greenfield.go:167`、`internal/workflow/testspec.go` |
| 规格贯穿全链路对账 | **已有**：`test_implementation_evidence`（case_id → 测试文件/结果）贯通 implementation → self_review → code_review → qa → qa_signoff → rework | `docs/acceptance-test-design-plan.md` Batch B |
| QA 不能顺手改业务代码 | **已有且更严**：QA 声明 `touched_paths`，平台比对真实 worktree git delta，双向严格一致、fail-closed，且路径必须像测试产物 | `internal/workflow/qa_touched_paths_verify.go`、`internal/workflow/testpaths.go` |
| 不信 agent 自述、只信可复算证据 | **已有**：ci_ready 闸门服务端复算 + 四分流；轮次封顶由平台而非模型裁决 | `internal/api/ci_ready_gate.go`、`internal/workflow/store.go:3229` |
| 真的把测试跑起来（单测/API/E2E/覆盖率） | **没有**：qa 步骤的"独立执行 auto Case"是提示词要求，平台无执行机器、无覆盖率门禁 | — |
| 跨任务长期回归语料 + 快跑回归模式 | **没有**：ATD 规格是 per-task 的，随任务归档消失 | — |
| 环境就绪体检（G0 doctor） | **部分**：`internal/ciready` 管 CI 基线，不管测试执行环境 | — |

所以准确的判断是：**契约层我们领先，执行层我们空白。**

## 3. 对第三方分工方案的三处复核修正

以下三条是独立复核结果（不复述原方案），每条都对着本仓库代码验过：

| 原论点 | 复核结果 | 证据 |
|---|---|---|
| "给不同节点挂上不同的 Skill" | **平台无此能力**。绑定粒度只有 worker / team / role / project / workspace；`WorkflowStep.Config` 无 skill/tool 字段，`internal/workflow` 全包零处读 skill。可行替代：挂到 **qa-agent 这个 worker**（流水线本就按步骤分 agent）；要真做"按步骤"需新增平台代码 | `internal/ctxbuild/builder.go:15-175`、`internal/entity/workflow.go:49` |
| "`.qa-agent/cases/` 作跨节点契约接力棒" | **会撞我们自己的 fail-closed 闸门（已实测）**。目录白名单是精确段匹配，`.qa-agent` ≠ `qa`，故 `cases/`、`spec-tasks/`、`config/`、`reports/` 下的 json/html 一律判为"不像测试产物"而拒绝；而它的 `tests/api/<模块>/` 反而能过（命中 `tests`）。根目录硬编码不可配，所以只能改它的代码或放宽我们的白名单——**后者是削弱 fail-closed 闸门，不采纳** | `internal/workflow/testpaths.go:34-40,120-146`；实测 `TestValidateQATouchedPathsRejects` 已新增 5 条 `.qa-agent/*` 断言并 PASS |
| "查库断言" | 它走 MySQL MCP。在本平台等于给 qa-agent 挂 MCP tool binding、凭据放 binding Config，即**把 DB 凭据送进沙箱**——与 `47d1844c` 刚收窄的凭据下发面直接冲突，属任务 #2 范畴。不是不能做，但必须是"只读 + 专用测试库"的显式决策，不得默认开 | `internal/api/agent_tool_binding_handlers.go:15-20`、`internal/runner/runner.go:2015-2053` |

## 4. 真正值得采纳的三样（按价值排序）

**① 用例确认人工门禁 —— 这是我们流水线的洞，不是它的功能。**
`greenfield.go:316` 的 `e-atd-impl` 是**无条件默认边**：ATD 规格由 agent 产出后直接进 `implementation`，全程无人确认过用例。ADR `2026-09-18-delivery-pipeline-duality-and-atd-necessity.md` 只决定用"纯函数结构化校验"替代人工——那能抓占位词与缺字段，抓不住"这条用例测错了业务意图"或"漏了资金/权限路径"。它把"用例确认"定为全流程唯一强制人工门禁、且确认后不可回头改用例，这个判断成立。补法：新增一个 `human_review` 步骤，成本是改模板 + 一次重新实例化。

**② 确定性门禁文件契约 —— 与我们 P2 同一哲学，独立来源相互印证。**
它把 `completion-check.json` / `code-review-check.json` / `readiness-check.json` 明确写成 deterministic source，并规定"runner 不能判 Ready、report 不能重跑测试"。我们的 qa 步骤输出是自由文本 `test_report`，平台只能读一段话。把这三个 json 变成 qa 步骤的结构化输出，平台就能像 `ci_ready_gate` 一样**复算**而非采信。

**③ 跨任务回归语料 + 修复循环语义。**
`.qa-agent/cases/<module>.json` 是跨任务长期资产，配"回归 <模块>"快跑模式（不重新分析、不需人确认）——这是我们完全没有的。另外它 `maxRepairLoops=5` 且明确定义了 spinning（"下一轮给不出新假设"），比我们的"3 轮封顶"可操作。**注意两个 cap 会打架，必须只有一个所有者。**

## 5. 分阶段计划

**阶段 1 · 用例确认门禁（可立即执行，零外部依赖）**
- `internal/workflow/greenfield.go`：新增 `test_spec_review`（`human_review`），把 `e-atd-impl` 改为 `acceptance_test_design → test_spec_review → implementation`，打回边回 `acceptance_test_design`。
- 新增字段一律 `optional`，否则 `TestHumanReviewGateOutputsAreNeverHandRequired` 会红。
- 步骤数 12 → 13；必须重新实例化（`POST /api/v1/workflows`）才生效，并按 `docs/runbook-gate-acceptance-2026-09-19.md` §4-§5 核对库内拓扑。
- **连带项**：该 runbook 目前只对 `unified-delivery-pipeline` 写了 13 步/18 边的核对数字，阶段 1 落地后需补 greenfield 的对应数字，否则真机核对无基准。
- 验收：模板结构测试 + 引擎路由测试（确认边/打回边各一条）+ `make test` 全绿。

**阶段 2 · 借契约不借代码（qa 步骤结构化输出）**
- 只采纳三个 check json 的 schema 思路，作为 qa 步骤的结构化输出，由平台复算；**不引入 `qa_agent.py`**。
- 与既有 `qa_rework_items` 聚合对齐，避免两套失败清单。

**阶段 3 · 最小范围试点挂载**
- 只挂 qa-agent worker，只跑 `doctor --strict` 与用例设计两阶段，不进流水线路由。
- 需先关掉 §6 的四个决策。

**阶段 4 · 回归语料库（单独立项）**
- 跨任务资产，涉及存储位置、生命周期与项目隔离，不塞进一次 skill 挂载。

## 6. 执行前置：到什么程度才可以动手

**阶段 1 的前置（都已满足）**
- [x] 闸门硬化与轮次封顶已提交（`7684add7`），不再与模板改动叠加归因。
- [x] 真机验收脚本已就位（`docs/runbook-gate-acceptance-2026-09-19.md`），阶段 1 的重新实例化可直接复用其 §4-§5 核对流程。
- [ ] 阶段 1 自身需一次 ADR 落盘后再改模板（沿用"文档先行"纪律）。

**阶段 2 的前置**
- [ ] **闸门真机验收四格不再是 `unknown`**（runbook §6 的 A-E 全部有实测记录）。理由：阶段 2 是往同一条 step-complete 路径上再加一个复算点；旧闸门未取证就叠加，出问题时无法归因。
- [ ] 一次带 CI 的真实项目全流程跑通（含双人 Mattermost 流转终验）。

**阶段 3 的前置（四个决策，缺一不可）**
- [ ] **路径冲突**：决定是 fork 改 `.qa-agent` 根目录，还是让产物落 `qa/`（能过白名单），还是别的方案。**不通过放宽 `testpaths.go` 白名单解决。**
- [ ] **浏览器**：`docker/runtime-base/Dockerfile:78-87` 有 python3/ripgrep/nodejs（G0 doctor 可过），但**无 Playwright 浏览器** → G4 E2E 在沙箱内跑不了。要么加镜像（体积成本），要么让 E2E 打 preview URL——但 preview 是 30 分钟租期 + 每分钟 reaper + 需签名 token，长 E2E 会被回收，需先解决租期与令牌。
- [ ] **cap 单一所有者**：它的 `maxRepairLoops=5` 与我们的 `maxReviewRounds=3` 必须收敛成一个裁决点。
- [ ] **DB 凭据面**：与任务 #2（凭据下发面收敛）合并决策，默认不下发；若必须，只读 + 专用测试库。

## 7. 确定度标注

| 断言 | 确定度 |
|---|---|
| `.qa-agent/*` 被 QA 白名单拒、`tests/api/*` 通过 | **实测**（`go test ./internal/workflow/ -run TestValidateQATouchedPaths` PASS，新增 6 条断言） |
| `e-atd-impl` 无条件、ATD 后无人工确认 | **实读源码**（`greenfield.go:316` + 全部 18 条边） |
| 平台无"按步骤挂 skill"能力 | **实读源码**（Config 被读的键仅 `designGate`/`joinPolicy`/`notify*`/`platform_gate`） |
| runtime-base 有 python3/rg/node、无 Playwright | **实读 Dockerfile** |
| preview 租期会回收长 E2E | **推断**，未实测 |
| `qa_agent.py` 的实际行为与产物 | **未运行过**，全部结论来自读它的文档与脚本源码 |

## 8. 明确不做

- 不整体安装 8 个 skill（四个前置决策未落地前，装了也跑不通）。
- 不为它放宽 `internal/workflow/testpaths.go` 的 fail-closed 白名单。
- 不默认向沙箱下发任何 DB 凭据。
- 不把它的 HTML 报告当作准出证据——准出仍由平台复算的结构化产物决定。
