package imbridge

import "testing"

// TestSupervisorFingerprintChangesWithSecrets pins the fingerprint semantics:
// any credential input change must produce a different fingerprint so the
// bridge rebuilds a stale supervisor; identical inputs keep the fingerprint
// stable so healthy supervisors are not churned.
func TestSupervisorFingerprintChangesWithSecrets(t *testing.T) {
	base := supervisorFingerprint("http://mm:8065", "tok", "hmac", "bot-1", "conn-1")
	if base == "" {
		t.Fatal("empty fingerprint")
	}
	same := supervisorFingerprint("http://mm:8065", "tok", "hmac", "bot-1", "conn-1")
	if same != base {
		t.Fatal("identical inputs must produce identical fingerprint")
	}
	for name, mutate := range map[string]func() string{
		"baseURL": func() string { return supervisorFingerprint("http://other:8065", "tok", "hmac", "bot-1", "conn-1") },
		"token":   func() string { return supervisorFingerprint("http://mm:8065", "tok2", "hmac", "bot-1", "conn-1") },
		"hmac":    func() string { return supervisorFingerprint("http://mm:8065", "tok", "hmac2", "bot-1", "conn-1") },
		"botID":   func() string { return supervisorFingerprint("http://mm:8065", "tok", "hmac", "bot-2", "conn-1") },
		"connID":  func() string { return supervisorFingerprint("http://mm:8065", "tok", "hmac", "bot-1", "conn-2") },
	} {
		if mutate() == base {
			t.Fatalf("%s change must alter the fingerprint", name)
		}
	}
}
