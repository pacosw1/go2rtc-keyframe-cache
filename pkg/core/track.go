package core

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/rs/zerolog/log"
)

// timeNow is a variable for testing - can be overridden
var timeNow = time.Now

var ErrCantGetTrack = errors.New("can't get track")

// CacheEvent represents an event from the keyframe cache system
type CacheEvent struct {
	Type       string `json:"type"`        // "keyframe_cached", "consumer_connected", "cache_sent"
	StreamName string `json:"stream_name"` // Name of the stream
	Codec      string `json:"codec"`       // Codec name (H264, H265)
	Packets    int    `json:"packets"`     // Number of packets in cache
	Bytes      int    `json:"bytes"`       // Number of bytes in cache
	Timestamp  int64  `json:"timestamp"`   // Unix timestamp in milliseconds
}

// CacheEventHandler is called when cache events occur
// Implement this to receive notifications about keyframe caching
type CacheEventHandler func(event CacheEvent)

// Global event handler - set via SetCacheEventHandler
var globalCacheEventHandler CacheEventHandler

// SetCacheEventHandler sets a global handler for cache events
// This is called from the main go2rtc process to receive notifications
func SetCacheEventHandler(handler CacheEventHandler) {
	globalCacheEventHandler = handler
	log.Info().Msg("[keyframe-cache] Event handler registered")
}

// emitCacheEvent sends an event to the registered handler
func emitCacheEvent(eventType, streamName, codec string, packets, bytes int) {
	if globalCacheEventHandler == nil {
		return
	}
	event := CacheEvent{
		Type:       eventType,
		StreamName: streamName,
		Codec:      codec,
		Packets:    packets,
		Bytes:      bytes,
		Timestamp:  timeNowMs(),
	}
	// Call handler in goroutine to avoid blocking
	go globalCacheEventHandler(event)
}

// timeNowMs returns current time in milliseconds
func timeNowMs() int64 {
	return timeNow().UnixMilli()
}

// KeyframeCacheConfig controls keyframe caching behavior
type KeyframeCacheConfig struct {
	// Enabled turns on keyframe caching for instant playback
	Enabled bool
	// MaxPackets limits buffer size (default: 500 packets, ~1-2 seconds of 4K video)
	MaxPackets int
	// MaxBytes limits buffer size in bytes (default: 2MB)
	MaxBytes int
}

// DefaultKeyframeCacheConfig returns sensible defaults for keyframe caching
// This can be modified at startup to change global behavior
var DefaultKeyframeCacheConfig = KeyframeCacheConfig{
	Enabled:    true, // RE-ENABLED with timing fix
	MaxPackets: 500,
	MaxBytes:   2 * 1024 * 1024, // 2MB
}

// SetGlobalKeyframeCacheConfig updates the default keyframe cache configuration
// Call this during initialization to configure caching behavior globally
func SetGlobalKeyframeCacheConfig(enabled bool, maxPackets, maxBytes int) {
	DefaultKeyframeCacheConfig.Enabled = enabled
	if maxPackets > 0 {
		DefaultKeyframeCacheConfig.MaxPackets = maxPackets
	}
	if maxBytes > 0 {
		DefaultKeyframeCacheConfig.MaxBytes = maxBytes
	}
}

// childBufferState tracks per-child playback state for time-shift buffering
type childBufferState struct {
	mode       string // "buffering" = playing from buffer, "live" = real-time
	readPos    int    // Current read position in ring buffer
	pumping    bool   // Whether pump goroutine is running
	stopPump   chan struct{}
}

