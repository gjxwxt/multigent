# Project AGENTS.md - AI Agent 协作与开发规范

本文档定义了 AI Agent 在本全栈项目中工作时的架构准则、分层规范与质量红线。任何修改必须符合本规范。

---

## 1. 架构与分层红线 (Architecture Invariants)

本项目严格遵守三层架构与前后端清晰分离：

### 1.1 后端分层约束 (`server/`)
- **`controller/` 极薄层**：
  - 仅负责 HTTP 参数提取、JSR-380 (`@Valid`, `@NotBlank` 等) 校验、调用 Service，并返回标准 HTTP 状态码与 DTO。
  - **严禁** 在 Controller 中直接写数据库逻辑、事务管理或复杂业务计算。
- **`service/` 业务逻辑层**：
  - 业务规则、领域模型转换、状态变迁全部收敛在 Service。
  - 按需选择设计：简单业务可直接编写 Service 类；在存在多态实现、替换策略或复杂领域边界时推荐采用接口 (`ItemService.java`) 与实现类 (`impl/ItemServiceImpl.java`) 分离，严禁无意义的形式主义套皮。
- **`repository/` 数据访问层**：
  - 负责持久化与查询抽象，不渗漏业务决策。
- **`model/` 模型与 DTO**：
  - 推荐优先使用 Java 17+ / 21 `record` 声明不可变 DTO 与请求响应对象。
- **`exception/` 集中异常管理**：
  - 业务失败抛出继承自 RuntimeException 的专有异常（如 `ResourceNotFoundException`）。
  - 由 `@RestControllerAdvice` (`GlobalExceptionHandler`) 统筹转换为统一 `ApiErrorResponse`。严禁返回裸异常堆栈。

### 1.2 前端分层约束 (`web/`)
- **`components/`**：无状态或受控的 UI 组件，不直接写内联 fetch。
- **`pages/`**：路由页面（react-router，路由表见 `App.tsx`），编排组件与调用 service。
- **`services/http.ts`**：fetch 封装（错误对象统一、状态码处理），不含任何业务端点知识。
- **`services/api/<domain>.ts`**：按后端域拆分的端点模块；**新增 API 的固定三步**：①类型进 `types/` → ②方法进 `api/<domain>.ts` → ③页面只 import `services/api` 聚合出口。
- **`hooks/`**：跨页面复用的数据/状态逻辑（如 `useAppData`）。
- **`types/`**：统一维护前端 TypeScript 类型与后端 DTO 对齐。

---

## 2. 文档即交付物 (Documentation Ownership)

文档与代码同 MR 演进，腐化即缺陷。每个文档有明确的 Owner Zone：

| 文档 | 所有权规则 | 机器可查 |
|---|---|---|
| `docs/api-spec.md` | **改 Controller 必须同 MR 更新**。API 路径集合与代码强一致 | `make docs-check`（CI 闸门，不一致即 fail） |
| `docs/architecture.md` | 只描述**结构性事实**（分层、目录、数据流）。**禁止**写具体类名清单——类会改名，结构才稳定 | 人工评审 |
| `README.md` | 只写"如何跑起来"。业务功能描述归 api-spec | 人工评审 |
| `contract/*.md` | 冻结契约，变更控制见 `contract/README.md`（`contract:` 前缀 MR + api-spec 同步） | `make docs-check` 覆盖 API 面 |
| `docs/handoff/` | 任务快照归档，规则见下节 | 归档纪律 |

## 3. Handoff 规则 (Task Handoff Protocol)

- **何时写**：每个工作流步骤完成时、会话结束前，写 `docs/handoff/t-<task-id>.md`（模板见 `docs/handoff/TEMPLATE.md`）。
- **写什么**：任务状态（done/in-progress/blocked）、验证证据（跑了什么命令什么结果）、未完成清单按优先级、风险与坑。
- **何时不写**：不复制代码（给文件路径与行号）、不写凭据与内部网络信息、不写预测计划（handoff 是快照不是计划书）。
- **归档纪律（不可协商）**：任务终结后 handoff **永不修改**。新状态写新任务的 handoff，历史文件保持原样——错误不会向历史传播，下一个会话永远可以信任"已完结任务的 handoff 就是当时的事实"。

