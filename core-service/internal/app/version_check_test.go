package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionCheckReportsNewestStableRelease(t *testing.T) {
	asked := 0
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		_, _ = w.Write([]byte(`[
			{"tag_name":"v99.0.0-rc1","prerelease":true},
			{"tag_name":"v99.1.0","draft":true},
			{"tag_name":"core-v9.0.0"},
			{"tag_name":"v99.0.0","name":"v99.0.0 — next","html_url":"https://example/v99","published_at":"2026-10-10T00:00:00Z"},
			{"tag_name":"v3.0.0"}
		]`))
	}))
	defer releases.Close()
	savedURL := updateCheckURL
	updateCheckURL = releases.URL
	defer func() { updateCheckURL = savedURL; latestRelease = updateCheckCache{} }()
	latestRelease = updateCheckCache{}
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/version/check", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if got["latest"] != "v99.0.0" || got["updateAvailable"] != true || got["releaseUrl"] != "https://example/v99" {
		t.Fatalf("check=%v", got)
	}
	// ix.12.7: the answer is kept, so the dashboard does not ask GitHub on
	// every request.
	for i := 0; i < 3; i++ {
		a.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/version/check", nil))
	}
	if asked != 1 {
		t.Fatalf("GitHub asked %d times for 4 checks", asked)
	}
	if compareSemver("v3.0.10", "3.0.9") != 1 || compareSemver("3.0.4", "v3.0.4") != 0 {
		t.Fatal("semver compare")
	}
}
