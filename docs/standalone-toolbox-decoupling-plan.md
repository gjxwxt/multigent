# Multigent 能力解耦方案：独立工具箱（Offline-Fallback / 可组装架构）

> 状态：提案 v1（2026-09-18，代码事实核实基线 dev @ a3203200）。
> 出发点：平台中断/升级期间，成员从 GitLab 拉代码后仍能自行部署、预览、造数、调试。
> 本文只写架构与分期，不含任何部署地址、凭据与内部拓扑。

---

## 1. 先修正一个前提：部署链路今天已大半不依赖平台

核实代码后的事实（区别于直觉）：

1. **部署的真正执行者是 GitLab CI + runner**，不是 Multigent 服务。平台在部署链路里只做两件 best-effort 副作用：把分配的 `APP_PORT` 推成 GitLab CI 变量（`internal/api/deploy_port.go:96`，推送失败只降级不阻断），以及给项目绑定默认 runner（`bindDefaultRunner`，同样 best-effort）。发布 job 由仓库内 `.gitlab-ci.yml`（tag 触发）驱动。
2. 也就是说，**平台挂掉时，有 GitLab 权限的成员 push 一个 tag 就能走完发布**——前提是该项目的 CI 基线已由 `mga ci ready` 种好且 runner 已绑定（这两件事都持久化在 GitLab 侧，不在平台 DB 里）。
3. 平台真正"卡脖子"的依赖是三样：
   - **凭据**：GitLab connection token 加密存在 controlDB（secretbox），离线拿不到——但这是安全边界，不是缺陷（见 §6 红线）；
   - **状态双源**：`APP_PORT`、runner 绑定等平台 DB 有记录、GitLab 侧也有一份，没有仓库内的权威快照；
   - **能力没有离线入口**：ci-ready 的确定性校验内核是纯函数（`internal/ciready/ciready.go` 的 `Ensure`/`Verify` 只吃 repoDir），但 CLI `mga ci ready` 却绕道服务端 HTTP 端点（`cmd/mga/main.go:114`）——平台一挂，本地明明能跑的检查也跑不了。

**结论**：目标不是"把部署从平台拆出去"（它本来就在外面），而是**把每项能力做成双模（dual-mode）：平台在线时走平台编排，平台离线时有独立 CLI 直连同一内核**。

---

## 2. 目标架构：双模内核（一个 library core，两个壳）

```text
   ┌─────────────────────┐          ┌─────────────────────┐
   │  multigent server    │          │  mgt（standalone     │
   │  （控制面壳：HTTP API、│          │   toolbox，离线壳：   │
   │   工作流、RBAC、审计） │          │   纯 CLI，零服务端）  │
   └─────────┬───────────┘          └──────────┬──────────┘
             │        调用同一批 Go 内核包        │
   ┌─────────┴──────────────────────────────────┴─────────┐
   │  ciready（纯函数）  codehost/gitlab（API 封装）          │
   │  preview/engine     gitworktree     fixtures（规划中）   │
   │  deploy（新：tag/pipeline 编排，纯库）                    │
   └───────────────────────────────────────────────────────┘
```

硬性架构规则（可加 CI 检查）：

- **内核包禁止 import `internal/api`** 与任何 server 态；server 只是编排壳。现状已大体满足（ciready、codehost、preview、gitworktree 都是独立包），欠的是把 mga 的 HTTP 绕行拆掉。
- **仓库即契约（GitOps 化）**：离线工具需要的一切输入都从仓库文件读取（`.gitlab-ci.yml`、`.multigent/runtime.json`、migrations、新增的 `.multigent/deploy.json`），不读平台 DB。
- **平台与 CLI 行为一致性用 conformance test 钉死**：同一内核对同一仓库输入，两种壳产出必须一致，防"双模漂移"。

这个形态同时是《现状 → 分布式改造》文档的垫脚石：内核包将来要拆成独立服务/worker 时，边界已经画好。

---

## 3. 能力清单与解耦评级（按代码事实）

