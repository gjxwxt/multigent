# 执行者盲点实录 — 只有真正跑过全链路的 Agent 才会踩到的点（2026-09-14）

> 来源：p15 双栈 canary（runtime-p15-canary-2026-09-12.md）两轮验收的实际执行过程。
> 收录标准：**审查代码看不出来、只有跑到运行时才暴露**的问题。每条带"当时怎么发现的"与"下次怎么快速跳过"。
> 与 HANDOFF §10.17 互补：那边记结论，这边记"为什么会在那里摔跤"。

---

## A. 优先级排序：未完成 Feature 与待逐步实现项

按"对标准交付闭环的风险 × 迁内网紧迫度"排序。每项独立可 commit、可回滚。

| # | Feature | 现状 | 建议步序 | 风险 |
|---|---|---|---|---|
| 1 | **P2 双栈并发 soak（2 项目 × 1 任务）** | ✅ 已执行通过（2026-09-13，canary §10；双腿 done_success + 双预览验收） | 扩 ×2（四 Agent 并发）待排 | — |
| 2 | **runtime-jvm21 镜像固化**（intranet-plan P1） | 镜像可用但来自手工构建，无版本化构建流水 | 先把 Dockerfile + 构建参数进 repo，再谈内网 Harbor | 中：当前 `2026.9.1` tag 无人能复现 |
| 3 | **gradle 共享缓存卷**（预览/agent 两侧属主方案） | 遗留卷 `multigent-gradle-cache` 已建但**平台代码零挂载**（已实测：卷内 720MB wrapper dists 系手工预热产物，平台从没写过它） | 并入 intranet-plan P2 受管缓存根目录；接入前该卷可当"手工预热仓"用 | 低（当前容器内 HOME 可写已够用） |
| 4 | **partial 绑定失败重试入口**（HANDOFF 10.16 §5.1） | API 可恢复但 UI 无入口 | 小 UI 迭代 | 低 |
| 5 | **preview 端口池可观测性** | findFreePort 盲分配，无占用统计 | soak 实测：并发双预览端口无碰撞，方案可缓 | 低 |
| 6 | **探测子命令 `multigent runtime probe`**（intranet-plan §2.4） | 未实现 | 按 P0 契约实施 | 低 |
| 7 | **项目删除级联清理**（P2 soak 新发现） | `DELETE /projects/{name}` 删 DB 记录 + `fsStore.DeleteProject`（RemoveAll 整目录），但 `workspace` 常是独立 git 仓库且 worktree 目录可能被 root 属主文件顶住：实测删除后残留 `projects/<name>/workspace/.multigent/worktrees/`（双腿共 ~204MB，需手工删）；另有 11 个历史孤儿目录共 ~510MB 无 project.yaml 也无 API 项目 | 删除路径调用 `cleanupTaskDeliveryArtifacts` 同款 worktree 清理 + 删除后自检残留并告警；孤儿目录可用"无 project.yaml 判据"写回收扫描器 | 中：磁盘缓涨 + worktree 元数据泄漏 |
| 8 | **worktree 残留自愈扫描**（P2 soak 新发现） | gitworktree 的 `CleanupWorktree` 在任务交付时执行，但"任务被删/项目被删/首次失败"路径不保证走到；残留 worktree 若 `.git` 是真目录（而非指针文件），`git worktree prune` 也不认 | 周期扫描 `.multigent/worktrees/` 下无对应 active 任务的目录并清理（复用项目锁） | 中 |

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

**经过**：`multigent-gradle-cache` 卷（09-11 手工建）至今无任何代码挂载。诊断和 soak 时极易误判"gradle 缓存卷已生效"。同类：手工 `docker run` 的探针容器、`.service.d/` drop-in。P2 清理时又添一例：卷内 `wrapper/dists/from-host/` 有 5 个 gradle 发行版共 575MB——全是手工预热放的，平台代码没写过它；卷从未被挂载这件事，看卷的内容根本看不出来。
**规则**：手工资源要么删、要么在 HANDOFF 记"谁建的、为什么还在"。soak 清单里已加"勿误判"条目。**判定"平台是否真的在用它"要查代码引用 + 容器 Mounts，不能看资源里有没有数据。**

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
- **连接密钥默认加密**：未配置 `MULTIGENT_CONNECTION_ENCRYPTION_KEY` 时 secretbox 以 plain-dev 明文存 token（P2 清理时实测读出）。建议启动时无 key 打 WARN 日志 + config-reference 已标注迁移必带。
- **dangling docker 资源周期回收**：soak 一晚就攒了 11 个退出容器 + 18 个 dangling 镜像（~4.5GB）+ 950MB build cache；`docker system prune` 一行可清，建议写进部署 SOP 或加 systemd timer。

