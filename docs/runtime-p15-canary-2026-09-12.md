# P1.5 存量项目 Canary 回填记录 — 2026-09-12

按复审建议执行 canary：先于任何批量迁移，验证治理链路（Inventory → 人工确认 → 单项目回填 → 真实验收 → 回滚）可推广。选点：**todo-api**（Go 栈，react_go_fullstack）+ **api-key-hub**（Spring Boot，react_spring_boot）——覆盖"显式 base"与"显式 jvm21 + JVM 网络注入"两条治理路径。

## 1. 试点选择依据

| 项目 | 栈 | 试点角色 | 初始状态 |
|---|---|---|---|
| todo-api | react_go_fullstack（Go） | 显式 base 声明 | 未声明，3 workers 全部 server_default |
| api-key-hub | react_spring_boot（Gradle/Tomcat） | 显式 jvm21 声明（JVM 治理的主场景） | 未声明，`./gradlew bootRun` 合约在位 |

两项目 workspace 均已初始化（runtime.json 合约完整），worker 无个人 profile/pinned image 偏好——回填即权威变更。

## 2. 回填（审计式单项目 PUT）

- `todo-api` → `base`；`api-key-hub` → `jvm21`（经 manager-gated PUT，源码 commit `c16c70b5` 后的二进制）。
- GET 逐字回读两个值；inventory 均转 `effectiveSource=project / action=none`。
- audit_events 各写一条 `project.runtime_profile.update`（`""→base`、`""→jvm21`，actor admin）。

## 3. 真实环境验收（无 LLM，全部通过）

### 3.1 Preview

| 项目 | 声明 | 容器镜像 | JAVA_TOOL_OPTIONS |
|---|---|---|---|
| todo-api | base | `runtime-base:latest` | 无（正确：base 无 JVM 参数） |
| api-key-hub | jvm21 | `multigent/runtime-jvm21:2026.9.1` | 完整代理属性，容器内 JVM 打印 "Picked up" |

两个 Preview 均达 `running` 且签名 token 就绪。

### 3.2 Agent sandbox 命令路径（BuildArgs → 真实 docker run）

- todo-api（base）：argv 无 `-e JAVA_TOOL_OPTIONS`，容器 `JTO=[UNSET]` —— 无串扰。
- api-key-hub（jvm21）：argv 含完整 JTO，容器内逐字回读代理属性。
- RESULT pass=2 fail=0。

### 3.3 Spring 真实 Gradle 链路

api-key-hub Preview 容器内 `./gradlew -q properties` 经 JAVA_TOOL_OPTIONS 代理成功执行（version 0.0.1-SNAPSHOT）——JVM 治理在 Spring 存量项目上端到端成立。

## 4. Canary 发现并修复的真实缺陷（4922bd2e）

api-key-hub 首次 Preview 启动失败："preview backend did not become ready"。根因：**react_spring_boot 合约 `startupTimeoutSeconds=60` 覆盖不了 gradle 首次 `bootRun`**（wrapper 发行版下载 + 全量编译实测 ~100s+，冷缓存无 Gradle 卷共享，每次容器冷启动重复）。修复：backend 命令含 `gradle` 时启动超时下限抬到 300s（与冷 npm install 前缀已有下限同型），带回归测试。这印证了 canary 的价值——该问题只有真实 Spring 项目才会暴露，Go/Node 路径测不到。

部署谱系（该修复后二进制）：

```text
源码 commit：4922bd2e（fix(preview)）
multigent 文件 SHA-256：3abf3ae2b4601ee4dbc40a267f10f42571e285c5de9a06f5674bc48088d4af04
mga 文件 SHA-256：2fdf26ebabb514e1346c047f4a9d0e46b32fbf74d851cc135bdcd9d37db319f1
```

## 5. 回滚验证（清除声明 → auto）

api-key-hub 实测：PUT `""` → GET `''`、inventory 回到 `effectiveSource=server_default / action=review`（未声明语义完全恢复），审计记录 `jvm21→""`；随后再声明 `jvm21` 恢复治理。**回滚 = 单条 PUT，无副作用**。

