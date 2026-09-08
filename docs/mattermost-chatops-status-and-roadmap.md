# Multigent × Mattermost ChatOps 架构现状与未来演进评估报告

- **报告日期**: 2026-09-08
- **责任架构师**: Antigravity Core Team
- **文档状态**: 评审决策版
- **关联代码分支**: `feat/task-branch-preview-incontext-pr` (`e3d4a86`, `ecb7ffd`)
- **核心执行依据**: `docs/mattermost-integration-plan-2026-09-04.md` (V2.2 定稿)

---

## 1. 核心定位与评估摘要 (Executive Summary)

本报告旨在为团队提供一份**客观、严谨、以源码与机器实测为唯一依据**的 Mattermost ChatOps 架构评估。

在评估系统前，必须首先在认知上划清一条**技术红线**：
> **“当前现状（P1b + Phase 2）” 与 “未来演进（P4 终态）” 绝不能混为一谈。**

- **当前现状（已落地、已上线、测试 100% 通过）**：
  采用 **1:1 独立 Bot 映射** + **严格持据绑定（PoP，Proof of Possession）** + **项目频道单任务 Thread 展开模型** + **交互式卡片与 Dual CAS 审批**。其核心特征是**“安全与权限硬隔离优先”**，杜绝跨会话串扰和冒名提权。
- **未来演进（规划中，P4 阶段目标）**：
  采用 **统一单入口 Bot（@multigent Dispatcher）** + **单根路由命令（`/agent <name>`）** + **D6 全局出站身份兜底**。其核心特征是**“统一交互体验优先”**，降低多 Agent 场景下的 Bot 运维与用户心智成本。

---

## 2. 现状架构深度剖析 (Current Architecture: P1b + Phase 2)

```mermaid
graph TB
    subgraph Mattermost 企业内网实例 (现状: 独立多 Bot)
        Team["Team: 研发团队"]
        BotMira["独立 Bot: @bot-mira (专属 Mira)"]
        BotLina["独立 Bot: @bot-lina (专属 Lina)"]
        ProjChannel["项目频道: #town-square"]
        UserAlex["新人账号: alex (已加入团队)"]
        
        Team --> ProjChannel
        BotMira -.已加入频道.-> ProjChannel
        BotLina -.已加入频道.-> ProjChannel
    end

    subgraph Multigent 协作平台 (现状: 1:1 绑定 + PoP 持据防伪)
        Workspace["工作区 (Workspace)"]
        Proj["项目: 1test"]
        AgentMira["Agent: Mira (代码编写)"]
        AgentLina["Agent: Lina (代码初审)"]
        
        BindMira["Mira 专属渠道绑定 (conn-mira)"]
        BindLina["Lina 专属渠道绑定 (conn-lina)"]
        
        UserRecord["平台用户: alex (项目 Manager)"]
        
        Workspace --> Proj
        Proj --> AgentMira
        Proj --> AgentLina
        AgentMira --> BindMira
        AgentLina --> BindLina
    end

    %% 物理连接
    BindMira <==独立 Token==> BotMira
    BindLina <==独立 Token==> BotLina
    
    %% 严格 PoP 绑定约束
    UserRecord -.1. 生成 10 分钟一次性绑定码 MG-xxx.-> UserAlex
    UserAlex ==2. 在 MM 必须发送 /bind MG-xxx (持有证明 PoP)==> ProjChannel
```

### 2.1 实体拓扑与 1:1 独立 Bot 机制
- **机制现状**：
  每个智能体在物理上对应一个独立的 Mattermost Bot 账号（例如当前生产环境中的 `bot-mira` 和 `bot-lina`）。
