# 部署中心整体方案（Deploy Center Proposal）

状态：待评审。定位：把「合并进 main」与「部署上线」解耦成两个动作，为项目提供独立的部署页面（分支/环境/变量/审批）、应用预览页（部署门控）、部署 AI 抽屉（发现问题攒会话转任务）、以及部署后自动校验。全篇围绕一个可消融的核心论点展开。

---

## 0. 一句话定位与消融假设（为什么加）

**核心论点：目前平台能交付「合并的代码」，不能交付「运行的应用」。** 部署中心补的是从 merge 到 live 的最后一公里，同时把每次部署变成一个可观测、可审批、可追溯、可回滚的一等公民动作。

每个模块都是独立可开关的，目的是让消融实验（ablation）成为第一公民——同一个交付任务，开/关某模块各跑一遍，用指标判断价值：

| 模块 | 假设（H） | 消融指标 | 开关 |
|---|---|---|---|
| M1 发布火车（部署与合并解耦） | 批量部署降低部署抖动，不增加 live 缺陷滞留 | 部署次数/合并次数比值；合并→live 时延；单次部署缺陷数 | 天然开关（不点部署就不部署） |
| M2 部署审批闸门（Mattermost 卡片） | 人工审批能拦下坏部署 | 拒绝率；拒绝的部署若放行会否故障（回溯） | 项目级开关 |
| M3 部署页（环境/版本/历史/讲故事数据） | 可视化减少人工查询与误操作 | 人工操作步数（对比 batch-test1 基线的手工 runbook 步数）；部署平均耗时 | 常驻 |
| M4 部署 AI 抽屉（会话攒问题转任务） | 发现问题的即时捕获能显著提高后续修复吞吐 | 会话转任务数 vs 无抽屉时期的口头遗漏数；任务描述质量（可复现性） | 环境级开关 |
| M5 部署后自动校验 | diff 驱动的部署后校验能比 QA 矩阵更早拦住回归 | 校验失败检出数；漏检数（对比人工 QA） | 环境级开关 |

基线：batch-test1 全链路（2026-09-30 收官）已给出无部署中心时的人工路径（release 步打 tag → CI 部署 → 人工确认 go_live），作为消融对照组。

---

## 1. 现状盘点（代码已核实，2026-09-30）

**已存在：**
- CI 基线（`internal/projecttemplate/files/*/.gitlab-ci.yml`）五 stage：lint/test/build/package/deploy。**deploy 仅在 tag 流水线触发**（`if: $CI_COMMIT_TAG`），做 compose up + 60s 健康探针。
- 每项目部署端口：模板初始化时从 28000-28999 池分配（`internal/api/deploy_port.go`），DB 权威 + 镜像为 GitLab CI 变量 `APP_PORT`。
- 工作流 release 步：greenfield/unified 模板会打 tag → 触发 tag 流水线 → 自动部署 → 记录健康状态（batch-test1 v0.1.0 实弹通过）。
- GitLab 客户端（`internal/codehost/gitlab.go`）：已有 PipelinesForSHA / PipelineJobs / SetProjectVariable / CreateOrUpdateMR / MergeMR。
- Mattermost 交互卡：`internal/imbridge/chatops_token.go` 已有 HMAC 签名 action token（approve/reject，含过期、状态版本、nonce），审批卡是既有机制的扩展而非新面。
- 任务创建 API：`POST /api/v1/projects/{name}/tasks`（write.go:345）现成，会话转任务直接复用。

**缺失（本方案要补的缺口）：**
1. **main/任意分支无部署动作**：MR 合并后 main 流水线只有 lint/test/build，代码停在仓库里。
2. **部署请求无实体**：没有 deploy request 概念、无部署历史表、无审批状态机。
3. **部署主机无字段**（deploy_port.go 注释明言 deploy host 通常是另一台机器但无配置）；实际上 deploy job 跑在挂 docker.sock 的 runner 机器上，runner 信息可从 pipeline job 取到。
4. **GitLab 客户端缺** ListBranches（分支选择器）、TriggerPipeline（触发部署）、ListRecentPipelines（流水线列表，现仅按 SHA 查）。
5. 无部署 AI、无部署后校验。

---

## 2. 概念模型