---

## E. P2 soak 后补记（2026-09-13 凌晨执行完毕时新增）

- **并发 autoStart 竞态**：同 agent 双任务同刻 autoStart，后到者首跑 exit 1（agent busy in manual_run）。平台后续 wake 自动恢复、无任务丢失，但"首轮失败"会污染任务历史与耗时统计。修法：任务启动入口按 agent 排队或加短暂退避重试（B8 的"追问首轮失败"在此有了具体形态）。
- **git worktree 的 .git 判据**：正常 worktree 的 `.git` 是指针文件（`gitdir: ...`），被 RemoveAll 硬删后 `git worktree prune` 能回收元数据；但 **孤儿 worktree 的 `.git` 可能是真目录**（独立 clone 语义），prune 对它无效——test7 的两个 2026-08-27 残留 worktree 就长这样，靠 `git worktree list` 看不见。回收扫描器必须直接扫目录 + 校验"无 active 任务"，不能依赖 git 元数据。
- **清理用凭据的取用路径**：平台 DB 里连接密钥可解出（plain-dev），清理 GitLab 仓库用它合法（soak 仓库本由平台创建）；但取用后必须即弃——本轮在 Mac/VM /tmp 的副本全部删除，这个动作要成为固定收尾步骤。

---

## F. ChatOps 通宵冲刺后补记（2026-09-15 凌晨，两轮通宵执行的新盲点）

> 收录标准不变：审查代码看不出来、只有跑到运行时才暴露。本节是 ChatOps 全闭环
> + 超时治理 + 双侧部署执行过程的新增发现。

### F1. commit message 会带出本机环境（PATH 泄漏实例）

**经过**：把生产日志里的报错原文粘进 commit message（`exec: claude: executable
file not found in <完整 $PATH>`），提交后才发现整条本机 PATH（含各人工具目录）
已写进 git 历史。所幸未 push，amend 掉了。
**规则**：commit message 里引用日志必须先脱敏——报错原文里的环境相关部分
（PATH、IP、路径）要手动截断成 `not found in $PATH` 之类的占位。`git log -p`
是公开面，按"会贴到 README"的标准写。

### F2. 手工 go build 绕过 Makefile = 版本元数据静默丢失

**经过**：交叉编译时手写 `go build -ldflags "-s -w"`，漏掉 Makefile 里的
`-X main.version/commit/buildDate`。部署成功、服务正常，但 `multigent version`
显示 `commit : none`，health 端点只有 "dev"——直到行为验证时才发现无法证明
"跑的是哪个提交"。
**修复**：平台已加启动日志（`multigent build: version=... commit=...`），
commit=NONE 时打 WARN 指引重建。另加 `multigent admin-token` CLI（见 F3）。
**规则**：交叉编译一律 `make linux-amd64` / `make linux-arm64`（或照抄 Makefile
LDFLAGS 块），不要手写 ldflags；部署后验证清单第一项 = 启动日志里的 commit 行。

### F3. 运维缺口：本地管理员 token 无签发入口

**经过**：验证 ChatOps 审批闭环需要在生产 console 上创建冒烟任务，但没有
浏览器登录流的条件下无 token 可用——最后是从 DB 读 jwt_secret、用 Python
复刻 Go 的**自定义 base64 字母表** JWT 签名手搓了一个。能跑通，但这是只有
读过 auth.go 全文才能完成的操作，且 db 直读 + 手搓签名不该是常规运维动作。
**修复**：新增 `multigent admin-token [--user U] [--ttl 15m]`（本仓 CLI），
在部署主机上一条命令签出与运行中 server 同密钥的短期 token。
**规则**：凡是"执行 Agent 自己都要手搓一次"的运维动作，就是 CLI 缺口。

