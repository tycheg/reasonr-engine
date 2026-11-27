package pii

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

var (
	ctx      = context.Background()
	customer = port.SubjectRef{Type: "Customer", ID: `"0192b6c1-0000-7000-8000-000000000001"`}
	other    = port.SubjectRef{Type: "Customer", ID: `"0192b6c1-0000-7000-8000-000000000002"`}
	phone    = []byte(`"+79990001122"`)
)

var timeZero time.Time

func TestHKDF(t *testing.T) {
	// RFC 5869, test 3: without salt and info.
	okm := hkdf(bytes.Repeat([]byte{0x0b}, 22), "", 42)
	want := "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8"
	if hex.EncodeToString(okm) != want {
		t.Fatalf("hkdf = %x", okm)
	}
}

func TestLocalKMS(t *testing.T) {
	k := NewMemoryKMS()
	w, err := k.Wrap(ctx, "a", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := k.Unwrap(ctx, "a", w); err != nil || string(p) != "secret" {
		t.Fatalf("unwrap = %q, %v", p, err)
	}
	if _, err := k.Unwrap(ctx, "b", w); err == nil {
		t.Fatal("unwrapped with another context")
	}
	if _, err := NewMemoryKMS().Unwrap(ctx, "a", w); err == nil {
		t.Fatal("unwrapped with another master key")
	}
	if _, err := ParseKey("c2hvcnQ="); err == nil {
		t.Fatal("a short key is accepted")
	}
	if b, err := ParseKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err != nil || len(b) != KeySize {
		t.Fatalf("ParseKey: %v", err)
	}
}

func newRing(kms port.KMS, keys port.KeyStore) *Keyring { return NewKeyring(kms, keys, Options{}) }

func TestSealOpen(t *testing.T) {
	keys := NewMemoryKeys()
	r := newRing(NewMemoryKMS(), keys)
	s := r.Sealer("t1")
	sealed, err := s.Seal(ctx, nil, customer, "Customer", "phone", phone)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, phone[1:len(phone)-1]) || len(sealed) != len(phone)+Overhead {
		t.Fatalf("ciphertext %x", sealed)
	}
	if u := s.Used(); len(u) != 1 || u[0].Subject != customer || u[0].Version != 1 || !u[0].Created {
		t.Fatalf("Used = %v", u)
	}
	// A key from the cache was not created by this sealer: the commit checks it.
	s2 := r.Sealer("t1")
	if _, err := s2.Seal(ctx, nil, customer, "Customer", "phone", phone); err != nil || s2.Used()[0].Created {
		t.Fatalf("Used from the cache = %v %v", s2.Used(), err)
	}
	plain, ok, err := r.Sealer("t1").Open(ctx, nil, customer, "Customer", "phone", sealed)
	if err != nil || !ok || !bytes.Equal(plain, phone) {
		t.Fatalf("Open = %q %v %v", plain, ok, err)
	}
	// AAD: another place, another subject, another tenant.
	if _, _, err := s.Open(ctx, nil, customer, "Customer", "name", sealed); !errors.Is(err, ErrTampered) {
		t.Fatalf("another field: %v", err)
	}
	if _, ok, err := s.Open(ctx, nil, other, "Customer", "phone", sealed); ok || err != nil {
		t.Fatalf("a subject without a key: %v %v", ok, err)
	}
	s.Seal(ctx, nil, other, "Customer", "phone", phone)
	if _, _, err := s.Open(ctx, nil, other, "Customer", "phone", sealed); !errors.Is(err, ErrTampered) {
		t.Fatalf("another subject: %v", err)
	}
	if _, ok, _ := r.Sealer("t2").Open(ctx, nil, customer, "Customer", "phone", sealed); ok {
		t.Fatal("opened in another tenant")
	}
	broken := bytes.Clone(sealed)
	broken[len(broken)-1] ^= 1
	if _, _, err := s.Open(ctx, nil, customer, "Customer", "phone", broken); !errors.Is(err, ErrTampered) {
		t.Fatalf("corruption: %v", err)
	}
	if _, _, err := s.Open(ctx, nil, customer, "Customer", "phone", sealed[:10]); !errors.Is(err, ErrTampered) {
		t.Fatalf("truncated: %v", err)
	}
}