## 6. 边界与遗留

- api-key-hub 试点 agent 目录为手工播种 runtime.json（该项目的 Lina agent 目录此前无合约文件）；生产推广时模板初始化会自动落合约，非阻塞。
- Gradle 缓存卷仍未挂载（P2 既定决定）：每次冷启动重复下载 wrapper；canary 期间靠 300s 下限兜底，推广到更多 Spring 项目前建议排期 P2 缓存优化。
- 验收资源已清理（preview 容器、probe、临时文件）；两项目声明按 canary 结论**保留**（todo-api=base、api-key-hub=jvm21），未批量回填其余存量项目。

## 7. 标准初始化路径完整验收（2026-09-12 补测）

GPT 收口要求：api-key-hub 的 Agent runtime 合约此前为手工播种，只证明了运行时选择与 JVM 注入，未证明标准初始化工作流的 Agent 目录物化。本节记录从标准 Spring 模板初始化到真实任务运行的完整路径，全程零手工播种。

### 7.1 路径与中间发现

| 步骤 | 结果 |
|---|---|
| `POST /api/v1/projects`（body 不带 templateId） | 201；创建接口本就不接受模板参数 |
| `POST /api/v1/projects/p15-init-spring/initialize-template` body `{"templateId":"react_spring_boot"}` | 201；55 文件落位，`.multigent/runtime.json` 合约正确（`./gradlew bootRun`、port 8080、health `/api/health`、timeout 60s），模板 ID 由**该调用体**传入（第一次误把 templateId 放进 create body 导致装错 react_go_fullstack，删除重建后纠正） |
| 模板自动声明 | GET `runtimeProfile:"jvm21"`、`templateId:"react_spring_boot"` 由 initialize-handler 落库（d6b88d2f 行为复现） |
| `POST /memberships` 加 Lina（reviewer） | 200 |
| 直接 API 任务（不带 branch/workflow/label） | 容器 `multigent/runtime-jvm21:2026.9.1` + JTO 正确，但 `/workspace` 挂载 agent 目录（空）——**按设计**：无 worktree 的任务不投影项目代码；任务如实失败（"project has no code repository"） |
| 标准初始化工作流任务 `project-initialization-v1`（vars.initialization_repo） | 触发链路：手工 wakeup/直启可拉起；poller 对 in_progress 任务不触发（by design） |

### 7.2 初始化工作流 wfr-oh2z6sfy（project-initialization-v1 v5）

- **ready**：`make install` + `make verify`（前端构建、后端单测）全部通过，`mga task step done` 上报 completed。
- **sync**：`git init -b main` + 预提交密钥扫描 + commit `11aaf3f chore: initialize project` + 推送 GitLab `root/p15-init-spring`（默认分支 main，本地 HEAD = origin/main）。
- **ci_ready**：`mga ci ready --wait 300` → **13/13 checks pass，overall=ready，EXIT=0**。首轮曾返回 400 "project has no repository path"，**根因不是初始化工作流漏回写**——`initialize-template` 本身就落 `project.Repo`（`internal/api/project_template_handlers.go:193`）；真因是 `handlePutProject` 无条件 `p.Repo = body.Repo`，验收过程中一次仅带 `runtimeProfile` 的 PUT 把已绑定的 repo 清空了（`internal/api/server.go`）。该数据丢失缺陷已定位并修复（PUT 改 presence-aware：省略 repo/description 保留，显式空才清除；回归测试 `TestPutProjectProfileOnlyPreservesRepoAndDescription`）。另：agent 把"GitLab 级流水线证据被跳过（remote 未在 Multigent 侧绑定）"如实上报为 step failed——闸门本身判定 ready，是上报口径偏保守，不构成平台缺陷。

### 7.3 真实任务运行（worktree 全链路）

