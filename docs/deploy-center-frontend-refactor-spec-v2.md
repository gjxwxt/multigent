# 部署中心前端重构实施规范 v2（Frontend Refactor Spec v2）

> **本版取代 v1**（`deploy-center-frontend-refactor-spec.md`）。v1 从 HTML 原型正推产生，把原型演示数据当成了后端契约；v2 经对抗式审查 + 消融实验（逐模块剥离真实后端核对）修正。差异总表见 §0，后端契约事实基准见 §2——**前端实现遇到与本规范冲突的"原型行为"，一律以 §2 为准**。
>
> **面向对象**：前端实现 Agent。执行完毕由审查 Agent 对照 §4 验收。

---

## 0. 与 v1 的差异总表（消融裁定映射）

| v1 模块 | 裁定 | v2 处置 |
|---|---|---|
| §1.1 探针巡检按钮 `POST /deploy/probe` | **P0：端点不存在** | 改为 `[刷新状态]`，重新 GET aggregate（探活本就内嵌其中） |
| §1.1 卡 3 "runner-01 + IP" | **P0：字段不存在** | 改为 部署机配置状态（布尔）+ 端口 |
| §1.1 卡 4 "最近耗时 2m10s" | **P0：finished_at 恒空** | 改为 "最近流水线" 卡（id/状态/GitLab 直达） |
| §1.2 四阶段进度条（API校验➔构建➔Compose➔探针） | **P0：无阶段级状态源** | 改渲染真实 `pipelines[].jobs`（lint/test/build/package/deploy 五 stage 八 job）；语义四阶段二期再议 |
| §1.2 计时器起于 `startedAt` | **P0：startedAt 恒空串** | 改用 `createdAt`（实弹验证：startedAt=''） |
| §1.3 审批人下拉 `approverId` | **P0：后端不收、MM 卡发项目频道** | 降级为"审批策略选择器"：不选=免审批，选"需审批"=推送项目绑定频道的 MM 卡（不指定人）；定向到人二期 |
| §1.3 待发布变更清单（逐 commit） | **P1：commitSpan 仅 [基线,目标] 两元素** | 一期显示 `基线 xxx → 目标 yyy`；逐 commit 清单二期（需 GitLab compare API） |
| §1.4 台账耗时列 | **P0：无处可算** | 一期砍列；二期 B1 落库 finished_at 后启用 |
| §1.4 回滚 | **可用**（显式 SHA 创建已支持） | 保留，两步走：create(sha=历史) → trigger |
| §1.5 日志终端模态框 | **P0：无日志数据源** | **一期整体不做**；二期 B2（GitLab job trace 代理）落地后再做 |
| §1.6 Hero 空态 | 可用 | 保留，纯前端改造 |
| §2 API 契约表 | **P0：两处编造** | 以本版 §2 为准 |
| §3 验收 "tsc 0 报错" | **P2：预存 3 错** | 改为"不新增错误"（CreateTaskDialog 3 错豁免） |

**不变的裁定**：RealityDeck、EnvVarsEditor、DeployEmptyHero、Header 收敛——后端支撑完整，一期直接开工。

---

## 1. 一期规范（纯前端，后端零改动）

### 1.0 组件结构

```
web/src/pages/projects/
└── ProjectDeployPage.tsx        （现 653 行，拆分为下述子组件，同目录新文件）
    ├── deploy/DeployHeader.tsx          面包屑 + 标题 + Live/Degraded 徽章 + 刷新/访问应用
    ├── deploy/InFlightCockpit.tsx       部署中驾驶舱（改造现有置顶区）
    ├── deploy/RealityDeck.tsx           4 张客观事实卡
    ├── deploy/DeployConsole.tsx         新建部署控制台（分支/SHA/审批策略/环境变量/按钮）
    ├── deploy/EnvVarsEditor.tsx         环境变量编辑器（独立文件，逻辑内聚）
    ├── deploy/DeployLedgerTable.tsx     台账 + 回滚入口
    ├── deploy/DeployRollbackModal.tsx   回滚确认模态框
    └── deploy/DeployEmptyHero.tsx       首次未部署居中空态
```

不做 `DeployLogViewerModal`、不建 `DeployAlertBanner`（健康异常已由卡 2 红色态 + Header 徽章覆盖，重复告警制造噪音）。

### 1.1 Header（DeployHeader）

