# P1.5 存量项目运行时治理与 JVM/Gradle 网络兼容性 — 验收记录

日期：2026-09-12 · 分支 `feat/chatops-live-card-and-d6` · 未推送远端

> **⚠️ 2026-09-12 复审修订**：本记录初版验收存在遗漏（见第 7 节），经复审后以 follow-up 提交 `d95b58f0` 修复三处语义缺陷并补做真实 agent-sandbox 无 LLM 验收。第 1–6 节保留初版事实，第 7–8 节为复审结论与修正后验收。

## 7. 复审发现的验收遗漏与语义缺陷（已全部修复）

初版验收"全绿"但复审发现以下问题，说明当时的检查面不足：

1. **RuntimeProfile 三态语义被破坏（最严重）**：PUT handler 把显式 `"base"` 折叠为 `""` 落库，导致"自动（未声明，继承 agent 偏好）"与"显式项目级 base（必须胜过 agent 偏好）"不可区分；agent 偏好 jvm21 时，用户显式选 base 实际会解析出 jvm21。初版测试甚至把这个错误行为断言成了正确行为（`TestPutProjectRuntimeProfileClearsViaBaseAlias`）。**修正**：显式值经 NormalizeProfile 校验后逐字落库（`d95b58f0`），三态语义：`""`=auto/未声明、`base`=显式项目级 base（胜过 agent 偏好与服务器默认）、`jvm21`=显式 jvm21；前端"自动"与"Base"产生不同的请求与持久化结果。
2. **Inventory 用单个任意 worker 代表整个项目**：初版只取第一个成员的观察值，多 worker 配置不一致时给出误导性结论；且未声明但有 pinned image 的项目被标为 `none`，掩盖了需要治理的事实。**修正**（`d95b58f0`）：返回全部成员的 runtime observations（稳定排序）；成员不一致时 `effectiveSource=mixed`；未声明项目一律 `suggestedAction=review`（pinned image 不再豁免）。
3. **JVM 注入别名不对称**：`ProfileDockerArgs` 严格比较 `cfg.Profile != ProfileJVM21`，而运行时解析走 `NormalizeProfile`（接受 jdk21/java21 别名）——别名配置的 agent 拿不到 JVM 网络参数。**修正**（`d95b58f0`）：先 NormalizeProfile 再判定。
4. **agent 沙箱命令路径零真实验证**：初版仅单测纯函数，未在真实 Docker 上跑过 BuildArgs 产物（见第 8.1 节补做）。

## 8. 修正后验收（d95b58f0，全部通过）

### 8.1 真实 Docker/Agent sandbox 命令路径（无 LLM）

方法：临时 Go probe 链接仓库内 `sandbox.BuildArgs`，对 `entity.DockerSandboxConfig`（flat JSON）生成真实 docker argv，逐场景断言（镜像 + argv 中 `-e JAVA_TOOL_OPTIONS=...` + 容器内实际值），在 VM 上以与 multigent 服务一致的网络环境运行：

| 场景 | cfg JSON | 结果 |
|---|---|---|
| canonical | `{"profile":"jvm21"}` | PASS：镜像 `multigent/runtime-jvm21:2026.9.1`，argv 含 JTO，容器打印 `JTO=[-Dhttps.proxyHost=… -Dhttp.nonProxyHosts=…]` |
| 别名 jdk21 | `{"profile":"jdk21"}` | PASS：同上（NormalizeProfile 对齐后别名生效） |
| 别名 java21 | `{"profile":"java21"}` | PASS：同上 |
| 项目 jvm21 + 显式 image | `{"profile":"jvm21","image":"registry.example/team/jdk-stack:21"}` | PASS：使用 pinned 镜像 **且** 携带 JTO（与 Preview 对称） |
| base 无串扰 | `{"profile":"base"}` | PASS：镜像 `runtime-base:latest`，argv 无 JTO，容器 `JTO=[UNSET]` |

RESULT pass=5 fail=0。

### 8.2 双 profile Preview 复验（三态语义下）

新建 p15v-base / p15v-jvm 两项目，agent Lina 设为 jvm21 偏好后加入两项目：

