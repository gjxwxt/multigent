# 部署中心一期实施方案（Implementation Plan）

状态：待用户点头开工。上游文档：`docs/deploy-center-proposal.md`（已过评审）。本方案由四路并行调研产出（后端数据层/前端模式/MM 审批卡/CI 模板），全部坐标已核实到行号。范围：一期（部署页+发布火车+审批卡+预览门控）；按用户裁定**不含**讲故事数据条、不含异常横幅回滚直达按钮。

---

## 0. 调研发现的三个新硬点（方案原稿没预见的）

1. **镜像名硬伤（必修）**：两个模板的 package job 用 `docker build -t "$CI_PROJECT_NAME:$CI_COMMIT_TAG"`，deploy 用 `TAG="$CI_COMMIT_TAG"`。分支流水线中 `$CI_COMMIT_TAG` 为空 → build 直接失败、compose 落到 `:-dev` 默认。放开 rules 的同时必须改为 `TAG="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"`。
2. **rules 变量可见性是中高置信推断，非实证**：通过 `POST /api/v4/projects/:id/pipeline` 传入的 variables 在 `rules:if` 中可见——依据 GitLab 官方文档语义（rules 评估用合成后变量集），但仓库内零实证。一期第一个分支部署必须实弹观察；若不生效，兜底为触发前用 SetProjectVariable 暂写 project-level `MULTIGENT_DEPLOY=1`（用后清除）。
3. **MM 审核卡不能直接复用**：`PostHumanReviewCard` 前置依赖 `ActiveTaskThreadProjection`（部署单没有 task thread），需新写 `PostDeployApprovalCard`；但底层 `createPostWithProps`/`resolveMMTarget`/`FormatHumanReviewAttachment`/ChatopsActionSession 防重放全部可复用。

另有两个工程纪律点：`make web` 只跑 vite build **不做类型检查**，验证须 `cd web && npm run build`（tsc -b）；`PipelineJobInfo` 现无 runner 字段，部署机信息需给解码 struct 加 `Runner`（GitLab API 实际返回该对象）。

## 0b. 对抗式审查补充（第二轮，四条全部采纳）

1. **verify token TTL 10min → 30min**：流水线可能在 runner 上排队 8 分钟+lint/test/build 4 分钟，10 分钟 TTL 会误杀合法部署；token 已绑 requestID+SHA+nonce，放宽到 30 分钟不损安全性。§5 的回查步骤不变。
2. **CI 容器网络寻址**：纯 Linux 宿主容器默认不解析 host.docker.internal。平台 TriggerPipeline 时把 `MULTIGENT_CONSOLE_URL`（平台可达 base URL，来自请求 Host 或配置）作为 CI 变量一并注入；yml 回查写 `${MULTIGENT_CONSOLE_URL:-http://host.docker.internal:27892}`（本机部署兜底）。
3. **启动自愈 recoverActiveDeployRequests（防互斥锁死锁）**：平台在 deploying 状态重启 → partial unique index 永久锁死该项目部署。Server 启动时扫描 deploying 单据：向 GitLab 查该 pipeline 终态——已结束则按结果回写 success/failed（success 附健康探活），pipeline 查无则置 failed 释放锁；参照 recoverActiveWorkflowRuns 的 3s 延迟+节流模式。列入批次 3。
4. **镜像清理容错**：`tail -n +6` 在镜像 <5 个时输出为空，老版 xargs 无 `-r` 会报错——已有 `|| true` 兜底，保持即可；不做额外改动（alpine 3.20 的 xargs 支持 -r，双保险已足）。

---

## 1. 数据层（internal/db + controldb）

无版本化 migration 机制，`migrate()` 是幂等语句列表。改动：

- `internal/db/migrations.go` stmts 末尾追加：
  ```sql
  CREATE TABLE IF NOT EXISTS deploy_requests (
    id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, project_id TEXT NOT NULL,
    branch TEXT NOT NULL, sha TEXT NOT NULL, env TEXT NOT NULL DEFAULT 'production',
    vars_json TEXT NOT NULL DEFAULT '{}', commit_span_json TEXT NOT NULL DEFAULT '[]',
    approval_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL,
    pipeline_id INTEGER NOT NULL DEFAULT 0, health_json TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL, created_at TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT ''
  );
  CREATE INDEX IF NOT EXISTS idx_deploy_requests_project ON deploy_requests(workspace_id, project_id, created_at);
  ```
  并发锁用 partial unique index：`CREATE UNIQUE INDEX ... ON deploy_requests(workspace_id, project_id) WHERE status IN ('pending_approval','approved','deploying')`。
