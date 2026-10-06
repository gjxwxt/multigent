package api

// Tests for the design delta audit contract (docs/
// design-gate-delta-audit-proposal.md v2.6): same-source freeze hash
// validation, report parsing normalizations, and the benchmark-B degraded
// source classification.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Same-source freeze: a payload whose html+hash agree parses; any tampering
// with either side fails closed.
func TestParseDesignDeltaConfirmPayloadHashValidation(t *testing.T) {
	html := "<html><body>quota panel</body></html>"
	decisions := `[{"delta_id":"d1","action":"adopt","note":"keep the panel"}]`
	valid := `{"html":"` + html + `","htmlSha256":"` + sha256Hex(html) + `","fetchedAt":"2026-10-04T00:00:00Z","decisions":` + decisions + `}`

	p, err := ParseDesignDeltaConfirmPayload(valid)
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if p.HTML != html || len(p.Decisions) != 1 || p.Decisions[0].Action != "adopt" {
		t.Fatalf("payload round-trip broken: %+v", p)
	}

	// Hash mismatch = the audited document and the submitted document differ.
	tampered := strings.Replace(valid, sha256Hex(html), sha256Hex("<html>other</html>"), 1)
	if _, err := ParseDesignDeltaConfirmPayload(tampered); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered hash accepted: %v", err)
	}

	// Content mutation without hash update.
	mutated := strings.Replace(valid, "quota panel", "quota CHANGED", 1)
	if _, err := ParseDesignDeltaConfirmPayload(mutated); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("mutated content accepted: %v", err)
	}

	// Missing html (re-fetch path) is rejected: same-source freeze required.
	noHTML := `{"htmlSha256":"` + sha256Hex(html) + `","decisions":` + decisions + `}`
	if _, err := ParseDesignDeltaConfirmPayload(noHTML); err == nil || !strings.Contains(err.Error(), "same-source") {
		t.Fatalf("missing html accepted: %v", err)
	}

	// Missing decisions rejected.
	noDec := `{"html":"` + html + `","htmlSha256":"` + sha256Hex(html) + `"}`
	if _, err := ParseDesignDeltaConfirmPayload(noDec); err == nil || !strings.Contains(err.Error(), "decisions") {
		t.Fatalf("missing decisions accepted: %v", err)
	}

	// Invalid action rejected.
	badAction := `{"html":"` + html + `","htmlSha256":"` + sha256Hex(html) + `","decisions":[{"delta_id":"d1","action":"auto_approve"}]}`
	if _, err := ParseDesignDeltaConfirmPayload(badAction); err == nil || !strings.Contains(err.Error(), "invalid action") {
		t.Fatalf("invalid action accepted: %v", err)
	}

	// Empty string = no audit ran (legacy confirm path).
	if p, err := ParseDesignDeltaConfirmPayload(""); err != nil || p != nil {
		t.Fatalf("empty payload must be (nil, nil), got %v, %v", p, err)
	}
}

