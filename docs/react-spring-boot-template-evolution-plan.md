# react_spring_boot 模板完善方案（v2 草案）

依据：batch-test2 项目（模板初始态 main 分支 + fu91es 交付分支）全仓考察，2026-10-08。
考察对象：`multigent` 内置模板 `internal/projecttemplate/files/react_spring_boot/`（1.1.0），
对照 batch-test2 workspace `main`（模板初始态）与 `feature/t-20261004-fu91es`（真实交付态，215 文件）。

---

## 0. 现状判定：模板"单薄"在哪里

模板初始态能跑通，但与真实 AI 团队交付的项目相比，缺的不是"更多脚手架代码"，而是四类东西：

| 维度 | 模板初始态 | fu91es 交付态（AI 真实长出来的样子） | 差距定性 |
|---|---|---|---|
| 前端路由 | 无 react-router，App.tsx 38 行 useState 切页 | 7 个页面仍用 `page === 'x' &&` 手写切换 | **模板欠账**：路由是第一需求，AI 被迫每次手写切页，页面 >5 个时必然失控 |
| 网络层 | api.ts 40 行，handleResponse 模式正确 | 269 行单文件，fetch 全收敛、鉴权头、错误对象统一 | 模板方向对，但**缺分域组织法**（单文件 >300 行后该拆 api/ 目录） |
| 通用 UI | Layout/CreateItemModal/ItemCard 三个样例 | ui.tsx 自研 6 组件 + Icon 自绘 SVG + 230 行全局 CSS | 缺**组件增长规则**：ui.tsx 何时拆、第三方库何时引入 |
| 工程纵深 | 无 ErrorBoundary、无懒加载、无 i18n、无表单校验 | 同样全无（AI 也没长出来） | **双向缺口**：模板和 AI 交付都缺，必须模板先立标杆 |
| 后端持久层 | ConcurrentHashMap in-memory repository | H2(兼容模式)+Flyway+MyBatis-Plus，prod 切 PostgreSQL | 初始态刻意零依赖是对的，但**升级路径没给**（AI 每次重新发明 H2 兼容配置） |
| 文档 | AGENTS.md 78 行（质量高）、architecture/api-spec/tdd-guide 各 ~70 行 | 同文件被 AI 续写放大且基本能对上代码 | 文档质量尚可，但**没有腐化对抗机制**，纯靠 AI 自觉 |
| 协作契约 | 无 | contract/ 目录（wp-0 冻结 DDL/错误码/API 骨架/签名规范） | **模板完全没有**，但这是多 agent 并行交付的关键基础设施，fu91es 自己发明了 |

一句话：模板是"单人演示级"，真实交付是"AI 团队工程级"，中间的组织规则（路由、目录增长、契约冻结、文档防腐）每次都靠 AI 临场发挥——这才是"单薄"的本质。

---

## 1. 完善方案（按优先级三批）

### B1 批：结构性补强（模板直接改，影响所有新项目）

**B1-1 前端路由与页面骨架**
- 引入 `react-router-dom`（唯一新增 runtime 依赖），App.tsx 换成 `<Routes>` 声明式路由 + 一个"路由即清单"的注释块。
- 新增 `web/src/pages/` 下第二个示例页（如 `ItemsPage` 列表 + `ItemDetailPage` 详情，带 `useParams`），示范页面间跳转与 404 兜底。
- `vite.config.ts` 加 `@` alias 指向 `src`，tsconfig 同步 `paths`——AI 交付里相对路径 `../../` 泛滥是可预见的腐化点，从模板掐掉。

**B1-2 api.ts 分域组织与类型同源**
- `services/` 拆为 `http.ts`（fetch 封装：handleResponse/authHeaders/ApiError）+ `api/`（按后端域拆模块，如 `items.ts`、`health.ts`），`api.ts` 保留为聚合出口。
- 在 `http.ts` 顶部写死规则注释："新增 API 先加 types/index.ts 类型 → api/ 域模块方法 → 页面只 import api"。这把 fu91es 里 AI 做对的事（收敛）固化成结构，把没做的事（分域）提前给路。

**B1-3 ErrorBoundary + 懒加载基准线**
- 模板内置 `components/ErrorBoundary.tsx`（class 组件，兜底 UI 显示错误摘要 + 刷新按钮），App 根部挂载。
- 路由级 `React.lazy + Suspense` 直接写进示例（第二个页面 lazy 化），让"页面多了自然拆 chunk"成为 AI 看得见的既成事实——对应 multigent 自身 2.3MB bundle 的教训。

**B1-4 后端持久层升级路径（双 profile 基线）**
- 保持 in-memory repository 为默认（零依赖、CI 快），但把 fu91es 验证过的 H2 兼容配置抄进 `application-db.yml`（可选 profile），并在 `docs/architecture.md` 写清"何时切 DB、怎么切"的三步路径（加依赖 → 建 V1 迁移 → 换 repository 实现）。
- 让 AI 从 in-memory 升级到 DB 时照着走，而不是每次重新摸索 H2 MODE=PostgreSQL 配置。

### B2 批：协作契约与文档防腐（本方案的核心增量）

