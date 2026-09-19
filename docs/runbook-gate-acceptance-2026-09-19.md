# 闸门真机验收 Runbook（2026-09-19）

适用改动：`47d1844c`(P0 节点领取边界) · `ecd7a9c6`(P1 真实 diff) · `7684add7`(P2 ci_ready 闸门四分流 + 轮次封顶)。

这份文档**只在真机上执行**，用来补自动化测试证明不了的部分。所有命令与期望值均已对照当前源码核对（源码位置随条目标注），不含臆造命令；真机地址、凭据、runner 标签、部署主机一律用占位符，实际值只写在私有 runbook 里。

## 1. 为什么自动化绿灯还不够

| 证据点 | `make test` 已覆盖 | 只有真机能回答 |
|---|---|---|
| ④ 模板重新实例化 | 模板结构断言（内存对象） | 库里那一行 definition 是不是新拓扑；新任务是否绑到它 |
| ① 闸门四分流 | 构造 `ciReadyResponse` 的分类表 + 2 个 HTTP 接线用例 | 真实 GitLab 的 pipeline 状态字符串、错误分支、409/400 的实际响应体 |
| ② 审核面板 diff | handler 6 例 + `DiffCommits` 5 例（含外部 diff 驱动器的恶意配置 canary） | 人在浏览器里**看得懂**这份 diff 吗 |
| ③ 三轮封顶 | 引擎路由 3 例（封顶强制升级 / 未封顶照常返工 / 人工打回重置预算） | `[平台裁决]` 记录与案卷在真实任务上是否可读 |

诚实边界：本脚本执行完成前，上面右侧四栏全部是 `unknown`，不得对外声称已验证。

## 2. 占位符

在部署主机上导出（值来自私有 runbook，不要写进仓库、聊天记录或截图）：

```sh
export MG='http://127.0.0.1:<port>'          # 控制台 API 基址
export PROJ='<project-name>'                  # 被验收的项目
export ADMIN_TOKEN="$(multigent admin-token --user admin --ttl 30m)"   # 部署主机上执行，需要与 server 相同的 MULTIGENT_DATA_DIR
export TASK='<task-id>'                       # 步骤 0 之后填入本次验收新建的任务
```

`multigent admin-token` 若不带对的环境变量会解析到错误的 DB 并签出被 server 拒绝的 token（源码 `cmd/multigent/token.go` 的 fail-closed 注释记录了这起事故）；返回的 token 是凭据，用完即弃。

API 调用统一形态：

```sh
curl -sS -X GET "$MG/api/v1/workflows" -H "Authorization: Bearer $ADMIN_TOKEN"
```

## 3. 前置检查（任一不过先停下）

| # | 检查 | 命令 | 通过判据 |
|---|---|---|---|
| P1 | runtime-node 直连执行环境里有 `mga` | 在运行节点上 `command -v mga && mga version` | 退出码 0。历史上 `mga` 是手工 symlink 进宿主 PATH 的，不随节点下发；缺了它 agent 节点一步都跑不动 |
| P2 | 承担本流程的 agent 带 `task` 触发器 | `curl -sS "$MG/api/v1/agents" -H "Authorization: Bearer $ADMIN_TOKEN" \| jq '.agents[] \| {id, name, triggers: .schedule.triggers}'` | `triggers` 含 `task`（触发器存在 agent worker 的 `schedule` 里，`internal/api/agent_worker_handlers.go:518`；`multigent show agent` **不**显示它）。**这是两处人工闸门之间会不会永久挂住的分水岭**：无 trigger 的 agent 在人工放行后不会被唤醒，表现为"点了通过然后没反应"（`docs/review-brief-for-gpt-2026-09-14-evening.md`） |
| P3 | 项目绑定了已验证的 GitLab remote | `curl -sS "$MG/api/v1/projects/$PROJ" -H "Authorization: Bearer $ADMIN_TOKEN"` | 见 §5：`environment`/`ci_failed`/`waiting_pipeline` 三格全部依赖 `s.verifiedBinding()` 为真。**未绑定 + 未声明 remote-required 时闸门整体是 no-op**（`internal/api/ci_ready_handlers.go:192` `localOnlySkip`），此时跑 ① 只会得到"通过"，那是假绿 |
| P4 | 任务处于可观测状态 | `multigent runs summary` / `multigent task show "$TASK"` | 能看到 active run 与当前 step |

## 4. 步骤 0：重新实例化模板（不做这步，真机跑的还是旧拓扑）