```
DeployRequest（部署单，一等公民）
├── branch + targetSHA        本次部署到哪个分支哪个 commit
├── env                       环境（一期仅 production；schema 留扩展位）
├── vars                      非敏感部署变量（敏感变量只留 key 引用，值在 GitLab CI 设置）
├── commitSpan                lastDeployedSHA..targetSHA 的 diff commit 列表（M5 的输入）
├── approval                  审批记录（需要时）：申请人/审批人/时间/意见
├── status                    draft → pending_approval → approved → deploying
│                             → success | failed | cancelled
├── pipeline                  触发的 GitLab pipeline id + job 状态 + runner 信息
└── health                    部署后健康探针结果（复用 CI deploy job 探针 + 平台侧复查）
```

**发布火车（M1）语义**：合并永不自动触发部署。部署只通过部署单发生——部署单可以攒：一下午三次合并，晚上一次部署单带上三次合并的 commit span。`lastDeployedSHA` 随每次成功部署推进，diff commit 自动累积。

**预览门控语义**：项目级预览 tab 展示的是**最新成功部署的应用**（不是任务级预览抽屉——那套保持现状不动）。未部署过 → 不隐藏 tab，进页面给指引卡片「请先部署」→ 一键跳部署页。部署中 → 预览页显示部署进度（轮询部署单状态）。

**安全边界继承**：所有新端点注册在主 mux（token 鉴权 + `checkProjectAccess`），绝不进 publicMux；Mattermost 审批走既有 chatops 回调（HMAC action token），act 枚举扩展 `deploy_approve`/`deploy_reject`；部署触发/审批写 audit_events；凭据不落盘（部署变量敏感项只在 GitLab CI 侧，平台存 key 引用与掩码）；Git 命令输出入库前 redact。

---

## 3. 信息架构与页面设计

### 3.1 项目侧边栏

```
任务 / 任务模板 / 资料 / OKR / 里程碑 / 消息 / 成员 / 运行
→ 新增：预览（app 预览）  部署（deploy center）      设置
```

### 3.2 部署页 `/projects/:id/deploy`

```
┌────────────────────────────────────────────────────────────┐
│ production 环境                                [打开应用 ↗] │
│ ┌──────────────┬──────────────┬──────────────┬───────────┐ │
│ │ 运行版本      │ 健康状态      │ 地址          │ 部署机     │ │
│ │ v0.1.3       │ ● UP 42ms    │ :28001       │ runner-01 │ │
│ │ a1b2c3d      │ 9/30 18:20   │ (或未配置主机) │ (来自job)  │ │
│ └──────────────┴──────────────┴──────────────┴───────────┘ │
│ 讲故事数据条：本周部署 5 次 · 合并→live 平均 3.2h · 成功率 96%│
├────────────────────────────────────────────────────────────┤
│ 发起部署                                    [需审批: 开关]   │
│ 分支 [main ▾]  基线 [main@e5f6a7b (最新)]                   │
│ 本次将携带 3 个新 commit（vs 上次部署 a1b2c3d）：            │
│   · fix: instance heartbeat 幂等  · feat: audit 导出 …      │
│ 变量：APP_PORT=28001（只读）  [+ 非敏感变量]                 │
│                                    [发起部署] →(审批卡片→MM) │
├────────────────────────────────────────────────────────────┤
│ 部署历史（部署单列表：状态/分支/commit span/审批人/耗时/健康） │
│ 流水线（最近 GitLab pipelines：tag 发版 + 部署流水线）        │
└────────────────────────────────────────────────────────────┘
```

要点：
- **分支选择器**从 GitLab branches 拉（默认 main），展示 targetSHA 与上次部署的 diff commit——这是发布火车的可视化表达。
- **部署并发锁**：同一项目同时只允许一个 in-flight 部署单（DB 唯一约束），防两口部署互踩。
- **重新部署** = 以历史部署单为模板发起新单（同 SHA 或滚回上一 SHA）；回滚语义明确记录为新部署单（可审计）。
- 健康状态平台侧主动探（GET 该端口 /api/health 或模板配置的探针路径），不只信 CI job 自述。

### 3.3 预览页 `/projects/:id/preview`

```
┌────────────────────────────────────────────────────────────┐
│ 预览（production@v0.1.3）              [新窗口打开 ↗] [刷新] │
│ ┌──────────────────────────────────────────────────────┐   │
│ │                                                      │   │
│ │           已部署应用 iframe（只读嵌入）                 │   │
│ │                                                      │   │
│ │                                    ┌─────────┐       │   │
│ └────────────────────────────────────│ AI 抽屉 ⚡│──────┘   │
└────────────────────────────────────────────────────────────┘
```

