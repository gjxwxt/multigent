package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

func fixturePlan() DeliveryPlan {
	return DeliveryPlan{
		SchemaVersion: DeliveryPlanSchemaVersion,
		PlanID:        "plan-s2-loop",
		Version:       1,
		RunID:         "wfr-plan-loop",
		RequirementItems: []PlanRequirementItem{
			{ID: "uc-1", Text: "session expiry clears local state", Source: "requirements.md"},
			{ID: "uc-2", Text: "audit export is capped at 10000 rows", Source: "requirements.md"},
		},
		SharedContract: []PlanContractItem{
			{ID: "api-skeleton", Artifact: "audit export endpoint", Path: "server/AuditController.java"},
		},
		WorkPackages: []PlanWorkPackage{
			{
				ID: "wp-session", Title: "Session lifecycle", Domain: "frontend session",
				AcceptanceCriteria: []string{"uc-1"},
				AgentBinding:       "Nora",
				ExpectedDelivery:   []string{"console-web/src/api"},
			},
			{
				ID: "wp-audit", Title: "Audit export", Domain: "server audit",
				AcceptanceCriteria: []string{"uc-2"}, AgentBinding: "Mira",
				ContractRefs:     []string{"api-skeleton"},
				ExpectedDelivery: []string{"auth-center-server/src"},
			},
			{
				ID: "wp-infra", Title: "CI wiring", Domain: "infra",
				DependsOn:          []string{"wp-session", "wp-audit"},
				AcceptanceCriteria: []string{"infra:ci-baseline"},
				AgentBinding:       "Mira",
			},
		},
		IntegrationPolicy: "join=all",
		QAPolicy:          "independent-qa",
	}
}

