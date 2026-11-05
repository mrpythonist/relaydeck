package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.ListenAddr != ":8080" || c.Workers != 4 || c.MaxPayloadBytes != 1<<20 || c.DataDir != "./data" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("RELAYDECK_LISTEN_ADDR", ":9090")
	t.Setenv("RELAYDECK_WORKERS", "8")
	t.Setenv("RELAYDECK_ALLOW_PRIVATE_DESTINATIONS", "true")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9090" || c.Workers != 8 || !c.AllowPrivate {
		t.Fatalf("env overrides wrong: %+v", c)
	}
}

func TestValidateErrors(t *testing.T) {
	c := Defaults()
	c.Workers = 0
	if err := c.Validate(); err == nil {
		t.Fatal("workers=0 should fail validation")
	}
	c = Defaults()
	c.MaxPayloadBytes = 0
	if err := c.Validate(); err == nil {
		t.Fatal("max payload 0 should fail validation")
	}
}

func TestYAMLLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "relaydeck.yaml")
	if err := os.WriteFile(p, []byte("listen_addr: :7070\nworkers: 2\nsources:\n  - name: stripe\n    secret: whsec_abc\ndestinations:\n  - name: api\n    url: https://example.com/hook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":7070" || c.Workers != 2 || len(c.Sources) != 1 || len(c.Destinations) != 1 {
		t.Fatalf("yaml load wrong: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("yaml config should validate: %v", err)
	}
}

func TestYAMLInvalid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(p, []byte(":\n  - [\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("invalid YAML should error")
	}
}
