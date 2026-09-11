# 验收测试设计前置方案（Greenfield Delivery Pipeline vNext）

## 1. 状态与前置条件

**状态：已设计，暂不实施。**

本方案排在当前“内网运行时与依赖治理”任务完成、复验并形成可追溯部署证据之后执行。实施前必须确认：

1. 当前工作区中正在进行的运行时、Preview、JDK 镜像相关改动已完成独立验收；
2. `make test`、`make build` 与目标环境验证均通过；
3. 本方案只新增一个版本化的**新项目交付流水线**，不修改运行中的工作流实例。

本方案的目标模板是 `greenfield-delivery-pipeline`，不是 `unified-delivery-pipeline`：前者已有需求审核、设计确认、合并前 QA、风险覆盖矩阵和 QA 打回研发的闭环；后者是兼容既有场景的另一条流水线，当前 QA 位于合并后，不应在本任务中混改。

## 2. 目标与非目标

### 目标

在需求和设计已确认、编码开始前，由独立 QA Agent 产出一份风险驱动的“验收测试规格”，让研发 Agent 的 TDD 有清晰的外部行为目标；后置 QA 再以同一规格为基线，独立验证实现与实际测试结果。

形成可追踪关系：

```text
验收标准（AC）
  -> 验收测试规格中的 Case
  -> 研发实现的自动化测试与证据
  -> QA 实际执行结果 / 风险覆盖矩阵
  -> QA 签核、豁免或返工
```

### 非目标

- 不承诺把每条自然语言 Case 自动、无损地翻译成测试代码；
- 不把 API 多步调用测试称为浏览器 E2E；没有浏览器/真实依赖环境时，它应标注为 API 业务流集成测试；
- 不新增一道人类审批门，避免审批疲劳；
- 不让 QA Agent 修改业务实现代码；
- 不改动已实例化的工作流。内置模板变更后，必须新建 vNext 工作流实例供新任务选择。

## 3. 流水线设计

MVP 采用串行路径，优先保证设计与测试规格一致：

```text
requirement_draft
  -> requirement_review（人工）
  -> design_review（人工设计确认或显式豁免）
  -> acceptance_test_design（QA Agent，新增）
  -> implementation
  -> self_review
  -> code_review（人工）
  -> qa
  -> qa_signoff（人工）
  -> pr_open_and_merge -> release -> go_live_confirm
```

将节点置于 `design_review` 之后而不是 `requirement_review` 之后：需求 AC 是测试规格的主输入，但已确认的页面状态、交互、接口边界同样影响测试设计。这样研发开始编码时能同时拿到需求、设计和测试规格。

未来若工作流引擎具备可靠的 fan-out / join 语义，可将“设计原型生成”和“初版测试策略”并行，再在实现前由 `test_spec_reconcile` 汇合；这不是 MVP 范围。

## 4. 节点契约

### 4.1 `acceptance_test_design` 输入

- `approved_requirement`：已审核需求全文，含 AC、非目标和已知约束；
- 已冻结的设计引用：`approved_design_snapshot_path`、设计豁免信息或等效服务端生成的设计引用；
- 当前项目技术模板/运行时能力，供判定哪些 Case 可自动化；
- 可选的既有契约：OpenAPI、数据库迁移策略、外部依赖说明。

### 4.2 输出

节点必须创建完整的测试规格文档，并输出以下引用：

- `test_spec_doc_id`：完整测试规格的 Doc ID；
- `test_spec_manifest`：小型结构化 JSON，至少包含 `version`、唯一 `case_id`、关联 `ac_id`、风险等级、自动化层级和执行类型；
- `test_spec_summary`：用例数量、风险分布、不可自动化项和环境前提的摘要。

文档本体采用固定 Markdown 模板；JSON manifest 用于工作流校验与后续追踪，避免只传一段不可解析的长文本。

### 4.3 每条 Case 的最小字段

| 字段 | 含义 |
| --- | --- |
| `case_id` | 稳定且唯一的编号，如 `AUTH-001` |
| `ac_id` | 关联的验收标准编号；没有 AC 编号时须标明需求章节 |
| `scenario` | Given / When / Then 或等效的外部行为描述 |
| `risk_level` | `high` / `medium` / `low` |
| `automation_level` | `unit` / `api_integration` / `ui_e2e` / `manual` |
| `execution_type` | `auto` / `manual` / `environment_blocked` |
| `test_data_and_preconditions` | 测试数据、权限、外部依赖和环境前提 |
| `expected_result` | 可观察、可断言的结果，不写实现细节 |
| `reason_if_not_auto` | 非自动化或受环境阻断时必填 |

规则：高风险 Case 必须有可执行验证路径或明确的人工豁免前提；禁止以“待观察”“视情况”充当预期结果；禁止为凑数量生成没有关联 AC 的边缘 Case。

### 4.4 下游消费

