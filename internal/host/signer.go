package host

import (
	"crypto/ecdsa"
	"fmt"
	"os"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/config"
)

// DefaultIssuer is iss in the engine's JWT when integrations.auth.issuer is not set.
const DefaultIssuer = "reasonr"

// BuildSigner builds the engine's JWT signer from integrations.auth (RFC
// §18): { key_file | key_env, kid, issuer, also: [{ key_file | key_env,
// kid }] }. Keys are EC P-256 in PEM; also may hold public ones. nil without
// integrations.auth: in test mode a key is created at startup.
func BuildSigner(n *config.Node, test bool) (*auth.Signer, error) {
	if n == nil {
		if !test {
			return nil, nil
		}
		return auth.GenerateSigner(DefaultIssuer)
	}
	priv, _, err := pemKey(n, "integrations.auth")
	if err != nil {
		return nil, err
	}
	if priv == nil {
		return nil, fmt.Errorf("integrations.auth: a private signing key is needed, not a public one")
	}
	s := &auth.Signer{Key: priv}
	if s.Kid, err = n.String("kid", ""); err != nil {
		return nil, err
	}
	if s.Issuer, err = n.String("issuer", DefaultIssuer); err != nil {
		return nil, err
	}
	if also := n.Get("also"); also != nil {
		if also.Kind != config.List {
			return nil, fmt.Errorf("integrations.auth.also: expected a list of keys")
		}
		for i, k := range also.Values {
			if err := k.Only("key_file", "key_env", "kid"); err != nil {
				return nil, err
			}
			_, pub, err := pemKey(k, fmt.Sprintf("integrations.auth.also[%d]", i))
			if err != nil {
				return nil, err
			}
			kid, err := k.String("kid", "")
			if err != nil {
				return nil, err
			}
			s.Also = append(s.Also, auth.PublicKey{Kid: kid, Key: pub})
		}
	}
	return s, nil
}

// pemKey reads a key from the file key_file or the environment variable key_env.
func pemKey(n *config.Node, where string) (*ecdsa.PrivateKey, *ecdsa.PublicKey, error) {
	file, err := n.String("key_file", "")
	if err != nil {
		return nil, nil, err
	}
	env, err := n.String("key_env", "")
	if err != nil {
		return nil, nil, err
	}
	var data []byte
	switch {
	case file != "" && env != "":
		return nil, nil, fmt.Errorf("%s: set key_file or key_env, not both", where)
	case file != "":
		if data, err = os.ReadFile(file); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", where, err)
		}
	case env != "":
		if data = []byte(os.Getenv(env)); len(data) == 0 {
			return nil, nil, fmt.Errorf("%s: the environment variable %s is empty", where, env)
		}
	default:
		return nil, nil, fmt.Errorf("%s: set key_file or key_env — an EC P-256 key in PEM", where)
	}
	priv, pub, err := auth.ParseECKey(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", where, err)
	}
	return priv, pub, nil
}