type Receiver struct {
	Node

	// Deprecated: should be removed
	Media *Media `json:"-"`
	// Deprecated: should be removed
	ID byte `json:"-"` // Channel for RTSP, PayloadType for MPEG-TS

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`

	// Time-shift buffer for instant playback with catch-up
	// Stores ~2 seconds of packets in a ring buffer
	ringBuffer      []*Packet
	ringHead        int  // Write position (next slot to write)
	ringSize        int  // Number of valid packets in buffer
	ringKeyframePos int  // Position of last keyframe in buffer
	ringMu          sync.RWMutex
	ringConfig      KeyframeCacheConfig

	// Per-child state for time-shift playback
	childState   map[*Node]*childBufferState
	childStateMu sync.RWMutex

	// Parameter sets (VPS/SPS/PPS) stored separately - needed before keyframe
	paramSets      []*Packet
	paramSetsBytes int
	paramSetsMu    sync.RWMutex

	// Stream identification for event emission
	streamName string
}

func NewReceiver(media *Media, codec *Codec) *Receiver {
	return NewReceiverWithCache(media, codec, DefaultKeyframeCacheConfig)
}

// NewReceiverWithCache creates a Receiver with configurable time-shift buffering
// The buffer enables instant playback: new consumers start from cached keyframe,
// play buffered content, then seamlessly switch to live when next keyframe arrives
func NewReceiverWithCache(media *Media, codec *Codec, cacheConfig KeyframeCacheConfig) *Receiver {
	// Ring buffer size: ~2 seconds at 60fps = 120 packets, but keyframes can be large
	// Use MaxPackets from config (default 500) for the ring buffer
	ringBufferSize := cacheConfig.MaxPackets
	if ringBufferSize < 100 {
		ringBufferSize = 100
	}

	r := &Receiver{
		Node:            Node{id: NewID(), Codec: codec},
		Media:           media,
		ringConfig:      cacheConfig,
		ringBuffer:      make([]*Packet, ringBufferSize),
		ringHead:        0,
		ringSize:        0,
		ringKeyframePos: -1, // No keyframe yet
		childState:      make(map[*Node]*childBufferState),
	}

	// Only enable buffering for video codecs that support keyframes
	isVideoWithKeyframes := codec != nil && (codec.Name == CodecH264 || codec.Name == CodecH265)
	bufferEnabled := cacheConfig.Enabled && isVideoWithKeyframes

	r.Input = func(packet *Packet) {
		r.Bytes += len(packet.Payload)
		r.Packets++

		if bufferEnabled {
			// Add packet to ring buffer and check if it's a keyframe
			isKeyframe := r.addToRingBuffer(packet)

			// If this is a keyframe, switch all buffering children to live mode
			if isKeyframe {
				r.switchBufferingChildrenToLive()
			}

			// Send to children in "live" mode only
			// Children in "buffering" mode get packets from pump goroutine
			// Children with nil state (not yet initialized) also get packets to maintain backward compat
			r.childStateMu.RLock()
			for _, child := range r.childs {
				state := r.childState[child]
				// Send if in "live" mode OR if state not set (backward compat with existing code)
				if state == nil || state.mode == "live" {
					child.Input(packet)
				}
			}
			r.childStateMu.RUnlock()
		} else {
			// Buffering disabled - send to all children immediately
			for _, child := range r.childs {
				child.Input(packet)
			}
		}
	}

	// Set up callback for new consumers
	if bufferEnabled {
		log.Debug().Str("codec", codec.Name).Uint32("receiverId", r.id).Msg("[timeshift] Buffer enabled")
		r.OnChildAdded = func(child *Node) {
			r.startChildInBufferMode(child)
		}
		r.OnChildRemoved = func(child *Node) {
			r.cleanupChildState(child)
		}
	} else {
		log.Debug().Str("codec", codec.Name).Bool("configEnabled", cacheConfig.Enabled).Bool("isVideoWithKeyframes", isVideoWithKeyframes).Msg("[timeshift] NOT enabled for receiver")
	}

	return r
}

// addToRingBuffer adds a packet to the ring buffer
// Returns true if the packet is a keyframe (IDR)
func (r *Receiver) addToRingBuffer(packet *Packet) bool {
	r.ringMu.Lock()
	defer r.ringMu.Unlock()

	nalInfo := r.classifyNAL(packet)

	// Store parameter sets separately - they're always needed before keyframe
	// Only keep ONE of each type (VPS, SPS, PPS) - newer replaces older
	if nalInfo.isParamSet {
		r.paramSetsMu.Lock()
		// For H.265: VPS=32, SPS=33, PPS=34
		// For H.264: SPS=7, PPS=8
		// Replace existing param set of same type instead of accumulating
		replaced := false
		for i, existing := range r.paramSets {
			existingInfo := r.classifyNAL(existing)
			if existingInfo.typeName == nalInfo.typeName {
				r.paramSetsBytes -= len(existing.Payload)
				r.paramSets[i] = packet
				r.paramSetsBytes += len(packet.Payload)
				replaced = true
				break
			}
		}
		if !replaced {
			r.paramSets = append(r.paramSets, packet)
			r.paramSetsBytes += len(packet.Payload)
		}
		r.paramSetsMu.Unlock()
	}

	// Add packet to ring buffer
	r.ringBuffer[r.ringHead] = packet

	// Track keyframe position
	if nalInfo.isIDR {
		wasFirst := r.ringKeyframePos < 0
		r.ringKeyframePos = r.ringHead
		if wasFirst {
			log.Info().Uint32("receiverId", r.id).Str("type", nalInfo.typeName).Int("pos", r.ringHead).Int("bufferSize", r.ringSize).Msg("[timeshift] First keyframe cached!")
		}
	}

	// Advance head (circular)
	r.ringHead = (r.ringHead + 1) % len(r.ringBuffer)
	if r.ringSize < len(r.ringBuffer) {
		r.ringSize++
	}

	return nalInfo.isIDR
}

// switchBufferingChildrenToLive switches all children in "buffering" mode to "live" mode
// Called when a new keyframe arrives - children jump to live stream
func (r *Receiver) switchBufferingChildrenToLive() {
	r.childStateMu.Lock()
	defer r.childStateMu.Unlock()

	switched := 0
	for child, state := range r.childState {
		if state != nil && state.mode == "buffering" {
			state.mode = "live"
			// Signal pump goroutine to stop
			if state.stopPump != nil {
				close(state.stopPump)
				state.stopPump = nil
			}
			switched++
			log.Trace().Uint32("childId", child.id).Msg("[timeshift] Child switched to live")
		}
	}

	if switched > 0 {
		log.Debug().Int("count", switched).Msg("[timeshift] Switched children to live on keyframe")
	}
}

// startChildInBufferMode starts a new child playing from the buffer
func (r *Receiver) startChildInBufferMode(child *Node) {
	codecName := ""
	if r.Codec != nil {
		codecName = r.Codec.Name
	}

	// Skip time-shift for consumers that don't need it (e.g., recorders)
	if child.SkipTimeshift {
		log.Info().Uint32("childId", child.id).Str("codec", codecName).Msg("[timeshift] Skipped (consumer opted out)")
		r.childStateMu.Lock()
		r.childState[child] = &childBufferState{mode: "live"}
		r.childStateMu.Unlock()
		return
	}

	r.ringMu.RLock()
	keyframePos := r.ringKeyframePos
	ringSize := r.ringSize
	bufferLen := len(r.ringBuffer)

	// Find param sets that precede the keyframe in the buffer
	// Cameras typically send VPS→SPS→PPS→IDR, so scan backwards to find them
	startPos := keyframePos
	paramSetsFound := 0
	for i := 1; i <= 10 && paramSetsFound < 3; i++ {
		checkPos := (keyframePos - i + bufferLen) % bufferLen
		if r.ringBuffer[checkPos] != nil {
			info := r.classifyNAL(r.ringBuffer[checkPos])
			if info.isParamSet {
				startPos = checkPos
				paramSetsFound++
			}
		}
	}
	r.ringMu.RUnlock()

	// If no keyframe in buffer yet, start in live mode
	if keyframePos < 0 || ringSize == 0 {
		log.Info().Uint32("childId", child.id).Str("codec", codecName).Int("keyframePos", keyframePos).Int("ringSize", ringSize).Msg("[timeshift] No keyframe yet, starting live")
		r.childStateMu.Lock()
		r.childState[child] = &childBufferState{mode: "live"}
		r.childStateMu.Unlock()
		return
	}

	// Create buffer state for this child
	// Start from param sets position (before keyframe) to include VPS/SPS/PPS
	state := &childBufferState{
		mode:     "buffering",
		readPos:  startPos,
		pumping:  true,
		stopPump: make(chan struct{}),
	}

	r.childStateMu.Lock()
	r.childState[child] = state
	r.childStateMu.Unlock()

	log.Info().Uint32("childId", child.id).Str("codec", codecName).Int("startPos", startPos).Int("keyframePos", keyframePos).Int("paramSetsFound", paramSetsFound).Int("ringSize", ringSize).Msg("[timeshift] Starting from buffer with param sets")

	// Start pump goroutine to feed buffered packets
	go r.pumpBufferToChild(child, state)
}

// clonePacket creates a copy of an RTP packet
// This is necessary because consumers may modify packet fields (like SequenceNumber)
// and we don't want to corrupt the original packet in the buffer
func clonePacket(p *Packet) *Packet {
	if p == nil {
		return nil
	}
	// Use Marshal/Unmarshal for a clean deep copy
	// This handles all fields including extensions correctly
	data, err := p.Marshal()
	if err != nil {
		// Fallback: create a shallow copy with new header
		clone := &rtp.Packet{
			Header: p.Header, // struct copy
		}
		clone.Payload = p.Payload // share payload (read-only)
		return clone
	}
	clone := &rtp.Packet{}
	if err := clone.Unmarshal(data); err != nil {
		// Fallback
		clone := &rtp.Packet{
			Header: p.Header,
		}
		clone.Payload = p.Payload
		return clone
	}
	return clone
}

// pumpBufferToChild sends buffered packets to a child until it catches up or gets a live keyframe
func (r *Receiver) pumpBufferToChild(child *Node, state *childBufferState) {
	defer func() {
		state.pumping = false
	}()

	// Note: Parameter sets (VPS/SPS/PPS) are included in buffer playback
	// We start from a position before the keyframe that includes them

	// Get keyframe position for burst mode calculation
	r.ringMu.RLock()
	keyframePos := r.ringKeyframePos
	r.ringMu.RUnlock()

	// Pump packets from buffer
	packetsent := 0
	keyframeSent := false
	burstPackets := 0 // Count packets sent in initial burst (no delay)

	for {
		select {
		case <-state.stopPump:
			log.Debug().Uint32("childId", child.id).Int("sent", packetsent).Int("burst", burstPackets).Msg("[timeshift] Pump stopped, live keyframe arrived")
			return
		default:
		}

		// Check if we're still in buffering mode
		r.childStateMu.RLock()
		currentState := r.childState[child]
		r.childStateMu.RUnlock()
		if currentState == nil || currentState.mode != "buffering" {
			return
		}

		// Get next packet from buffer and clone it
		// Clone is necessary because consumers modify packet fields (SequenceNumber)
		r.ringMu.RLock()
		readPos := state.readPos
		head := r.ringHead
		packet := clonePacket(r.ringBuffer[readPos])
		r.ringMu.RUnlock()

		if packet == nil {
			// Buffer slot is empty (shouldn't happen normally)
			state.readPos = (readPos + 1) % len(r.ringBuffer)
			continue
		}

		// Check if this is the keyframe position
		if readPos == keyframePos {
			keyframeSent = true
		}

		// Send cloned packet to child
		child.Input(packet)
		packetsent++

		// Advance read position
		state.readPos = (readPos + 1) % len(r.ringBuffer)

		// Check if we've caught up to live position
		if state.readPos == head {
			log.Info().Uint32("childId", child.id).Int("sent", packetsent).Int("burst", burstPackets).Msg("[timeshift] Caught up, switching to live")
			r.childStateMu.Lock()
			state.mode = "live"
			r.childStateMu.Unlock()
			return
		}

		// BURST MODE: Send VPS/SPS/PPS + keyframe + a few extra frames with NO DELAY
		// This ensures the decoder can start immediately (< 100ms) instead of waiting
		// for paced delivery. After the keyframe + 30 extra packets, start pacing.
		if !keyframeSent || burstPackets < 30 {
			burstPackets++
			// No sleep - send as fast as possible for decoder init
			continue
		}

		// PACED MODE: After burst, pace packets to catch up without overwhelming receiver
		// At 30fps, real-time is ~33ms per frame. Send at ~2ms to catch up ~15x faster
		time.Sleep(2 * time.Millisecond)
	}
}

// cleanupChildState removes state for a disconnected child
func (r *Receiver) cleanupChildState(child *Node) {
	r.childStateMu.Lock()
	defer r.childStateMu.Unlock()

	if state, ok := r.childState[child]; ok {
		if state.stopPump != nil {
			close(state.stopPump)
		}
		delete(r.childState, child)
	}
}

// nalInfo contains classification of a NAL unit
type nalInfo struct {
	isIDR      bool   // True for actual IDR frames (I-frames)
	isParamSet bool   // True for VPS/SPS/PPS parameter sets
	typeName   string // Human-readable type name for logging
}

// classifyNAL determines the type of NAL unit in the packet
func (r *Receiver) classifyNAL(packet *Packet) nalInfo {
	if r.Codec == nil || len(packet.Payload) < 1 {
		return nalInfo{}
	}

	switch r.Codec.Name {
	case CodecH265:
		return classifyH265NAL(packet.Payload)
	case CodecH264:
		return classifyH264NAL(packet.Payload)
	}
	return nalInfo{}
}

// isKeyframe checks if packet contains a keyframe based on codec (for backward compat)
func (r *Receiver) isKeyframe(packet *Packet) bool {
	info := r.classifyNAL(packet)
	return info.isIDR || info.isParamSet
}

// classifyH265NAL classifies H.265/HEVC NAL units
func classifyH265NAL(payload []byte) nalInfo {
	if len(payload) < 2 {
		return nalInfo{}
	}
	// H.265 NAL unit type is in bits 1-6 of first byte
	nalType := (payload[0] >> 1) & 0x3F

	switch nalType {
	case 19: // IDR_W_RADL
		return nalInfo{isIDR: true, typeName: "IDR_W_RADL"}
	case 20: // IDR_N_LP
		return nalInfo{isIDR: true, typeName: "IDR_N_LP"}
	case 21: // CRA_NUT (clean random access)
		return nalInfo{isIDR: true, typeName: "CRA_NUT"}
	case 32: // VPS
		return nalInfo{isParamSet: true, typeName: "VPS"}
	case 33: // SPS
		return nalInfo{isParamSet: true, typeName: "SPS"}
	case 34: // PPS
		return nalInfo{isParamSet: true, typeName: "PPS"}
	case 48: // Aggregation Packet (AP) - may contain multiple NALs including param sets
		// Parse AP to check if it contains param sets or IDR
		// AP structure: PayloadHdr (2 bytes) + DONL? + [NALU size (2 bytes) + NALU data]*
		return classifyH265AP(payload)
	case 49: // Fragmentation Unit (FU) - check inner NAL type
		if len(payload) >= 3 {
			fuType := payload[2] & 0x3F
			startBit := (payload[2] >> 7) & 1
			if startBit == 1 {
				switch fuType {
				case 19:
					return nalInfo{isIDR: true, typeName: "FU-IDR_W_RADL"}
				case 20:
					return nalInfo{isIDR: true, typeName: "FU-IDR_N_LP"}
				case 21:
					return nalInfo{isIDR: true, typeName: "FU-CRA_NUT"}
				}
			}
		}
	}
	return nalInfo{}
}

// classifyH265AP parses H.265 Aggregation Packet to check for param sets or IDR
func classifyH265AP(payload []byte) nalInfo {
	if len(payload) < 4 {
		return nalInfo{}
	}

	// Skip AP header (2 bytes)
	offset := 2
	hasParamSet := false
	hasIDR := false

	// Parse each aggregated NAL unit
	for offset+2 < len(payload) {
		// Get NALU size (2 bytes, big-endian)
		naluSize := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2

		if offset+naluSize > len(payload) || naluSize < 2 {
			break
		}

		// Get NAL type from first byte of this NAL unit
		innerNalType := (payload[offset] >> 1) & 0x3F

		switch innerNalType {
		case 19, 20, 21: // IDR frames
			hasIDR = true
		case 32, 33, 34: // VPS, SPS, PPS
			hasParamSet = true
		}

		offset += naluSize
	}

	// IDR takes precedence (triggers cache reset with param sets)
	if hasIDR {
		return nalInfo{isIDR: true, typeName: "AP-IDR"}
	}
	if hasParamSet {
		return nalInfo{isParamSet: true, typeName: "AP-ParamSet"}
	}
	return nalInfo{}
}

// classifyH264NAL classifies H.264/AVC NAL units
func classifyH264NAL(payload []byte) nalInfo {
	if len(payload) < 1 {
		return nalInfo{}
	}
	// H.264 NAL unit type is in bits 0-4 of first byte
	nalType := payload[0] & 0x1F

	switch nalType {
	case 5: // IDR slice
		return nalInfo{isIDR: true, typeName: "IDR"}
	case 7: // SPS
		return nalInfo{isParamSet: true, typeName: "SPS"}
	case 8: // PPS
		return nalInfo{isParamSet: true, typeName: "PPS"}
	case 24: // STAP-A (Single-time aggregation packet) - may contain SPS+PPS+IDR
		return classifyH264STAPA(payload)
	case 28: // FU-A fragmentation
		if len(payload) >= 2 {
			fuType := payload[1] & 0x1F
			startBit := (payload[1] >> 7) & 1
			if startBit == 1 && fuType == 5 {
				return nalInfo{isIDR: true, typeName: "FU-IDR"}
			}
		}
	}
	return nalInfo{}
}

// classifyH264STAPA parses H.264 STAP-A packet to check for param sets or IDR
func classifyH264STAPA(payload []byte) nalInfo {
	if len(payload) < 3 {
		return nalInfo{}
	}

	// Skip STAP-A header (1 byte)
	offset := 1
	hasParamSet := false
	hasIDR := false

	// Parse each aggregated NAL unit
	for offset+2 < len(payload) {
		// Get NALU size (2 bytes, big-endian)
		naluSize := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2

		if offset+naluSize > len(payload) || naluSize < 1 {
			break
		}

		// Get NAL type from first byte of this NAL unit
		innerNalType := payload[offset] & 0x1F

		switch innerNalType {
		case 5: // IDR
			hasIDR = true
		case 7, 8: // SPS, PPS
			hasParamSet = true
		}

		offset += naluSize
	}

	// IDR takes precedence (triggers cache reset with param sets)
	if hasIDR {
		return nalInfo{isIDR: true, typeName: "STAP-IDR"}
	}
	if hasParamSet {
		return nalInfo{isParamSet: true, typeName: "STAP-ParamSet"}
	}
	return nalInfo{}
}

// GetBufferedPacketsFromKeyframe returns packets from last keyframe to current position
// Used for debugging/inspection - actual playback uses pumpBufferToChild
func (r *Receiver) GetBufferedPacketsFromKeyframe() []*Packet {
	r.ringMu.RLock()
	defer r.ringMu.RUnlock()

	if r.ringKeyframePos < 0 || r.ringSize == 0 {
		return nil
	}

	// Calculate how many packets from keyframe to head
	var packets []*Packet
	pos := r.ringKeyframePos
	for pos != r.ringHead {
		if r.ringBuffer[pos] != nil {
			packets = append(packets, r.ringBuffer[pos])
		}
		pos = (pos + 1) % len(r.ringBuffer)
	}

	return packets
}

// BufferStats returns current ring buffer statistics
func (r *Receiver) BufferStats() (bufferedPackets int, keyframePos int, headPos int) {
	r.ringMu.RLock()
	defer r.ringMu.RUnlock()
	return r.ringSize, r.ringKeyframePos, r.ringHead
}

// BufferStatsDetailed returns detailed buffer statistics
func (r *Receiver) BufferStatsDetailed() (bufferedPackets, keyframePos, headPos, paramPackets, paramBytes int) {
	r.ringMu.RLock()
	bufferedPackets = r.ringSize
	keyframePos = r.ringKeyframePos
	headPos = r.ringHead
	r.ringMu.RUnlock()

	r.paramSetsMu.RLock()
	paramPackets = len(r.paramSets)
	paramBytes = r.paramSetsBytes
	r.paramSetsMu.RUnlock()

	return
}

// GetChildState returns the current playback state for a child (for debugging)
func (r *Receiver) GetChildState(child *Node) (mode string, readPos int) {
	r.childStateMu.RLock()
	defer r.childStateMu.RUnlock()

	if state, ok := r.childState[child]; ok {
		return state.mode, state.readPos
	}
	return "unknown", -1
}

// Deprecated: should be removed
func (r *Receiver) WriteRTP(packet *rtp.Packet) {
	r.Input(packet)
}

// Deprecated: should be removed
func (r *Receiver) Senders() []*Sender {
	if len(r.childs) > 0 {
		return []*Sender{{}}
	} else {
		return nil
	}
}

// Deprecated: should be removed
func (r *Receiver) Replace(target *Receiver) {
	MoveNode(&target.Node, &r.Node)
}

func (r *Receiver) Close() {
	r.Node.Close()
}

type Sender struct {
	Node

	// Deprecated:
	Media *Media `json:"-"`
	// Deprecated:
	Handler HandlerFunc `json:"-"`

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`
	Drops   int `json:"drops,omitempty"`

	buf  chan *Packet
	done chan struct{}
}

