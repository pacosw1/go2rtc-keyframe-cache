package core

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// Helper to create a test packet with specified NAL type
func createTestPacket(seq uint16, nalType byte, isH265 bool) *Packet {
	var payload []byte
	if isH265 {
		// H.265: NAL type is in bits 1-6 of first byte
		payload = []byte{nalType << 1, 0x00, 0x00, 0x01}
	} else {
		// H.264: NAL type is in bits 0-4 of first byte
		payload = []byte{nalType, 0x00, 0x00, 0x01}
	}
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 3000,
		},
		Payload: payload,
	}
}

// H.265 NAL types
const (
	h265IDR = 19 // IDR_W_RADL
	h265VPS = 32
	h265SPS = 33
	h265PPS = 34
	h265P   = 1 // Non-IDR slice
)

// H.264 NAL types
const (
	h264IDR = 5
	h264SPS = 7
	h264PPS = 8
	h264P   = 1
)

func TestRingBufferBasics(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 10, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Initially empty
	buffered, kfPos, head := r.BufferStats()
	if buffered != 0 {
		t.Errorf("Expected 0 buffered packets, got %d", buffered)
	}
	if kfPos != -1 {
		t.Errorf("Expected keyframe pos -1, got %d", kfPos)
	}
	if head != 0 {
		t.Errorf("Expected head 0, got %d", head)
	}

	// Add some P-frames (no keyframe yet)
	for i := 0; i < 5; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	buffered, kfPos, head = r.BufferStats()
	if buffered != 5 {
		t.Errorf("Expected 5 buffered packets, got %d", buffered)
	}
	if kfPos != -1 {
		t.Errorf("Expected keyframe pos -1 (no keyframe), got %d", kfPos)
	}

	t.Logf("Ring buffer basics: buffered=%d, kfPos=%d, head=%d", buffered, kfPos, head)
}

func TestKeyframeTracking(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add VPS, SPS, PPS first
	r.Input(createTestPacket(0, h265VPS, true))
	r.Input(createTestPacket(1, h265SPS, true))
	r.Input(createTestPacket(2, h265PPS, true))

	// Check param sets are stored
	_, _, _, paramPackets, _ := r.BufferStatsDetailed()
	if paramPackets != 3 {
		t.Errorf("Expected 3 param sets, got %d", paramPackets)
	}

	// Add keyframe
	r.Input(createTestPacket(3, h265IDR, true))

	_, kfPos, _ := r.BufferStats()
	if kfPos != 3 {
		t.Errorf("Expected keyframe pos 3, got %d", kfPos)
	}

	// Add more P-frames
	for i := 4; i < 10; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	// Keyframe position should still be 3
	_, kfPos, _ = r.BufferStats()
	if kfPos != 3 {
		t.Errorf("Expected keyframe pos still 3, got %d", kfPos)
	}

	// Add another keyframe
	r.Input(createTestPacket(10, h265IDR, true))

	_, kfPos, _ = r.BufferStats()
	if kfPos != 10 {
		t.Errorf("Expected keyframe pos 10, got %d", kfPos)
	}

	t.Logf("Keyframe tracking working correctly")
}

func TestChildStartsInBufferMode(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add param sets and keyframe
	r.Input(createTestPacket(0, h265VPS, true))
	r.Input(createTestPacket(1, h265SPS, true))
	r.Input(createTestPacket(2, h265PPS, true))
	r.Input(createTestPacket(3, h265IDR, true))

	// Add some P-frames
	for i := 4; i < 10; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	// Create a child node
	var receivedPackets []*Packet
	var mu sync.Mutex
	child := &Node{
		id: NewID(),
		Input: func(packet *Packet) {
			mu.Lock()
			receivedPackets = append(receivedPackets, packet)
			mu.Unlock()
		},
	}

	// Add child - should start in buffer mode
	r.AppendChild(child)

	// Check child state
	mode, readPos := r.GetChildState(child)
	if mode != "buffering" {
		t.Errorf("Expected child mode 'buffering', got '%s'", mode)
	}
	if readPos != 3 { // Should start at keyframe position
		t.Errorf("Expected readPos 3 (keyframe), got %d", readPos)
	}

	// Wait for pump to send some packets
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := len(receivedPackets)
	mu.Unlock()

	// Should have received param sets + buffered packets
	if count < 3 { // At least param sets
		t.Errorf("Expected at least 3 packets (param sets), got %d", count)
	}

	t.Logf("Child started in buffer mode, received %d packets", count)
}

func TestSwitchToLiveOnKeyframe(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add param sets and first keyframe
	r.Input(createTestPacket(0, h265VPS, true))
	r.Input(createTestPacket(1, h265SPS, true))
	r.Input(createTestPacket(2, h265PPS, true))
	r.Input(createTestPacket(3, h265IDR, true))

	// Add some P-frames to create buffer content
	for i := 4; i < 20; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	// Create a child
	var receivedPackets []*Packet
	var mu sync.Mutex
	child := &Node{
		id: NewID(),
		Input: func(packet *Packet) {
			mu.Lock()
			receivedPackets = append(receivedPackets, packet)
			mu.Unlock()
		},
	}

	// Add child
	r.AppendChild(child)

	// Verify in buffer mode
	mode, _ := r.GetChildState(child)
	if mode != "buffering" {
		t.Errorf("Expected 'buffering', got '%s'", mode)
	}

	// Wait a bit for pump to start
	time.Sleep(50 * time.Millisecond)

	// Send a new keyframe - should trigger switch to live
	r.Input(createTestPacket(20, h265IDR, true))

	// Wait for mode switch
	time.Sleep(50 * time.Millisecond)

	// Check child is now in live mode
	mode, _ = r.GetChildState(child)
	if mode != "live" {
		t.Errorf("Expected 'live' after keyframe, got '%s'", mode)
	}

	// Send more live packets
	for i := 21; i < 25; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	count := len(receivedPackets)
	mu.Unlock()

	t.Logf("After switch to live, total packets received: %d", count)

	// Should have received buffered packets + live packets
	if count < 10 {
		t.Errorf("Expected at least 10 packets, got %d", count)
	}
}

