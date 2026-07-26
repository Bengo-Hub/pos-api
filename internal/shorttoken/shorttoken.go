// Package shorttoken base58-encodes a UUID into a short, human-friendlier code and back. It adds
// no new secret/entropy — a code is just a shorter representation of the SAME UUID capability
// token (e.g. POSOrder.public_token) — so it's safe to share both a long-form UUID route and a
// short-form code route for the exact same underlying token, and safe to import from both the
// producing side (orders service, building the link) and the consuming side (the public receipt
// handler, decoding the link) with no import-cycle risk since this package depends on nothing but
// the standard library + uuid.
package shorttoken

import (
	"errors"
	"math/big"

	"github.com/google/uuid"
)

// alphabet is the Bitcoin/IPFS base58 set — no 0/O/I/l, so a code read aloud or squinted at in a
// WhatsApp message is never ambiguous between visually-similar characters.
const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var errInvalid = errors.New("shorttoken: invalid code")

var base = big.NewInt(int64(len(alphabet)))

// Encode returns a ~22-character base58 encoding of the UUID's 128 bits.
func Encode(id uuid.UUID) string {
	n := new(big.Int).SetBytes(id[:])
	if n.Sign() == 0 {
		return string(alphabet[0])
	}
	var out []byte
	zero := big.NewInt(0)
	mod := new(big.Int)
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// Decode reverses Encode back into the 16-byte UUID it was built from.
func Decode(code string) (uuid.UUID, error) {
	if code == "" {
		return uuid.UUID{}, errInvalid
	}
	n := big.NewInt(0)
	for _, c := range code {
		idx := -1
		for i, a := range alphabet {
			if a == c {
				idx = i
				break
			}
		}
		if idx < 0 {
			return uuid.UUID{}, errInvalid
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	b := n.Bytes()
	if len(b) > 16 {
		return uuid.UUID{}, errInvalid
	}
	var id uuid.UUID
	copy(id[16-len(b):], b)
	return id, nil
}