- **代码强制约束（硬红线）**：
  位于 `internal/api/agent_channel_handlers.go:1159`：
  ```go
  if strings.TrimSpace(b.ExternalBotID) == strings.TrimSpace(result.ExternalBotID) {
      return controldb.AgentChannelBinding{}, fmt.Errorf(
          "bot %s is already bound to %s/%s in this workspace; each agent must have a dedicated bot account", 
          result.ExternalBotID, b.ProjectID, b.AgentID,
      )
  }
  ```
  **如果管理员尝试复用同一个 Bot Token 绑定多个 Agent，平台 API 会直接返回 HTTP 400 拒绝配置。**
- **架构设计收益**：
  1. **入站路由零歧义**：IM 事件传入时，服务端通过 `bot_id`（`metadata.appID`）毫秒级唯一定位项目和 Agent，根除跨工作区、跨项目串扰；
  2. **权限与审计硬隔离**：每个 Bot 具备独立的 Secretbox 加密密钥，Bot A 的凭据泄露绝不影响 Bot B；
  3. **沙箱并发隔离**：不同 Agent 唤醒独立的 Docker 容器与独立的 Git Worktree，无提示词注入和并发写锁竞争风险。
- **现状代价**：
  随着项目内 Agent 数量增长（如增加架构师、QA、安全测试等），管理员需要去 Mattermost 创建 N 个 Bot 账号并录入 N 组 Token。

---

### 2.2 身份绑定与持据防伪机制 (Proof of Possession, PoP)
- **为什么坚决不做“用户名/邮箱静默自动匹配”？**
  在企业 IM 接入方案三轮红队评审中，曾明确裁定：**严禁在无持有证明的情况下做同名自动映射**。
  - **严重安全漏洞场景**：在允许自行注册或改名的企业 IM 中，攻击者只要将 Mattermost 用户名改成 `admin` 或高管姓名，如果系统搞“静默同名映射”，攻击者无需密码即可直接在 IM 中点击通过关键发布门禁，造成灾难级提权越权！
- **PoP 持据绑定工作原理**：
  1. 用户首先在 Multigent 网页控制台完成安全登录；
  2. 点击生成一个有效时间仅 10 分钟的一次性持据绑定码（例如 `MG-A1B2-C3D4`）；
  3. 用户复制该码，亲自回到 Mattermost 客户端输入 `/bind MG-A1B2-C3D4`；
  4. 系统捕获 IM 消息发送者的不可伪造的真实 `user_id`，完成两端账号的加密绑定。
  - *结论*：这一机制做到了**“零特权依赖 + 密码学证明”**，是平台不可退让的安全底线。
- **出站通知边界（D6 现状）**：
  当前数据表 `user_channel_identities` 记录在会话级。如果员工 `alex` 只在 `Mira` 的会话里绑过码，当工作流需要 `Lina` 向他发送出站私信（`mga notify --to alex`）时，由于 D6 全局出站回退机制尚未开发，系统在 Lina 的绑定表中查不到路由，出站通知会投递失败。

---

### 2.3 Task Thread 展开与 Phase 2 交互式审批闭环
- **单任务单线程（Task Thread）折叠模型**：
  - 项目配置 `default_im_channel`（如 `#town-square`）；
  - 任务启动时，系统自动在频道投递一条火箭图标的任务主帖（Root Post）；
  - 任务流转过程中，所有执行进展（Step Done）全部作为 Thread 回帖折叠收拢在右侧侧边栏，主频道保持清爽不刷屏。
- **双重防漂移（Dual CAS）**：
  - 审批卡片包含 `ExpectedStateVersion`（状态版本）与 `ReviewSnapshotHash`（SHA-256 参数指纹）；
  - 任何人点击卡片时，端点强制执行 CAS 双校验，一旦上游产物被篡改或漂移，直接以 `409 Conflict: Review Stale` 阻断幽灵审批。
- **卡片自动拆除与永久印章**：
  - 审批通过或驳回后，系统调用 Mattermost `PUT /api/v4/posts/{post_id}/patch` 立即拆除卡片底部所有交互按钮，就地加盖绿/红色永久完成印章，杜绝二次重放点击。

---

### 2.4 内网新环境真实上线 SOP（基于现状的落地实操）

