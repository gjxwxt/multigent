package workflow

import (
	"github.com/multigent/multigent/internal/entity"
)

// greenfieldDeliveryTemplate is the greenfield-only delivery pipeline: for a
// project's first delivery there is no existing UI, so a visual design gate
// (OpenDesign, human_review with Config designGate=true) sits between the
// quick requirement review and implementation. Existing projects keep using
// the unified pipeline untouched.
func greenfieldDeliveryTemplate(locale string) entity.WorkflowTemplate {
	locale = normalizeTemplateLocale(locale)
	text := localizedTemplateText(locale, map[string]string{
		"name":             "New Project Delivery (Design Gate)",
		"description":      "First-delivery pipeline for brand-new projects: requirement clarification, a visual design gate powered by OpenDesign, then implementation, agent self-review, human code review, PR merge, first release, and go-live confirmation.",
		"approved":         "approved",
		"changesRequested": "changes requested",

		"reqDraftTitle":  "Requirement Clarification",
		"reqDraftDesc":   "An agent turns the initial request into a structured requirement: problem, goal, scope, non-goals, risks, and open questions.",
		"reqReviewTitle": "Requirement Quick Review",
		"reqReviewDesc":  "A human quickly confirms the requirement draft is aiming at the right thing before any design or code work starts.",
		"designTitle":    "Design Confirmation",
		"designDesc":     "Review the visual prototype produced in OpenDesign (or pick an existing design). Confirm the direction, request changes, or generate a new variant before implementation begins.",

		"scaleGateTitle":        "Delivery Scale Gate",
		"scaleGateDesc":         "Classify the approved requirement into a delivery shape. Output scale_verdict: linear (single coherent implementation pass — small/medium change, low intra-requirement independence) or batched (large requirement decomposed into parallel workstreams). When batched, also output batch_plan: the workstream list (id, title, cohesive domain rationale, expected UC set) plus the SHARED CONTRACT list (database schema, error codes, API skeleton) that must be committed to the baseline before any workstream starts. The verdict is advisory — the requirement reviewer confirms it; branch editing happens on the canvas before the run reaches the parallel stage.",
		"scaleVerdictField":     "Delivery shape verdict: linear (default when undecided) or batched. Routed deterministically by the platform.",
		"batchPlanField":        "When batched: structured JSON delivery plan. workPackages[] entries need id (or branchId), title, cohesive domain rationale, dependsOn[] (ids of work packages that must complete first), acceptanceCriteria[] (requirement_items ids from requirement_draft, or \"infra:<name>\" for infrastructure-only packages), agentBinding (the agent that owns the package), and optionally expectedDelivery[] and contractRefs[]. Also: sharedContract[] (id/artifact/path) and optionally planId/integrationPolicy/qaPolicy. This plan is what the contract review freezes; the parallel stage derives one branch per work package from it — no canvas branch editing.",
		"deliveryPlanField":     "Preferred field name for the structured delivery plan (same schema as batch_plan).",
		"requirementItemsField": "JSON array of the requirement anchors this delivery traces to: [{id, text, source}]. The ids are frozen by the requirement review and referenced by every non-infrastructure work package's acceptanceCriteria.",

		"contractBatchTitle":     "Shared Contract Batch",
		"contractBatchDesc":      "BATCHED PATH ONLY. Build the shared foundation every workstream depends on: database schema and migrations, error-code enumeration, API skeleton (paths/auth/error semantics), and the tech-stack skeleton (per batch_plan). Commit to the integration baseline BEFORE parallel work starts — parallel branches cannot see each other's code, so this contract is their only shared surface. Follow the batch_plan contract list; do not implement workstream business logic here. When entering this step through a rework loop (or whenever the upstream requirement snapshot was malformed), you MUST re-emit requirement_items as a structured output: a JSON array of {\"id\",\"text\",\"source\"} objects covering every requirement anchor — the human contract review and the plan freeze both consume this output.",
		"contractArtifactsField": "Committed shared-contract summary: schema objects, error codes, API paths, and the baseline commit SHA carrying them. The baseline commit SHA must be a full 40-hex commit SHA or be omitted entirely — a short SHA or branch name is refused at the freeze gate.",

		"contractReviewTitle":      "Shared Contract Review",
		"contractReviewDesc":       "Human gate: confirm the committed contract (schema, error codes, API skeleton, stack skeleton) is complete and correct before parallel workstreams fork from it. Contract errors multiply into every branch — approve only with the baseline SHA recorded.",
		"contractReviewNotesField": "Review notes for the shared contract; edits requested here go back to contract_batch.",

		"integrationReviewTitle": "Integration Review",
		"integrationReviewDesc":  "Human gate after parallel workstreams converge (joinPolicy all): verify the branches merge cleanly on the shared contract — no schema drift, no duplicate/conflicting error codes, no API contract violations. Seam problems found here route to the linear implementation step as unified rework; branch-internal problems were already caught inside each branch.",
		"atdTitle":               "Acceptance Test Design",
		"atdDesc":                "Independent QA agent produces a risk-driven acceptance test specification BEFORE coding starts, so the developer's TDD has explicit external behavior targets. Inputs: the approved requirement (ACs, non-goals, constraints), the frozen design references, and the project's runtime capabilities. Output contract: test_spec_doc holds the full specification document (one section per case: case_id, ac_id, scenario Given/When/Then, expected_result observable and assertable); test_spec_manifest holds a JSON array — every entry exactly {\"case_id\" (unique, stable, e.g. AUTH-001), \"ac_id\" (linked acceptance criterion or requirement section), \"risk_level\" (high|medium|low), \"automation_level\" (unit|api_integration|ui_e2e|manual), \"execution_type\" (auto|manual|environment_blocked), \"expected_result\" (non-empty)}. Hard rules: high-risk cases must have an executable verification path or an explicit manual waiver premise; 'to be observed' or 'as appropriate' is never a valid expected_result; do not pad the case list with scenarios that link to no AC. test_spec_summary states the case count, risk distribution, non-automatable items and environment prerequisites.",
		"implTitle":              "Implementation",
		"implDesc":               "Implement the approved requirement following the approved design references AND the acceptance test specification (test_spec_doc + manifest): every automated case in the manifest is an external behavior target — write the failing test first (TDD), then make it pass. Record the PR, tests executed (with case_id mapping in test_implementation_evidence), and any risks.",
		"selfReviewTitle":        "Agent Self Review",
		"selfReviewDesc":         "Independent reviewer agent — judge the diff against an independently built baseline, not against the author's narrative. Method: (1) read the approved requirement and design references; without opening the diff, write your own baseline of the behavior each acceptance criterion implies, the data flow, permission boundaries, and the test surfaces that should exist. (2) Note the base SHA and HEAD SHA from the implementation evidence, then read ONLY that diff and compare it against your baseline, criterion by criterion, including design conformance. (3) Re-run build and tests yourself — a check you did not run is unverified and cannot back an approval. Request fixes for up to three rounds. Write the verdict to self_review_verdict: pass when the acceptance criteria are met, issues_fixed when the implementation must be reworked, with findings written as structured issues (what contract is violated, file:line, impact, minimal fix, missing verification). Independence boundaries: do not treat 'a different architecture would be better' as a defect, and judge only the diff and its observable behavior, not the author's reasoning.",
		"codeReviewTitle":        "Human Code Review",
		"codeReviewDesc":         "A human reviews the implementation evidence and decides whether it can proceed to PR and merge.",
		"prMergeTitle":           "Open PR and Merge",
		"prMergeDesc":            "Open the merge request, wait for CI to pass, then merge. Record the merged SHA and PR URL.",
		"releaseTitle":           "First Release",
		"releaseDesc":            "Tag the first version, deploy, and probe the health endpoint. Record the tag, deployed version, and health status.",
		"goLiveTitle":            "Go-Live Confirmation",
		"goLiveDesc":             "A human verifies the live deployment meets expectations and formally accepts the first delivery.",

		"requestField":           "Original request and business context for the new project.",
		"contextField":           "Known background, constraints, and references.",
		"decisionField":          "approve or request_changes.",
		"commentsField":          "Review comments and guidance.",
		"designSourceField":      "Optional. existing (pick a listed design) or generated (created from the requirement). Empty on rework.",
		"designProjectField":     "Optional. OpenDesign project id backing the approved design.",
		"designPreviewField":     "Optional. Preview URL of the approved design for downstream reference.",
		"designHTMLField":        "Optional. Frozen HTML of the approved design captured at confirm time; consumers must use this copy, not the live OD project.",
		"designSnapshotField":    "Optional. Workspace-relative path of the frozen design snapshot bundle (see manifest.json).",
		"testSpecDocField":       "Full acceptance test specification document (one section per case with Given/When/Then and observable expected results).",
		"testSpecManifestField":  "JSON array of test case entries: case_id, ac_id, risk_level, automation_level, execution_type, expected_result. Validated structurally by the platform.",
		"testSpecSummaryField":   "Case count, risk distribution, non-automatable items and environment prerequisites.",
		"prField":                "PR or patch summary produced by implementation.",
		"testsField":             "Tests executed with evidence.",
		"risksField":             "Known risks and follow-ups.",
		"reviewCommentsField":    "Findings from the agent self review.",
		"verdictField":           "Self-review verdict: pass (proceed to human review) or issues_fixed (rework).",
		"mergedField":            "Merged commit SHA on the integration branch.",
		"prURLField":             "URL of the opened merge request.",
		"tagField":               "Release tag (e.g. v0.1.0).",
		"deployedField":          "Deployed version identifier.",
		"healthField":            "Health probe result after deployment.",
		"qaTitle":                "QA Test & Risk Matrix",
		"qaDesc":                 "Independent QA agent evaluates the candidate against the acceptance test specification (not just the developer's claims): reconcile the original test_spec_manifest, the actual base/head diff and the developer's test_implementation_evidence mapping; run the automated cases independently; add boundary, exception, authorization, idempotency and regression checks the spec implies; produce the structured risk-coverage matrix. QA may add regression tests but only touches test files/fixtures/probe scripts — never business code. Cases that cannot be verified in the sandbox must be marked environment_blocked or manual in the matrix — never silently green.",
		"qaSignoffTitle":         "QA Sign-off",
		"qaSignoffDesc":          "Human QA reviews the risk-coverage matrix and test report. Any high-risk unpassed item requires explicit per-item waiver signoff before merging to main.",
		"riskMatrixField":        "Mandatory risk-coverage matrix in JSON format covering acceptance criteria, affected APIs, risk level, test dimension, and verification evidence.",
		"testReportField":        "Detailed execution logs, test outputs, and evidence links.",
		"manualWaiversField":     "Optional JSON mapping of item_id to explicit waiver rationale for unexecuted or blocked high-risk items.",
		"qaReworkItemsField":     "Platform-derived structured rework list (JSON array of failed/blocked/unexecuted items with item_id, risk_level, status, acceptance_criteria, evidence). Populated by the server on qa_signoff rejection; downstream implementation consumes it as fix targets.",
		"qaTouchedPathsField":    "Newline-separated list of every file path the QA agent created or modified (tests, fixtures, probe scripts only — business code is forbidden and the platform gate rejects it).",
		"designWaiverField":      "Explicit justification for approving when automated design snapshot capture failed.",
		"designWaivedField":      "true if design verification was granted an explicit audited waiver.",
		"designConformanceField": "Reviewer agent's structured assessment of implementation visual and interaction fidelity against the design prototype.",
	}, map[string]string{
		"name":             "新项目交付流水线（设计确认闸门）",
		"description":      "新项目首次交付专用流水线：需求澄清后先经 OpenDesign 设计确认闸门，再实现编码、Agent 初审、人工代码审核、开 PR 合并、首版发布与上线确认。",
		"approved":         "通过",
		"changesRequested": "需要修改",

		"reqDraftTitle":  "需求澄清",
		"reqDraftDesc":   "Agent 把初始请求整理成结构化需求：问题、目标、范围、非目标、风险与待澄清问题。",
		"reqReviewTitle": "需求快审",
		"reqReviewDesc":  "在设计或编码开始前，人工快速确认需求方向正确。",
		"designTitle":    "设计确认",
		"designDesc":     "审阅 OpenDesign 生成的可视化原型（或选择已有设计），确认方向、打回修改或重新生成，然后再进入实现。",

		"scaleGateTitle":        "交付规模裁决",
		"scaleGateDesc":         "把已批准的需求归类为交付形态。输出 scale_verdict：linear（单条实现线——中小型变更、需求内部独立性低）或 batched（大需求分解为并行工作流）。batched 时同时输出 batch_plan：工作流清单（branch_id、标题、内聚域依据、预期 UC 集合）以及必须在任何工作流开工前提交到基线的共享契约清单（数据库 schema、错误码、API 骨架）。裁决是建议性的——需求审核人最终拍板；run 到达并行阶段前可在画布上编辑分支。",
		"scaleVerdictField":     "交付形态裁决：linear（未决时默认）或 batched。由平台确定性路由。",
		"batchPlanField":        "batched 时：结构化 JSON 交付计划。workPackages[] 每项需含 id（或 branchId）、标题、内聚域依据、dependsOn[]（必须先完成的工作包 id）、acceptanceCriteria[]（引用 requirement_draft 的 requirement_items id，或基础设施包用 \"infra:<名称>\"）、agentBinding（承接该包的 agent），可选 expectedDelivery[] 与 contractRefs[]。另可含 sharedContract[] 与 planId/integrationPolicy/qaPolicy。这份计划就是 contract_review 冻结的对象，并行阶段按它派生分支——无需在画布上编辑分支。",
		"deliveryPlanField":     "结构化交付计划的首选字段名（与 batch_plan 同 schema）。",
		"requirementItemsField": "本次交付追溯的需求锚点 JSON 数组：[{id, text, source}]。id 由需求评审冻结，所有非基础设施工作包的 acceptanceCriteria 都必须引用它们。",

		"contractBatchTitle":     "共享契约批",
		"contractBatchDesc":      "仅 batched 路径。构建所有工作流共同依赖的共享地基：数据库 schema 与迁移、错误码枚举、API 骨架（路径/鉴权/错误语义）与技术栈骨架（按 batch_plan）。在并行工作开始前提交到集线基线——并行分支互相看不见对方代码，这份契约是它们唯一的共享面。按 batch_plan 的契约清单执行；不要在此实现工作流的业务逻辑。经返工环进入本步骤（或上游需求快照畸形）时，必须把 requirement_items 作为结构化输出重新提交：JSON 数组，每项含 {\"id\",\"text\",\"source\"}，覆盖全部需求锚点——人工契约评审与计划冻结都以这份输出为准。",
		"contractArtifactsField": "已提交的共享契约摘要：schema 对象、错误码、API 路径，以及承载它们的基线 commit SHA。基线 commit SHA 必须是完整 40 位十六进制 SHA，否则整字段省略——短 SHA 或分支名会在冻结闸门被拒。",

		"contractReviewTitle":      "共享契约评审",
		"contractReviewDesc":       "人工闸门：确认已提交的契约（schema、错误码、API 骨架、栈骨架）完整且正确，然后才允许并行工作流从它分叉。契约错误会放大到每一个分支——仅在基线 SHA 记录在案后批准。",
		"contractReviewNotesField": "共享契约的审核意见；此处要求的修改打回 contract_batch。",

		"integrationReviewTitle": "集成评审",
		"integrationReviewDesc":  "并行工作流汇聚后（joinPolicy all）的人工闸门：核对各分支在共享契约上无缝合并——无 schema 漂移、无错误码重复/冲突、无 API 契约违约。此处发现的接缝问题路由到线性实现步骤统一返工；分支内部问题已在各分支内消化。",
		"atdTitle":               "验收测试设计",
		"atdDesc":                "独立 QA Agent 在编码开始前产出风险驱动的验收测试规格，让研发的 TDD 有清晰的外部行为目标。输入：已审核需求全文（验收标准、非目标、约束）、已冻结的设计引用、项目运行时能力。输出契约：test_spec_doc 为完整规格文档（每条 Case 一节：case_id、ac_id、Given/When/Then 场景、可观察可断言的 expected_result）；test_spec_manifest 为 JSON 数组——每项恰好包含 {\"case_id\"（唯一稳定，如 AUTH-001）、\"ac_id\"（关联验收标准或需求章节）、\"risk_level\"（high|medium|low）、\"automation_level\"（unit|api_integration|ui_e2e|manual）、\"execution_type\"（auto|manual|environment_blocked）、\"expected_result\"（非空）}。硬规则：高风险 Case 必须有可执行验证路径或明确的人工豁免前提；「待观察」「视情况」不构成合法预期结果；禁止生成不关联任何 AC 的凑数 Case。test_spec_summary 概述用例数量、风险分布、不可自动化项与环境前提。",
		"implTitle":              "实现编码",
		"implDesc":               "以已确认的设计产物和验收测试规格（test_spec_doc + manifest）为参考实现需求：manifest 中每条 auto Case 都是外部行为目标——先写失败测试（TDD），再使其通过。记录 PR、执行的测试（在 test_implementation_evidence 中给出 case_id 映射）与风险。",
		"selfReviewTitle":        "Agent 初审",
		"selfReviewDesc":         "独立 reviewer agent——对照自己独立建立的预期基线裁决 diff，而非对照作者的自述。方法：(1) 先读已确认的需求与设计参考，不打开 diff，独立写出预期基线：每条验收标准蕴含的行为、数据流、权限边界、应当存在的测试切面。(2) 从实现证据记下 base SHA 与 HEAD SHA，只读该区间的 diff，逐条标准对照基线（含设计一致性）。(3) 亲自重跑构建与测试——没有亲手跑过的检查视为 unverified，不能支撑通过。最多三轮返工。初审结论写入 self_review_verdict：验收达标写 pass，需要返工写 issues_fixed；发现的问题写成结构化条目（违反什么契约、文件:行号、影响、最小修复、缺失的验证）。独立性边界：不要把「换一种架构更好」当作缺陷；只裁决 diff 与其可观察行为，不读取或信任作者的思考链。",
		"codeReviewTitle":        "人工代码审核",
		"codeReviewDesc":         "人工审核实现证据，决定是否进入 QA 验证。",
		"qaTitle":                "QA 测试与风险矩阵",
		"qaDesc":                 "独立 QA Agent 以验收测试规格为基线（而非研发自述）审查候选变更：将原始 test_spec_manifest、实际 base/head diff 与研发的 test_implementation_evidence 映射对账；独立执行 auto Case；补充规格蕴含的边界、异常、鉴权、幂等与回归验证；产出结构化风险-覆盖矩阵。QA 可编写回归测试，但只允许修改测试文件/夹具/探测脚本——禁止顺手修改业务代码。沙箱无法验证的外部系统或长时任务必须在矩阵中标注 environment_blocked 或 manual——严禁静默标绿。",
		"qaSignoffTitle":         "QA 准出签核",
		"qaSignoffDesc":          "人工 QA 审核风险-覆盖矩阵与测试报告。高风险未测/阻塞项必须由人类按 item_id 逐项签核豁免理由后方可准入主干。",
		"prMergeTitle":           "开 PR 并合并",
		"prMergeDesc":            "开合并请求，等待 CI 通过后合并。记录合并 SHA 与 PR 地址。",
		"releaseTitle":           "首版发布",
		"releaseDesc":            "打首版 tag、部署并探测健康检查端点。记录 tag、部署版本与健康状态。",
		"goLiveTitle":            "上线确认",
		"goLiveDesc":             "人工验收线上部署效果，正式确认首次交付完成。",

		"requestField":           "新项目的原始请求与业务背景。",
		"contextField":           "已知背景、约束与参考资料。",
		"decisionField":          "approve 或 request_changes。",
		"commentsField":          "审核意见与指导。",
		"designSourceField":      "可选。existing（选择已有设计）或 generated（由需求生成）。打回时留空。",
		"designProjectField":     "可选。支撑已确认设计的 OpenDesign 项目 ID。",
		"designPreviewField":     "可选。已确认设计的预览地址，供下游参考。",
		"designHTMLField":        "可选。确认时冻结的已确认设计 HTML 快照；下游一律使用此副本，不读 OD 实时项目。",
		"designSnapshotField":    "可选。冻结设计快照包的工作区相对路径（见 manifest.json）。",
		"testSpecDocField":       "完整验收测试规格文档（每条 Case 一节，含 Given/When/Then 与可观察的预期结果）。",
		"testSpecManifestField":  "测试用例 JSON 数组：case_id、ac_id、risk_level、automation_level、execution_type、expected_result。平台做结构化校验。",
		"testSpecSummaryField":   "用例数量、风险分布、不可自动化项与环境前提。",
		"prField":                "实现产出的 PR 或补丁摘要。",
		"testsField":             "已执行的测试与证据。",
		"risksField":             "已知风险与后续事项。",
		"reviewCommentsField":    "Agent 初审发现的问题。",
		"verdictField":           "初审结论：pass（进入人工审核）或 issues_fixed（返工）。",
		"mergedField":            "集成分支上的合并提交 SHA。",
		"prURLField":             "合并请求地址。",
		"tagField":               "发布 tag（如 v0.1.0）。",
		"deployedField":          "部署版本标识。",
		"healthField":            "部署后的健康探测结果。",
		"riskMatrixField":        "必填的风险-覆盖矩阵 JSON 数组：[{\"item_id\":\"A1\",\"acceptance_criteria\":\"...\",\"risk_level\":\"high\",\"status\":\"passed\",\"evidence\":\"...\"}]，覆盖验收项、风险级别、状态与执行证据。",
		"testReportField":        "详细测试执行日志、用例输出与证据链接。",
		"manualWaiversField":     "可选的 JSON 映射：高风险项 item_id 到人工特批豁免理由的键值对。",
		"qaReworkItemsField":     "平台聚合的结构化返工清单（JSON 数组：failed/blocked/unexecuted 项的 item_id、risk_level、status、acceptance_criteria、evidence）。qa_signoff 打回时由服务端自动生成，实现节点按此作为修复目标。",
		"qaTouchedPathsField":    "QA Agent 新建或修改的全部文件路径（换行分隔，仅限测试文件/夹具/探测脚本——业务代码被平台闸门拒绝）。",
		"designWaiverField":      "设计快照抓取异常时，人工特批放行的明确豁免理由。",
		"designWaivedField":      "若本次交付经过设计特批豁免则为 true。",
		"designConformanceField": "初审 Agent 对比实现产物与设计原型的结构化一致性评估与合理偏差说明。",
	})
	field := func(name, descKey string) entity.WorkflowField {
		return entity.WorkflowField{Name: name, Description: text[descKey]}
	}
	optionalField := func(name, descKey string) entity.WorkflowField {
		f := field(name, descKey)
		f.Optional = true
		return f
	}

	designStep := tmplStep("design_review", "human_review", text["designTitle"], text["designDesc"], "product-owner", "violet", 640,
		[]entity.WorkflowField{
			field("approved_requirement", "requestField"),
			optionalField("requirement_draft", "requestField"),
		},
		[]entity.WorkflowField{
			field("decision", "decisionField"),
			field("comments", "commentsField"),
			optionalField("approved_design_source", "designSourceField"),
			optionalField("approved_design_project_id", "designProjectField"),
			optionalField("approved_design_preview_url", "designPreviewField"),
			optionalField("approved_design_html", "designHTMLField"),
			optionalField("approved_design_snapshot_path", "designSnapshotField"),
			optionalField("design_waiver_reason", "designWaiverField"),
			optionalField("design_waived", "designWaivedField"),
		})
	if designStep.Config == nil {
		designStep.Config = map[string]string{}
	}
	designStep.Config["designGate"] = "true"
	// Task 0.1: the unified marker is declarative and survives step renames;
	// the legacy designGate flag above stays until the Task 0.5 deprecation.
	designStep.Config[GateConfigKey] = GateKindDesignGate

	// Delivery scale gate (large-requirement module S1): a structured verdict
	// step after the design gate. The agent PROPOSES linear/batched; the
	// deterministic cond() edges route on the verdict; the default edge falls
	// back to linear so an undecided verdict can never silently fan out
	// (fail-safe toward the historical single-track path).
	scaleGateStep := tmplStep("scale_gate", "agent_task", text["scaleGateTitle"], text["scaleGateDesc"], "pm-agent", "cyan", 500,
		[]entity.WorkflowField{
			field("approved_requirement", "requestField"),
			optionalField("requirement_draft", "requestField"),
			optionalField("requirement_items", "requirementItemsField"),
			optionalField("approved_design_snapshot_path", "designSnapshotField"),
		},
		[]entity.WorkflowField{
			field("scale_verdict", "scaleVerdictField"),
			optionalField("delivery_plan", "deliveryPlanField"),
			optionalField("batch_plan", "batchPlanField"),
		})

	// Shared-contract batch + human contract review (batched path only).
	// The contract MUST land on the integration baseline before branches fork
	// (large-requirement module DM5): parallel branches cannot see each
	// other's code, so the committed contract is their only shared surface.
	contractBatchStep := tmplStep("contract_batch", "agent_task", text["contractBatchTitle"], text["contractBatchDesc"], "developer-agent", "violet", 700,
		[]entity.WorkflowField{
			field("approved_requirement", "requestField"),
			optionalField("delivery_plan", "deliveryPlanField"),
			optionalField("batch_plan", "batchPlanField"),
			optionalField("requirement_items", "requirementItemsField"),
			optionalField("approved_design_snapshot_path", "designSnapshotField"),
		},
		[]entity.WorkflowField{
			field("contract_artifacts", "contractArtifactsField"),
			optionalField("delivery_plan", "deliveryPlanField"),
			// S2 hardening batch 2 (run4 finding): requirement_items is an
			// INPUT-only field on this step, yet the freeze anchor chain
			// (plan_freeze.go) reads it from contract_batch OUTPUTS — the
			// request_changes→contract_batch rework loop could never repair a
			// malformed requirement_draft snapshot because the step-complete
			// whitelist rejected the field before it could flow to the freeze.
			// Declaring it here makes the rework loop able to land corrected
			// anchors; the freeze validation itself stays unchanged and
			// fail-closed. The e-contract-batch-review edge forwards THIS
			// output to the human review so what the reviewer sees is what
			// the freeze consumes.
			optionalField("requirement_items", "requirementItemsField"),
		})

	contractReviewStep := tmplStep("contract_review", "human_review", text["contractReviewTitle"], text["contractReviewDesc"], "owner-engineer", "amber", 860,
		[]entity.WorkflowField{
			field("contract_artifacts", "contractArtifactsField"),
			optionalField("delivery_plan", "deliveryPlanField"),
			optionalField("batch_plan", "batchPlanField"),
			optionalField("requirement_items", "requirementItemsField"),
		},
		[]entity.WorkflowField{
			field("decision", "decisionField"),
			field("comments", "commentsField"),
		})
	// The approving contract review is the ONLY entry that freezes the
	// delivery plan: on approve the platform validates the structured plan and
	// commits the frozen record INSIDE the same transition that advances the
	// run; request_changes never freezes.
	contractReviewStep.Config[PlanFreezeConfigKey] = "true"

	// Parallel workstream stage (batched path). Branches are STATIC at
	// instantiation (module DM4): two generic workstream branches ship with
	// the template as the minimal usable fan-out; humans edit titles, actor
	// roles, descriptions and input fields on the canvas following the
	// batch_plan BEFORE the run reaches this step. Engine-side, each branch
	// spawns a child task + sub-run and must carry an agent actor binding.
	parallelStep := tmplStep("parallel_workstreams", "parallel_stage", "Parallel Workstreams", "Fan-out of the decomposed workstreams. Branches are derived from the FROZEN delivery plan approved at the contract review (one branch per work package, dependency order); no canvas editing is required. The static branches below are the legacy fallback for runs without a frozen plan.", "", "violet", 1020,
		[]entity.WorkflowField{
			field("contract_artifacts", "contractArtifactsField"),
			optionalField("approved_requirement", "requestField"),
		},
		nil)
	parallelStep.JoinPolicy = "all"
	// Plan-driven stage: with a frozen plan the branch list is derived from the
	// plan's ready work packages; without one the stage refuses (fail-closed)
	// rather than falling back to generic branches.
	parallelStep.Config = map[string]string{"planMaterialization": "frozen"}
	parallelStep.Branches = []entity.WorkflowBranch{
		{
			ID:          "workstream_1",
			Title:       "Workstream 1",
			Description: "Generic first workstream — rename and scope on the canvas per batch_plan (cohesive domain, e.g. crypto chain / state machine / UI).",
			ActorRole:   "workstream-agent-1",
			InputFields: []entity.WorkflowField{
				field("contract_artifacts", "contractArtifactsField"),
				optionalField("approved_requirement", "requestField"),
			},
			OutputFields: []entity.WorkflowField{field("branch_summary", "prField"), field("touched_paths", "qaTouchedPathsField")},
		},
		{
			ID:          "workstream_2",
			Title:       "Workstream 2",
			Description: "Generic second workstream — rename and scope on the canvas per batch_plan. Delete or add branches so the count matches the plan; each branch needs its own agent binding.",
			ActorRole:   "workstream-agent-2",
			InputFields: []entity.WorkflowField{
				field("contract_artifacts", "contractArtifactsField"),
				optionalField("approved_requirement", "requestField"),
			},
			OutputFields: []entity.WorkflowField{field("branch_summary", "prField"), field("touched_paths", "qaTouchedPathsField")},
		},
	}

	integrationReviewStep := tmplStep("integration_review", "human_review", text["integrationReviewTitle"], text["integrationReviewDesc"], "owner-engineer", "amber", 1240,
		[]entity.WorkflowField{
			field("contract_artifacts", "contractArtifactsField"),
			optionalField("approved_requirement", "requestField"),
			optionalField("branch_reports", "prField"),
			// Optional spec round-trip inputs: empty on first entry (the spec is
			// designed after integration), present when integration rework looped
			// through implementation — pass-through keeps the baseline alive.
			optionalField("test_spec_doc", "testSpecDocField"),
			optionalField("test_spec_manifest", "testSpecManifestField"),
			optionalField("test_spec_summary", "testSpecSummaryField"),
		},
		[]entity.WorkflowField{
			field("decision", "decisionField"),
			field("comments", "commentsField"),
		})

	acceptanceTestDesignStep := tmplStep("acceptance_test_design", "agent_task", text["atdTitle"], text["atdDesc"], "qa-agent", "rose", 780,
		[]entity.WorkflowField{
			field("approved_requirement", "requestField"),
			optionalField("approved_design_source", "designSourceField"),
			optionalField("approved_design_project_id", "designProjectField"),
			optionalField("approved_design_preview_url", "designPreviewField"),
			optionalField("approved_design_html", "designHTMLField"),
			optionalField("approved_design_snapshot_path", "designSnapshotField"),
		},
		[]entity.WorkflowField{
			field("test_spec_doc", "testSpecDocField"),
			field("test_spec_manifest", "testSpecManifestField"),
			field("test_spec_summary", "testSpecSummaryField"),
		})

	return templateFromParts("greenfield-delivery-pipeline", text["name"], text["description"], locale, "requirement_draft",
		[]entity.WorkflowStep{
			tmplStep("requirement_draft", "agent_task", text["reqDraftTitle"], text["reqDraftDesc"], "pm-agent", "sky", 80,
				[]entity.WorkflowField{field("request", "requestField"), field("context", "contextField")},
				[]entity.WorkflowField{field("requirement_draft", "requestField"), field("open_questions", "contextField"), optionalField("requirement_items", "requirementItemsField")}),
			tmplStep("requirement_review", "human_review", text["reqReviewTitle"], text["reqReviewDesc"], "product-owner", "amber", 360,
				[]entity.WorkflowField{field("requirement_draft", "requestField"), optionalField("requirement_items", "requirementItemsField")},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField"), optionalField("approved_requirement", "requestField")}),
			designStep,
			scaleGateStep,
			contractBatchStep,
			contractReviewStep,
			parallelStep,
			integrationReviewStep,
			acceptanceTestDesignStep,
			tmplStep("implementation", "agent_task", text["implTitle"], text["implDesc"], "developer-agent", "emerald", 920,
				[]entity.WorkflowField{
					field("approved_requirement", "requestField"),
					optionalField("approved_design_source", "designSourceField"),
					optionalField("approved_design_project_id", "designProjectField"),
					optionalField("approved_design_preview_url", "designPreviewField"),
					optionalField("approved_design_html", "designHTMLField"),
					optionalField("approved_design_snapshot_path", "designSnapshotField"),
					optionalField("test_spec_doc", "testSpecDocField"),
					optionalField("test_spec_manifest", "testSpecManifestField"),
					optionalField("test_spec_summary", "testSpecSummaryField"),
					optionalField("review_comments", "reviewCommentsField"),
					optionalField("previous_pr", "prField"),
					optionalField("qa_rework_items", "qaReworkItemsField"),
				},
				[]entity.WorkflowField{field("pr", "prField"), field("tests_run", "testsField"), field("risks", "risksField"), field("test_implementation_evidence", "testReportField")}),
			tmplStep("self_review", "agent_task", text["selfReviewTitle"], text["selfReviewDesc"], "reviewer-agent", "rose", 1200,
				[]entity.WorkflowField{
					field("pr", "prField"),
					field("approved_requirement", "requestField"),
					optionalField("approved_design_source", "designSourceField"),
					optionalField("approved_design_project_id", "designProjectField"),
					optionalField("approved_design_preview_url", "designPreviewField"),
					optionalField("approved_design_html", "designHTMLField"),
					optionalField("approved_design_snapshot_path", "designSnapshotField"),
					optionalField("design_waiver_reason", "designWaiverField"),
					optionalField("design_waived", "designWaivedField"),
					optionalField("test_spec_doc", "testSpecDocField"),
					optionalField("test_spec_manifest", "testSpecManifestField"),
					optionalField("test_spec_summary", "testSpecSummaryField"),
					optionalField("test_implementation_evidence", "testReportField"),
				},
				[]entity.WorkflowField{
					field("self_review_verdict", "verdictField"),
					field("review_comments", "reviewCommentsField"),
					optionalField("design_conformance", "designConformanceField"),
				}),
			tmplStep("code_review", "human_review", text["codeReviewTitle"], text["codeReviewDesc"], "owner-engineer", "amber", 1480,
				[]entity.WorkflowField{
					field("pr", "prField"),
					field("approved_requirement", "requestField"),
					optionalField("approved_design_preview_url", "designPreviewField"),
					optionalField("approved_design_snapshot_path", "designSnapshotField"),
					optionalField("design_waiver_reason", "designWaiverField"),
					optionalField("design_waived", "designWaivedField"),
					field("review_comments", "reviewCommentsField"),
					optionalField("design_conformance", "designConformanceField"),
					optionalField("test_spec_doc", "testSpecDocField"),
					optionalField("test_spec_manifest", "testSpecManifestField"),
					optionalField("test_implementation_evidence", "testReportField"),
				},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField"), optionalField("approved_change", "prField")}),
			tmplStep("qa", "agent_task", text["qaTitle"], text["qaDesc"], "qa-agent", "rose", 1760,
				[]entity.WorkflowField{
					field("pr", "prField"),
					field("approved_change", "prField"),
					field("approved_requirement", "requestField"),
					optionalField("approved_design_snapshot_path", "designSnapshotField"),
					field("test_spec_doc", "testSpecDocField"),
					field("test_spec_manifest", "testSpecManifestField"),
					optionalField("test_spec_summary", "testSpecSummaryField"),
					optionalField("test_implementation_evidence", "testReportField"),
				},
				[]entity.WorkflowField{
					field("risk_coverage_matrix", "riskMatrixField"),
					field("touched_paths", "qaTouchedPathsField"),
					field("test_report", "testReportField"),
					// QA's own probe evidence (including regression cases QA
					// added) replaces/stale-marks the developer mapping when
					// reconciled — qa_signoff and rework read it from here.
					optionalField("test_implementation_evidence", "testReportField"),
				}),
			tmplStep("qa_signoff", "human_review", text["qaSignoffTitle"], text["qaSignoffDesc"], "qa-owner", "amber", 2040,
				[]entity.WorkflowField{
					field("risk_coverage_matrix", "riskMatrixField"),
					field("test_report", "testReportField"),
					optionalField("approved_design_snapshot_path", "designSnapshotField"),
					optionalField("design_waived", "designWaivedField"),
					field("pr", "prField"),
					field("approved_change", "prField"),
					optionalField("approved_requirement", "requestField"),
					optionalField("test_spec_doc", "testSpecDocField"),
					optionalField("test_spec_manifest", "testSpecManifestField"),
					optionalField("test_spec_summary", "testSpecSummaryField"),
					optionalField("test_implementation_evidence", "testReportField"),
				},
				[]entity.WorkflowField{
					field("decision", "decisionField"),
					field("comments", "commentsField"),
					optionalField("manual_waivers", "manualWaiversField"),
					// Batch B-a: platform-derived structured rework list — the
					// server aggregates failed/blocked/unexecuted matrix items
					// into qa_rework_items before persisting, so the reworked
					// implementation receives machine-readable fix targets
					// instead of only free-text comments.
					optionalField("qa_rework_items", "qaReworkItemsField"),
				}),
			withStepConfig(tmplStep("pr_open_and_merge", "agent_task", text["prMergeTitle"], text["prMergeDesc"], "developer-agent", "violet", 2320,
				[]entity.WorkflowField{field("approved_change", "prField"), field("pr", "prField")},
				[]entity.WorkflowField{field("merged_sha", "mergedField"), field("pr_url", "prURLField")}), map[string]string{StepConfigRequiresRemote: "true"}),
			withStepConfig(tmplStep("release", "agent_task", text["releaseTitle"], text["releaseDesc"], "release-agent", "emerald", 2600,
				[]entity.WorkflowField{field("merged_sha", "mergedField")},
				[]entity.WorkflowField{field("tag", "tagField"), field("deployed_version", "deployedField"), field("health_status", "healthField")}), map[string]string{StepConfigRequiresRemote: "true"}),
			tmplStep("go_live_confirm", "human_review", text["goLiveTitle"], text["goLiveDesc"], "product-owner", "amber", 2880,
				[]entity.WorkflowField{field("tag", "tagField"), field("deployed_version", "deployedField"), field("health_status", "healthField")},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField")}),
		},
		[]entity.WorkflowEdge{
			edge("e-req-draft-review", "requirement_draft", "requirement_review", "", nil, nil, true),
			edge("e-req-review-design", "requirement_review", "design_review", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"approved_requirement": "$output.approved_requirement",
				"requirement_draft":    "$input.requirement_draft",
				"requirement_items":    "$input.requirement_items",
			}, false),
			edge("e-req-review-rework", "requirement_review", "requirement_draft", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{"review_comments": "$output.comments", "previous_draft": "$input.requirement_draft"}, false),
			// Design gate approve now routes to the scale gate (large-requirement
			// module S1) instead of directly to acceptance_test_design; the scale
			// gate forwards the full design contract downstream on both paths.
			edge("e-design-approve", "design_review", "scale_gate", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"requirement_draft":             "$input.requirement_draft",
				"requirement_items":             "$input.requirement_items",
				"approved_design_source":        "$output.approved_design_source",
				"approved_design_project_id":    "$output.approved_design_project_id",
				"approved_design_preview_url":   "$output.approved_design_preview_url",
				"approved_design_html":          "$output.approved_design_html",
				"approved_design_snapshot_path": "$output.approved_design_snapshot_path",
				"design_waiver_reason":          "$output.design_waiver_reason",
				"design_waived":                 "$output.design_waived",
			}, false),
			// Scale gate routing: linear is the DEFAULT (fail-safe — an undecided
			// verdict keeps the historical single-track path); batched forks into
			// the contract batch. The design contract carries over on both.
			edge("e-scale-linear", "scale_gate", "acceptance_test_design", "linear", cond("scale_verdict", "eq", "linear"), map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
			}, false),
			edge("e-scale-batched", "scale_gate", "contract_batch", "batched", cond("scale_verdict", "eq", "batched"), map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"requirement_items":             "$input.requirement_items",
				"delivery_plan":                 "$output.delivery_plan",
				"batch_plan":                    "$output.batch_plan",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
			}, false),
			edge("e-scale-default-linear", "scale_gate", "acceptance_test_design", "", nil, map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
			}, true),
			// Contract batch → human contract review → parallel fan-out.
			edge("e-contract-batch-review", "contract_batch", "contract_review", "", nil, map[string]string{
				"contract_artifacts": "$output.contract_artifacts",
				"delivery_plan":      "$input.delivery_plan",
				"batch_plan":         "$input.batch_plan",
				// S2 hardening batch 2 (P1-1, review round): forward the OUTPUT
				// side. The freeze anchor chain reads contract_batch outputs
				// (newest-producer-first), so the human review must see the
				// same snapshot the freeze will consume — forwarding the input
				// side let a reviewer approve anchors the freeze would replace
				// with the reworked output (or vice versa).
				"requirement_items": "$output.requirement_items",
			}, true),
			edge("e-contract-review-rework", "contract_review", "contract_batch", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{
				"review_comments":               "$output.comments",
				"batch_plan":                    "$input.batch_plan",
				"requirement_items":             "$input.requirement_items",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
			}, false),
			edge("e-contract-review-parallel", "contract_review", "parallel_workstreams", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"delivery_plan":                 "$input.delivery_plan",
				"contract_artifacts":            "$input.contract_artifacts",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
			}, false),
			// All branches complete (joinPolicy all) → aggregated branch outputs
			// flow into the integration review via $output.* (the engine passes
			// the aggregate as the parallel stage's completion outputs).
			edge("e-parallel-integration", "parallel_workstreams", "integration_review", "", nil, map[string]string{
				"contract_artifacts":   "$input.contract_artifacts",
				"approved_requirement": "$input.approved_requirement",
				"branch_reports":       "$output.branch_reports",
			}, true),
			edge("e-integration-approve", "integration_review", "acceptance_test_design", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
				// Spec round-trip: when integration rework previously looped through
				// implementation, an existing acceptance baseline must survive and
				// reach acceptance_test_design (which will refresh it).
				"test_spec_doc":      "$input.test_spec_doc",
				"test_spec_manifest": "$input.test_spec_manifest",
				"test_spec_summary":  "$input.test_spec_summary",
			}, false),

			// Integration rework: seam problems route to the LINEAR implementation
			// step as unified rework (re-entering the parallel stage would require
			// branch-instance resets — deliberately out of scope for v1). The spec
			// fields flow via pass-through: integration_review carries them as
			// optional inputs fed by e-integration-approve? No — atd runs AFTER
			// integration in the batched path, so the spec does not exist yet and
			// implementation's spec inputs stay empty (contract: batched-path
			// acceptance baseline is produced post-integration, same as linear).
			edge("e-integration-rework", "integration_review", "implementation", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{
				"review_comments":               "$output.comments",
				"previous_pr":                   "$input.branch_reports",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
				// vNext spec pass-through: empty on first entry (spec is designed
				// after integration in the batched path), but if implementation was
				// reworked before and a spec exists, the loop must not lose it.
				"test_spec_doc":      "$input.test_spec_doc",
				"test_spec_manifest": "$input.test_spec_manifest",
				"test_spec_summary":  "$input.test_spec_summary",
			}, false),
			edge("e-atd-impl", "acceptance_test_design", "implementation", "", nil, map[string]string{
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
				"test_spec_doc":                 "$output.test_spec_doc",
				"test_spec_manifest":            "$output.test_spec_manifest",
				"test_spec_summary":             "$output.test_spec_summary",
			}, true),
			edge("e-design-rework", "design_review", "requirement_draft", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{"review_comments": "$output.comments", "previous_draft": "$input.requirement_draft"}, false),
			edge("e-impl-self-review", "implementation", "self_review", "", nil, map[string]string{
				"pr":                            "$output.pr",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
				"test_implementation_evidence":  "$output.test_implementation_evidence",
			}, true),
			edge("e-self-review-pass", "self_review", "code_review", "", nil, map[string]string{
				"pr":                            "$input.pr",
				"review_comments":               "$output.review_comments",
				"design_conformance":            "$output.design_conformance",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waiver_reason":          "$input.design_waiver_reason",
				"design_waived":                 "$input.design_waived",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
				"test_implementation_evidence":  "$input.test_implementation_evidence",
			}, true),
			// Rework 1: self_review -> implementation
			edge("e-self-review-rework", "self_review", "implementation", text["changesRequested"], cond("self_review_verdict", "eq", "issues_fixed"), map[string]string{
				"review_comments":               "$output.review_comments",
				"previous_pr":                   "$input.pr",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
			}, false),
			// Code review approve routes to pre-merge QA
			edge("e-code-review-qa", "code_review", "qa", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"pr":                            "$input.pr",
				"approved_change":               "$output.approved_change",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
				"test_implementation_evidence":  "$input.test_implementation_evidence",
			}, false),
			// Rework 2: code_review -> implementation
			edge("e-code-review-rework", "code_review", "implementation", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{
				"review_comments":               "$output.comments",
				"previous_pr":                   "$input.pr",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
			}, false),
			// QA -> QA Sign-off
			edge("e-qa-to-signoff", "qa", "qa_signoff", "", nil, map[string]string{
				"risk_coverage_matrix":          "$output.risk_coverage_matrix",
				"test_report":                   "$output.test_report",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"design_waived":                 "$input.design_waived",
				"pr":                            "$input.pr",
				"approved_change":               "$input.approved_change",
				"approved_requirement":          "$input.approved_requirement",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
				"test_implementation_evidence":  "$input.test_implementation_evidence",
			}, true),
			// Rework 3: qa_signoff -> implementation
			edge("e-qa-rework", "qa_signoff", "implementation", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{
				"review_comments":               "$output.comments",
				"qa_rework_items":               "$output.qa_rework_items",
				"previous_pr":                   "$input.pr",
				"approved_requirement":          "$input.approved_requirement",
				"approved_design_source":        "$input.approved_design_source",
				"approved_design_project_id":    "$input.approved_design_project_id",
				"approved_design_preview_url":   "$input.approved_design_preview_url",
				"approved_design_html":          "$input.approved_design_html",
				"approved_design_snapshot_path": "$input.approved_design_snapshot_path",
				"test_spec_doc":                 "$input.test_spec_doc",
				"test_spec_manifest":            "$input.test_spec_manifest",
				"test_spec_summary":             "$input.test_spec_summary",
			}, false),
			// QA Sign-off approve routes to PR Open & Merge (Main branch protected!)
			edge("e-qa-signoff-approve", "qa_signoff", "pr_open_and_merge", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"approved_change": "$input.approved_change",
				"pr":              "$input.pr",
			}, false),
			edge("e-pr-merge-release", "pr_open_and_merge", "release", "", nil, map[string]string{"merged_sha": "$output.merged_sha", "pr_url": "$output.pr_url"}, true),
			edge("e-release-go-live", "release", "go_live_confirm", "", nil, map[string]string{"tag": "$output.tag", "deployed_version": "$output.deployed_version", "health_status": "$output.health_status"}, true),
		})
}
