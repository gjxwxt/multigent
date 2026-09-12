# 执行者盲点实录 — 只有真正跑过全链路的 Agent 才会踩到的点（2026-09-14）

> 来源：p15 双栈 canary（runtime-p15-canary-2026-09-12.md）两轮验收的实际执行过程。
> 收录标准：**审查代码看不出来、只有跑到运行时才暴露**的问题。每条带"当时怎么发现的"与"下次怎么快速跳过"。
> 与 HANDOFF §10.17 互补：那边记结论，这边记"为什么会在那里摔跤"。

---

## A. 优先级排序：未完成 Feature 与待逐步实现项

按"对标准交付闭环的风险 × 迁内网紧迫度"排序。每项独立可 commit、可回滚。

| # | Feature | 现状 | 建议步序 | 风险 |
|---|---|---|---|---|
| 1 | **P2 双栈并发 soak（2 项目 × 1 任务）** | 清单已备（HANDOFF §10.18），未执行 | 新会话照单开跑，一轮通过后扩 ×2 | 中：npm/gradle 并发双写共享卷未验证 |
| 2 | **runtime-jvm21 镜像固化**（intranet-plan P1） | 镜像可用但来自手工构建，无版本化构建流水 | 先把 Dockerfile + 构建参数进 repo，再谈内网 Harbor | 中：当前 `2026.9.1` tag 无人能复现 |
| 3 | **gradle 共享缓存卷**（预览/agent 两侧属主方案） | 遗留卷 `multigent-gradle-cache` 已建但**平台代码零挂载** | 并入 intranet-plan P2 受管缓存根目录，先删遗留卷防误判 | 低（当前容器内 HOME 可写已够用） |
| 4 | **partial 绑定失败重试入口**（HANDOFF 10.16 §5.1） | API 可恢复但 UI 无入口 | 小 UI 迭代 | 低 |
| 5 | **preview 端口池可观测性** | findFreePort 盲分配，无占用统计 | soak 记录实测端口分布后再定方案 | 低 |
| 6 | **探测子命令 `multigent runtime probe`**（intranet-plan §2.4） | 未实现 | 按 P0 契约实施 | 低 |

**明确不做**（防越界，与 intranet-plan 边界一致）：不改 Dockerfile 默认值、不提前动卷挂载、不引入 mise。

---

## B. 执行者盲点（代码审查发现不了的）

### B1. "纯函数测试全绿"会漏掉接线错误 —— 组合逻辑必须测到 exec 边界

**经过**：snapshot 预览的 staging 修复，我先写了 `stageReadOnlyServicesPrefix`（生成拷贝命令）和 `readOnlyStagedRuntimeSpec`（改写运行目录）两个纯函数，各配测试，全绿。首个部署把"staged cd 目标"发出去了，"拷贝步骤"却没发——engine 里 prefix 先组装、随后被第二次 `StartupCommand` 赋值**覆写**。容器死在 `cd /tmp/multigent-preview-stage/...`，两个单测依然全绿。
**教训**：两块各自正确的纯函数，拼接处（谁先谁后、谁的返回值覆盖谁）是零覆盖盲区。**修复方式**：测试里用 PATH stub 伪造 `docker`，捕获 `startPreview` 实际传给 docker 的完整 argv 再断言（`TestStartPreviewReadOnlyComposesStagingBeforeStartupCommand`）。
**泛化规则**：凡是"A 的输出拼进 B 的输入"的组合代码，必须有一条**捕获真实 exec 参数**的测试；伪造器只需一个记录 argv 的 shell 脚本。

### B2. 部署后验证二进制"是不是真的在跑新代码"，hash 不可靠，行为探针才可靠

**经过**：本轮曾一度怀疑部署没生效（staging 修复部署后容器命令依旧旧样），绕了远路查 docker events、反复 `sudo cp`。最终确认部署是新的、错的是逻辑。两个已知坑的叠加：
- `go build` 带 VCS 戳，同源码两次构建 hash 必不同 → hash 只能证"repo 产物 == VM 部署"，**不能**证"行为 == 源码"（HANDOFF §10.11 已记，但实操中仍然差点再踩）。
- 正确姿势：部署后立刻做**一次行为级探针**（如新逻辑特有的字符串/新端点/新日志行），探针通过才算部署完成。本轮 staging 场景的行为探针是"容器 Cmd 里应出现 `multigent-preview-stage`"。
**泛化规则**：部署 SOP 的最后一步必须是可断言的行为探针，不是 `is-active`。

### B3. docker 诊断三件套的正确顺序（先 events 后 inspect 后 logs）

**经过**：容器 `rm -f` 后 inspect 拿不回 Cmd，logs 随容器消失。failInstance 抓的是"死前 120 行日志"，但**容器的 argv 本身**只能从 docker events 的 create 前后窗口或还未删除的容器 inspect 拿到。
**顺序**：① `docker events --since`（带 `--filter image=` 缩小面，注意加 `timeout` 防止 events 挂住不返回）→ ② 乘容器还在时 `docker inspect .Config.Cmd` → ③ 才是 logs。本轮最贵的一次绕路（约 20 分钟）就是因为反着来。

### B4. 工作流跨步复用同一任务记录 → 任何"以任务状态为判据"的钩子都会踩