- **三态回环**：PUT `base` → GET 逐字返回 `runtimeProfile:"base"`（不再折叠）；PUT `jvm21` 同理；inventory 两项目均 `effectiveSource=project / action=none`。
- **显式 base 胜过 agent 偏好（关键反例）**：p15v-base Preview 容器镜像 `runtime-base:latest`、env 无 `JAVA_TOOL_OPTIONS`——agent 的 jvm21 偏好未渗透。
- **显式 jvm21**：p15v-jvm Preview 镜像 `multigent/runtime-jvm21:2026.9.1`、env 含完整 JTO、容器内 JVM 打印 "Picked up JAVA_TOOL_OPTIONS" 且 `System.getProperty("https.proxyHost")` 返回代理地址。
- 两个 Preview HTTP 200（签名 token 鉴权路径）。

### 8.3 修正后门禁

- `go test ./...` 全绿（含新增三态 API 测试、显式 base 反偏好测试、多 worker/mixed/pinned-review inventory 测试、别名对称与 pinned-image BuildArgs 测试）
- `make web`、`make build` 成功；产物部署 VM（multigent sha256 `823731ae…`，mga `3cdab3e5…`），console 200、journal 无 panic

## 1. 交付范围（4 个新提交，基于 P1 的 d582f633）

| 提交 | 内容 |
|---|---|
| `00f5e39d` | **Phase A 可观测与受管配置**：GET `/api/v1/projects`（列表+详情）序列化 `runtimeProfile`；PUT 接受 `runtimeProfile`（指针语义：省略=保留声明值，显式值经 `sandbox.NormalizeProfile` 校验落库，显式 `base` 归一化为空=未声明）；未知 profile 400 fail-closed；项目设置页新增 profile 选择器（自动/base/jvm21）并注明解析优先级（显式镜像 > 项目 profile > agent 偏好 > 服务器默认）；en/zh-CN i18n |
| `02be8c37` | **Phase B 盘点与审计回填**：只读 `GET /api/v1/projects/runtime-inventory`（workspace-admin 限定），逐项目报告声明值（未声明保持空=unknown，绝不从 build 文件推断）、有效运行时来源（镜像自 `taskExecutingAgentRuntime` 的查找顺序：project > pinned image（sandbox.image 与 docker.image 双位置）> agent docker profile > server_default）与保守建议（已声明/pinned=none，未声明=review）；回填走既有 manager-gated PUT，实际变更时写 `project.runtime_profile.update` 审计事件（before/after）；无批量写路径 |
| `4f80d478` | **Phase C 受管 JVM 代理注入**：JVM 不读 HTTPS_PROXY 环境变量 → Gradle wrapper 下载/依赖解析在代理环境挂死（P1 验收实测）。`sandbox.JVMToolchainProxyEnv` 在容器创建时从类型化网络配置派生 `-D` 代理属性（https/http proxyHost/Port、NO_PROXY→http.nonProxyHosts 翻译且强制含 localhost/loopback、畸形 URL 优雅跳过），经 JVM 标准入口 `JAVA_TOOL_OPTIONS` 注入 jvm21 沙箱（BuildArgs）与 jvm21 Preview（profilePreviewEnv）；无代理则不注入；模板零提交、无占位 gradle.properties |
| `b1915e02` | **Preview 冷启动竞态修复**（验收中发现）：`install && (backend) & (frontend)` 的 shell 解析把 {install && backend} 后台化、frontend 立即启动 → 冷工作区与自身 npm install 竞速，vite 缺失 exit 127。改为前置 braced 组确保 install 完成后才启动服务；慢速假 npm 回归测试验证 |

## 2. 测试与构建门禁

- `make test` 全量零 FAIL（含新增：runtime profile API 8 例、inventory 4 例、审计 1 例、JVM 代理 6 子例、安装竞态 1 例）
- `make build` 成功；`make web`（TS/Vite）成功

## 3. 部署证据（可追溯）