如果明天将平台迁移到企业内网并对接一套崭新的 Mattermost，**标准实操流程**如下：

```markdown
1. 【Mattermost 准备 Bot 账号】
   - 登录 Mattermost (管理员账号) -> System Console -> Integrations -> Bot Accounts;
   - 创建 bot-mira (显示名 Mira) -> 生成 Bot Access Token 1;
   - 创建 bot-lina (显示名 Lina) -> 生成 Bot Access Token 2;
   - 将两个 Bot 邀请加入项目协作频道 (如 #town-square 或 #proj-alpha)。

2. 【Mattermost 注册斜杠命令】
   - 在团队设置中创建 Custom Slash Command:
     - Trigger Word: bind
     - Callback URL: http://<multigent-ip>:27892/api/v1/im/mattermost/commands/bind
     - Method: POST

3. 【Multigent 录入渠道连接】
   - 登录 Multigent 控制台 -> 进入项目 1test -> Agents -> Mira -> Channels:
     - 录入 Mattermost 地址、Bot Access Token 1、HMAC 秘钥 -> 点击保存连接;
   - 进入 Agents -> Lina -> Channels:
     - 录入 Mattermost 地址、Bot Access Token 2、HMAC 秘钥 -> 点击保存连接。

4. 【新员工入职协同体验】
   - 新人 alex 在 Multigent 登录控制台，进入项目 Agent 渠道卡片，点击 "生成绑定码" (获得 MG-xxxx);
   - 新人 alex 打开 Mattermost，在群频道或私聊窗口输入 /bind MG-xxxx 发送;
   - 收到绑定成功回执后，日常所有任务通知与审批流全部跑通。
```

---

## 3. 未来演进路线深度剖析 (Future Evolution: P4 Dispatcher & D6)

```mermaid
graph TB
    subgraph P4 终态目标 (规划中 - 演进路线)
        SysBot["企业统一机器人: @multigent (唯一单入口)"]
        Dispatcher["统一消息分发器 (Dispatcher)"]
        SingleBind["全平台统一账号绑定 (/agent bind)"]
        D6Fallback["D6: 全局出站身份兜底回退 (Outbound Identity Fallback)"]
    end
    
    subgraph 现状 (P1b + Phase 2 - 已落地)
        MultiBots["每 Agent 独立 Bot (bot-mira, bot-lina)"]
        DedicatedConn["1:1 独立连接约束 (复用报 400)"]
        StrictPoP["严格持据绑定 (/bind MG-xxxx)"]
        ThreadApprove["Task Thread 折叠展开 + 交互式卡片审批"]
    end

    MultiBots -.演进升级.-> SysBot
    DedicatedConn -.演进升级.-> Dispatcher
    StrictPoP -.体验增强.-> SingleBind
    ThreadApprove -.无缝继承.-> ThreadApprove
```

### 3.1 P4 统一单入口 Bot (Unified Dispatcher)
- **目标场景**：
  消除企业中“Agent 越多、Bot 越多”的运维负担，在 Mattermost 中**只部署一个统一应用 Bot（`@multigent`）**。
- **单根命令路由机制（Single-Root Command）**：
  采用企业级命令空间设计，注册唯一顶级命令 `/agent`（或 `/multigent`）：
  ```bash
  /agent mira 帮我审查当前的 PR #12
  /agent lina 运行自动化回归测试
  ```
  - **优势**：杜绝占用 `/mira`、`/lina` 等顶级词导致与企业第三方插件（如 `/jira`、`/gitlab`）命名冲突。
- **同名消歧策略**：
  当一个用户同时参与多个项目，且项目内都存在名为 `lina` 的智能体时，输入 `/agent lina` 系统会**拒绝盲目执行**，而是通过私信返回候选列表供用户点选（例如提示使用 `/agent proj-alpha/lina` 进行精确定位）。
- **会话上下文锁定（Context Lock）**：
  支持在与统一 Bot 私聊中发送 `use mira` 命令，后续所有对话自动路由给 Mira，上下文状态持久化至 SQLite 数据库，保证会话连续性。

