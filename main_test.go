package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --------------------------------------------------------------------------- #
// loadDotEnv
// --------------------------------------------------------------------------- #

func TestLoadDotEnv_BasicParsing(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	content := `
# This is a comment
KEY1=value1
KEY2="quoted value"
KEY3='single quoted'

KEY4=no_quotes
`
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatalf("writing test .env: %v", err)
	}

	// Clear env keys before test.
	for _, k := range []string{"KEY1", "KEY2", "KEY3", "KEY4"} {
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"KEY1", "KEY2", "KEY3", "KEY4"} {
			os.Unsetenv(k)
		}
	})

	if err := loadDotEnv(envFile); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}

	tests := []struct{ key, want string }{
		{"KEY1", "value1"},
		{"KEY2", "quoted value"},
		{"KEY3", "single quoted"},
		{"KEY4", "no_quotes"},
	}
	for _, tc := range tests {
		if got := os.Getenv(tc.key); got != tc.want {
			t.Errorf("%s = %q; want %q", tc.key, got, tc.want)
		}
	}
}

func TestLoadDotEnv_DoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	os.Setenv("EXISTING_KEY", "original")
	t.Cleanup(func() { os.Unsetenv("EXISTING_KEY") })

	if err := os.WriteFile(envFile, []byte("EXISTING_KEY=overridden\n"), 0600); err != nil {
		t.Fatalf("writing test .env: %v", err)
	}

	if err := loadDotEnv(envFile); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}

	if got := os.Getenv("EXISTING_KEY"); got != "original" {
		t.Errorf("EXISTING_KEY = %q; expected original value to be preserved", got)
	}
}

func TestLoadDotEnv_MissingFileIsNotAnError(t *testing.T) {
	err := loadDotEnv("/tmp/this-file-does-not-exist-webex-scribe-test.env")
	if err != nil {
		t.Errorf("expected nil error for missing file, got: %v", err)
	}
}

func TestLoadDotEnv_IgnoresBlankAndCommentLines(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	content := "# comment\n\n   \nVALID=yes\n"
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatalf("writing test .env: %v", err)
	}

	os.Unsetenv("VALID")
	t.Cleanup(func() { os.Unsetenv("VALID") })

	if err := loadDotEnv(envFile); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}
	if got := os.Getenv("VALID"); got != "yes" {
		t.Errorf("VALID = %q; want %q", got, "yes")
	}
}

func TestLoadDotEnv_LineWithoutEquals(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	// Lines without '=' are silently skipped; should not cause an error.
	content := "NOTANASSIGNMENT\nGOOD=value\n"
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatalf("writing test .env: %v", err)
	}

	os.Unsetenv("GOOD")
	os.Unsetenv("NOTANASSIGNMENT")
	t.Cleanup(func() {
		os.Unsetenv("GOOD")
		os.Unsetenv("NOTANASSIGNMENT")
	})

	if err := loadDotEnv(envFile); err != nil {
		t.Fatalf("loadDotEnv unexpected error: %v", err)
	}
	if got := os.Getenv("GOOD"); got != "value" {
		t.Errorf("GOOD = %q; want %q", got, "value")
	}
}

func TestLoadDotEnv_UserConfigPrecedenceForWebexToken(t *testing.T) {
	userDir := t.TempDir()
	projectDir := t.TempDir()
	userEnv := filepath.Join(userDir, ".env")
	projectEnv := filepath.Join(projectDir, ".env")

	if err := os.WriteFile(userEnv, []byte("WEBEX_TOKEN=user-token\n"), 0600); err != nil {
		t.Fatalf("writing user .env: %v", err)
	}
	if err := os.WriteFile(projectEnv, []byte("WEBEX_TOKEN=stale-project-token\n"), 0600); err != nil {
		t.Fatalf("writing project .env: %v", err)
	}

	os.Unsetenv("WEBEX_TOKEN")
	t.Cleanup(func() { os.Unsetenv("WEBEX_TOKEN") })

	// Match startup order in main(): load user config first, then project .env.
	if err := loadDotEnv(userEnv); err != nil {
		t.Fatalf("loadDotEnv user config: %v", err)
	}
	if err := loadDotEnv(projectEnv); err != nil {
		t.Fatalf("loadDotEnv project .env: %v", err)
	}

	if got := os.Getenv("WEBEX_TOKEN"); got != "user-token" {
		t.Fatalf("WEBEX_TOKEN = %q; want %q", got, "user-token")
	}
}