- 未部署过：中央显示指引卡「此项目还没有部署过 → [前往部署]」，tab 不隐藏。
- 部署中：中央显示部署进度卡（当前部署单状态/流水线 job 实时状态）。
- 部署完成：iframe 嵌入 `http://<deploy-host>:<APP_PORT>`（deploy host 未配置时诚实降级为链接按钮）。嵌入是只读的——不注入 inspector 脚本、没有 DOM 选取，与任务级预览抽屉的 Copilot 写链路完全隔离（不触碰预览写锁与 origin fail-closed 机制；origin 配置继承既有 MULTIGENT_PREVIEW_ORIGIN 红线）。
- **AI 抽屉（M4）**：右下角抽屉，与预览抽屉 Copilot 形似但本质不同——**不改代码，只攒问题**。对话中 AI 从陈述里抽取「发现项」（页面现象/预期/复现路径），列表可视化；点「转任务」把勾选的发现项组装成任务描述，走既有 `POST tasks` 创建（description 附会话摘要 + 发现项清单，`created_by` 标注 deploy-ai）。抽屉无沙箱工具，纯对话+结构化抽取，无写锁交互。

---

## 4. 后端设计

### 4.1 新端点（全部主 mux + token + checkProjectAccess + 频率限制）

| 端点 | 作用 |
|---|---|
| `GET /api/v1/projects/{name}/deploy` | 聚合：当前运行版本/健康/端口/host/讲故事数据/最近部署单/最近流水线 |
| `POST /api/v1/projects/{name}/deploy/requests` | 创建部署单（branch/vars/审批标记）→ pending_approval 或直接 approved |
| `POST /api/v1/projects/{name}/deploy/requests/{id}/approve` · `/reject` | 审批（控制台侧） |
| `POST /api/v1/projects/{name}/deploy/requests/{id}/cancel` | 取消（仅 pending/deploying 前） |
| `POST /api/v1/projects/{name}/deploy/requests/{id}/trigger` | approved → 触发 GitLab pipeline（带 `MULTIGENT_DEPLOY=1` 变量），记录 pipeline id |
| `GET /api/v1/projects/{name}/deploy/branches` | 分支列表（GitLab ListBranches 代理） |
| `GET /api/v1/projects/{name}/deploy/preview-state` | 预览页轮询：最新成功部署 + in-flight 部署单状态 |

### 4.2 GitLab 客户端新增方法（`internal/codehost/gitlab.go`）

- `ListBranches(projectID)` — GET `/api/v4/projects/:id/repository/branches`
- `TriggerPipeline(projectID, ref, variables)` — POST `/api/v4/projects/:id/pipeline`（变量注入 `MULTIGENT_DEPLOY=1`）
- `ListRecentPipelines(projectID, limit)` — GET `/api/v4/projects/:id/pipelines`
- 部署 job 的 runner 信息从既有 `PipelineJobs` 取（job.runner 字段）→ 部署机展示的权威来源；`MULTIGENT_DEPLOY_HOST` 作为链接拼接的补充配置，未配置时诚实显示端口无链接。

### 4.3 DB

`deploy_requests` 表（controldb 迁移）：id/project/branch/sha/env/vars_json（非敏感）/commit_span_json/approval_json/status/pipeline_id/health_json/created_by/created_at/started_at/finished_at。项目级 `deploy_approval_enabled` 布尔加在 projects 或独立设置。`last_deployed_sha` 冗余在项目或以最新 success 部署单为准（后者，无第二事实源）。全部触发/审批/拒绝写 audit_events。

### 4.4 Mattermost 审批卡（M2）

- 部署单进入 pending_approval → 复用任务审核卡的投影通道发卡片：分支/commit span/发起人/变量摘要 + [批准][驳回] 按钮。
- 按钮带既有 HMAC action token（act 扩展 `deploy_approve`/`deploy_reject`，payload 塞 deploy request id，过期/nonce/状态版本全继承）。
- 回调处理：验证 token → 幂等推进部署单状态 → approved 后自动 trigger 或通知发起人。
- **不开任何新的无鉴权端点**；审批人身份沿用 chatops 既有绑定。

### 4.5 CI 基线改动（templates yml，版本号 bump）

deploy/package 两个 job 的 rules 各加一条：

```yaml
rules:
  - if: $CI_COMMIT_TAG
  - if: $CI_PIPELINE_SOURCE == "api" && $MULTIGENT_DEPLOY == "1"
```

