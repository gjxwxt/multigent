# Multigent 内网运行时与依赖治理 — 最终实施方案

> 状态：P0 实施契约（经评审收敛；路线图不是实施授权）。
> 本文是给实现 Agent 的执行契约。**只实现 P0；P1–P4 是既定路线图，不要提前实施。**
>
> 证据边界：带 `file:line` 的条目是本工作树源码事实；“镜像/VM 实测”是当时的运行证据，
> 仅在部署或镜像测试时重新验证，不能据此假定当前工作树、当前 VM 或自定义镜像仍相同。

---

## 0. 背景与目标

Multigent 迁入企业内网后，Agent 沙箱必须从受控的 Nexus/Harbor/GitLab 获取依赖；运行时版本、证书、缓存来源需可预检、可追溯。

**不做的事（边界）：**
- 不把任意宿主机目录开放给项目/任务挂载；
- 不把 Registry Token、代理密码、证书密码写进 `multigent.conf`；
- 不依赖 `latest`、公网 `direct` 兜底或关闭校验来"临时跑通"；
- **不改变任何现有默认行为**——公网/国内镜像（CN_MIRROR）场景原样保留，内网模式仅在显式配置后生效。

---

## 1. 已核实的代码事实（实现时直接引用，勿重新调查）

| 事实 | 位置 |
|---|---|
| 环境变量注入顺序（docker `-e` 后者覆盖前者）：<br>① 固定沙箱 env → ② `wellKnownEnvKeys` 白名单透传（`-e KEY` 从宿主继承）→ ③ 项目 `ExtraEnv` → ④ hostUser 覆盖（最后） | `internal/sandbox/docker.go:215-250` |
| 平台组合的 `PATH` 在项目 ExtraEnv **之后**由 `DockerProvider.Command` 追加 → 项目级 PATH 今天实际不生效 | `internal/runenv/spec.go:76` |
| 白名单现有内容：代理全家（大小写）+ `NPM_CONFIG_REGISTRY`，仅此一个 registry | `internal/sandbox/docker.go:1019-1023` |
| `DockerReachableProxyEnvValue` **存在**将 localhost 代理改写为 `host.docker.internal` 的辅助函数，但当前 `BuildArgs` 的白名单透传只传 `-e KEY`，**没有调用它** | `internal/sandbox/docker.go:228-231, 993-1014` |
| appconfig 是**手写行式解析器**（bufio+strings，非 TOML 库）；`setValue` 对未知 key、未知 section **静默忽略** | `internal/appconfig/config.go:87-175` |
| 配置→环境变量桥：`applyConfigEnv` 用 `setEnvIfEmpty`，现有桥接 `MULTIGENT_RUNTIME_IMAGE`/`MULTIGENT_RUNTIME_REGION`/`NPM_CONFIG_REGISTRY` | `cmd/multigent/root.go:208` |
| 四个命名卷硬编码：toolchains/npm-cache/go-cache/go-build-cache | `internal/runenv/spec.go:59-63`；属主处理 `internal/sandbox/docker.go:921-936` `EnsureVolumeOwnership`（chown 为服务 uid/gid） |
| Agent CLI 安装：marker 幂等机制，**无并发锁**；`NPM_CONFIG_PREFIX=$MULTIGENT_TOOLCHAIN_HOME/npm` | `internal/agentcli/agentcli.go:139-173`（npmInstaller）、`:175+`（scriptInstaller） |
| `flock` 已存在于 runtime-base 镜像（util-linux 2.39.3，`/usr/bin/flock`）——**无需加包**（已实测） | 镜像内 |
| **生产沙箱容器全部以非 root 运行**：`runAsHostUser` Linux 默认 true（`--user <服务uid:gid>`），生产服务用户为 `gao` | `internal/sandbox/docker.go:97-99, 827`；VM 实测 systemd `User=gao` |
| runtime-base 镜像实测版本：Ubuntu 24.04，Node v22.23.1（nodesource 构建），pip **24.0**（系统包），Python 3.12.3，**无 JDK** | 镜像实测 |
| Node（nodesource 构建）**不读系统 CA store**（内嵌 Mozilla 根库）；pip 24.0 的信任来自 certifi bundle——两者都不受 `update-ca-certificates` 影响 | 实测版本推断 |
| CN_MIRROR 构建写入 `GOPROXY=https://goproxy.cn,direct GONOSUMDB='*'`（已知不安全默认，P1 才修，**P0 不动**） | `docker/runtime-base/Dockerfile:108-110` |
| 项目级 `extra_volumes`/`extra_env` 存在且支持 `KEY=VALUE` 显式注入 | `internal/entity/types.go:465-484`、`docker.go:233-241` |
| npm scoped auth 已有独立机制（连接密钥 → `.npmrc`），与全局 registry 透传互不相干 | `internal/runner/runner.go:3127` `materializeNPMRegistryConfig` |

