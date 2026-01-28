package webrtc

import (
	"testing"
)

func TestSequenceRewriterContiguous(t *testing.T) {
	sr := &sequenceRewriter{}

	// Send contiguous packets - should work normally
	results := make([]uint16, 10)
	for i := 0; i < 10; i++ {
		results[i] = sr.rewrite(uint16(100 + i))
	}

	// First packet should keep its original sequence
	if results[0] != 100 {
		t.Errorf("Expected first packet seq 100, got %d", results[0])
	}

	// All packets should be contiguous
	for i := 1; i < 10; i++ {
		if results[i] != results[i-1]+1 {
			t.Errorf("Packets not contiguous at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	t.Logf("Contiguous rewrite: %v", results)
}

func TestSequenceRewriterGap(t *testing.T) {
	sr := &sequenceRewriter{}

	// Send a few packets
	r1 := sr.rewrite(100)
	r2 := sr.rewrite(101)
	r3 := sr.rewrite(102)

	// Now simulate a large gap (like cached keyframe injection)
	r4 := sr.rewrite(5000) // Big jump

	// Continue from the gap
	r5 := sr.rewrite(5001)
	r6 := sr.rewrite(5002)

	results := []uint16{r1, r2, r3, r4, r5, r6}

	// All output sequence numbers should be contiguous
	for i := 1; i < len(results); i++ {
		if results[i] != results[i-1]+1 {
			t.Errorf("Sequence not contiguous after gap at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	t.Logf("Gap handling: input [100,101,102,5000,5001,5002] -> output %v", results)
}

func TestSequenceRewriterNegativeGap(t *testing.T) {
	sr := &sequenceRewriter{}

	// Start with high sequence numbers (live stream)
	r1 := sr.rewrite(5000)
	r2 := sr.rewrite(5001)
	r3 := sr.rewrite(5002)

	// Now inject old cached packets (going backwards)
	r4 := sr.rewrite(100) // Jump backwards

	// Continue from cached position
	r5 := sr.rewrite(101)
	r6 := sr.rewrite(102)

	results := []uint16{r1, r2, r3, r4, r5, r6}

	// All output should still be contiguous (monotonically increasing)
	for i := 1; i < len(results); i++ {
		if results[i] != results[i-1]+1 {
			t.Errorf("Sequence not contiguous after negative gap at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	t.Logf("Negative gap handling: input [5000,5001,5002,100,101,102] -> output %v", results)
}

func TestSequenceRewriterWraparound(t *testing.T) {
	sr := &sequenceRewriter{}

	// Start near wraparound point
	r1 := sr.rewrite(65534)
	r2 := sr.rewrite(65535)
	r3 := sr.rewrite(0) // Wraparound
	r4 := sr.rewrite(1)
	r5 := sr.rewrite(2)

	results := []uint16{r1, r2, r3, r4, r5}

	// Output should handle wraparound correctly
	for i := 1; i < len(results); i++ {
		expected := (results[i-1] + 1) & 0xFFFF // 16-bit wrap
		if results[i] != expected {
			t.Errorf("Wraparound not handled at %d: expected %d, got %d", i, expected, results[i])
		}
	}

	t.Logf("Wraparound handling: %v", results)
}

// Timestamp rewriter tests

func TestTimestampRewriterContiguous(t *testing.T) {
	tr := &timestampRewriter{}

	// Send contiguous timestamps (3000 units apart = ~33ms at 90kHz)
	results := make([]uint32, 5)
	for i := 0; i < 5; i++ {
		results[i] = tr.rewrite(uint32(90000 + i*3000))
	}

	// First timestamp should be preserved
	if results[0] != 90000 {
		t.Errorf("Expected first timestamp 90000, got %d", results[0])
	}

	// All timestamps should be increasing
	for i := 1; i < 5; i++ {
		if results[i] <= results[i-1] {
			t.Errorf("Timestamps not increasing at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	t.Logf("Contiguous timestamps: %v", results)
}

func TestTimestampRewriterLargeGap(t *testing.T) {
	tr := &timestampRewriter{}

	// Start with some packets
	r1 := tr.rewrite(100000)
	r2 := tr.rewrite(103000) // +3000
	r3 := tr.rewrite(106000) // +3000

	// Now simulate a large gap (cached to live transition)
	// Jump forward by 10 seconds worth of timestamps
	r4 := tr.rewrite(1006000) // +900000 (10 seconds at 90kHz)

	// Continue from there
	r5 := tr.rewrite(1009000)
	r6 := tr.rewrite(1012000)

	results := []uint32{r1, r2, r3, r4, r5, r6}

	// All output timestamps should be monotonically increasing
	for i := 1; i < len(results); i++ {
		if results[i] <= results[i-1] {
			t.Errorf("Timestamps not increasing after gap at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	// The gap should be smoothed (not 900000 units)
	gapAt4 := results[3] - results[2]
	if gapAt4 > 10000 {
		t.Errorf("Gap should be smoothed, got %d units", gapAt4)
	}

	t.Logf("Large gap handling: input [100000,103000,106000,1006000,1009000,1012000] -> output %v", results)
}

func TestTimestampRewriterNegativeGap(t *testing.T) {
	tr := &timestampRewriter{}

	// Start with high timestamps (live stream)
	r1 := tr.rewrite(1000000)
	r2 := tr.rewrite(1003000)
	r3 := tr.rewrite(1006000)

	// Now inject old cached packets (going backwards)
	r4 := tr.rewrite(100000) // Jump backwards by 900000

	// Continue from cached position
	r5 := tr.rewrite(103000)
	r6 := tr.rewrite(106000)

	results := []uint32{r1, r2, r3, r4, r5, r6}

	// All output should still be monotonically increasing
	for i := 1; i < len(results); i++ {
		if results[i] <= results[i-1] {
			t.Errorf("Timestamps not increasing after negative gap at %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	t.Logf("Negative gap handling: input [1000000,1003000,1006000,100000,103000,106000] -> output %v", results)
}

func TestTimestampRewriterCachedThenLive(t *testing.T) {
	tr := &timestampRewriter{}

	// Simulate the actual use case:
	// 1. Cached keyframe packets arrive first (old timestamps)
	// 2. Live packets follow (newer timestamps)

	// Cached packets from 5 seconds ago
	cachedBase := uint32(500000)
	r1 := tr.rewrite(cachedBase)
	r2 := tr.rewrite(cachedBase + 3000)
	r3 := tr.rewrite(cachedBase + 6000)
	r4 := tr.rewrite(cachedBase + 9000)

	// Live packets (5 seconds later = 450000 units at 90kHz)
	liveBase := cachedBase + 450000
	r5 := tr.rewrite(liveBase)
	r6 := tr.rewrite(liveBase + 3000)
	r7 := tr.rewrite(liveBase + 6000)

	results := []uint32{r1, r2, r3, r4, r5, r6, r7}

	// All should be monotonically increasing
	for i := 1; i < len(results); i++ {
		if results[i] <= results[i-1] {
			t.Errorf("Timestamps not increasing at cached->live transition %d: %d -> %d", i, results[i-1], results[i])
		}
	}

	// The jump from cached to live should be smoothed
	gapAtTransition := results[4] - results[3]
	if gapAtTransition > 10000 {
		t.Errorf("Cached->live transition should be smoothed, got %d units gap", gapAtTransition)
	}

	t.Logf("Cached->Live transition: %v (gap at transition: %d)", results, gapAtTransition)
}
