package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	tests := []struct {
		name     string
		in       string
		gone     string
		wantKind string
	}{
		{"jwt", "token was " + jwt + " ok", jwt, "jwt"},
		{"bearer", "Authorization: Bearer abc123.def-456_xyz", "abc123.def-456_xyz", "bearer"},
		{"short value pair", "client_secret: s3cr3t!", "s3cr3t!", "secret_pair"},
		{"password pair", "db password=hunter2 connect", "hunter2", "secret_pair"},
		{"api key json", `{"api_key": "AKIAIOSFODNN7EXAMPLE"}`, "AKIAIOSFODNN7EXAMPLE", "api_key"},
		{"token env style", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "api_key"},
		{"vendor key", "using sk-ant-abcdefghijklmnopqrstuv for calls", "sk-ant-abcdefghijklmnopqrstuv", "api_key"},
		{"email", "owner alice@example.com paged", "alice@example.com", "email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if strings.Contains(got.Text, tt.gone) {
				t.Fatalf("secret survived: %q", got.Text)
			}
			if got.Counts[tt.wantKind] == 0 {
				t.Fatalf("kind %q not counted: %v", tt.wantKind, got.Counts)
			}
		})
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	in := "OOMKilled exit code 137, restarts 7, image polinux/stress, memory 512Mi, password policy unchanged"
	got := Redact(in)
	if got.Text != in {
		t.Fatalf("changed ordinary text: %q", got.Text)
	}
	if len(got.Counts) != 0 {
		t.Fatalf("unexpected counts: %v", got.Counts)
	}
}

func TestRedactKeepsKeyName(t *testing.T) {
	got := Redact("password=hunter2")
	if !strings.HasPrefix(got.Text, "password=") {
		t.Fatalf("key name lost: %q", got.Text)
	}
}

func TestRedactIdempotent(t *testing.T) {
	once := Redact("token=abc123 mail bob@example.org")
	twice := Redact(once.Text)
	if once.Text != twice.Text {
		t.Fatalf("not idempotent: %q vs %q", once.Text, twice.Text)
	}
}

func TestRedactSurvivesInvisibleCharacters(t *testing.T) {
	for name, in := range map[string]string{
		"nul inside key":        "pass\x00word=hunter2",
		"escape inside key":     "to\x1b[0mken=abc123",
		"zero width in key":     "pass\u200bword=hunter2",
		"bidi override in mail": "mail alice@exa\u202ample.com",
	} {
		t.Run(name, func(t *testing.T) {
			got := Redact(in).Text
			for _, secret := range []string{"hunter2", "abc123", "alice@exa"} {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q survived: %q", secret, got)
				}
			}
			for _, r := range got {
				if r == 0 || r == 0x1b || r == 0x200b || r == 0x202e {
					t.Fatalf("invisible character %U survived: %q", r, got)
				}
			}
		})
	}
}

func TestRedactKeepsNewlinesAndTabs(t *testing.T) {
	in := "line one\n\tindented line two\nline three"
	if got := Redact(in).Text; got != in {
		t.Fatalf("layout changed: %q", got)
	}
}