func TestNoKeyframeStartsInLiveMode(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add only P-frames (no keyframe)
	for i := 0; i < 5; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	// Create child - should start in live mode since no keyframe
	child := &Node{
		id:    NewID(),
		Input: func(packet *Packet) {},
	}

	r.AppendChild(child)

	mode, _ := r.GetChildState(child)
	if mode != "live" {
		t.Errorf("Expected 'live' mode when no keyframe, got '%s'", mode)
	}

	t.Logf("Correctly started in live mode when no keyframe available")
}

func TestRingBufferWraparound(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	// Note: Minimum buffer size is 100, so we test with that
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add keyframe at position 0
	r.Input(createTestPacket(0, h265IDR, true))

	_, kfPos, _ := r.BufferStats()
	if kfPos != 0 {
		t.Errorf("Expected kfPos 0, got %d", kfPos)
	}

	// Fill buffer past 100 to test wraparound
	for i := 1; i < 150; i++ {
		r.Input(createTestPacket(uint16(i), h265P, true))
	}

	buffered, kfPos, head := r.BufferStats()

	// Buffer should be full (100 packets)
	if buffered != 100 {
		t.Errorf("Expected 100 buffered, got %d", buffered)
	}

	// Head should have wrapped: 150 % 100 = 50
	if head != 50 {
		t.Errorf("Expected head 50, got %d", head)
	}

	// Original keyframe at pos 0 was overwritten, position is stale
	t.Logf("Wraparound: buffered=%d, kfPos=%d (stale), head=%d", buffered, kfPos, head)

	// Add new keyframe
	r.Input(createTestPacket(150, h265IDR, true))

	_, kfPos, _ = r.BufferStats()
	// New keyframe should be at position 50 (where head was)
	if kfPos != 50 {
		t.Errorf("Expected new keyframe at pos 50, got %d", kfPos)
	}
	t.Logf("New keyframe pos: %d", kfPos)
}

func TestH264Support(t *testing.T) {
	codec := &Codec{Name: CodecH264}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add H.264 param sets and keyframe
	r.Input(createTestPacket(0, h264SPS, false))
	r.Input(createTestPacket(1, h264PPS, false))
	r.Input(createTestPacket(2, h264IDR, false))

	_, kfPos, _ := r.BufferStats()
	if kfPos != 2 {
		t.Errorf("Expected H.264 keyframe at pos 2, got %d", kfPos)
	}

	_, _, _, paramPackets, _ := r.BufferStatsDetailed()
	if paramPackets != 2 {
		t.Errorf("Expected 2 H.264 param sets, got %d", paramPackets)
	}

	t.Logf("H.264 support working correctly")
}

func TestBufferDisabledForAudio(t *testing.T) {
	// Audio codec should not enable buffering
	codec := &Codec{Name: CodecPCMA}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Child should start in live mode (no buffering for audio)
	child := &Node{
		id:    NewID(),
		Input: func(packet *Packet) {},
	}

	r.AppendChild(child)

	// For audio, childState won't be set since buffering is disabled
	mode, _ := r.GetChildState(child)
	if mode != "unknown" {
		t.Errorf("Expected 'unknown' for audio (no state tracking), got '%s'", mode)
	}

	t.Logf("Correctly disabled buffering for audio codec")
}

func TestCleanupOnChildRemoval(t *testing.T) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 100, MaxBytes: 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	// Add keyframe
	r.Input(createTestPacket(0, h265IDR, true))

	// Create and add child
	child := &Node{
		id:    NewID(),
		Input: func(packet *Packet) {},
	}
	r.AppendChild(child)

	// Verify child state exists
	mode, _ := r.GetChildState(child)
	if mode == "unknown" {
		t.Errorf("Expected child state to exist")
	}

	// Remove child
	r.RemoveChild(child)

	// Wait for cleanup
	time.Sleep(20 * time.Millisecond)

	// Verify state is cleaned up
	mode, _ = r.GetChildState(child)
	if mode != "unknown" {
		t.Errorf("Expected 'unknown' after removal, got '%s'", mode)
	}

	t.Logf("Cleanup on child removal working correctly")
}

// Benchmark for ring buffer performance
func BenchmarkRingBufferAdd(b *testing.B) {
	codec := &Codec{Name: CodecH265}
	config := KeyframeCacheConfig{Enabled: true, MaxPackets: 500, MaxBytes: 2 * 1024 * 1024}
	r := NewReceiverWithCache(nil, codec, config)

	packet := createTestPacket(0, h265P, true)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packet.SequenceNumber = uint16(i)
		r.addToRingBuffer(packet)
	}
}
