package pii

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// The value ciphertext format (RFC §20): the subject key version (uint32,
// big-endian) ‖ nonce (12 bytes) ‖ AES-256-GCM(the value's canonical JSON)
// with the tag (16 bytes).
const (
	versionSize = 4
	nonceSize   = 12
	tagSize     = 16
	// Overhead is the ciphertext length beyond the plaintext.
	Overhead = versionSize + nonceSize + tagSize
	// BlindSize is the length of a blind index value.
	BlindSize = 16
)

// Seal appends to dst the ciphertext of plain under the subject key subj.
// owner and member are the owner type of the place and the field or
// parameter name: they are part of the AAD, so the ciphertext cannot be
// moved to another place.
func (s *Sealer) Seal(ctx context.Context, dst []byte, subj port.SubjectRef, owner, member string, plain []byte) ([]byte, error) {
	k, created, err := s.current(ctx, subj)
	if err != nil {
		return dst, err
	}
	s.markUsed(subj, k.version, created)
	dst = binary.BigEndian.AppendUint32(dst, k.version)
	n := len(dst)
	dst = append(dst, make([]byte, nonceSize)...)
	if _, err := io.ReadFull(s.k.opts.Rand, dst[n:]); err != nil {
		return dst[:n-versionSize], err
	}
	s.buf = valueAAD(s.buf[:0], s.tenant, subj, k.version, owner, member)
	return k.aead.Seal(dst, dst[n:n+nonceSize], plain, s.buf), nil
}

// Open appends to dst the plaintext of the ciphertext sealed. ok == false —
// the key of this version is destroyed or does not exist: the value reads
// as none. A ciphertext that does not decrypt with the live key of its
// version gives ErrTampered.
func (s *Sealer) Open(ctx context.Context, dst []byte, subj port.SubjectRef, owner, member string, sealed []byte) ([]byte, bool, error) {
	if len(sealed) < Overhead {
		return dst, false, ErrTampered
	}
	v := binary.BigEndian.Uint32(sealed)
	k, ok, err := s.atVersion(ctx, subj, v)
	if err != nil || !ok {
		return dst, false, err
	}
	s.buf = valueAAD(s.buf[:0], s.tenant, subj, v, owner, member)
	out, err := k.aead.Open(dst, sealed[versionSize:versionSize+nonceSize], sealed[versionSize+nonceSize:], s.buf)
	if err != nil {
		return dst, false, ErrTampered
	}
	s.noteOpened(subj, v)
	return out, true, nil
}

// SealedVersion returns the key version of a ciphertext.
func SealedVersion(sealed []byte) (uint32, error) {
	if len(sealed) < Overhead {
		return 0, ErrTampered
	}
	return binary.BigEndian.Uint32(sealed), nil
}

// Blind returns the blind index value of the field field (type.field) for
// a value's canonical JSON: HMAC-SHA-256 with the tenant's field key,
// truncated to BlindSize bytes (RFC §20).
func (s *Sealer) Blind(ctx context.Context, field string, canonical []byte) ([BlindSize]byte, error) {
	var out [BlindSize]byte
	t, err := s.tenantKeys(ctx)
	if err != nil {
		return out, err
	}
	pool := t.fieldMACs(field)
	m := pool.Get().(hash.Hash)
	m.Reset()
	m.Write(canonical)
	var sum [sha256.Size]byte
	copy(out[:], m.Sum(sum[:0]))
	pool.Put(m)
	return out, nil
}

// fieldMACs returns the HMAC pool with the field's blind index key.
func (t *tenantKeys) fieldMACs(field string) *sync.Pool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.fields[field]
	if !ok {
		key := hkdf(t.blind, "reasonr/blind/"+field, KeySize)
		p = &sync.Pool{New: func() any { return hmac.New(sha256.New, key) }}
		t.fields[field] = p
	}
	return p
}

// appendPart appends a part with its length: the AAD parts do not merge
// into an ambiguous string.
func appendPart(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func cacheKey(b []byte, tenant port.Tenant, ref port.SubjectRef) []byte {
	return appendPart(appendPart(appendPart(b, string(tenant)), ref.Type), ref.ID)
}

func keyAAD(b []byte, tenant port.Tenant, ref port.SubjectRef) []byte {
	return cacheKey(append(b, "reasonr/subject-key\x00"...), tenant, ref)
}

func valueAAD(b []byte, tenant port.Tenant, ref port.SubjectRef, v uint32, owner, member string) []byte {
	b = cacheKey(append(b, "reasonr/value\x00"...), tenant, ref)
	b = binary.BigEndian.AppendUint32(b, v)
	return appendPart(appendPart(b, owner), member)
}
