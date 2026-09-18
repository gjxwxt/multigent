# 交付流水线双轨制分工与验收测试规格前置 (ATD) 必要性判定

**Decision**

1. **坚持流水线物理双轨制架构，不搞一刀切**：
   - **日常快速迭代流水线 (`unified-delivery-pipeline`)**：保持精简高效（需求澄清 $\to$ 澄清快审 $\to$ 编码实现 $\to$ 独立初审 $\to$ CI 闸门 $\to$ 人工代码审 $\to$ 合并 $\to$ 后置 QA $\to$ 发布；此处步骤列表为关键主干摘要，含变更日志与 PR 审核在内的完整 13 步拓扑定义以 `internal/workflow/store.go` 为准）。坚决不插入前置测试规格设计（ATD）节点与前置设计门，杜绝日常小需求、Bug 修复和轻量重构的流程阻力与模型上下文膨胀。
   - **全新工程交付流水线 (`greenfield-delivery-pipeline` vNext)**：针对从零起跑、高保真交付的新工程，完整保留 `design_review`（OpenDesign 高保真原型门）与 `acceptance_test_design`（ATD 验收测试规格前置设计门）。
2. **保留 Greenfield 中的 ATD 节点，作为研发与 QA 之间的确定性契约锚点**：
   - 不回退或删除 `acceptance_test_design`。该节点产出的 `test_spec_manifest` 由纯函数强类型校验器（`internal/workflow/testspec.go`）硬门禁拦截（占位词拦截、字段校验），向研发提供不可变的机器可读测试目标，向初审与后置 QA 提供事实对账基线。
3. **正确归因历史成功经验 (`t-20260910-pu8mhs`)**：
   - 2026-09-10 任务的良好体验验证了“设计确认 + 独立初审 + 合并前 QA 闸门”大骨架的鲁棒性，以及大模型直读 HTML 原型写前端的高还原度；
   - 但它无法防御“作者声称单测 2/2 通过但自身断言颠倒（如 403 掩盖 401 契约破坏）”的隐蔽缺陷。ATD 的价值在于将“事后抓破绽”前置为“事前定契约”。
4. **验证路径**：
   - 停止在理论层面的抽象争论；在部署窗口中，将 Batch C-2（真实 GitLab CI runner 联调）与带 ATD 的 Greenfield vNext 真实从零交付合为一体进行实测，客观对比手感稳定性与往返打回率。

**Reason**

1. **消除日常开发稳定性与敏捷度焦虑**：
   - 串行多 Agent 流水线每增加一个 LLM 步骤，全流程的综合稳定性与耗时就多承受一层偶发风险。
   - 日常高频修改若被强制要求产出几十项测试 Case 和 JSON 映射，研发 Agent 会陷入注意力分散与认知过载（把精力消耗在凑用例上，而非业务逻辑本身）。双轨制彻底解耦了日常敏捷迭代与重型新项目交付。
2. **防范大模型“假测试”的系统性腐蚀**：
   - 在缺乏前置客观测试基线时，研发 Agent 极易写出“为了让 `make test` 变绿而迎合自身错误代码”的无效断言；
   - ATD 产出的 `expected_result` 形成独立不可变的客观第三方法定事实，初审 Agent 和 QA Agent 均凭此对账，使研发无法单方面漂移契约。
3. **确定性纯函数闸门保障稳定性**：
   - ATD 节点的校验核心是纯函数（无模型黑盒二次裁决），输出不合规立即 400 打回，不带脏数据进入下游。

**Cost to reverse**

若未来在真实生产实践中证明特定场景下 Greenfield 的 ATD 依然造成过度开销：
1. 可在 `design_review` 审批时增加 `skip_atd: true` 旁路选项，支持人类审批人一键跳过 ATD 直接流向 `implementation`；
2. 严禁直接破坏 `greenfield-delivery-pipeline` 的状态机定义或删除 `internal/workflow/testspec.go` 纯函数保障。
