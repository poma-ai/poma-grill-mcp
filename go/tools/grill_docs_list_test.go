package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The note is the only truncation signal the calling LLM sees, so these cases
// lock in: complete listings stay silent, every kind of gap (page cap, failed
// follow-up page, degraded page) is spelled out, and the count claim never
// contradicts the returned slice.
func TestGrillDocsListNote(t *testing.T) {
	tests := []struct {
		name      string
		shown     int
		total     int
		truncated bool
		degraded  bool
		pagingErr string
		want      []string // substrings that must all be present
		wantEmpty bool
	}{
		{
			name:      "complete listing has no note",
			shown:     5,
			total:     5,
			wantEmpty: true,
		},
		{
			name:  "shown below total notes the count",
			shown: 100,
			total: 734,
			want:  []string{"Showing 100 of 734 documents."},
		},
		{
			name:      "truncated flag notes the count even when totals agree",
			shown:     20,
			total:     20,
			truncated: true,
			want:      []string{"Showing 20 of 20 documents."},
		},
		{
			name:      "total below shown is clamped to shown",
			shown:     10,
			total:     4,
			truncated: true,
			want:      []string{"Showing 10 of 10 documents."},
		},
		{
			name:     "degraded page is called out",
			shown:    2,
			total:    3,
			degraded: true,
			want:     []string{"Showing 2 of 3 documents.", "temporarily unavailable"},
		},
		{
			name:      "paging error is called out",
			shown:     100,
			total:     300,
			truncated: true,
			pagingErr: "grill docs list: HTTP 500: boom",
			want:      []string{"Showing 100 of 300 documents.", "Fetching additional pages failed", "HTTP 500"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grillDocsListNote(tt.shown, tt.total, tt.truncated, tt.degraded, tt.pagingErr)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("note = %q, want empty", got)
				}
				return
			}
			for _, sub := range tt.want {
				if !strings.Contains(got, sub) {
					t.Errorf("note = %q, missing %q", got, sub)
				}
			}
		})
	}
}