工作流是**两层**结构：代码里的 `Templates()` 只是只读目录，任务实际绑的是 `POST /api/v1/workflows` 落库的那一行 definition（`internal/workflow/store.go`，`internal/api/write.go:377` 用 `workflowDefinitionId` 启动 run）。改模板不重新实例化 = 改动不存在。

关键后果，脚本必须按此断言：重新实例化写的是**一行新的 definition（新 `wf…` id）**，不是原地覆盖。已存在的任务和 run 继续绑在旧 id 上。所以④的核对必须在"新建任务"之后做，验收任务必须建在新 definition 上。

```sh
# 0.1 记录改造前的库内拓扑（对照用）
multigent workflow templates --locale zh-CN            # 代码目录：ID/NAME/STEPS
multigent workflow list --format json                  # 库内已有 definition 及其 steps/edges

# 0.2 用当前代码模板新建一行 definition
curl -sS -X POST "$MG/api/v1/workflows" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"templateId":"unified-delivery-pipeline","locale":"zh-CN","name":"研发交付流水线-闸门验收-20260919"}'
# → 201；记下返回的 workflow id（下面记作 $DEF）。主机上有 DB 权限时等价命令：
#   multigent workflow create --template unified-delivery-pipeline --locale zh-CN --name "研发交付流水线-闸门验收-20260919"
#   multigent workflow list        # 从表里读同一行 id
```

请求体的键是 `templateId`（小写 `Id`，`workflowCreateBody` 的 json tag）。写成 `templateID` 不会报"字段不认识"，而是被当作"没给模板"落进手写 steps 的分支，返回一句和模板无关的校验错误 —— 遇到这种错先核对键名。

## 5. 证据点 ④：新拓扑核对（先做，它决定后面三点跑在哪张图上）

```sh
export DEF='wf…'                                       # §4 里 201 响应返回的 id
multigent workflow show "$DEF" --format json > /tmp/def.json
```

对照源码逐项打勾（`internal/workflow/store.go:1017-1064`）：

- [ ] `len(steps) == 13`，其中含 `ci_ready_gate`，位于 `agent_self_review` 与 `code_review` 之间。
- [ ] `len(edges) == 18`。
- [ ] `e-self-pass` 的 target 是 `ci_ready_gate`（旧拓扑里这一步可能根本不存在，或是 `code_review`）。
- [ ] `e-code-rework` 的 `inputMapping.review_rounds == "0"` —— 这是本次"人工打回买到 3 轮新预算"的落库证据（`store.go:1055`）。
- [ ] `agent_self_review` 的 output 含 `self_review_verdict` 与可选 `escalation_case`。

再确认新任务真的绑在它上面：

```sh
curl -sS -X POST "$MG/api/v1/projects/$PROJ/tasks" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"agent\":\"<pm-agent>\",\"title\":\"闸门验收 $(date +%m%d-%H%M)\",\"prompt\":\"<一个真实但小切口的需求>\",\"workflowDefinitionId\":\"$DEF\",\"workflowActorBindings\":{…按项目 agent 填…}}"
export TASK='<上一步返回的 task id>'
curl -sS "$MG/api/v1/projects/$PROJ/tasks/$TASK/workflow" -H "Authorization: Bearer $ADMIN_TOKEN"
```

- [ ] `/workflow` 响应里 run 的 `definitionId == $DEF`，且 `activeStepId` 是模板的 `clarify`。
- [ ] 控制台任务页显示的节点数与 `/tmp/def.json` 一致（**这一步是人眼核对，API 绿灯不算**）。

## 6. 证据点 ①：闸门四分流在真实 GitLab 上的表现

被验收的机制：agent 在 `ci_ready_gate` 步跑 `mga ci ready` 只是它自己的读数；平台在 `POST /api/v1/runtime/tasks/{id}/workflow/step/complete` 里**重算一遍**，不通过就不让这一步闭合（`internal/api/runtime_workflow_handlers.go:1085-1108`、`internal/api/ci_ready_gate.go`）。分类优先级：仓库缺陷 > pipeline 状态。HTTP 语义只有一条规则：**`waiting_pipeline` → 409，其余受阻原因 → 400**（`ciReadyGateIsRetryable`）。

每个用例都用 `mga task step done`（或等价的 curl `--data`）在 `ci_ready_gate` 步上提交完成，然后记录**实际**状态码与响应体。期望的响应体字段：`gate/stepId/cause/retryable/needsHuman/detail`。

