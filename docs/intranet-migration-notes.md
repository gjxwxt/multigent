# 内网迁移卡点与配置项设计（执行者现场笔记）

> 2026-09-15。作者：Batch 1-3 执行 Agent。输入：VM 部署 E2E（batch1-rc1）、
> fixturesandbox/generator 与 preview origin 的实现现场、全仓 MULTIGENT_* 环境变量
> 清单核对。本文记录"只看代码/评审意见发现不了、实际执行时才撞上"的卡点，
> 并给出配置项的公共设计约束。占位符原则：真实地址、凭据、镜像仓库凭据不入本文件。

---

## 1. 进程与 origin 拓扑（当前部署形态）

单一 multigent 二进制承担 console 与 preview 两个逻辑 origin；preview origin 不是
独立监听者，靠边缘反代 + Host/X-Forwarded-* 路由区分：

```text
                     ┌──────────────────────────────────────────────┐
   浏览器 ──────────►│  边缘反代 multigent-edge（systemd 服务）        │
                     │  :27893 (0.0.0.0)                            │
                     │  Rewrite: Out.Host = In.Host                 │
                     │           SetXForwarded() ← 从 socket 重建    │
                     └──────────────┬───────────────────────────────┘
                                    │ http (loopback)
                                    ▼
                     ┌──────────────────────────────────────────────┐
                     │  multigent 二进制 :27892（仅绑内网/本地接口）    │
                     │                                              │
                     │  请求路由（按 scheme+Host 全等比较配置 origin）: │
                     │  ├─ console origin → API + 静态资源 + WS       │
                     │  └─ preview origin → /preview/* 代理           │
                     │       ├─ gate: scheme+host 全等（伪造即 404）   │
                     │       ├─ token 校验（query 兑换 HttpOnly cookie）│
                     │       └─ 沙箱容器（docker，动态端口）            │
                     └──────────────┬───────────────────────────────┘
                                    │ docker API
                                    ▼
                     ┌──────────────────────────────────────────────┐
                     │  沙箱容器（runtime-base 镜像）                  │
                     │  镜像来源三选一（见 §2 镜像解析链）              │
                     │  APP_DB_PATH 注入（fixturesandbox）            │
                     └──────────────────────────────────────────────┘
```

**迁移含义**：内网迁移时这台二进制必须能同时被两个 origin 的 Host 访问到。
边缘反代必须 (a) 保留入站 Host 原样传给后端，(b) 覆盖客户端伪造的
X-Forwarded-Proto/Host/Port（`SetXForwarded()` 语义）。若内网已有 nginx/
traefik，等价配置是显式 `proxy_set_header Host $host` + 显式重建全部
X-Forwarded-*，绝不能 `proxy_pass` 裸转发头。

## 2. 镜像解析链（卡点一：generator 曾硬编码 GHCR，已修）

全平台容器镜像必须走同一条解析链，否则内网必然出现"沙箱能跑、播种容器
拉不动 GHCR"的分裂故障：

```text
镜像选择顺序（所有容器统一）：
  1. MULTIGENT_RUNTIME_IMAGE 显式覆盖（内网私有 registry 地址写这里）
  2. MULTIGENT_RUNTIME_REGION=cn → 阿里云镜像（外网国内部署用）
  3. 默认 ghcr.io/multigent/multigent/runtime-base:latest

消费方：
  - Agent 沙箱（internal/sandbox.resolveImage）      ← 原生支持 ✓
  - fixture 播种 generator（internal/fixturesandbox） ← 2026-09-15 起支持（本卡点修复）
  - OD 设计确认：multigent 侧只是 HTTP 客户端（od_client.go 无镜像引用），
    OD daemon 自身的镜像属部署环境配置，不在代码解析链内（已核对）
```

修复前的实际故障模式：fixturesandbox 的 `GeneratorImage` 是 Go 常量
（`ghcr.io/...:latest`），预览启动时 provision 先于 docker 调用执行——
GHCR 不可达的内网里，**每个带 fixtures 契约的预览都会在 provision 阶段
fail-closed**，而错误信息只说 "generator container failed"，不指向镜像
拉取问题。这是"评审看得见代码、只有部署时才撞见"的典型。

