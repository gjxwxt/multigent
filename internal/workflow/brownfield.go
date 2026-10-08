package workflow

import (
	"time"

	"github.com/multigent/multigent/internal/entity"
)

const BrownfieldOnboardingWorkflowID = "brownfield-onboarding-v1"

// EnsureBrownfieldOnboardingDefinition installs the fixed 5+1 step Brownfield
// onboarding pipeline and keeps it current. The pipeline enforces controlled,
// read-only scanning, containerized minimal verification, deterministic
// readiness evaluation, human signoff, and sandbox contract materialization.
func (s *Store) EnsureBrownfieldOnboardingDefinition() error {
	// definitionVersion history: v1 shipped the 6-step scan→materialize
	// pipeline; v2 prepends the preflight clarify gate (preflight +
	// preflight_review, StartStepID moves to preflight). The bump lets this
	// ensure upgrade already-seeded v1 copies in place; runs started on v1
	// snapshots keep executing their frozen definition unchanged.
	const definitionVersion = 2
	if existing, ok, err := s.Definition(BrownfieldOnboardingWorkflowID); err != nil {
		return err
	} else if ok && existing.Version >= definitionVersion {
		return nil
	}

	now := time.Now().UTC()
	tmpl := brownfieldOnboardingTemplate("zh-CN")

	def := entity.WorkflowDefinition{
		ID:          BrownfieldOnboardingWorkflowID,
		Name:        tmpl.Name,
		Description: tmpl.Description,
		Version:     definitionVersion,
		Scope:       "workspace",
		StartStepID: "preflight",
		Steps:       tmpl.Steps,
		Edges:       tmpl.Edges,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.SaveDefinition(&def)
}

func brownfieldOnboardingTemplate(locale string) entity.WorkflowTemplate {
	locale = normalizeTemplateLocale(locale)
	text := localizedTemplateText(locale, map[string]string{
		"name":        "Brownfield Repository Onboarding",
		"description": "Preflight clarify gate, then a controlled readiness pipeline for existing repositories: read-only scan, baseline pin & audit, sandbox verification, readiness evaluation & contract synthesis, human signoff, and sandbox contract materialization.",

		"preflightTitle":               "Preflight Check",
		"preflightDesc":                "Read-only preflight check before onboarding starts: inspect the repository as-is (tech stacks, lockfiles, git status/baseline, whether a .multigent/runtime.json contract already exists, whether the repo is already onboarded) and confirm the onboarding parameters per question (target branch, verification profile choice, etc.). Write the structured check conclusions to preflight_report. When parameters are undecided or something needs a ruling, ask the user per question via open_questions (e.g. \"which branch should onboarding pin as the baseline?\"); when everything is settled output open_questions exactly as the single token [] — the platform surfaces the freeze button to the reviewer only on that declaration. When clarifications is present this is a rework entry: apply every conclusion and re-emit the report.",
		"preflightReviewTitle":         "Preflight Review",
		"preflightReviewDesc":          "Per-question preflight clarification gate (same interaction as the requirement quick review). Each open question gets exactly one default action: write a conclusion (a low-frequency \"defer with reason\" link is collapsed per card). Submitting is the ONLY exit of a round: the conclusions flow back to the preflight agent, which applies them and re-emits the report. The approve/freeze button appears only after a re-run reports open_questions empty — onboarding with undecided parameters can never reach the read-only scan.",
		"preflightReportField":         "Structured preflight check conclusions: repo state (tech stacks, lockfiles, git baseline, existing .multigent/runtime.json contract, onboarding status) and the confirmed onboarding parameters (target branch, verification profile).",
		"preflightClarificationsField": "Structured per-question conclusions (JSON array): [{id, priority, category, question, status (answered|deferred), conclusion, deferred_reason}]. Submitted each round; the agent MUST apply every answered conclusion and re-emit the report keeping only genuinely pending items as open_questions.",
		"openQuestionsField":           "JSON array of the questions still pending a human ruling, one entry per question (id/priority/category/question/blocking_reason). Output the single token [] exactly when nothing is pending.",

		"scanTitle":    "Read-Only Scan",
		"scanDesc":     "Pure read-only scan of the repository: detect tech stacks, package managers, lockfiles (package-lock.json, go.sum, Cargo.lock, etc.), scripts, CI configs and container files. Strictly forbidden to modify any repository files.",
		"baseTitle":    "Baseline Pin & Audit",
		"baseDesc":     "Pin repository baseline: record HEAD commit SHA as immutable base_commit; run git status to audit worktree cleanliness and ensure no leaked credentials. Security invariant: never push or modify main/master directly.",
		"verifyTitle":  "Minimal Verification",
		"verifyDesc":   "Execute bounded dependency installation and build verification inside isolated Docker sandbox (using recommended profile: base or jvm21). Strictly forbidden to execute build scripts directly on the host.",
		"evalTitle":    "Readiness Evaluation",
		"evalDesc":     "Server-side deterministic readiness gate: inspect lockfile determinism (blocking on missing lockfiles, warning on unhashed requirements.txt), verify wrapper permissions, and synthesize valid .multigent/runtime.json contract.",
		"signoffTitle": "Human Onboarding Signoff",
		"signoffDesc":  "Human project owner reviews readiness report, warnings, and synthesized runtime contract; confirms environment parameters and approves onboarding.",
		"matTitle":     "Materialize Contract & Grant Access",
		"matDesc":      "In sandbox, materialize the approved .multigent/runtime.json contract, commit chore: establish multigent runtime contract, and record the ready baseline commit. Project is now marked READY for delivery tasks.",

		"requestField":       "Onboarding mode, repo path, and target branch requirements.",
		"scanReportField":    "Structured JSON scan report of tech stacks, lockfiles, and configs.",
		"baseCommitField":    "Immutable HEAD commit SHA before onboarding.",
		"baseStatusField":    "Cleanliness and credential hygiene audit report.",
		"verifyLogField":     "Execution logs of install, test, and build in sandbox.",
		"buildExitCodeField": "Exit code of verification commands.",
		"readinessRepField":  "Readiness evaluation report with categorized issues (blocking vs warning).",
		"synthRuntimeField":  "Synthesized .multigent/runtime.json contract draft.",
		"decisionField":      "approve or request_changes.",
		"commentsField":      "Review comments and guidance.",
		"apprRuntimeField":   "Approved .multigent/runtime.json contract content.",
		"readyCommitField":   "Commit SHA establishing the onboarded runtime contract.",
		"onboardStatusField": "Final onboarding status: ready or not_ready.",
	}, map[string]string{
		"name":        "存量工程接入就绪流水线",
		"description": "企业存量仓库（Brownfield）开工前逐题澄清闸门 + 受控就绪流水线：开工前检查、只读探测、基线锁定与审计、沙箱最小验证、就绪评估与契约推导、人工签核、沙箱契约固化。",

		"preflightTitle":               "开工前检查",
		"preflightDesc":                "接入开工前的只读检查：核对企业存量仓库现状（技术栈、锁文件、Git 状态/基线、是否已有 .multigent/runtime.json 契约、是否已纳管），并逐题确认接入参数（目标分支、验证 profile 选择等）。结构化检查结论写入 preflight_report。参数未定或需要裁决时，通过 open_questions 逐题向用户提问（如「接入基线应固定到哪个分支？」）；一切确认完毕时 open_questions 必须精确输出单词元 []——平台只有在收到该申报后才会向审核人展示「确认冻结」按钮。当 clarifications 存在时，本次是返工进入：应用每条结论后重新提交报告。",
		"preflightReviewTitle":         "开工前审核",
		"preflightReviewDesc":          "逐题开工前澄清闸门（与需求快审同一交互模型）。每个待澄清问题只有一个默认动作：写结论（低频出口「本轮不答」折叠在卡片角落，需留原因）。提交是每一轮的唯一出口：结论回流给开工前检查 agent，agent 应用结论并重新提交报告。只有当重跑后申报 open_questions 已清零时，才会出现「确认冻结」按钮——参数未定的接入永远到不了只读探测。",
		"preflightReportField":         "结构化开工前检查结论：仓库现状（技术栈、锁文件、Git 基线、已有 .multigent/runtime.json 契约、纳管状态）与已确认的接入参数（目标分支、验证 profile）。",
		"preflightClarificationsField": "逐题结论结构化数组（JSON）：[{id, priority, category, question, status（answered|deferred）, conclusion, deferred_reason}]。每轮提交；agent 必须应用每条已答复结论并重新提交报告，只保留真正待定项为 open_questions。",
		"openQuestionsField":           "仍待人工裁决的问题 JSON 数组，每题一项（id/priority/category/question/blocking_reason）。无待定项时必须精确输出单词元 []。",

		"scanTitle":    "只读探测",
		"scanDesc":     "对仓库目录执行零副作用的只读探测：识别多语言技术栈、服务目录、包管理器及确定性依赖锁文件、现有构建脚本、CI/CD 与容器文件。严禁修改仓库任何文件。",
		"baseTitle":    "基线固定与审计",
		"baseDesc":     "锁定当前仓库基线：读取当前 HEAD Commit SHA 并记录为不可变 base_commit；审计工作区纯净度与密钥安全。安全红线：严禁在 main/master 默认分支直接修改或推送。",
		"verifyTitle":  "依赖安装与最小验证",
		"verifyDesc":   "在 Docker 沙箱内执行有界依赖安装与构建测试验证（采用探测推荐的 base 或 jvm21 镜像）。严禁在控制面宿主机直接执行项目构建脚本。",
		"evalTitle":    "就绪评估与契约推导",
		"evalDesc":     "服务端确定性就绪判定：按分层规则检查锁文件确定性（缺失锁文件判定为 blocking 阻断，Python requirements.txt 标记为 warning 告警），推导并生成合法的 .multigent/runtime.json 运行时契约草案。",
		"signoffTitle": "人工准入签核",
		"signoffDesc":  "人工负责人复核存量仓库就绪报告、告警项与推导出的运行时契约；核验环境变量与探针路径，确认批准准入或要求整改。",
		"matTitle":     "固化契约并授予权限",
		"matDesc":      "在 Docker 沙箱内安全写入经审批的 .multigent/runtime.json 契约，提交 commit，记录新的已纳管基线 commit。项目正式标记为 READY，开放后续业务任务协作权限。",

		"requestField":       "存量仓库路径、初始化模式与目标分支参数。",
		"scanReportField":    "包含技术栈、包管理器、锁文件及配置的只读探测报告 JSON。",
		"baseCommitField":    "接入前不可变的初始 HEAD Commit SHA。",
		"baseStatusField":    "工作区纯净度与无密钥泄漏审计状态。",
		"verifyLogField":     "沙箱内执行安装、测试与构建的完整日志。",
		"buildExitCodeField": "验证命令退出码。",
		"readinessRepField":  "结构化就绪评估报告（含阻断项与告警项清单）。",
		"synthRuntimeField":  "推导生成的 .multigent/runtime.json 契约草案 JSON。",
		"decisionField":      "approve 或 request_changes 审批裁决。",
		"commentsField":      "审批人审阅意见与指导。",
		"apprRuntimeField":   "经人工核准确认的 .multigent/runtime.json 契约内容。",
		"readyCommitField":   "固化运行时契约后的正式纳管 Commit SHA。",
		"onboardStatusField": "最终存量接入就绪状态：ready 或 not_ready。",
	})

	step := func(id, typ, titleKey, descKey, role, color string, x int, inputs, outputs []entity.WorkflowField) entity.WorkflowStep {
		cfg := map[string]string{}
		if color != "" {
			cfg["color"] = color
		}
		return entity.WorkflowStep{
			ID:           id,
			Type:         typ,
			Title:        text[titleKey],
			Description:  text[descKey],
			ActorRole:    role,
			InputFields:  inputs,
			OutputFields: outputs,
			ReviewPolicy: reviewPolicyForType(typ),
			Position:     entity.WorkflowPosition{X: x, Y: 180},
			Config:       cfg,
		}
	}

	steps := []entity.WorkflowStep{
		// v2 head: preflight readiness/parameter check + per-question human
		// clarify gate, reusing the requirement_review open_questions
		// mechanics (the open_questions input declaration activates the UI
		// cards and the server-side freeze gate). Only existing step types.
		step("preflight", "agent_task", "preflightTitle", "preflightDesc", "project-initializer", "emerald", 80,
			// request comes from the task creation inputs (this is now the
			// start step); clarifications/review_comments arrive from the
			// preflight_review rework edge.
			[]entity.WorkflowField{
				{Name: "request", Description: text["requestField"]},
				{Name: "clarifications", Description: text["preflightClarificationsField"], Optional: true},
				{Name: "review_comments", Description: text["commentsField"], Optional: true},
			},
			[]entity.WorkflowField{
				{Name: "preflight_report", Description: text["preflightReportField"]},
				{Name: "open_questions", Description: text["openQuestionsField"]},
			},
		),
		step("preflight_review", "human_review", "preflightReviewTitle", "preflightReviewDesc", "owner-engineer", "amber", 360,
			// request is declared AND forwarded by the inbound edge mapping so
			// the approve edge can re-emit it into readonly_scan as
			// onboarding_request ($input resolves from THIS instance's
			// materialized inputs). open_questions declares the clarify loop.
			[]entity.WorkflowField{
				{Name: "request", Description: text["requestField"]},
				{Name: "preflight_report", Description: text["preflightReportField"]},
				{Name: "open_questions", Description: text["openQuestionsField"]},
			},
			[]entity.WorkflowField{
				{Name: "decision", Description: text["decisionField"]},
				{Name: "comments", Description: text["commentsField"]},
				{Name: "clarifications", Description: text["preflightClarificationsField"], Optional: true},
			},
		),
		step("readonly_scan", "agent_task", "scanTitle", "scanDesc", "project-initializer", "sky", 640,
			[]entity.WorkflowField{{Name: "onboarding_request", Description: text["requestField"]}},
			[]entity.WorkflowField{{Name: "scan_report", Description: text["scanReportField"]}},
		),
		step("baseline_pin", "agent_task", "baseTitle", "baseDesc", "project-initializer", "sky", 940,
			[]entity.WorkflowField{{Name: "scan_report", Description: text["scanReportField"]}},
			[]entity.WorkflowField{
				{Name: "base_commit", Description: text["baseCommitField"]},
				{Name: "baseline_status", Description: text["baseStatusField"]},
			},
		),
		step("verify_build", "agent_task", "verifyTitle", "verifyDesc", "project-initializer", "amber", 1240,
			[]entity.WorkflowField{
				{Name: "scan_report", Description: text["scanReportField"]},
				{Name: "base_commit", Description: text["baseCommitField"]},
			},
			[]entity.WorkflowField{
				{Name: "verification_log", Description: text["verifyLogField"]},
				{Name: "build_exit_code", Description: text["buildExitCodeField"]},
			},
		),
		step("evaluate_readiness", "agent_task", "evalTitle", "evalDesc", "project-initializer", "indigo", 1540,
			[]entity.WorkflowField{
				{Name: "scan_report", Description: text["scanReportField"]},
				{Name: "base_commit", Description: text["baseCommitField"]},
				{Name: "verification_log", Description: text["verifyLogField"]},
				{Name: "build_exit_code", Description: text["buildExitCodeField"]},
			},
			[]entity.WorkflowField{
				{Name: "readiness_report", Description: text["readinessRepField"]},
				{Name: "synthesized_runtime_json", Description: text["synthRuntimeField"]},
			},
		),
		step("human_signoff", "human_review", "signoffTitle", "signoffDesc", "owner-engineer", "emerald", 1840,
			[]entity.WorkflowField{
				{Name: "readiness_report", Description: text["readinessRepField"]},
				{Name: "synthesized_runtime_json", Description: text["synthRuntimeField"]},
				{Name: "base_commit", Description: text["baseCommitField"]},
			},
			[]entity.WorkflowField{
				{Name: "decision", Description: text["decisionField"]},
				{Name: "comments", Description: text["commentsField"]},
				{Name: "approved_runtime_json", Description: text["apprRuntimeField"], Optional: true},
			},
		),
		step("materialize_contract", "agent_task", "matTitle", "matDesc", "project-initializer", "sky", 2140,
			[]entity.WorkflowField{
				{Name: "approved_runtime_json", Description: text["apprRuntimeField"]},
				{Name: "base_commit", Description: text["baseCommitField"]},
			},
			[]entity.WorkflowField{
				{Name: "ready_commit", Description: text["readyCommitField"]},
				{Name: "onboarding_status", Description: text["onboardStatusField"]},
			},
		),
	}

	edges := []entity.WorkflowEdge{
		// Preflight forward edge: request AND the agent's outputs
		// (preflight_report + open_questions) materialize into the review
		// step's instance inputs — $input.request on the approve edge below
		// resolves from this instance, so the mapping here is what keeps the
		// original request flowing into readonly_scan.
		edge("e-preflight-review", "preflight", "preflight_review", "", nil, map[string]string{
			"request":          "$input.request",
			"preflight_report": "$output.preflight_report",
			"open_questions":   "$output.open_questions",
		}, true),
		// Preflight clarify loop (v2): submitting a round is the only
		// non-freeze exit; the conclusions flow back to the preflight agent.
		// The approve edge forwards the original request into the read-only
		// scan via explicit mapping (readonly_scan is no longer the start
		// step, so it no longer receives the task's initial inputs directly).
		edge("e-preflight-review-rework", "preflight_review", "preflight", "要求整改", cond("decision", "eq", "request_changes"), map[string]string{
			"request":         "$input.request",
			"review_comments": "$output.comments",
			"clarifications":  "$output.clarifications",
		}, false),
		edge("e-preflightreview-scan", "preflight_review", "readonly_scan", "批准准入", cond("decision", "eq", "approve"), map[string]string{
			"onboarding_request": "$input.request",
		}, false),
		edge("e-scan-baseline", "readonly_scan", "baseline_pin", "", nil, nil, true),
		edge("e-baseline-verify", "baseline_pin", "verify_build", "", nil, nil, true),
		edge("e-verify-evaluate", "verify_build", "evaluate_readiness", "", nil, nil, true),
		edge("e-evaluate-signoff", "evaluate_readiness", "human_signoff", "", nil, nil, true),
		edge("e-signoff-materialize", "human_signoff", "materialize_contract", "批准准入", cond("decision", "eq", "approve"), nil, true),
		edge("e-signoff-rework", "human_signoff", "readonly_scan", "要求整改", cond("decision", "eq", "request_changes"), nil, false),
	}

	return templateFromParts(
		"brownfield-onboarding-pipeline",
		text["name"],
		text["description"],
		locale,
		"preflight",
		steps,
		edges,
	)
}