- 新建 `internal/db/deploy_requests.go`：照 remote_binding.go 的 Upsert（`ON CONFLICT DO UPDATE`）+ task_thread_projections.go 的 Filter/List/scan 模式；状态流转用 `UPDATE ... WHERE status=` 条件写（CAS 语义）。
- 状态机：`draft → pending_approval → approved → deploying → success | failed | cancelled`（approved→triggering 由 trigger 端点原子推进）。

## 2. 后端端点（internal/api，全部主 mux + withTokenAuth）

新文件 `internal/api/deploy_handlers.go`。路由注册 server.go 主 mux 段：

| 端点 | 权限 | 说明 |
|---|---|---|
| `GET /api/v1/projects/{name}/deploy` | checkProjectAccess | 聚合：项目 deployPort、最近成功部署单、in-flight 单、健康探针结果（实时 GET host:port /api/health，host 未配则返回 unknown）、最近流水线（ListRecentPipelines×PipelineJobs） |
| `POST .../deploy/requests` | checkProjectOperator | 创建部署单：branch/sha/vars（敏感 key 转发 GitLab 后丢弃）/commit span 计算（lastDeployedSHA..targetSHA，git log 本地算）/审批开关判断 → pending_approval（发 MM 卡）或 approved；写 audit `deploy.request.create` |
| `POST .../deploy/requests/{id}/approve`·`/reject`·`/cancel` | checkProjectOperator | 状态 CAS 推进；approve 后自动 trigger；audit 留痕 |
| `POST .../deploy/requests/{id}/trigger` | checkProjectOperator | approved→deploying；TriggerPipeline(ref=branch, variables=[MULTIGENT_DEPLOY=1, ...vars])；记录 pipeline_id；异步轮询 job 终态回写 success/failed+health |
| `GET .../deploy/branches` | checkProjectAccess | ListBranches 代理 |
| `GET .../deploy/preview-state` | checkProjectAccess | 预览页轮询用（最新成功部署+in-flight 摘要） |

惯例复用：`readJSON`(write.go:258)/`jsonError`(errors.go:93)/`serverError`(server.go:2444)；限流抄 `previewChatBucket`(preview_token.go:344-378) 新 map `deployReqSeen`；审计 `s.auditLog`(audit_handlers.go:30)。

**审批凭据 token（CI 回查用）**：`internal/api/deploy_verify_token.go` 新建，复刻 chatops_token 形状：payload `{p:project, s:sha, r:requestID, exp, nonce}`，HMAC-SHA256（key=users.Secret()），TTL 10 分钟；signPreviewToken 的 base64 拼装 + hmac.Equal 常量时间校验。

**验证端点（publicMux 唯一豁免，只读）**：
`publicMux.Handle("GET /api/v1/deploy-verify", s.withDeployVerifyAuth(...))` —— 按 `withContextIngestAuth`(server.go:1042) 的"窄机器边界"先例形态；handler 内验签 token → 查 deploy_requests 该 SHA 是否 approved/deploying → 200/403。**零写副作用**，符合红线第 1 条"handler 内校验签名 token"形状；须在 AGENTS.md 红线条目补一句豁免依据（CI runner 无平台 token，签名自证 + 只读 + 限流）。

## 3. GitLab 客户端（internal/codehost/gitlab.go）

照 PipelinesForSHA(GET)/SetProjectVariable(POST) 骨架新增：
- `ListBranches(projectID)` → GET `/projects/:id/repository/branches`
- `TriggerPipeline(projectID, ref, vars)` → POST `/projects/:id/pipeline`，JSON body `{"ref","variables":[{"key","value"}]}`，201 判成功
- `ListRecentPipelines(projectID, perPage)` → GET `/projects/:id/pipelines`
- `PipelineJobInfo` 加 `Runner *struct{ ID int64; Description string }`（解码扩展，不破坏现有消费方）

部署机展示权威源 = deploy job 的 runner.description；`MULTIGENT_DEPLOY_HOST` 仅作链接拼接补充。

## 4. Mattermost 审批卡（internal/imbridge + internal/api/chatops_handlers.go）

