package tools

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// No test in this package may reach a live POMA API. TestMain clears every
// credential and base-URL variable the package reads (common.go: POMA_API_KEY
// for the token, POMA_API_BASE_URL and POMA_STATUS_API_BASE_URL for the hosts,
// POMA_PROJECT_ID for scoping) and points the API host at a dead stub. The
// status host is cleared rather than set, so it follows POMA_API_BASE_URL —
// the dead stub here, a test's own stub inside that test. A test
// that wants an API sets its own stub with t.Setenv, which restores the dead
// one afterwards; a test that forgets falls through to the dead stub, and the
// run fails listing the stray requests — instead of the request silently going
// to https://api.index4.ai with whatever key the developer has exported.
// (2026-10-01: TestHandleIngestUploadMissingToken did exactly that.)

var (
	deadAPIMu   sync.Mutex
	deadAPIHits []string
)

func TestMain(m *testing.M) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadAPIMu.Lock()
		deadAPIHits = append(deadAPIHits, r.Method+" "+r.URL.Path)
		deadAPIMu.Unlock()
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"test reached the default API host; set POMA_API_BASE_URL to a stub"}`))
	}))
	for _, k := range []string{"POMA_API_KEY", "POMA_PROJECT_ID", "POMA_STATUS_API_BASE_URL"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("POMA_API_BASE_URL", dead.URL)

	code := m.Run()
	dead.Close()
	if len(deadAPIHits) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d request(s) reached the default API host (would have gone to the live API):\n", len(deadAPIHits))
		for _, h := range deadAPIHits {
			fmt.Fprintln(os.Stderr, "  "+h)
		}
		code = 1
	}
	os.Exit(code)
}

// noLiveAPI makes a test that must not talk to any API explicit about it: no
// token from the environment, and an API host that fails the test on any
// request.
func noLiveAPI(t *testing.T) {
	t.Helper()
	t.Setenv("POMA_API_KEY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("POMA_API_BASE_URL", srv.URL)
	t.Setenv("POMA_STATUS_API_BASE_URL", "") // follows POMA_API_BASE_URL
}
