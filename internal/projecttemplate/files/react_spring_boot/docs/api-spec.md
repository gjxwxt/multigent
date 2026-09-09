# RESTful API 规范与契约 (API Specification)

## 1. 基础约定

- **根路径**：业务 API 统一使用 `/api/v1` 前缀；健康检查与平台契约使用 `/api/health`。
- **传输编码**：所有请求与响应体统一采用 UTF-8 编码的 JSON 格式（`Content-Type: application/json`）。
- **时间格式**：采用 ISO 8601 字符串格式，如 `2026-09-09T14:30:00Z`。

---

## 2. 统一健康检查契约

### `GET /api/health`
系统健康状态探针，供容器健康检查与 Multigent 预览引擎探活使用。

#### 响应示例 (200 OK)
```json
{
  "status": "UP",
  "timestamp": "2026-09-09T14:30:00Z",
  "service": "server"
}
```

---

## 3. 示例业务接口：Item 管理

### 3.1 查询列表
- **路径**：`GET /api/v1/items`
- **响应 (200 OK)**：
```json
[
  {
    "id": "item-1",
    "title": "Initial Task",
    "description": "Baseline task created during bootstrap",
    "status": "PENDING",
    "createdAt": "2026-09-09T14:30:00Z"
  }
]
```

### 3.2 创建 Item
- **路径**：`POST /api/v1/items`
- **请求体**：
```json
{
  "title": "New Task",
  "description": "Task description"
}
```
- **字段约束**：`title` 不能为空（`@NotBlank`），最大 100 字符。
- **响应 (201 Created)**：创建成功的 Item 对象。

### 3.3 根据 ID 查询
- **路径**：`GET /api/v1/items/{id}`
- **响应 (200 OK)**：Item 对象。
- **响应 (404 Not Found)**：当 ID 不存在时返回标准错误响应。

### 3.4 删除 Item
- **路径**：`DELETE /api/v1/items/{id}`
- **响应 (204 No Content)**：删除成功。

---

## 4. 统一错误响应格式 (Uniform Error Response)

当请求失败（4xx 或 5xx）时，服务端严格返回统一格式的 JSON：

```json
{
  "code": "RESOURCE_NOT_FOUND",
  "message": "Item with id 'item-999' was not found",
  "details": [],
  "timestamp": "2026-09-09T14:30:00Z"
}
```

#### 参数校验失败示例 (400 Bad Request)
```json
{
  "code": "VALIDATION_FAILED",
  "message": "Validation failed for 1 fields",
  "details": [
    "title: must not be blank"
  ],
  "timestamp": "2026-09-09T14:30:00Z"
}
```
