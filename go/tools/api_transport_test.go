package tools

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// Every request of one client carries origin "mcp" (overriding poma-cli's "cli") and
// the same client trace id; another client gets another id.
func TestGrillClientStampsOriginAndTraceID(t *testing.T) {
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	c := grillClient("poma_acc_test")
	for _, call := range []struct {
		name string
		do   func() (int, error)
	}{
		{name: "Do", do: func() (int, error) { _, st, err := c.Do(http.MethodGet, "/docs", nil, nil); return st, err }},
		{name: "DoJSON", do: func() (int, error) {
			_, st, err := c.DoJSON(http.MethodPost, "/search", map[string]string{"q": "x"})
			return st, err
		}},
	} {
		if st, err := call.do(); err != nil || st != http.StatusOK {
			t.Fatalf("%s: status %d, err %v", call.name, st, err)
		}
	}
	if _, _, err := grillClient("poma_acc_test").Do(http.MethodGet, "/docs", nil, nil); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d requests, want 3", len(got))
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	for i, h := range got {
		if o := h.Values("X-Request-Origin"); len(o) != 1 || o[0] != requestOrigin {
			t.Errorf("request %d: X-Request-Origin = %q, want [%q]", i, o, requestOrigin)
		}
		if id := h.Get(headerClientTraceID); !valid.MatchString(id) {
			t.Errorf("request %d: %s = %q, not accepted by the API", i, headerClientTraceID, id)
		}
		if h.Get("Authorization") != "Bearer poma_acc_test" {
			t.Errorf("request %d: Authorization = %q", i, h.Get("Authorization"))
		}
	}
	if got[0].Get(headerClientTraceID) != got[1].Get(headerClientTraceID) {
		t.Error("one client sent two trace ids")
	}
	if got[0].Get(headerClientTraceID) == got[2].Get(headerClientTraceID) {
		t.Error("two clients shared a trace id")
	}
}
