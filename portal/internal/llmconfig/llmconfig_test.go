// llmconfig tests: AES-GCM round-trip, master key derivation modes,
// and the SSRF guard. The DB-dependent resolver is exercised through
// the httpapi package's A2 tests (those set up the full Server with a
// real Postgres); this file covers the pure crypto + IP logic.
package llmconfig

import (
	"os"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	// Use a raw 32-byte key so MasterKey skips the KDF (faster test).
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	enc := base64Encode(key)
	getenvOverride = func(string) string { return enc }
	t.Cleanup(func() { getenvOverride = os.Getenv })
	masterKeyInit = false // force re-derivation

	plaintext := []byte("wh_supersecret_key_12345")
	ciphertext, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(ciphertext) == string(plaintext) {
		t.Fatal("ciphertext equals plaintext — encryption did nothing")
	}
	got, err := Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestMasterKeyPassphraseMode(t *testing.T) {
	getenvOverride = func(string) string { return "a-short-passphrase" }
	t.Cleanup(func() { getenvOverride = os.Getenv })
	masterKeyInit = false

	key1, err := MasterKey()
	if err != nil {
		t.Fatalf("master key 1: %v", err)
	}
	if len(key1) != 32 {
		t.Fatalf("master key len = %d, want 32", len(key1))
	}
	// Same passphrase → same key (Argon2id is deterministic).
	masterKeyInit = false
	key2, err := MasterKey()
	if err != nil {
		t.Fatalf("master key 2: %v", err)
	}
	if string(key1) != string(key2) {
		t.Fatal("passphrase mode is not deterministic")
	}
}

func TestMasterKeyMissing(t *testing.T) {
	getenvOverride = func(string) string { return "" }
	t.Cleanup(func() { getenvOverride = os.Getenv })
	masterKeyInit = false

	if _, err := MasterKey(); err == nil {
		t.Fatal("MasterKey with no env var should error")
	}
}

func TestDecryptTamperedFails(t *testing.T) {
	key := make([]byte, 32)
	enc := base64Encode(key)
	getenvOverride = func(string) string { return enc }
	t.Cleanup(func() { getenvOverride = os.Getenv })
	masterKeyInit = false

	ciphertext, err := Encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Flip a byte in the ciphertext body (after the nonce).
	tampered := append([]byte{}, ciphertext...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := Decrypt(tampered); err == nil {
		t.Fatal("decrypt of tampered ciphertext should fail (GCM auth tag)")
	}
}

func TestIsPrivateBaseURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:4000", true},              // loopback
		{"http://10.0.0.1:4000", true},               // 10/8
		{"http://172.16.0.1:4000", true},             // 172.16/12
		{"http://192.168.1.1:4000", true},            // 192.168/16
		{"http://169.254.169.254", true},             // cloud metadata
		{"http://[::1]:4000", true},                  // IPv6 loopback
		{"http://[fc00::1]:4000", true},              // ULA
		{"http://[fe80::1]:4000", true},              // link-local
		{"http://localhost:4000", true},              // resolves to 127.0.0.1
		{"https://api.openai.com", false},            // public
		{"https://h11hp9dd1y90-d.space-z.ai", false}, // public (WebHub)
		{"http://not-a-real-host.invalid", true},     // DNS fail → refuse
		{"::not-a-url::", true},                      // unparseable → refuse
	}
	for _, c := range cases {
		got := IsPrivateBaseURL(c.url)
		if got != c.want {
			t.Errorf("IsPrivateBaseURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestLabelFromKey(t *testing.T) {
	if got := LabelFromKey("wh_abcdef1234"); got != "1234" {
		t.Errorf("label = %q, want 1234", got)
	}
	if got := LabelFromKey("ab"); got != "" {
		t.Errorf("short key label = %q, want ''", got)
	}
}

// base64Encode is a tiny wrapper so the test does not import
// encoding/base64 in every case line.
func base64Encode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, 44)
	for i := 0; i < len(b); i += 3 {
		c := b[i]
		out = append(out, alphabet[c>>2])
		if i+1 < len(b) {
			out = append(out, alphabet[((c&3)<<4)|(b[i+1]>>4)])
		} else {
			out = append(out, alphabet[(c&3)<<4], '=', '=', '=')
			break
		}
		if i+2 < len(b) {
			out = append(out, alphabet[((b[i+1]&15)<<2)|(b[i+2]>>6)], alphabet[b[i+2]&63])
		} else {
			out = append(out, alphabet[(b[i+1]&15)<<2], '=')
			break
		}
	}
	return string(out)
}
