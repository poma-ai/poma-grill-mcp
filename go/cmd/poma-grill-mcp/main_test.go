package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCrossOriginProtection covers the header shapes real clients send. The
// rows that matter most are the allowed ones: a non-browser MCP client sends
// neither Sec-Fetch-Site nor Origin.
func TestCrossOriginProtection(t *testing.T) {
	const host = "mcp.example.com"

	tests := []struct {
		name    string
		trusted string
		method  string
		headers map[string]string
		allowed bool
	}{
		{name: "non-browser client sends neither header", method: "POST", allowed: true},
		{name: "same-origin", method: "POST", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, allowed: true},
		{name: "direct navigation", method: "POST", headers: map[string]string{"Sec-Fetch-Site": "none"}, allowed: true},
		{name: "old browser, Origin matches Host", method: "POST", headers: map[string]string{"Origin": "https://" + host}, allowed: true},
		{name: "GET is a safe method", method: "GET", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, allowed: true},
		{name: "OPTIONS is a safe method", method: "OPTIONS", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, allowed: true},

		{name: "cross-site is blocked", method: "POST", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, allowed: false},
		{name: "sibling subdomain is cross-origin", method: "POST", headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://console.example.com"}, allowed: false},
		{name: "old browser, Origin differs from Host", method: "POST", headers: map[string]string{"Origin": "https://evil.example"}, allowed: false},

		{
			name:    "a trusted origin is allowed cross-site",
			trusted: "https://console.example.com",
			method:  "POST",
			headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://console.example.com"},
			allowed: true,
		},
		{
			name:    "trusting one origin does not trust another",
			trusted: "https://console.example.com",
			method:  "POST",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
			allowed: false,
		},
		{
			name:    "a list is parsed, with spaces and empty entries tolerated",
			trusted: " https://a.example.com , , https://console.example.com ",
			method:  "POST",
			headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://console.example.com"},
			allowed: true,
		},
		{
			name:    "an invalid entry is skipped without dropping the valid ones",
			trusted: "not a url,https://console.example.com",
			method:  "POST",
			headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://console.example.com"},
			allowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reached bool
			h := crossOriginProtection(tt.trusted).Handler(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

			req := httptest.NewRequest(tt.method, "https://"+host+"/", nil)
			req.Host = host
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if reached != tt.allowed {
				t.Errorf("reached handler = %v, want %v (status %d)", reached, tt.allowed, rec.Code)
			}
			if !tt.allowed && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
		})
	}
}

// TestCrossOriginProtectionEmptyEnv is the default deployment: no trusted
// origins configured, non-browser clients still work.
func TestCrossOriginProtectionEmptyEnv(t *testing.T) {
	for _, trusted := range []string{"", "   ", ",", " , "} {
		var reached bool
		h := crossOriginProtection(trusted).Handler(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		req := httptest.NewRequest("POST", "https://mcp.example.com/", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !reached {
			t.Errorf("trusted=%q blocked a non-browser request", trusted)
		}
	}
}
