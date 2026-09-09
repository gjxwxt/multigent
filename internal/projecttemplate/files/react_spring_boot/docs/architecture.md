# 系统架构设计与分层规范 (Architecture Specification)

## 1. 全局架构拓扑 (Global Topology)

本项目采用前后端解耦的企业级工程拓扑结构：

```text
[ Browser / Client ]
         │
         ├── (Development) ──> Vite Dev Server (:5173 / :27891)
         │                          │ (Reverse Proxy /api)
         │                          ▼
         └── (Production)  ──> Spring Boot Application (:8080)
                                    │
                         ┌──────────┴──────────┐
                         │  Spring MVC Layer   │
                         │  (WebConfig, CORS)  │
                         └──────────┬──────────┘
                                    │
                                    ▼
                         ┌─────────────────────┐
                         │ Controller Layer    │ (HealthController, ItemController)
                         └──────────┬──────────┘
                                    │
                                    ▼
                         ┌─────────────────────┐
                         │   Service Layer     │ (ItemService, Domain Logic)
                         └──────────┬──────────┘
                                    │
                                    ▼
                         ┌─────────────────────┐
                         │  Repository Layer   │ (ItemRepository, In-Memory/DB)
                         └─────────────────────┘
```

---

## 2. 后端分层详细约定 (`server/`)

### 2.1 包结构与职责边界
- **`com.example.app`**
  - **`Application.java`**: Spring Boot 启动入口。
  - **`config/`**: 全局配置，如 CORS 策略、静态资源转发、安全拦截。
  - **`controller/`**: RESTful API 控制器。
    - 规则：仅处理参数绑定、`@Valid` 格式校验、调用 Service、组装 `ResponseEntity`。
    - 禁止：直接进行数据库查询或执行复杂业务计算。
  - **`service/`**: 业务服务层。
    - 规则：承载业务规则与校验、事务边界控制、抛出清晰的领域异常。简单业务可直接编写 Service 类；在存在多态实现、替换策略或复杂领域边界时推荐采用接口 (`ItemService.java`) 与实现类 (`impl/ItemServiceImpl.java`) 分离，按需使用，避免无意义的形式主义套皮。
  - **`repository/`**: 数据持久化与查询接口及实现。
    - 规则：负责存储交互，保持纯粹的数据 CRUD 与索引。
  - **`model/`**: 数据模型、实体与传输对象。
    - 推荐：不可变 Java 21 `record` 声明 DTO。
  - **`exception/`**: 全局异常处理机制。
    - 包含业务异常类（如 `ResourceNotFoundException`）与统一处理切面 `GlobalExceptionHandler`。

---

## 3. 前端分层约定 (`web/`)

- **`src/types/`**: 所有后端 DTO 对应的 TypeScript interface / type 定义。
- **`src/services/`**: 封装所有 API 异步交互函数，严禁在 UI 组件直接写原生 fetch。
- **`src/components/`**: 保持高内聚、低耦合，受控属性通过 props 传入。
- **`src/pages/`**: 路由对应的页面容器，负责组合组件与触发 service 加载。
- **`src/test/`**: 基于 Vitest 与 React Testing Library 的真实渲染与行为测试。