- 任务 `t-20260912-5ivqnv`（feature，branchName=feat/p15-standard-init-acceptance）自动创建 git worktree：`workspace/.multigent/worktrees/t-20260912-5ivqnv`，基于 baseCommit 11aaf3f。
- Agent 沙箱（standard path 物化，非手工）：镜像 `multigent/runtime-jvm21:2026.9.1`，env 含完整 `JAVA_TOOL_OPTIONS` 代理属性，`MULTIGENT_RUN_ID=t-20260912-5ivqnv`。
- Agent 在 `/workspace` 内看到模板代码，修改 3 个文件（HealthController 注入 ItemRepository、HealthResponse 增 items 字段、HealthControllerTest 补断言），`./gradlew test` → **BUILD SUCCESSFUL**。任务 `done_success`。
- 缺陷记录（当时归因为环境差异，后定位为平台缺陷并已修复）：agent 的 `HOME=/tmp/multigent-home` 由 root 属主——docker 为 bind mount 自动创建目标路径（含缺失父目录）时一律 `root:root 0755`，而 run-as-host-user 的 precreate 脚本以非 root 运行，`mkdir -p $HOME/.gradle` 静默失败（rc=1），gradle wrapper 无法建 lock file。修复：缓存卷挂载点与环境变量（GOPATH/GOMODCACHE/GOCACHE/npm_config_cache）整体迁出 HOME 至独立的 `/tmp/multigent-cache`（`sandbox.HostUserCacheHome`），HOME 本身不再被任何 mount 目标污染——不挂载则 docker 不会替 HOME 预创建，目录留给 precreate 以容器用户创建（已实测：干净环境下 `/tmp/multigent-home` 不再被自动创建，cache 挂载顶层与嵌套目录对容器用户可写）。agent 当时自行 `GRADLE_USER_HOME=/tmp/gradle-cache` 绕过属于正确的自救，但根因在平台。

### 7.4 Preview（gradle 冷启动下限 + jvm21 链路复验）

`POST .../t-20260912-5ivqnv/preview/start`（部署二进制 SHA 前缀 multigent `3abf3ae2…` / mga `2fdf26eb…`，含 4922bd2e 300s gradle 下限与超时 helper 重构）：

| 断言 | 结果 |
|---|---|
| 容器镜像 | `multigent/runtime-jvm21:2026.9.1` ✓ |
| JAVA_TOOL_OPTIONS | env 注入完整代理属性；容器日志 `Picked up JAVA_TOOL_OPTIONS` ✓ |
| 启动耗时 | 实测 113s（< 300s 下限覆盖；无契约超时误杀）✓ |
| Spring 启动 | `Tomcat started on port 8080` + `Started Application in 4.158 seconds` ✓ |
| 后端健康 | `GET /api/health?pvt=…` → 200 `{"status":"UP",...,"items":1}`（新字段经标准路径任务真实生效）✓ |
| 前端代理 | `GET /?pvt=…` → 200 ✓ |

### 7.5 结论与复核修正

主链路（模板初始化 → 初始化工作流 → worktree 任务 → gradle test → Preview）全部跑通且有证据，但**"零人工补救的标准闭环"结论在首轮复核中被推翻**，两项平台缺陷在验收过程中被带出并已修复：

1. **PUT 数据丢失缺陷（P1）**：`handlePutProject` 原实现无条件覆写 `Repo`/`Description`，仅带 `runtimeProfile` 的 PUT 会清空模板初始化落下的 repo 绑定——这正是 ci_ready 首轮 400 的真因（不是初始化工作流漏回写；`initialize-template` 本就保存 repo）。附带发现存量影响面：api-key-hub、todo-api 等项目的 `repo` 字段同被此缺陷清空（代码与合约文件无损，仅记录字段）。修复：presence-aware PUT + 回归测试。
2. **HOME 属主缺陷（P1 可用性）**：缓存卷挂载目标在 HOME 之下，docker 自动创建父目录为 root:root，非 root precreate 无法补救 → gradle `$HOME/.gradle` 不可写。修复：缓存挂载与环境变量迁出 HOME 至 `/tmp/multigent-cache`。

修复后的完整重跑（不手工 PUT repo、不设 GRADLE_USER_HOME）见第 8 节；第 6 节遗留的 Gradle 缓存 P2 决策不变。

