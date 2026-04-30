package smt

// FixedSizeMap is a MapStore that uses a Go map keyed by [32]byte arrays
// instead of strings. It avoids the per-Set string allocation that
// SimpleMap incurs when the hasher emits 32-byte digests (the SHA-256
// case). All keys passed in must be exactly 32 bytes; otherwise this
// type panics. Use only with hashers whose Size() == 32.
type FixedSizeMap struct {
	m map[[32]byte][]byte
}

// NewFixedSizeMap creates a new empty FixedSizeMap.
func NewFixedSizeMap() *FixedSizeMap {
	return &FixedSizeMap{
		m: make(map[[32]byte][]byte),
	}
}

// NewFixedSizeMapWithCapacity creates a FixedSizeMap pre-sized for n entries,
// avoiding rehash churn during a known-size epoch.
func NewFixedSizeMapWithCapacity(n int) *FixedSizeMap {
	return &FixedSizeMap{
		m: make(map[[32]byte][]byte, n),
	}
}

func toFixedKey(key []byte) (k [32]byte) {
	if len(key) != 32 {
		panic("FixedSizeMap requires 32-byte keys")
	}
	copy(k[:], key)
	return
}

func (sm *FixedSizeMap) Get(key []byte) ([]byte, error) {
	k := toFixedKey(key)
	if value, ok := sm.m[k]; ok {
		return value, nil
	}
	return nil, &InvalidKeyError{Key: key}
}

func (sm *FixedSizeMap) Set(key []byte, value []byte) error {
	sm.m[toFixedKey(key)] = value
	return nil
}

func (sm *FixedSizeMap) Delete(key []byte) error {
	k := toFixedKey(key)
	if _, ok := sm.m[k]; ok {
		delete(sm.m, k)
		return nil
	}
	return &InvalidKeyError{Key: key}
}

// Len returns the number of stored entries; useful for tests/diagnostics.
func (sm *FixedSizeMap) Len() int { return len(sm.m) }
