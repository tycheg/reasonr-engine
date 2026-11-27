package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/port"
)

// Signer signs the engine's JWT for the integration service (RFC §18):
// ES256, iss is Issuer, aud the external system, tenant, jti the request's
// idempotency key, iat, exp. The service takes the public keys from the
// engine's JWKS (GET /.well-known/jwks.json): the signing key and Also are
// there, so the key changes without failures while the service still
// trusts the previous one.
type Signer struct {
	Key    *ecdsa.PrivateKey
	Kid    string // "" — the key thumbprint (RFC 7638)
	Issuer string
	// Also holds more public keys in JWKS: during rotation the next one
	// (before switching the signature) and the previous one (after).
	Also []PublicKey
	TTL  time.Duration // the token lifetime; 0 — 5 min
	Now  func() time.Time

	once sync.Once
	jwks []byte
}

// PublicKey is an ES256 public key in JWKS.
type PublicKey struct {
	Kid string // "" — the key thumbprint
	Key *ecdsa.PublicKey
}

var _ ports.RequestSigner = (*Signer)(nil)

// GenerateSigner creates a signer with a random key — reasonrd --test.
func GenerateSigner(issuer string) (*Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Signer{Key: key, Issuer: issuer}, nil
}

// Token signs the token of the request id to the system system in the tenant tenant.
func (s *Signer) Token(system string, tenant port.Tenant, id string) (string, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	ttl := s.TTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	claims := map[string]any{"iss": s.Issuer, "aud": system, "tenant": string(tenant), "jti": id,
		"iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	return SignWith("ES256", s.kid(), claims, func(signed []byte) ([]byte, error) {
		digest := sha256.Sum256(signed)
		r, sv, err := ecdsa.Sign(rand.Reader, s.Key, digest[:])
		if err != nil {
			return nil, err
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		sv.FillBytes(sig[32:])
		return sig, nil
	})
}

func (s *Signer) kid() string {
	if s.Kid != "" {
		return s.Kid
	}
	return Thumbprint(&s.Key.PublicKey)
}

// JWKS returns the public key set: the signing key and Also.
func (s *Signer) JWKS() []byte {
	s.once.Do(func() {
		keys := []any{jwk(s.kid(), &s.Key.PublicKey)}
		for _, k := range s.Also {
			kid := k.Kid
			if kid == "" {
				kid = Thumbprint(k.Key)
			}
			keys = append(keys, jwk(kid, k.Key))
		}
		s.jwks, _ = json.Marshal(map[string]any{"keys": keys})
	})
	return s.jwks
}

func jwk(kid string, k *ecdsa.PublicKey) map[string]string {
	x, y := coords(k)
	return map[string]string{"kty": "EC", "crv": "P-256", "kid": kid, "use": "sig", "alg": "ES256", "x": x, "y": y}
}

// coords are the coordinates of a P-256 key point in base64url (32 bytes each).
func coords(k *ecdsa.PublicKey) (x, y string) {
	var point []byte
	if e, err := k.ECDH(); err == nil {
		point = e.Bytes() // 0x04 || X || Y
	}
	if len(point) != 65 {
		return "", ""
	}
	return base64.RawURLEncoding.EncodeToString(point[1:33]), base64.RawURLEncoding.EncodeToString(point[33:])
}

// Thumbprint is the EC public key thumbprint by RFC 7638: the default kid.
func Thumbprint(k *ecdsa.PublicKey) string {
	x, y := coords(k)
	b := []byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParseECKey reads a P-256 key from PEM: a private one (PKCS #8 or SEC 1)
// with its public one; a public one (PKIX) — only public, then private is nil.
func ParseECKey(data []byte) (*ecdsa.PrivateKey, *ecdsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, nil, errors.New("not PEM")
	}
	var key any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "PUBLIC KEY":
		key, err = x509.ParsePKIXPublicKey(block.Bytes)
	default:
		return nil, nil, fmt.Errorf("PEM %q: expected an EC key", block.Type)
	}
	if err != nil {
		return nil, nil, err
	}
	var pub *ecdsa.PublicKey
	priv, _ := key.(*ecdsa.PrivateKey)
	if priv != nil {
		pub = &priv.PublicKey
	} else if pub, _ = key.(*ecdsa.PublicKey); pub == nil {
		return nil, nil, errors.New("not an EC key")
	}
	if pub.Curve != elliptic.P256() {
		return nil, nil, errors.New("the key is not on curve P-256 (ES256)")
	}
	return priv, pub, nil
}
