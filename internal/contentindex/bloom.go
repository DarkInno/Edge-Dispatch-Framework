package contentindex

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"sync"
)

// BloomFilter is a space-efficient probabilistic data structure for membership testing.
type BloomFilter struct {
	bits []uint64
	k    uint32
	m    uint32
}

// NewBloomFilter creates a Bloom filter with estimated capacity and false-positive rate.
// capacity: expected number of elements
// fpRate: desired false positive rate (e.g., 0.01 = 1%)
func NewBloomFilter(capacity int, fpRate float64) *BloomFilter {
	m := optimalM(capacity, fpRate)
	k := optimalK(capacity, m)
	words := (m + 63) / 64
	if words == 0 {
		words = 1
	}
	// Align m to word boundary so serialization is round-trip safe
	m = words * 64
	return &BloomFilter{
		bits: make([]uint64, words),
		k:    k,
		m:    m,
	}
}

// NewBloomFilterFromBytes reconstructs a Bloom filter from serialized data.
func NewBloomFilterFromBytes(data []byte, k uint32) *BloomFilter {
	if k > 100 {
		k = 1
	}
	words := (len(data) + 7) / 8
	if words == 0 {
		words = 1
	}
	bits := make([]uint64, words)
	for i := 0; i < words; i++ {
		start := i * 8
		end := start + 8
		if end > len(data) {
			end = len(data)
		}
		var padded [8]byte
		copy(padded[:], data[start:end])
		bits[i] = binary.LittleEndian.Uint64(padded[:])
	}
	return &BloomFilter{
		bits: bits,
		k:    k,
		m:    uint32(words * 64),
	}
}

// Add inserts an element into the Bloom filter.
func (bf *BloomFilter) Add(key []byte) {
	h1, h2 := bf.hash(key)
	for i := uint32(0); i < bf.k; i++ {
		pos := (h1 + uint64(i)*h2) % uint64(bf.m)
		wordIdx := pos / 64
		bitIdx := pos % 64
		bf.bits[wordIdx] |= 1 << bitIdx
	}
}

// AddString inserts a string element.
func (bf *BloomFilter) AddString(s string) {
	bf.Add([]byte(s))
}

// Contains tests whether an element may be in the Bloom filter.
// Returns false if the element is definitely not present.
func (bf *BloomFilter) Contains(key []byte) bool {
	h1, h2 := bf.hash(key)
	for i := uint32(0); i < bf.k; i++ {
		pos := (h1 + uint64(i)*h2) % uint64(bf.m)
		wordIdx := pos / 64
		bitIdx := pos % 64
		if bf.bits[wordIdx]&(1<<bitIdx) == 0 {
			return false
		}
	}
	return true
}

// ContainsString tests string membership.
func (bf *BloomFilter) ContainsString(s string) bool {
	return bf.Contains([]byte(s))
}

// Bytes serializes the Bloom filter bits for storage.
func (bf *BloomFilter) Bytes() []byte {
	data := make([]byte, len(bf.bits)*8)
	for i, w := range bf.bits {
		binary.LittleEndian.PutUint64(data[i*8:], w)
	}
	return data
}

// K returns the number of hash functions used.
func (bf *BloomFilter) K() uint32 { return bf.k }

// M returns the total number of bits.
func (bf *BloomFilter) M() uint32 { return bf.m }

func (bf *BloomFilter) hash(key []byte) (uint64, uint64) {
	h := fnv.New128a()
	h.Write(key)
	sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8]), binary.LittleEndian.Uint64(sum[8:])
}

func optimalM(capacity int, fpRate float64) uint32 {
	n := float64(capacity)
	p := math.Log(fpRate)
	m := -n * p / (math.Ln2 * math.Ln2)
	if m < 64 {
		m = 64
	}
	return uint32(math.Ceil(m))
}

func optimalK(capacity int, m uint32) uint32 {
	k := float64(m) / float64(capacity) * math.Ln2
	if k < 1 {
		k = 1
	}
	return uint32(math.Ceil(k))
}

// ContentIndex is a thread-safe in-memory content cache index.
// It tracks which nodes have which hot keys and a Bloom filter summary.
type ContentIndex struct {
	mu         sync.RWMutex
	hotKeys    map[string]map[string]struct{} // nodeID -> hot key set
	blooms     map[string]*BloomFilter      // nodeID -> bloom filter
	totalKeys  map[string]int64             // nodeID -> total cached keys
}

// NewContentIndex creates an empty content index.
func NewContentIndex() *ContentIndex {
	return &ContentIndex{
		hotKeys:   make(map[string]map[string]struct{}),
		blooms:    make(map[string]*BloomFilter),
		totalKeys: make(map[string]int64),
	}
}

// Update applies a content summary from an edge node.
func (ci *ContentIndex) Update(nodeID string, summary ContentSummaryData) {
	ci.mu.Lock()
	defer ci.mu.Unlock()

	if summary.HotKeys != nil {
		set := make(map[string]struct{}, len(summary.HotKeys))
		for _, k := range summary.HotKeys {
			set[k] = struct{}{}
		}
		ci.hotKeys[nodeID] = set
	}
	if summary.BloomBytes != nil && summary.BloomK > 0 {
		ci.blooms[nodeID] = NewBloomFilterFromBytes(summary.BloomBytes, summary.BloomK)
	}
	ci.totalKeys[nodeID] = summary.TotalKeys
}

// IsCached checks whether a key is likely cached on a given node.
// Returns (isHot, likelyCached).
func (ci *ContentIndex) IsCached(nodeID, key string) (bool, bool) {
	ci.mu.RLock()
	defer ci.mu.RUnlock()

	if hotKeys, ok := ci.hotKeys[nodeID]; ok {
		if _, found := hotKeys[key]; found {
			return true, true
		}
	}

	if bf, ok := ci.blooms[nodeID]; ok {
		if bf.ContainsString(key) {
			return false, true
		}
	}

	return false, false
}

// FindNodesWithKey returns two maps of node IDs that have the key in
// hot set or bloom filter respectively. Called once per dispatch
// instead of N times (once per node).
func (ci *ContentIndex) FindNodesWithKey(key string) (hotNodes, bloomNodes map[string]bool) {
	ci.mu.RLock()
	defer ci.mu.RUnlock()

	hot := make(map[string]bool)
	bloom := make(map[string]bool)

	for nodeID, hotKeys := range ci.hotKeys {
		if _, found := hotKeys[key]; found {
			hot[nodeID] = true
		}
	}
	for nodeID, bf := range ci.blooms {
		if hot[nodeID] {
			continue
		}
		if bf.ContainsString(key) {
			bloom[nodeID] = true
		}
	}
	return hot, bloom
}

// FindNodesWithKeySlice is a convenience wrapper that returns []string.
func (ci *ContentIndex) FindNodesWithKeySlice(key string) (hotNodes, bloomNodes []string) {
	hot, bloom := ci.FindNodesWithKey(key)
	for id := range hot {
		hotNodes = append(hotNodes, id)
	}
	for id := range bloom {
		bloomNodes = append(bloomNodes, id)
	}
	return
}

// RemoveNode removes all index data for a node.
func (ci *ContentIndex) RemoveNode(nodeID string) {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	delete(ci.hotKeys, nodeID)
	delete(ci.blooms, nodeID)
	delete(ci.totalKeys, nodeID)
}

// ContentSummaryData is the minimal data format for updating the index.
type ContentSummaryData struct {
	HotKeys    []string
	BloomBytes []byte
	BloomK     uint32
	TotalKeys  int64
}
