package pii

import (
	"crypto/hmac"
	"crypto/sha256"
)

// hkdf derives a key of length n from a secret by RFC 5869 (HKDF-SHA-256
// without salt). crypto/hkdf appeared only in Go 1.24.
func hkdf(secret []byte, info string, n int) []byte {
	ext := hmac.New(sha256.New, make([]byte, sha256.Size))
	ext.Write(secret)
	prk := ext.Sum(nil)
	out := make([]byte, 0, n+sha256.Size)
	var t []byte
	for c := byte(1); len(out) < n; c++ {
		m := hmac.New(sha256.New, prk)
		m.Write(t)
		m.Write([]byte(info))
		m.Write([]byte{c})
		t = m.Sum(nil)
		out = append(out, t...)
	}
	return out[:n]
}
