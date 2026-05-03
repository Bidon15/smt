package smt

// ArenaFixedSizeMap is a MapStore that copies value bytes into
// pre-allocated slab arenas instead of holding pointers to per-Set
// allocations. The map stores (slabIdx, offset, length) tuples; Get
// returns a sub-slice into the appropriate slab.
//
// Compared to FixedSizeMap on a 100K-update BulkUpdate workload:
//   - GC scan cost: ~slabCount slabs (~100s of objects) instead of
//     ~100K individual heap allocations to mark.
//   - Cache locality: sequential within a slab. Adjacent entries set
//     in close temporal proximity end up adjacent in memory.
//   - Map indirection: slightly more work per Get (extra hop through
//     the slab table), but no per-Set heap alloc on the hot path.
//
// Use only with hashers whose Size() == 32 (SHA-256, BLAKE3-default).
//
// Thread safety: the same as FixedSizeMap — single-writer per map.
// Within a single SMT, this is the per-shard tree's responsibility
// (ShardedSMT runs one goroutine per shard).
//
// Memory: deleted entries' bytes are NOT reclaimed within the arena.
// Live key count stays bounded (~tree size); slab bytes grow with
// total Set count over the map's lifetime. Call Reset() to drop all
// slabs and the map together; useful for ephemeral per-epoch trees
// where the entire MapStore is discarded between epochs.
type ArenaFixedSizeMap struct {
	m        map[[32]byte]arenaEntry
	slabs    [][]byte
	cur      []byte // = slabs[len(slabs)-1] when slabs non-empty
	slabSize int
}

type arenaEntry struct {
	slab uint32
	off  uint32
	ln   uint32
}

// defaultArenaSlabSize is 256 KiB. Each slab holds ~4000 internal-node
// value buffers (typical 65-byte payload). Big enough to amortize
// per-slab GC overhead, small enough that an oversized "huge value"
// outlier doesn't waste much.
const defaultArenaSlabSize = 256 * 1024

// NewArenaFixedSizeMap creates an empty ArenaFixedSizeMap with the
// default slab size.
func NewArenaFixedSizeMap() *ArenaFixedSizeMap {
	return NewArenaFixedSizeMapWithSlabSize(defaultArenaSlabSize)
}

// NewArenaFixedSizeMapWithSlabSize creates an empty ArenaFixedSizeMap
// with a configurable slab size. Larger slabs amortize per-slab GC
// further but waste more memory on short-lived maps.
func NewArenaFixedSizeMapWithSlabSize(slabSize int) *ArenaFixedSizeMap {
	if slabSize < 64 {
		slabSize = 64
	}
	return &ArenaFixedSizeMap{
		m:        make(map[[32]byte]arenaEntry),
		slabSize: slabSize,
	}
}

func (am *ArenaFixedSizeMap) Get(key []byte) ([]byte, error) {
	k := toFixedKey(key)
	if e, ok := am.m[k]; ok {
		return am.slabs[e.slab][e.off : e.off+e.ln], nil
	}
	return nil, &InvalidKeyError{Key: key}
}

func (am *ArenaFixedSizeMap) Set(key, value []byte) error {
	n := len(value)
	// Need a new slab if current one would overflow.
	if len(am.cur)+n > cap(am.cur) {
		size := am.slabSize
		if n > size {
			size = n
		}
		am.cur = make([]byte, 0, size)
		am.slabs = append(am.slabs, am.cur)
	}
	off := len(am.cur)
	am.cur = append(am.cur, value...)
	// Mirror the updated len back into slabs[last] so Get sees the
	// extended slice (len bumped, same backing array).
	am.slabs[len(am.slabs)-1] = am.cur

	am.m[toFixedKey(key)] = arenaEntry{
		slab: uint32(len(am.slabs) - 1),
		off:  uint32(off),
		ln:   uint32(n),
	}
	return nil
}

func (am *ArenaFixedSizeMap) Delete(key []byte) error {
	k := toFixedKey(key)
	if _, ok := am.m[k]; ok {
		delete(am.m, k)
		return nil
	}
	return &InvalidKeyError{Key: key}
}

// Len returns the number of stored entries.
func (am *ArenaFixedSizeMap) Len() int { return len(am.m) }

// SlabBytes returns the total bytes allocated across all slabs (includes
// dead entries from Deletes / overwrites). For diagnostics.
func (am *ArenaFixedSizeMap) SlabBytes() int {
	total := 0
	for _, s := range am.slabs {
		total += len(s)
	}
	return total
}

// Reset drops all slabs and clears the map. Use when an entire MapStore
// is being discarded — e.g. between epoch trees in ephemeral usage. It
// is a no-op for typical SMT workloads where the same MapStore is
// reused across BulkUpdate calls; in that case you simply let dead
// arena bytes accumulate (bounded by total Set count).
func (am *ArenaFixedSizeMap) Reset() {
	am.m = make(map[[32]byte]arenaEntry)
	am.slabs = am.slabs[:0]
	am.cur = nil
}
