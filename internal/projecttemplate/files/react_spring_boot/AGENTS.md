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
- **`pages/`**：视图路由页面，编排组件与调用 service。
- **`services/api.ts`**：所有 HTTP 请求严格收敛在 services 模块，具备强类型入参和出参。
- **`types/`**：统一维护前端 TypeScript 类型与后端 DTO 对齐。

---

## 2. 测试驱动开发 (TDD) 规范

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

---

## 3. 常用命令速查

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
```

---

## 4. 交付检查清单 (Delivery Checklist)
在提交改动前，Agent 必须核实：
1. `make test` 全绿，无跳过的测试。
2. 新增或修改的文件放入正确的子目录（`controller`, `service`, `components` 等），不随意在根目录散落文件。
3. 无硬编码密钥或明文凭据。
4. API 变动同步更新 `docs/api-spec.md`。