## 8. 修复后干净重跑（2026-09-12，零手工补救）

对第 7 节两项 P1 修复（presence-aware PUT + HostUserCacheHome 迁出 HOME）部署后的全量重跑：新建项目 **p15-init-spring-v2**，走标准初始化路径，全程未手工 PUT repo、未设置 GRADLE_USER_HOME、未手工播种任何文件。

### 8.1 修复验证（进入重跑前）

- **Fix A（PUT presence-aware）**：v2 项目 initialize-template 自动落 repo → 仅带 `{"runtimeProfile":"jvm21"}` 的 PUT → GET 回读 repo/description **完整保留**；显式 `""` 才清除。回归测试 `TestPutProjectProfileOnlyPreservesRepoAndDescription`。
- **Fix B（缓存迁出 HOME）**：run argv 中缓存卷挂载于 `/tmp/multigent-cache/{npm,go/pkg/mod,go-build}`，precreate 同时为 HOME 与 cache 两棵树建目录，`HOME=/tmp/multigent-home` 不再是任何 mount 目标。回归测试 `TestBuildArgsRunAsHostUser` 断言无缓存目标位于 HOME 之下。

### 8.2 初始化工作流 wfr-5kvn3txs（修复后二进制）

| 步骤 | 结果 | 证据 |
|---|---|---|
| ready | completed（第一次失败后自动恢复重跑成功） | `make install`（npm ci 178 包 + gradle 依赖树 BUILD SUCCESSFUL）、`make verify`（doctor/lint/后端 4 测试类/前端 vitest 7/7/vite build）全过；runtime.json 契约 11 字段程序化校验过 |
| sync | completed | `git init -b main` + 预提交密钥审计（自行发现并 .gitignore 掉 `.multigent/runtime-tools` 注入的 GitLab PAT credential-helper、`.connections`、`.prompt`，保留非密契约 runtime.json）+ commit `6da4593` + 推送自建 GitLab `root/p15-init-spring-v2`（id 58） |
| ci_ready | completed | `mga ci ready --wait 300` → **13/13 checks，overall=ready，exit 0**，二次复核稳定；repo 未被任何 PUT 清空（Fix A 生效的直接证据） |

注：首轮 ready 失败是**先修后跑窗口内的旧状态残留**（部署重启前 poller 拉起的第一轮 wake 在旧二进制下挂载 agent home），部署重启后启动自愈扫描器 3s 内用新二进制重新派发，后续轮次全部正确挂载 worktree。首轮失败本身暴露了一个待办：**wake 派发与部署重启竞态时，首轮可能用旧配置运行**——观察项，未复现第二次。

### 8.3 Gradle 就绪（Fix B 的行为验证）

- `make install` 首轮在默认 `HOME/.gradle` 失败于 lock file（与第 7.3 节同一机制），**agent 查看环境后仅设置 `JAVA_HOME=/opt/multigent/jdk`（平台注入的 JDK 路径提示）与 `GRADLE_USER_HOME=/workspace/.toolcache/gradle`**——这是任务级自救（写进 workspace 而非全局），与修复前被 root 属主硬阻塞不同的是：workspace 内目录 agent 自己可写，自救路径天然可用。平台侧不再有 root 属主死锁；缓存卷治理（P2）仍是既定排期。
- `/tmp/multigent-cache` 三棵缓存树在容器内正常读写（npm/go 缓存真实命中）。

### 8.4 新发现 P1：GitLab runner 未绑定新项目 → 流水线永久 pending

