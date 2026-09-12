# P1.5 存量项目运行时治理与 JVM/Gradle 网络兼容性 — 验收记录

日期：2026-09-12 · 分支 `feat/chatops-live-card-and-d6` · 未推送远端

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

## 6. 未验证边界 / 待决策

- **TLS 拦截代理的自定义 truststore**：当前类型化配置无字段表达，按边界规则未实现 workaround；需要时先扩展 `[network]`/trust 配置。
- **agent 沙箱路径**（非 Preview）的 JAVA_TOOL_OPTIONS 注入已实现并有单测（ProfileDockerArgs），未在本轮做真实 agent run 验收（零 LLM 验收范围）；逻辑与 Preview 共用同一纯函数。
- **批量回填**：仅有 inventory + 单项目审计回填；真实批量迁移按任务书要求另开任务。
- **shutdown SEGV**：未动（证据收集阶段：SIGTERM 时 log writer Close 与 signal-goroutine log.Printf 竞争，完整堆栈+最小复现后再立项）。
- gradle 缓存卷仍未挂入 Preview（P2 既有决定）：每轮首启重新下载 Gradle 发行版，JAVA_TOOL_OPTIONS 解决的是"能否下载"，不是"重复下载"。
- `runtime-jvm21:smoke` 旧 tag 与旧二进制备份（`/opt/multigent/bin.prev-*`）保留未清理。
