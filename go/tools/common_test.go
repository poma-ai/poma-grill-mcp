package tools

import (
	"context"
	"testing"
)

func TestEnvTokenPrecedence(t *testing.T) {
	t.Setenv("POMA_GRILL_API_KEY", "")
	t.Setenv("POMA_API_KEY", "")
	if tok, name := envToken(); tok != "" || name != "" {
		t.Fatalf("unset: got (%q, %q)", tok, name)
	}

	t.Setenv("POMA_API_KEY", "acc")
	if tok, name := envToken(); tok != "acc" || name != "POMA_API_KEY" {
		t.Fatalf("account only: got (%q, %q)", tok, name)
	}
	if got := tokenSource(context.Background(), ""); got != "POMA_API_KEY env var" {
		t.Fatalf("tokenSource account only: got %q", got)
	}

	t.Setenv("POMA_GRILL_API_KEY", "proj")
	if tok, name := envToken(); tok != "proj" || name != "POMA_GRILL_API_KEY" {
		t.Fatalf("both set, project key must win: got (%q, %q)", tok, name)
	}
	if got := getToken(context.Background(), ""); got != "proj" {
		t.Fatalf("getToken env fallback: got %q", got)
	}
	if got := getToken(context.Background(), "arg"); got != "arg" {
		t.Fatalf("getToken explicit arg must win: got %q", got)
	}
	if got := tokenSource(context.Background(), ""); got != "POMA_GRILL_API_KEY env var" {
		t.Fatalf("tokenSource: got %q", got)
	}
}