func NewSender(media *Media, codec *Codec) *Sender {
	var bufSize uint16

	if GetKind(codec.Name) == KindVideo {
		if codec.IsRTP() {
			// in my tests 40Mbit/s 4K-video can generate up to 1500 items
			// for the h264.RTPDepay => RTPPay queue
			bufSize = 4096
		} else {
			bufSize = 64
		}
	} else {
		bufSize = 128
	}

	buf := make(chan *Packet, bufSize)
	s := &Sender{
		Node:  Node{id: NewID(), Codec: codec},
		Media: media,
		buf:   buf,
	}
	s.Input = func(packet *Packet) {
		s.mu.Lock()
		// unblock write to nil chan - OK, write to closed chan - panic
		select {
		case s.buf <- packet:
			s.Bytes += len(packet.Payload)
			s.Packets++
		default:
			s.Drops++
		}
		s.mu.Unlock()
	}
	s.Output = func(packet *Packet) {
		s.Handler(packet)
	}
	return s
}

// Deprecated: should be removed
func (s *Sender) HandleRTP(parent *Receiver) {
	s.WithParent(parent)
	s.Start()
}

// Deprecated: should be removed
func (s *Sender) Bind(parent *Receiver) {
	s.WithParent(parent)
}

func (s *Sender) WithParent(parent *Receiver) *Sender {
	s.Node.WithParent(&parent.Node)
	return s
}

