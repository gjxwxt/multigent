# 配置与日志

## 配置来源

长期运行命令支持 TOML 配置文件：

```bash
multigent --config ./config.toml start
multigent --config ./config.toml api serve
multigent --config ./config.toml daemon install
```

优先级：

```text
CLI 参数 > 环境变量 > config.toml > 默认值
```

也可以通过环境变量指定全局配置文件：

```bash
MULTIGENT_CONFIG=/etc/multigent/config.toml
```

示例配置见 [config.example.toml](../config.example.toml)。

> **严格配置解析**：自本版本起，配置文件中任何未知的配置段（section）或未知键名（key）在服务启动时都将严格报错拒绝，以防止拼写错误导致关键配置静默失效。

当前配置覆盖：

- workspace 数据目录 (`[workspace]`)
- server 监听地址 (`[server]`)
- API key (`[auth]`)
- SMTP 邀请邮件 (`[smtp]`)
- 服务日志 (`[logging]`)
- 运行时镜像与地区 (`[runtime]`)
- Sandbox 宿主直跑与自托管 E2B API URL (`[sandbox]`, `[sandbox.e2b]`)
- 远程协作方案 registry (`[playbooks]`)
- 依赖仓库源 (`[registries]`): `npm`、`pip`、`go`、`go_sumdb`、`go_private`
  - *说明*：配置 `go` (`GOPROXY`) 时，必须同时配置受管的 `go_sumdb`（不得为 `off`），作为启用内网 Go 代理时的完整性前提，杜绝在校验时向默认公网 checksum 服务泄漏模块并确保依赖安全。
- 网络代理 (`[network]`): `https_proxy`、`http_proxy`、`no_proxy`

## 依赖源与运行时探针 (Runtime Probe)

在企业内网部署时，可通过 `[registries]` 和 `[network]` 配置企业私有源和网络代理。配置的依赖与网络变量会通过 Docker 的 `-e KEY` 继承机制注入 Agent 沙箱，保证凭据不会泄露在命令行参数或日志中。

要预检运行时容器能否通过真实客户端链路连接并下载配置源中的依赖工件，可使用探针子命令：

```bash
multigent runtime probe
# 或输出为 JSON 结构：
multigent runtime probe --format=json
# 或指定外部配置文件：
multigent --config /path/to/multigent.conf runtime probe
```

探针运行在与沙箱相同 UID/GID 的非 root 一次性临时容器中，不挂载任何工作区，不透传模型 API Key 与敏感凭据。

## 日志策略

Multigent 有两类日志：

- 服务日志：API/Web 进程生命周期和平台事件。
- Agent 运行日志：每次 agent 执行的 stdout/stderr 和对话转录。

服务日志默认：

- 路径：`~/.multigent/logs/multigent.log`
- 格式：JSON
- 级别：`info`
- 轮转：保留一个 `<log>.1`
- 默认大小：`10 MB`

生产环境推荐：

```toml
[logging]
file = "/var/log/multigent/multigent.log"
level = "info"
format = "json"
max_size_mb = 100
stderr = false
```

Agent 运行日志仍然归属到对应 agent workspace：

```text
projects/<project>/agents/<agent>/.multigent/runs/
```

服务日志应保持结构化，便于汇总到中心日志系统；运行日志是某次执行的产物，主要用于回放和排障。

## 常用环境变量

日志：

- `MULTIGENT_LOG_FILE`
- `MULTIGENT_LOG_LEVEL`
- `MULTIGENT_LOG_FORMAT`
- `MULTIGENT_LOG_MAX_SIZE_MB`
- `MULTIGENT_LOG_STDERR`

服务：

- `MULTIGENT_SERVER_ADDR`
- `MULTIGENT_API_ADDR`
- `MULTIGENT_WEB_API_KEY`

SMTP：

- `MULTIGENT_SMTP_HOST`
- `MULTIGENT_SMTP_PORT`
- `MULTIGENT_SMTP_USERNAME`
- `MULTIGENT_SMTP_PASSWORD`
- `MULTIGENT_SMTP_FROM`
- `MULTIGENT_SMTP_FROM_NAME`
- `MULTIGENT_SMTP_TLS`

Sandbox：

- `MULTIGENT_E2B_API_URL`

协作方案：

- `MULTIGENT_PLAYBOOK_REGISTRY_URLS`

依赖源：

- `NPM_CONFIG_REGISTRY`
- `PIP_INDEX_URL`
- `GOPROXY`
- `GOSUMDB`
- `GOPRIVATE`

网络与代理：

- `HTTPS_PROXY` / `https_proxy`
- `HTTP_PROXY` / `http_proxy`
- `NO_PROXY` / `no_proxy`
