# React + Go Fullstack Starter

一个可直接开发、构建和预览的前后端一体项目模板：

- `web/`：React + Vite 前端，开发时将 `/api` 代理到 Go API。
- `server/`：Go 标准库 HTTP API，提供确定性的健康检查接口。
- `.multigent/runtime.json`：预览引擎的启动与就绪探针契约。

## 本地开发

```bash
make doctor
make dev
```

前端默认运行在 `http://127.0.0.1:27891`，后端运行在 `http://127.0.0.1:8080`。

## 验证

```bash
make test
curl http://127.0.0.1:8080/api/health
```

初始化任务会在这个骨架上安装依赖、执行测试与构建，并负责初始化 Git 和同步远程仓库。不要把 Token 写入 remote URL 或提交到仓库。