func (s *Sender) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.buf == nil || s.done != nil {
		return
	}
	s.done = make(chan struct{})

	// pass buf directly so that it's impossible for buf to be nil
	go func(buf chan *Packet) {
		for packet := range buf {
			s.Output(packet)
		}
		close(s.done)
	}(s.buf)
}

func (s *Sender) Wait() {
	if done := s.done; done != nil {
		<-done
	}
}

func (s *Sender) State() string {
	if s.buf == nil {
		return "closed"
	}
	if s.done == nil {
		return "new"
	}
	return "connected"
}

func (s *Sender) Close() {
	// close buffer if exists
	s.mu.Lock()
	if s.buf != nil {
		close(s.buf) // exit from for range loop
		s.buf = nil  // prevent writing to closed chan
	}
	s.mu.Unlock()

	s.Node.Close()
}

func (r *Receiver) MarshalJSON() ([]byte, error) {
	v := struct {
		ID      uint32   `json:"id"`
		Codec   *Codec   `json:"codec"`
		Childs  []uint32 `json:"childs,omitempty"`
		Bytes   int      `json:"bytes,omitempty"`
		Packets int      `json:"packets,omitempty"`
	}{
		ID:      r.Node.id,
		Codec:   r.Node.Codec,
		Bytes:   r.Bytes,
		Packets: r.Packets,
	}
	for _, child := range r.childs {
		v.Childs = append(v.Childs, child.id)
	}
	return json.Marshal(v)
}

func (s *Sender) MarshalJSON() ([]byte, error) {
	v := struct {
		ID      uint32 `json:"id"`
		Codec   *Codec `json:"codec"`
		Parent  uint32 `json:"parent,omitempty"`
		Bytes   int    `json:"bytes,omitempty"`
		Packets int    `json:"packets,omitempty"`
		Drops   int    `json:"drops,omitempty"`
	}{
		ID:      s.Node.id,
		Codec:   s.Node.Codec,
		Bytes:   s.Bytes,
		Packets: s.Packets,
		Drops:   s.Drops,
	}
	if s.parent != nil {
		v.Parent = s.parent.id
	}
	return json.Marshal(v)
}
