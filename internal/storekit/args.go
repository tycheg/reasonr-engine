package storekit

import (
	"context"
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// Timer arguments (RFC §20): the store keeps them without personal values
// (none), and the ciphertexts next to them, in the EncodeSealed format.
// The subjects come from TimerOp.Subjects: the VM computes them from the
// entity the timer calls the command on.

// SealArgs encrypts the personal arguments args (the parameters params of
// the method owner) with the keys of the subjects subjects: it returns the
// arguments without them and the ciphertexts (nil — no personal values). A
// personal value without a subject gives ErrNoSubject, as for fields.
func (pc *PayloadCrypto) SealArgs(ctx context.Context, a *reasonr.Arena, owner string, params []reasonr.FieldInfo, args reasonr.Value,
	subjects []reasonr.ArgSubject) (reasonr.Value, []byte, error) {
	of := pc.Crypto.ArgSubjects(a, subjects)
	stripped, sealed, err := pc.Crypto.Seal(ctx, pc.Sealer, a, owner, params, args, func(i int) ([]port.SubjectRef, error) {
		refs, err := of(i)
		if err == nil && len(refs) == 0 && params[i].Personal != nil {
			err = fmt.Errorf("%w: %s", ErrNoSubject, params[i].Name)
		}
		return refs, err
	})
	if err != nil {
		return reasonr.None, nil, err
	}
	data, err := EncodeSealed(params, sealed)
	return stripped, data, err
}

// OpenArgs decrypts the ciphertexts data into the arguments args (in
// place): the values of forgotten subjects stay none.
func (pc *PayloadCrypto) OpenArgs(ctx context.Context, a *reasonr.Arena, owner string, params []reasonr.FieldInfo, args reasonr.Value, data []byte) error {
	ss, err := DecodeSealed(params, data)
	if err != nil || len(ss) == 0 {
		return err
	}
	if err := Prefetch(ctx, pc.Sealer, ss); err != nil {
		return err
	}
	return pc.Crypto.Open(ctx, pc.Sealer, a, owner, params, args, ss)
}
