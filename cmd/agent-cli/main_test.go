package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"version"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent-cli") || !strings.Contains(out.String(), version) {
		t.Errorf("out=%q", out.String())
	}
}

func TestUnknownCommandExits2WithUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"bogus"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), "Usage") || !strings.Contains(errOut.String(), "bogus") {
		t.Errorf("stderr=%q", errOut.String())
	}
}

func TestNoCommandExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), "Usage") {
		t.Errorf("stderr=%q", errOut.String())
	}
}

func TestBadFlagExits2(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestHelpExits0(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-h"}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d", code)
	}
}

func TestVersionRejectsExtraArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestUsageListsMCP(t *testing.T) {
	var out, errOut bytes.Buffer
	run(nil, &out, &errOut)
	if !strings.Contains(errOut.String(), "mcp") || !strings.Contains(errOut.String(), "serve the read-only tools to an MCP client over stdio") {
		t.Errorf("stderr=%q", errOut.String())
	}
}
