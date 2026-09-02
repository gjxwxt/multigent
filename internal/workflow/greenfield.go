package workflow

import (
	"github.com/multigent/multigent/internal/entity"
)

// greenfieldDeliveryTemplate is the greenfield-only delivery pipeline: for a
// project's first delivery there is no existing UI, so a visual design gate
// (OpenDesign, human_review with Config designGate=true) sits between the
// quick requirement review and implementation. Existing projects keep using
// the unified pipeline untouched. See docs/opendesign-integration-plan.md §9.
func greenfieldDeliveryTemplate(locale string) entity.WorkflowTemplate {
	locale = normalizeTemplateLocale(locale)
	text := localizedTemplateText(locale, map[string]string{
		"name":     "Greenfield Delivery Pipeline",
		"description": "First-delivery pipeline for brand-new projects: requirement clarification, a visual design gate powered by OpenDesign, then implementation, agent self-review, human code review, PR merge, first release, and go-live confirmation.",
		"approved":         "approved",
		"changesRequested": "changes requested",

		"reqDraftTitle":   "Requirement Clarification",
		"reqDraftDesc":    "An agent turns the initial request into a structured requirement: problem, goal, scope, non-goals, risks, and open questions.",
		"reqReviewTitle":  "Requirement Quick Review",
		"reqReviewDesc":   "A human quickly confirms the requirement draft is aiming at the right thing before any design or code work starts.",
		"designTitle":     "Design Confirmation",
		"designDesc":      "Review the visual prototype produced in OpenDesign (or pick an existing design). Confirm the direction, request changes, or generate a new variant before implementation begins.",
		"implTitle":       "Implementation",
		"implDesc":        "Implement the approved requirement following the approved design references. Record the PR, tests executed, and any risks.",
		"selfReviewTitle": "Agent Self Review",
		"selfReviewDesc":  "An independent reviewer agent inspects the implementation against the requirement and design, requesting fixes up to three rounds.",
		"codeReviewTitle": "Human Code Review",
		"codeReviewDesc":  "A human reviews the implementation evidence and decides whether it can proceed to PR and merge.",
		"prMergeTitle":    "Open PR and Merge",
		"prMergeDesc":     "Open the merge request, wait for CI to pass, then merge. Record the merged SHA and PR URL.",
		"releaseTitle":    "First Release",
		"releaseDesc":     "Tag the first version, deploy, and probe the health endpoint. Record the tag, deployed version, and health status.",
		"goLiveTitle":     "Go-Live Confirmation",
		"goLiveDesc":      "A human verifies the live deployment meets expectations and formally accepts the first delivery.",

		"requestField":       "Original request and business context for the new project.",
		"contextField":       "Known background, constraints, and references.",
		"decisionField":      "approve or request_changes.",
		"commentsField":      "Review comments and guidance.",
		"designSourceField":  "Optional. existing (pick a listed design) or generated (created from the requirement). Empty on rework.",
		"designProjectField": "Optional. OpenDesign project id backing the approved design.",
		"designPreviewField": "Optional. Preview URL of the approved design for downstream reference.",
		"prField":            "PR or patch summary produced by implementation.",
		"testsField":         "Tests executed with evidence.",
		"risksField":         "Known risks and follow-ups.",
		"reviewCommentsField":"Findings from the agent self review.",
		"mergedField":        "Merged commit SHA on the integration branch.",
		"prURLField":         "URL of the opened merge request.",
		"tagField":           "Release tag (e.g. v0.1.0).",
		"deployedField":      "Deployed version identifier.",
		"healthField":        "Health probe result after deployment.",
	}, map[string]string{
		"name":     "Greenfield 交付流水线",
		"description": "新项目首次交付专用流水线：需求澄清后先经 OpenDesign 设计确认闸门，再实现编码、Agent 初审、人工代码审核、开 PR 合并、首版发布与上线确认。",
		"approved":         "通过",
		"changesRequested": "需要修改",

		"reqDraftTitle":   "需求澄清",
		"reqDraftDesc":    "Agent 把初始请求整理成结构化需求：问题、目标、范围、非目标、风险与待澄清问题。",
		"reqReviewTitle":  "需求快审",
		"reqReviewDesc":   "在设计或编码开始前，人工快速确认需求方向正确。",
		"designTitle":     "设计确认",
		"designDesc":      "审阅 OpenDesign 生成的可视化原型（或选择已有设计），确认方向、打回修改或重新生成，然后再进入实现。",
		"implTitle":       "实现编码",
		"implDesc":        "以已确认的设计产物为参考实现需求。记录 PR、执行的测试与风险。",
		"selfReviewTitle": "Agent 初审",
		"selfReviewDesc":  "独立 reviewer agent 依据需求与设计检查实现，最多三轮返工。",
		"codeReviewTitle": "人工代码审核",
		"codeReviewDesc":  "人工审核实现证据，决定是否进入开 PR 与合并。",
		"prMergeTitle":    "开 PR 并合并",
		"prMergeDesc":     "开合并请求，等待 CI 通过后合并。记录合并 SHA 与 PR 地址。",
		"releaseTitle":    "首版发布",
		"releaseDesc":     "打首版 tag、部署并探测健康检查端点。记录 tag、部署版本与健康状态。",
		"goLiveTitle":     "上线确认",
		"goLiveDesc":      "人工验收线上部署效果，正式确认首次交付完成。",

		"requestField":       "新项目的原始请求与业务背景。",
		"contextField":       "已知背景、约束与参考资料。",
		"decisionField":      "approve 或 request_changes。",
		"commentsField":      "审核意见与指导。",
		"designSourceField":  "可选。existing（选择已有设计）或 generated（由需求生成）。打回时留空。",
		"designProjectField": "可选。支撑已确认设计的 OpenDesign 项目 ID。",
		"designPreviewField": "可选。已确认设计的预览地址，供下游参考。",
		"prField":            "实现产出的 PR 或补丁摘要。",
		"testsField":         "已执行的测试与证据。",
		"risksField":         "已知风险与后续事项。",
		"reviewCommentsField":"Agent 初审发现的问题。",
		"mergedField":        "集成分支上的合并提交 SHA。",
		"prURLField":         "合并请求地址。",
		"tagField":           "发布 tag（如 v0.1.0）。",
		"deployedField":      "部署版本标识。",
		"healthField":        "部署后的健康探测结果。",
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
		[]entity.WorkflowField{field("requirement_draft", "requestField")},
		[]entity.WorkflowField{
			field("decision", "decisionField"),
			field("comments", "commentsField"),
			optionalField("approved_design_source", "designSourceField"),
			optionalField("approved_design_project_id", "designProjectField"),
			optionalField("approved_design_preview_url", "designPreviewField"),
		})
	if designStep.Config == nil {
		designStep.Config = map[string]string{}
	}
	designStep.Config["designGate"] = "true"

	return templateFromParts("greenfield-delivery-pipeline", text["name"], text["description"], locale, "requirement_draft",
		[]entity.WorkflowStep{
			tmplStep("requirement_draft", "agent_task", text["reqDraftTitle"], text["reqDraftDesc"], "pm-agent", "sky", 80,
				[]entity.WorkflowField{field("request", "requestField"), field("context", "contextField")},
				[]entity.WorkflowField{field("requirement_draft", "requestField"), field("open_questions", "contextField")}),
			tmplStep("requirement_review", "human_review", text["reqReviewTitle"], text["reqReviewDesc"], "product-owner", "amber", 360,
				[]entity.WorkflowField{field("requirement_draft", "requestField")},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField"), optionalField("approved_requirement", "requestField")}),
			designStep,
			tmplStep("implementation", "agent_task", text["implTitle"], text["implDesc"], "owner-engineer", "emerald", 920,
				[]entity.WorkflowField{
					field("approved_requirement", "requestField"),
					optionalField("approved_design_source", "designSourceField"),
					optionalField("approved_design_project_id", "designProjectField"),
					optionalField("approved_design_preview_url", "designPreviewField"),
				},
				[]entity.WorkflowField{field("pr", "prField"), field("tests_run", "testsField"), field("risks", "risksField")}),
			tmplStep("self_review", "agent_task", text["selfReviewTitle"], text["selfReviewDesc"], "owner-engineer", "rose", 1200,
				[]entity.WorkflowField{field("pr", "prField"), field("approved_requirement", "requestField")},
				[]entity.WorkflowField{field("review_comments", "reviewCommentsField")}),
			tmplStep("code_review", "human_review", text["codeReviewTitle"], text["codeReviewDesc"], "owner-engineer", "amber", 1480,
				[]entity.WorkflowField{field("pr", "prField"), field("review_comments", "reviewCommentsField")},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField"), optionalField("approved_change", "prField")}),
			tmplStep("pr_open_and_merge", "agent_task", text["prMergeTitle"], text["prMergeDesc"], "owner-engineer", "violet", 1760,
				[]entity.WorkflowField{field("approved_change", "prField")},
				[]entity.WorkflowField{field("merged_sha", "mergedField"), field("pr_url", "prURLField")}),
			tmplStep("release", "agent_task", text["releaseTitle"], text["releaseDesc"], "owner-engineer", "emerald", 2040,
				[]entity.WorkflowField{field("merged_sha", "mergedField")},
				[]entity.WorkflowField{field("tag", "tagField"), field("deployed_version", "deployedField"), field("health_status", "healthField")}),
			tmplStep("go_live_confirm", "human_review", text["goLiveTitle"], text["goLiveDesc"], "product-owner", "amber", 2320,
				[]entity.WorkflowField{field("tag", "tagField"), field("deployed_version", "deployedField"), field("health_status", "healthField")},
				[]entity.WorkflowField{field("decision", "decisionField"), field("comments", "commentsField")}),
		},
		[]entity.WorkflowEdge{
			edge("e-req-draft-review", "requirement_draft", "requirement_review", "", nil, nil, true),
			edge("e-req-review-design", "requirement_review", "design_review", text["approved"], cond("decision", "eq", "approve"), map[string]string{"approved_requirement": "$output.approved_requirement"}, false),
			edge("e-req-review-rework", "requirement_review", "requirement_draft", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{"review_comments": "$output.comments", "previous_draft": "$input.requirement_draft"}, false),
			edge("e-design-approve", "design_review", "implementation", text["approved"], cond("decision", "eq", "approve"), map[string]string{
				"approved_design_source":      "$output.approved_design_source",
				"approved_design_project_id":  "$output.approved_design_project_id",
				"approved_design_preview_url": "$output.approved_design_preview_url",
			}, false),
			edge("e-design-rework", "design_review", "requirement_draft", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{"review_comments": "$output.comments", "previous_draft": "$input.requirement_draft"}, false),
			edge("e-impl-self-review", "implementation", "self_review", "", nil, nil, true),
			edge("e-self-review-pass", "self_review", "code_review", "", nil, map[string]string{"pr": "$input.pr", "review_comments": "$output.review_comments"}, true),
			edge("e-self-review-rework", "self_review", "implementation", text["changesRequested"], cond("review_comments", "neq", ""), map[string]string{"review_comments": "$output.review_comments", "previous_pr": "$input.pr"}, false),
			edge("e-code-review-approve", "code_review", "pr_open_and_merge", text["approved"], cond("decision", "eq", "approve"), map[string]string{"approved_change": "$output.approved_change"}, false),
			edge("e-code-review-rework", "code_review", "implementation", text["changesRequested"], cond("decision", "eq", "request_changes"), map[string]string{"review_comments": "$output.comments", "previous_pr": "$input.pr"}, false),
			edge("e-pr-merge-release", "pr_open_and_merge", "release", "", nil, map[string]string{"merged_sha": "$output.merged_sha", "pr_url": "$output.pr_url"}, true),
			edge("e-release-go-live", "release", "go_live_confirm", "", nil, map[string]string{"tag": "$output.tag", "deployed_version": "$output.deployed_version", "health_status": "$output.health_status"}, true),
		})
}
