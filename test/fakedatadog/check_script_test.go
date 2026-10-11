package fakedatadog_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidsugianto/upsera-agent/test/fakedatadog"
)

const (
	apiKey = fakedatadog.ValidAPIKey
	appKey = "fakeappkey-" + fakedatadog.AppKeyLast4 // ends in the last4 the fake lists
)

// runCheck runs hack/datadog-readonly-check.sh against the fake server. DD_SITE
// is an unresolvable name on purpose: if the script ignored DD_API_URL it would
// fail to connect instead of reaching the real Datadog.
func runCheck(t *testing.T, serverURL, api, app string, args ...string) (code int, out string) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			// In CI a missing tool must not turn the check into a silent pass.
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is required in CI but was not found on PATH", tool)
			}
			t.Skipf("%s not installed", tool)
		}
	}
	script, err := filepath.Abs("../../hack/datadog-readonly-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = []string{
		"PATH=" + pathEnv(),
		"DD_API_KEY=" + api,
		"DD_APP_KEY=" + app,
		"DD_SITE=fake.invalid",
		"DD_API_URL=" + serverURL,
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	out = buf.String()
	if err == nil {
		return 0, out
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out
	}
	t.Fatalf("run script: %v", err)
	return -1, out
}

func pathEnv() string {
	p, _ := exec.LookPath("bash")
	return filepath.Dir(p) + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin"
}

func TestCheckScript(t *testing.T) {
	tests := []struct {
		name     string
		scenario fakedatadog.Scenario
		api, app string
		args     []string
		wantCode int
		wantOut  []string
	}{
		{"read-only scopes pass", fakedatadog.ReadOnly, apiKey, appKey, nil, 0,
			[]string{"OK: application key scopes are read-only", "monitors_read"}},
		{"a write scope fails", fakedatadog.HasWriteScope, apiKey, appKey, nil, 1,
			[]string{"monitors_write ALLOWS WRITES", "FAIL"}},
		{"an unscoped key fails", fakedatadog.Unscoped, apiKey, appKey, nil, 1,
			[]string{"UNSCOPED"}},
		{"listing forbidden is unverified", fakedatadog.ListForbidden, apiKey, appKey, nil, 3,
			[]string{"cannot verify application key scopes", "HTTP 403"}},
		{"listing forbidden with --allow-unverified", fakedatadog.ListForbidden, apiKey, appKey,
			[]string{"--allow-unverified"}, 0, []string{"continuing because --allow-unverified"}},
		{"key not in the list is unverified", fakedatadog.KeyNotListed, apiKey, appKey, nil, 3,
			[]string{"not in the current user's application key list"}},
		{"shared last four, all read-only", fakedatadog.SharedLast4ReadOnly, apiKey, appKey, nil, 0,
			[]string{"2 application keys share the last four"}},
		{"shared last four, one writes", fakedatadog.SharedLast4OneWrites, apiKey, appKey, nil, 1,
			[]string{"ALLOWS WRITES"}},
		{"key on the second page is found", fakedatadog.Paginated, apiKey, appKey, nil, 0,
			[]string{"OK: application key scopes are read-only"}},
		{"an invalid API key fails", fakedatadog.ReadOnly, "not-the-api-key", appKey, nil, 1,
			[]string{"INVALID (HTTP 403)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakedatadog.New(tt.scenario)
			defer srv.Close()
			code, out := runCheck(t, srv.URL, tt.api, tt.app, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", code, tt.wantCode, out)
			}
			for _, w := range tt.wantOut {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q\n%s", w, out)
				}
			}
			for _, secret := range []string{tt.api, tt.app} {
				if strings.Contains(out, secret) {
					t.Errorf("output leaks a key (%q)", secret)
				}
			}
		})
	}
}

func TestCheckScriptRefusesNonLoopbackOverride(t *testing.T) {
	for _, u := range []string{"http://example.com:8080", "https://api.datadoghq.com", "http://10.0.0.5:9"} {
		code, out := runCheck(t, u, apiKey, appKey)
		if code != 2 || !strings.Contains(out, "DD_API_URL") {
			t.Errorf("%s: exit %d, want 2 mentioning DD_API_URL\n%s", u, code, out)
		}
	}
}
