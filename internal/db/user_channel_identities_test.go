package db

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestClaimMattermostIdentityInScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	wsID := "ws-claim-test"
	if err := db.UpsertWorkspace(Workspace{ID: wsID, Name: "TestWS", Slug: "testws", Root: "/tmp/ws", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := db.UpsertUser(User{Username: "alice", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := db.UpsertUser(User{Username: "bob", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}

	// Connections
	conn1 := Connection{ID: "conn-mm-1", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-1", Status: "active", CreatedAt: nowUTC()}
	conn2 := Connection{ID: "conn-mm-2", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-2", Status: "active", CreatedAt: nowUTC()}
	connIndependent := Connection{ID: "conn-mm-independent", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-Indep", Status: "active", CreatedAt: nowUTC()}
	for _, c := range []Connection{conn1, conn2, connIndependent} {
		if err := db.UpsertConnection(c); err != nil {
			t.Fatalf("upsert conn %s: %v", c.ID, err)
		}
	}

	// Bindings
	bind1 := AgentChannelBinding{ID: "b-1", WorkspaceID: wsID, ProjectID: "p1", AgentID: "a1", ConnectionID: conn1.ID, Provider: "mattermost", Status: "connected"}
	bind2 := AgentChannelBinding{ID: "b-2", WorkspaceID: wsID, ProjectID: "p2", AgentID: "a2", ConnectionID: conn2.ID, Provider: "mattermost", Status: "connected"}
	bindIndep := AgentChannelBinding{ID: "b-indep", WorkspaceID: wsID, ProjectID: "p3", AgentID: "a3", ConnectionID: connIndependent.ID, Provider: "mattermost", Status: "connected"}
	for _, b := range []AgentChannelBinding{bind1, bind2, bindIndep} {
		if err := db.UpsertAgentChannelBinding(b); err != nil {
			t.Fatalf("upsert binding %s: %v", b.ID, err)
		}
	}

	// 1. Alice claims mm-user-1 on conn-mm-1 (scope: conn1, conn2)
	err = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
		ID:               "claim-1",
		WorkspaceID:      wsID,
		UserID:           "alice",
		ChannelBindingID: "b-1",
		AllowedConnIDs:   []string{conn1.ID, conn2.ID},
		ExternalUserID:   "mm-user-1",
		ExternalChatID:   "chat-1",
	})
	if err != nil {
		t.Fatalf("Alice claim failed: %v", err)
	}

	// Verify Alice is recorded
	identities, err := db.ListUserChannelIdentities(UserChannelIdentityFilter{
		WorkspaceID:      wsID,
		ChannelBindingID: "b-1",
	})
	if err != nil || len(identities) != 1 || identities[0].UserID != "alice" {
		t.Fatalf("expected alice in b-1, got: %#v err=%v", identities, err)
	}

	// 2. Bob tries to claim the SAME mm-user-1 on conn-mm-2 (same trusted scope: conn1, conn2) -> Must be rejected!
	err = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
		ID:               "claim-2",
		WorkspaceID:      wsID,
		UserID:           "bob",
		ChannelBindingID: "b-2",
		AllowedConnIDs:   []string{conn1.ID, conn2.ID},
		ExternalUserID:   "mm-user-1",
		ExternalChatID:   "chat-2",
	})
	if err == nil {
		t.Fatalf("expected conflict error for Bob, but got nil")
	}
	var conflictErr *IdentityConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected IdentityConflictError, got: %v", err)
	}
	if conflictErr.ConflictingUserID != "alice" {
		t.Fatalf("expected conflicting user to be alice, got: %s", conflictErr.ConflictingUserID)
	}

	// 3. Bob claims the SAME mm-user-1 on conn-mm-independent (independent scope: connIndependent only) -> Must succeed!
	err = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
		ID:               "claim-3",
		WorkspaceID:      wsID,
		UserID:           "bob",
		ChannelBindingID: "b-indep",
		AllowedConnIDs:   []string{connIndependent.ID},
		ExternalUserID:   "mm-user-1",
		ExternalChatID:   "chat-indep",
	})
	if err != nil {
		t.Fatalf("Bob independent claim should succeed, got: %v", err)
	}

	// 4. Alice re-binds/updates her chat ID on b-1 -> Must succeed (idempotent)
	err = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
		ID:               "claim-4",
		WorkspaceID:      wsID,
		UserID:           "alice",
		ChannelBindingID: "b-1",
		AllowedConnIDs:   []string{conn1.ID, conn2.ID},
		ExternalUserID:   "mm-user-1",
		ExternalChatID:   "chat-1-updated",
	})
	if err != nil {
		t.Fatalf("Alice re-claim should succeed, got: %v", err)
	}
	identities, err = db.ListUserChannelIdentities(UserChannelIdentityFilter{
		WorkspaceID:      wsID,
		ChannelBindingID: "b-1",
	})
	if err != nil || len(identities) != 1 || identities[0].ExternalChatID != "chat-1-updated" {
		t.Fatalf("expected updated chat ID, got: %#v", identities)
	}
}

func TestClaimMattermostIdentity_ConcurrentMutualExclusion(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "multigent.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	wsID := "ws-concurrent-test"
	_ = db.UpsertWorkspace(Workspace{ID: wsID, Name: "TestWS", Slug: "testws", Root: "/tmp/ws", CreatedAt: nowUTC()})
	_ = db.UpsertUser(User{Username: "user-a", CreatedAt: nowUTC()})
	_ = db.UpsertUser(User{Username: "user-b", CreatedAt: nowUTC()})
	conn := Connection{ID: "conn-shared", WorkspaceID: wsID, Provider: "mattermost", ConnectionName: "MM-Shared", Status: "active", CreatedAt: nowUTC()}
	_ = db.UpsertConnection(conn)
	bindA := AgentChannelBinding{ID: "b-a", WorkspaceID: wsID, ProjectID: "p1", AgentID: "a1", ConnectionID: conn.ID, Provider: "mattermost", Status: "connected"}
	bindB := AgentChannelBinding{ID: "b-b", WorkspaceID: wsID, ProjectID: "p2", AgentID: "a2", ConnectionID: conn.ID, Provider: "mattermost", Status: "connected"}
	_ = db.UpsertAgentChannelBinding(bindA)
	_ = db.UpsertAgentChannelBinding(bindB)

	var wg sync.WaitGroup
	wg.Add(2)

	var errA, errB error
	go func() {
		defer wg.Done()
		errA = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
			ID:               "claim-conc-a",
			WorkspaceID:      wsID,
			UserID:           "user-a",
			ChannelBindingID: "b-a",
			AllowedConnIDs:   []string{conn.ID},
			ExternalUserID:   "same-external-user",
		})
	}()

	go func() {
		defer wg.Done()
		errB = db.ClaimMattermostIdentityInScope(ClaimMattermostIdentityInput{
			ID:               "claim-conc-b",
			WorkspaceID:      wsID,
			UserID:           "user-b",
			ChannelBindingID: "b-b",
			AllowedConnIDs:   []string{conn.ID},
			ExternalUserID:   "same-external-user",
		})
	}()

	wg.Wait()

	successCount := 0
	conflictCount := 0
	for _, e := range []error{errA, errB} {
		if e == nil {
			successCount++
		} else {
			var ce *IdentityConflictError
			if errors.As(e, &ce) {
				conflictCount++
			}
		}
	}

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("expected exactly 1 success and 1 conflict, got success=%d conflict=%d (errA=%v, errB=%v)",
			successCount, conflictCount, errA, errB)
	}
}
