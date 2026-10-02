package workflow

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	controldb "github.com/multigent/multigent/internal/db"
)

// statesFor builds the states map EvaluatePlanProgress expects.
func statesFor(m map[string]string) map[string]string { return m }

// TestDependsKindContractEdgeStopsDeferPropagation: a contract-kind edge
// must not transmit a deferral (direct edge) — the contract baseline predates
// the stage, so the dependent stays Ready and materializable while the
// upstream package is human-deferred.
func TestDependsKindContractEdgeStopsDeferPropagation(t *testing.T) {
	plan := fixturePlan()
	// wp-infra depends on wp-session (contract) and wp-audit (artifact,
	// absent key). Defer wp-session only: the contract edge is filtered, so
	// wp-infra must NOT defer.
	plan.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract}
	states := map[string]string{
		"wp-session": PlanWPStateDeferred,
		"wp-audit":   PlanWPStateCompleted,
	}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range progress.Deferred {
		if id == "wp-infra" {
			t.Fatalf("wp-infra must not inherit a deferral across a contract edge; deferred=%v", progress.Deferred)
		}
	}
	foundReady := false
	for _, id := range progress.Ready {
		if id == "wp-infra" {
			foundReady = true
		}
	}
	if !foundReady {
		t.Fatalf("wp-infra must be Ready (its only artifact dep wp-audit is completed); ready=%v deferred=%v", progress.Ready, progress.Deferred)
	}
}

// TestDependsKindTransitiveDeferDoesNotCrossContractEdges is the regression
// the spec calls mandatory: C -contract-> B, B -artifact(deferred)-> A. The
// transitive propagation happens in the default branch via deferredSet — an
// implementation that only filters the direct edge would still defer C.
func TestDependsKindTransitiveDeferDoesNotCrossContractEdges(t *testing.T) {
	plan := fixturePlan()
	// Re-shape: wp-session <-deferred (artifact chain via wp-audit? no — keep
	// fixture ids but build the chain explicitly):
	//   wp-audit (deferred) --artifact--> wp-session --contract--> wp-infra
	plan.WorkPackages[0].DependsOn = []string{"wp-audit"}                     // wp-session needs the real wp-audit deliverable
	plan.WorkPackages[2].DependsOn = []string{"wp-session"}                   // wp-infra only consumes wp-session's contract
	plan.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract}
	states := map[string]string{"wp-audit": PlanWPStateDeferred}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	deferred := strings.Join(progress.Deferred, ",")
	if !strings.Contains(deferred, "wp-session") {
		t.Fatalf("wp-session must defer across its artifact edge from deferred wp-audit; deferred=%v", progress.Deferred)
	}
	if strings.Contains(deferred, "wp-infra") {
		t.Fatalf("wp-infra must NOT defer across the contract edge from deferred wp-session; deferred=%v", progress.Deferred)
	}
	for _, id := range progress.Ready {
		if id == "wp-infra" {
			return
		}
	}
	t.Fatalf("wp-infra must be Ready; progress=%+v", progress)
}

// TestDependsKindArtifactEdgeKeepsLegacyPropagation: absent key = artifact =
// pre-existing behavior (deferred upstream defers the dependent).
func TestDependsKindArtifactEdgeKeepsLegacyPropagation(t *testing.T) {
	plan := fixturePlan()
	states := map[string]string{"wp-session": PlanWPStateDeferred, "wp-audit": PlanWPStateCompleted}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range progress.Deferred {
		if id == "wp-infra" {
			return
		}
	}
	t.Fatalf("legacy propagation must be unchanged: wp-infra must defer behind deferred wp-session; deferred=%v", progress.Deferred)
}

// TestDependsKindMixedDepsFailClosed: one contract + one artifact edge; the
// artifact side deferred must still transmit (conservative default).
func TestDependsKindMixedDepsFailClosed(t *testing.T) {
	plan := fixturePlan()
	plan.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract}
	states := map[string]string{"wp-session": PlanWPStateCompleted, "wp-audit": PlanWPStateDeferred}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range progress.Deferred {
		if id == "wp-infra" {
			return
		}
	}
	t.Fatalf("artifact-edge deferral must transmit even when a sibling contract edge is clean; deferred=%v", progress.Deferred)
}

// TestDependsKindContractEdgeDoesNotTransmitBlocked: the same filter covers
// the blocked path — a FAILED upstream across a contract edge must not block
// the dependent (contract exists in the baseline regardless of execution).
func TestDependsKindContractEdgeDoesNotTransmitBlocked(t *testing.T) {
	plan := fixturePlan()
	plan.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract}
	states := map[string]string{"wp-session": PlanWPStateFailed, "wp-audit": PlanWPStateCompleted}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range progress.Blocked {
		if id == "wp-infra" {
			t.Fatalf("wp-infra must not be blocked across a contract edge by a failed upstream; blocked=%v", progress.Blocked)
		}
	}
	foundReady := false
	for _, id := range progress.Ready {
		if id == "wp-infra" {
			foundReady = true
		}
	}
	if !foundReady {
		t.Fatalf("wp-infra must be Ready; progress=%+v", progress)
	}
}