| 节点 | 必须消费 | 必须输出 |
| --- | --- | --- |
| `implementation` | 测试规格 Doc、manifest、需求、设计引用 | `test_implementation_evidence`：Case ID -> 测试文件/测试名/执行命令/结果的映射 |
| `self_review` | 测试规格、实现映射、代码变更 | 对高风险 Case、空断言、过度 Mock、未覆盖 Case 的反查结论 |
| `code_review` | 规格摘要、研发映射、自审结论 | 对范围、架构和高风险测试缺口的人工裁决 |
| `qa` | 原始测试规格、研发映射、已批准需求、设计引用、实际 base/head Diff | 独立 `risk_coverage_matrix` 与 `test_report` |
| `qa_signoff` | QA 矩阵、报告、原始规格 | 通过、逐项人工豁免，或打回 |

QA 不应只重跑研发测试。它必须把原始规格、实际 Diff 与研发映射对账，再补充边界、异常、鉴权、幂等性及回归验证。

## 5. QA 返工与测试代码边界

1. `qa_signoff -> implementation` 继续使用现有 `e-qa-rework` 路径；服务端应将风险矩阵中的 failed / blocked / unexecuted 项汇总进 `review_comments`，即使人工未输入额外评论也不能丢失失败上下文。
2. 返工边必须同时保留 `test_spec_doc_id`、`test_spec_manifest`、需求和设计引用，禁止研发在返工轮次失去原始验收基线。
3. QA 可以编写可复现的回归测试，但只能修改测试文件、测试夹具或探测脚本，禁止顺手修业务代码。
4. QA 新增测试必须形成可追溯 checkpoint（提交 SHA 或等效不可变工件）；研发在同一任务分支上修复并使该测试转绿。禁止把仅存在于临时容器中的测试当作验收证据。
5. 沙箱不能验证的外部系统、长时任务或真实设备，必须在矩阵中标成 `environment_blocked` 或 `manual`，由 QA Owner 逐项裁决；严禁静默标绿。

## 6. 质量门禁

### 6.1 确定性校验

在工作流层增加对 `test_spec_manifest` 的最小校验：非空、合法 JSON、Case ID 唯一、每项包含 AC 引用/风险/自动化层级/预期结果。该校验只保证结构，不代替业务判断。

### 6.2 独立性

- TDD Skill 是研发行为规范，不是测试质量证明；
- CI 证明指定命令在指定提交上执行成功，不证明覆盖完整；
- QA 的“独立”必须体现在可获取真实 Diff、原始规格和独立执行证据，而不是只阅读研发自述；
- 高风险 Case 的 `passed` 必须有非空证据；不可测项必须走显式 waiver。

### 6.3 证据分级

| 等级 | 可接受证据 |
| --- | --- |
| 强证据 | 可定位 commit、执行命令及退出码、测试框架报告、CI job/pipeline 链接 |
| 辅助证据 | Agent 运行日志、Doc ID、风险矩阵、测试报告摘要 |
| 不可单独采信 | Agent 自由文本的“全部通过”“已覆盖”结论 |

## 7. 实施批次

### Batch A：模板与契约

- 新建 `greenfield-delivery-pipeline` 的 vNext 实例/版本，不修改存量实例；
- 新增 `acceptance_test_design` 节点、字段和边映射；
- 为研发、初审、QA 节点更新明确的输入/输出 Prompt；
- 增加 manifest 最小结构校验。

### Batch B：追踪与返工

- 增加研发 Case 到实际测试的映射工件；
- 保证 QA 签核打回时，失败项和测试规格引用无损回流；
- 实现 QA 测试 checkpoint 规则与审计展示。

### Batch C：验收与试点

以一个 React + Spring Boot 的真实小需求试点，至少覆盖：

1. 需求 AC -> 测试规格 -> 研发自动化测试 -> QA 矩阵的全链路；
2. 一个高风险异常/鉴权 Case；
3. 一次 QA 新增失败测试并打回研发；
4. 一项 `environment_blocked` 的人工 waiver；
5. CI 与 QA 证据指向同一提交 SHA；
6. 设计变更后测试规格更新，并由实现节点消费最新版本。

## 8. 必须通过的自动化测试

- 模板结构测试：新节点位置、字段、返工边映射完整；
- manifest 校验测试：空文档、重复 Case ID、缺少 AC/风险/预期结果必须失败；
- 工作流端到端测试：需求审核 -> 设计审核 -> 测试规格 -> 实现 -> QA -> QA 打回，断言 implementation 收到失败项和原测试规格引用；
- QA 测试 checkpoint 权限测试：允许测试工件，拒绝 QA 修改业务实现；
- 前端构建与交互测试：规格引用、风险矩阵和 waiver 状态可读；
- `make test`、`make build`，以及试点项目 CI 的实际 pipeline 验证。

## 9. 完成定义

只有当以下条件同时满足，才可将该能力标记为已交付：

- 新创建的 Greenfield vNext 工作流可以稳定运行；
- 实现、初审、QA 都能读取同一个不可变测试规格版本；
- QA 打回的失败 Case、证据和规格引用会回流至研发；
- 至少一次真实试点证明研发修复后对应测试从红转绿；
- 未将 API 业务流测试伪称为浏览器 E2E，也未将 Agent 报告伪称为不可伪造证据；
- 存量工作流、现有任务和 `unified-delivery-pipeline` 行为不受影响。
