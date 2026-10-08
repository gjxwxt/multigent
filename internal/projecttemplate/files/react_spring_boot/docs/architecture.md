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
- **`src/services/`**: `http.ts`（fetch 封装）+ `api/<domain>.ts`（按域拆分的端点模块）+ `api.ts`（聚合出口）。严禁在 UI 组件直接写原生 fetch。
- **`src/hooks/`**: 跨页面复用的数据与状态逻辑。
- **`src/components/`**: 保持高内聚、低耦合，受控属性通过 props 传入；超过 6 个通用组件时拆 `components/ui/`。
- **`src/pages/`**: 路由对应的页面容器（react-router），新页面按 `App.tsx` 路由表注册，页面多后转 `React.lazy` 分包。
- **`src/test/`**: 基于 Vitest 与 React Testing Library 的真实渲染与行为测试。

---

## 4. 持久层升级路径 (Persistence Upgrade Path)

默认形态：in-memory `ConcurrentHashMap` repository（零依赖、CI 快）。需要持久化时**按以下三步走**，禁止临场发明数据库配置：

1. **启用 db profile（已内置，零代码）**：`application-db.yml` 已备好 H2 PostgreSQL 兼容模式 + `V1__init.sql` 基线表。激活：`SPRING_PROFILES_ACTIVE=db ./gradlew bootRun`（或部署环境同名环境变量）。
2. **引入迁移与访问层**：迁移文件进 `server/src/main/resources/db/migration/V<N>__*.sql`；表结构定稿后同步冻结到 `contract/ddl.md`。当迁移数量增多或需要复杂查询时，引入 Flyway（接管 `db/migration/`）与 MyBatis-Plus/JPA，repository 改为接口 + DB 实现，Service 层不动。
3. **切生产数据库**：新增 `application-prod.yml`（或部署环境变量）覆盖 datasource 指向 PostgreSQL/MySQL；H2 依赖保持 `runtimeOnly` 不进编译期。repository 实现按 profile 装配（`@Profile("db")` / `@Profile("prod")`）。

切换判据：数据需要跨重启存活（第一秒就该切）、并发写冲突成为正确性问题、或需求明确要求审计/事务——三者任一出现即升级，不要等"数据变多"。
