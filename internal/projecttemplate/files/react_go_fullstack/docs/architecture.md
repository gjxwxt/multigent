# Architecture & Technical Design

## 1. 系统概览

本项目采用 Go + React 单二进制内嵌架构：
- 前端使用 React 18 + Vite 构建，产物直接输出到 `web/dist/`。
- `make build` 将 `web/dist/` 复制到 `server/webui/dist/`。
- Go 核心服务通过 `//go:embed all:webui/dist` 将前端静态资源打包为单一可执行二进制程序。
- 单一容器交付：`deploy/Dockerfile` 只需一个构建与运行阶段，即可提供前端页面和后端 REST API。

## 2. 目录职责

```text
├── Makefile                   # 统一生命周期管理 (dev/install/test/verify/build/doctor)
├── AGENTS.md                  # AI Agent 开发规范与 TDD 约束
├── CLAUDE.md                  # 开发者与 Agent 调试速查
├── README.md                  # 项目介绍与快速开始
├── docs/                      # 架构设计、接口文档、TDD 指南
├── deploy/                    # Dockerfile 与 compose.yml
├── server/                    # Go 核心服务
│   ├── main.go                # HTTP ServeMux、API Handler 与 SPA 静态资源路由
│   ├── main_test.go           # 核心接口单元测试与表驱动测试
│   └── webui/dist/            # 嵌入式静态文件目录 (placeholder.txt 保底编译)
└── web/                       # React 18 + TypeScript 前端工程
    ├── src/                   # 组件、页面与状态
    ├── package.json
    └── package-lock.json      # 确定性锁文件
```

## 3. 请求路由机制

1. `/api/*`：由 Go 标准库 `http.ServeMux` 匹配并由对应 Handler 处理，返回 JSON。
2. 静态资源与页面路径：由 `spaHandler()` 处理。若命中内嵌的静态文件则直接以正确的 MIME 类型返回；若未命中则回落到 `index.html`，由前端路由（React Router / 客户端逻辑）接管。