// The parser normalizes the model's free-form reply: aliases mapped, unknown
// actions downgraded to skip, ids deduped, directions grouped extra → diverge
// → missing. Without human turns in context, source_class is pinned to
// uncertain regardless of what the model claimed.
func TestParseDesignDeltaReportNormalizes(t *testing.T) {
	reply := `[
	 {"delta_id":"x1","direction":"extra","suggested_action":"adopt_into_requirement","prototype_side":{"present":true,"reading":"stats nav item"},"source_class":"user_requested","source_evidence":"invented"},
	 {"direction":"prototype_missing","suggested_action":"nonsense_action","requirement_side":{"present":true,"reading":"quota trend view"}},
	 {"direction":"both_diverge","suggested_action":"skip"},
	 {"direction":"bogus_kind"},
	 {"delta_id":"x1","direction":"missing","suggested_action":"rework_design"}
	]`
	deltas, err := parseDesignDeltaReport(reply, false)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(deltas) != 4 {
		t.Fatalf("want 4 valid deltas (bogus dropped), got %d: %+v", len(deltas), deltas)
	}
	for _, d := range deltas {
		if d.SourceClass != "uncertain" {
			t.Fatalf("no-turns source_class must be pinned to uncertain, got %q on %s", d.SourceClass, d.ID)
		}
	}
	if deltas[0].Direction != string(deltaExtra) || deltas[0].ID != "x1" {
		t.Fatalf("first delta should be the extra with kept id, got %s/%s", deltas[0].Direction, deltas[0].ID)
	}
	// The duplicate id was renumbered rather than dropped.
	last := deltas[len(deltas)-1]
	if last.ID == "x1" {
		t.Fatalf("duplicate id not renumbered: %+v", last)
	}
	// Order: extra, then diverge, then missings.
	ranks := map[string]int{string(deltaExtra): 0, string(deltaDiverge): 1, string(deltaMissing): 2}
	for i := 1; i < len(deltas); i++ {
		if ranks[deltas[i].Direction] < ranks[deltas[i-1].Direction] {
			t.Fatalf("ordering broken at %d: %s after %s", i, deltas[i].Direction, deltas[i-1].Direction)
		}
	}
	// Unknown action downgraded to skip (the missing entry carried
	// "nonsense_action"; the trailing missing entry's rework_design stays).
	for _, d := range deltas {
		if d.Direction == string(deltaMissing) && d.SuggestedAction == "nonsense_action" {
			t.Fatalf("invalid action not downgraded to skip: %q", d.SuggestedAction)
		}
	}
}

// With human turns in context (benchmark B live), a claimed source class is
// trusted; garbage claims still degrade to uncertain.
func TestParseDesignDeltaReportSourceWithTurns(t *testing.T) {
	reply := `[
	 {"delta_id":"a","direction":"extra","suggested_action":"skip","source_class":"user_requested"},
	 {"delta_id":"b","direction":"extra","suggested_action":"skip","source_class":"hallucinated_kind"}
	]`
	deltas, err := parseDesignDeltaReport(reply, true)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if deltas[0].SourceClass != "user_requested" {
		t.Fatalf("trusted claim lost: %q", deltas[0].SourceClass)
	}
	if deltas[1].SourceClass != "uncertain" {
		t.Fatalf("garbage claim not degraded: %q", deltas[1].SourceClass)
	}
}

// A prose reply (no JSON array) fails closed — the caller routes to the
// escape hatch instead of rendering garbage.
func TestParseDesignDeltaReportRejectsProse(t *testing.T) {
	if _, err := parseDesignDeltaReport("I found no differences in the prototype.", false); err == nil {
		t.Fatal("prose reply must fail closed")
	}
	// Fenced JSON still parses (model ignored the no-fence rule).
	deltas, err := parseDesignDeltaReport("```json\n[{\"direction\":\"extra\",\"suggested_action\":\"skip\"}]\n```", false)
	if err != nil || len(deltas) != 1 {
		t.Fatalf("fenced array should parse: %v, %d", err, len(deltas))
	}
}