- `task_thread_projection_service.go` 新增 `PostDeployApprovalCard` + `FormatDeployApprovalAttachment`：复用 `createPostWithProps`(:653)/`resolveMMTarget`(:698，项目绑定→workspace 兜底)，去掉 task thread 依赖；卡片文本硬编码中英混排（与现状一致，无 i18n）。
- 按钮 integration.context 塞 chatops action token：`act` 扩 `deploy_approve`/`deploy_reject`，payload 的 `tsk` 字段塞 deployRequestID；TTL 2h。
- 回调 `handleMattermostActionCallback`(chatops_handlers.go:221-412)：路由/验签/防重放零改动；在 approve 分支(:425)旁加 `deploy_*` 分支 → 验 ExpectedStateVersion（用部署单 status 做 ver）→ CAS 推进部署单 → `RemoveCardActionsAndSetStatus` 归档卡片；`isDialogAction`(:338) 不含 deploy_*（直接推进，不弹 Dialog）。
- 触发点：创建端点把单置 pending_approval 后异步发卡。

## 5. CI 模板（两个 yml 统一 patch + 版本 bump）

对 `internal/projecttemplate/files/react_go_fullstack/.gitlab-ci.yml` 与 `react_spring_boot/.gitlab-ci.yml` 做同形 patch：

1. package/deploy 的 rules 各追加 `- if: $CI_PIPELINE_SOURCE == "api" && $MULTIGENT_DEPLOY == "1"`（workflow:rules 已含 api source，不用动）。
2. 镜像名修复：package `docker build -t "$CI_PROJECT_NAME:${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"`；deploy `TAG="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"`。
3. deploy script **首行**加 fail-closed 回查（在 apk 之前）：
   ```yaml
   - |
     DEPLOY_URL="${MULTIGENT_CONSOLE_URL:-http://host.docker.internal:27892}/api/v1/deploy-verify"
     wget -q --header "X-Multigent-Deploy-Token: ${MULTIGENT_DEPLOY_TOKEN}" -O /dev/null "$DEPLOY_URL?pjt=$CI_PROJECT_NAME&sha=$CI_COMMIT_SHA" \
       || { echo "deploy gate: platform approval check FAILED"; exit 1; }
   ```
   （tag 流水线无此变量——`wget` 前加 `[ -n "$MULTIGENT_DEPLOY_TOKEN" ] || echo "tag pipeline, gate skipped"`；tag 路径不受闸门约束，维持发版仪式。）
4. script 末尾加镜像治理（`|| true` 不反噬部署结果；不动 /cache/apk；不用 system prune）：
   ```yaml
   - docker images --format '{{.Repository}}:{{.Tag}}' "$CI_PROJECT_NAME" | tail -n +6 | xargs -r docker rmi || true
   - docker image prune -f --filter "until=168h" || true
   ```
5. 头注释更新（版本号 + 「部署链触发」措辞）。

版本 bump：`template.go:24` ReactGoFullstackVersion 1.2.0→1.3.0、`:27` ReactSpringBootVersion 1.0.0→1.1.0；顺手把 yml 注释与 AGENTS.md:145 的「1.1.0」旧账对齐。**存量项目债**：ciready seedMissing 只补缺不覆盖，改过 yml 的存量项目拿不到新 yml——runbook 里记一条手工替换步骤，不为它做自动迁移。

## 6. 前端（web/）