---

### 3.2 D6 全局出站通知降级兜底 (Outbound Identity Fallback)
- **解决的核心痛点**：
  解决“新人只绑过 Agent A，Agent B 无法向其发送私信通知”的问题。
- **设计思路**：
  当 `mga notify --to alex` 触发时，若在当前 Agent 的 `user_channel_identities` 中未找到匹配通道，系统自动回退查询租户级的 `external_identities` 表；若发现该用户曾在同工作区的其他 Agent 绑通过 Mattermost 账号，系统自动为当前 Agent 建立出站通道映射并成功投递。

---

### 3.3 企业级 SSO/LDAP 对接演进（PoP 安全底线下的可信免绑）
- **未来演进方向**：
  如果企业内部全面推行 OIDC / SAML 单点登录（SSO）：
  - Multigent 平台与 Mattermost 均作为企业同一 IdP 的下游服务；
  - 双方用户的 Sub（唯一主体 ID）或可信 Claims 具有密码学签名背书；
  - 此时，系统可以升级支持**“基于企业 IdP 签名的自动可信对齐”**，在不牺牲持有证明的前提下，实现企业新员工免敲 `/bind` 的无缝静默体验。

---

## 4. 现状架构 vs 未来演进全方位对照表 (Comparison Matrix)

| 评估维度 | 当前现状（P1b + Phase 2 - 已上线） | 未来演进（P4 Dispatcher + D6 规划） | 对评估者的决策参考 |
|---|---|---|---|
| **Bot 账号数量** | **N 个（每个 Agent 专属 Bot）** | **1 个（统一系统 Bot `@multigent`）** | 现状在 Agent 较少时简单稳固；未来更适合 Agent 矩阵规模化 |
| **Token 隔离性** | **物理强隔离**（Token A 泄露不影响 Token B） | **逻辑集中**（依赖分发器做审计与权限隔离） | 现状安全性极高，符合企业审计初期红线 |
| **复用同一 Bot** | **代码显式禁止（报 400）** | **原生支持**（底层由 Dispatcher 动态路由） | 现状防止了路由歧义事故，是深思熟虑的防错设计 |
| **身份对齐机制** | **持据防伪码（`/bind MG-xxxx`）** | **单根命令绑定（`/agent bind`）/ IdP 对齐** | 现状杜绝一切同名冒充；未来在保留安全底线下提升便捷度 |
| **出站通知覆盖** | **仅限已发生绑定的会话通道** | **全局出站降级兜底（D6）** | 现状需注意通知通道可达性；未来彻底消除出站盲区 |
| **审批与卡片交互** | **Task Thread 展开 + Dual CAS + 自动拆除盖章** | **完全继承现有 Phase 2 成果** | **这一层是终态设计，未来无需推倒重来** |

---

## 5. 架构演进与技术评审建议 (Recommendations)

1. **内网试点期（当前阶段）：坚持现状架构**
   - 现有的 **1:1 独立 Bot 架构与持据绑定码（PoP）** 虽然在开通时多花了 2 分钟建 Bot，但换来了**最强的权限物理隔离、最清爽的审计归因和零越权漏洞**。
   - 在第一批项目（如 1~3 个核心 Agent）接入内网时，完全足够、运行极稳、且具备扎实的实机测试证据。
2. **对外汇报与写教程口径**：
   - 统一采用**第 2.4 节的“现状实操 SOP”**；
   - 将 P4 统一 Dispatcher 作为**“后续演进路线图（Roadmap）”**向团队展示，既展现了方案的严谨求实，又展现了架构前瞻性。
3. **P4 启动时机建议**：
   - 当单个项目内的智能体数量膨胀到 **5 个以上**，或多项目并行导致 Bot 账号维护成本成为主要矛盾时，正式启动 P4 统一 Dispatcher 与 D6 出站兜底的设计与开发。