---

## 2. P0 实施范围（本次交付）

> 目标：**平台注入基础**——强类型 registries 配置、严格解析、Agent CLI 安装锁、真实客户端探针。
> 明确不做：不动 Dockerfile（P1）、不动卷挂载（P2）、不做 runtime profile 权限（P3）、不引入 mise（P4）。

### 2.1 新增配置段（强类型，拒绝自由 env 表）

在 `internal/appconfig/config.go` 新增：

```go
type RegistriesConfig struct {
    NPM string   // → NPM_CONFIG_REGISTRY
    PIP string   // → PIP_INDEX_URL
    Go  string   // → GOPROXY（企业内网值不带 ,direct）
    GoSumDB string // → GOSUMDB（受管 checksum database；不得为 off）
    GoPrivate string // → GOPRIVATE（逗号分隔的企业模块域名 pattern）
}

type NetworkConfig struct {
    HTTPSProxy string // → HTTPS_PROXY / https_proxy
    HTTPProxy  string // → HTTP_PROXY  / http_proxy
    NoProxy    string // → NO_PROXY / no_proxy
}
```

```ini
[registries]
npm = "https://nexus.corp.example/repository/npm-group/"
pip = "https://nexus.corp.example/repository/pypi/simple/"
go = "https://nexus.corp.example/repository/go/"
go_sumdb = "sum.corp.example"
go_private = "git.corp.example/*"

[network]
https_proxy = "http://proxy.corp.example:3128"
no_proxy = "localhost,127.0.0.1,.corp.example"
```

**桥接**（`cmd/multigent/root.go` `applyConfigEnv`，全部 `setEnvIfEmpty`）：
- `registries.npm` → `NPM_CONFIG_REGISTRY`（已有，勿重复添加）
- `registries.pip` → `PIP_INDEX_URL`；`registries.go` → `GOPROXY`；`registries.go_sumdb` → `GOSUMDB`；`registries.go_private` → `GOPRIVATE`
- `network.*` → 大小写两个变体（小写留给已有透传语义）

**透传白名单扩展**（`docker.go` `wellKnownEnvKeys` 的 `common` 数组）：
`PIP_INDEX_URL`、`GOPROXY`、`GOSUMDB`、`GOPRIVATE`。四个 key 全部走既有 `-e KEY` 继承路径，值不出现在 docker argv 中。

**设计约束（硬性）：**
- 注入必须落在第②步（透传段），**不得**在项目 ExtraEnv 之后追加——保持"项目级可覆盖全局"的语义；
- 类型化字段天然杜绝 `PATH`/`HOME`/`MULTIGENT_*` 进入配置——不要提供任何"任意键值"逃生口；
- `MULTIGENT_*` 前缀保留给平台控制变量，任何新配置字段不得映射到该前缀。
- 新增 registry URL 必须是绝对 `https://` URL、不得含 userinfo、query 或 fragment；`registries.go` 是单一代理 URL，不得含 `,direct`。不满足即在加载配置时失败，不能延迟到容器内才报错。
- 一旦设置 `registries.go`，必须同时设置受管的 `registries.go_sumdb`；后者不得为 `off`。它沿用 Go 的 GOSUMDB 格式（可带受信任的 name/key/URL 组合），实现仅做格式与 `off` 防护，不把私有模块误塞进 `GONOSUMDB`。这是防止真实 Go 构建在校验时暗中访问默认公网 checksum database 的硬要求。
- `[network]` 的代理 URL 同样不得含 userinfo。P0 **不**启用现有但未接线的 localhost 自动改写：配置值必须已经是容器可达地址（Docker bridge 下可用 `host.docker.internal`）。这样既不把代理密码放入 docker argv，也不改变既有宿主环境变量的透传语义。

### 2.2 appconfig 严格解析（未知 key/section 报错）

- `setValue` 加 `default` 分支：`return fmt.Errorf("unknown key %q in section [%s]", key, section)`；
- `Load` 的 section 切换处校验 section 名合法（workspace/server/auth/smtp/logging/runtime/sandbox/sandbox.e2b/playbooks/registries/network）；
- `sandbox.e2b` 作为子段处理方式保持现状；
- 这里的“严格”只指 schema（未知 key/section）和新增 registry/network 值校验；不要顺带改变既有整数、布尔值或数组的宽容解析语义。
- 全量补单测：每个 section 的合法 key 集合、未知 key 报错、未知 section 报错、新增 URL 校验、多行数组与注释行为不回归。