新文件 `web/src/pages/projects/ProjectDeployPage.tsx`，按已定稿的 UI 原型（docs/deploy-center-prototype.html）裁剪实现：
- 入口链路（5 文件 6 处）：nav-config.ts:47 ProjectNavKey 加 `'deploy'`、:90-100 projectSubNav 加 `{segment:'deploy', icon: Rocket}`（全项目可见，不 adminOnly）、:134-135 正则加 `deploy`、App.tsx:155-171 加路由、zh/en common.json projectNav 加 `"deploy"`；**Sidebar.tsx 零改动**（自动渲染）。
- 相对原型的修正（已评审定稿）：删 API_SECRET 类明文敏感变量行（敏感变量只建 key、值转发 GitLab 后不留存，UI 显示 `***` 占位）；删「容器运行状态」卡（无数据源，换「最近流水线状态」）；审批人下拉改「项目审批开关（项目级布尔，PUT project 随 body 存 approvalRequired）+ 追加审批人」，无「免审批」旁路；空态删「底层资源预热/Compose就绪」虚话，只留预分配端口（真）+ runner 绑定状态（GitLab 可查）；删讲故事数据条与异常横幅回滚按钮（用户裁定）。
- 保留原型的：四状态切换（常态/部署中/空态/探针异常）、部署中驾驶舱置顶（阶段卡=平台事件+job 轮询合成）、commit span 清单（+`git diff --numstat` 行数统计）、台账表格（盒装 hover 风格抄 ProjectRunsPage:475-495，statusCls 抄 :129-140）、日志视窗（阶段 tab；实现为轮询 GitLab job trace API，非 WebSocket）、回滚弹窗措辞（"不 reset，对历史 commit 重新构建部署"）。
- 惯例：useApiJson(path, reloadKey) 读、apiPost/apiPut 写（自动 toast）、showToast/confirmDialog、开关抄 SettingsPage:692-705（button+aria-pressed）、暗色 neutral↔zinc、卡片 `rounded-lg border border-neutral-200/80 bg-white dark:border-zinc-700/60 dark:bg-zinc-900/40`。

## 7. 实现批次与验证

批次（同分支 feature/deploy-center-phase1，基于 fix/overnight-integration，**全部已提交**）：
1. DB 层+DAO+单测（deploy_requests CAS 流转、partial index 并发互斥）→ c6dcec6b
2. GitLab 客户端三方法+PipelineJobInfo.Runner+单测（httptest）→ 60fee882
3. deploy_handlers 七端点+verify token+publicMux 回查端点+AGENTS.md 豁免注记+单测 → eeae0472
4. MM 审批卡+chatops 分支+单测（deployTriggerHook 在 NewServer 接线）→ 864904c4
5. CI yml patch+template bump+ciready 校验不回归验证（跑 ciready 十项单测）→ 45ec2bfc
6. 前端页面+入口+i18n → 3d4bf1ab + b5b4a8f8（span 契约对齐）
7. 跨批次契约修复：CI 闸门回调 project 参数改用 MULTIGENT_PROJECT_NAME（注入变量，兜底 CI_PROJECT_NAME）——平台项目名≠repo slug 的 brownfield 绑定否则必被闸门拒 → 68a2731f

集成验证（2026-10-01 实测）：`go build ./...` 全绿；`go test ./...` 全仓唯一失败=预存 TestDesignProxyAdmitsSignatureTokenWithoutBearer（干净 HEAD 同样失败，豁免）；`npx tsc -b` 除预存 CreateTaskDialog 3 错全绿；`vite build` 通过。

浏览器实弹（VM 部署后，与 VM 验证收尾共享窗口）：ias-auth-center 或一次性项目上：建部署单（无审批）→ trigger → 观察 GitLab rules 命中（**硬点 2 的实弹验证点**）→ deploy job 回查通过 → compose up → 健康卡 UP → 台账留痕；再走一遍带审批路径（MM 卡点击 approve）。

### 7b. 对抗式自审裁定（2026-10-01，独立审查 agent 全量 diff）

审查发现 3 P1 + 5 P2，处置：
- **P1-1 CAS ok 丢弃**（双触发/取消竞态）→ 已修 4bb7d117：两处 CAS 检查 ok，输掉即 409；补 2 条并发回归测试。
- **P1-2 chatops 注入不可达 console URL** → 已修：consoleReachableURL 优先 MULTIGENT_CONSOLE_URL → MULTIGENT_API_URL（启动时从真实监听地址推导）→ 请求 Host 兜底。
- **P1-3 前后端字段名契约断裂**（created_at 等四个 snake_case 键前端读不到）→ 已修：DeployRequest JSON tag 全改 camelCase + 契约测试防回归。
- **P2-2 chatops 触发失败报喜不报忧** → 已修：审批 CAS 提交后触发失败用 deployTriggerFailure 类型区分，卡片如实报"已批准但触发失败"。
- **P2-3 RBAC 项目交叉校验** → 已修：req.ProjectID 必须等于 token claim，且 RBAC 用 req.ProjectID。
- **P2-5 闸门 project 参数假设 repo slug == 平台项目名** → 评审前已修 68a2731f（MULTIGENT_PROJECT_NAME 注入）。
- **P2-1 watcher 30min 超时后 deploying 挂起** → **验收边界**：超长流水线（>30min 排队+构建）占住互斥槽到下次重启；启动自愈 + 2h 查无 pipeline 兜底。计划二期加周期 sweeper 或管理员强制 fail 端点。
- **P2-4 项目级审批开关 TODO**（deployApprovalRequired 恒 false，前端每次手选）→ **验收边界**：一期按显式按钮双路径交付，项目级默认值二期限进 entity.Project。

