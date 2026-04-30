package smt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"sync"
)

// ShardedSMT is a wrapper that splits keys across N independent
// SparseMerkleTrees by the high bits of the key digest. Each shard
// runs its own MapStore and can be updated in parallel from a
// goroutine pool. The final root is the Merkle root of the N shard
// roots.
//
// Key routing: each key is hashed once using the configured hasher;
// the first 6 bits of the digest select the shard (so N must be a
// power of two ≤ 64). Using the digest (rather than the raw key
// first byte) ensures uniform shard load even for adversarial input
// distributions.
//
// Concurrency model: BulkUpdate fans out to all shards in parallel
// via per-shard goroutines. Single Update is serial against one
// shard (no parallelism win for a single key). Get is serial.
//
// Proof API: NOT IMPLEMENTED on this wrapper. Per-shard proofs are
// available via the underlying SparseMerkleTree, but a sharded-proof
// (per-shard SMT proof + Merkle inclusion of the shard root in the
// final root) is left as future work — the perf path doesn't depend
// on it. Verifiers can replay sequenced txs into a fresh
// ShardedSMT to re-derive the root.
type ShardedSMT struct {
	shards    []*SparseMerkleTree
	shardBits uint // log2(len(shards))
	hasherFn  func() hash.Hash
}

// ErrInvalidShardCount is returned by NewShardedSMT when the count is
// not a power-of-two between 1 and 64 inclusive.
var ErrInvalidShardCount = errors.New("sharded smt: shard count must be a power of two between 1 and 64")

// NewShardedSMT creates a ShardedSMT with `shards` empty
// SparseMerkleTrees. mapstoreFactory is called once per shard with
// the shard index to produce its (nodes, values) MapStores;
// hasherFactory is called per construction (each tree gets its own
// hasher) and is also stored for use in computing the combined root
// across shard sub-roots.
//
// Typical usage:
//
//	smt.NewShardedSMT(64,
//	    func(_ int) (smt.MapStore, smt.MapStore) {
//	        return smt.NewFixedSizeMap(), smt.NewFixedSizeMap()
//	    },
//	    sha256.New,
//	)
func NewShardedSMT(
	shardCount int,
	mapstoreFactory func(shardIdx int) (nodes, values MapStore),
	hasherFactory func() hash.Hash,
) (*ShardedSMT, error) {
	if shardCount < 1 || shardCount > 64 {
		return nil, ErrInvalidShardCount
	}
	if shardCount&(shardCount-1) != 0 {
		return nil, ErrInvalidShardCount
	}
	bits := uint(0)
	for x := shardCount; x > 1; x >>= 1 {
		bits++
	}
	s := &ShardedSMT{
		shards:    make([]*SparseMerkleTree, shardCount),
		shardBits: bits,
		hasherFn:  hasherFactory,
	}
	for i := 0; i < shardCount; i++ {
		nodes, values := mapstoreFactory(i)
		s.shards[i] = NewSparseMerkleTree(nodes, values, hasherFactory())
	}
	return s, nil
}

// shardOf returns the shard index for a given key, computed from the
// first shardBits bits of sha256(key). Using a fixed (sha256) routing
// hash decouples shard assignment from the per-shard tree's hasher
// and gives uniform distribution regardless of the user-supplied
// hasher. Keys with the same path digest in the same shard land
// adjacent post-sort, preserving BulkUpdate's prefix-amortization.
func (s *ShardedSMT) shardOf(key []byte) int {
	h := sha256.Sum256(key)
	return int(h[0] >> (8 - s.shardBits))
}

// Update routes a single (key, value) to the right shard and applies
// it. Returns the new combined root.
//
// For high-throughput workloads, prefer BulkUpdate so the per-shard
// sub-roots are recomputed once instead of once per Update.
func (s *ShardedSMT) Update(key, value []byte) ([]byte, error) {
	idx := s.shardOf(key)
	if _, err := s.shards[idx].Update(key, value); err != nil {
		return nil, err
	}
	return s.computeRoot()
}

