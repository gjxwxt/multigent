# 环境变量与配置面参考 + 内网迁移卡点（2026-09-14）

> 目标读者：迁移执行者、平台配置审计者。事实来源为 `grep 'MULTIGENT_' internal/ cmd/` 全量扫描（51 个变量，2026-09-14）与 systemd 实测。
> 定位：与 `docs/intranet-runtime-plan.md`（P0 实施契约，未动）互补——那边定架构决策，这边给**现状清单 + 迁移日核对表 + 公共配置面设计建议**。
> 安全面：本文档全部使用占位符，不含真实内网地址、凭据或部署细节。

---

## 1. 配置面架构（现状）

```text
┌────────────────────────────────────────────────────────────────────┐
│                        multigent 进程 (VM systemd)                  │
│                                                                    │
│  /opt/multigent/data/multigent.conf      systemd Environment=      │
│  (appconfig 行式解析器)                   + .service.d/*.conf       │
│         │ applyConfigEnv (setEnvIfEmpty)        │                  │
│         ▼                                       ▼                  │
│  ┌────────────────── 进程环境变量（合并后） ─────────────────┐       │
│  └───────────────────────┬───────────────────────────────────┘       │
│                          │                                           │
│        ┌─────────────────┼──────────────────┐                        │
│        ▼                 ▼                  ▼                        │
│  ① 沙箱白名单透传     ② 进程内消费       ③ 派生注入                  │
│  TransportEnvKeys    MULTIGENT_*      JAVA_TOOL_OPTIONS            │
│  (proxy 全家/         直读 os.Getenv  (jvm_proxy.go 从 NO_PROXY     │
│   NPM/PIP/GO 系列)    (51 个 key)      派生 -D 属性)                │
│        │                 │                  │                        │
│        ▼                 ▼                  ▼                        │
│  agent 容器           server 自身        预览/agent JVM 容器          │
│  (docker -e KEY)      (HTTP 监听等)      (JAVA_TOOL_OPTIONS=-e)      │
└────────────────────────────────────────────────────────────────────┘
```

三条注入路径的**优先级**（对同一 key）：项目 ExtraEnv > hostUser 覆盖 > 白名单透传 > 配置桥（setEnvIfEmpty 只在为空时设）。详见 intranet-plan §1 表。

## 2. 环境变量清单（按消费方分类，51 个）

### 2.1 网络与代理（迁移日必查）
| 变量 | 消费方 | 说明 |
|---|---|---|
| `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY`（大小写） | 沙箱白名单透传 + jvm 派生 | NO_PROXY 派生 JVM nonProxyHosts；**漏列内网服务 = 该服务经代理必挂** |
| `MULTIGENT_API_URL` | 沙箱→控制面回连 | 容器内可达性预检（`RuntimeAPIReachableFromContainer` 走 host.docker.internal） |
| `MULTIGENT_PUBLIC_URL`/`MULTIGENT_CONSOLE_URL`/`MULTIGENT_WEB_BASE_URL` | server | 对外 URL；迁移后域名/端口变化处 |
| `CHATOPS_CALLBACK_BASE_URL` | daemon | Mattermost 回调；fail-closed，缺失即审批按钮不可用 |

### 2.2 运行时与镜像
| 变量 | 说明 |
|---|---|
| `MULTIGENT_RUNTIME_IMAGE` | 显式镜像覆盖（最高优先）；内网切 Harbor 的主开关 |
| `MULTIGENT_RUNTIME_REGION=cn` | 公网 CN 镜像源；内网模式不用 |
| `NPM_CONFIG_REGISTRY`/`PIP_INDEX_URL`/`GOPROXY`/`GOSUMDB`/`GOPRIVATE` | 沙箱透传白名单（intranet-plan P0 的 registries 桥目标） |

### 2.3 存储与数据
`MULTIGENT_DATA_DIR`（workspace 根）、`MULTIGENT_CONTROL_DATA_DIR`（控制面 DB 重定向，测试用）、`MULTIGENT_CONFIG`（conf 路径覆盖）、`MULTIGENT_LOG_FILE`、`MULTIGENT_SHUTDOWN_TIMEOUT`、`MULTIGENT_SERVER_ADDR`。

### 2.4 安全与凭据（迁移敏感）
| 变量 | 说明 |
|---|---|
| `MULTIGENT_CONNECTION_ENCRYPTION_KEY` | 连接凭据加密主钥；**迁移必带**，丢了所有已存凭据作废 |
| `MULTIGENT_REQUIRE_ENCRYPTED_SECRETS` | 凭据硬闸：置 `1` 后缺主钥时所有密封路径（连接/模型商/OAuth）直接报错，不再落明文（dev 兜底关闭）；内网/生产建议开启 |
| `MULTIGENT_TRUSTED_PROXY_SECRET` | 反向代理信任链 |
| `MULTIGENT_WORKER_TOKEN`/`MULTIGENT_WORKER_ID`/`MULTIGENT_WORKER_MODE`/`MULTIGENT_WORKER_WORKSPACE` | Runtime Node 注册 |
| `MULTIGENT_WEB_API_KEY` | Web API key |
| `MULTIGENT_ALLOW_SIGNUP` | 注册开关（内网建议 false） |