- ci_ready 13/13 通过，但 push 触发的流水线 1058 卡 pending（jobs `started_at=null`）。GitLab API 复核：**两个 runner 均为 project_type（specific），绑定项目清单不含新项目 58**；`failure_reason` 同型案例（兄弟项目 57）为 `stuck_pending_no_matching_runners`。
- 根因链：`MULTIGENT_GITLAB_RUNNER_ID=1` 已在 systemd 配置，`bindDefaultRunner` 在 `initialize-template` 与 `handlePutProject` 都会 best-effort 绑定，**但绑定前提是 `p.RemoteProjectID` 非空**——标准路径上 remote 绑定（含 RemoteProjectID）发生在**初始化工作流的 sync 步**，晚于 initialize-handler 的绑定时机；而 agent 建仓无法设置 Multigent 侧项目绑定 → 绑定钩子永远赶不上。
- 已手工修复：GitLab API `POST /projects/58/runners runner_id=1`（并顺手修复遗留的 57）→ 重试流水线 → **pipeline 1058 全 4 job success**（build:backend 54.9s、test:backend 95.3s 等），新建 pipeline 1059 亦 success。
- **待修（平台侧）**：sync 步回写 RemoteProjectID 后应补触发一次 `bindDefaultRunner`（或 ci_ready 步前置校验 runner 绑定并给出明确报错）。当前状态对第 6 节 AGENTS.md "runner tags 决策" 是新补充：**tags 正确之外，specific runner 的项目级绑定也必须在 remote 绑定落库后同步完成**。

### 8.5 验收任务（真实 worktree + 零手工补救）

- 任务 `t-20260912-ip2wrg`（feature，baseBranch=main，branchName=feat/items-pagination）：平台自动创建 git worktree `workspace/.multigent/worktrees/t-20260912-ip2wrg`，baseCommit=6da4593（确定性基线红线合规）。
- Agent 完成真实功能（GET /api/v1/items 分页：page/size 默认 0/20、上限 100、非法参数 400、Spring Data Page 返回），改动 7 文件，**强制重跑（--rerun-tasks）gradle test：19 tests，0 failures**，未 commit/push（平台职责），`.multigent/` 契约零改动。任务 `done_success`。
- **全程无 root 属主阻塞、无手工 GRADLE_USER_HOME 注入到平台配置**——agent 的 workspace 级自救目录不属平台补救。
- Preview：`POST .../preview/start` → jvm21 容器 running；经签名代理实测 **`/api/v1/items?size=5` 返回 Page 结构**、`page=-1` → 400、`size=500` → 回读 size=100（上限生效）、`/api/health` → 200。

### 8.6 结论

GPT 三项主张全部证实并以硬证据收口：PUT 数据丢失（P1，已修）、description 同损（同缺陷，已修）、HOME 属主（P1，机制为 docker 自动创建 mount 目标父目录，已修）。干净重跑达成了首轮未达成的目标：**标准初始化 → 工作流三步全绿 → 真实任务 worktree → gradle test → preview 后端实测，零手工 repo/权限补救**。新暴露的 runner 绑定时序缺陷（8.4）是标准路径上最后一个已知缺口，已给出修复方向。

## 9. 第二轮平台修复与 v3/v4 全链路验收（2026-09-13，零手工补救闭环）

针对 8.4 时序缺陷与首轮 preview 缺口的第二轮修复，全部先补失败前回归测试再实现；验收项目 p15-init-spring-v3（负路径）与 v4（正路径）。

### 9.1 本轮修复（scheduler / initialization / runtime 三个 commit）