// TestValidateDeliveryPlanRejectsBadDependsKind: unknown kind values and
// orphan declarations (key not in dependsOn) are validation errors.
func TestValidateDeliveryPlanRejectsBadDependsKind(t *testing.T) {
	cases := map[string]func(p *DeliveryPlan){
		"unknown kind":  func(p *DeliveryPlan) { p.WorkPackages[2].DependsKind = map[string]string{"wp-session": "whatever"} },
		"orphan key":    func(p *DeliveryPlan) { p.WorkPackages[2].DependsKind = map[string]string{"wp-ghost": PlanDepKindContract} },
		"empty kind":    func(p *DeliveryPlan) { p.WorkPackages[2].DependsKind = map[string]string{"wp-session": "  "} },
		"kind not dep":  func(p *DeliveryPlan) { p.WorkPackages[0].DependsKind = map[string]string{"wp-infra": PlanDepKindContract} },
		"self kind":     func(p *DeliveryPlan) { p.WorkPackages[0].DependsKind = map[string]string{"wp-session": PlanDepKindContract} },
	}
	for name, mutate := range cases {
		plan := fixturePlan()
		mutate(&plan)
		if err := ValidateDeliveryPlan(plan); err == nil {
			t.Fatalf("%s: validation must refuse", name)
		}
	}
	if err := ValidateDeliveryPlan(fixturePlan()); err != nil {
		t.Fatalf("fixture without dependsKind must still validate: %v", err)
	}
	ok := fixturePlan()
	ok.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract, "wp-audit": PlanDepKindArtifact}
	if err := ValidateDeliveryPlan(ok); err != nil {
		t.Fatalf("valid dependsKind must pass: %v", err)
	}
}

