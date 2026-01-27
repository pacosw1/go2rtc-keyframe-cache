package core

import (
	"testing"
)

func TestKeyframeCacheH265(t *testing.T) {
	// Create a receiver with keyframe caching enabled for H.265
	codec := &Codec{Name: CodecH265, ClockRate: 90000}
	media := &Media{Kind: KindVideo}
	recv := NewReceiver(media, codec)

	// Verify cache is empty initially
	packets, bytes := recv.CacheStats()
	if packets != 0 || bytes != 0 {
		t.Errorf("Expected empty cache, got %d packets, %d bytes", packets, bytes)
	}

	// Send a non-keyframe packet (P-frame, NAL type 1)
	pFrame := &Packet{Payload: []byte{0x02, 0x00, 0x00, 0x00, 0x00}} // NAL type 1
	recv.Input(pFrame)

	// Cache should still be empty (no keyframe received yet)
	packets, bytes = recv.CacheStats()
	if packets != 0 {
		t.Errorf("Expected empty cache before keyframe, got %d packets", packets)
	}

	// Send a keyframe (IDR frame, NAL type 19)
	keyframe := &Packet{Payload: []byte{0x26, 0x00, 0x00, 0x00, 0x00}} // NAL type 19
	recv.Input(keyframe)

	// Cache should now have the keyframe
	packets, bytes = recv.CacheStats()
	if packets != 1 {
		t.Errorf("Expected 1 packet in cache after keyframe, got %d", packets)
	}

	// Send another P-frame
	pFrame2 := &Packet{Payload: []byte{0x02, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(pFrame2)

	// Cache should have both packets
	packets, bytes = recv.CacheStats()
	if packets != 2 {
		t.Errorf("Expected 2 packets in cache, got %d", packets)
	}

	// Send a new keyframe - should reset cache
	keyframe2 := &Packet{Payload: []byte{0x28, 0x00, 0x00, 0x00, 0x00}} // NAL type 20
	recv.Input(keyframe2)

	packets, bytes = recv.CacheStats()
	if packets != 1 {
		t.Errorf("Expected 1 packet after new keyframe (cache reset), got %d", packets)
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

	// Create a new consumer (child node)
	consumer := &Node{
		Input: func(pkt *Packet) {
			receivedPackets = append(receivedPackets, pkt)
		},
	}

	// Add consumer - should receive cached keyframe packets
	recv.AppendChild(consumer)

	// Consumer should have received 3 cached packets
	if len(receivedPackets) != 3 {
		t.Errorf("Expected new consumer to receive 3 cached packets, got %d", len(receivedPackets))
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

	// Cache should be empty
	packets, _ := recv.CacheStats()
	if packets != 0 {
		t.Errorf("Expected empty cache before keyframe, got %d packets", packets)
	}

	// Send a keyframe (IDR slice, NAL type 5)
	// 0x65 = 0b01100101 = ref_idc=3, type=5 (IDR)
	keyframe := &Packet{Payload: []byte{0x65, 0x00, 0x00, 0x00, 0x00}}
	recv.Input(keyframe)

	// Cache should have the keyframe
	packets, _ = recv.CacheStats()
	if packets != 1 {
		t.Errorf("Expected 1 packet in cache after keyframe, got %d", packets)
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

	// Cache should remain empty when disabled
	packets, _ := recv.CacheStats()
	if packets != 0 {
		t.Errorf("Expected empty cache when disabled, got %d packets", packets)
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

	// Cache should be empty for audio
	packets, _ := recv.CacheStats()
	if packets != 0 {
		t.Errorf("Expected no caching for audio, got %d packets", packets)
	}
}