| 用例 | 怎么造 | 期望 `cause` | 期望 HTTP | `retryable` | `needsHuman` | 实际 |
|---|---|---|---|---|---|---|
| A 通过 | HEAD 的 pipeline 已 success | 不阻断（这一步正常闭合） | 200 | — | — | ☐ |
| B 仓库缺陷 | 在任务分支上破坏一项确定性检查再 push：把 `.gitlab-ci.yml` 改出 YAML 语法错（→ `ci_yaml_parses`），或删掉 `deploy/Dockerfile`（→ `baseline_files`）。检查名见 `internal/ciready/ciready.go` | `repairable` | 400 | false | false | ☐ |
| C 等 pipeline | 刚 push 完、pipeline 还在跑或尚未建立，立刻提交完成 | `waiting_pipeline` | **409** | **true** | false | ☐ |
| D pipeline 真挂 | 在 `.gitlab-ci.yml` 里加一个必挂的 job（`script: ["exit 1"]`），push 并等它终态为 `failed`，再提交完成 | `ci_failed` | **400** | false | false | ☐ |
| E 环境缺凭据 | 项目未绑定 remote，且 remote-required 生效（P3 的显式声明） | `environment` | 400 | false | **true** | ☐ |

两条执行纪律：**B/D 的破坏性改动只允许存在于任务分支，验收完立刻在分支上 revert 掉再让流程往下走**——必挂 job 一旦被合并就是生产事故；闸门读的是**瞬时快照**（`waitSeconds=0`），刚 push 完读不到 pipeline 属正常，不要据此判定 C 失败。

必须被记录成事实（而不是"看起来对"）的四件事：

1. **D 是 400 不是 409**。这是本次验收要回答的核心疑问：一条失败的 pipeline 是缺陷，不是"再等等看"。如果真机观察到 409，说明 `cause` 判定或 `Retryable` 落点与设计不符 —— 停下来，把实际响应体贴进 §9，别改脚本让它变绿。
2. **C 的两条来源都要试**：GitLab 已建 pipeline 且非终态（`detail` 含 pipeline id 与状态）；HEAD 还没有任何 pipeline（`detail` 是 `no pipeline observed for the current HEAD yet…`）。两者都归 `waiting_pipeline`，但第二种的真实含义可能是"根本没推上去"，值得在记录里区分开。
3. **闸门不写仓库**：D 之后 `git -C <repo> status --porcelain` 必须为空。平台进程进工作树写文件是这次刻意排除的边界（`ci_ready_handlers.go:152-157`：闸门走 `ciready.Verify`，只有 init 端点走 `Ensure`）。
4. **普通步骤不受闸门影响**：在非 `ci_ready*` 步（例如 `implement`）提交完成，应当正常闭合，不出现 `gate` 字段。白名单见 `ciReadyGateStepIDs`，加 `platform_gate` 配置或步骤 ID 命中才生效。

顺带确认 agent 侧提示语：受阻时 `detail`/`message` 是否真的告诉它"谁该动手、下一步做什么"，尤其 C 那句"不要为了等 pipeline 去改代码"。

## 7. 证据点 ②：人工审核面板的真实 diff（浏览器实点）

前提：任务跑到 `code_review`（人工审核节点），且 `baseCommit` 与 `completionCommit` 都已记录。

先用 API 定后端事实，再用眼睛定前端事实：

```sh
curl -sS "$MG/api/v1/projects/$PROJ/tasks/$TASK/diff" -H "Authorization: Bearer $ADMIN_TOKEN"
curl -sS "$MG/api/v1/projects/$PROJ/tasks/$TASK/diff?base=<7-40位SHA>&head=<7-40位SHA>" -H "Authorization: Bearer $ADMIN_TOKEN"
```

后端断言：
- [ ] 默认区间就是 `baseCommit..completionCommit`；返回 `files[].{path,oldPath,status,additions,deletions,binary}` 与 `patch`。
- [ ] 非法 SHA → 400（`ValidateCommitSHA`，只接受 7–40 位十六进制）。
- [ ] 不存在的 commit → 404。
- [ ] 无基线可比 → 409（消息里给的是"还没有基线与完成快照"，不是 500）。
- [ ] 不带 token → 401；换另一个项目的 taskID → 404（越界探测不得变成 403 泄漏存在性）。
- [ ] 有外部 diff 驱动器/`textconv` 的仓库里仍然返回纯文本 diff（`--no-ext-diff --no-textconv` 位置见 `internal/gitworktree/purified_clone.go`；宿主上不执行外部程序是这条的存在理由）。

