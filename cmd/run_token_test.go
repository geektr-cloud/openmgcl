package cmd

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestGenerateRunToken(t *testing.T) {
	first, err := generateRunToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateRunToken()
	if err != nil {
		t.Fatal(err)
	}

	if first == "" || second == "" {
		t.Fatal("generated token is empty")
	}
	if first == second {
		t.Fatal("two runs generated the same token")
	}

	raw, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("token is not base64url without padding: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("token entropy = %d bytes, want at least 32", len(raw))
	}
	if strings.ContainsAny(first, "+/=") {
		t.Fatalf("token contains non-URL-safe characters: %q", first)
	}
}

func TestResolveRunToken(t *testing.T) {
	t.Run("explicit token is preserved", func(t *testing.T) {
		called := false
		token, generated, err := resolveRunToken("fixed-token", func() (string, error) {
			called = true
			return "unused", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if token != "fixed-token" {
			t.Fatalf("token = %q, want fixed-token", token)
		}
		if generated {
			t.Fatal("explicit token reported as generated")
		}
		if called {
			t.Fatal("generator called for an explicit token")
		}
	})

	t.Run("empty token is generated", func(t *testing.T) {
		calls := 0
		token, generated, err := resolveRunToken("", func() (string, error) {
			calls++
			return "generated-token", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if token != "generated-token" {
			t.Fatalf("token = %q, want generated-token", token)
		}
		if !generated {
			t.Fatal("generated token not reported as generated")
		}
		if calls != 1 {
			t.Fatalf("generator calls = %d, want 1", calls)
		}
	})

	t.Run("generator failure is returned", func(t *testing.T) {
		wantErr := errors.New("no entropy")
		_, _, err := resolveRunToken("", func() (string, error) {
			return "", wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})
}
