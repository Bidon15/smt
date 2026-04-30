package smt

import "sync"

// pathSliceCap mirrors th.pathSize()*8 for SHA-256 — the maximum tree
// depth. Allocating at this capacity once avoids reslicing during the
// per-update walk.
const pathSliceCap = 256

// hashBufCap sizes the per-update hash scratch buffers. 64 bytes covers
// SHA-256 (32B), BLAKE3-256 (32B) and SHA-512 (64B); the actual Size()
// is decided by the hasher and only the first Size() bytes are written.
const hashBufCap = 64

// pathSlices holds the two parallel slices that fillSideNodes fills
// during an Update / Delete / Prove walk. Pooling them removes two
// allocations per Update on the hot path (a 256-cap [][]byte and a
// 257-cap [][]byte — together about 12 KB).
//
// The contained slices' element bytes are stored long-term in the
// MapStore; only the outer slices are reused. We nil out the entries on
// release so the underlying byte slices remain GC-eligible if the map
// later drops them.
type pathSlices struct {
	sideNodes [][]byte
	pathNodes [][]byte
}

var pathSlicesPool = sync.Pool{
	New: func() any {
		return &pathSlices{
			sideNodes: make([][]byte, 0, pathSliceCap),
			pathNodes: make([][]byte, 0, pathSliceCap+1),
		}
	},
}

func getPathSlices() *pathSlices {
	return pathSlicesPool.Get().(*pathSlices)
}

func putPathSlices(p *pathSlices) {
	for i := range p.sideNodes {
		p.sideNodes[i] = nil
	}
	for i := range p.pathNodes {
		p.pathNodes[i] = nil
	}
	p.sideNodes = p.sideNodes[:0]
	p.pathNodes = p.pathNodes[:0]
	pathSlicesPool.Put(p)
}

// updateScratch bundles the byte buffers needed by a single Update /
// Delete walk so the hot path makes one pool checkout instead of four.
// All four buffers are sized for hashes up to hashBufCap; only the
// first hasher.Size() bytes of each are ever written.
//
// Layout:
//   - pathBuf:      key digest (the SMT path)
//   - valueHashBuf: digest of the user-supplied leaf value
//   - hashBufA / hashBufB: tick-tock destinations for the running
//     digest of the running root as the walk climbs the tree.
//
// The byte slices stored in MapStore are NOT taken from this struct —
// node values are sized as prefix+left+right (~65 bytes) and are
// allocated separately because they must outlive the Update call.
type updateScratch struct {
	pathBuf      [hashBufCap]byte
	valueHashBuf [hashBufCap]byte
	hashBufA     [hashBufCap]byte
	hashBufB     [hashBufCap]byte
}

var updateScratchPool = sync.Pool{
	New: func() any { return new(updateScratch) },
}

func getUpdateScratch() *updateScratch {
	return updateScratchPool.Get().(*updateScratch)
}

func putUpdateScratch(s *updateScratch) {
	updateScratchPool.Put(s)
}
