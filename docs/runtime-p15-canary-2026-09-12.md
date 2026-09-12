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
- **ci_ready**：`mga ci ready --wait 300` → **13/13 checks pass，overall=ready，EXIT=0**。首轮曾因 Multigent 项目记录 `repo` 为空（初始化工作流未回写项目记录，工作流输入 `initialization_request` 亦为空）返回 400 "project has no repository path"；PUT repo=workspace 路径绑定后闸门全绿。注意 agent 把"GitLab 级流水线证据被跳过（remote 未在 Multigent 侧绑定）"如实上报为 step failed——闸门本身判定 ready，是上报口径偏保守，不构成平台缺陷；流水线证据属 ci_ready 语义的可选段。

### 7.3 真实任务运行（worktree 全链路）

- 任务 `t-20260912-5ivqnv`（feature，branchName=feat/p15-standard-init-acceptance）自动创建 git worktree：`workspace/.multigent/worktrees/t-20260912-5ivqnv`，基于 baseCommit 11aaf3f。
- Agent 沙箱（standard path 物化，非手工）：镜像 `multigent/runtime-jvm21:2026.9.1`，env 含完整 `JAVA_TOOL_OPTIONS` 代理属性，`MULTIGENT_RUN_ID=t-20260912-5ivqnv`。
- Agent 在 `/workspace` 内看到模板代码，修改 3 个文件（HealthController 注入 ItemRepository、HealthResponse 增 items 字段、HealthControllerTest 补断言），`./gradlew test` → **BUILD SUCCESSFUL**。任务 `done_success`。
- 注意：agent 需 `GRADLE_USER_HOME=/tmp/gradle-cache` 规避 `/tmp/multigent-home` root 属主问题（运行时环境变量，未改项目文件）——P2 缓存治理时一并收敛。

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

### 7.5 结论

GPT 收口条件满足：**标准初始化工作流 → Agent 目录物化 → 真实任务 → Preview** 全链路在零手工播种下成立；jvm21 声明（模板自动落库）贯穿 agent 沙箱与 Preview 两条执行路径。清理：preview 容器已移除；p15-init-spring 项目与两个任务记录保留作证据。遗留同第 6 节：Gradle 缓存 P2；初始化工作流未回写项目 `repo` 字段（sync 步骤只做 git+远端，项目记录绑定需平台侧补一步——已在 7.2 记录，作为初始化工作流的小缺口待后续提交修复）。
