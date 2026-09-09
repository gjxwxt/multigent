# Spring Boot + React 现代企业级全栈 Starter

这是一个企业级全栈模板，基于 **Java 21 + Spring Boot 3.3.3 + Gradle** 后端与 **React 18 + Vite + TypeScript + Tailwind CSS** 前端。

---

## 目录结构 (Directory Structure)

```text
.
├── AGENTS.md                  # AI Agent 协作与开发规范（强制遵循）
├── CLAUDE.md                  # 开发者与助手速查手册
├── Makefile                   # 统一构建与测试命令
├── docs/                      # 架构设计与工程规范文档
│   ├── architecture.md        # 系统分层与调用拓扑设计
│   ├── api-spec.md            # RESTful API 规范与统一错误响应
│   └── tdd-guide.md           # TDD 测试驱动开发与测试规范
├── deploy/                    # 容器化与运维部署
│   ├── Dockerfile             # 多阶段 Docker 生产镜像构建
│   └── compose.yml            # 本地与生产 Docker Compose 编排
├── server/                    # Spring Boot 3.3.3 后端 (Java 21, Gradle)
│   ├── build.gradle
│   ├── gradlew
│   └── src/
│       ├── main/
│       │   ├── resources/application.yml
│       │   └── java/com/example/app/
│       │       ├── Application.java
│       │       ├── config/        # Web、CORS 与配置类
│       │       ├── controller/    # HTTP 控制器 (参数校验、DTO 映射)
│       │       ├── service/       # 业务逻辑接口及实现
│       │       ├── repository/    # 数据持久层抽象与存储
│       │       ├── model/         # 实体、DTO 与请求/响应 Record
│       │       └── exception/     # 全局异常捕获与标准错误响应
│       └── test/                  # 单元测试与 MockMvc 切片测试
└── web/                       # React 18 SPA 前端 (Vite, TS, Tailwind)
    ├── package.json
    ├── vite.config.ts
    └── src/
        ├── components/        # 可复用 UI 组件
        ├── pages/             # 页面级视图组件
        ├── services/          # 类型安全的 HTTP API 调用封装
        ├── types/             # TypeScript 类型定义
        └── test/              # Vitest + RTL 组件单元测试
```

---

## 快速开始 (Getting Started)

### 环境依赖
- **Java**: 21+
- **Node.js**: 20+
- **Docker**: 可选（用于容器化构建与发布）

### 本地开发
```bash
# 启动后端服务 (监听 http://localhost:8080)
make dev-backend
# 或在 server/ 目录下:
./gradlew bootRun

# 启动前端开发服务器 (监听 http://localhost:5173，反向代理 /api)
make dev-frontend
# 或在 web/ 目录下:
npm install && npm run dev
```

### 运行测试
```bash
# 运行全部测试 (后端 JUnit 5 + 前端 Vitest)
make test

# 仅测试后端
make test-backend

# 仅测试前端
make test-frontend
```

---

## 生产构建与容器化

```bash
# 构建前端并打包 Spring Boot 可执行 Jar
make build

# 容器化构建与启动
docker compose -f deploy/compose.yml up --build -d
```
健康检查端点位于：`http://localhost:8080/api/health`。