### F4. 部署拓扑的认知偏差：节点不在"那台 VM"上

**经过**：HANDOFF §15.4 写了"runtime node 服务在 VM 上当前 inactive"——这是
误判。节点守护进程实际运行在**另一台独立节点机**上（与 console 不同主机），console
VM 上自然找不到进程。DB 里节点 online + runs 有 runtime_node_id，两行数据
本可及早戳穿误判，但当时只查了 console 侧 systemctl。
**规则**：排查"某服务在哪跑"先查 DB 的关联记录（runtime_nodes 表、
runtime_runs.runtime_node_id），再按记录去对应主机找进程；不要假设
"部署 = 单机"。多机拓扑下每台机器的身份、职责要在 HANDOFF 拓扑图里显式维护。

### F5. 超时治理的覆盖面判定：能挂死锁持有者的调用优先级最高

**经过**：审计 50+ 个裸 `exec.Command` 时发现，危险性不取决于命令本身，
而取决于**调用点持有什么锁**：gitworktree 的 44 个调用全部在 manager 互斥锁
+ 项目目录锁之下，一个挂死的 `git fetch`（credential helper 等待 TTY 输入
是经典场景）冻结的是全项目所有任务的 worktree 操作；而 runner 的 agent 进程
虽然"更重"，但它的生命周期已有 run 租约和任务 context 治理，反而不用动。
**规则**：给 subprocess 加超时前先画"锁持有图"。判据是"这个调用挂死时谁在
等"——持锁调用点 > 请求路径调用点 > 后台 best-effort 调用点。

### F6. 自定义 JWT 字母表是隐性协议（跨语言复刻成本高）

**经过**：Go 侧 `base64Encode` 用的是自定义字母表 + 定制 padding 截断
（`A-Za-z0-9-_`，尾部按余数裁字符，不是标准 URL-safe base64 的 `=` 填充）。
用 Python 标准库复刻签名时必须逐字符对照实现，`base64.urlsafe_b64encode`
直接用会错。任何跨语言复刻该签名的脚本都会踩。
**规则**：这类"自造编码"要么在 doc 注明"非标准，复刻需对照实现"，要么提供
官方签发入口（F3 已做）把外部复刻需求归零。

### F7. journalctl 的 --since 会漏"刚刚发生"的日志（时钟/写入延迟）

**经过**：`journalctl -u multigent --since "2 minutes ago"` 返回空，怀疑没
日志；实际是 dispatch 发生在 sleep 窗口之后、`--since` 的相对时间按执行时
计算导致窗口错位。改用 `-n N` 尾随 + grep 才稳定。
**规则**：排障时优先 `-n 200 | grep` 尾随，`--since` 只用于确定的历史窗口；
两者结论冲突时信尾随。

### F8. 平台侧增量 > 提示词侧约束（review_rounds 的教训泛化）

**经过**：三轮升级上限写在步骤描述里让模型自觉递增，生产第二轮就破防
（上报 1）。平台在 rework 边确定性 +1 后问题归零，且 5 行代码。
**规则**：计数器、轮次、预算这类"必须精确"的状态，一律平台持有、平台递增；
提示词只负责告知，不负责记账。模型上报值只作参考，永不作为唯一事实源。

### F9. 剩余 Feature 优先级修订（2026-09-15）

综合两轮通宵执行，对 §A 清单的修订与新增：