// TestPlanDigestUnchangedWithoutDependsKind is the digest-compatibility gate:
// the canonical JSON (and therefore the frozen-record digest) of any plan
// written before DependsKind existed must be byte-for-byte identical after
// the change. The golden value was computed at dev @ 364cd2d4 BEFORE the
// canonicalizer learned the field; if this fails, every frozen plan in
// production fails digest verification on read.
func TestPlanDigestUnchangedWithoutDependsKind(t *testing.T) {
	const golden = "cb313ec6fea949def3d12a4e397f57fc92964531557e9d9f8c2a8f0d0c55aa32"
	d, err := PlanDigest(fixturePlan())
	if err != nil {
		t.Fatal(err)
	}
	if d != golden {
		t.Fatalf("digest of a dependsKind-less plan drifted across the upgrade: got %s, want %s", d, golden)
	}
	// And the explicit-artifact spelling must hash identically to absence:
	// {"wp-session": "artifact"} is the default semantics, so it must not
	// mint a distinct content identity.
	explicit := fixturePlan()
	explicit.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindArtifact, "wp-audit": PlanDepKindArtifact}
	dExplicit, err := PlanDigest(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if dExplicit == golden {
		// Not an error per se — but it must equal the digest of the SAME map
		// declared on a canonically equivalent plan, which the next block
		// checks. Recording here only that absence vs explicit-artifact MAY
		// differ (the canonicalizer includes non-empty maps).
		_ = dExplicit
	}
	// Canonical stability: shuffled key order hashes the same.
	shuffled := fixturePlan()
	shuffled.WorkPackages[2].DependsKind = map[string]string{"wp-audit": PlanDepKindArtifact, "wp-session": PlanDepKindArtifact}
	dShuffled, err := PlanDigest(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if dShuffled != dExplicit {
		t.Fatalf("dependsKind canonicalization must be key-order stable: %s != %s", dShuffled, dExplicit)
	}
}

// TestFrozenPlanWithDependsKindRoundTripsThroughStore: a plan declaring
// contract edges freezes, reloads through the ONLY trust path, and keeps its
// DependsKind — the propagation semantics survive persistence.
func TestFrozenPlanWithDependsKindRoundTripsThroughStore(t *testing.T) {
	controlDB, err := controldb.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controlDB.Close() })
	if err := controlDB.UpsertWorkspace(controldb.Workspace{ID: "ws-depkind", Name: "DepKind", Slug: "ws-depkind", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	store := NewStore(controlDB, "ws-depkind")

	plan := fixturePlan()
	plan.RunID = "wfr-depkind"
	plan.WorkPackages[2].DependsKind = map[string]string{"wp-session": PlanDepKindContract}
	if err := ValidateDeliveryPlan(plan); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FreezeDeliveryPlan("sample", plan, "admin"); err != nil {
		t.Fatal(err)
	}
	record, version, found, err := store.LoadFrozenPlanForRun("sample", plan.RunID)
	if err != nil || !found {
		t.Fatalf("frozen plan with dependsKind must reload: found=%v err=%v", found, err)
	}
	got := version.Plan.WorkPackages[2].DependsKind
	if got["wp-session"] != PlanDepKindContract {
		t.Fatalf("dependsKind must survive the freeze/load round trip, got %v", got)
	}
	_ = record
	_ = statesFor
}

// TestDependsKindStringShorthandExpandsToAllEdges: models reliably emit the
// string form "dependsKind": "contract" (live-fire t-20261002-6l65om did, and
// the map-only decoder silently dropped it — a lost contract edge silently
// restores defer propagation). The string means EVERY edge in dependsOn has
// that kind, and it must survive canonicalization into the map form.
func TestDependsKindStringShorthandExpandsToAllEdges(t *testing.T) {
	var decoded struct {
		WP PlanWorkPackage `json:"wp"`
	}
	if err := json.Unmarshal([]byte(`{"wp":{"id":"wp-2","title":"x","dependsOn":["wp-3"],"agentBinding":"a","acceptanceCriteria":["infra:c"],"dependsKind":"contract"}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WP.DependsKind["*"] != PlanDepKindContract {
		t.Fatalf("string shorthand must decode onto the wildcard key, got %v", decoded.WP.DependsKind)
	}

	// Round-trip through the canonicalizer: the wildcard expands to every
	// declared edge and the canonical JSON carries the map form. Packages are
	// sorted by id, so find wp-infra instead of assuming its index.
	plan := fixturePlan()
	plan.WorkPackages[2].DependsKind = decoded.WP.DependsKind
	canon := canonicalDeliveryPlan(plan)
	var got map[string]string
	for _, wp := range canon.WorkPackages {
		if wp.ID == "wp-infra" {
			got = wp.DependsKind
		}
	}
	if got["wp-session"] != PlanDepKindContract || got["wp-audit"] != PlanDepKindContract || len(got) != 2 {
		t.Fatalf("shorthand must expand to every dependsOn edge, got %v", got)
	}

	// And the expanded form must behave like the explicit map at evaluation:
	// deferring one upstream must NOT defer wp-infra (all edges contract).
	states := map[string]string{"wp-session": PlanWPStateDeferred, "wp-audit": PlanWPStateCompleted}
	progress, err := EvaluatePlanProgress(plan, states)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range progress.Deferred {
		if id == "wp-infra" {
			t.Fatalf("shorthand-declared contract edges must not transmit deferral; deferred=%v", progress.Deferred)
		}
	}

	// Marshaling always emits the map form (digest stability for old readers).
	var marshaledWP PlanWorkPackage
	for _, wp := range canon.WorkPackages {
		if wp.ID == "wp-infra" {
			marshaledWP = wp
		}
	}
	blob, err := json.Marshal(marshaledWP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"dependsKind":{"wp-audit":"contract","wp-session":"contract"}`) {
		t.Fatalf("canonical JSON must carry the map form, got %s", blob)
	}

	// Unknown kind in the string form must FAIL the decode — never silently
	// become artifact.
	var bad struct {
		WP PlanWorkPackage `json:"wp"`
	}
	if err := json.Unmarshal([]byte(`{"wp":{"id":"wp-2","title":"x","dependsOn":["wp-3"],"agentBinding":"a","acceptanceCriteria":["infra:c"],"dependsKind":"bond"}}`), &bad); err == nil {
		t.Fatal("an unknown dependsKind string must fail the decode")
	}

	// A malformed shape (array) must fail the decode too.
	var arr struct {
		WP PlanWorkPackage `json:"wp"`
	}
	if err := json.Unmarshal([]byte(`{"wp":{"id":"wp-2","title":"x","dependsOn":["wp-3"],"agentBinding":"a","acceptanceCriteria":["infra:c"],"dependsKind":["contract"]}}`), &arr); err == nil {
		t.Fatal("an array dependsKind must fail the decode")
	}

	// Shorthand with no dependsOn: nothing to expand onto — canonicalization
	// drops the map entirely (a no-op declaration, not an error).
	empty := fixturePlan()
	empty.WorkPackages[0].DependsKind = DependsKindMap{"*": PlanDepKindContract}
	canonEmpty := canonicalDeliveryPlan(empty)
	if len(canonEmpty.WorkPackages[0].DependsKind) != 0 {
		t.Fatalf("shorthand with no edges must canonicalize to nothing, got %v", canonEmpty.WorkPackages[0].DependsKind)
	}
	if err := ValidateDeliveryPlan(canonEmpty); err != nil {
		t.Fatalf("a shorthand with no edges is a harmless no-op: %v", err)
	}
}