- 左侧：面包屑 `项目 > {projectId} > 部署` + 标题；徽章：`health.status==='up'` → 绿色 `Production Live`；`down` → 红色 `Service Degraded`；`unknown`/null → 灰色 `未配置探针`。
- 右侧：
  1. `[刷新状态]`：调 `reload()`（重新 GET aggregate，探活随 aggregate 自带）。
  2. `[访问运行中应用 ↗]`：`deployHostConfigured && agg.deployPort` 时渲染外链，**URL 固定 `http://{window.location.hostname}:{agg.deployPort}`**（部署端口经宿主发布，与控制台同主机可达——这是当前部署拓扑的实测口径；`deployHostConfigured=false` 时置灰 + tooltip"未配置部署机地址"）。

### 1.2 RealityDeck（4 张卡）

| 卡 | 内容 | 数据源 |
|---|---|---|
| 线上活跃版本 | `shortSha(lastDeployed.sha)` + branch + "部署于 {相对时间} · {createdBy}"（**不用 finishedAt**） | `agg.lastDeployed` |
| 健康探针 | `up` → 呼吸绿点 + `{latencyMs}ms`；`down` → 红点 + reason；`unknown` → 灰"未配置" | `agg.health` |
| 部署机 & 端口 | `:{deployPort}` 大字 + `deployHostConfigured ? "部署机已配置" : "未配置部署机地址"` | `agg.deployPort/deployHostConfigured` |
| 最近流水线 | `#{pipelines[0].id}` + 状态徽章 + `[GitLab ↗]`（`pipelines[0].webUrl`） | `agg.pipelines[0]` |

**禁止**渲染 runner 节点名、IP、容器实例数、部署耗时——后端无此数据。

### 1.3 InFlightCockpit（改造现有置顶驾驶舱）

触发条件不变：`inflight && ['pending_approval','approved','deploying'].includes(inflight.status)`。

1. **顶栏**：呼吸蓝点 + `正在执行部署流水线` / `等待审批`（pending_approval 时）+ 单号 Badge（真实 `dep-<16hex>` 格式，**不**格式化为日期序号）+ 计时器 `mm:ss`（**基准 = `inflight.createdAt`**，本地 `setInterval(1s)`；5s 数据轮询不变）+ `[取消部署]`（二次确认 → `POST .../cancel`，现有端点）。
   - **不放** `[查看日志详情]`（二期）。
2. **元数据条**：`目标 branch @ shortSha`；`发起人 createdBy`；`审批 {approval.required ? '需审批' : '免审批'}`。
3. **进度区（替代虚构四阶段）**：用 `inflight.pipelineId` 在 `agg.pipelines` 中找到对应流水线，渲染其 `jobs` 为真实阶段列表：每 job 一行（stage 分组），状态映射 `success→✓绿` / `running→spinner蓝` / `failed→✗红` / `pending/created→灰`。找不到流水线（尚未 created）时显示"流水线创建中…"。
4. `approved` 状态特殊态：显示"已批准 · 待触发"+ 行内 `[触发]` 按钮（现有行为保留）。

### 1.4 DeployConsole

1. **分支与基线**：分支 `<select>`（`branches`）；基线显示 `branches.find(name).commitId` 的 shortSha + `HEAD` 标识（BranchInfo 自带 commitId，无额外请求）。
2. **审批策略选择器**（替代 v1 审批人下拉）：单个 `<select>`：
   - `无需审批（直接触发部署）` → `approvalRequired: false`
   - `需审批（推送 Mattermost 审批卡至项目频道）` → `approvalRequired: true`
   - 提示文本随选择切换；代码注释注明"定向审批人（approverId）待后端 B3 支持，届时 value 换成 member_id"。
3. **部署区间**（替代逐 commit 清单）：一行文字 `基线 {commitSpan[0].shortSha || '首次'} → 目标 {shortSha(selectedSha)}`；`commitSpan` 只有两个元素（基线=上次成功部署），**不要**把它当逐 commit 列表 map 渲染。
4. **EnvVarsEditor**：
   - 系统预留行：`APP_PORT = {agg.deployPort}`，灰底锁定 + 盾牌图标 + `系统只读`。**提交时必须从 vars 中剔除 `APP_PORT`**（红线：后端会把 vars 原样推进 GitLab CI 变量，覆盖端口镜像会导致 deploy job 绑错端口）。
   - 动态行：`[+ 添加变量]` 增删键值对。
   - 敏感判定：key 大写化后含 `TOKEN/SECRET/PASSWORD/KEY` 任一（对齐后端 `deploySensitiveVar`）→ 输入框 `type="password"` + 眼睛切换。**眼睛只作用于本次新输入**；提示文案："敏感变量仅推送 GitLab CI，平台只存 `***` 掩码"。**禁止**对台账回显的历史 vars 做掩码切换（明文在后端不存在，`***` 是唯一真相）。