安全面结论（审查员核实）：verify 端点只读+常量时 HMAC+opaque 403+限流齐备；无 fail-open 路径；敏感变量不进 ledger/审计/卡片/前端错误；chatops deploy 分支完整复用验证链。

## 9. VM 验证收尾现状（2026-09-30 16:00 实测，接手须知）

后台验证 agent（agent_1e4bbe07）**已退出且未跑完全部验证**。生产库实测：
- fa92b65c 二进制已部署（/opt/multigent/bin/multigent mtime 9/30 13:31），multigent 与 bridge 服务 active。
- deploy-verify-09301406 项目：V1 幽灵归档探针任务 t-20260930-3rlxjc 仍 pending（未执行）；V2 资料死锁链 t-20260930-adjusj done_failed（败因 docker sandbox 不可达）；V4 服务健康间接验证（多轮 docker 恢复后服务均 active）。
- **dockerd 在验证期间再次僵死**（`/_ping` 10s 超时、33 线程 sleeping），且 scheduler 在僵死窗口 22 分钟生成 117 个 wakeup 失败任务（风暴），已按 runbook SIGKILL+start 恢复，mattermost 手动 start 后 healthy，调度恢复 done_success。
- **V1/V2 待补验**：dockerd 看门狗+内存上调落地后重跑（V1 是 pending 任务可直接被领取；V2 需重建任务）。
- 风暴教训：dockerd 僵死时 scheduler wakeup 生成速率 ≈ 5/min 持续积累失败任务——看门狗落地前这是必现行为，不必人工清理（任务为终态，不占队列）。

## 8. 风险登记

| 风险 | 等级 | 缓解 |
|---|---|---|
| rules:if 变量可见性 | 中高（文档推断） | 首次实弹必验；兜底 SetProjectVariable 暂写 |
| MM 卡 channel 解析失败（项目无绑定） | 低 | resolveMMTarget 已有 workspace 兜底 |
| 存量项目 yml 旧 | 确定发生 | runbook 手工替换；不自动迁移 |
| deploy job runner 与部署机解耦假设 | 低 | 现状单 runner=部署机；job.runner 字段如实展示 |
| 审批闸门绕过（直接调 GitLab API） | 已接受残余 | fail-closed 回查拦平台外直调；持有 GitLab token 者可绕（威胁模型已在提案声明） |
| watcher 30min 超时 deploying 挂起 | 中 | 启动自愈兜底；二期周期 sweeper（见 §7b P2-1） |

## 10. VM 验收前部署清单（接手须知）

1. 交叉编译 linux-amd64 新二进制（当前 VM 跑 fa92b65c，需含 c6dcec6b..4bb7d117 共 11 commits）并按私有 runbook 部署；multigent 与 multigent-mattermost-bridge 同二进制，**都要重启**。
2. systemd 环境确认：`MULTIGENT_DEPLOY_HOST`（部署机地址，健康探针与"访问应用"直达依赖）；`MULTIGENT_CONSOLE_URL`（可选，兜底已走 MULTIGENT_API_URL）。
3. 目标项目需要：verified remote binding + 模板 yml ≥1.3.0（存量项目 yml 手工替换，不自动迁移）+ deploy/compose.yml + /api/health 端点。
4. 实弹路径见 §7"浏览器实弹"；硬点 2（rules 变量可见性）首个部署必验。

## 11. 实弹部署验证收官（2026-10-01 07:55，全部通过）

**环境拓扑实测（1test/devpulse 项目，写代码时假设已全部证实）**：
- GitLab（容器 gitlab）与 devpulse 部署容器都在 node-2（orb 名 ubuntu-node-2，IP …221）；devpulse 端口发布经 OrbStack 落在 Mac 宿主；**容器发布端口的可达口径 = Mac 在 VM 网络的网关 IP（192.168.139.3）**，不是部署容器所在机器的 IP。
- VM231（orb ubuntu）跑 multigent 平台；systemd 注入 `MULTIGENT_DEPLOY_HOST=192.168.139.3`（平台健康卡与 deploy job 探活同口径）+ `MULTIGENT_CONSOLE_URL`。
- Mihomo 代理把 host.docker.internal 劫持到 fake-IP 0.250.250.254 → job 内探活直连该名必死；这就是 22b6e9d6（探针 host 参数化 + MULTIGENT_DEPLOY_HOST 注入）的由来。1test 实例 yml 已同步 f435ed28（探活 `${PROBE_HOST}:${APP_PORT}` + ps 证据步补 `TAG=`）。

