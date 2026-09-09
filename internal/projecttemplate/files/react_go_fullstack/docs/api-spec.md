# REST API Specification

## 1. 健康检查

- **URL**: `GET /api/health`
- **说明**: 供容器编排与宿主存活探测使用。
- **响应格式**: `application/json`
- **响应示例**:
  ```json
  {
    "status": "ok",
    "service": "api"
  }
  ```

## 2. 问候接口

- **URL**: `GET /api/hello`
- **说明**: 示例业务接口，用于前后端连通性测试。
- **响应示例**:
  ```json
  {
    "message": "hello from Go"
  }
  ```

## 3. 统一错误响应

- **状态码**: `4xx` / `5xx`
- **响应格式**: `application/json`
- **响应示例**:
  ```json
  {
    "error": "not found"
  }
  ```