// docsPage renders a wire page. hasMore/nextCursor/degraded are only emitted
// when set, so the zero-values also model the pre-pagination API (fields
// absent entirely).
func docsPage(t *testing.T, docIDs []string, total int, hasMore bool, nextCursor string, degraded bool, legacy bool) string {
	t.Helper()
	docs := make([]map[string]string, 0, len(docIDs))
	for _, id := range docIDs {
		docs = append(docs, map[string]string{"doc_id": id})
	}
	page := map[string]any{
		"documents":       docs,
		"namespace":       "account_14d12545",
		"total_documents": total,
	}
	if !legacy {
		page["has_more"] = hasMore
		page["degraded"] = degraded
		if nextCursor != "" {
			page["next_cursor"] = nextCursor
		} else {
			page["next_cursor"] = nil
		}
	}
	b, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// End-to-end handler coverage against a stub API: old-API single-shot
// behavior is preserved bit-for-bit (one request, no note), paged responses
// are merged transparently, the 10-page cap and mid-loop failures return
// partial results with an explicit note, and first-page failures stay errors.
func TestGrillDocsListAutoPaging(t *testing.T) {
	tests := []struct {
		name string
		// pages returns status + body for the docs request with the given
		// cursor; call is the 1-based request count.
		pages         func(t *testing.T, cursor string, call int) (int, string)
		wantRequests  int
		wantDocs      int
		wantTotal     int
		wantErrSub    string // non-empty: expect tool-level error
		wantNoteSub   []string
		wantNoteEmpty bool
	}{
		{
			name: "old API without pagination fields keeps single-request behavior",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				if cursor != "" {
					t.Errorf("old API must never receive a cursor, got %q", cursor)
				}
				return 200, docsPage(t, []string{"d1", "d2"}, 2, false, "", false, true)
			},
			wantRequests:  1,
			wantDocs:      2,
			wantTotal:     2,
			wantNoteEmpty: true,
		},
		{
			name: "paged response merges all pages without a note",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				switch cursor {
				case "":
					return 200, docsPage(t, []string{"d1", "d2"}, 5, true, "c2", false, false)
				case "c2":
					return 200, docsPage(t, []string{"d3", "d4"}, 5, true, "c3", false, false)
				case "c3":
					return 200, docsPage(t, []string{"d5"}, 5, false, "", false, false)
				}
				t.Errorf("unexpected cursor %q", cursor)
				return 500, "{}"
			},
			wantRequests:  3,
			wantDocs:      5,
			wantTotal:     5,
			wantNoteEmpty: true,
		},
		{
			name: "page cap stops the loop and notes the truncation",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				ids := []string{fmt.Sprintf("d%d-1", call), fmt.Sprintf("d%d-2", call)}
				return 200, docsPage(t, ids, 100, true, fmt.Sprintf("c%d", call+1), false, false)
			},
			wantRequests: 10,
			wantDocs:     20,
			wantTotal:    100,
			wantNoteSub:  []string{"Showing 20 of 100 documents."},
		},
		{
			name: "degraded page is surfaced in the note",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				return 200, docsPage(t, []string{"d1", "d2"}, 3, false, "", true, false)
			},
			wantRequests: 1,
			wantDocs:     2,
			wantTotal:    3,
			wantNoteSub:  []string{"Showing 2 of 3 documents.", "temporarily unavailable"},
		},
		{
			name: "has_more without a usable cursor stops with a note",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				return 200, docsPage(t, []string{"d1"}, 50, true, "", false, false)
			},
			wantRequests: 1,
			wantDocs:     1,
			wantTotal:    50,
			wantNoteSub:  []string{"Showing 1 of 50 documents."},
		},
		{
			name: "follow-up page failure returns the accumulated pages with a note",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				if cursor == "" {
					return 200, docsPage(t, []string{"d1", "d2"}, 6, true, "c2", false, false)
				}
				return 500, `{"error":"boom"}`
			},
			wantRequests: 2,
			wantDocs:     2,
			wantTotal:    6,
			wantNoteSub:  []string{"Showing 2 of 6 documents.", "Fetching additional pages failed", "HTTP 500"},
		},
		{
			name: "first page failure is a tool error",
			pages: func(t *testing.T, cursor string, call int) (int, string) {
				return 500, `{"error":"boom"}`
			},
			wantRequests: 1,
			wantErrSub:   "grill docs list: HTTP 500",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetProjectsCache()
			defer resetProjectsCache()

			requests := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/v3/projects", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("[]"))
			})
			mux.HandleFunc("/v3/grill/docs", func(w http.ResponseWriter, r *http.Request) {
				requests++
				st, body := tt.pages(t, r.URL.Query().Get("cursor"), requests)
				w.WriteHeader(st)
				_, _ = w.Write([]byte(body))
			})
			ts := httptest.NewServer(mux)
			defer ts.Close()
			t.Setenv("POMA_API_BASE_URL", ts.URL)

			// Unique token per case so the projects cache never crosses tests.
			input := GrillDocsListInput{Token: fmt.Sprintf("test-token-%d", i)}
			res, out, err := GrillDocsList(context.Background(), nil, input)
			if err != nil {
				t.Fatalf("GrillDocsList returned protocol error: %v", err)
			}

			if tt.wantErrSub != "" {
				if res == nil || !res.IsError {
					t.Fatal("expected IsError result")
				}
				if !strings.Contains(out.Error, tt.wantErrSub) {
					t.Fatalf("Error = %q, want substring %q", out.Error, tt.wantErrSub)
				}
				return
			}
			if out.Error != "" {
				t.Fatalf("unexpected tool error: %q", out.Error)
			}
			if requests != tt.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tt.wantRequests)
			}
			if len(out.Documents) != tt.wantDocs {
				t.Errorf("len(documents) = %d, want %d", len(out.Documents), tt.wantDocs)
			}
			if out.TotalDocuments != tt.wantTotal {
				t.Errorf("total_documents = %d, want %d", out.TotalDocuments, tt.wantTotal)
			}
			if tt.wantNoteEmpty && out.Note != "" {
				t.Errorf("note = %q, want empty", out.Note)
			}
			for _, sub := range tt.wantNoteSub {
				if !strings.Contains(out.Note, sub) {
					t.Errorf("note = %q, missing %q", out.Note, sub)
				}
			}
			if out.Scope == nil {
				t.Error("scope must always be resolved (may degrade, never nil)")
			}
		})
	}
}
