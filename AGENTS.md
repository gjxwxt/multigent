# Multigent - Agent 开发与架构指南 (AGENTS.md)

本文档旨在为 AI Agent（以及开发者）提供 **Multigent** 项目的技术架构说明、构建方法、运行流程及协同开发规范。

---

## 1. 项目简介 (Project Overview)

**Multigent** 是一个面向团队的人机协作 Agent 操作系统（Human-agent collaboration infrastructure）。

它帮团队将 Prompt、工具、工作流及人工审核整合为一个协调协同的操作系统：
- **共享上下文 (Shared Context)**：统一管理 Workspace、Team、Role、Project、Task、Docs、Skills 及工具状态。
- **人机协同工作流 (Visual SOP & Workflows)**：可视化设计工作流，支持人工审核节点与 Agent 自动化任务交接。
- **RBAC 权限控制**：基于角色的访问控制，细粒度隔离用户与 Agent 的数据访问及工具权限。
- **沙箱隔离执行 (Sandbox Execution)**：Agent 运行在 Docker 隔离环境 (`ghcr.io/multigent/multigent/runtime-base`) 中，通过 `mga` 命令行工具安全地与宿主控制台交互。

---

## 2. 技术栈与架构设计 (Tech Stack & Architecture)

### 核心技术栈
- **后端 (Backend)**：Go (1.26+), SQLite (`modernc.org/sqlite` 纯 Go 驱动), Cobra CLI 框架, Gorilla WebSocket.
- **前端 (Frontend)**：React 18, TypeScript, Vite, Tailwind CSS, SVG/Lucide 图标库。
- **打入集成 (Embedding)**：前端静态资源通过 Go `//go:embed` 直接内嵌到 `multigent` 二进制文件中，单文件打包发布。
- **沙箱运行环境 (Sandbox)**：Docker 容器环境，内置 `mga` runtime CLI。

### 核心目录结构
```text
multigent/
├── cmd/
│   ├── multigent/          # 管理端 CLI、Server、Runtime Node Service 与 Workflow 自动化控制入口
│   └── mga/                # Agent 沙箱内使用的 Runtime CLI
├── internal/
│   ├── api/                # REST API 路由、Web 静态资源服务、Workflow Trigger、Identity Provider 及 WebSocket 处理
│   ├── db/                 # SQLite 数据库模型与数据访问层 (Audit, Runtime Nodes, Entitlements)
│   ├── sandbox/            # Docker 容器生命周期管理与镜像准备
│   ├── workflow/           # 工作流引擎、触发器 (Webhook/Cron/Event)、状态节点转换与分支逻辑
│   ├── rbac/               # 权限控制与角色管理
│   ├── secretbox/          # 敏感凭据/模型 API Key 加密存储
│   ├── imbridge/           # 飞书 (Feishu/Lark)、Slack 等 IM 平台对接
│   ├── worker/ & daemon/   # 异步 Worker、Heartbeat 心跳及定时调度器
│   └── builtins/           # 内置 Skill (如 multigent-admin-ops) 与默认模板
├── web/                    # React + Vite 前端控制台源码 (工作流画布、配额指示器、连接管理)
├── Makefile                # 项目构建、打包、部署与测试自动化脚本
├── README.md & INSTALL.md  # 详细说明文档
└── docs/                   # 项目深入设计、工作流账号集成与Getting Started文档
```

---

## 3. 构建与启动指南 (Build & Run Guide)

### 环境依赖
- **Go**: 1.26+
- **Node.js**: 20+
- **Docker**: 推荐安装并启动（Agent 运行与沙箱测试需要 Docker）

### 常用指令 (Makefile)

1. **一键构建完整应用 (嵌入 Web 控制台)**
   ```bash
   make build
   ```
   *该命令会自动安装前端依赖、构建 Vite 静态文件到 `web/dist`，并编译出二进制文件 `dist/multigent` 和 `dist/mga`。*

2. **启动 Web 控制台 (启动项目)**
   ```bash
   ./dist/multigent start
   ```
   *服务启动后访问：`http://127.0.0.1:27892`*
   - 默认初始管理员账号：`admin`
   - 默认初始管理员密码：`admin123`

3. **前端热重载开发 (Web Local Dev)**
   ```bash
   # 终端 1：启动后端 API 服务 (监听 27893 端口供 Vite 开发服务器反向代理)
   ./dist/multigent start --addr 127.0.0.1:27893

   # 终端 2：启动前端 Vite 热重载服务 (监听 27891 端口)
   make web-dev
   ```
   *说明：`make web-dev` 仅启动 Vite 前端 HMR 开发服务器，后端 API 服务需要单独在另一个终端启动。*

4. **预热 Docker 沙箱镜像 (Agent Run Prewarm)**
   ```bash
   ./dist/multigent sandbox prepare
   # 国内镜像源预热：
   ./dist/multigent sandbox prepare --region cn
   ```

