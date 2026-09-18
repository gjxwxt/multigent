# 测试数据沙盒与黄金基准管理方案 (Test Data Sandbox & Fixture Management Plan)

## 1. 状态与前置说明

**状态：已设计，待评审实施。**

本方案旨在解决 Multigent 研发协作平台中“新特性分支与新项目数据真空”问题。经多轮架构评审（吸纳 Staff/Principal 级反馈），本方案坚决摒弃“全量共享活数据库”与“通用 MySQL/Postgres 秒级任意克隆”等不可靠假设，聚焦于 **“确定性契约、文件型秒级隔离、生命周期销毁重建、不可变资产发布”** 的工业级落地路径。

---

## 2. 目标与非目标 (Goals & Non-Goals)

### 目标 (Goals)
1. **解决数据真空**：确保在应用构建后、QA 自动化测试（Playwright / QA Agent）及人工预览（Preview）环节，应用具备结构完整、展示生动、覆盖边界用例的测试数据。
2. **状态确定性方程式**：同一 `baseCommit + fixtureVersion + scenario` 必须在任意机器、任意时间点确定性生成完全相同的数据状态。
3. **物理级隔离（Zero Collision）**：不同任务（Task）的沙箱数据库完全独立，并发执行时互不见数据，任何 DDL 变更或脏写绝对不互相干扰。
4. **零污染生产交付（Zero Leakage）**：
   - Git 仓库**只收录文本型 Manifest 契约与确定性生成脚本**，绝不将大型二进制 Dump 提交到代码版本历史；
   - 生产环境部署流水线（GitLab CI）严格基于环境变量注入生产库，**绝不调用任何测试 Fixture 脚本**。
5. **极速抗脆弱重置**：数据重置采用“销毁临时实例 + 从干净模板快速重拷”，**不依赖任何脆弱的下行迁移（Down Migration / Rollback）**。

### 非目标 (Non-Goals)
- **V1 不承诺外部通用 MySQL/Postgres 的秒级无成本克隆**（避免引入对底层 ZFS/Btrfs CoW 卷或云 RDS Aurora Clone 的强依赖）；
- **不让 Agent 凭空任意写 SQL 污染项目基准**：Agent 造数只生成受限 DSL/JSON 或针对当前沙箱的局部补数，沉淀为黄金基准必须走受控发布门；
- **不在 Phase 1 提前堆砌复杂 UI**：优先打通底层 Contract、租约隔离、重置与回收机制，验收通过后再做预览界面的前端悬浮抽屉。

---

## 3. 核心架构设计与生命周期

### 3.1 三层架构模型

```text
┌─────────────────────────────────────────────────────────────┐
│ 1. Git 契约层：.multigent/fixtures.json (轻量文本清单)        │
│    - 记录 fixtureVersion、schemaFingerprint、生成脚本        │
└──────────────────────────────┬──────────────────────────────┘
                               │ 校验指纹并挂载
                               ▼
┌─────────────────────────────────────────────────────────────┐
│ 2. 任务沙箱层：Task Private DB (运行时物理隔离)              │
│    - 继承项目缓存的只读 template.db                           │
│    - 秒级复制为任务专享的可写 private.db                      │
│    - 执行当前分支 Migration -> 注入 Scenario 补数            │
│    - 重置：直接销毁 private.db 并重新复制 (无需回滚)         │
│    - 销毁：租期到期由 Preview Reaper 统一物理抹除            │
└──────────────────────────────┬──────────────────────────────┘
                               │ 任务完成：丢弃沙箱库，仅收编源码
                               ▼
┌─────────────────────────────────────────────────────────────┐
│ 3. 生产交付层：GitLab CI 部署管道 (环境分离)                 │
│    - 注入真实生产 DATABASE_URL                               │
│    - 仅执行 DDL Migrations，纯净启动                         │
└─────────────────────────────────────────────────────────────┘
```

---

## 4. 规范与契约设计 (Specification & Manifest)

在项目根目录下通过 `.multigent/fixtures.json` 声明测试数据契约（与 `.multigent/runtime.json` 并列）：

```json
{
  "version": 1,
  "engine": "sqlite",
  "storage": "server/data/app.db",
  "defaultFixtureVersion": "1.0.0",
  "schemaFingerprintPaths": [
    "server/migrations/*.sql",
    "server/schema.prisma"
  ],
  "generator": {
    "command": "npm run db:seed:baseline",
    "timeoutSeconds": 60
  },
  "scenarios": {
    "default": {
      "description": "基础业务演示包 (5个客户, 20条各状态订单)",
      "command": "npm run db:seed:scenario -- --name default"
    },
    "edge_cases": {
      "description": "极限边界包 (超长文本, 1000条分页, 特殊字符)",
      "command": "npm run db:seed:scenario -- --name edge_cases"
    }
  }
}
```

### 字段说明：
- `engine`：V1 强制为 `sqlite`（或支持文件型的 DuckDB/嵌入式 DB）；
- `storage`：应用在 Worktree 目录内访问该数据库的相对路径；
- `schemaFingerprintPaths`：用于计算当前表结构指纹的路径匹配，保证表结构变更时能够精准识别基准兼容性；
- `scenarios`：声明可供测试节点与评审人员选择的预设场景命令。

