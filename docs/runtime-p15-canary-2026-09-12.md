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
