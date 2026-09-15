# React + Go Fullstack Starter

一个可直接开发、构建和预览的前后端一体项目模板：

- `web/`：React + Vite 前端，开发时将 `/api` 代理到 Go API。
- `server/`：Go 标准库 HTTP API，SQLite 持久层（`modernc.org/sqlite` 纯 Go 驱动）。
- `.multigent/runtime.json`：预览引擎的启动与就绪探针契约。
- `.multigent/fixtures.json`：测试数据沙盒契约（基线/场景种子命令、schema 指纹路径）。

## 数据库与测试数据

应用数据库路径由 `APP_DB_PATH` 环境变量决定（默认 `server/data/app.db`，已 gitignore——schema 进 Git，数据永不进 Git）：

```bash
make db:seed:baseline                      # 装载确定性演示基线（5 客户 / 10 订单）
make db:seed:scenario NAME=edge_cases      # 边界场景（超长文本/SQL注入样文本/emoji/200 行分页量）
make db:seed:scenario NAME=default         # 与基线一致，用于场景复位
```

种子是确定性的：同一二进制 + 同一目标库，任意机器任意时间产出的**逻辑导出摘要**（`go run ./cmd/dbseed digest`）完全一致。Multigent 的测试数据沙盒按 `.multigent/fixtures.json` 的契约在预览启动前自动执行装载；修改 `server/migrations/*.sql` 会改变 schema 指纹，需要重新发布基线 artifact。

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
