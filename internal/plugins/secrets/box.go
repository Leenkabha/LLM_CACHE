// Package secrets encrypts plugin secrets at rest and scrubs sensitive values
// out of anything that might be logged or returned by an API.
//
// Secrets are sealed with AES-256-GCM under a master key supplied through the
// PLUGIN_SECRET_KEY environment variable (base64 of 32 random bytes). Every
// encryption uses a fresh random 96-bit nonce, and the ciphertext is bound to
// its plugin and secret name so blobs cannot be swapped between records.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ErrKeyMismatch means the blob was sealed under a different master key.
var ErrKeyMismatch = errors.New("plugin secrets were encrypted with a different PLUGIN_SECRET_KEY; restore the original key or re-enter the secrets")

// ErrCorrupt means the blob failed authentication under the right key.
var ErrCorrupt = errors.New("stored plugin secret failed authentication and may be corrupted")

// Sealed is an encrypted secret as persisted. It never leaves the registry.
type Sealed struct {
	Version    int    `json:"v"`
	KeyID      string `json:"kid"`
	Nonce      string `json:"n"`
	Ciphertext string `json:"c"`
}

// Box seals and opens secrets under one master key.
type Box struct {
	aead  cipher.AEAD
	keyID string
}

// NewBox builds a Box from a base64-encoded 32-byte key.
func NewBox(b64 string) (*Box, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, errors.New("PLUGIN_SECRET_KEY is not set; generate one with: openssl rand -base64 32")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("PLUGIN_SECRET_KEY must be base64 (generate one with: openssl rand -base64 32)")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("PLUGIN_SECRET_KEY must decode to exactly 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte("llmcache-plugin-key-id:"), key...))
	return &Box{aead: aead, keyID: hex.EncodeToString(sum[:6])}, nil
}

func aad(pluginID, name string) []byte { return []byte(pluginID + "\x00" + name) }

// Seal encrypts value for the given plugin and secret name.
func (b *Box) Seal(pluginID, name, value string) (Sealed, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, fmt.Errorf("generate nonce: %w", err)
	}
	ct := b.aead.Seal(nil, nonce, []byte(value), aad(pluginID, name))
	return Sealed{
		Version:    1,
		KeyID:      b.keyID,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// Open decrypts a sealed secret.
func (b *Box) Open(pluginID, name string, s Sealed) (string, error) {
	if s.KeyID != b.keyID {
		return "", ErrKeyMismatch
	}
	nonce, err := base64.StdEncoding.DecodeString(s.Nonce)
	if err != nil || len(nonce) != b.aead.NonceSize() {
		return "", ErrCorrupt
	}
	ct, err := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return "", ErrCorrupt
	}
	pt, err := b.aead.Open(nil, nonce, ct, aad(pluginID, name))
	if err != nil {
		return "", ErrCorrupt
	}
	return string(pt), nil
}

// ---- scrubbing ---------------------------------------------------------------

var scrubbers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(bearer|basic)?\s*[^\s,;"']+`), `${1}[redacted]`},
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`), `Bearer [redacted]`},
	{regexp.MustCompile(`(?i)([?&](?:key|api_?key|token|access_token|secret|password|sig|signature|auth)=)[^&\s"']+`), `${1}[redacted]`},
	// key=value / key: value where the key names a credential, anywhere in text.
	{regexp.MustCompile(`(?i)\b((?:access[_-]?|api[_-]?|auth[_-]?|private[_-]?)?(?:token|secret|password|passwd|key))(\s*[=:]\s*)[^\s,;"'&]{4,}`), `${1}${2}[redacted]`},
	{regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`), `${1}[redacted]@`},
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`), `[redacted]`},
	{regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`), `[redacted]`},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), `[redacted]`},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), `[redacted]`},
}

// Scrub removes the given known secret values and anything that looks like a
// credential from s. It is applied to every log line and error message that is
// stored or returned to an administrator.
func Scrub(s string, known ...string) string {
	for _, k := range known {
		if len(k) >= 4 {
			s = strings.ReplaceAll(s, k, "[redacted]")
		}
	}
	for _, sc := range scrubbers {
		s = sc.re.ReplaceAllString(s, sc.with)
	}
	return s
}

// SafeURL strips user info, query string and fragment from a URL so only
// scheme://host/path is ever displayed or stored as source metadata.
func SafeURL(raw string) string {
	s := raw
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		host := rest
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			host = rest[:j]
		}
		if k := strings.LastIndexByte(host, '@'); k >= 0 {
			s = s[:i+3] + rest[k+1:]
		}
	}
	return s
}