### 2.3 Agent CLI 安装并发锁

位置：`internal/agentcli/agentcli.go` 的 `npmInstaller` 与 `scriptInstaller`。

- 锁文件：`$MULTIGENT_TOOLCHAIN_HOME/markers/locks/<marker-hash>.lock`。`marker-hash` 必须复用 `markerPath(cfg)` 的完整身份输入（vendor/package manager/package/version/channel），不能只用 binary+version；
- `npmInstaller`：用 `flock` 包住**整个临界区**：marker 检查 → `npm install -g` → `touch marker`；
- `scriptInstaller` 当前没有 marker/idempotence 协议。P0 只将 `cfg.Install` + binary/check 校验串行化，**不要**未经设计把 npm 的 marker 语义复制过去；
- `flock` 必须使用有界 `-w` 等待时间（覆盖最长安装超时并留余量），超时给出“同一 toolchain 正被其他容器安装”的明确错误；先 `command -v flock`，自定义 runtime image 缺失该命令时 fail-closed；
- 实现方式：临界区主体写入临时脚本或用 `flock <lockfile> sh -c '...'` 包装（保持与现有 shell 字符串拼接风格一致）；
- 脚本内 `mkdir -p` 锁目录（marker 目录已由现有代码创建，locks 子目录需补）；
- 锁文件随 `multigent-toolchains` 命名卷共享，属主已由 `EnsureVolumeOwnership` 处理为服务 uid/gid——**不要**把锁放在容器本地路径（跨容器无效）。

单测断言生成的脚本包含 flock 包装且锁路径在 `$MULTIGENT_TOOLCHAIN_HOME` 下。

### 2.4 探针子命令 `multigent runtime probe`

位置：`cmd/multigent/`（Cobra 子命令，与 `sandbox prepare` 同级）。

行为：启动**一次性临时容器**（`docker run --rm`，探完即删），验证已配置依赖源的真实客户端传输链路。它不是带凭据的依赖安装验收，也不得作为 CI/生产发布放行证据。

**探针容器约束（硬性）：**
- 使用与沙箱**相同的镜像**（`MULTIGENT_RUNTIME_IMAGE` 或配置的 runtime.image）、bridge 网络参数及依赖/代理环境语义；若真实沙箱加 `host.docker.internal`，探针也必须加同样的 host-gateway 映射；
- **禁止调用 `sandbox.BuildArgs`，也禁止靠“先 BuildArgs 再删参数”实现。** 它会加入工作区、agent 目录、模型相关白名单和 host-user 行为，违反本命令的零工作区、零模型凭据边界。抽取一个仅服务于依赖传输的共享 helper，allowlist 固定为代理变量、`NPM_CONFIG_REGISTRY`、`PIP_INDEX_URL`、`GOPROXY`、`GOSUMDB`、`GOPRIVATE`；探针与真实沙箱各自调用该 helper。新增测试必须断言 probe docker argv 中没有 workspace mount、credential mount 或任何模型 API key；
- **以真实 Agent 相同的非 root UID/GID 运行**，并在 `/tmp` 下创建独立 probe HOME/缓存目录；不挂工作区、不注入模型 API key/凭据。P0 已不探测 APT，不能为了不存在的 `apt-get update` 需求改成 root 并掩盖权限问题；
- 网络模式与真实沙箱一致（默认 bridge）。

探针动作（**必须用真实客户端，禁止 curl**——curl 走系统 truststore 会假绿，掩盖 Node/pip 独立信任库问题）：
| 源 | 命令 |
|---|---|
| npm | `npm view <probe package> version`（registry 取注入值） |
| pip | `pip download --no-deps --dest /tmp <probe package>` |
| Go | 临时目录中 `go mod init probe && go mod download <probe module>@<version>`（同时走已注入的 GOPROXY 与 GOSUMDB） |
| 代理（如配置） | 以上任一动作经由代理即验证 |

`apt` 不属于 P0 探针：P0 没有受管 APT 源，也不改 runtime image；对默认公网 apt 源执行 `apt-get update` 会造成与“只验证显式配置源”相反的假绿。APT 真客户端验收属于 P1 的 `corp.sources` 镜像交付。

探测包/模块默认固定为 `npm`、`pip`、`golang.org/x/mod@v0.24.0`，并允许用明确 flag 覆盖，避免企业镜像库没有默认探针工件时产生无意义失败。至少提供 `--npm-package`、`--pip-package`、`--go-module`（含版本）三个 flag；默认值与最终命令行 help 一起固化为测试。没有配置的 registry 跳过而非探测公网。