效果：平台 API 触发分支流水线且带部署标记时，package+deploy 在该分支 SHA 就地构建镜像并 compose up——tag 发版仪式完全不变，部署动作从「打 tag」泛化到「部署单」。

---

## 5. 分期计划

| 期 | 内容 | 工作量 | 依赖 |
|---|---|---|---|
| **一期**：部署页 + 发布火车 + 审批卡 + 预览门控 | 4.1 端点（approve/cancel/trigger/branches/preview-state + deploy 聚合）、4.2 GitLab 方法、4.3 DB、4.4 MM 审批卡、4.5 CI yml、部署页前端、预览页骨架（指引/进度/iframe）、`MULTIGENT_DEPLOY_HOST` 配置 | 后端 2d + 前端 2d + CI 0.5d + 部署验证 0.5d ≈ **5d** | 无（不依赖监控专案） |
| **二期**：部署 AI 抽屉（M4） | 会话（无工具纯对话）+ 发现项结构化抽取 + 发现项列表 + 转任务（复用 POST tasks） | **1.5-2d** | 一期预览页 |
| **三期a**：部署后自动校验 v1（M5 确定性版） | 部署成功后：diff commits → agent 生成冒烟校验规格（HTTP 检查清单：method/path/期望状态码/期望断言）→ 平台确定性执行（纯 HTTP，无浏览器）→ 报告；失败 → attention 信号 + 自动建任务（issue） | **2d** | 一期 |
| **三期b**：浏览器级校验 + 自动修复闭环 | 校验规格升级为浏览器操作流（需 playwright 容器 spike）；失败自动修复分支 + 自动 merge（用户已定：后置） | spike 1d + 2-3d，**另行评审** | 三期a |

一期与监控告警 mock 页共享实现批次（同一次 make build + VM 部署 + 浏览器验收）。

---

## 6. 消融实验设计（写完之后怎么判断一波）

对照基线：batch-test1 无部署中心的人工路径（手工打 tag/观察 CI/手工探活/runbook 记录，操作步数已有实录）。

同一交付任务跑两遍（模块开/关），采集：
1. **合并→live 时延**（M1/M3）：部署中心应把分散的人工动作压成一次点击。
2. **人工操作步数**（M3）：runbook 步数对比。
3. **部署审批拒绝率与拦截质量**（M2）：拒绝的部署回放放行后果。
4. **发现问题→任务转化率**（M4）：有抽屉时期的转任务数 vs 无抽屉时的遗漏数（batch-test1 期间 QA 后人工补提的项就是对照组）。
5. **部署后回归检出**（M5）：三期a 上线后，校验失败数 vs 同期 QA 矩阵失败数的交集/差集。

开关落点：M2 项目级布尔；M4/M5 环境级 env（`MULTIGENT_DEPLOY_AI` / `MULTIGENT_DEPLOY_VERIFY`），关闭时页面隐藏对应入口，不留死按钮。

---

## 7. 开放问题（评审前需裁定）

- **Q1 审批人范围**：项目 operator？workspace admin？还是部署单指定审批人？建议一期=项目 operator 及以上。
- **Q2 tag 与分支部署的关系**：建议分支部署为日常（发布火车），tag 保留为发版仪式（版本号/ changelog）。是否需要「部署单自动打 tag」选项？建议否，保持两者正交。
- **Q3 回滚保留窗口**：就地构建镜像不推 registry，宿主会积累 `<project>:<tag|sha>` 镜像；回滚依赖旧镜像仍在或重触发旧 SHA 流水线（重构建）。一期接受重构建路径（慢但正确），镜像清理策略后置。
- **Q4 预览 iframe 跨域**：部署应用与控制台不同源，普通嵌入可行（生成的应用默认无 X-Frame-Options），但若应用未来加了安全头会白屏——预览页永远保留「新窗口打开」逃生口。
- **Q5 deploy host 多机**：一期假设单 runner 单部署机（现状如此）；多部署机 = 部署单加 target host 字段 + runner 选择，三期后再说。
- **Q6 校验规格的存储与重放**：三期a 的校验规格是否随部署单固化（可重放/可作为回归资产）？建议是，规格存 deploy_request 上。
- **Q7 与 unified-delivery-pipeline release 步的关系**：release 步打 tag 的行为不变；部署中心上线后 release 步的「部署+探活」叙述可与部署单打通（release 步创建部署单而非直接依赖 tag 流水线）——一期不动，二期后评估，避免动 13 步模板。