---

## 5. 任务级运行流水线与生命周期状态机

### 5.1 完整执行流向

```text
1. 锁定基线 (baseCommit)
       │
       ▼
2. 选定 fixtureVersion ──> 校验 schema 指纹是否兼容
       │
       ▼
3. Worktree 沙箱就绪 ──> 拷贝只读模板为 task-private.db
       │
       ▼
4. 执行当前分支增量 Migration (保证新增表/字段就绪)
       │
       ▼
5. 针对性补数 (Apply Scenario / Delta Seeding)
       │
       ▼
6. 自动化 QA 测试 (Playwright / QA Agent 验证)
       │
       ▼
7. 人工审核与即时预览 (Preview Copilot 可查可微调)
       │
       ▼
8. 任务结束 ──> Preview Reaper 物理抹除 private.db (或随 Worktree 清理)
```

### 5.2 抗脆弱重置（Fast Reset）
当测试过程需要重新开始时：
1. 切断与当前 `task-private.db` 的连接；
2. 物理删除 `task-private.db`；
3. 从只读母版重新复制一份干净文件；
4. 重新执行当前分支迁移与指定 Scenario。
*全过程在毫秒级内完成，绝不调用不可靠的 `migrate down`。*

---

## 6. 黄金基准的受控晋级门 (Controlled Promotion Gate)

测试数据**绝不允许 Agent 任意执行写操作后一键覆盖母版**。当某个任务产生了高价值的新业务测试数据需要沉淀时，必须遵循以下晋级流水线：

```text
[发起晋级申请 (Promote Candidate)]
          │
          ▼
┌─────────────────────────────────┐
│ ① 自动化合规扫描 (Automated Gate)│
│   - PII 隐私数据扫描 (正则/敏感词)│
│   - 凭据泄漏扫描 (Token/Secret) │
│   - 行数与体积硬封顶 (< 10MB)   │
└─────────────────┬───────────────┘
                  │ 通过
                  ▼
┌─────────────────────────────────┐
│ ② 结构兼容性校验 (Schema Check) │
│   - 与 main 分支 Migration 校验 │
│   - 外键完整性与有效性检查      │
└─────────────────┬───────────────┘
                  │ 通过
                  ▼
┌─────────────────────────────────┐
│ ③ 人工审核与发布 (Admin Signoff)│
│   - Admin 确认语义合理性        │
│   - 发布为新版本 (如 v1.1.0)     │
│   - 历史版本不可变保持          │
└─────────────────────────────────┘
```

---

## 7. 分期实施路线图 (Phased Roadmap)

### Phase 1: 核心契约与引擎闭环 (MVP)
- [ ] 制定 `.multigent/fixtures.json` 契约加载与校验器；
- [ ] 在 `internal/preview/engine.go` 与 `internal/gitworktree/` 注入文件型数据库的只读模板拷贝与 `task-private.db` 挂载机制；
- [ ] 结合 Preview Engine 既有的 30 分钟租期机制与后台 Reaper，实现过期沙箱数据库物理清理；
- [ ] 自动化测试节点接入：在 `qa` 步骤前自动执行模板拷贝与 Scenario 装载；
- [ ] 实现毫秒级“销毁重建式”重置逻辑；
- [ ] CI/CD 交付安全校验：确保 `unified-delivery-pipeline` 生产打包完全排除测试数据。

### Phase 2: 预览交互与受控沉淀 (UX & Promotion)
- [ ] 预览悬浮抽屉：在 Web 预览工具栏增加「🎭 数据沙盒」面板，支持切换预设 Scenario 与一键重置；
- [ ] 预览 Copilot 结构化造数：限制 Agent 只能通过声明式 DSL/JSON 产生局部增量数据，并记入审计日志；
- [ ] 候选数据晋级工作流：实现 PII 扫描与 Admin 审核发布新 `fixtureVersion` 的控制面。

### Phase 3: 复杂数据源演进 (Enterprise Extension)
- [ ] 探索基于轻量 Docker tmpfs 或本地存储卷快照的 MySQL / PostgreSQL 隔离方案；
- [ ] 支持基于生产/预发只读环境的结构化脱敏数据导入导出。

---

## 8. MVP 验收标准 (Acceptance Criteria)

1. **确定性重现**：同一 `baseCommit + fixtureVersion + scenario` 连续执行 5 次，生成的数据库文件哈希或行级数据完全一致；
2. **并发无干扰**：同时启动两个任务沙箱，任务 A 恶意修改/清空所有记录，任务 B 的查询与测试结果完全不受影响；
3. **安全重置**：执行重置操作后，数据库完全恢复至初始模板状态，耗时不超过 100ms；
4. **生命周期闭环**：任务完成或预览租期到期后，`task-private.db` 被后台 Reaper 物理删除，无磁盘垃圾泄漏；
5. **生产绝对隔离**：模拟执行完整 GitLab CI 交付流程，构建产物内无任何测试数据文件，生产初始化脚本正常跑通。
