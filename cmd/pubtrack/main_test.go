package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pubtrack/internal/digest"
)

func TestWriteDigestDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	d := digest.Digest{Date: "2026-10-19", Items: []digest.Item{{}}}
	first, err := writeDigest(dir, d)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeDigest(dir, d)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "digest-2026-10-19.md" || filepath.Base(second) != "digest-2026-10-19-2.md" {
		t.Errorf("got %s then %s", first, second)
	}
	if _, err := os.Stat(strings.TrimSuffix(second, ".md") + ".json"); err != nil {
		t.Errorf("JSON not written next to markdown: %v", err)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("no usage text: %q", errOut.String())
	}
}

func TestRunWithBadConfigFailsCleanly(t *testing.T) {
	var out, errOut bytes.Buffer
	code := realMain([]string{"run", "-config", filepath.Join(t.TempDir(), "missing.json")}, &out, &errOut)
	if code != 1 || !strings.HasPrefix(errOut.String(), "pubtrack: reading config:") {
		t.Errorf("code %d, stderr %q", code, errOut.String())
	}
}
