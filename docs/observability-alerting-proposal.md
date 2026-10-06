# 服务监控与告警中心专案计划书（提案，待评审）

状态：**待评审**（评审人：独立评审 Agent）｜ 提出人：gao ｜ 执笔：ZCode ｜ 日期：2026-09-30
关联：`internal/api/deploy_port.go`（部署端口池）、项目侧边栏"部署中心"一期（另案，本计划书只覆盖监控告警）

---

## 1. 需求定案（已经用户两次澄清，评审不得重新开放这两点）

1. **交互形态**：监控告警中心是一个**独立部署的应用**（用户原话："一般这种告警中心都是一个单独的项目，用来部署……相当于是网关，给它单独分配一个端口"）。multigent 控制台的"部署中心"页从它**取数展示**（用户原话："如果要展示东西，就拿里面的一些数据"）。
2. **监控内容（用户口径）**：
   - **接口成功率**（"97、95、99 指的是接口成功率，一般大厂的告警中心展示的这些"）——SLO 口径，非 Trace ID 检索；
   - **前端报错**（JS 异常等）；
   - 服务可用性（崩溃/挂掉）。
3. **走开源优先路线**：优先调研"现代化、优秀、适配内网部署"的开源方案；自建专案是备选。
4. **差异化期望（本计划书补充，用户已认可方向）**：告警可转成 multigent 的 attention 信号 → agent 可领排障任务。这是与 Grafana 类平台的核心差异。

**明确不在本期范围**：日志全文检索（Loki/ELK 类）、机器资源监控详情（CPU/内存曲线，见 §6 开放问题）、Trace ID 链路追踪。

## 2. 现状盘点（代码核实，2026-09-30）

**已有**：
- 每个模板初始化项目分配 `deployPort`（28000-28999 池），持久化在项目上并镜像为 GitLab CI 变量 `APP_PORT`（`internal/api/deploy_port.go`）。
- GitLab 客户端（`internal/codehost/gitlab.go`）：已具备 `SetProjectVariable`，可扩展 pipeline 触发/状态/日志。
- 告警通道：multigent 原生 attention 信号 + IM bridge（飞书/Mattermost）+ agent 任务派发——告警落地端零新建。
- 模板初始化体系（`internal/projecttemplate/`）：模板可版本化升级，新项目可内置 SDK。

**缺失（本专案要补的）**：
- 部署主机地址字段（部署中心一期的缺口，监控中心选址也依赖它）；
- 应用侧指标上报（模板内无 /metrics、无错误上报 SDK）；
- 指标存储、窗口成功率计算、阈值告警、前端错误采集——全链路均无。

## 3. 开源方案调研（2026-09-30 官方文档/GitHub 核实）

| 方案 | 定位 | 许可 | 资源占用（官方口径） | 内网适配 | 与需求的匹配 |
|---|---|---|---|---|---|
| **Uptime Kuma** | 拨测/可用性监控+状态页 | MIT | 单 Node 容器，轻 | ✅ 自托管 | 覆盖"服务挂没挂"（HTTP/TCP/关键字/推送型探针，20s 间隔）；**不覆盖成功率时序与前端报错** |
| **Prometheus (+Alertmanager)** | 指标时序+告警标准件 | Apache-2.0 | 单进程数百 MB 级 | ✅ pull 模型 | **成功率/错误率的标准解**；需应用暴露 /metrics；UI 需配 Grafana（可用 multigent 页替代） |
| **Grafana + Loki + Alloy** | 全栈可观测 | AGPL/混合 | 整栈 GB 级 | ⚠️ 偏重 | 超出需求（日志检索明确不进本期）；VM 已跑 GitLab+Mattermost，叠加压力大 |
| **GlitchTip** | 错误聚合（Sentry SDK 兼容） | 开源（GitLab 托管，需核 LICENSE） | Django+Postgres，~500MB 级 | ✅ 自托管 | **前端报错的标准解**；模板层只需引入 Sentry SDK 指向自建 DSN；后端异常也可收 |
| **Netdata** | 单机全指标 | Agent GPLv3+ UI 闭源免费 | ~150MB/节点 | ✅ | 强在机器/容器资源，弱在应用级 SLO；UI 闭源与许可洁癖冲突；本期不需要 |
| **Keep (keephq)** | 告警汇聚/AIOps 控制台 | 开源（需核 LICENSE） | docker compose 多组件 | ⚠️ | 是**汇聚层**不产指标；12.4k stars 但引入即多一套运维；等告警源多了再考虑 |
| **自建专案** | Go+SQLite 薄告警中心 | — | 极轻 | ✅ | 与技术栈一致、告警直通 agent；但要自造探针/时序/图表，维护成本全在自己 |

**结论性判断**：纯自建在"成功率时序+图表"上重复造轮子收益低；纯外采（Prometheus+Grafana 全栈）超出 VM 承载与需求范围。**推荐混合：外采成熟件做数据层，multigent 自建薄集成层做告警落地与展示**。

## 4. 推荐架构

