package workflow

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPlanWorkPackageClaimAdmitsSingleConcurrentWinner: concurrent stage
// activations race the same work package; the plan record is the CAS arbiter
// and must admit exactly ONE claimer (a second creator would produce a second
// branch instance/task for one work package, breaking 1:1:1:1).
func TestPlanWorkPackageClaimAdmitsSingleConcurrentWinner(t *testing.T) {
	store, project := newPlanTestStore(t)
	plan := fixturePlan()
	if _, _, err := store.FreezeDeliveryPlan(project, plan, "admin"); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-session")
			if err != nil {
				errs <- err
				results <- false
				return
			}
			results <- claim.Claimed
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim must not fail: %v", err)
		}
	}
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent claimer may win a work package, got %d", winners)
	}

	// A later (post-crash) claim after the first materialization recorded its
	// task id must LOSE — that is the second-identity guard.
	winnerRecord, _, _, err := store.LoadFrozenPlanForRun(project, plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ownerToken := ""
	for _, entry := range winnerRecord.Materializations {
		if entry.WPID == "wp-session" {
			ownerToken = entry.ClaimToken
		}
	}
	if ownerToken == "" {
		t.Fatal("the winning claim must carry a token")
	}
	if _, err := store.RecordPlanMaterialization(project, plan.RunID, PlanMaterializationEntry{
		WPID: "wp-session", BranchID: "wp-session", ClaimToken: ownerToken, TaskID: "t-plan-winner",
	}); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT attempt may not fill an identity it does not own, even before
	// a task id is recorded (token guard, deterministic).
	stolen, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-release")
	if err != nil || !stolen.Claimed {
		t.Fatalf("fresh claim for token-guard check: %+v err=%v", stolen, err)
	}
	if _, err := store.RecordPlanMaterialization(project, plan.RunID, PlanMaterializationEntry{
		WPID: "wp-release", BranchID: "wp-release", ClaimToken: "claim-not-the-owner", TaskID: "t-plan-thief",
	}); err == nil || !strings.Contains(err.Error(), "owned by another materialization attempt") {
		t.Fatalf("a foreign token must not fill the claim, got %v", err)
	}
	if _, err := store.RecordPlanMaterialization(project, plan.RunID, PlanMaterializationEntry{
		WPID: "wp-release", BranchID: "wp-release", ClaimToken: stolen.Token, TaskID: "t-plan-owner",
	}); err != nil {
		t.Fatalf("the owner must be able to fill its claim: %v", err)
	}
	claimedAgain, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-session")
	if err != nil {
		t.Fatal(err)
	}
	if claimedAgain.Claimed || claimedAgain.ExistingTaskID != "t-plan-winner" {
		t.Fatalf("claim after materialization must report the owner, got %+v", claimedAgain)
	}
	// A claim whose owner died before creating a task id is NOT immediately
	// re-takeable (a live sibling may be mid-flight) — and it becomes
	// re-takeable once the lease expires.
	first, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-audit")
	if err != nil || !first.Claimed {
		t.Fatalf("fresh work package must be claimable: %+v err=%v", first, err)
	}
	insideLease, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-audit")
	if err != nil {
		t.Fatal(err)
	}
	if insideLease.Claimed {
		t.Fatal("a live claim must not be taken over inside the lease window")
	}
	// Age the claim past the lease: the next attempt takes it over.
	record, _, _, err := store.LoadFrozenPlanForRun(project, plan.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range record.Materializations {
		if record.Materializations[i].WPID == "wp-audit" {
			record.Materializations[i].ClaimedAt = time.Now().Add(-2 * planClaimLease).UTC().Format(time.RFC3339)
		}
	}
	if _, err := store.mutatePlanRecord(project, plan.RunID, nil, func(rec *FrozenPlanRecord) (bool, error) {
		rec.Materializations = record.Materializations
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	afterLease, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-audit")
	if err != nil || !afterLease.Claimed {
		t.Fatalf("an expired claim must be re-takeable: %+v err=%v", afterLease, err)
	}

	// A claim with a corrupt timestamp fails CLOSED: without positive lease
	// evidence the claim is never taken over (storage fault must not fork the
	// work package identity).
	firstDeliver, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-infra")
	if err != nil || !firstDeliver.Claimed {
		t.Fatalf("fresh claim for corruption check: %+v err=%v", firstDeliver, err)
	}
	if _, err := store.mutatePlanRecord(project, plan.RunID, nil, func(rec *FrozenPlanRecord) (bool, error) {
		for i := range rec.Materializations {
			if rec.Materializations[i].WPID == "wp-infra" {
				rec.Materializations[i].ClaimedAt = "not-a-timestamp"
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPlanWorkPackage(project, plan.RunID, "wp-infra"); err == nil ||
		!strings.Contains(err.Error(), "corrupt claim timestamp") {
		t.Fatalf("a corrupt claim timestamp must fail closed, got %v", err)
	}
}

// TestPlanMaterializationRecordsSurviveConcurrentAppends: sibling branch
// completions append DIFFERENT work packages to the same plan record
// concurrently. The CAS read-modify-write loop must keep every entry — the
// plain Load→Upsert shape this replaced lost the loser's entry.
func TestPlanMaterializationRecordsSurviveConcurrentAppends(t *testing.T) {
	for iteration := 0; iteration < 5; iteration++ {
		store, project := newPlanTestStore(t)
		plan := fixturePlan()
		plan.RunID = fmt.Sprintf("wfr-concurrent-%d", iteration)
		if _, _, err := store.FreezeDeliveryPlan(project, plan, "admin"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, len(plan.WorkPackages))
		for i, wp := range plan.WorkPackages {
			wg.Add(1)
			go func(i int, wpID string) {
				defer wg.Done()
				<-start
				_, err := store.RecordPlanMaterialization(project, plan.RunID, PlanMaterializationEntry{
					WPID:      wpID,
					BranchID:  wpID,
					TaskID:    fmt.Sprintf("t-plan-%d", i),
					WaveIndex: PlanWaveIndex(plan, wpID),
				})
				errs <- err
			}(i, wp.ID)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: concurrent append must not fail: %v", iteration, err)
			}
		}

		record, _, _, err := store.LoadFrozenPlanForRun(project, plan.RunID)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, entry := range record.Materializations {
			seen[entry.WPID]++
		}
		if len(record.Materializations) != len(plan.WorkPackages) {
			t.Fatalf("iteration %d: lost update — got %d entries for %d work packages: %+v",
				iteration, len(record.Materializations), len(plan.WorkPackages), record.Materializations)
		}
		for _, wp := range plan.WorkPackages {
			if seen[wp.ID] != 1 {
				t.Fatalf("iteration %d: work package %s must appear exactly once, got %d", iteration, wp.ID, seen[wp.ID])
			}
		}
	}
}

// TestPlanRecordConcurrentFreezeVersionsBothLand: two concurrent freezes of
// DIFFERENT plan texts must both survive (one supersedes the other) — a
// version list is the audit trail and silently dropping one would misreport
// which plan the run was approved against.
func TestPlanRecordConcurrentFreezeVersionsBothLand(t *testing.T) {
	store, project := newPlanTestStore(t)
	base := fixturePlan()
	base.RunID = "wfr-freeze-race"
	if _, _, err := store.FreezeDeliveryPlan(project, base, "admin"); err != nil {
		t.Fatal(err)
	}
	edited := base
	edited.WorkPackages[0].Title = "Work package A (re-approach)"
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := store.FreezeDeliveryPlan(project, edited, fmt.Sprintf("admin-%d", i))
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent freeze must not fail: %v", err)
		}
	}
	record, current, _, err := store.LoadFrozenPlanForRun(project, base.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current.Plan.WorkPackages[0].Title, "re-approach") {
		t.Fatalf("the surviving current version must be the edited plan, got %q", current.Plan.WorkPackages[0].Title)
	}
	if len(record.Versions) < 2 {
		t.Fatalf("any previously frozen version must survive for audit, got %+v", record.Versions)
	}
	if record.Versions[0].Status != PlanStatusFrozen {
		t.Fatalf("history entries keep their own status, got %q", record.Versions[0].Status)
	}
}