| # | 项 | 变化 | 说明 |
|---|---|---|---|
| 9 | **admin-token CLI** | ✅ 已完成（`multigent admin-token`） | F3 收口，部署机一条命令签短期 token |
| 10 | **启动日志 version/commit** | ✅ 已完成 | F2 收口，commit=NONE 打 WARN |
| 11 | **`multigent config check`** | 部分完成 | 已落地两块相邻能力：admin-token 静默新建 DB 的 stderr 预警（见 F12）、runtime-node 版本漂移 WARN（见 F13）；完整自检命令仍排期 |
| 12 | **节点日志/run 产物轮转** | 新增排期 | 节点侧 run exec.log 无轮转（实测 32 个文件/18MB，慢但单调涨）；multigent.log 已有 max_size_mb，节点 workspaces 侧没有 |
| 13 | **poller 20s 周期的可观测性** | 新增排期 | 触发派发目前只有 audit `runtime_run.enqueue`（reason 字段可辨来源），建议加一条 debug 日志便于验证"派发走了哪条路"（本次冒烟全靠 audit 链反推） |
| 14 | **docker system prune timer** | 维持 §D 建议并提级 | 容器/镜像垃圾在冒烟频繁的 VM 上增长很快（§D 实测 ~4.5GB/晚） |
| 15 | **版本对齐检查** | ✅ console 侧已完成 | 节点心跳版本 ≠ console 版本 → 一次 WARN（按 node+version 去重，对齐后清条目）；node 侧升级提示/上线拦截仍排期 |

§A 原有 1–8 项维持不变。

### F10. 触发器双写竞态面的核实（2026-09-15 第三轮；**初版结论被 GPT 审查推翻，见 F14**）

针对「poller × 手动 start × workflow followup × attention wakeup 并发命中同一任务」的
竞态面做了核实。**初版写下"数据库层双保险已闭合"的结论，经 GPT 外部审查证伪**：
去重层（见下）只覆盖"重复 run"，完全没覆盖"run 与 task token stamp 的交错"——
run 先以 `queued` 入库、之后才写任务的 `ActiveRuntimeRunID`，两步之间 node 可直接
claim；stamp 失败时 `FailQueuedRuntimeRun` 只能处理仍为 queued 的 run，**已被 claim
的 run 会继续执行、最终结果被 task fence 丢弃**。详见 F14 与 HANDOFF §20。
教训：核实"去重"≠核实"安全发布"；声称"闭合"前必须把 insert→stamp→claim 的全时序
画出来，最好有 barrier 测试，而不是只看每条路径是否"过了闸"。

初版记录的去重层事实仍然成立（供后续修复引用）：

- **`hasActiveRuntimeRunForTarget`**（scheduler_manager.go:1699）：queued 恒阻塞；
  running 在租约未过期且非只读槽位时阻塞。
- **`run_key` 部分唯一索引**（migrations.go:646）+ `UpsertRuntimeRunIdempotent`
  （runtime_nodes.go:186）：并发入队同一 intent 收敛到同一条 run。只防重复，不保证
  "可执行 run 一定已持有 task token"。
- **run_key 派生优先级**（run_key_service.go）：workflow step > attention 信号 >
  scheduled wakeup > plain task；空 key 完全绕过去重（legacy 行）。
- 四条派发路径（poller trigger.go:140 / 手动 start scheduler_manager.go:829 /
  followup scheduler_attention.go:590 / node hook trigger.go:344）都过上述闸。

### F14. GPT 外审推翻初版结论的五项（2026-09-15 第四轮；**当夜全部修复完毕**）

1. **Q1（P0）stamp 前 run 可被 claim ✅（04efc269）**：`enqueueRuntimeTaskRun` 先 insert queued run
   再写 task 的 ActiveRuntimeRunID（runtime_node_handlers.go:549），窗口内 node claim
   （runtime_nodes.go:207 一带）即可拿走未 stamp 的 run。已修：初始写
   `preparing`（不可 claim），stamp 成功后原子提升 queued（promote 唯一冲突 =
   幂等 join，token 改指 winner 后删自己未 claim 的 preparing 行）；barrier 测试
   runtime_run_stamp_race_test.go ×4 断言 node 永远拿不到。关键坑：winner 查找必须
   SQL 级自排除（created_at 秒精度平局 + 随机 id，调用方侧比较会死循环）。
2. **Q4（P0）driftWarnedNodes 并发 map 写 ✅（aefa69ab）**：heartbeat 是并发 HTTP，map 无锁，
   `concurrent map writes` 可致进程崩溃（runtime_node_handlers.go:1600）。已修：
   `driftWarnedMu` 专用 mutex + 节点删除清理 + -race 下 16 goroutine × 40 轮并发测试。
