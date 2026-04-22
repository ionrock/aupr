package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	if err := validate(Defaults()); err != nil {
		t.Fatalf("defaults must validate, got %v", err)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUPR_CONFIG", filepath.Join(dir, "does-not-exist.toml"))
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.TickMinutes != 15 {
		t.Errorf("tick minutes = %d, want 15", cfg.Daemon.TickMinutes)
	}
}

func TestLoadExplicitMissingFileErrors(t *testing.T) {
	if _, err := Load("/does/not/exist.toml"); err == nil {
		t.Fatal("expected error for explicit missing config")
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTOML(&buf, Defaults()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "tick_minutes") {
		t.Errorf("expected tick_minutes in output, got: %s", buf.String())
	}
}

func TestExpandHome(t *testing.T) {
	p, err := expandHome("~/foo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(p, "~") {
		t.Errorf("expandHome did not expand: %s", p)
	}
	if got, _ := expandHome("/abs"); got != "/abs" {
		t.Errorf("absolute path mangled: %s", got)
	}
}