输出：逐源 `PASS` / `AUTH_REQUIRED` / `FAIL` + 耗时 + 错误摘要（**对全部捕获输出做 URL userinfo 脱敏**）。无凭据场景下可证明 TLS/网络链路但需要认证的结果只能标记 `AUTH_REQUIRED`，不得伪称依赖可用；真正的带凭据依赖安装验收留给后续受控 runner 阶段。退出码固定为：全部所选源 `PASS` 为 `0`；有任一 `FAIL` 为 `1`（优先级最高）；无 `FAIL` 但有 `AUTH_REQUIRED` 为 `2`；没有任何已配置或显式选择的源为 `3`。JSON/文本格式与这套语义必须有单测，避免脚本把它误判成绿灯。

实现注意：探针容器命令以**位置参数**直传 docker（`cmdArgs []string`，经 `validateProbeTarget` 白名单校验），禁止再退回 `sh -c <拼接字符串>` 形态；超时控制（整体 120s，可 flag 覆盖）；探针不写任何持久状态。

### 2.5 测试与验证门禁

- `make test` 全绿（新增：appconfig 严格解析、registries/network 桥接、白名单透传、flock 脚本断言、探针参数构造）；
- `make build` 通过；
- 本机功能验证：临时 `multigent.conf` 配置 registries + probe 包/模块 → `multigent runtime probe` PASS；故意配错端口 → FAIL 且退出码非零；
- 默认行为回归：对**原本合法的空配置**，创建沙箱运行一个任务，行为与改前完全一致（无新增 env、无锁副作用）。未知 key/section 由本次安全改造起失败，是有意且应单独验证的兼容性变化。

### 2.6 配置文档同步

- 更新 `config.example.toml`，只添加注释化的 `[registries]` / `[network]` 字段说明与 `*.example` 占位值；不得写入真实内网域名、代理地址、账号或 token；
- 更新 `docs/getting-started/configuration-and-logging.md` 及中文对应文档，说明优先级仍为 CLI/环境变量优先于配置、未知 key/section 从本版本起会拒绝启动、`go_sumdb` 是启用内网 GOPROXY 时的完整性前提；
- 本地运维 SOP、实际 registry/代理地址仍只留在本文件或部署 runbook，不能复制进公开样例。

---

## 3. P1–P4 路线图（本轮不实现，防止实现 Agent 越界）

**P1 企业 runtime-jvm21 镜像**：
- JDK 21 + 企业 APT 源（镜像内置 `corp.sources`，非运行时 env）+ 系统 CA + Java cacerts + `NODE_EXTRA_CA_CERTS` + `PIP_CERT`（两者都指向镜像内同一 bundle 路径）；
- Gradle：受管 init script 放**独立只读挂载**（防 Agent 篡改），`GRADLE_USER_HOME` 挂独立命名卷（可写），init.d 只读覆盖其上——init script 与缓存落点耦合，P1 一次做掉；
- 受保护不可覆盖 tag（如 `runtime-jvm21:2026.10.1`），启动日志记录实际 digest，禁 `latest` 进生产；
- 修正 Dockerfile:109 的 `GONOSUMDB='*'`：CN 公网场景保留 `,direct`，企业镜像去 `,direct` 且 GONOSUMDB 收敛到企业域——**做成构建参数，不翻默认值**；
- CA 不进包装脚本动态安装：生产容器以非 root 运行，`update-ca-certificates` 必失败。P1 的完成定义是镜像内置的受管 CA/源；运行时 CA 轮换**不属于 P1**，必须另行评审。后续方案至少要做到：受控 Docker root helper 生成版本化、只读的完整系统信任目录 + 合并 PEM bundle + 经 `keytool` 校验的 truststore；新容器切换到新版本后才回收旧卷；校验非 root sandbox 用户可读；拒绝项目 mount/env 覆盖受管信任路径。不能只写两个文件或原地修改正在使用的卷。

**P2 受管缓存根目录**：`named_volume | bind` 双模式；只切宿主机挂载源，容器内路径（`/opt/multigent/toolchains` 等）**永不变更**；toolchains 按 profile/架构隔离，npm/go/pip 依赖缓存共享（内容寻址，勿按镜像 digest 切分）；权限/磁盘/迁移/回滚检查；先测试工作区试点。同时改 `spec.go:59` 与 `docker.go:544` 两处硬编码。

**P3 权限与可观测性**：runtime profile 白名单（**新权限面**：UI + API + 校验 + 迁移，独立立项）；控制台展示实际镜像 tag+digest、缓存模式、各源连通状态（不展示凭据）；GitLab Runner 第三平面接入验收。

