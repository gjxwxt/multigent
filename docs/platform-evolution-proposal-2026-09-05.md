# 平台改造方案：从 IAS Auth Center 批 0 试点沉淀（2026-09-05）

> 状态：经用户批准的优先级归档（P0/P1/P2）。来源：批 0 全链路试点复盘（工作流 wfr-4eagwoy4 → wfr_ias…）+ 红队/均衡双 Agent 分析 + 用户裁决。
> 定性结论：**"产得好、收不了尾"** —— 质量闸门真实有效（4 个人工门拦截了 Agent 自审未发现的缺陷），但环境/发布链断点使"交付完成"无法自证。

## 1. 试点暴露的问题清单（按根因归类）

| # | 问题 | 根因 | 现场后果 |
|---|------|------|----------|
| 1 | CI 调度死锁 | 项目创建时未走初始化流水线，模板 `.gitlab-ci.yml`（含 tags:[docker]）在技术栈替换时被静默丢弃 | pipeline 全部 stuck；rc1 成"断头 tag" |
| 2 | backend job 拉不到 Maven Central | runner 容器 egress 受限（TLS handshake 中断），无镜像配置 | pipeline 983/984/985 backend 连续红 |
| 3 | 推送凭据不可用 | 平台 PushBranch 走宿主侧 git push，沙箱 credential-helper 路径未重映射（HANDOFF §6-2 已知缺陷） | Agent 两次"自救"（symlink .gitconfig + GIT_CONFIG_GLOBAL） |
| 4 | Runner 未绑定项目 | 新项目无自动绑定 Runner 机制 | 需 gitlab-rails 手工 `Ci::RunnerProject` 绑定 |
| 5 | 发布状态无机器可验证定义 | `prepared` 状态即可打 tag，tag 与 CI/主干无一致性约束 | rc1 tag 指向从未合入 main 的 commit |
| 6 | 审查摘要靠人肉 | 每个人工门的上下文（diff 摘要、风险点）需指挥者手工整理 | 门效率低、遗漏风险（tags 问题已记录但未拦截 = "记录了≠处理了"） |
| 7 | 大需求批次数值超限 | 批 0 单 run 上下文过载（4KB 文本门、handoff 压缩告警） | 需母子工作流/批次拆分 |
| 8 | E2E 无家可归 | 平台无部署/冒烟状态机，E2E 在宿主手工搭 compose | 证据不入状态机，无法作为发布前置 |

## 2. P0（下一迭代必须）

### P0-1 发布状态机
`prepared → published → ci_verified → deployed → smoke_verified`，逐级推进、只前进不回跳（回跳需人工门）：
- `published`：tag 必须指向已合入 main 的 commit（机器校验 `git merge-base --is-ancestor`），杜绝断头 tag。
- `ci_verified`：tag pipeline backend/frontend 双绿（deploy 允许因环境限制失败但须如实记录）。
- `deployed` / `smoke_verified`：接 deploy_port（28000+）与 E2E 证据落库。
- 与六大生命周期解耦原则一致：发布状态是任务生命周期的第7个独立维度，不混用 task.Status。

### P0-2 系统级凭据代理注入
修复沙箱 → 宿主推送链路：credential-helper 路径重映射或平台代理 push 端点（Agent 侧 `mga git push` → 平台代为执行，凭据永不出宿主）。消除 Agent 自救先例（那本身就是安全味道）。

### P0-3 新项目自动绑定 Runner
项目创建/初始化流水线中，若远端为 GitLab，自动 `Ci::RunnerProject` 绑定默认 runner（按标签匹配，勿参数化模板 yml 的 `tags: [docker]` —— GitLab tags: 不支持变量展开，见 AGENTS.md §6）。

### P0-4 implementation/code_review 后强制 ci_ready 门
在 unified-delivery-pipeline 的 implementation（或 code_review）之后硬性挂载 `mga ci ready` 检查门（契约驱动，读 `.multigent/runtime.json` healthPath），使"代码写完"≠"交付完成"。引擎无系统步骤类型时借道 agent_task（先例：init v2 的 ci_ready 步骤）。

## 3. P1

- **Spring Boot + Vue 多模板解耦**：`templateEntries()` 目前硬编码 `ReactGoFullstackID`（internal/projecttemplate/template.go:43）；拆为模板注册表，初始化流水线可选技术栈（Spring Boot 模板需含：aliyun maven 镜像 settings、.gitlab-ci.yml 全 job tags:[docker]、deploy/compose.yml、`.multigent/runtime.json` 契约）。平台内置模板只增不删（红线）。
- **制品单次构建复用**：CI 构建 → 制品入库（GitLab artifacts 或宿主卷），E2E/deploy 直接挂载制品（本次宿主 E2E 用的 prebuilt-artifact bind-mount 模式应产品化），消除"每阶段重复构建"与"离线环境重复拉依赖"。
- **审查摘要自动生成**：每个人工门的进入条件自动汇总 diff 统计、测试结果、CI 状态、与基线 SHA 的偏离，减少"记录了但没人看"。

## 4. P2

- **母子工作流 / 批次拆分**：大需求按批 0-4 拆分，母工作流管里程碑，子工作流管批次交付，各自独立 baseCommit 链。
- **4KB 文本硬门禁**：prompt/summary 超 4KB 强制拆分或落附件；基线文档（tech-spec/PRD）自动入仓（docs/ 目录随首次 commit 入库），不留在对话里。

## 5. 已在现场闭环的项（无需开发）

- Runner 1 → Project 44 绑定：已通过 gitlab-rails `Ci::RunnerProject.find_or_create_by!` 手工完成，验证 P0-3 的必要性。
- tags:[docker] 补齐：agent commit 3d3f108，调度死锁解除（frontend job 可跑通）。
- Maven aliyun 镜像：作为 CI 收官任务 t-20260905-5cszgl 派发（backend job 双绿为验收判据）。
- MR #1（feat/ias-auth-center-impl → main）已合并，main = 870bd18，待用户主干合入审计。

## 6. 验收方式

P0 各项的验收 = 下一轮新项目试点（建议 Spring Boot 小需求）全程无手工介入：初始化流水线产出可用 CI 基线、Runner 自动绑定、推送零自救、发布状态机逐步推进到 ci_verified。