**浏览器验收（全部通过，截图与台账留痕）**：
1. 常态页全要素：导航/发起区/台账诚实展示成功失败/流水线卡 10 条/运行版本/健康 UP/端口 ：28000/访问应用链接。
2. 直触路径：发起确认对话框 → dep-b373eb8d 建单 → 行内触发 → 部署中驾驶舱（并发互斥锁发起区、取消按钮、#1213 created 实时出现）→ **pipeline 1213 失败（test:frontend flaky，同 SHA 1212 全绿）→ 驾驶舱回切常态、台账"失败"——失败终态状态机闭环**。
3. 需审批路径：发起（需审批）→ 台账"待审批"+行内"批准" → 批准 → **自动触发** pipeline 1214 → 8/8 success → 台账"成功"、运行版本更新 07:34——审批后无需再次手动触发，符合设计。
4. 观测 mock 页（/observability）：待确认 6/进行中 9/今日运行 2815/7 天 telemetry 23429 次 5220.5M tokens 渲染正常。

**过程中发现并修复的平台缺陷（851307be）**：lastDeployed 语义 = "最新终态单"（含 failed），失败部署会占据"运行版本"卡并让健康探针照常打 UP——失败单从未改变线上版本。已改为只认 status==success（aggregate + preview-state 两处），`isTerminalDeployStatus` 删除，回归测试钉死。实弹验证：1213 失败后运行版本卡回落到 07:00 成功部署而非 07:16 失败单。

**V1/V2 补验（deploy-verify-09301406 项目，全部通过）**：
- V1 幽灵归档：t-20260930-3rlxjc 实跑 done_success 自动归档 → PUT 重开 pending → 归档章被清（active 视图可见、scope=archived 不再包含）——2026-09-29 ghost archive 修复实弹确认。
- V2 资料链：重建任务 t-20260930-asljp0 + 绑定 asf-58a2809c（requirement_input, required）→ run 带 `/mnt/multigent/assets:ro` 挂载 + `MULTIGENT_TASK_ASSETS_DIR` → 模型 Read vdoc.md 成功 → marker 行 `UNIQUE_MARKER_QWERTY_9277` 原文出现在 run log → mga task complete success。首败（adjusj）败因是当时 dockerd 僵死，非资料链缺陷。
- **探针项目 agent 必须配模型**：verify-agent 最初 model=""→ runner 走 genericInvoker 兜底 = `cat` prompt 即退出（exit 0、秒完成、假 done_success）。PATCH /api/v1/agents/{id} 补 model=claudecode + defaultModelAccountId 后全链真实执行。给一次性验证 agent 建任务前先查这个。

**部署操作教训（本会话实弹踩坑）**：
- Mac 上 `go build` 直接产出 Mach-O；部署 VM 必须 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`（ Mach-O 在 Linux 会以 `posix_spawn: no such file or directory` crash-loop）。已产 851307be linux/amd64 部署 VM231（md5 79bbc518…），旧版备份 multigent.bak-22b6。
- 传文件：Mac `python3 -m http.server` + VM `curl http://192.168.139.3:<port>`；orb push 目标解析不可靠。
- GitLab rails runner：heredoc 写脚本进容器再 `gitlab-rails runner /tmp/x.rb`；项目是 `Project.find(26)` 路径 "1test"；trace 读 `b.trace.raw`。
- deploy-verify 项目存留 t-20260930-adjusj（done_failed，dockerd 僵死时期败因）与 t-20260930-3rlxjc（done_success 后被重开过）在归档区，作历史证据保留。

**遗留边界（不阻塞收官）**：1213 test:frontend 为 1test 项目自身 flaky 测试（DeploymentsPage 深链兜底 fetch 断言），与平台无关；P2-1 watcher 超时挂起与 P2-4 项目级审批开关仍按 §7b 二期计划。