**经过**：adopt 钩子判 `task.Status == done_success`，实测永远不命中——`CompleteAndAdvance` 在步骤完成 HTTP 响应返回前就把任务重置为 pending（为下一步准备）。审计 after_json 才看到 status 是 "pending"。
**泛化规则**：平台里 task 是**跨步骤的可变游标**，不是步骤结果。凡需要"这一步成没成"，判据必须是步骤实例状态（runtime POST 里的 stepStatus），不是任务字段。今后所有 step-complete 钩子（adopt、bind、notify）都适用。

### B5. vite ESM config loader 的 :ro 必死，且 `--config` 换路径救不了

**经过**：`"type": "module"` 的前端在 :ro 快照预览必死——vite 5.4 在 `vite.config.ts` 旁写 `*.timestamp-*.mjs` 再 import。试过的弯路：`--config /tmp/...`（模块解析从 config 位置出发，找不到 vite）、symlink staging（deps optimizer 要写 `node_modules/.vite`，穿透 symlink 还是 :ro）。**最终可行**：把服务目录整体 `cp -a` 到 /tmp 真实目录再跑。
**给模板作者的规则**：前端模板只要带 `"type": "module"` + vite，就假设"预览=拷贝运行"。哪天 vite 升到有 `configLoader: runner` 的版本，可以移除 staging。

### B6. 环境里会残留"看起来是平台一部分"的手工资源

**经过**：`multigent-gradle-cache` 卷（09-11 手工建）至今无任何代码挂载。诊断和 soak 时极易误判"gradle 缓存卷已生效"。同类：手工 `docker run` 的探针容器、`.service.d/` drop-in。
**规则**：手工资源要么删、要么在 HANDOFF 记"谁建的、为什么还在"。soak 清单里已加"勿误判"条目。

### B7. 错误信息聚合会把两条报错粘成一条（日志可读性）

**经过**：预览失败日志出现过 `can't cd to .../backend-servercan't cd to .../frontend-web`——两条 stderr 无分隔拼接，初看像一条畸形报错。这是 failInstance `strings.TrimSpace(string(logs))` 直拼 CombinedOutput 的副作用。
**低优先改进**：failInstance 拼接日志时按行加前缀（如 `  | `），成本极低，排障收益大。

### B8. 端到端验收时，"环境自愈"会掩盖回归

**经过**：v3 首轮 ready 失败是"先修后跑窗口"的旧二进制残留，重启后自愈扫描器 3 秒重派，后续全绿——**首轮失败本身是对的信号**，当时差点被"后面绿了就行"略过。这个竞态（部署重启 vs 首轮 wake 用旧配置）只复现过一次，仍是观察项。
**规则**：验收里每个"自动恢复"都必须追问首轮失败原因并记录，自愈成功 ≠ 无缺陷。

---

## C. 迁内网时执行层面的卡点（在 intranet-plan P0 之外的补充）

> intranet-plan 已覆盖镜像/源/证书的架构面；这里补执行面（真正动手迁移那天会撞上的）。

1. **systemd drop-in 是隐形配置面**：`/etc/systemd/system/multigent.service.d/{proxy,gitlab-runner}.conf` 携带代理与 runner ID，`systemctl show multigent --property=Environment` 才能看到。迁移时若只迁移主 conf 不迁移 drop-in，代理静默失效、runner 绑定静默缺失（后者导致流水线永久 pending——8.4 的根因同款）。**迁移清单必须含 drop-in 逐条核对**。
2. **NO_PROXY 必须列全内网服务**：GitLab 主机、控制面主机、host.docker.internal 都在现网 NO_PROXY 里。漏一个 → 该服务流量进代理 → 代理不通内网 → 挂。特别是 jvm 侧：`JAVA_TOOL_OPTIONS` 的 nonProxyHosts 是从 NO_PROXY **派生**的（jvm_proxy.go），NO_PROXY 错 = gradle/gitlab 全挂。
3. **凭据会话挂载路径已迁出 HOME**（`/tmp/multigent-session`）：迁移后如有工具把凭据写回 `$HOME/...`，非 root 容器内 HOME 已无挂载、工具将静默丢凭据。迁移验收要加一条"每个 agent CLI 的凭据文件落在 session 目录"。
4. **文档内网地址泄露面**：跟踪中的 `docs/mattermost-chatops-blueprint-and-reference-implementation.md` 含 2 处控制面主机 IP（:265-266，排障示例）。含内网拓扑的文档全在 `.git/info/exclude` 白名单（HANDOFF/audit-port-pool/gate-rulings 等）。**开源/共享前**：blueprint 这两处需改占位符，或把该文档也移入 exclude。
5. **Mac→VM 部署链依赖 OrbStack**：`/mnt/mac/tmp` 路径、`orb -m ubuntu` 都不在生产 VM 上存在。内网迁移后部署链要换成 registry 拉取或 scp。现有 SOP（HANDOFF §4）标注"仅限当前 OrbStack 拓扑"。
6. **探针容器的网络前提**：`RuntimeAPIReachableFromContainer` 走 `host.docker.internal`，迁移后 Docker 网络（桥接/add-host）配置变化会让该预检假阴性。先在目标网跑一次探针再切流。

---

## D. 低优先但值得排期的小项

- failInstance 日志按行前缀化（B7）。
- `docker events` 类诊断命令的 `timeout` 包装写进排障 SOP（防交互式挂住）。
- preview 双端口（前端 + 后端映射）在 instance JSON 里输出 backendPort，便于外部探活。
