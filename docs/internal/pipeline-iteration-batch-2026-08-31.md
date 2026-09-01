# 流水线迭代批次方案(2026-08-31)

> 状态:P0/P1 本 commit 实现;P2 为实验任务(平台零代码);P3 待立项。
> 依据:统一交付流水线首次端到端验证(t-20260831-s5n4ul)后的复盘,经双向评审(Antigravity 评审 + 逐条代码核实)后的最终版。

## 背景

首次全流程真实验证暴露四类改进:人工闸门填写负担重(字段描述单薄、审核人不知如何填写)、缺少紧急部署形态、CI 搭建调试需要 agent 多轮跟进能力验证、预填草稿可消除人工搬运。本批次按 P0-P3 组织,依赖关系:

```
P0(文案) ──► 重新实例化模板 ──► P3(预填,后续立项)
P1(hotfix 模板) ── 独立
P2(CI 实验任务) ── 独立,建议先执行
```

## P0:人工闸门字段描述增强(本 commit)

**问题**:审核节点输出字段描述仅一行标签(如 `approved_scope` = "已批准的范围与验收标准"),审核人不清楚填什么、粒度、与另一框分工。实证:需求快审与 QA 准出节点用户均无法独立完成。

**方案**:增强 `internal/workflow/store.go` 字段描述(中英双语),排版采用**两段式**(评审优化 1):主旨句前置 + "必须包含"细则后置,避免 `WorkflowFieldTitle` 整段加粗渲染导致弹窗拥挤。覆盖:`approvedScopeField`、`candidateField`/`releaseCandidateField`、`approvedChangeField`、`commentsField`(各闸门通用)。

**验收**:描述含"填什么 + 粒度 + 去向"三要素;模板结构测试同步更新;模板重新实例化后新任务可见。

**关键语义(写入描述)**:`release_candidate` 必须写具体不可变引用(SHA/Tag),禁止分支名;`comments` 不向下游传递,裁决必须折叠进契约字段。

## P1:hotfix-deploy-pipeline 内置模板(本 commit)

**问题**:紧急上线/线上故障修复时 12 步全流程过重;纯手动 GitLab 触发又丢失 agent 定位修复能力与平台留痕。

**方案**:新增内置模板 `hotfix-deploy-pipeline`,5 步 2 个人工闸门(方案评审定稿,采纳 confirm 双打回建议):

```
triage(agent, triage-agent)          定位问题、固化 base_commit(main HEAD SHA)、输出 diagnosis
  │ e-triage-review(默认边)
  ▼
hotfix_review(human, product-owner)  快速确认方案,输出 decision / approved_fix / comments
  │ e-fix-approved(approve,显式映射 base_commit: $input.diagnosis.base_commit,安全红线#4) ──► implement_fix
  │ e-fix-rejected(request_changes,带 review_comments + previous_diagnosis) ──► triage
  ▼
implement_fix(agent, developer-agent) 基于 base_commit 开分支修复 + 回归测试
  │ e-fix-verify ────────────────► verify_and_tag
  ▼
verify_and_tag(agent, developer-agent) 实测重跑测试、打 Tag 推送、尽力触发部署流水线
  │ (无 CI 如实填 none + 原因;禁止未实测就打标)
  │ e-verify-confirm(默认边)
  ▼
confirm(human, product-owner)        确认部署结果与观察
  ├─ approve → 流程完成(isTerminalReviewApproval 终态语义)
  ├─ request_changes → implement_fix(修复类问题,带 review_comments + previous_fix 旧 Tag 上下文)
  └─ escalate → triage(方案性错误,重新诊断,带 review_comments)
```

**设计决策**(评审定稿):
- 不引入 `review_rounds` 三轮封顶:hotfix 打回由人控,不存在 agent 自循环;但 verify_and_tag 步骤描述强制实测纪律;
- triage/implement_fix 角色语义分离(`triage-agent`/`developer-agent`),物理绑定在实例化时可指向同一 agent(平台 actor 绑定机制原生支持);
- `base_commit` 经边显式传递,implement_fix 严禁以流动的 origin/main 为基线(安全红线第 4 条);
- confirm 双打回:修复类问题回 implement_fix,方案性错误回 triage;
- 打回边显式映射 `review_comments` + `previous_fix`/`previous_diagnosis`(引擎语义:重入步骤的输入由边映射**完全替换**,store.go `buildNextInputValues`;unified 模板 `e-pr-rework` 同款惯例,未声明字段也可经映射携带)。

**验收**:结构测试(步骤/闸门/关键边映射)通过;既有 6 模板测试零破坏;实例化后可选用于任务。

## P2:CI 搭建调试任务实验(平台零代码,单独建任务执行)

**目的**:验证 agent 多轮跟进长时任务(改 CI YAML → 触发 → 轮询 → 读日志 → 修复循环)。

