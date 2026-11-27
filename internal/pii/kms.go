package pii

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tycheg/reasonr-engine/port"
)

// KeySize is the length of every key: the local master key, tenant and
// subject keys (AES-256).
const KeySize = 32

// ErrNoKMS: no master key KMS is set, but the model stores personal data.
var ErrNoKMS = errors.New("pii: no personal data master key KMS is set (crypto.kms in engine.yaml)")

// NoKMS means no KMS is set: wrapping and unwrapping tenant keys fail with
// ErrNoKMS. The store of a model without personal data does not call the KMS.
type NoKMS struct{}

func (NoKMS) Wrap(context.Context, string, []byte) ([]byte, error)   { return nil, ErrNoKMS }
func (NoKMS) Unwrap(context.Context, string, []byte) ([]byte, error) { return nil, ErrNoKMS }

// LocalKMS is a master key in process memory: the memory adapter (a random
// key, tests and --test) and local (a key from the environment or a file;
// development and air-gapped setups). Wrapping is AES-256-GCM, the AAD is
// the key context.
type LocalKMS struct {
	aead cipher.AEAD
	rand io.Reader // the wrap nonce; nil — crypto/rand
}

var _ port.KMS = (*LocalKMS)(nil)

// NewLocalKMS creates a KMS with the master key key (32 bytes).
func NewLocalKMS(key []byte) (*LocalKMS, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("pii: the master key must be %d bytes, not %d", KeySize, len(key))
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &LocalKMS{aead: aead}, nil
}

// NewMemoryKMS creates a KMS with a random master key: the data lives as
// long as the process.
func NewMemoryKMS() *LocalKMS {
	k, err := NewLocalKMS(RandomKey())
	if err != nil {
		panic(err)
	}
	return k
}

// ParseKey parses a master key in base64 (standard or URL).
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, fmt.Errorf("pii: the master key must be %d bytes, not %d", KeySize, len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("pii: the master key is not base64")
}

// Wrap wraps a key (port.KMS).
func (k *LocalKMS) Wrap(_ context.Context, keyContext string, plain []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+len(plain)+k.aead.Overhead())
	r := k.rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, plain, []byte(keyContext)), nil
}

// Unwrap unwraps a key (port.KMS).
func (k *LocalKMS) Unwrap(_ context.Context, keyContext string, wrapped []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(wrapped) < n+k.aead.Overhead() {
		return nil, errors.New("pii: the wrapped key is too short")
	}
	plain, err := k.aead.Open(nil, wrapped[:n], wrapped[n:], []byte(keyContext))
	if err != nil {
		return nil, fmt.Errorf("pii: the key does not unwrap with the master key (another key or corruption): %w", err)
	}
	return plain, nil
}

// WithRand returns a KMS with the same master key and the nonce source r:
// deterministic wrapping for copies of one state (Options.Rand).
func (k *LocalKMS) WithRand(r io.Reader) *LocalKMS { return &LocalKMS{aead: k.aead, rand: r} }

// RandomKey returns a new random key.
func RandomKey() []byte { return randomKey(rand.Reader) }

// randomKey returns a new key from the source r.
func randomKey(r io.Reader) []byte {
	b := make([]byte, KeySize)
	if _, err := io.ReadFull(r, b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return b
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