| 项 | 值 |
|---|---|
| 部署源码 SHA | `b1915e02`（从提交构建，非工作区） |
| multigent 二进制 sha256 | `59934aa02afb310969580c7c8b1d1d843557d027de1fbd7ef749bfd1540bf71c` |
| mga 二进制 sha256 | `708c9b7414bdb65a5eb962e8a3ae6c9ef5715a4a9b5e1690db3e0572dbe9bc75` |
| jvm21 镜像 tag | `multigent/runtime-jvm21:2026.9.1`（本地 Docker Image ID `6ec252a15fa9`；legacy builder 重建，INCLUDE_DEV_TOOLS=1；未推送，无 registry digest） |

部署后 console HTTP 200、服务 active、journal 无 panic。

## 4. 真实环境验收结果（全部通过）

1. **Inventory 真实数据**：端点列出全部存量项目；p15-jvm（回填后）`declaredKnown=true / effectiveSource=project / action=none`；未声明项目 `effectiveSource=server_default / action=review`。
2. **可观测回环**：PUT jvm21 → GET 返回 `runtimeProfile: jvm21`；非法 profile `jvm` → 400 `validation_failed`。
3. **审计**：control 库 audit_events 出现 `project.runtime_profile.update`（before `""` → after `"jvm21"`，actor admin）；同值重发不再产生新事件。
4. **jvm21 Preview**：容器镜像 `multigent/runtime-jvm21:2026.9.1`；容器内 `JAVA_TOOL_OPTIONS="-Dhttps.proxyHost=… -Dhttps.proxyPort=… -Dhttp.nonProxyHosts=localhost|127.0.0.1|::1|host.docker.internal|…"`（由 JVM 亲自打印 "Picked up JAVA_TOOL_OPTIONS"）；`JAVA_HOME=/opt/multigent/jdk`；JDK 21.0.5；后端 health `{"service":"api","status":"ok"}`。
5. **真 Gradle 客户端路径**（非 curl）：容器内 Gradle 8.9 经 JAVA_TOOL_OPTIONS 代理从 Maven Central 解析 guava 33.0.0-jre → 编译 → 运行输出 `gradle-proxy-ok`。**P1 时必须手工种 gradle.properties 的流程已不再需要。**
6. **base 无串扰**（同一 worker）：base Preview 镜像 `runtime-base:latest`、`JAVA_TOOL_OPTIONS` UNSET、`JAVA_HOME` UNSET、无 java；两个 Preview 后端同时 health ok。

## 5. 验收过程中发现并修复的问题

1. **`runtime-jvm21:2026.9.1` 镜像缺 Go 工具链**（构建时 INCLUDE_DEV_TOOLS=0）→ react_go_fullstack 模板 Preview `go run .` exit 127。已在 VM 以 INCLUDE_DEV_TOOLS=1 重建（Image ID 见上表）。遗留：镜像内容与构建参数无版本化记录，建议 CI 固化。
2. **Preview 冷启动竞态**（见提交 b1915e02）。
3. VM 无 buildx，Dockerfile 的 `--platform=$BUILDPLATFORM` 与 `COPY --from=trust`（额外 build context）在 legacy builder 下失败——用临时 sed + 空 trust stage 绕过完成重建，**仓库 Dockerfile 本身未改**。

## 6. 未验证边界 / 待决策（初版；其中 agent 沙箱路径已在第 8 节补验）

- **TLS 拦截代理的自定义 truststore**：当前类型化配置无字段表达，按边界规则未实现 workaround；需要时先扩展 `[network]`/trust 配置。
- ~~**agent 沙箱路径**未做真实 run 验收~~ → 已在 `d95b58f0` 后补做（第 8.1 节，5/5 PASS）。
- **批量回填**：仅有 inventory + 单项目审计回填；真实批量迁移按任务书要求另开任务。
- **shutdown SEGV**：未动（证据收集阶段：SIGTERM 时 log writer Close 与 signal-goroutine log.Printf 竞争，完整堆栈+最小复现后再立项）。
- gradle 缓存卷仍未挂入 Preview（P2 既有决定）：每轮首启重新下载 Gradle 发行版，JAVA_TOOL_OPTIONS 解决的是"能否下载"，不是"重复下载"。
- `runtime-jvm21:smoke` 旧 tag 与旧二进制备份（`/opt/multigent/bin.prev-*`）保留未清理。