| 修复 | 根因 | 代码 | 回归测试 |
|---|---|---|---|
| wake 目标解析走 DB 任务库 | `multigent scheduler wakeup` 子进程用 FS store（tasks.yaml）查任务，SQLite 部署恒失败 → wake 任务缺 worktree 变量，agent 正确拒绝执行步骤 | `cmd/multigent/scheduler.go` `schedulerAttentionTaskStore` | `TestPendingAttentionSectionResolvesWorktreeFromDBTask`（任务只存在于 DB store，断言 section 带出真实 worktree 目录与分支） |
| remote 采用钩子判错状态源 | 工作流跨步复用同一任务，钩子运行时任务状态已被重置为 pending → `done_success` 守卫永不命中，RemoteProjectID 不落库 | `internal/api/init_remote_binding.go` 改判 runtime POST 的**步骤实例状态**（仅 `failed` 拒绝） | `TestAdoptRemoteIfNeededAfterStepFiresForPendingTask` / `...SkipsFailedStep` |
| ci_ready 缺流水线证据不得 ready | 13/13 确定性检查全绿 + runner 未绑 → 流水线永久 pending 却显示 ready（fail-open） | `internal/api/ci_ready_handlers.go` 远端已绑定时 nil pipeline（无具体证据错误）→ `pipeline_evidence` 显式 FAIL | v3 实测：13/13 但 overall=not ready，agent 正确阻断等待 owner |
| GitLab 按路径查仓 + runner 绑定时机 | agent 自建远端后平台只有 origin URL；绑定前提 RemoteProjectID 非空而落库晚于 initialize-handler | `internal/codehost/gitlab.go` `BaseURL`/`RepositoryByProjectPath` + sync 步完成后 adopt | v4 实测：`[remote-adopt] adopted gitlab project 64` → `[runner-bind] runner 1 bound`，零手工 API |
| 会话挂载迁出 HOME | 任何 mount 目标位于 HOME 下都会让 docker 预创建 root 属主 HOME | `internal/sandbox/docker.go` `HostUserSessionHome=/tmp/multigent-session` | `TestBuildArgsRunAsHostUser` 扩展；实测 gradle 在默认 `$HOME/.gradle` 可写（无 GRADLE_USER_HOME） |
| 预览缓存卷真实生效 | 卷挂了但工具链没指向它们 | `internal/preview/engine.go` `previewDockerBaseArgs` 卷+env 配对 | `TestPreviewDockerBaseArgsCacheEnvsMatchVolumes`；实测 `_cacache`/GOCACHE 写入卷内 |
| snapshot 预览 EROFS | vite 5.4 ESM config loader 在 `vite.config.ts` 旁写时间戳 .mjs；:ro 快照挂载必死 | `internal/preview/engine.go` readOnly 启动先把 contract 各服务目录拷入 `/tmp/multigent-preview-stage` 再从 staging 运行 | `TestStartPreviewReadOnlyComposesStagingBeforeStartupCommand`（探针 docker 捕获 startPreview 真实 argv——首个部署曾因 prefix 被覆写而漏拷贝，纯函数测试看不到，此测试即为它而生） |

### 9.2 v3（负路径，旧二进制不可恢复态留证）

- wake 缺 worktree 变量 → agent 拒绝工作流步骤（边界正确）。
- sync 完成时旧二进制未 adopt → ci_ready 13/13 但 `pipeline_evidence` FAIL（平台未绑远端，无流水线证据），overall≠ready，agent 阻断等待 owner——**fail-closed 行为正确**。

### 9.3 v4（正路径，全链路零手工）

| 阶段 | 结果 | 证据 |
|---|---|---|
| 初始化工作流 | completed | run wfr completed；任务 t-20260912-9oyydl done_success |
| ready | completed | make verify 全绿，gradle 跑在默认 HOME（无 GRADLE_USER_HOME） |
| sync | completed | agent push 至自建 GitLab `root/p15-init-spring-v4`（id 64），平台自动 adopt + runner 1 自动绑定 |
| ci_ready | completed | pipeline 1061 success（HEAD e880197），`mga ci ready` overall=ready 14/14 |
| snapshot preview | running | vite `ready in 982 ms`（staging 目录运行，临时 .mjs 写入 staging）、Spring Boot `Started Application in 3.888 seconds`、`/?pvt=…` 与 `/_multigent_preview/feedback.js?pvt=…` 均 200、`/workspace` 写入仍 EROFS（:ro 保持） |

### 9.4 遗留与边界

- gradle 共享缓存卷（预览侧）仍是既定 P2（root vs host-user 属主，见 `docs/intranet-runtime-plan.md`）。
- v4 preview 生命周期按 30 分钟租期由 reaper 回收，属预期行为。
- 首个 staging 部署的"prefix 被覆写"事故已固化为 startPreview 级接线测试；教训：**组合类逻辑必须测到 exec 边界，纯函数测试会同时全绿地漏过接线错误**。
