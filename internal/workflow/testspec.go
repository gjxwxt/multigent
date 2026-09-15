// Acceptance test specification manifest validation (greenfield vNext,
// acceptance-test-design-plan §6.1): structural checks only — the platform
// guarantees the manifest parses, cases are unique and every entry carries
// its AC reference, risk level, automation level and an observable expected
// result. Business judgment stays with the QA agent and the human sign-off.
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TestSpecCase is one entry of the test_spec_manifest JSON array.
type TestSpecCase struct {
	CaseID          string `json:"case_id"`
	ACID            string `json:"ac_id"`
	RiskLevel       string `json:"risk_level"`
	AutomationLevel string `json:"automation_level"`
	ExecutionType   string `json:"execution_type"`
	ExpectedResult  string `json:"expected_result"`
}

// validRiskLevels / validAutomationLevels / validExecutionTypes mirror the
// template contract in greenfield.go (§4.3 of the design plan).
var (
	validRiskLevels       = map[string]bool{"high": true, "medium": true, "low": true}
	validAutomationLevels = map[string]bool{"unit": true, "api_integration": true, "ui_e2e": true, "manual": true}
	validExecutionTypes   = map[string]bool{"auto": true, "manual": true, "environment_blocked": true}
)

// ValidateTestSpecManifest parses and structurally validates a
// test_spec_manifest payload. An empty/blank payload fails (the step must
// always produce a spec); a non-array or malformed entries fail; duplicate
// case IDs fail; every entry must carry ac_id, a known risk_level, a known
// automation_level, a known execution_type and a non-empty expected_result
// that is not a placeholder ("to be observed"/"视情况"/"待观察" rejected).
func ValidateTestSpecManifest(raw string) ([]TestSpecCase, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("test_spec_manifest is empty")
	}
	var cases []TestSpecCase
	if err := json.Unmarshal([]byte(trimmed), &cases); err != nil {
		return nil, fmt.Errorf("test_spec_manifest is not a valid JSON array: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("test_spec_manifest has no cases")
	}
	if len(cases) > 500 {
		return nil, fmt.Errorf("test_spec_manifest exceeds 500 cases")
	}
	seen := make(map[string]int, len(cases))
	for i, c := range cases {
		where := fmt.Sprintf("case %d", i+1)
		if strings.TrimSpace(c.CaseID) == "" {
			return nil, fmt.Errorf("%s: case_id is required", where)
		}
		if prev, dup := seen[c.CaseID]; dup {
			return nil, fmt.Errorf("%s (%q): duplicate case_id (first seen at case %d)", where, c.CaseID, prev+1)
		}
		seen[c.CaseID] = i
		if strings.TrimSpace(c.ACID) == "" {
			return nil, fmt.Errorf("%s (%q): ac_id is required — link every case to an acceptance criterion or requirement section", where, c.CaseID)
		}
		if !validRiskLevels[c.RiskLevel] {
			return nil, fmt.Errorf("%s (%q): risk_level must be high|medium|low, got %q", where, c.CaseID, c.RiskLevel)
		}
		if !validAutomationLevels[c.AutomationLevel] {
			return nil, fmt.Errorf("%s (%q): automation_level must be unit|api_integration|ui_e2e|manual, got %q", where, c.CaseID, c.AutomationLevel)
		}
		if !validExecutionTypes[c.ExecutionType] {
			return nil, fmt.Errorf("%s (%q): execution_type must be auto|manual|environment_blocked, got %q", where, c.CaseID, c.ExecutionType)
		}
		if err := validateExpectedResult(where, c.CaseID, c.ExpectedResult); err != nil {
			return nil, err
		}
	}
	return cases, nil
}

// placeholderExpectedResults are phrases that describe observation intent,
// not an assertable outcome (§4.3: 禁止以"待观察""视情况"充当预期结果).
var placeholderExpectedResults = []string{
	"待观察", "视情况", "待定", "观察", "待确认",
	"to be observed", "as appropriate", "as needed", "to be decided", "tbd",
}

func validateExpectedResult(where, caseID, result string) error {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return fmt.Errorf("%s (%q): expected_result is required and must be observable/assertable", where, caseID)
	}
	lower := strings.ToLower(trimmed)
	for _, p := range placeholderExpectedResults {
		if strings.Contains(lower, strings.ToLower(p)) {
			return fmt.Errorf("%s (%q): expected_result contains placeholder %q — state an observable, assertable outcome instead", where, caseID, p)
		}
	}
	return nil
}
