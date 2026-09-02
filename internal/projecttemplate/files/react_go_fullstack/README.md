# React + Go Fullstack Starter

一个可直接开发、构建和预览的前后端一体项目模板：

- `web/`：React + Vite 前端，开发时将 `/api` 代理到 Go API。
- `server/`：Go 标准库 HTTP API，提供确定性的健康检查接口。
- `.multigent/runtime.json`：预览引擎的启动与就绪探针契约。

## 本地开发

```bash
make doctor
make install
make verify
make dev
```

前端默认运行在 `http://127.0.0.1:27891`，后端运行在 `http://127.0.0.1:8080`。

## 验证

```bash
make verify
curl http://127.0.0.1:8080/api/health
```

初始化任务会按平台的 Project Initialization 流程执行依赖安装、测试、构建、健康检查、Git 初始化和远程同步。不要把 Token 写入 remote URL 或提交到仓库。

## 构建产物

```bash
make build   # 产出 server/bin/app：前端产物已内嵌（go:embed webui/dist），单文件即完整应用
```

## CI/CD 与部署

仓库自带 GitLab CI 基线（`.gitlab-ci.yml` + `deploy/`），推送到 GitLab 后自动生效：

- push / MR：`lint → test → build`（后端 go vet/test，前端构建并内嵌进二进制）。
- 打 tag：追加 `package → deploy`——就地构建镜像 `deploy/Dockerfile`，经 `deploy/compose.yml` 发布到 runner 宿主 `APP_PORT`（默认 8088）并探活。

```bash
git tag v0.1.0 && git push origin v0.1.0   # 触发打包部署链
```

环境差异（发布端口等）在 GitLab 项目 Settings > CI/CD > Variables 覆盖，不改 YAML。
runner 标签约定与内网迁移注意事项见 `.gitlab-ci.yml` 头部注释。