**任务卡要点**:
1. 在 `1test` 添加 `.gitlab-ci.yml`(lint/test/build 三 stage,test 产覆盖率 artifact);
2. 经已授权 GitLab 连接触发流水线(`POST /projects/:id/pipeline`);
3. 轮询策略(评审修订版):触发后等 3-5 分钟首轮查询;**仍在跑则创建 `NotBefore` 延时任务安排下轮检查**(`entity/types.go` NotBefore 机制),不沙箱内死等;
4. 失败读 job trace 分析根因,修复重推,循环至全绿;
5. 产出:最终 YAML、流水线 URL、调试问题清单;不可自动化项(如 runner 缺失)如实记录为平台阻塞。

**前置**（2026-08-31 已核销）:连接器 `local-gitlab` active 且 grants 含 `1test`;GitLab runner `AITP Local CI Runner` 在线,已关联到 `root/1test`(project runner,原仅绑 personal-ai-pipeline-local)。**注意**:该 runner `run_untagged=false`,CI job 必须声明 `tags: [docker]`(或 aitp/local)才会被拾取——任务卡第 1 步写 YAML 时带上。

**实验观察目标**:NotBefore 延时唤醒可靠性、轮询节拍、是否值得封装 `mga pipeline status` 命令(结论入 HANDOFF 候选)。

## P3:审核弹窗预填草稿(待立项,2-3 天)

**方案**:新增 `GET .../workflow/review/draft`,按活跃步骤输出字段计算预填;前端弹窗拉取草稿(带来源标注 + 重新生成按钮),decision 恒人工选择。

**release_candidate 提取规则**(评审定稿,砍掉正则项):
1. 上游 `merged_sha` 结构化输出(键明确,准确率最高);
2. 任务 `IntegratedCommit` → `RemoteSyncCommit` → `CompletionCommit`;
3. 均无返回 null,**绝不回填分支名,绝不正则扫报告正文**(正文可能引用回滚目标等其他 SHA,会抓错)。

**设计决策:绝不引入 LLM 摘要**(升格为设计决策,非仅非目标):幻觉篡改一位 SHA 即发运错误代码;LLM 调用让弹窗转圈;规则可表驱动测试。comments 草稿用规则拼接(上游产物关键数字 + 裁决要点),前缀 `[草稿,请审核修改]`。

**顺手项**(非阻塞):长描述在 `WorkflowFieldTitle` 中拆分主旨句(加粗)与细则(灰字小号),前端一行改动。

## 实施记录

- P0/P1 本 commit 交付;模板重新实例化由部署脚本/手动 curl 完成(模板 ≠ 可用流程);
- P2 建议作为下一个独立任务;P3 待 P0 实例化后立项。

## P2 实验结果(2026-09-01,已完结 ✅)

任务 `t-20260831-tsyhis`(1test,统一交付流水线 wf-g837rxka,Lina)12 步全流程走完:

- **交付**:`.gitlab-ci.yml`(lint/test/build 三 stage、6 job、全 job `tags:[docker]`、workflow rules 四源)+ changelog + gitignore worktree 修复;MR !3 合并提交 `92573d1`;Tag `v0.2.0` 指向该 SHA,tag 触发的发布流水线 #874 success。三次失败-修复循环均自主完成(镜像拉取挂 36min→换 alpine 为最典型)。
- **实验观察目标全部兑现**:
  1. **多轮跟进长时任务**:✅ agent 自主改 YAML→触发→有界后台轮询(30s×8min 上限)→读 trace→修复重推,无需人工干预;
  2. **NotBefore 延时唤醒**:✅ 机制可用,但注意**新建 agent 心跳默认 disabled**,事件触发能推节点流转,cycle 失败后的"下一轮接力"无兜底,需编排者手动 `POST /api/v1/scheduler/wakeup` 补触发(或打开心跳);
  3. **是否值得封装 `mga pipeline status`**:倾向**值得**——agent 现在靠 `curl GitLab API` 轮询,每次都要拼 project_id/编码路径/找 token;一条 `mga pipeline status --ref <branch>` 能省掉每轮 ~3 个工具调用,结论已列 HANDOFF 候选。
- **过程性发现**(已修/已记):ArchivedAt 卡死 bug(坑 17,commit da7c88d)、wakeup API 误用(坑 16)、编排 Agent 回合预算纪律(坑 15 修订)、CI runner egress 对全量 golang 镜像不友好(任务卡已沉淀 alpine 惯例)。
- **人工闸门体验**:三轮审核(clarify_review/code_review/qa_signoff+pr_review)全部通过 API 提交;增强后的字段描述("填什么+粒度+去向"两段式)首次实战,审批人可直接照描述填 `release_candidate`(不可变 SHA 引用)——P0 效果符合预期,P3 预填的价值进一步确认。
