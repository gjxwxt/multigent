# Test-Driven Development (TDD) Guide

本指南说明如何在 Go + React 工程中开展测试驱动开发。

---

## 1. TDD 开发循环

对于任何新增功能或缺陷修复，必须严格遵循 **Red-Green-Refactor**：

1. **Red**: 先在 `server/*_test.go` 中按功能要求添加测试用例。用 `httptest.NewRecorder()` 模拟 HTTP 请求，明确断言 HTTP 状态码与响应体字段。运行 `go test ./...` 确保测试因缺少实现而失败。
2. **Green**: 在 `server/main.go` 或子包中编写实现，使其恰好通过测试。
3. **Refactor**: 优化实现结构，保证 `go vet ./...` 与静态检查无报错，且测试持续通过。

---

## 2. 表驱动测试范式 (Table-Driven Tests)

Go 单元测试推荐使用表驱动测试结构：

```go
func TestExampleEndpoints(t *testing.T) {
    tests := []struct {
        name       string
        method     string
        path       string
        wantStatus int
        wantBody   string
    }{
        {
            name:       "health check returns ok",
            method:     "GET",
            path:       "/api/health",
            wantStatus: http.StatusOK,
            wantBody:   `"status":"ok"`,
        },
    }

    handler := newHandler()
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            req := httptest.NewRequest(tt.method, tt.path, nil)
            rec := httptest.NewRecorder()
            handler.ServeHTTP(rec, req)

            if rec.Code != tt.wantStatus {
                t.Fatalf("expected status %d, got %d", tt.wantStatus, rec.Code)
            }
            if !strings.Contains(rec.Body.String(), tt.wantBody) {
                t.Fatalf("expected body to contain %q, got %q", tt.wantBody, rec.Body.String())
            }
        })
    }
}
```

---

## 3. 测试质量底线

- **严禁虚假断言**：禁止仅调用函数而不检验结果。
- **覆盖边界与异常**：至少包含正常流、未授权/未找到（404/401/403）及无效入参（400）。
- **CI 闸门保护**：提交前必须执行 `make test` 与 `make verify`。
