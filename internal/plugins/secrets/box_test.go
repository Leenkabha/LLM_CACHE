package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := NewBox(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Seal("p1", "API_TOKEN", "hunter2-value")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Ciphertext, "hunter2") {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := b.Open("p1", "API_TOKEN", s)
	if err != nil || got != "hunter2-value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestNoncesAreUnique(t *testing.T) {
	b, _ := NewBox(newKey(t))
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		s, _ := b.Seal("p", "N", "same plaintext")
		if seen[s.Nonce] {
			t.Fatalf("nonce reused after %d encryptions", i)
		}
		seen[s.Nonce] = true
	}
}

func TestSameValueEncryptsDifferently(t *testing.T) {
	b, _ := NewBox(newKey(t))
	a, _ := b.Seal("p", "N", "v")
	c, _ := b.Seal("p", "N", "v")
	if a.Ciphertext == c.Ciphertext {
		t.Fatal("identical ciphertexts for identical plaintext")
	}
}

func TestWrongKeyIsAClearError(t *testing.T) {
	b1, _ := NewBox(newKey(t))
	b2, _ := NewBox(newKey(t))
	s, _ := b1.Seal("p", "N", "v")
	if _, err := b2.Open("p", "N", s); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
}

func TestCiphertextBoundToPluginAndName(t *testing.T) {
	b, _ := NewBox(newKey(t))
	s, _ := b.Seal("p1", "A", "v")
	if _, err := b.Open("p2", "A", s); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("other plugin: err = %v", err)
	}
	if _, err := b.Open("p1", "B", s); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("other secret name: err = %v", err)
	}
}

func TestTamperedCiphertextRejected(t *testing.T) {
	b, _ := NewBox(newKey(t))
	s, _ := b.Seal("p", "N", "value")
	raw, _ := base64.StdEncoding.DecodeString(s.Ciphertext)
	raw[0] ^= 1
	s.Ciphertext = base64.StdEncoding.EncodeToString(raw)
	if _, err := b.Open("p", "N", s); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}

func TestBadKeys(t *testing.T) {
	for name, k := range map[string]string{
		"empty":     "",
		"not b64":   "!!!",
		"too short": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"too long":  base64.StdEncoding.EncodeToString(make([]byte, 64)),
	} {
		if _, err := NewBox(k); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestScrub(t *testing.T) {
	in := "GET https://h/x?api_key=abc123&ok=1 Authorization: Bearer abcdefghijklmnop user https://bob:pw@host/y sk-abcdefghijklmnopqrstuv known-secret-value"
	out := Scrub(in, "known-secret-value")
	for _, leak := range []string{"abc123", "abcdefghijklmnop", "bob:pw", "sk-abcdefghij", "known-secret-value"} {
		if strings.Contains(out, leak) {
			t.Errorf("Scrub leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "ok=1") {
		t.Errorf("Scrub removed harmless content: %s", out)
	}
}

func TestScrubRemovesKeyValueCredentialsInFreeText(t *testing.T) {
	for _, in := range []string{
		"registry said: unauthorized; token=abc123secret456",
		"failed: password: hunter2hunter2",
		"env API_KEY=zzzzzzzz1234 was rejected",
		"secret = topsecretvalue99 leaked",
	} {
		out := Scrub(in)
		for _, leak := range []string{"abc123secret456", "hunter2hunter2", "zzzzzzzz1234", "topsecretvalue99"} {
			if strings.Contains(out, leak) {
				t.Errorf("Scrub(%q) leaked %q: %s", in, leak, out)
			}
		}
	}
	if got := Scrub("the token was refused"); got != "the token was refused" {
		t.Errorf("Scrub altered harmless prose: %q", got)
	}
}

func TestSafeURL(t *testing.T) {
	if got := SafeURL("https://u:p@example.com/path?token=abc#frag"); got != "https://example.com/path" {
		t.Fatalf("SafeURL = %q", got)
	}
}