func TestDeliveryPlanDigestIsCanonicalAndTamperEvident(t *testing.T) {
	base, err := PlanDigest(fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	// Canonicalization: shuffled slices + untrimmed strings must hash the same.
	shuffled := fixturePlan()
	shuffled.WorkPackages[0], shuffled.WorkPackages[2] = shuffled.WorkPackages[2], shuffled.WorkPackages[0]
	shuffled.WorkPackages[0].DependsOn = []string{"  wp-audit ", "wp-session"} // reversed + padded
	shuffled.RequirementItems[0], shuffled.RequirementItems[1] = shuffled.RequirementItems[1], shuffled.RequirementItems[0]
	shuffled.RequirementItems[0].Text = "  " + shuffled.RequirementItems[0].Text + "  "
	shuffled.WorkPackages[2].Title = " " + shuffled.WorkPackages[2].Title + " "
	got, err := PlanDigest(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("canonical digest must be order/whitespace stable: %s != %s", got, base)
	}

	// Reverse: a one-byte edit in any covered field must change the digest.
	edits := map[string]func(p *DeliveryPlan){
		"work package title":   func(p *DeliveryPlan) { p.WorkPackages[0].Title = "Session lifecycle!" },
		"dependency edge":      func(p *DeliveryPlan) { p.WorkPackages[2].DependsOn = []string{"wp-session"} },
		"acceptance reference": func(p *DeliveryPlan) { p.WorkPackages[1].AcceptanceCriteria = []string{"uc-1"} },
		"requirement snapshot": func(p *DeliveryPlan) { p.RequirementItems[0].Text = "session expiry clears state." },
		"agent binding":        func(p *DeliveryPlan) { p.WorkPackages[0].AgentBinding = "Mira" },
		"shared contract path": func(p *DeliveryPlan) { p.SharedContract[0].Path = "server/AuditController2.java" },
		"infra prefix":         func(p *DeliveryPlan) { p.WorkPackages[2].AcceptanceCriteria = []string{"infra:ci-baseline-2"} },
		"qa policy":            func(p *DeliveryPlan) { p.QAPolicy = "independent-qa+signoff" },
	}
	// The plan TEXT is version-agnostic (the frozen record owns versioning): a
	// version bump alone must NOT change the digest, otherwise re-approving
	// identical text would mint a spurious new version.
	versionBump := fixturePlan()
	versionBump.Version = 7
	bumped, err := PlanDigest(versionBump)
	if err != nil {
		t.Fatal(err)
	}
	if bumped != base {
		t.Fatalf("version field must not change the content digest: %s != %s", bumped, base)
	}
	seen := map[string]bool{base: true}
	for name, edit := range edits {
		mutated := fixturePlan()
		edit(&mutated)
		d, err := PlanDigest(mutated)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d == base {
			t.Fatalf("digest did not change after editing %s", name)
		}
		if seen[d] {
			t.Fatalf("distinct edits collided on digest for %s", name)
		}
		seen[d] = true
	}
}

func TestValidateDeliveryPlanRejectsUnsafeShapes(t *testing.T) {
	cases := map[string]func(p *DeliveryPlan){
		"cycle":            func(p *DeliveryPlan) { p.WorkPackages[0].DependsOn = []string{"wp-infra"} },
		"self dependency":  func(p *DeliveryPlan) { p.WorkPackages[0].DependsOn = []string{"wp-session"} },
		"unknown dep":      func(p *DeliveryPlan) { p.WorkPackages[0].DependsOn = []string{"wp-ghost"} },
		"unknown ac ref":   func(p *DeliveryPlan) { p.WorkPackages[0].AcceptanceCriteria = []string{"uc-404"} },
		"empty infra ref":  func(p *DeliveryPlan) { p.WorkPackages[0].AcceptanceCriteria = []string{"infra:"} },
		"empty ac list":    func(p *DeliveryPlan) { p.WorkPackages[0].AcceptanceCriteria = nil },
		"unknown contract": func(p *DeliveryPlan) { p.WorkPackages[0].ContractRefs = []string{"nope"} },
		"no actor":         func(p *DeliveryPlan) { p.WorkPackages[0].AgentBinding = "" },
		"duplicate wp":     func(p *DeliveryPlan) { p.WorkPackages[1].ID = p.WorkPackages[0].ID },
		"duplicate uc":     func(p *DeliveryPlan) { p.RequirementItems[1].ID = p.RequirementItems[0].ID },
		"bad schema":       func(p *DeliveryPlan) { p.SchemaVersion = DeliveryPlanSchemaVersion + 1 },
		"empty plan":       func(p *DeliveryPlan) { p.PlanID = "" },
		"no work packages": func(p *DeliveryPlan) { p.WorkPackages = nil },
		"unsanitized id":   func(p *DeliveryPlan) { p.WorkPackages[0].ID = "WP Session/1" },
		"no title":         func(p *DeliveryPlan) { p.WorkPackages[0].Title = " " },
	}
	for name, mutate := range cases {
		plan := fixturePlan()
		mutate(&plan)
		if err := ValidateDeliveryPlan(plan); err == nil {
			t.Fatalf("%s: validation must refuse this plan", name)
		}
	}
	if err := ValidateDeliveryPlan(fixturePlan()); err != nil {
		t.Fatalf("fixture plan must validate: %v", err)
	}
}

func TestPlanWavesAndProgressAreDerivedFromDependencies(t *testing.T) {
	plan := fixturePlan()
	waves, err := PlanWaves(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 2 || strings.Join(waves[0], ",") != "wp-audit,wp-session" || strings.Join(waves[1], ",") != "wp-infra" {
		t.Fatalf("unexpected waves: %v", waves)
	}
	for i := 0; i < 5; i++ {
		again, err := PlanWaves(plan)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(again[0], ",") != strings.Join(waves[0], ",") {
			t.Fatalf("wave derivation must be deterministic: %v vs %v", again, waves)
		}
	}

	cycle := fixturePlan()
	cycle.WorkPackages[0].DependsOn = []string{"wp-infra"}
	if _, err := PlanWaves(cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle must be reported, got %v", err)
	}
	if _, err := EvaluatePlanProgress(cycle, nil); err == nil {
		t.Fatal("progress evaluation must propagate cycle refusal")
	}

	// Nothing materialized: only the dependency-free packages are ready.
	progress, err := EvaluatePlanProgress(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(progress.Ready, ",") != "wp-audit,wp-session" || len(progress.Waiting) != 1 || progress.Waiting[0] != "wp-infra" {
		t.Fatalf("unexpected initial progress: %+v", progress)
	}

	// Wave 1 running: nothing new is ready yet.
	progress, err = EvaluatePlanProgress(plan, map[string]string{"wp-session": PlanWPStateRunning, "wp-audit": PlanWPStateCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Ready) != 0 || len(progress.InProgress) != 1 || len(progress.Waiting) != 1 {
		t.Fatalf("wave 2 must wait for wave 1 to complete: %+v", progress)
	}

	// Wave 1 done: the dependent package becomes ready.
	progress, err = EvaluatePlanProgress(plan, map[string]string{"wp-session": PlanWPStateCompleted, "wp-audit": PlanWPStateCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(progress.Ready, ",") != "wp-infra" {
		t.Fatalf("dependent package must become ready: %+v", progress)
	}
}

func TestEvaluatePlanProgressBlocksDownstreamOnUpstreamFailure(t *testing.T) {
	plan := fixturePlan()
	// A transitive chain so blocking must propagate beyond the direct child.
	plan.WorkPackages = append(plan.WorkPackages, PlanWorkPackage{
		ID: "wp-release", Title: "Release", DependsOn: []string{"wp-infra"},
		AcceptanceCriteria: []string{"infra:release"}, AgentBinding: "Mira",
	})
	progress, err := EvaluatePlanProgress(plan, map[string]string{"wp-session": PlanWPStateFailed})
	if err != nil {
		t.Fatal(err)
	}
	// The failed package itself is Blocked (visible), its dependent and the
	// transitive dependent are blocked with it; the independent sibling is
	// unaffected.
	if strings.Join(progress.Blocked, ",") != "wp-infra,wp-release,wp-session" {
		t.Fatalf("failed package and its downstream must be blocked, got %+v", progress)
	}
	if strings.Join(progress.Ready, ",") != "wp-audit" {
		t.Fatalf("the independent wave-1 package stays ready: %+v", progress)
	}
	// wp-infra waits on wp-session → now blocked; wp-release depends on a
	// blocked package → transitively blocked. Both stay VISIBLE.
	progress, err = EvaluatePlanProgress(plan, map[string]string{
		"wp-session": PlanWPStateFailed,
		"wp-audit":   PlanWPStateCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(progress.Blocked, ",") != "wp-infra,wp-release,wp-session" {
		t.Fatalf("downstream blocked set must be explicit, got %+v", progress)
	}
	if len(progress.Ready) != 0 {
		t.Fatalf("nothing may be ready behind a failed dependency: %+v", progress)
	}
	// skipped behaves like failed for propagation.
	progress, err = EvaluatePlanProgress(plan, map[string]string{"wp-session": PlanWPStateSkipped, "wp-audit": PlanWPStateCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(progress.Blocked, ",") != "wp-infra,wp-release,wp-session" {
		t.Fatalf("skipped upstream must block downstream too, got %+v", progress)
	}
}

func newPlanTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	controlDB, err := controldb.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	// kv_records.workspace_id is a foreign key; the workspace must exist.
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "ws-plan", Name: "Plan WS", Slug: "ws-plan", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return NewStore(controlDB, "ws-plan"), "sample"
}

func TestFreezeDeliveryPlanIsAtomicIdempotentAndVersioned(t *testing.T) {
	store, project := newPlanTestStore(t)
	plan := fixturePlan()

	record, version, err := store.FreezeDeliveryPlan(project, plan, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if record.CurrentVersion != 1 || version.Status != PlanStatusFrozen || version.Digest == "" || version.ApprovedBy != "admin" {
		t.Fatalf("unexpected frozen record: %+v / %+v", record, version)
	}

	// Idempotent re-freeze: same text + same approver → no new version.
	again, againVersion, err := store.FreezeDeliveryPlan(project, plan, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if again.CurrentVersion != 1 || againVersion.Digest != version.Digest || len(again.Versions) != 1 {
		t.Fatalf("re-freezing identical plan must not mint a version: %+v", again)
	}

	// A one-byte change is a NEW version that must be re-approved.
	edited := fixturePlan()
	edited.WorkPackages[0].Title = "Session lifecycle v2"
	edited.Version = 2
	next, nextVersion, err := store.FreezeDeliveryPlan(project, edited, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if next.CurrentVersion != 2 || nextVersion.Digest == version.Digest || len(next.Versions) != 2 {
		t.Fatalf("edited plan must become version 2: %+v", next)
	}
	if nextVersion.SupersedesDigest != version.Digest {
		t.Fatalf("version 2 must record what it superseded: %+v", nextVersion)
	}

	// Unapproved freeze is refused outright.
	if _, _, err := store.FreezeDeliveryPlan(project, fixturePlan(), "  "); err == nil {
		t.Fatal("freezing without an approver must be refused")
	}
	// Invalid plan is refused before any write.
	if _, _, err := store.FreezeDeliveryPlan(project, DeliveryPlan{SchemaVersion: DeliveryPlanSchemaVersion, PlanID: "x", Version: 1, RunID: "r"}, "admin"); err == nil {
		t.Fatal("freezing an invalid plan must be refused")
	}
}

func TestLoadFrozenPlanRefusesTamperedUnfrozenAndMissingRecords(t *testing.T) {
	store, project := newPlanTestStore(t)
	plan := fixturePlan()
	if _, _, err := store.FreezeDeliveryPlan(project, plan, "admin"); err != nil {
		t.Fatal(err)
	}

	// Trusted path returns the frozen version.
	record, version, ok, err := store.LoadFrozenPlanForRun(project, plan.RunID)
	if err != nil || !ok || version.Version != 1 {
		t.Fatalf("frozen plan must load: ok=%v err=%v version=%+v", ok, err, version)
	}

	// Missing record: clean miss (the caller decides; a plan-driven stage refuses).
	if _, _, ok, err := store.LoadFrozenPlanForRun(project, "wfr-does-not-exist"); err != nil || ok {
		t.Fatalf("missing plan must be a clean miss, got ok=%v err=%v", ok, err)
	}

	// REVERSE: tamper one byte of the stored plan payload → digest mismatch.
	tampered := record
	tampered.Versions[0].Plan.WorkPackages[0].Title = "Session lifecycle!"
	payload, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.UpsertRecord(planRecordTable, store.workspaceID, []string{project, plan.RunID}, string(payload)); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = store.LoadFrozenPlanForRun(project, plan.RunID)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered plan must be refused with a digest mismatch, got %v", err)
	}

	// A record whose current version is not frozen is refused as well.
	unfrozen := record
	unfrozen.Versions[0].Status = PlanStatusDraft
	payload, err = json.Marshal(unfrozen)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.UpsertRecord(planRecordTable, store.workspaceID, []string{project, plan.RunID}, string(payload)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.LoadFrozenPlanForRun(project, plan.RunID); err == nil || !strings.Contains(err.Error(), "not frozen") {
		t.Fatalf("unfrozen plan must be refused, got %v", err)
	}
}

func TestRecordPlanMaterializationIsIdempotentAndRefusesSecondIdentity(t *testing.T) {
	store, project := newPlanTestStore(t)
	plan := fixturePlan()
	if _, _, err := store.FreezeDeliveryPlan(project, plan, "admin"); err != nil {
		t.Fatal(err)
	}
	entry := PlanMaterializationEntry{
		WPID: "wp-session", BranchID: "wp-session", DefinitionID: "branch-def-1",
		DefinitionDigest: "sha256:def", TaskID: "t-plan-1", ChildRunID: "wfr-child-1", WaveIndex: 0,
	}
	record, err := store.RecordPlanMaterialization(project, plan.RunID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Materializations) != 1 {
		t.Fatalf("first materialization must be recorded: %+v", record.Materializations)
	}
	firstAt := record.Materializations[0].MaterializedAt

	// Re-drive with the same identity: no duplicate row, original timestamp kept.
	replay := entry
	replay.ChildRunID = "wfr-child-1b"
	record, err = store.RecordPlanMaterialization(project, plan.RunID, replay)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Materializations) != 1 || record.Materializations[0].ChildRunID != "wfr-child-1b" {
		t.Fatalf("re-drive must reuse the same identity: %+v", record.Materializations)
	}
	if record.Materializations[0].MaterializedAt != firstAt {
		t.Fatalf("re-drive must keep the original materializedAt")
	}

	// REVERSE: a different task id for the same work package is refused —
	// that would be a second identity for one work package.
	second := entry
	second.TaskID = "t-plan-2"
	if _, err := store.RecordPlanMaterialization(project, plan.RunID, second); err == nil ||
		!strings.Contains(err.Error(), "second identity") {
		t.Fatalf("second identity must be refused, got %v", err)
	}

	// Materialization against a run without a frozen plan is refused.
	if _, err := store.RecordPlanMaterialization(project, "wfr-unknown-run", entry); err == nil {
		t.Fatal("recording a materialization without a frozen plan must be refused")
	}
}