前端断言（必须人眼回答，不允许用测试代替）：
- [ ] 打开任务详情 → 人工审核面板，diff 证据块**看得见**（不是折叠到需要猜的角落）。
- [ ] 文件列表能读出改了什么（增删行数、重命名的旧路径、`base..head` 短 SHA）。
- [ ] 展开 patch 后**行级着色可读**，长 diff 截断时有明确告警而不是静默省略。
- [ ] 空状态（无改动）不伪装成错误。
- [ ] 中英两种 locale 文案都不溢出/不漏 key（`tasks.diff.*`、`workflows.detail.changeDiff`）。

截图存本地即可，**不要提交进仓库**（截图里通常带真实地址与主机名）。

## 8. 证据点 ③：三轮封顶升级（案卷可读性）

用生产管道本身把三轮跑完（`mga task step done` → 同一个 step/complete 端点），这样验的是引擎与呈现，不是模型判断力。这个区分要在记录里写清楚：**下面不能证明 reviewer-agent 真的会判断对，只证明它判断错到底时平台接管**。

```sh
# 每轮：implement 报 review_rounds 递增 → agent_self_review 报 issues_fixed → 自动回到 implement
mga task step done --id "$TASK" --agent <reviewer-agent> --status success \
  --summary "第 N 轮初审：仍有契约缺口" \
  --output self_review="轮次 N 的证据：…" \
  --output self_review_verdict=issues_fixed \
  --output review_rounds=N
```

- [ ] 第 1、2 轮：路由回 `implement`，且 `review_rounds` 由**平台**递增（模型报 2，下一步收到 3）。
- [ ] 第 3 轮报 `issues_fixed`：路由**强制**改为升级，进入 `code_review`（人工）。
- [ ] 转换记录里出现 `[平台裁决] Agent 初审轮次已达上限 3…（agent 原判：…）`（文案见 `internal/workflow/store.go:2717` 附近的 transition summary）。
- [ ] 人在 `code_review` 面板上读得到：已达上限的事实、修了什么、还有什么没修、P0/P1 归类。**判定标准是"我作为审核人愿意据此打回或通过"，不是"字段存在"。**
- [ ] 人工打回（`decision=request_changes`）后再跑：`review_rounds` 从 1 开始（预算重置，见 §4 的 `review_rounds: "0"` 落库证据）。
- [ ] （可选，成本高）换成真实模型跑满三轮，验证 reviewer-agent 是否会在封顶前自己写 `escalate`。这栏单独记，不要和上面的引擎验证混成一条结论。

## 9. 证据记录

真机结果写进 `docs/agent/handoffs/current.md`（新增一条「闸门真机验收」条目，编号接在当时最新条目之前），格式：

- 每个用例一行：命令、期望、**实际**（含完整响应体）、判定 通过/不符/未执行。
- 不符的项必须原样保留失败输出并给出下一步归因，禁止"重跑一次就绿了"式记录。
- 未执行的格子明确留 `unknown`（例如没凑出 remote-required 环境时，E 就是未执行）。

## 10. 回退

| 层级 | 动作 | 说明 |
|---|---|---|
| 闸门本身 | `git revert 7684add7` | 故意**没有**环境变量逃逸开关：一个可以被配置关掉的闸门等于没有闸门。要停就 revert 并重新部署 |
| diff 面板 | `git revert ecd7a9c6` | 只读端点 + 前端组件，回退不影响路由 |
| 节点领取边界 | `git revert 47d1844c` | 回退前确认现场只有一个启用节点，否则等于把凭据下发面重新打开 |
| 库内 definition | 控制台/`multigent workflow delete <id>` 删掉 §4 新建的那一行 | 只影响之后新建的任务；已跑起来的 run 绑在旧 definition 上，不动 |

## 11. 本脚本不覆盖的边界

- `cause/retryable/needsHuman` 在控制台与 Mattermost 卡片上的**呈现**（后端已有字段，UI/IM 文案未做）。
- `ci_failed` "同一失败项连续两轮 → 停车给人"的去重逻辑（未实现）。
- 人工打回归因二分（`rework`/`rescope`）与永不清零的 `human_interventions` 计数（未实现，需新边 + 再次重新实例化）。
- P0 凭据下发面收敛：run spec 仍带项目 env 与模型凭据、节点 token 永久有效、`ScopesJSON` 存而不用、主机名可自报（任务 #2）。
