# Runtime Node 部署手册

> 适用版本：`feat/chatops-live-card-and-d6`（2026-09-13，收口 6 + scheduler panic 修复后）。
> 本文以 2026-09-13 在 OrbStack Ubuntu 26.04 (arm64) 上端到端实测通过的流程为基准：
> 全链路（start→queued→claim→running→succeeded→done_success）、失败路径
> （CLI 缺失 / provider 401 → done_failed）、reaper 全场景（SIGKILL node →
> lease_expired 收割 → 退避 → 重跑恢复）均已真实验证。

本文回答两个问题：

1. 一台新服务器如何变成 Multigent 的 Runtime Node（控制台不动，只加执行机）。
2. Agent 运行依赖的「挂载与环境」（模型账号、连接凭据、上传文件、工具链、
   Docker 镜像）在节点拆分后如何到达每台机器——**答案：绝大多数不是挂载，
   而是按 run 下发 + 节点本地物化/自装**，详见 [§4 环境与挂载模型](#4-环境与挂载模型拆分节点的关键认知)。

---

## 1. 前置条件

| 项 | 要求 |
|---|---|
| OS | Linux（systemd 可用）；arm64/amd64 均可（二进制需与架构匹配） |
| 网络 | 节点 → 控制台 `MULTIGENT_API_URL` 端口可达（如 `192.168.139.231:27892`） |
| Docker | Docker Engine 可用且 daemon 运行中（`docker info` 成功）。OrbStack 虚拟机可复用宿主 daemon（见 §3.1 坑位） |
| 磁盘 | ≥2GB（runtime-base 镜像 ~700MB + 工具链/工作区增长） |
| Agent CLI | 按所用模型安装：`claudecode` → Node.js 20+ 且 `claude` 在 PATH；服务以 root 运行时装到 `/usr/local/bin` 并重启 node 服务 |
| 模型网络 | 节点需可达模型 provider 的 base_url（如内网网关 `http://<host>:3456`）——console 配置在 DB，网络可达性是节点自己的事 |

**版本对齐**：节点二进制与控制台必须同代构建。旧版节点的续租/完成请求会被
409 拒（缺 `lease_generation`），节点侧取消 run context，属预期 fail-closed
（Q0 不支持新旧混跑；升级顺序：drain → 停 node → 升 console → 升 node）。

## 2. 部署步骤（实测序列）

```bash
# 0) 交叉编译（在开发机上，按目标架构）
GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o work/multigent-linux-arm64 ./cmd/multigent
# amd64 用 GOARCH=amd64；mga 二进制节点不直接需要（镜像内自带）

# 1) 拷到目标机并赋予执行权限
sudo mkdir -p /opt/multigent/bin
sudo cp multigent-linux-arm64 /opt/multigent/bin/multigent
sudo chmod +x /opt/multigent/bin/multigent

# 2) 自检：docker + 镜像 + 资源（此时还没 join，只看本地能力）
/opt/multigent/bin/multigent runtime doctor
# docker.available 必须 true；runtimeImage.available false 也没关系，join 时会 pull

# 3) 控制台侧：管理员创建 join token（30 分钟有效）
#    UI：Settings → Runtime Nodes → 添加
#    或 API：
curl -s -X POST http://<console>/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<密码>"}' | jq -r .token   # → $ADMIN_TOKEN
curl -s -X POST http://<console>/api/v1/runtime-nodes/join-token \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"<节点名>","kind":"personal_computer","expiresIn":"30m"}'
# 响应里的 joinToken 一次性使用

# 4) 节点侧：join（会注册 + 预热镜像；--skip-prepare 可跳过预热）
sudo /opt/multigent/bin/multigent runtime join \
  --server http://<console>:27892 \
  --token mgrt_<...>

# 5) 安装为 systemd 服务（默认并发 1、3s 轮询）
sudo /opt/multigent/bin/multigent runtime install-service
systemctl status multigent-runtime-node
tail -f /root/.multigent/runtime/runtime-node.log   # 应看到 "runtime node connected"

# 6) 控制台确认：GET /api/v1/runtime-nodes → status=online、os/arch/version 上报
```

`install-service` 固定要素（systemd, Linux）：服务名 `multigent-runtime-node`、
二进制 `/opt/multigent/bin/multigent`、配置 `/root/.multigent/runtime/config.json`、
日志 `/root/.multigent/runtime/runtime-node.log`（默认并发 1，可在 install-service
传 `--concurrency` 调整）。配置文件里保存的是 join token（mgrt_…），**节点机器
本身等同比凭据保管方，不要把它放进共享镜像**。

## 3. 实测踩过的坑（新机必查）

1. **agent 必须同时配 `model` + `defaultModelAccountId`**（Settings → Agent）：
   - 只配 `runtimeModel` 不配 `model` → meta.Model 默认 `human` → runner 走
     genericInvoker（`cat` 读 prompt 即退出）→ **run 报 succeeded 但是假跑**。
   - `defaultModelAccountId` 不配 → console readiness 直接拒绝入队
     （「未配置模型账号」），fallback 走 attention，不会到 node。
   - 判别方法：看 run 的 exec log 首行 Command 是 `claude -p …`（真跑）还是
     `/usr/bin/cat …`（假跑）。
2. **服务以 root 运行**：Claude Code CLI 需装进 `/usr/local/bin`（root PATH），
   装完 `systemctl restart multigent-runtime-node`，否则 claim 后
   `agent_run_failed: exec: "claude": executable file not found`（task 落
   done_failed，token 正常释放，不会卡死）。
3. **OrbStack 虚拟机没有自带 docker CLI**：daemon 可用但 `docker` 二进制缺
   失。处理：`ln -sf /mnt/mac/<user>/.orbstack/bin/docker /usr/local/bin/docker`
   复用宿主 daemon（架构需匹配，OrbStack daemon 是 linux/arm64）。物理服务器
   直接装 Docker CE 即可。
4. **provider 网络可达性是节点侧的事**：console 里 provider 的 base_url（如
   `http://<host>:3456`）必须从节点可达；否则 run 失败
   `401 Invalid API key / connection refused`（收敛为 done_failed，可重跑）。
5. **join token 30 分钟过期**是设计行为；过期重发即可，不影响已 join 节点
   （节点用注册时下发的长期凭据心跳，不走 join token）。

## 4. 环境与挂载模型（拆分节点的关键认知）

**核心结论：不要按「把宿主目录挂进每台节点」的思路做。** 这套架构里,
per-run 的环境大多数通过 **run spec（JSON）+ 节点本地物化** 传递，只有
Docker 沙箱自身是挂载。逐项拆解：

| 依赖 | 传递方式 | 节点侧落点 | 需要运维做什么 |
|---|---|---|---|
| 模型账号 / API Key | run spec `ProviderEnv`（console 下发，经加密存储解出） | 进程环境变量，不落盘 | 无（只需网络可达 provider） |
| 连接凭据（DB/GitLab 等） | `GET /api/v1/runtime/connections` 连接文档，节点侧写临时 `MULTIGENT_CONNECTIONS_FILE` | 临时文件，run 结束清理 | 无 |
| 上传文件 / 上下文附件 | run spec 指示 → 节点本地物化到 `<node-root>/.multigent/files/`，`MULTIGENT_FILES_DIR` 指向 | 节点本地目录 | 无（files 目录会随用随建） |
| 任务提示词 | run spec `Prompt` | 临时 prompt 文件 | 无 |
| 工具链（node/go/python 构建 CLI 等） | 容器镜像内置 + 首跑自装（`/opt/multigent/toolchains`，flock 防并发安装） | 镜像层 + 节点卷（docker 模式） | 预热 `runtime prepare` 可选，不预热则首跑慢几分钟 |
| Agent CLI（claude/codex…） | **不传递**，节点本机 PATH 上的二进制 | `/usr/local/bin/claude` 等 | **必须手动装**（§3.2） |
| Docker 沙箱挂载卷 | docker provider 自动构造：workspace、agentDir、prompt 文件、files 目录、toolchains 卷、go/npm 缓存卷 | Docker named volumes + bind mounts | 无（自动） |
| Git worktree（代码任务） | **不跨机同步**：worktree 在 console 侧项目根 `.multigent/worktrees/` 创建；节点上的 run 收到 worktree 变量时 fail-closed（`execution_scope_mismatch`） | — | 见下方「worktree 边界」 |

**节点本地目录结构**（root 用户）：

```
/root/.multigent/runtime/
├── config.json                          # join 配置（含 token，等同凭据）
├── runtime-node.log                     # 服务日志
└── workspaces/<workspace-id>/           # 按 workspace 隔离的执行根
    ├── .multigent/files/                # 上传文件物化点（MULTIGENT_FILES_DIR）
    └── projects/<project>/agents/<agent>/
        ├── .multigent/runs/             # 每次 run 的 exec 日志
        └── .multigent/runtime-home/     # agent CLI 的 home（会话持久化）
```

**workspace 内容不是预同步的**：节点上 `projects/<project>/workspace` 在
首个 run 到达时才创建。当前实现下，代码仓库类任务（依赖 worktree/预置
workspace 内容）设计上归属 console 本地执行或指定节点常驻场景；纯指令型
任务（写文件、调 API、跑脚本）在任意节点都能跑通。把 worktree 变量透传给
节点的路径已被 fail-closed 过滤（普通任务即使 vars 里带了 worktree 键也会
被剥掉，仅 wakeup 逃生通道且节点上不可达时直接报错）。

**Docker vs host 两种沙箱**（agent `runtime_config_json.sandbox.provider`）：
- `docker`（Nora/Lina 现用）：容器隔离，镜像 `runtime-base`/`runtime-jvm21`，
  挂载卷自动构造，toolchains 在容器卷里首跑自装。
- 未配置（Rex 默认）：直接 host 执行（`sandbox=host`），要求节点上预装
  agent CLI；信任边界 = 节点本身是可信执行机。
- 生产建议：给 agent 显式配 `sandbox.provider=docker`，避免 host 直接执行
  （root + bypassPermissions 的组合只应在可信节点启用）。

## 5. 内网迁移时的多节点拓扑建议

1. **一个 console，多个 node**：每台节点重复 §2 即可，节点之间互不感知，
   不共享任何目录。任务按 agent 绑定的 `defaultRuntimeNodeId` 路由，
   不是全局负载均衡——同一 agent 永远去同一节点。
2. **agent ↔ node 绑定改法**：`PATCH /api/v1/agents/{id}` 传
   `defaultRuntimeNodeId`；或 UI Agent 详情页。
3. **想让同一 agent 的多台节点分流**：目前不支持（run 锁 agent 的
   membership + slot）；按 agent 维度拆分到节点才是正解。
4. **凭据边界**：join token 只用于注册；节点长期身份凭据由 join 时下发并
   存 `/root/.multigent/runtime/config.json`。迁移/重装节点 = 重新 join，
   旧节点在 console 上 disable/delete。
5. **升级**：console 与 node 同代构建。console 先升（向后兼容旧 node 期间
   会 409 拒续租），node 尽快跟上；reaper 会在 node 停机 90s+180s 后收割
   其在跑任务，退避后可重跑，无需人工清状态。

## 6. 验证清单（每台新节点上线后跑一遍）

```bash
# 节点上
/opt/multigent/bin/multigent runtime doctor     # docker/runtimeImage 可用
systemctl is-active multigent-runtime-node      # active
grep -c "runtime node connected" /root/.multigent/runtime/runtime-node.log

# 控制台上：建测试项目 + agent 绑该 node（model + modelAccount 都要配！）
# 创建任务 autoStart=true → 观察链路：
#   runtime_runs: queued → running → succeeded
#   kv_records(tasks): done_success，ActiveRuntimeRunID 清空
#   节点 log: "runtime run claimed" → "runtime run completed"
#   agent 真跑标志：exec log Command: claude -p …（不是 /usr/bin/cat）

# reaper 演练（可选）：run 进行中 systemctl stop multigent-runtime-node
# 预期 ≈4.5 分钟后 console 打 "runtime run reaped: lease expired past grace"，
# run→failed/lease_expired，task→pending+infraFailureStreak=1+notBefore=+5m；
# node 重启后重跑 → done_success。
```