func TestForgetAndRebirth(t *testing.T) {
	keys := NewMemoryKeys()
	kms := NewMemoryKMS()
	a, b := newRing(kms, keys), newRing(kms, keys)
	old, _ := a.Sealer("t").Seal(ctx, nil, customer, "Customer", "phone", phone)
	if _, ok, _ := b.Sealer("t").Open(ctx, nil, customer, "Customer", "phone", old); !ok {
		t.Fatal("the second instance did not open")
	}
	keys.Destroy("t", []port.SubjectRef{customer})
	a.Forgotten("t", []port.SubjectRef{customer})
	if _, ok, err := a.Sealer("t").Open(ctx, nil, customer, "Customer", "phone", old); ok || err != nil {
		t.Fatalf("after forget: %v %v", ok, err)
	}
	// Instance b still remembers the key until it gets the drop.
	b.Forgotten("t", []port.SubjectRef{customer})
	if _, ok, _ := b.Sealer("t").Open(ctx, nil, customer, "Customer", "phone", old); ok {
		t.Fatal("opened after the cache was dropped")
	}
	s := a.Sealer("t")
	fresh, err := s.Seal(ctx, nil, customer, "Customer", "phone", phone)
	if err != nil || s.Used()[0].Version != 2 {
		t.Fatalf("a new version: %v %v", s.Used(), err)
	}
	if _, ok, _ := b.Sealer("t").Open(ctx, nil, customer, "Customer", "phone", fresh); !ok {
		t.Fatal("the new version did not open")
	}
	if _, ok, _ := b.Sealer("t").Open(ctx, nil, customer, "Customer", "phone", old); ok {
		t.Fatal("the old version opened after the rebirth")
	}
	if v, _ := SealedVersion(fresh); v != 2 {
		t.Fatalf("version %d", v)
	}
}

// TestResealer: the resealer encrypts only with the version that decrypted
// the value and creates no keys (RFC §31.2).
func TestResealer(t *testing.T) {
	keys := NewMemoryKeys()
	ring := newRing(NewMemoryKMS(), keys)
	if _, err := ring.Resealer("t", nil).Seal(ctx, nil, customer, "Customer", "phone", phone); !errors.Is(err, ErrKeyGone) {
		t.Fatalf("without a tenant key: %v", err)
	}
	if k, _ := keys.TenantKey(ctx, "t"); k != nil {
		t.Fatal("the resealer created a tenant key")
	}
	old, _ := ring.Sealer("t").Seal(ctx, nil, customer, "Customer", "phone", phone)
	opener := ring.Sealer("t")
	if _, ok, _ := opener.Open(ctx, nil, customer, "Customer", "phone", old); !ok {
		t.Fatal("did not open")
	}
	pinned := opener.Opened()
	if len(pinned) != 1 || pinned[0] != (port.KeyVersion{Subject: customer, Version: 1}) {
		t.Fatalf("Opened: %+v", pinned)
	}
	again, err := ring.Resealer("t", pinned).Seal(ctx, nil, customer, "Customer", "phone", phone)
	if v, _ := SealedVersion(again); err != nil || v != 1 {
		t.Fatalf("resealing with version 1: %d %v", v, err)
	}
	if _, err := ring.Resealer("t", pinned).Seal(ctx, nil, other, "Customer", "phone", phone); !errors.Is(err, ErrKeyGone) {
		t.Fatalf("a subject without a version: %v", err)
	}
	if ks, _ := keys.SubjectKeys(ctx, "t", []port.SubjectRef{other}); len(ks) != 0 {
		t.Fatal("the resealer created a subject key")
	}
	// The resealer also remembers the versions it decrypted with itself.
	rs := ring.Resealer("t", nil)
	if _, ok, _ := rs.Open(ctx, nil, customer, "Customer", "phone", old); !ok {
		t.Fatal("the resealer did not open")
	}
	if _, err := rs.Seal(ctx, nil, customer, "Customer", "phone", phone); err != nil {
		t.Fatalf("with the version it decrypted with itself: %v", err)
	}
	keys.Destroy("t", []port.SubjectRef{customer})
	ring.Forgotten("t", []port.SubjectRef{customer})
	ring.Sealer("t").Seal(ctx, nil, customer, "Customer", "phone", phone) // a new version 2
	if _, err := ring.Resealer("t", pinned).Seal(ctx, nil, customer, "Customer", "phone", phone); !errors.Is(err, ErrKeyGone) {
		t.Fatalf("a destroyed version with a live new one: %v", err)
	}
}

