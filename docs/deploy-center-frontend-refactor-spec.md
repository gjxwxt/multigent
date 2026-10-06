# 部署中心前端重构与完善实施规范 (Frontend Refactor Spec)

> **目标**：将 `web/src/pages/projects/ProjectDeployPage.tsx` 从当前简陋的 MVP 状态，全面对齐并升级至标准高保真交互原型 [`docs/deploy-center-prototype.html`](file:///Users/imac/Documents/code/github/multigent/docs/deploy-center-prototype.html) 的生产级水准。  
> **面向对象**：前端实现 Agent / 开发者。执行完毕后由审查 Agent 对照本规范进行行号级验收。

---

## 0. 核心重构目标与断层清零

| 序号 | 缺失/断层模块 | 重构与落地要求 |
|---|---|---|
| **1** | **独立日志详情模态框 (`DeployLogViewerModal`)** | 新增 Linux 终端黑底视窗，支持 Grep 过滤、自动滚屏切换、4 阶段 Tab 过滤、一键复制与 ESC 快捷关闭。 |
| **2** | **置顶部署中驾驶舱 (`InFlightCockpit`)** | 置顶展示：单号 + 实时耗时计时器（`00:42`）+ 4 阶段全景进度条（API校验 ➔ 镜像构建 ➔ Compose重启 ➔ 探针校验）+ `[查看日志详情]`。 |
| **3** | **部署环境变量注入编辑器 (`EnvVarsEditor`)** | 支持 Key-Value 动态增删；第一行预置 `APP_PORT`（系统只读锁定）；支持敏感变量密码掩码（Eye 开关）。 |
| **4** | **审批人下拉选择与企业级纯色按钮** | 废弃原先的两个分裂按钮，采用单个「审批人下拉选择器」（不选=免审批，选人=推送 Mattermost）；发起按钮为纯色 `bg-sky-600`，部署中自动切换为置灰禁用的「并发互斥已锁定」。 |
| **5** | **历史台账与一键确定性安全回滚** | 历史台账成功记录右侧增加 `[回滚]` 按钮；弹出 `DeployRollbackModal`，明确不可变生成新单逻辑。 |
| **6** | **客观事实看板 (4 张卡) 与 Header 收敛** | `[探针巡检]` 与 `[访问运行中应用 ↗]` 提升至页面 Header；4 张指标卡纯粹展示：运行版本、健康探针 (SSOT)、宿主机端口、容器/最近耗时。 |
| **7** | **首次未部署纯净居中空态** | 未部署过时，垂直居中渲染 Hero 空态卡片；点击「发起首次部署」平滑展开配置表单，拒绝与空白表格生硬堆叠。 |

---

## 1. 详细组件拆分与实现细节

建议在 `web/src/pages/projects/` 或其子目录下按以下结构拆分实现，或在 `ProjectDeployPage.tsx` 内保持高内聚实现：

```
web/src/pages/projects/
└── ProjectDeployPage.tsx
    ├── Header (面包屑 + 标题 + Production Live 徽章 + 探针巡检 + 访问运行中应用)
    ├── InFlightCockpit (置顶部署中驾驶舱)
    ├── RealityDeck (4 张客观事实指标卡)
    ├── DeployAlertBanner (探针失败时 502 报警横幅)
    ├── DeployConsole (新建部署控制台：分支/SHA/审批人/Diff/环境变量/部署按钮)
    ├── DeployLedgerTable (历史审计台账与回滚入口)
    ├── DeployRollbackModal (回滚确认模态框)
    ├── DeployLogViewerModal (独立终端风格日志视窗)
    └── DeployEmptyHero (垂直居中首次未部署卡片)
```

---

### 1.1 页面头部与 4 张客观事实看板 (Reality Deck)

#### Header 布局
* **左侧**：面包屑 `项目 > {projectId} > 部署中心` + 主标题 `生产环境部署` + 状态 Badge（健康时为绿色的 `Production Live`，探活异常时为红色的 `Service Degraded`）。
* **右侧操作组**：
  1. `[探针巡检]` 按钮：点击调用 `POST /api/v1/projects/:id/deploy/probe` 或重新 GET 刷新健康状态。
  2. `[访问运行中应用 ↗]` 按钮：当 `deployHostConfigured` 为 true 且 `deployPort` 存在时，高亮为 `bg-sky-600` 外链按钮；未配置时置灰提示。

#### 4 张客观事实卡 (4-Card Grid)
```tsx
<div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
  {/* 卡 1: 线上活跃版本 */}
  {/* 展示 lastDeployed.sha (shortSha) + lastDeployed.branch + "部署于 X 时间前 · 发起人 XXX" */}

  {/* 卡 2: 实时健康探针 (Health SSOT) */}
  {/* 展示 200 OK + 延迟 ms (如 38ms) + 呼吸脉冲绿点；异常时展示 502/Down 红色状态 */}

  {/* 卡 3: 部署宿主机 & 端口 */}
  {/* 展示 runner 节点名 (如 runner-01) + 端口 (如 :28001) + IP 地址 */}

  {/* 卡 4: 容器运行状态 / 最近耗时 */}
  {/* 展示 "1 实例运行中" + 最近一次成功部署耗时 (如 2m 10s) */}
</div>
```

---

### 1.2 置顶部署中驾驶舱 (`InFlightCockpit`)

当 `inflight` 存在且状态处于 `pending_approval` / `approved` / `deploying` 时，在 4 张指标卡上方置顶展示：

1. **顶栏信息**：
   - 动画蓝点呼吸灯 + `正在执行部署流水线` 标题 + 部署单号 Badge（如 `dep-20260930-04`）。
   - 运行耗时时钟（计算自 `createdAt` 或 `startedAt` 的秒数，格式化为 `mm:ss`）。
   - `[查看日志详情]` 按钮（点击打开 `DeployLogViewerModal`）。
   - `[取消部署]` 按钮（点击二次确认调用 `POST .../cancel`）。
2. **关键元数据条（3 列）**：
   - `目标分支 & 基线`（如 `main @ e5f6a7b`）。
   - `提交说明`（如 `feat(auth): 新增多因素认证验证中间件`）。
   - `触发与执行宿主`（如 `admin · Runner: runner-01`）。
3. **4 阶段全景进度卡片流 (4-Stage Progress Grid)**：
   - `1. API 触发校验`：权限、并发锁、参数校验（已完成，显示耗时如 `0.8s`）。
   - `2. 镜像构建打包`：Docker 镜像编译与打包（已完成/执行中/等待）。
   - `3. Compose 重启`：环境变量载入与容器拉起（执行中带 Spinner 高亮）。
   - `4. 端口探针校验`：主动探测 `:APP_PORT/api/health` 连通性（等待/探活中）。

---

### 1.3 新建部署控制台 (`DeployConsole`) 与环境变量编辑器

1. **分支与基线选择**：
   - `部署目标分支`：`<select>` 下拉选择（默认 main）。
   - `目标 Commit 基线`：展示选定分支的最新 Commit SHA（带 `HEAD 最新` 标识）。
2. **审批人下拉选择 (Approver Select)**：
   - 字段名：`指定审批人 (Approver)`（可选）。
   - 选项：
     - `<option value="">无需审批 (直接触发部署)</option>`
     - `<option value="member_id">成员姓名 (角色/职务)</option>`
   - 提示文本随选择动态切换：未选时提示“直接触发流水线”；选定后提示“将推送 Mattermost 审批卡片”。
3. **待发布代码变更清单 (Pending Changes)**：
   - 展示相比线上基线的增量 commit 列表（Commit SHA、提交信息、提交者、时间）。
4. **部署环境变量注入 (Environment Variables Injection)**：
   - **系统预留行**：Key=`APP_PORT`, Value=`{agg.deployPort}`, 灰色背景只读锁定 + `系统只读` 盾牌图标。
   - **动态自定义行**：支持用户点击 `[+ 添加变量]` 动态新增/删除键值对。
   - **密码掩码**：支持敏感值密码框（`<input type="password">`）与小眼睛图标明文/密文切换。
5. **部署操作控制条**：
   - 提示：`部署单将写入不可变审计台账，单项目全局并发互斥保护已开启`。
   - 按钮：
     - 未在部署中：渲染企业纯色按钮 `[确认发起部署 (Deploy Now)]`（`bg-sky-600 hover:bg-sky-700 text-white`）。
     - 部署进行中：自动切换为禁用的灰色按钮 `[部署进行中 · 并发互斥已锁定]`。

---

### 1.4 历史审计台账与一键确定性回滚 (`DeployLedgerTable` & `DeployRollbackModal`)

1. **历史台账表格列**：
   - `部署单号 / 环境`：如 `dep-20260930-03` + 生产环境。
   - `目标 Commit & 增量`：如 `main@a1b2c3d` + `+1 commit`。
   - `触发 / 审批人`：如 `admin` · `免审批` 或 `审批: 李主管 (Mattermost)`。
   - `耗时`：如 `2m 10s`。
   - `探针结果`：如 `● 200 OK (42ms)` 或 `● 探活失败 (Timeout)`。
   - `状态`：成功 / 失败 / 已归档。
   - `操作列`：
     - `[查看日志]`：点击打开对应部署单的 `DeployLogViewerModal`。
     - `[回滚]`（对已归档的稳定版本可用）：点击打开 `DeployRollbackModal`。
     - `[GitLab ↗]`：外部链接直达 GitLab Pipeline 页面。
2. **回滚确认模态框 (`DeployRollbackModal`)**：
   - 标题：`确认回滚生产环境？`
   - 副标题：`遵循不可变原则：回滚将作为一张新部署单生成并审计`。
   - 回滚详情卡片：目标单号、目标 Commit SHA、审批策略。
   - 明确说明：“该操作不会在 Git 仓库中执行任何 reset 动作，而是重新触发针对历史 Commit 的打包与 Compose 部署流程”。
   - 按钮：`[取消]` + `[确认发起回滚部署]`（`bg-amber-600 hover:bg-amber-700`）。

---

### 1.5 独立终端风格日志视窗 (`DeployLogViewerModal`)

1. **弹窗外观**：
   - 遮罩层：`bg-black/75 backdrop-blur-md`。
   - 主体容器：`max-w-5xl h-[85vh] rounded-2xl border border-neutral-800 bg-zinc-950 text-zinc-100 font-mono`。
2. **顶栏工具组**：
   - 左侧：单号 `dep-xxx` + 实时拉取状态 Badge + Target SHA + Host。
   - 右侧：
     - 搜索输入框：`过滤关键字 (grep)...`（实时前端高亮/过滤）。
     - 自动滚屏 Checkbox（默认选中，内容更新时自动 `scrollTop = scrollHeight`）。
     - `[复制]` 按钮：将当前日志文本写入剪贴板并 Toast 提示。
     - `[X]` 关闭按钮（同时绑定 `ESC` 快捷键监听）。
3. **阶段切换 Tab**：
   - `全部日志 (All)` / `1. API 校验` / `2. 镜像构建打包` / `3. Compose 重启` / `4. 端口探针校验`。
4. **终端内容展示区**：
   - 带有 ANSI 颜色解析或语义化颜色高亮（如时间戳灰色、Stage 信息绿色/蓝色、探针警告黄色、错误红色）。

---

### 1.6 首次未部署居中 Hero 空态 (`DeployEmptyHero`)

当 `!lastDeployed && !inflight && agg?.deployPort` 时展示：
1. 整个工作区垂直居中（`min-h-[75vh] flex flex-col items-center justify-center`）。
2. Hero 卡片：
   - 居中火箭图标徽章。
   - 主标题：`该项目尚未部署运行应用`。
   - 说明：已分配专属宿主机端口 `{deployPort}`，运行环境已就绪。
   - 三小卡：`预分配端口 :28001` / `调度执行节点 runner-01` / `运行环境 Compose 就绪`。
   - 底部全宽按钮：`[发起首次部署 (Initial Deploy)]`。
3. **交互动作**：点击后将状态标记为 `showInitialConfig = true`，平滑滚动至新建部署控制台。

---

## 2. API 契约对齐核对表

| 前端动作 | 调用的后端接口 | 请求 Body 结构 |
|---|---|---|
| **拉取部署总览** | `GET /api/v1/projects/:id/deploy` | 无 |
| **拉取分支列表** | `GET /api/v1/projects/:id/deploy/branches` | 无 |
| **创建部署单** | `POST /api/v1/projects/:id/deploy/requests` | `{ branch: string, sha: string, approverId?: string, approvalRequired?: boolean, vars?: Record<string, string> }` |
| **批准部署单** | `POST /api/v1/projects/:id/deploy/requests/:depId/approve` | `{}` |
| **驳回部署单** | `POST /api/v1/projects/:id/deploy/requests/:depId/reject` | `{}` |
| **取消部署单** | `POST /api/v1/projects/:id/deploy/requests/:depId/cancel` | `{}` |
| **手动触发流水线** | `POST /api/v1/projects/:id/deploy/requests/:depId/trigger` | `{}` |
| **拉取历史台账** | `GET /api/v1/projects/:id/deploy/requests` | 无 |
| **获取部署日志** | `GET /api/v1/projects/:id/deploy/requests/:depId/logs` | 无（或直接读取聚合响应） |
| **手动健康探活** | `POST /api/v1/projects/:id/deploy/probe` | 无 |

---

## 3. 验收标准与交付检查点 (Checklist)

前端 Agent 完成修改后，必须通过以下命令验证无报错：

1. **类型检查**：
   ```bash
   cd web && npx tsc -b
   ```
   *必须 0 报错通过。*

2. **前端打包**：
   ```bash
   cd web && npm run build
   ```
   *Vite 打包构建产物顺利输出到 `web/dist`。*

3. **完整工程构建**：
   ```bash
   make build
   ```

4. **视觉与交互核对**：
   - [ ] 打开页面，Header 右侧包含 `[探针巡检]` 与 `[访问运行中应用 ↗]`。
   - [ ] 4 张客观事实卡整齐排列，无突兀的大按钮卡。
   - [ ] 部署中时，顶部置顶 4 阶段进度条与计时器，点击可打开全屏 Linux 终端日志。
   - [ ] 新建部署支持环境变量输入，`APP_PORT` 处于系统只读状态。
   - [ ] 审批人采用下拉选择器，部署按钮为企业纯色，并发锁触发时自动置灰。
   - [ ] 历史表格支持点击 `[回滚]` 弹出不可变回滚确认窗。
   - [ ] 空态垂直居中，点击平滑展开部署表单。
