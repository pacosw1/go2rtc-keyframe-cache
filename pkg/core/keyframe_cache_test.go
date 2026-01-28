package core

import (
	"sync"
	"testing"
	"time"
)

func TestKeyframeCacheH265(t *testing.T) {
	// Create a receiver with keyframe caching enabled for H.265
	codec := &Codec{Name: CodecH265, ClockRate: 90000}
	media := &Media{Kind: KindVideo}
	recv := NewReceiver(media, codec)

	// Verify buffer is empty initially
	packets, kfPos, _ := recv.BufferStats()
	if packets != 0 {
		t.Errorf("Expected empty buffer, got %d packets", packets)
	}
	if kfPos != -1 {
		t.Errorf("Expected no keyframe pos (-1), got %d", kfPos)
	}

	// Send a non-keyframe packet (P-frame, NAL type 1)
	pFrame := &Packet{Payload: []byte{0x02, 0x00, 0x00, 0x00, 0x00}} // NAL type 1
	recv.Input(pFrame)

	// Buffer should have packet but no keyframe position
	packets, kfPos, _ = recv.BufferStats()
	if packets != 1 {
		t.Errorf("Expected 1 packet in buffer, got %d", packets)
	}
	if kfPos != -1 {
		t.Errorf("Expected no keyframe pos before keyframe, got %d", kfPos)
	}

	// Send a keyframe (IDR frame, NAL type 19)
	keyframe := &Packet{Payload: []byte{0x26, 0x00, 0x00, 0x00, 0x00}} // NAL type 19
	recv.Input(keyframe)

	// Buffer should have keyframe position
	packets, kfPos, _ = recv.BufferStats()
	if packets != 2 {
		t.Errorf("Expected 2 packets in buffer, got %d", packets)
	}
	if kfPos != 1 { // Second packet is keyframe
		t.Errorf("Expected keyframe at pos 1, got %d", kfPos)
	}

	// Send another P-frame
	pFrame2 := &Packet{Payload: []byte{0x02, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(pFrame2)

	// Buffer should have 3 packets
	packets, kfPos, _ = recv.BufferStats()
	if packets != 3 {
		t.Errorf("Expected 3 packets in buffer, got %d", packets)
	}
	if kfPos != 1 { // Keyframe still at pos 1
		t.Errorf("Expected keyframe still at pos 1, got %d", kfPos)
	}

	// Send a new keyframe - keyframe pos should update
	keyframe2 := &Packet{Payload: []byte{0x28, 0x00, 0x00, 0x00, 0x00}} // NAL type 20
	recv.Input(keyframe2)

	packets, kfPos, _ = recv.BufferStats()
	if packets != 4 {
		t.Errorf("Expected 4 packets in buffer, got %d", packets)
	}
	if kfPos != 3 { // New keyframe at pos 3
		t.Errorf("Expected new keyframe at pos 3, got %d", kfPos)
	}
}

func TestKeyframeCacheSendToNewConsumer(t *testing.T) {
	// Create a receiver with keyframe caching
	codec := &Codec{Name: CodecH265, ClockRate: 90000}
	media := &Media{Kind: KindVideo}
	recv := NewReceiver(media, codec)

	// Send some packets including a keyframe
	keyframe := &Packet{Payload: []byte{0x26, 0x00, 0x00, 0x00, 0x00}} // IDR
	pFrame1 := &Packet{Payload: []byte{0x02, 0x00, 0x01, 0x02, 0x03}}
	pFrame2 := &Packet{Payload: []byte{0x02, 0x00, 0x04, 0x05, 0x06}}

	recv.Input(keyframe)
	recv.Input(pFrame1)
	recv.Input(pFrame2)

	// Track packets received by new consumer
	var receivedPackets []*Packet
	var mu sync.Mutex

	// Create a new consumer (child node)
	consumer := &Node{
		id: NewID(),
		Input: func(pkt *Packet) {
			mu.Lock()
			receivedPackets = append(receivedPackets, pkt)
			mu.Unlock()
		},
	}

	// Add consumer - should start in buffer mode and receive cached packets
	recv.AppendChild(consumer)

	// Wait for buffer pump to send packets
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(receivedPackets)
	mu.Unlock()

	// Consumer should have received buffered packets (from keyframe onwards)
	// Note: With new time-shift buffer, consumer starts at keyframe position
	if count < 3 {
		t.Errorf("Expected at least 3 packets, got %d", count)
	}
}

func TestKeyframeCacheH264(t *testing.T) {
	// Create a receiver with keyframe caching enabled for H.264
	codec := &Codec{Name: CodecH264, ClockRate: 90000}
	media := &Media{Kind: KindVideo}
	recv := NewReceiver(media, codec)

	// Send a non-keyframe packet (P-frame, NAL type 1)
	// H.264 NAL header: forbidden_zero_bit(1) + nal_ref_idc(2) + nal_unit_type(5)
	// 0x41 = 0b01000001 = ref_idc=2, type=1 (P-frame)
	pFrame := &Packet{Payload: []byte{0x41, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(pFrame)

	// Buffer should have packet but no keyframe
	packets, kfPos, _ := recv.BufferStats()
	if packets != 1 {
		t.Errorf("Expected 1 packet in buffer, got %d", packets)
	}
	if kfPos != -1 {
		t.Errorf("Expected no keyframe before IDR, got pos %d", kfPos)
	}

	// Send a keyframe (IDR slice, NAL type 5)
	// 0x65 = 0b01100101 = ref_idc=3, type=5 (IDR)
	keyframe := &Packet{Payload: []byte{0x65, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(keyframe)

	// Buffer should have keyframe position
	packets, kfPos, _ = recv.BufferStats()
	if packets != 2 {
		t.Errorf("Expected 2 packets in buffer, got %d", packets)
	}
	if kfPos != 1 {
		t.Errorf("Expected keyframe at pos 1, got %d", kfPos)
	}
}

func TestKeyframeCacheDisabled(t *testing.T) {
	// Create a receiver with caching disabled
	codec := &Codec{Name: CodecH265, ClockRate: 90000}
	media := &Media{Kind: KindVideo}

	config := KeyframeCacheConfig{Enabled: false}
	recv := NewReceiverWithCache(media, codec, config)

	// Send a keyframe
	keyframe := &Packet{Payload: []byte{0x26, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(keyframe)

	// Buffer should remain empty when disabled
	packets, _, _ := recv.BufferStats()
	if packets != 0 {
		t.Errorf("Expected empty buffer when disabled, got %d packets", packets)
	}
}

func TestKeyframeCacheAudioNoCache(t *testing.T) {
	// Audio codecs should not have keyframe caching
	codec := &Codec{Name: CodecOpus, ClockRate: 48000}
	media := &Media{Kind: KindAudio}
	recv := NewReceiver(media, codec)

	// Send a packet
	packet := &Packet{Payload: []byte{0x01, 0x02, 0x03, 0x04}}
	recv.Input(packet)

	// Buffer should be empty for audio (buffering disabled)
	packets, _, _ := recv.BufferStats()
	if packets != 0 {
		t.Errorf("Expected no buffering for audio, got %d packets", packets)
	}
}