// TestRotate: rotation rewraps the tenant key; a repeat skips the rewrapped
// ones; a key that no master key unwraps stops the rotation.
func TestRotate(t *testing.T) {
	keys := NewMemoryKeys()
	a, b, c := NewMemoryKMS(), NewMemoryKMS(), NewMemoryKMS()
	sealed, _ := newRing(a, keys).Sealer("t").Seal(ctx, nil, customer, "Customer", "phone", phone)
	if res, err := Rotate(ctx, keys, a, b); err != nil || res != (Rotated{Rotated: 1}) {
		t.Fatalf("rotation: %+v %v", res, err)
	}
	if got, ok, err := newRing(b, keys).Sealer("t").Open(ctx, nil, customer, "Customer", "phone", sealed); !ok || err != nil || !bytes.Equal(got, phone) {
		t.Fatalf("after rotation: %q %v %v", got, ok, err)
	}
	if res, err := Rotate(ctx, keys, a, b); err != nil || res != (Rotated{Skipped: 1}) {
		t.Fatalf("repeat: %+v %v", res, err)
	}
	if _, err := Rotate(ctx, keys, a, c); err == nil {
		t.Fatal("a key that no master key unwraps was rewrapped")
	}
}

func TestConcurrentCreate(t *testing.T) {
	keys := NewMemoryKeys()
	kms := NewMemoryKMS()
	rings := make([]*Keyring, 8)
	sealed := make([][]byte, len(rings))
	var wg sync.WaitGroup
	for i := range rings {
		rings[i] = newRing(kms, keys)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if sealed[i], err = rings[i].Sealer("t").Seal(ctx, nil, customer, "Customer", "phone", phone); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := range rings {
		for j := range sealed {
			if _, ok, err := rings[i].Sealer("t").Open(ctx, nil, customer, "Customer", "phone", sealed[j]); !ok || err != nil {
				t.Fatalf("instance %d did not open ciphertext %d: %v", i, j, err)
			}
		}
	}
}

func TestBlind(t *testing.T) {
	keys := NewMemoryKeys()
	kms := NewMemoryKMS()
	a, b := newRing(kms, keys), newRing(kms, keys)
	x, _ := a.Sealer("t").Blind(ctx, "Customer.phone", phone)
	y, _ := b.Sealer("t").Blind(ctx, "Customer.phone", phone)
	if x != y {
		t.Fatal("the blind index differs between instances")
	}
	if z, _ := a.Sealer("t").Blind(ctx, "Driver.phone", phone); z == x {
		t.Fatal("the same in different fields")
	}
	if z, _ := a.Sealer("u").Blind(ctx, "Customer.phone", phone); z == x {
		t.Fatal("the same in different tenants")
	}
	if z, _ := a.Sealer("t").Blind(ctx, "Customer.phone", []byte(`"+79990001123"`)); z == x {
		t.Fatal("the same for different values")
	}
}

func TestCacheEviction(t *testing.T) {
	c := newLRU(2, 1<<62)
	for i, k := range []string{"a", "b", "c"} {
		c.put(k, subjectKey{version: uint32(i + 1)}, timeZero)
	}
	if _, ok := c.get([]byte("a"), timeZero); ok {
		t.Fatal("the old entry is not evicted")
	}
	c.put("b", subjectKey{version: 1}, timeZero) // an older version does not replace
	if v, _ := c.get([]byte("b"), timeZero); v.version != 2 {
		t.Fatalf("version %d", v.version)
	}
}

func BenchmarkSeal(b *testing.B) {
	s := newRing(NewMemoryKMS(), NewMemoryKeys()).Sealer("t")
	buf, _ := s.Seal(ctx, nil, customer, "Customer", "phone", phone)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, _ = s.Seal(ctx, buf[:0], customer, "Customer", "phone", phone)
	}
}

func BenchmarkOpen(b *testing.B) {
	s := newRing(NewMemoryKMS(), NewMemoryKeys()).Sealer("t")
	sealed, _ := s.Seal(ctx, nil, customer, "Customer", "phone", phone)
	var buf []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, _, _ = s.Open(ctx, buf[:0], customer, "Customer", "phone", sealed)
	}
}

func BenchmarkBlind(b *testing.B) {
	s := newRing(NewMemoryKMS(), NewMemoryKeys()).Sealer("t")
	s.Blind(ctx, "Customer.phone", phone)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Blind(ctx, "Customer.phone", phone)
	}
}
