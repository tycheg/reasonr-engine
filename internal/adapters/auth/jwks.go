package auth

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// JWKS holds keys from a JSON Web Key Set by URL (/.well-known/jwks.json):
// the set is cached for TTL; an unknown kid rereads the set, but at most
// once per MinRefresh — so forged kids do not turn into load on the issuer.
type JWKS struct {
	URL        string
	Client     *http.Client  // nil — http.DefaultClient
	TTL        time.Duration // 0 — 10 min
	MinRefresh time.Duration // 0 — 30 s

	mu      sync.Mutex
	keys    map[string]any
	fetched time.Time
}

// Key returns a key by kid; kid "" — the only key of the set.
func (j *JWKS) Key(ctx context.Context, kid, _ string) (any, error) {
	ttl, minRefresh := j.TTL, j.MinRefresh
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if minRefresh == 0 {
		minRefresh = 30 * time.Second
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if k := j.find(kid); k != nil && time.Since(j.fetched) < ttl {
		return k, nil
	}
	if j.keys == nil || time.Since(j.fetched) >= minRefresh {
		keys, err := j.fetch(ctx)
		if err != nil {
			if k := j.find(kid); k != nil {
				return k, nil // the issuer is unavailable: the previous set
			}
			return nil, err
		}
		j.keys, j.fetched = keys, time.Now()
	}
	if k := j.find(kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("no key %q in JWKS", kid)
}

func (j *JWKS) find(kid string) any {
	if kid == "" && len(j.keys) == 1 {
		for _, k := range j.keys {
			return k
		}
	}
	return j.keys[kid]
}

func (j *JWKS) fetch(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, err
	}
	client := j.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	return ParseJWKS(body)
}

// ParseJWKS parses a key set: RSA and EC P-256 (kty, kid, n, e, crv, x, y);
// keys of other kinds are skipped.
func ParseJWKS(data []byte) (map[string]any, error) {
	var set struct {
		Keys []struct {
			Kty, Kid, N, E, Crv, X, Y string
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	out := map[string]any{}
	for _, k := range set.Keys {
		switch k.Kty {
		case "RSA":
			n, err1 := bigInt(k.N)
			e, err2 := bigInt(k.E)
			if err := errors.Join(err1, err2); err != nil || !e.IsInt64() {
				return nil, fmt.Errorf("JWKS: RSA key %q: wrong n or e", k.Kid)
			}
			out[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := bigInt(k.X)
			y, err2 := bigInt(k.Y)
			if err := errors.Join(err1, err2); err != nil {
				return nil, fmt.Errorf("JWKS: EC key %q: wrong x or y", k.Kid)
			}
			if len(x.Bytes()) > 32 || len(y.Bytes()) > 32 {
				return nil, fmt.Errorf("JWKS: EC key %q is not on curve P-256", k.Kid)
			}
			point := append(append([]byte{4}, x.FillBytes(make([]byte, 32))...), y.FillBytes(make([]byte, 32))...)
			if _, err := ecdh.P256().NewPublicKey(point); err != nil {
				return nil, fmt.Errorf("JWKS: EC key %q is not on curve P-256", k.Kid)
			}
			out[k.Kid] = &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		}
	}
	return out, nil
}

func bigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("not base64url")
	}
	return new(big.Int).SetBytes(b), nil
}