5. **操作条**：提示语不变；按钮：空闲 → `[确认发起部署]`（`bg-sky-600`）；inflight → 禁用灰 `[部署进行中 · 并发互斥已锁定]`。

### 1.5 DeployLedgerTable 与回滚

**列**：部署单号（`dep-xxx` mono）/ 目标（`branch@shortSha` + 基线 `← commitSpan[0].shortSha`）/ 发起人 / 状态（成功/失败/待审批/部署中/已取消/已驳回）/**探针结果**（`r.health?.status==='up'` → `● 200 OK ({latencyMs}ms)` 绿；`down` → 红；**无 health 快照（失败单）→ `—`**）/ 操作。

**操作列**：
- `[GitLab ↗]`：`pipelines.find(p => p.id === r.pipelineId)?.webUrl`，找不到不渲染。
- `[触发]`：`approved` 行（现有）。
- `[回滚]`：仅 `status==='success'` 行。点击开 `DeployRollbackModal`：
  - 标题 `确认回滚生产环境？`；副标题 `遵循不可变原则：回滚将作为一张新部署单生成并审计`。
  - 详情：目标单号 / 目标 `branch@shortSha` / 审批策略沿用当前选择器值。
  - 说明文案：**不执行任何 git reset，重新对该历史 Commit 触发打包与 Compose 部署**。
  - 确认动作两步：`POST .../deploy/requests {branch: r.branch, sha: r.sha, approvalRequired: <当前选择>}` → 取响应 `id` → `POST .../deploy/requests/{id}/trigger`。全部现有端点，轮询自然接管后续状态。
  - 确认按钮 `bg-amber-600`。

### 1.6 DeployEmptyHero

条件 `agg && !lastDeployed && !inflight`。垂直居中（`min-h-[75vh]` 居中容器）；Hero 卡：火箭徽章 + `该项目尚未部署运行应用` + `已分配专属宿主机端口 :{deployPort}`；三小卡：`预分配端口 :{deployPort}` / `远端绑定 {branches.length ? '已连接 GitLab' : '未绑定'}`（**不放 runner 节点名**）/ `运行环境 Compose 就绪`；底部 `[发起首次部署]` → `showInitialConfig=true` + 平滑滚动至控制台。现有空态卡逻辑迁移即可。

### 1.7 i18n（一期硬性要求）

现页面 47 个 `projectDeploy.*` key 在 `web/src/locales/{zh-CN,en}/common.json` 中**零落库**，全靠 defaultValue。本次新增子组件后：**所有 key 必须在两个 locale 文件补齐**（含现有 47 个）；验收时 grep 检查。

---

## 2. 后端契约事实基准（实现前必读，与原型冲突以此为准）

### 2.1 端点全集（server.go 实测）

```
GET  /api/v1/projects/:id/deploy                        aggregate 总览
GET  /api/v1/projects/:id/deploy/branches               分支列表
GET  /api/v1/projects/:id/deploy/preview-state          轻量 {lastDeployed, inflight}
POST /api/v1/projects/:id/deploy/requests               创建 {branch, sha, vars?, approvalRequired?}
GET  /api/v1/projects/:id/deploy/requests               台账
POST /api/v1/projects/:id/deploy/requests/:id/approve   {}
POST /api/v1/projects/:id/deploy/requests/:id/reject    {}
POST /api/v1/projects/:id/deploy/requests/:id/trigger   {}
POST /api/v1/projects/:id/deploy/requests/:id/cancel    {}
```

**不存在**：`deploy/logs`、`deploy/probe`、`deploy/rollback`、创建 body 的 `approverId`。探活内嵌 aggregate；回滚 = create(历史 sha)+trigger 两步。

### 2.2 aggregate 响应关键字段（实测）

```jsonc
{
  "deployPort": 28000, "deployHostConfigured": true,
  "health": {"status": "up", "latencyMs": 212},          // unknown 时带 reason
  "lastDeployed": {                                       // 仅 status==success 的单（851307be 语义）
    "id": "dep-<16hex>", "branch": "main", "sha": "...", "env": "production",
    "vars": {"DB_PASSWORD": "***"},                       // 敏感值恒为掩码，永不回明文
    "commitSpan": [ {基线}, {目标} ],                      // 恒两元素，非逐 commit
    "approval": {"required": true, "state": "approved"},
    "status": "...", "pipelineId": 1214,
    "health": {...仅成功单有快照, 失败单无},
    "createdAt": "...", "startedAt": "", "finishedAt": ""  // 后两者恒空，勿用
  },
  "inflight": {同上结构} | null,
  "pipelines": [ {"id":1214,"ref":"main","sha":"...","status":"success","webUrl":"https://.../pipelines/1214",
                  "jobs":[{"name":"deploy","stage":"deploy","status":"success","runnerDescription":"AITP Local CI Runner"}, ...8个] } ]
}
```

真实 pipeline stage 集：`lint / test / build / package / deploy`（各 2 job 共 8）。平台判定只看 pipeline 整体状态 + 独立探针。

### 2.3 行为语义

- 同项目**并发互斥**：部分唯一索引 `uq_deploy_requests_inflight`，第二张单 409。
- `approved` 不自动触发；`approve` 端点会自动触发（CAS 后共用 trigger 路径）。
- 轮询节奏现状：deploying/pending_approval/approved 时 5s，否则 30s（保留）。
- 创建后 `vars` 中敏感 key 被 GitLab CI 推送 + 本地掩码；非敏感 key 原样推 GitLab（`MULTIGENT_DEPLOY_VAR_` 前缀）。

---

## 3. 二期后端前置任务（独立立项，前端勿提前依赖）

| # | 任务 | 内容 | 解锁的前端能力 |
|---|---|---|---|
| B1 | 时间戳落库 | `UpdateDeployRequestStatus` 扩展或在 trigger CAS / watcher finish 路径补 `started_at`/`finished_at` 写入（列已存在，恒空） | 台账耗时列、精确秒表 |
| B2 | 日志链路 | `GitLabHost.JobTrace(jobID)`（GET /projects/:id/jobs/:id/trace）+ 平台代理端点 `GET .../deploy/requests/:id/logs`（checkProjectAccess + redact + 大小上限） | DeployLogViewerModal（终端视窗/grep/自动滚屏/ESC/复制；Tab 按真实 stage 或全量） |
| B3 | 审批人定向 | `entity.Project.ApprovalRequired` 持久化（batch-4 TODO 已登记）+ 创建 body 收 `approverId`，MM 审批卡定向（DM 或 @） | 审批策略选择器升级为审批人下拉 |
| B4 | 逐 commit 清单 | GitLab compare API 封装 + aggregate 扩展或独立端点 | 待发布变更清单逐 commit 渲染 |

## 4. 验收标准（修正版）

1. `cd web && npx tsc -b` —— **不新增错误**（预存 CreateTaskDialog 3 错豁免）。
2. `cd web && npm run build` —— Vite 产物正常输出。
3. `make build` —— 完整工程构建通过。
4. i18n：`projectDeploy.*` 全部 key 在 zh-CN 与 en 双语落库，语言切换无 fallback 英文。
5. 浏览器实弹（1test 项目）：
   - [ ] Header 徽章随健康态切换；`[访问运行中应用 ↗]` 指向 `:{deployPort}` 可打开 devpulse。
   - [ ] 4 卡无任何虚构数据（无 runner 名/IP/耗时）。
   - [ ] 发起直触 → 驾驶舱出现真实 8 job 进度 + 计时器走秒 → 取消路径可用。
   - [ ] 需审批 → 批准自动触发全链（对齐已验收的 1214 行为）。
   - [ ] EnvVarsEditor：APP_PORT 锁定且提交剔除；敏感 key 密码框 + 眼睛仅作用新输入。
   - [ ] 台账成功行 [回滚] → modal 文案含"不可变/新部署单/git 不动" → 确认后生成新单并自动触发。
   - [ ] 失败单行探针结果显示 `—`（不显示 200 OK）。
   - [ ] 空态垂直居中，点击平滑展开表单。
6. **禁止事项**（审查 Agent 逐条核）：渲染 runner 节点名/IP/部署耗时/日期序号单号/语义四阶段进度条/日志按钮；对历史 vars 做明文切换；提交含 `APP_PORT` 的 vars。
