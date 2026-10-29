package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestConsoleURLInUserMessages pins that every message sending a user to the
// console follows POMA_CONSOLE_URL, so the index4ai MCP never points at the POMA
// console, and that the default stays console.poma-ai.com.
func TestConsoleURLInUserMessages(t *testing.T) {
	protected := []byte(`{"code":403,"reason":"project_protected"}`)
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "unset uses the POMA console", env: "", want: "https://console.poma-ai.com"},
		{name: "override", env: "https://console-dev.index4.ai", want: "https://console-dev.index4.ai"},
		{name: "trailing slash and space trimmed", env: " https://console.index4.ai/ ", want: "https://console.index4.ai"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POMA_CONSOLE_URL", tt.env)
			if got := consoleURL(); got != tt.want {
				t.Fatalf("consoleURL() = %q, want %q", got, tt.want)
			}
			msgs := map[string]string{}
			msgs["402"], _ = interpretAuthError(context.Background(), "tok", http.StatusPaymentRequired, nil, "op")
			msgs["401"], _ = interpretAuthError(context.Background(), "tok", http.StatusUnauthorized, nil, "op")
			msgs["403 protected"], _ = interpretAuthError(context.Background(), "tok", http.StatusForbidden, protected, "op")
			_, out, err := GrillExplain(context.Background(), nil, GrillExplainInput{})
			if err != nil {
				t.Fatal(err)
			}
			msgs["explain"] = out.Explanation
			// Exact text around the URL: every verb is %s, so a swapped Sprintf
			// argument would still contain the URL somewhere and go vet can't see it.
			around := map[string]string{
				"402":           "Visit " + tt.want + " to check your usage",
				"401":           "Generate a valid API key at " + tt.want + " and set it",
				"403 protected": "Generate one at " + tt.want + " in the project settings",
				"explain":       "Get API key from " + tt.want + "\n",
			}
			for k, m := range msgs {
				if !strings.Contains(m, around[k]) {
					t.Errorf("%s message lacks %q: %q", k, around[k], m)
				}
				if !strings.Contains(m, tt.want) {
					t.Errorf("%s message does not contain %q: %q", k, tt.want, m)
				}
				if tt.want != defaultConsoleURL && strings.Contains(m, defaultConsoleURL) {
					t.Errorf("%s message still names %s: %q", k, defaultConsoleURL, m)
				}
			}
			if strings.Contains(out.Explanation, "{{CONSOLE_URL}}") {
				t.Error("explanation still carries the {{CONSOLE_URL}} placeholder")
			}
		})
	}
}
