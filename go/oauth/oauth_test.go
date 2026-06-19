package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// makeToken creates a signed JWT with the given claims for testing.
func makeToken(t *testing.T, secret []byte, claims *oauthClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// goodClaims returns a valid set of oauthClaims for tests.
func goodClaims(aud string) *oauthClaims {
	return &oauthClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{aud},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		AccountID: "acct-123",
		Project:   "proj-456",
		Scope:     "mcp.tools.read mcp.tools.write",
		ClientID:  "cid-789",
		TokenType: "oauth-access",
	}
}

func TestRequireBearer(t *testing.T) {
	const testSecret = "test-secret-32-bytes-long-enough!"
	const testResource = "https://mcp.grill.poma-ai.com/"

	// Override package-level vars for these tests.
	orig := jwtSecret
	origRes := mcpResourceURI
	jwtSecret = []byte(testSecret)
	mcpResourceURI = testResource
	t.Cleanup(func() {
		jwtSecret = orig
		mcpResourceURI = origRes
	})

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	handler := RequireBearer(next)

	tests := []struct {
		name       string
		setup      func(r *http.Request)
		wantStatus int
		wantNext   bool
	}{
		{
			name: "valid oauth jwt passes",
			setup: func(r *http.Request) {
				tok := makeToken(t, []byte(testSecret), goodClaims(testResource))
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name: "x-api-key bypasses jwt check",
			setup: func(r *http.Request) {
				r.Header.Set("x-api-key", "some-key")
			},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name: "legacy non-jwt bearer passes to next",
			setup: func(r *http.Request) {
				// A raw api key (no dots) is not a JWT.
				r.Header.Set("Authorization", "Bearer poma_ak_notajwtatall")
			},
			wantStatus: http.StatusOK,
			wantNext:   true,
		},
		{
			name: "no credentials returns 401",
			setup: func(r *http.Request) {
				// No headers set.
			},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name: "wrong signature returns 401",
			setup: func(r *http.Request) {
				tok := makeToken(t, []byte("different-secret-32b-long-enough!"), goodClaims(testResource))
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name: "expired token returns 401",
			setup: func(r *http.Request) {
				c := goodClaims(testResource)
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
				tok := makeToken(t, []byte(testSecret), c)
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name: "wrong aud returns 401",
			setup: func(r *http.Request) {
				tok := makeToken(t, []byte(testSecret), goodClaims("https://other.mcp.example.com/"))
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
		{
			name: "wrong typ returns 401",
			setup: func(r *http.Request) {
				c := goodClaims(testResource)
				c.TokenType = "login"
				tok := makeToken(t, []byte(testSecret), c)
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusUnauthorized,
			wantNext:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			tc.setup(req)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rr.Code, tc.wantStatus)
			}
			if reached != tc.wantNext {
				t.Errorf("next reached: got %v, want %v", reached, tc.wantNext)
			}
		})
	}
}

func TestAudContains(t *testing.T) {
	tests := []struct {
		aud    jwt.ClaimStrings
		target string
		want   bool
	}{
		{jwt.ClaimStrings{"https://mcp.grill.poma-ai.com/"}, "https://mcp.grill.poma-ai.com/", true},
		{jwt.ClaimStrings{"https://other.com/"}, "https://mcp.grill.poma-ai.com/", false},
		{jwt.ClaimStrings{"https://a.com/", "https://mcp.grill.poma-ai.com/"}, "https://mcp.grill.poma-ai.com/", true},
		{jwt.ClaimStrings{}, "https://mcp.grill.poma-ai.com/", false},
		{nil, "https://mcp.grill.poma-ai.com/", false},
	}
	for _, tc := range tests {
		got := audContains(tc.aud, tc.target)
		if got != tc.want {
			t.Errorf("audContains(%v, %q) = %v, want %v", tc.aud, tc.target, got, tc.want)
		}
	}
}

func TestIsJWTStructure(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"aaa.bbb.ccc", true},
		{"poma_ak_notajwtatall", false},
		{"", false},
		{"aaa.bbb", false},
		{"aaa.bbb.ccc.ddd", true}, // still has two dots
	}
	for _, tc := range tests {
		if got := isJWTStructure(tc.s); got != tc.want {
			t.Errorf("isJWTStructure(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
