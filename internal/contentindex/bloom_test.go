package contentindex

import (
	"testing"
)

func TestBloomFilterBasic(t *testing.T) {
	bf := NewBloomFilter(1000, 0.01)

	bf.AddString("file1.txt")
	bf.AddString("video.mp4")
	bf.AddString("image.jpg")

	if !bf.ContainsString("file1.txt") {
		t.Fatal("expected file1.txt to be contained")
	}
	if !bf.ContainsString("video.mp4") {
		t.Fatal("expected video.mp4 to be contained")
	}
	if !bf.ContainsString("image.jpg") {
		t.Fatal("expected image.jpg to be contained")
	}

	if bf.ContainsString("missing.txt") {
		// This is a false positive - should be rare with 1% FP rate
		t.Log("false positive detected (expected ~1% of the time)")
	}
}

func TestBloomFilterSerialization(t *testing.T) {
	bf := NewBloomFilter(500, 0.001)
	for i := 0; i < 100; i++ {
		bf.AddString("key" + string(rune('0'+i%10)))
	}

	data := bf.Bytes()
	if len(data) == 0 {
		t.Fatal("serialized data should not be empty")
	}

	bf2 := NewBloomFilterFromBytes(data, bf.K())
	for i := 0; i < 100; i++ {
		key := "key" + string(rune('0'+i%10))
		if !bf2.ContainsString(key) {
			t.Fatalf("deserialized filter missing key: %s", key)
		}
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	bf := NewBloomFilter(10000, 0.01)

	for i := 0; i < 10000; i++ {
		bf.AddString("existing-key-" + string(rune(i)))
	}

	falsePositives := 0
	testCount := 10000
	for i := 0; i < testCount; i++ {
		if bf.ContainsString("nonexistent-key-" + string(rune(i))) {
			falsePositives++
		}
	}

	fpRate := float64(falsePositives) / float64(testCount)
	t.Logf("measured FP rate: %.4f (target: 0.01)", fpRate)
	if fpRate > 0.05 {
		t.Fatalf("false positive rate too high: %.4f", fpRate)
	}
}

func TestBloomFilterEmpty(t *testing.T) {
	bf := NewBloomFilter(100, 0.01)
	if bf.ContainsString("anything") {
		t.Fatal("empty filter should not contain anything")
	}
	data := bf.Bytes()
	if len(data) == 0 {
		t.Fatal("empty filter should still serialize")
	}
}

func TestContentIndexUpdate(t *testing.T) {
	ci := NewContentIndex()

	ci.Update("node1", ContentSummaryData{
		HotKeys:   []string{"hot1", "hot2"},
		TotalKeys: 100,
	})

	isHot, likelyCached := ci.IsCached("node1", "hot1")
	if !isHot || !likelyCached {
		t.Fatalf("expected hot1 to be hot+cached, got isHot=%v likelyCached=%v", isHot, likelyCached)
	}

	isHot, likelyCached = ci.IsCached("node1", "missing")
	if isHot || likelyCached {
		t.Fatalf("expected missing to not be cached, got isHot=%v likelyCached=%v", isHot, likelyCached)
	}
}

func TestContentIndexBloomLookup(t *testing.T) {
	ci := NewContentIndex()

	bf := NewBloomFilter(100, 0.01)
	bf.AddString("bloom-key-1")
	bf.AddString("bloom-key-2")

	ci.Update("node2", ContentSummaryData{
		BloomBytes: bf.Bytes(),
		BloomK:     bf.K(),
		TotalKeys:  2,
	})

	isHot, likelyCached := ci.IsCached("node2", "bloom-key-1")
	if isHot {
		t.Fatal("should not be hot")
	}
	if !likelyCached {
		t.Fatal("expected bloom-key-1 to be likely cached via bloom")
	}
}

func TestContentIndexRemoveNode(t *testing.T) {
	ci := NewContentIndex()

	ci.Update("node1", ContentSummaryData{
		HotKeys:   []string{"hot1"},
		TotalKeys: 1,
	})

	ci.RemoveNode("node1")

	_, likelyCached := ci.IsCached("node1", "hot1")
	if likelyCached {
		t.Fatal("node should have been removed")
	}
}

func BenchmarkBloomFilterAdd(b *testing.B) {
	bf := NewBloomFilter(b.N, 0.01)
	key := []byte("benchmark-key")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.Add(key)
	}
}

func BenchmarkBloomFilterContains(b *testing.B) {
	bf := NewBloomFilter(100000, 0.01)
	for i := 0; i < 100000; i++ {
		bf.AddString("key" + string(rune(i)))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bf.ContainsString("key" + string(rune(i%100000)))
	}
}