// Over the delta cap = parse failure (runaway lists are a prompt pathology,
// fail closed rather than rendering 200 rows).
func TestParseDesignDeltaReportCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < designDeltaMaxDeltas+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"direction":"extra","suggested_action":"skip"}`)
	}
	b.WriteString("]")
	if _, err := parseDesignDeltaReport(b.String(), false); err == nil || !strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("over-cap report accepted: %v", err)
	}
}

// The audit prompt carries the benchmark disclosure, the differential context
// block when prior decisions exist, and the human-turn block only when turns
// were read back.
func TestDesignDeltaAuditPromptContract(t *testing.T) {
	prompt := designDeltaAuditPrompt("req text", "<html><script>x=1</script><body>ui</body></html>", nil, designDeltaPrevContext{})
	for _, want := range []string{"uncertain", "唯一权威基准", "prototype_extra", "element_fingerprint", "不可用"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	// Script bodies stripped from the prototype excerpt (audit reads structure).
	if strings.Contains(prompt, "x=1") {
		t.Fatal("script body leaked into audit prompt")
	}
	if !strings.Contains(prompt, "<body>ui</body>") {
		t.Fatal("body content lost from audit prompt")
	}
	// Differential context appears when prior decisions are supplied.
	withPrev := designDeltaAuditPrompt("req", "<html></html>", nil, designDeltaPrevContext{
		decisions: []designDeltaDecision{{ID: "d1", Action: "adopt", Note: "keep"}},
	})
	if !strings.Contains(withPrev, "差分复审上下文") || !strings.Contains(withPrev, `"d1"`) {
		t.Fatal("prior decisions not embedded in differential prompt")
	}
	// Human turns render as numbered evidence when the read-back succeeded.
	withTurns := designDeltaAuditPrompt("req", "<html></html>", []designDeltaTurn{
		{Seq: 1, Content: "统计入口放侧栏第三个"},
	}, designDeltaPrevContext{})
	if !strings.Contains(withTurns, "设计会话中人的输入") || !strings.Contains(withTurns, "输入 #1") || !strings.Contains(withTurns, "统计入口") {
		t.Fatal("human turns not embedded as intent evidence")
	}
}

// Babel-standalone single-file prototypes (the OD pattern) keep every screen's
// JSX inside <script type="text/babel">. Those bodies ARE the page structure —
// they must survive the excerpt so the audit can see pages behind the login.
// Plain executable JS and src-only tags are still stripped. Regression for the
// 2026-10-05 fu91es incident: the whole admin console lived in a text/babel
// block and was stripped, so the audit reported the console as missing.
func TestDesignDeltaHTMLExcerptKeepsBabelUITemplates(t *testing.T) {
	html := `<html><head>
 <script src="https://cdn.tailwindcss.com"></script>
 <script>window.cfg={debug:true};</script>
 <style>.login-wrap{}</style>
</head><body>
 <div class="login-card">登录</div>
 <script type="text/babel" data-presets="react">
   function AdminConsole(){ return <div className="nav">授权管理 实例监控 审计日志</div>; }
 </script>
</body></html>`
	out := designDeltaHTMLExcerpt(html)
	if strings.Contains(out, "window.cfg") {
		t.Fatal("executable inline JS not stripped")
	}
	if strings.Contains(out, "cdn.tailwindcss.com\")></script>") && !strings.Contains(html, out) {
		t.Fatal("excerpt mutated source unexpectedly")
	}
	for _, want := range []string{"text/babel", "AdminConsole", "授权管理 实例监控 审计日志", "login-card"} {
		if !strings.Contains(out, want) {
			t.Fatalf("UI template content lost from excerpt: missing %q", want)
		}
	}
	// External src tag body is empty anyway; the tag itself may go either way,
	// but the surrounding document must survive.
	if !strings.Contains(out, "<body>") {
		t.Fatal("document shell lost")
	}
	// text/jsx (alternate Babel type spelling) is kept too.
	jsx := `<div><script type="text/jsx">const A=()=><b>仪表盘</b>;</script></div>`
	if !strings.Contains(designDeltaHTMLExcerpt(jsx), "仪表盘") {
		t.Fatal("text/jsx body stripped")
	}
	// Unterminated script block: kept as-is (fail-open to more context, never less).
	unterminated := `<div><script type="text/babel">const B=()=><i>审计</i>;</div>`
	if !strings.Contains(designDeltaHTMLExcerpt(unterminated), "审计") {
		t.Fatal("unterminated script mishandled")
	}
}

// The action summary renders a stable tally for the audit comment trail.
func TestDesignDeltaActionSummary(t *testing.T) {
	got := designDeltaActionSummary([]designDeltaDecision{
		{ID: "d1", Action: "adopt"}, {ID: "d2", Action: "adopt"},
		{ID: "d3", Action: "skip"}, {ID: "d4", Action: "drop"},
	})
	if got != "adopt×2, skip×1, drop×1" {
		t.Fatalf("unexpected summary: %q", got)
	}
}