**P4 mise（可选）**：仅在"同平台多版本并存"成为真实需求后引入；需完整 `MISE_DATA_DIR` 预热 + 后端下载源镜像 + 并发锁。**不用它填补 JDK 空缺**（那是 P1 的活）。

---

## 4. 执行注意事项（实现 Agent 必读）

### 4.1 工作区纪律
1. **开工先 `git status --short --branch`**：此工作树可能有其他会话并发写入，任何旧报告中的“未提交改动清单”都不是当前事实。把命令输出作为唯一基线；逐文件确认后再改，禁止覆盖他人未提交工作。
2. **不提交**。改完留在工作区，由用户审计后自行提交。提交信息规范也不用准备。
3. 完成后输出变更文件清单 + 每个文件改了什么（供审计）。

### 4.2 安全面（违反即事故）
4. 用户在 UI 配置的 agent 模型、连接凭据**一个字节都不动**。
5. 新配置字段**禁止接受含 userinfo（`user:pass@`）的 URL**——解析时检测并报错；日志/探针输出打印 URL 前必须 redact userinfo。
6. 凭据不落盘：`.npmrc` auth 已有独立机制（`materializeNPMRegistryConfig`），不要把 token 塞进 registries 配置或环境变量桥。
7. 探针容器以真实 Agent 相同的非 root UID/GID 运行且**零凭据零工作区**——不要为了"验证更真实"往里挂东西。
8. 透传白名单只加 `PIP_INDEX_URL`/`GOPROXY`/`GOSUMDB`/`GOPRIVATE` 四个 key，**不要顺手加别的**。

### 4.3 兼容性与部署
9. **严格解析的部署风险**：生产 VM 的 `/opt/multigent/data/` 下 conf 文件若含历史遗留键，严格模式会导致启动失败。部署前先在 VM 上审计实际 conf 内容；若存在未知键，先决定是补 schema 还是清理配置，**不允许为绕过而放宽严格解析**。
10. 默认行为零变化是硬要求：对原本合法的空配置，`setEnvIfEmpty` 语义保证不配置即不生效；回归测试必须包含"空配置前后 BuildArgs 输出一致"。未知 key/section 的拒绝是本次明确授权的例外。
11. 部署 SOP（Mac → VM）：`make build` → `GOOS=linux GOARCH=amd64 go build`（multigent 与 mga 两个产物）→ `orb -m ubuntu sudo cp` 到 `/opt/multigent/bin/` → `systemctl restart multigent` → `journalctl -u multigent` 确认无 panic。
12. `flock` 方案仅在单 Docker 主机内有效（锁文件经共享命名卷跨容器生效）；多主机部署是未来课题，代码注释里写明这一限制。
13. 不改 `docker/runtime-base/Dockerfile`（CN_MIRROR/GONOSUMDB 的不安全默认是 P1 的事，P0 碰它会破坏国内镜像构建）。

### 4.4 工程习惯
14. appconfig 是手写解析器：不支持变量展开、引号嵌套；新 section 解析复用 `stringValue` 等现有 helper，风格与 `setValue` 现有 switch 保持一致。
15. 测试跟随现有表驱动风格；探针子命令的可测部分（参数构造、输出格式、redact）单独抽函数测。
16. 本文件当前未跟踪，且含本地部署 SOP；除非用户明确要求且已剥离私有运维信息，不得顺手提交到公开仓库。
17. 完成定义：`make test` + `make build` 全绿 + 本机探针功能验证 + 空配置回归，缺一不可，未验证不得声称完成。

---

## 5. 验收清单（P0 完成定义）

- [ ] `[registries]`/`[network]` 配置段解析 + 桥接 + 透传，全链路单测覆盖
- [ ] 未知 key/section 报错，全部既有 section 的合法 key 有测试锚定
- [ ] npmInstaller 的 marker 临界区与 scriptInstaller 的安装/check 串行化；锁以完整 marker 身份派生、位于共享卷且有界等待
- [ ] `multigent runtime probe`：真实客户端探活已配置的 npm/pip/go 源（Go 同时验证 GOPROXY/GOSUMDB）；与 Agent 相同 UID/GID 的临时容器、零凭据/零工作区/零模型 key、redact 输出、固定退出码；APT 留给 P1
- [ ] 空配置回归：BuildArgs 输出与改前一致
- [ ] 配置样例与中英文配置说明已同步，且只含占位值、无内网或凭据泄露
- [ ] `make test` + `make build` 全绿
- [ ] 变更清单已输出，未做任何 git commit