**凭据安全基线（2026-09-14 起）**：服务启动时审计三类密钥存储面（connection_secrets / model_providers.api_key / oauth_client_configs），存在明文记录则输出 `[secrets-baseline] WARNING`（只含表名/计数，绝无密钥内容）。配套 CLI：`multigent secrets audit`（盘点，退出码 2 = 有明文）与 `multigent secrets migrate [--apply]`（默认 dry-run；--apply 自动备份控制 DB 后重加密，退出码 3 = 部分失败可重跑）。迁移顺序：生成 key（`openssl rand -hex 32`）→ `secrets migrate --apply` → 服务注入 key（systemd drop-in）并重启 → audit 复核零明文。已加密记录缺 key 时读取 fail-closed（报错不降级），回滚 = 用迁移自动备份（`multigent.db.pre-encrypt-<ts>`）恢复 DB 文件。

### 2.5 GitLab / CI
`MULTIGENT_GITLAB_RUNNER_ID`（默认 runner 自动绑定；8.4 缺陷修复的配置前提，当前经 systemd drop-in 注入）。

`MULTIGENT_CI_REMOTE_PIPELINE_REQUIRED`：ci_ready 闸门的工作区级**默认值**（非强制下限）——未显式声明 `remote_pipeline_required` 的项目在置 `1`/`true`/`yes`/`required` 后走严格闸门（未绑远端 = pipeline_evidence FAIL）。项目级声明（`required`/`local`）优先于该默认值；如未来需要组织级强制策略（项目级 `local` 不得覆盖），应另立独立设置而非复用此变量。另：远端自动认领授权可用 `MULTIGENT_GITLAB_ADOPT_NAMESPACE_ALLOWLIST`（逗号分隔命名空间，段精确前缀匹配）放开平台未建仓场景，默认关闭。

### 2.6 嵌入式/子进程内部（迁移时**勿带**）
`MULTIGENT_WORKER_*`（node 自注册）、`MULTIGENT_WORKTREE_DIR`/`MULTIGENT_WAKEUP_*`（attention 派发）、`MULTIGENT_RUNTIME_NODE_DAEMON_CHILD`、`MULTIGENT_RUN_ID`——这些是进程间契约，出现在迁移清单里会造成噪音。

## 3. 内网迁移核对表（执行日逐条打勾）

```text
[ ] 1. multigent.conf + systemd drop-in 逐条核对（drop-in 是隐形配置面，漏带 = 代理/runner 静默失效）
[ ] 2. NO_PROXY 列全：新内网 GitLab / 控制面 / host.docker.internal / DNS 名（jvm 派生依赖它）
[ ] 3. MULTIGENT_CONNECTION_ENCRYPTION_KEY 原样迁移（丢了凭据库即作废）
[ ] 4. MULTIGENT_RUNTIME_IMAGE 指向 Harbor 受管 tag（禁 latest）；DIGEST 记录进验收文档
[ ] 5. registries 段（intranet-plan P0）就位：npm/pip/go 四件套 + 严格解析对存量 conf 先审计
[ ] 6. 预热：镜像 + toolchains 卷 + npm/go 缓存卷先在目标网跑 `sandbox prepare`
[ ] 7. 探针先行：`multigent runtime probe`（P0 交付后）+ RuntimeAPIReachableFromContainer 在目标网验证
[ ] 8. 凭据落点验收：agent CLI 凭据文件全部位于 /tmp/multigent-session（HOME 已无挂载）
[ ] 9. 部署链切换：OrbStack /mnt/mac 路径 → registry/scp（旧 SOP 仅限当前拓扑）
[ ] 10. 文档泄密面扫描：跟踪中文档不得含内网 IP（见 §4-4）
```

## 4. 公共配置面设计建议（演进方向，非本轮实施）

1. **单一事实源**：现状"conf + systemd Environment + 进程内 Getenv"三层并存，迁移审计要扫三处。建议 P0/P1 落地时以 conf 为唯一声明源，systemd 只保留 secrets（加密 key/token），其余全部收进 conf 的强类型 section——appconfig 严格解析（intranet-plan §2.2）正好是承载面。
2. **`multigent config check`**：启动前自检命令，输出"生效配置快照"（secret 打码）。比 `systemctl show` + grep 源码拼凑可靠得多，迁移日收益直接。
3. **配置项分代标注**：每个配置在文档里标"嵌入式内部 / 部署面 / 开发面"（§2.6 的分类就是起点），避免内部契约变量被误当部署配置带进生产。
4. **NO_PROXY 声明化**：与其手写逗号串，不如 conf 里声明"内网服务清单"，由平台生成 NO_PROXY 与 JVM nonProxyHosts 两个视图——消除两处手写漂移（现网 GitLab 主机/控制面主机的手写重复已现端倪）。

## 5. 与既有文档的关系

- `docs/intranet-runtime-plan.md`：P0 契约（registries/严格解析/锁/探针），本文不重复、不改其范围。
- `docs/executor-blindspots-2026-09-14.md`：执行盲点与 Feature 优先级。
- `HANDOFF.md` §10.18：P2 soak 启动清单（本地文档）。