| 能力 | 现状 | 离线可行性 | 缺口 |
|---|---|---|---|
| **发布/部署** | GitLab CI 执行；平台仅 best-effort 推 APP_PORT、绑 runner | ✅ 已基本可行 | 离线侧用自己的 GitLab 凭据触发/轮询 pipeline；APP_PORT 记录进仓库 |
| **CI ready 校验** | 内核纯函数，但 CLI 绕道服务端端点 | ✅ 一天可通 | `--local` 模式直调 ciready，不经过 HTTP |
| **本地预览** | preview engine 独立包，但生命周期由 API/租期驱动 | ⚠️ 部分 | `mgt preview up/down` 直接读 `.multigent/runtime.json` 起容器 |
| **测试数据/造数** | fixture sandbox 方案已按仓库契约设计（`.multigent/fixtures.json`） | ✅ 设计即离线优先 | 按该方案落地即可，无额外解耦工作 |
| **工作流/QA 状态** | 状态机在 controlDB | ⚠️ | 平台把里程碑状态导出为仓库内快照（`.multigent/state/`），离线只读 |
| **凭据/连接** | 加密存 controlDB | ❌ 且不导出 | 离线模式使用操作者本人的 PAT / glab 认证——安全边界，见 §6 |

---

## 4. 独立 CLI 形态：`mgt`（multigent toolbox）

单二进制（与 dist/multigent、dist/mga 同仓构建），命令草图：

```bash
mgt status                       # 读仓库 .multigent/ 快照：CI 基线、部署配置、最近里程碑
mgt ci ready [--wait]            # 本地跑 ciready.Ensure+Verify，--wait 时直连 GitLab 轮询 pipeline 证据
mgt deploy --tag vX.Y.Z [--wait] # push tag 或调 pipeline trigger API，轮询到终态并输出 job 证据
mgt preview up|down|status       # 按 runtime.json 契约起/停本地预览容器（复用 preview engine）
mgt fixtures apply <scenario>    # 按 fixtures.json 装载黄金基准/场景数据（依赖 fixture 方案落地）
```

凭据来源优先级：`GITLAB_TOKEN`/`GLAB_TOKEN` 环境变量 → glab CLI 配置 → 交互式提示。**永不读取平台 controlDB。**

`mga`（沙箱内 runtime CLI）保持现状定位不变：它面向"平台在线、Agent 在沙箱内"的场景；`mgt` 面向"平台离线、人在自己机器上"的场景。两者共享内核，不互相依赖。

---

## 5. 分期路线图

- **Phase 0（零代码，先做）**：把"平台不可用时的手工降级 Runbook"写出来——push tag 发布、本地跑确定性检查、本地起预览的命令序列。核实表明这些今天就能手工做到，缺的只是一份 SOP。这份 Runbook 同时是后续 CLI 的验收脚本。
- **Phase 1（小）**：`mga ci ready --local`（或 `mgt ci ready`）直调 ciready；`mgt deploy` 基于 codehost/gitlab 实现触发+轮询+证据输出。
- **Phase 2（中）**：`.multigent/deploy.json` 落地为仓库内权威部署契约（端口、runner tags、发布策略），平台初始化时写入仓库而不是只推 GitLab 变量；消灭 APP_PORT 双源。
- **Phase 3（中）**：`mgt preview`、`mgt fixtures`（后者跟随 fixture sandbox 方案的 Phase 1）。
- **Phase 4（可选）**：平台里程碑状态导出到 `.multigent/state/`，`mgt status` 可回答"这个任务做到哪了"。

---

## 6. 红线与权衡

1. **凭据永不导出**：离线降级复用"操作者本人的 GitLab 权限"，不是平台凭据库的副本。平台挂了 ≠ 任何人都能发布，权限判定回到 GitLab 自身的 membership——这恰好是正确的 fail-safe。
2. **仓库契约不含环境秘密**：`.multigent/deploy.json` 只存端口号、tag 规则、runner 标签等非密配置（与 AGENTS.md §7 部署信息不入公开仓库的约定一致；敏感部署配置仍走 GitLab CI 变量）。
3. **为什么不做微服务化/MCP 化**：现阶段没有外部用户，为"平台可能挂"支付常驻多服务的运维成本不划算。单二进制 + 双模内核以近乎零运维成本获得同等兜底能力；将来做分布式控制面时，内核包边界直接复用。
4. **主要风险是双模漂移**：平台路径与 CLI 路径行为不一致会比"没有离线模式"更糟（用户以为发布成功）。对策是 conformance test 与"内核唯一实现"规则，两壳都不得私写业务逻辑。
5. **诚实边界**：工作流编排、人工审核、RBAC、审计这些"协作面"能力离线时就是没有——工具箱兜底的是"单人也能把代码发出去"，不是"平台功能离线等价"。方案不承诺后者。