5. **运行单元测试**
   ```bash
   make test
   ```

---

## 4. Agent 协同与修改规范 (Guidelines for AI Agents)

1. **修改 API 或后端逻辑时**：
   - 新增 API 路由请在 `internal/api/` 下添加，并确保接口鉴权与 RBAC 检查到位。
   - 若添加了控制台支持的参数或指令，请同步在 `cmd/multigent/` 下添加或更新 Cobra 命令。
2. **修改前端页面时**：
   - 页面代码位于 `web/src/pages/`，组件位于 `web/src/components/`。
   - 确保修改后通过 `make web` 校验 TypeScript 与 Vite 构建无报错。
3. **保持提交质量**：
   - 避免直接删改现有的单元测试与接口约定。
   - 修改完代码后，必须运行 `make test` 或 `make build` 进行验证，不可未经验证直接声明成功。

---

## 5. 安全红线 (Security Invariants — 违反即事故)

以下约定由真实事故沉淀而来，**任何修改不得绕过**；细节与事故背景见 `HANDOFF.md` 第 7 节：

1. **端点鉴权边界**：`publicMux`（server.go）上的路由完全无认证。新增有副作用的端点一律注册到带 `withTokenAuth` 的主 mux 并做 `checkProjectAccess`；必须暴露给预览 iframe 的端点，handler 内必须校验预览签名 token（`internal/api/preview_token.go`），写端点再加频率限制。
2. **凭据不落盘**：Git remote URL 持久化必须保持纯净（无 token）；凭据只在推送瞬时注入（credential-helper 或运行时注入）。Git 命令输出入库/返回前端前必须 redact（参考 `redactGitOutput`）。
3. **并发写保护**：预览 Copilot 写入受工作流节点写锁约束——判定用 `isTaskAtHumanReviewStep`（human_review 放行，查询失败 fail-closed 保持加锁），不能只看 `task.Status`。
4. **确定性基线**：任务派生必须基于不可变 `baseCommit`（SHA），继承前置任务的 `completionCommit`；禁止以 `origin/main` 的移动引用作为基线。

---

## 6. 核心机制速查 (Key Mechanisms)

改动以下区域前先读懂对应机制，避免破坏既有设计：

| 机制 | 位置 | 要点 |
|---|---|---|
| 预览引擎与租期回收 | `internal/preview/engine.go` | 30 分钟租期 + 后台 reaper 每分钟回收；`.multigent/runtime.json` 契约 fail-closed 校验 |
| Git Worktree 隔离 | `internal/gitworktree/worktree.go` | 项目锁串行化 git 操作；`sanitizeTaskID` 防路径穿越；快照失败必须阻断清理（防丢未推送工作） |
| 审核自动提交 | `internal/api/workflow_handlers.go` `commitAndPushReviewChanges` | 人工审核 approve 时收编 Copilot 工作区改动为 checkpoint commit；push 失败写任务评论告警，不静默 |
| 启动自愈扫描 | `internal/api/workflow_handlers.go` `recoverActiveWorkflowRuns` | 重启后 3s 自动恢复停在 agent 节点的 active run；永不自动恢复 human_review；150ms 节流 |
| 六大生命周期解耦 | `HANDOFF.md` 第 6 节 | 任务/代码基线/Worktree/预览会话/容器/远程同步各自独立字段与状态机，禁止混用单一状态 |
| 工作流双层体系 | `internal/workflow/store.go` | 代码内置 `Templates()`（只读目录）→ 经 `POST /api/v1/workflows` 实例化落库才可供任务选用；改模板后必须重新实例化才能在 UI 生效 |
| 统一交付流水线 | 模板 ID `unified-delivery-pipeline` | 12 步闭环，核心是编码后的 Agent 初审闸门（独立 reviewer-agent、实测验证、`review_rounds` 三轮封顶）；发布步 CI 触发为 best-effort（无权限如实填 none） |

---

## 7. 部署与验证环境 (Deployment Context)

- 生产运行环境为 OrbStack Ubuntu VM（`127.0.0.1`），服务监听 `0.0.0.0:27892`；Mac 侧可用 VM IP 直连（推荐，与 HANDOFF.md 拓扑一致）或 OrbStack 的 `127.0.0.1` 端口转发（等价别名）。
- **修改代码后的部署 SOP（Mac → VM）**：`make build` → `GOOS=linux GOARCH=amd64 go build`（multigent 与 mga 两个产物）→ `orb -m ubuntu sudo cp` 到 `/opt/multigent/bin/` → `systemctl restart multigent` → `journalctl -u multigent` 查日志。完整命令见 `HANDOFF.md` 第 4 节。
- 部署重启会触发启动自愈扫描器；重启后应检查日志确认无 `panic` 且 `[auto-recovery]` 行为符合预期。
- `work/multigent-linux-amd64` 是随仓库管理的部署产物，发布新版本后按惯例刷新并单独提交（`build: refresh linux deployment artifact`）。