## 3. 配置项公共设计约束（新增配置时必须遵守）

现状盘点：平台已有约 40 个 `MULTIGENT_*` 环境变量（数据目录、origin、
镜像、凭证、日志、SMTP、worker 等）。公共设计约束从现有实现归纳：

1. **同一语义只允许一个 env 键**，别名必须在 `effectiveServerAddr` 式的
   单一解析函数里归一（现状好例：`MULTIGENT_SERVER_ADDR` /
   `MULTIGENT_API_ADDR` 在 `cmd/multigent/runtime_config.go` 一处归一）。
   反例警示：日志相关存在 `MULTIGENT_LOG_MAX_SIZE` 与
   `MULTIGENT_LOG_MAX_SIZE_MB` 两个键——新增时不得再复制这种双键。
2. **解析函数集中、可单测**：每个配置域一个 `effectiveXxx(cfg, fallback)`
   纯函数，优先级 env > config file > flag default，禁止在业务代码里散落
   `os.Getenv`（现状违例点已登记在 §6 待办）。
3. **安全相关配置 fail-closed**：origin 类配置缺省即禁用功能（preview
   origin 未设置 → `/preview/*` 整体 404），绝不回退同源或 Host 推导。
   内网迁移新增任何门面配置（回调基址、公网 URL）都必须沿用该模式。
4. **部署专属值不入仓库**：镜像地址、端口、主机名、凭据写部署环境的
   systemd drop-in / 私有 runbook；仓库文档只能出现键名与占位符
   （本文遵守同一红线）。
5. **配置变更必须留下验证命令**：每项部署配置在 runbook 里配一条
   可执行的验证命令（curl 断言 / `multigent admin-token` + API 探针），
   "改完即验"，不留"应该可以了"。

## 4. 内网迁移卡点清单（按执行顺序）

### 卡点 A：出口依赖（最先撞上）

| 依赖 | 用途 | 缺失时的症状 | 对策 |
|---|---|---|---|
| GHCR（ghcr.io） | 沙箱/播种/OD 镜像 | 预览 fail-closed；任务无法启动 | 私有 registry + `MULTIGENT_RUNTIME_IMAGE`（generator 已修，见 §2） |
| Go module proxy | 模板物化、Agent 容器内 `go test` | materialize 超时；CI 红 | `GOPROXY` 指内网代理或 vendored module cache 卷 |
| npm registry | react_go_fullstack 模板 `npm ci` | 播种 generator 失败 | 内网 npm mirror + 模板 `.npmrc` 占位（部署配置，不入库） |
| 更新检查 | 启动时 telemetry/update-check | 启动日志噪音、可能阻塞 | `MULTIGENT_NO_UPDATE_CHECK=1`（已有键，部署必配） |

### 卡点 B：origin 与回调（配置错就是静默断链）

- `MULTIGENT_PREVIEW_ORIGIN` / `MULTIGENT_CONSOLE_ORIGIN` 必须写成内网
  DNS 可解析的**公网形式 origin**（scheme+host，无路径），两个 origin
  的 host 必须不同——同 host 不同端口不构成 origin 隔离，gate 会按
  设计拒绝。
- 工作流 trigger 回调链（webhook → IM 卡片 → 回调端点）用
  `MULTIGENT_WEB_BASE_URL` 生成外链；不配则回退请求 Host，反代后面
  会生成内网回环地址的链接（用户点开即断）。**部署必配**。
- 遗留项（暂不改，登记 §6）：`workflowWebBaseURL` 里有一段
  27893→27894 的硬编码端口替换，是 2026-07 dev 拓扑（API 默认 :27893、
  bridge status :27894）的化石，仅在回环地址上触发，配了
  `MULTIGENT_WEB_BASE_URL` 后不生效；内网迁移不影响，但应择期删除。

### 卡点 C：Docker 与路径

