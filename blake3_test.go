package smt

import (
	"bytes"
	"encoding/binary"
	"hash"
	"math/rand"
	"testing"

	"lukechampine.com/blake3"
)

func newBlake3() hash.Hash { return blake3.New(32, nil) }

// BLAKE3 produces different roots than SHA-256 (different hash function),
// but Prove/VerifyProof must still round-trip on a BLAKE3-backed tree.
func TestBlake3_ProofRoundTrip(t *testing.T) {
	const ops = 1000
	const keySpace = 200

	rng := rand.New(rand.NewSource(13))
	tree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), newBlake3())

	keys := make(map[string][]byte)
	for i := 0; i < ops; i++ {
		key := randomKey(uint64(rng.Intn(keySpace)))
		val := make([]byte, 16)
		binary.LittleEndian.PutUint64(val[:8], uint64(i))
		if _, err := tree.Update(key, val); err != nil {
			t.Fatal(err)
		}
		keys[string(key)] = val
	}

	// Membership.
	for k, v := range keys {
		proof, err := tree.Prove([]byte(k))
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), []byte(k), v, newBlake3()) {
			t.Fatalf("blake3 membership proof failed: %x", k)
		}
		cp, err := tree.ProveCompact([]byte(k))
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyCompactProof(cp, tree.Root(), []byte(k), v, newBlake3()) {
			t.Fatalf("blake3 compact proof failed: %x", k)
		}
	}

	// Non-membership.
	for i := uint64(keySpace + 1000); i < uint64(keySpace+1100); i++ {
		k := randomKey(i)
		proof, err := tree.Prove(k)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), k, defaultValue, newBlake3()) {
			t.Fatalf("blake3 non-membership proof failed: %x", k)
		}
	}
}

// Same input sequence on the same hasher must produce the same root —
// this guards against any hasher-related state corruption introduced by
// the pooled tick-tock buffers when Size() is exactly 32.
func TestBlake3_RootDeterminism(t *testing.T) {
	keys := make([][]byte, 1000)
	vals := make([][]byte, 1000)
	rng := rand.New(rand.NewSource(99))
	for i := range keys {
		keys[i] = randomKey(uint64(rng.Intn(300)))
		vals[i] = make([]byte, 16)
		binary.LittleEndian.PutUint64(vals[i], uint64(i))
	}
	t1 := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), newBlake3())
	t2 := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), newBlake3())
	for i := range keys {
		if _, err := t1.Update(keys[i], vals[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := t2.Update(keys[i], vals[i]); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(t1.Root(), t2.Root()) {
		t.Fatalf("blake3 determinism violation: %x vs %x", t1.Root(), t2.Root())
	}
}
