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
	const definitionVersion = 1
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
		StartStepID: "readonly_scan",
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
		"description": "5-step controlled readiness pipeline for existing repositories: read-only scan, baseline pin & audit, sandbox verification, readiness evaluation & contract synthesis, human signoff, and sandbox contract materialization.",

		"scanTitle":   "Read-Only Scan",
		"scanDesc":    "Pure read-only scan of the repository: detect tech stacks, package managers, lockfiles (package-lock.json, go.sum, Cargo.lock, etc.), scripts, CI configs and container files. Strictly forbidden to modify any repository files.",
		"baseTitle":   "Baseline Pin & Audit",
		"baseDesc":    "Pin repository baseline: record HEAD commit SHA as immutable base_commit; run git status to audit worktree cleanliness and ensure no leaked credentials. Security invariant: never push or modify main/master directly.",
		"verifyTitle": "Minimal Verification",
		"verifyDesc":  "Execute bounded dependency installation and build verification inside isolated Docker sandbox (using recommended profile: base or jvm21). Strictly forbidden to execute build scripts directly on the host.",
		"evalTitle":   "Readiness Evaluation",
		"evalDesc":    "Server-side deterministic readiness gate: inspect lockfile determinism (blocking on missing lockfiles, warning on unhashed requirements.txt), verify wrapper permissions, and synthesize valid .multigent/runtime.json contract.",
		"signoffTitle": "Human Onboarding Signoff",
		"signoffDesc":  "Human project owner reviews readiness report, warnings, and synthesized runtime contract; confirms environment parameters and approves onboarding.",
		"matTitle":    "Materialize Contract & Grant Access",
		"matDesc":     "In sandbox, materialize the approved .multigent/runtime.json contract, commit chore: establish multigent runtime contract, and record the ready baseline commit. Project is now marked READY for delivery tasks.",

		"requestField":        "Onboarding mode, repo path, and target branch requirements.",
		"scanReportField":     "Structured JSON scan report of tech stacks, lockfiles, and configs.",
		"baseCommitField":     "Immutable HEAD commit SHA before onboarding.",
		"baseStatusField":     "Cleanliness and credential hygiene audit report.",
		"verifyLogField":      "Execution logs of install, test, and build in sandbox.",
		"buildExitCodeField":  "Exit code of verification commands.",
		"readinessRepField":   "Readiness evaluation report with categorized issues (blocking vs warning).",
		"synthRuntimeField":   "Synthesized .multigent/runtime.json contract draft.",
		"decisionField":       "approve or request_changes.",
		"commentsField":       "Review comments and guidance.",
		"apprRuntimeField":    "Approved .multigent/runtime.json contract content.",
		"readyCommitField":    "Commit SHA establishing the onboarded runtime contract.",
		"onboardStatusField":  "Final onboarding status: ready or not_ready.",
	}, map[string]string{
		"name":        "存量工程接入就绪流水线",
		"description": "企业存量仓库（Brownfield）5步受控就绪流水线：只读探测、基线锁定与审计、沙箱最小验证、就绪评估与契约推导、人工签核、沙箱契约固化。",

		"scanTitle":   "只读探测",
		"scanDesc":    "对仓库目录执行零副作用的只读探测：识别多语言技术栈、服务目录、包管理器及确定性依赖锁文件、现有构建脚本、CI/CD 与容器文件。严禁修改仓库任何文件。",
		"baseTitle":   "基线固定与审计",
		"baseDesc":    "锁定当前仓库基线：读取当前 HEAD Commit SHA 并记录为不可变 base_commit；审计工作区纯净度与密钥安全。安全红线：严禁在 main/master 默认分支直接修改或推送。",
		"verifyTitle": "依赖安装与最小验证",
		"verifyDesc":  "在 Docker 沙箱内执行有界依赖安装与构建测试验证（采用探测推荐的 base 或 jvm21 镜像）。严禁在控制面宿主机直接执行项目构建脚本。",
		"evalTitle":   "就绪评估与契约推导",
		"evalDesc":    "服务端确定性就绪判定：按分层规则检查锁文件确定性（缺失锁文件判定为 blocking 阻断，Python requirements.txt 标记为 warning 告警），推导并生成合法的 .multigent/runtime.json 运行时契约草案。",
		"signoffTitle": "人工准入签核",
		"signoffDesc":  "人工负责人复核存量仓库就绪报告、告警项与推导出的运行时契约；核验环境变量与探针路径，确认批准准入或要求整改。",
		"matTitle":    "固化契约并授予权限",
		"matDesc":     "在 Docker 沙箱内安全写入经审批的 .multigent/runtime.json 契约，提交 commit，记录新的已纳管基线 commit。项目正式标记为 READY，开放后续业务任务协作权限。",

		"requestField":        "存量仓库路径、初始化模式与目标分支参数。",
		"scanReportField":     "包含技术栈、包管理器、锁文件及配置的只读探测报告 JSON。",
		"baseCommitField":     "接入前不可变的初始 HEAD Commit SHA。",
		"baseStatusField":     "工作区纯净度与无密钥泄漏审计状态。",
		"verifyLogField":      "沙箱内执行安装、测试与构建的完整日志。",
		"buildExitCodeField":  "验证命令退出码。",
		"readinessRepField":   "结构化就绪评估报告（含阻断项与告警项清单）。",
		"synthRuntimeField":   "推导生成的 .multigent/runtime.json 契约草案 JSON。",
		"decisionField":       "approve 或 request_changes 审批裁决。",
		"commentsField":       "审批人审阅意见与指导。",
		"apprRuntimeField":    "经人工核准确认的 .multigent/runtime.json 契约内容。",
		"readyCommitField":    "固化运行时契约后的正式纳管 Commit SHA。",
		"onboardStatusField":  "最终存量接入就绪状态：ready 或 not_ready。",
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
		step("readonly_scan", "agent_task", "scanTitle", "scanDesc", "project-initializer", "sky", 80,
			[]entity.WorkflowField{{Name: "onboarding_request", Description: text["requestField"]}},
			[]entity.WorkflowField{{Name: "scan_report", Description: text["scanReportField"]}},
		),
		step("baseline_pin", "agent_task", "baseTitle", "baseDesc", "project-initializer", "sky", 380,
			[]entity.WorkflowField{{Name: "scan_report", Description: text["scanReportField"]}},
			[]entity.WorkflowField{
				{Name: "base_commit", Description: text["baseCommitField"]},
				{Name: "baseline_status", Description: text["baseStatusField"]},
			},
		),
		step("verify_build", "agent_task", "verifyTitle", "verifyDesc", "project-initializer", "amber", 680,
			[]entity.WorkflowField{
				{Name: "scan_report", Description: text["scanReportField"]},
				{Name: "base_commit", Description: text["baseCommitField"]},
			},
			[]entity.WorkflowField{
				{Name: "verification_log", Description: text["verifyLogField"]},
				{Name: "build_exit_code", Description: text["buildExitCodeField"]},
			},
		),
		step("evaluate_readiness", "agent_task", "evalTitle", "evalDesc", "project-initializer", "indigo", 980,
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
		step("human_signoff", "human_review", "signoffTitle", "signoffDesc", "owner-engineer", "emerald", 1280,
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
		step("materialize_contract", "agent_task", "matTitle", "matDesc", "project-initializer", "sky", 1580,
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
		"readonly_scan",
		steps,
		edges,
	)
}