- 沙箱/播种容器 `-v worktreeDir:/workspace` 挂载的是**宿主绝对路径**：
  数据目录（`MULTIGENT_DATA_DIR`）一旦变更，历史任务的 worktree 绝对路径
  失效，旧任务预览/续跑全部断。迁移必须整目录搬或放弃历史任务，不能改
  路径前缀。
- 容器内 UID 与宿主 multigent 进程不一致时文件所有权漂移——已有
  `RepairWorkspaceOwnership` 自愈（best-effort），但 runbook 应安排
  迁移后首跑一次全量任务做验证。
- fixturesandbox 的 artifact/lease 目录在数据目录下
  （`<dataDir>/fixturesandbox/`），schema 指纹与模板迁移文件绑定：模板
  版本升级后旧 artifact 自动漂移拒绝（设计如此），迁移后第一次播种失败
  属预期，重新触发即可。

### 卡点 D：Git 远端与凭证（红线复述）

- remote URL 持久化保持纯净（无 token），凭据只在 push 瞬时注入；
  内网 GitLab/Gitea 的 token 走部署环境的 credential helper 配置，
  不入库、不进 kv_records、不进任务评论（`redactGitOutput` 契约）。
- 审核提交路径已接跨进程项目锁 + SanitizedGitEnv + config 中和
  （commit 4daec299）；内网迁移不需要额外配置，但**第一次审核提交**
  应人工盯日志确认锁与净化路径正常。

### 卡点 E：验证序列（迁移后按序执行）

```text
1. multigent start（两个 origin env 就位）→ 日志确认 "preview sharing enabled"
2. curl console origin /api/health                     → 200
3. curl preview origin 无 token                        → 401（gate 活着）
4. curl preview origin 伪造 XFP + 伪造 cookie          → 401（反代覆盖生效）
5. MULTIGENT_RUNTIME_IMAGE 指向内网 registry 后 docker pull 通
6. 创建测试项目 → 模板物化（GOPROXY/npm mirror 通）      → 任务启动
7. 预览启动（fixtures 契约项目）                        → provision 成功
8. 审核approve → review commit 落库                    → 锁/净化路径验证
```

## 5. 为什么这样设计（架构取舍记录）

- **单一二进制 + 边缘反代**而非双监听：进程生命周期、DB 句柄、调度器
  天然共享，避免双进程间的状态同步；origin 隔离本质是浏览器同源策略
  的事，服务端只需按 Host 分流——反代是最薄的一层。
- **env 覆盖 > region 镜像 > 默认 GHCR** 的三级解析：显式覆盖服务内网，
  region 服务外网国内，默认服务公有云一键部署；三层都必须存在于
  **每个**容器消费方，缺一个就是 §2 的分裂故障。
- **fail-closed 优先**：安全配置（origin、token、写权限）缺省即拒绝，
  可用性配置（GOPROXY、npm mirror）缺省即报错但不静默降级——两类
  配置的失败模式必须区分。

## 6. 待办（迁移相关的登记项，按优先级）

1. ~~删除 27893→27894 化石端口替换~~（已完成 2026-09-15 夜间：`workflowWebBaseURL`
   不再做 dev 拓扑端口改写，请求基址原样透传，公网 URL 由
   `MULTIGENT_WEB_BASE_URL` 承担；回归测试锁定）。
2. ~~日志双键归一~~（已定案 2026-09-15 夜间：**不收敛为单键**——`MULTIGENT_LOG_MAX_SIZE`
   （字节，daemon systemd/launchd 安装器写入）与 `MULTIGENT_LOG_MAX_SIZE_MB`
   （MB，runtime-node 安装器写入）都是**已装机 systemd 单元在用的键**，删任何
   一个都会破坏存量安装。定案为"canonical=_MB 键，字节键为兼容回退"，优先级
   由 `TestResolveServiceLogOptionsLogSizeKeys` 锁死，新部署一律写 `_MB` 键。
3. **配置清单文档化**：把 40 个 env 键按 §3 的域分组写进正式文档
   （键名 + 语义 + fail-open/fail-closed 标注），作为 runbook 的配套。