---

## 4. 测试驱动开发 (TDD) 规范

下游 Agent 在开发新功能或修复缺陷时，**必须执行 TDD 循环**：
1. **Red（先写测试）**：
   - 依据验收标准 (AC) 编写针对性的单元测试或切片测试。
   - 运行测试并验证其因缺少实现而失败。
2. **Green（实现最小可用代码）**：
   - 编写恰好通过测试的业务代码。
   - 运行测试确认通过。
3. **Refactor（重构与整理）**：
   - 清理代码、遵循静态检查规范，确保测试依然全部通过。

### 禁止的测试反模式 (Anti-patterns)
- ❌ **禁止空测试**：如只有空的 `@Test void testMethod() {}`。
- ❌ **禁止无断言测试**：如只有方法调用没有 `assertThat` 或 `assertEquals`。
- ❌ **禁止浅层全 Mock**：不得只 Mock 掉所有层而没有真实的业务验证。
- ❌ **禁止修改现有测试使坏代码通过**：不得降级断言要求。
- ❌ **禁止跳过/注释掉失败测试后宣称通过**：`@Disabled`/`it.skip` 必须在任务描述中登记理由。

---

## 5. 组件与依赖增长判据 (Growth Rules)

模板刻意保持零 UI 框架依赖。增长按判据走，不按直觉走：

- **组件库引入判据**：页面 ≥8 个，或出现第二处重复交互模式（同样的表单/弹窗/表格逻辑写第二遍）时，评估引入组件库。引入时必须：①根部有 `ErrorBoundary`；②`npm run build` 后核对主 chunk，**>800kB 必须拆分**（路由级 lazy）。
- **`components/` 拆分规则**：通用 UI 组件超过 6 个时拆 `components/ui/` 目录（一组件一文件），禁止继续堆单文件。
- **依赖引入最小化**：每个新 runtime 依赖在任务描述中登记理由。单页项目可卸载 `react-router-dom`（`npm uninstall react-router-dom` + 恢复 `App.tsx` 为单页），但路由是默认基线。
- **服务端依赖升级路径**：默认 in-memory repository（零依赖、CI 快）。需要持久化时按 `docs/architecture.md` §4 的三步走，禁止临场发明 H2/连接配置。

---

## 6. 常用命令速查

```bash
# 运行后端全部测试
cd server && ./gradlew test

# 运行指定测试类
cd server && ./gradlew test --tests "com.example.app.controller.ItemControllerTest"

# 运行后端代码静态检查
cd server && ./gradlew check -x test

# 运行前端测试与构建
cd web && npm test -- --run
cd web && npm run build

# 文档漂移校验（API 面与 Controller 强一致）
make docs-check

# 启用 db profile（H2 PostgreSQL 兼容模式）本地验证
cd server && SPRING_PROFILES_ACTIVE=db ./gradlew bootRun
```

---

## 7. 交付检查清单 (Delivery Checklist)
在提交改动前，Agent 必须核实：
1. `make test` 全绿，无跳过的测试。
2. 新增或修改的文件放入正确的子目录（`controller`, `service`, `components` 等），不随意在根目录散落文件。
3. 无硬编码密钥或明文凭据（含测试与配置文件；密钥一律走环境变量）。
4. API 变动同步更新 `docs/api-spec.md` 与 `contract/api-skeleton.md`，且 `make docs-check` 通过。
5. 持久化/加密/鉴权相关改动不得引入硬编码密钥、永不过期的令牌或匿名可写端点——安全默认值与业务代码同 MR 评审。