```
部署主机（现有，跑业务应用）
 └─ 业务应用（模板初始化，带 SDK）
     ├─ /metrics（Micrometer/等价物）        ← Prometheus pull
     ├─ Sentry SDK → GlitchTip（前端+后端异常）
     └─ Push 探针（或由 Kuma 主动拨测端口）

监控主机（multigent VM 或独立 VM，见 §6 Q1）
 ├─ Uptime Kuma（:3001）     可用性拨测+状态页     ← 一期外采件
 ├─ Prometheus（:9090）      成功率/错误率时序      ← 二期外采件
 ├─ GlitchTip（:9000）       前端报错聚合           ← 三期外采件
 └─ multigent（:27892，现有）
     └─ alert-bridge（自建薄模块）
         ├─ 收 Kuma/Prometheus/GlitchTip webhook
         ├─ 阈值判定 → attention 信号 → IM 告警
         ├─ 告警升级为 agent 排障任务（差异化核心）
         └─ 供部署中心页取数渲染成功率卡/错误列表/告警历史
```

原则：**每个外采件独立端口独立进程**（符合用户"单独项目"的心智）；multigent 不自己存指标时序；multigent 自建部分只有一个 alert-bridge 模块（webhook 进 + API 出，无时序库）。

## 5. 分期落地计划

**第 0 期：容量与选址验证（半天，先行）**
- 盘点 VM 现有内存余量（GitLab+Mattermost+runner 已是重负载，这是最大风险，见 §6 Q1）。
- 用 docker stats 实测 Kuma 单件增量，确认可容纳再开工。
- 产出：选址结论（multigent VM 同机 vs 独立 VM）。

**第 1 期：可用性监控 + 告警落地（1-2 天）**
- 部署 Uptime Kuma，为每个有 deployPort 的项目建 HTTP 拨测。
- multigent 内实现 alert-bridge v1：收 Kuma webhook → attention 信号 → IM 告警（复用现有 bridge）。
- 部署中心页（一期另案）加"服务状态"卡：从 Kuma API 取 up/down 展示。
- 验收：手动 kill 一个服务 → IM 收到告警 → attention 队列出现可处理信号。

**第 2 期：接口成功率（2-3 天）**
- react_spring_boot 模板 vNext：Spring Actuator + Micrometer 暴露 /metrics（模板版本 bump，存量项目不自动获得——迁移策略见 §6 Q3）。
- Prometheus 抓取（服务发现暂用静态配置或 multigent 生成配置文件）。
- 告警规则：窗口成功率 < 95% 触发 webhook → alert-bridge → 同第 1 期通道。
- 部署中心页加成功率卡（PromQL 查询 multigent 后端代理渲染，不引 Grafana）。
- 验收：压测制造失败请求 → 成功率跌破阈值 → 告警链路全通。

**第 3 期：前端报错（2 天）**
- 部署 GlitchTip；react_go_fullstack 模板前端引入 Sentry JS SDK（DSN 指向自建 GlitchTip）。
- 部署中心页加"最近报错"列表（GlitchTip API）。
- 验收：前端埋一个测试异常 → GlitchTip 收到 → 页面可见。

**第 4 期：agent 排障闭环（1-2 天，差异化冲刺）**
- alert-bridge 把告警升级为 multigent 任务：附上下文（哪个服务/哪个端口/最近变更=最近合并的 commit）。
- 验收：告警 → agent 自动领取 → 拉 CI 日志/GitLab 流水线 → 产出初判报告到任务线程。

## 6. 风险与开放问题（请评审重点攻击）

- **Q1（最高风险）VM 容量**：GitLab+Mattermost+runner 已重载，再叠 3 个监控件可能击穿内存——此前已发生过 OOM 导致 dockerd 僵死事故。第 0 期必须先回答；若容量不足，方案是监控件迁独立 VM 或砍 GlitchTip 换轻量自建错误上报。
- **Q2 安全边界**：alert-bridge 的 webhook 入口若挂在 multigent 主服务上，受 publicMux 无认证红线约束（AGENTS.md §5）——必须带 webhook 签名 token 或独立监听+token；Kuma/Prometheus/GlitchTip 自身只绑内网。请评审核对本条是否完整。
- **Q3 存量项目迁移**：模板 vNext 只惠及新项目；ias-auth-center 等存量项目要不要手动升级 SDK？建议：存量项目不做自动迁移，部署中心页对未接入项目诚实显示"未接入监控"。
- **Q4 多线程部署覆盖**：本 VM 有多线程共用、互相覆盖部署的前科（2026-09-29 资料功能被覆盖事故），监控件的部署须走脚本化+版本核对，避免重演。
- **Q5 pull 拓扑**：业务应用在部署主机、监控在 multigent VM（若同机），Prometheus pull 跨机器可达性依赖内网互通；若部署主机无固定地址则 push 模型（Remote Write 或 Kuma push 探针）更稳。请评审裁决 pull vs push。
- **Q6 闭源/许可洁癖**：Netdata UI 闭源已剔除；GlitchTip LICENSE 与 Keep LICENSE 需在引入前核实（文档未明示具体条款）。
- **Q7 重复建设自查**：multigent 已有 agent_runs 遥测（workspace 库）——alert-bridge 与它职责边界要分清（平台自身运行指标 vs 被部署业务应用指标），不要混一锅。

## 7. 总体工作量与顺序

第 0-4 期串行约 6-9 个工作日；与"部署中心页一期"（另案，1 天级）共享部署窗口，建议部署中心页先行合入，监控件逐期挂上来。