// Get fetches a key from the appropriate shard.
func (s *ShardedSMT) Get(key []byte) ([]byte, error) {
	idx := s.shardOf(key)
	return s.shards[idx].Get(key)
}

// BulkUpdate splits the (keys, values) batch by shard and applies
// each shard's slice in parallel via per-shard goroutines. Each
// shard uses its underlying SparseMerkleTree.BulkUpdate, which runs
// the prefix-amortization recursion within the shard. The combined
// root is computed once after all shards finish.
//
// Returns the new combined root (a fresh allocation).
func (s *ShardedSMT) BulkUpdate(keys, values [][]byte) ([]byte, error) {
	if len(keys) != len(values) {
		return nil, ErrBulkInputMismatch
	}
	if len(keys) == 0 {
		return s.computeRoot()
	}

	// Bucket inputs by shard. Pre-size each shard's slice to len/N
	// to avoid grow churn for uniformly-distributed keys.
	N := len(s.shards)
	hint := len(keys) / N
	if hint < 4 {
		hint = 4
	}
	bucketK := make([][][]byte, N)
	bucketV := make([][][]byte, N)
	for i := range bucketK {
		bucketK[i] = make([][]byte, 0, hint)
		bucketV[i] = make([][]byte, 0, hint)
	}
	for i := range keys {
		idx := s.shardOf(keys[i])
		bucketK[idx] = append(bucketK[idx], keys[i])
		bucketV[idx] = append(bucketV[idx], values[i])
	}

	// Fan out per-shard BulkUpdate calls. We capture the first error
	// across shards if any occurs.
	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		gotErr error
	)
	for shardIdx, shard := range s.shards {
		if len(bucketK[shardIdx]) == 0 {
			continue
		}
		wg.Add(1)
		go func(shard *SparseMerkleTree, ks, vs [][]byte) {
			defer wg.Done()
			if _, err := shard.BulkUpdate(ks, vs); err != nil {
				errMu.Lock()
				if gotErr == nil {
					gotErr = err
				}
				errMu.Unlock()
			}
		}(shard, bucketK[shardIdx], bucketV[shardIdx])
	}
	wg.Wait()

	if gotErr != nil {
		return nil, gotErr
	}
	return s.computeRoot()
}

// computeRoot folds the N shard roots into a final combined root via
// a balanced Merkle tree. The fold uses the same node-prefix scheme
// as the SMT's internal nodes (digest(nodePrefix || left || right)),
// which keeps the wire format consistent with how the SMT itself
// hashes pairs of subtrees. With 64 shards and 32-byte hashes the
// fold runs 63 hashes (a balanced tree of 64 leaves has 63 internal
// nodes); negligible vs. the 100K+ hashes inside the shards.
func (s *ShardedSMT) computeRoot() ([]byte, error) {
	level := make([][]byte, len(s.shards))
	for i, sh := range s.shards {
		level[i] = sh.Root()
	}
	h := s.hasherFn()
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				// Odd count — pair with itself (or duplicate). Standard
				// merkle-tree behavior: hash with self.
				next = append(next, mergeHash(h, level[i], level[i]))
				continue
			}
			next = append(next, mergeHash(h, level[i], level[i+1]))
		}
		level = next
	}
	out := make([]byte, len(level[0]))
	copy(out, level[0])
	return out, nil
}

// Root recomputes and returns the combined root.
func (s *ShardedSMT) Root() []byte {
	r, err := s.computeRoot()
	if err != nil {
		panic(fmt.Errorf("sharded smt: computeRoot failed: %w", err))
	}
	return r
}

// ShardRoot returns the underlying root of a specific shard. Useful
// for verifiers that build their own per-shard proofs.
func (s *ShardedSMT) ShardRoot(idx int) []byte {
	return s.shards[idx].Root()
}

// ShardCount returns the number of shards.
func (s *ShardedSMT) ShardCount() int { return len(s.shards) }

// Equal compares two combined roots, accounting for the case where one
// or both are nil/zero.
func Equal(a, b []byte) bool { return bytes.Equal(a, b) }

func mergeHash(h hash.Hash, left, right []byte) []byte {
	h.Reset()
	h.Write(nodePrefix)
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}