**B2-1 contract/ 契约目录内置**
fu91es 的 wp-0 实践（冻结 DDL、错误码目录、API 骨架、共享规范）直接模板化：
```
contract/
├── README.md            # 冻结基线说明 + 变更控制规则
├── api-skeleton.md      # 初始 API 面（health/items）
├── error-codes.md       # 错误码目录（与 GlobalExceptionHandler 同源）
└── ddl.md               # 仅声明"启用 DB 后在此冻结表结构"
```
README 写死变更控制：契约文件改动必须在 MR 标题带 `contract:` 前缀，且 api-spec.md 与 controller 必须同 MR 同步——这是 fu91es 实际跑通的多 agent 并行前提。

**B2-2 AGENTS.md 增加"文档即交付物"章节（AI 时代文档防腐）**
现状问题：模板文档是静态的，AI 交付后文档必然与代码漂移（fu91es 靠 AI 自觉续写，长期必腐）。模板内置三件事：

1. **文档所有权声明**：AGENTS.md 里给每个文档标 Owner Zone——
   - `docs/api-spec.md` ← 改 controller 必须同 MR 更新（机器可查：路径清单）；
   - `docs/architecture.md` 只描述"结构性事实"（分层、目录、数据流），**禁止**写具体类名清单（fu91es 的架构文档把 HealthController 写进拓扑图，改一次名就错一次）；
   - `README.md` 只写"如何跑起来"，业务功能描述归 api-spec。
2. **Handoff 搭建规则**（用户点名要纳入的）：模板内置 `docs/handoff/` 目录与 `docs/handoff/TEMPLATE.md`，规定：
   - 何时写：每个任务（workflow step）完成时、会话结束前；
   - 写什么：任务状态（done/in-progress/blocked）、验证证据（跑了什么测试什么结果）、未完成清单按优先级、风险与坑；
   - 何时不写：不复制代码、不写部署凭据、不预测未来（handoff 是快照不是计划书）；
   - 腐化规则：handoff 文件按任务归档（`t-<id>.md`），任务完结后**不再修改**——历史 handoff 永远不更新，新状态写新文件，杜绝"一份 handoff 改到天荒地老"的腐化模式。
3. **文档漂移自检**：Makefile 加 `make docs-check` 目标——脚本核对 api-spec.md 中列出的 API 路径与 `@RequestMapping` 注解的实际路径集合一致（不一致即 fail）。把"文档是否腐化"变成 CI 可判定的确定性校验，这是对抗 AI 时代文档高腐化率的唯一可持续手段（multigent 自身 ci_ready 闸门的同款思路：确定性优先）。

**B2-3 CLAUDE.md/AGENTS.md 分工明晰**
现模板两文件内容重叠 70%。改为：CLAUDE.md 只保留"命令速查 + 本文件与 AGENTS.md 冲突时以 AGENTS.md 为准"，所有规范收口 AGENTS.md——单一事实源，防两份文件各自漂移。

### B3 批：组件体系（观察项，暂不内置）

- 不内置 antd/组件库：模板当前零 UI 依赖是正确决策（batch-test2 交付态也证明 AI 能自研轻量组件），引入组件库是产品级决策应留给需求。
- 但在 AGENTS.md 加一条**组件库引入判据**："页面 ≥8 个或出现第二处重复交互模式时，评估引入组件库；引入时必须配 ErrorBoundary 与 bundle 体积核对（npm run build 后主 chunk >800kB 需拆分）"。
- ui.tsx 增长规则：通用 UI 组件超过 6 个时拆 `components/ui/` 目录（一个组件一文件），禁止继续堆单文件。

---

## 2. 明确不做

- 不内置 react-query/zod/i18next：模板示范页只有一个资源，引入即过度设计；判据写进 AGENTS.md（列表页出现服务端缓存失效需求时引 react-query；多语言需求出现时引 i18next）由 AI 按需升级。
- 不预置 CI 之外的部署编排：deploy/ 保持 Dockerfile+compose 现状。
- 不做 IDE 配置、lint 规则定制：保持模板薄。

## 3. 实施与验收

1. B1 三项改模板源码（`internal/projecttemplate/files/react_spring_boot/`），跑 `make test`（模板 seed 测试）+ 本地实例化一个试点项目验证 `make verify` 全绿。
2. B2 的 docs-check 脚本放模板 `scripts/docs-check.mjs`，Makefile 接线，试点项目实弹跑一次（故意改错一个路径看是否 fail）。
3. 模板版本号 1.1.0 → 1.2.0，AGENTS.md 记录变更。
4. handoff TEMPLATE.md 随首个真实项目验证一轮后修订。

## 4. 风险

- B1-1 引入 react-router 是唯一 runtime 依赖增加，若下游项目确属单页可 `npm uninstall`，AGENTS.md 注明。
- B2-2 文档所有权规则依赖 AI 遵守，docs-check 只能覆盖 api-spec 一个面；其余文档腐化靠 handoff 归档制止损（历史不可改=错误不会累积传播）。
- contract/ 目录对小项目（单任务）是仪式负担，README 注明"单任务项目可跳过，直接在 AGENTS.md 记录决策"。
