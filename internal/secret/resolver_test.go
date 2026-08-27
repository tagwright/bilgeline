// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileResolution pins that a secret is read from a file under secretsDir
// and returned with its trailing newline trimmed.
func TestFileResolution(t *testing.T) {
	dir := t.TempDir()
	// A file written with a trailing newline, the common case for a secret
	// dropped in by a deploy step or `echo token > file`.
	if err := os.WriteFile(filepath.Join(dir, "ntfy-token"), []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	resolve := FileEnvResolver(dir)

	got, err := resolve("ntfy-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("resolved = %q, want %q (trailing newline trimmed)", got, "s3cr3t")
	}
}

// TestEnvFallback pins that a name with no file falls back to the
// BILGELINE_SECRET_<NAME> env var, with the same trailing-whitespace trim, and
// that "-" in the name maps to "_" in the env var.
func TestEnvFallback(t *testing.T) {
	dir := t.TempDir() // empty: force the env fallback
	resolve := FileEnvResolver(dir)

	// gatus-push-token -> BILGELINE_SECRET_GATUS_PUSH_TOKEN
	t.Setenv("BILGELINE_SECRET_GATUS_PUSH_TOKEN", "  pushtok\t\n")

	got, err := resolve("gatus-push-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "pushtok" {
		t.Errorf("resolved = %q, want %q (env, trimmed both ends)", got, "pushtok")
	}
}

// TestFileWinsOverEnv pins the resolution order: a file is preferred over the
// env fallback when both exist.
func TestFileWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dual"), []byte("fromfile"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Setenv("BILGELINE_SECRET_DUAL", "fromenv")
	resolve := FileEnvResolver(dir)

	got, err := resolve("dual")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "fromfile" {
		t.Errorf("resolved = %q, want fromfile (file wins over env)", got)
	}
}

// TestMissingSecret pins that a name with neither a file nor an env var is an
// error naming the secret, so a channel send fails loudly rather than sending
// an empty credential.
func TestMissingSecret(t *testing.T) {
	resolve := FileEnvResolver(t.TempDir())
	_, err := resolve("nope")
	if err == nil {
		t.Fatal("resolve: want error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q should name the missing secret", err)
	}
}

// TestEmptyName pins that an empty secret name is rejected up front.
func TestEmptyName(t *testing.T) {
	resolve := FileEnvResolver(t.TempDir())
	if _, err := resolve(""); err == nil {
		t.Fatal("resolve: want error for empty name, got nil")
	}
}

// TestDefaultSecretsDir pins that an empty secretsDir falls back to the package
// default rather than resolving against the process working directory.
func TestDefaultSecretsDir(t *testing.T) {
	resolve := FileEnvResolver("")
	// The default dir almost certainly does not exist in a test env, so a
	// missing name errors, and the error names the default path.
	_, err := resolve("whatever")
	if err == nil {
		t.Fatal("resolve: want error, got nil")
	}
	if !strings.Contains(err.Error(), DefaultSecretsDir) {
		t.Errorf("error %q should reference the default secrets dir %q", err, DefaultSecretsDir)
	}
}