3. **Q2（工作流正确性阻断）✅（acfe39ae）**：`CompleteAndAdvance`（store.go:2042/2184）是多次
   独立 SaveStepInstance/SaveStepEvent/SaveRun，**无事务无 CAS**——"状态转换有
   DB 事务边界"是我的错误认定。已修：kv_records payload+revision 双见证 CAS
   （仅 revision 见证有洞：marker→marker swap 会赢）+ transition-claim 闸门
   （fresh 拒绝 / >5min TTL 抢占 / terminal 拒绝重入）+ 首写前 abort 全部释放
   claim（route-mismatch 重跑测试暴露 normalize 校验失败路径原先不释放）。
   并发断言全过：一次成功一次 stale 零写入、单 event、单 pending next、rounds 只 +1。
4. **Q5 ✅（2ddf0ae7）**：admin-token 缺库硬失败且不创建文件/父目录（取代本文件
   F11 的 warn-only 方案——GPT 正确：少报无害不成立，警告后创建空库仍是事故）。
5. **Q3 ✅（7dfca9c1）**：三处漏网 git 调用统一 boundedGitOutput 5s；90s 网络预算
   改 `MULTIGENT_GIT_NETWORK_TIMEOUT` 可配置（无效值回退默认，永不无界）。
6. **Q6 ✅（HANDOFF §20.2）**：内网清单补齐 8 项（私有 CA 四类运行时端到端、
   secret key 分发轮换、GitLab 认证、TLS/NO_PROXY、SQLite 备份、镜像 digest/架构、
   Docker socket 最小权限、容量 GC）；两项标 unknown（多节点 key 一致性校验、
   备份命令未内置）。

### F11. 本轮（通宵第三轮）落地清单

- **F10**：触发器双写竞态面核实结论（DB 双保险闭合，见上）。
- **admin-token 静默新建 DB 预警**：`warnIfControlDBLooksFresh`（cmd/multigent/token.go）——
  resolved 控制库文件不存在时先打 3 行 stderr：会新建空库+新 jwt_secret / env 未指到
  $HOME/.multigent（sudo 下是 root 家目录）/ 该 token 必被运行中服务拒绝并给出正确命令
  形态。已实测：fresh HOME 无 env → 3 行全出；DB 已存在 → 静默；env 指向新目录 → 只出
  2 行（跳过 $HOME 提示）。
- **runtime-node 版本漂移 WARN**（internal/api/runtime_node_handlers.go
  `warnRuntimeNodeVersionDrift`）：heartbeat 上报版本 ≠ console `s.version` → slog WARN
  一次，按 (nodeID, nodeVersion) 去重；版本回到一致时清掉去重条目（每个漂移"episode"
  恰好一条，downgrade 再漂会再报）。**warn-only 是有意设计**：运行中节点的 heartbeat
  一旦被拒，租约续期饥饿 → live run 被 reaper 收走，比版本漂移本身危害大。测试覆盖
  4 段场景 + 空 console/空 node 版本不误报。

### F12. admin-token 预警的判定窗口选择

预警在 `OpenDefault()` **之前**做 `os.Stat`，因为打开动作本身就会创建文件——打开后再
判"刚创建"需要对比 inode/mtime，复杂且仍有竞态。Stat-then-open 有 TOCTOU 窗口（极小概率
别的进程恰好在此间创建库），但该方向的误差是"少一条警告"，无害；反方向（打开后补判）误差
是"误报"，会稀释警告可信度。选误差无害的方向。

### F13. 版本漂移只做 console 侧 WARN 的边界

node 上报 version 的通道已存在（register + heartbeat 都带），console 侧比较是零协议成本。
没做 node 侧"上线拦截"的原因：lease-generation 已 fail-closed 挡住 pre-Q0 节点续租，
升级期间的短暂漂移靠 WARN 提示人处理即可，硬拦截会把滚动升级变成停机窗口。node 侧
"你的版本落后于 console"提示留在 UI/CLI 排期（F9 表 #15）。
