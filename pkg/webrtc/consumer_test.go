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
