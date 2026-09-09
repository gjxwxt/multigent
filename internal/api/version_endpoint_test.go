package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionAndHealthEndpoints(t *testing.T) {
	s := &Server{version: "v1.2.3-test-commit"}

	for _, path := range []string{"/api/v1/health", "/api/v1/version"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.handleHealth(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s returned status %d", path, w.Code)
		}
		var resp map[string]any
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		if resp["ok"] != true {
			t.Fatalf("%s expected ok=true, got %v", path, resp["ok"])
		}
		if resp["version"] != "v1.2.3-test-commit" {
			t.Fatalf("%s expected version 'v1.2.3-test-commit', got %v", path, resp["version"])
		}
	}
}
