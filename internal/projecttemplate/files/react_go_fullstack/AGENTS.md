# Project AGENTS.md - AI Agent 协作与开发规范

本文档定义了 AI Agent 在本全栈项目中工作时的架构准则、分层规范与质量红线。任何修改必须符合本规范。

---

## 1. 架构与分层红线 (Architecture Invariants)

本项目采用 Go + React 单一容器/单二进制全栈架构：

### 1.1 后端约束 (`server/`)
- **Go 标准库优先**：网络服务采用 Go 1.22+ 增强型 `http.ServeMux` 路由（支持带有方法与路径参数的路由匹配）。
- **极薄 Handler**：HTTP Handler 仅负责请求解析、校验、调用业务逻辑并返回结构化 JSON。
- **静态资源内嵌**：`server/webui/dist` 由 `make build` 产出并通过 `//go:embed` 打包入 Go 二进制中。`server/webui/dist/placeholder.txt` 保证在未编译前端时仍可通过 `go test ./...`。
- **错误处理规范**：严禁吞掉错误，严禁在生产返回未格式化的内部错误信息；API 错误响应遵循统一 JSON 结构 `{"error": "..."}`。

### 1.2 前端约束 (`web/`)
- **React 18 + TypeScript + Vite**：
- **`src/` 目录组织**：保持组件与页面解耦，请求通过统一方法发起，避免在 UI 组件中随意散落原生 fetch。
- **类型定义对齐**：前端接口入参及出参类型需与后端 Go 结构体序列化字段保持一致。

---

## 2. 测试驱动开发 (TDD) 规范

下游 Agent 在开发新功能或修复缺陷时，**必须执行 TDD 循环**：
1. **Red（先写测试）**：
   - 依据验收标准 (AC) 编写针对性的表驱动测试（Table-driven test）或 API 切片测试 (`net/http/httptest`)。
   - 运行测试并验证其因缺少实现而失败。
2. **Green（实现最小可用代码）**：
   - 编写恰好通过测试的业务代码。
   - 运行测试确认通过。
3. **Refactor（重构与整理）**：
   - 清理代码、遵循 `go vet` 与静态检查规范，确保测试依然全部通过。

### 禁止的测试反模式 (Anti-patterns)
- ❌ **禁止空测试**：如只有空的 `func TestSomething(t *testing.T) {}`。
- ❌ **禁止无断言测试**：如只有函数调用没有对返回结果或状态码的断言。
- ❌ **禁止修改现有测试使坏代码通过**：不得随意降级断言要求。

---

## 3. 常用命令速查

```bash
# 运行后端全部测试
cd server && go test -v ./...

# 运行依赖下载与安装
make install

# 运行全链路健康与环境检查
make doctor

# 完整验证 (Doctor + Test + Build)
make verify
```

---

## 4. 交付检查清单 (Delivery Checklist)
在提交改动前，Agent 必须核实：
1. `make test` 全绿，无跳过的测试。
2. `make verify` 成功通过。
3. 新增或修改的文件放入正确的子目录，不随意在根目录散落文件。
4. 无硬编码密钥或明文凭据。
5. API 变动同步更新 `docs/api-spec.md`。
