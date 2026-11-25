package secret

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadOrCreateAndReload(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "master.key")

	b1, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate(create): %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat(master.key): %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("master.key perm = %#o, want 0600", info.Mode().Perm())
	}

	sealed := b1.Seal("top-secret-password")
	if !strings.HasPrefix(sealed, prefix) {
		t.Fatalf("Seal() = %q, want %q prefix", sealed, prefix)
	}

	// Reload from the same key file and verify decryption succeeds.
	b2, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate(reload): %v", err)
	}
	got, err := b2.Open(sealed)
	if err != nil {
		t.Fatalf("Open(reloaded): %v", err)
	}
	if got != "top-secret-password" {
		t.Errorf("Open(reloaded) = %q, want %q", got, "top-secret-password")
	}

	// Corrupt key file must fail cleanly.
	badPath := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badPath, []byte("not-hex\n"), 0o600); err != nil {
		t.Fatalf("write bad key: %v", err)
	}
	if _, err := LoadOrCreate(badPath); err == nil {
		t.Errorf("LoadOrCreate(invalid key) succeeded, want error")
	}
}

func TestSealOpenRoundtripAndPlaintextBehavior(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "master.key")
	b, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Empty string passthrough.
	if got := b.Seal(""); got != "" {
		t.Errorf("Seal(\"\") = %q, want empty", got)
	}
	if got, err := b.Open(""); err != nil || got != "" {
		t.Errorf("Open(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}

	// Roundtrip with random nonces (same plaintext seals to distinct ciphertexts).
	plain := "db-password-!@#$%^&*()_+-=中文"
	c1 := b.Seal(plain)
	c2 := b.Seal(plain)
	if c1 == c2 {
		t.Errorf("two Seal calls produced identical ciphertext %q", c1)
	}
	for i, ct := range []string{c1, c2} {
		got, err := b.Open(ct)
		if err != nil {
			t.Fatalf("Open(c%d): %v", i+1, err)
		}
		if got != plain {
			t.Errorf("Open(c%d) = %q, want %q", i+1, got, plain)
		}
	}

	// Unencrypted plaintext (no "v1:" prefix) is rejected by Open and silently returns "" in MustOpen.
	if _, err := b.Open("plain-unencrypted-value"); err == nil {
		t.Errorf("Open(plaintext) succeeded, want error")
	}
}

func TestTamperRejectionAndMustOpenWarning(t *testing.T) {
	b1, err := LoadOrCreate(filepath.Join(t.TempDir(), "k1.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate k1: %v", err)
	}
	b2, err := LoadOrCreate(filepath.Join(t.TempDir(), "k2.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate k2: %v", err)
	}

	sealed := b1.Seal("sensitive-token")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		t.Fatalf("decode sealed payload: %v", err)
	}

	// Flip a byte in the ciphertext/tag.
	tamperedBytes := append([]byte(nil), raw...)
	tamperedBytes[len(tamperedBytes)-1] ^= 0xff
	tampered := prefix + base64.StdEncoding.EncodeToString(tamperedBytes)

	if _, err := b1.Open(tampered); err == nil {
		t.Errorf("Open(tampered) succeeded, want error")
	}
	if _, err := b2.Open(sealed); err == nil {
		t.Errorf("Open(wrong key) succeeded, want error")
	}
	if _, err := b1.Open(prefix + base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Errorf("Open(short ciphertext) succeeded, want error")
	}
	if _, err := b1.Open(prefix + "%%%not-base64%%%"); err == nil {
		t.Errorf("Open(invalid base64) succeeded, want error")
	}

	// Capture slog output to verify MustOpen logs warning on "v1:" failure
	// and stays quiet for unencrypted strings.
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	if got := b1.MustOpen("unencrypted-plain"); got != "" {
		t.Errorf("MustOpen(plain) = %q, want empty", got)
	}
	if logBuf.Len() != 0 {
		t.Errorf("MustOpen(plain) logged unexpectedly: %s", logBuf.String())
	}

	if got := b1.MustOpen(tampered); got != "" {
		t.Errorf("MustOpen(tampered) = %q, want empty", got)
	}
	if !strings.Contains(logBuf.String(), "secret decrypt failed") {
		t.Errorf("MustOpen(tampered) log = %q, want 'secret decrypt failed'", logBuf.String())
	}
}
