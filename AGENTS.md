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
