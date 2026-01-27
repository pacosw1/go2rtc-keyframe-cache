package core

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/pion/rtp"
)

var ErrCantGetTrack = errors.New("can't get track")

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
	Enabled:    true,
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

type Receiver struct {
	Node

	// Deprecated: should be removed
	Media *Media `json:"-"`
	// Deprecated: should be removed
	ID byte `json:"-"` // Channel for RTSP, PayloadType for MPEG-TS

	Bytes   int `json:"bytes,omitempty"`
	Packets int `json:"packets,omitempty"`

	// Keyframe cache for instant playback
	kfCache       []*Packet
	kfCacheBytes  int
	kfCacheMu     sync.RWMutex
	kfCacheConfig KeyframeCacheConfig
}

func NewReceiver(media *Media, codec *Codec) *Receiver {
	return NewReceiverWithCache(media, codec, DefaultKeyframeCacheConfig)
}

// NewReceiverWithCache creates a Receiver with configurable keyframe caching
func NewReceiverWithCache(media *Media, codec *Codec, cacheConfig KeyframeCacheConfig) *Receiver {
	r := &Receiver{
		Node:          Node{id: NewID(), Codec: codec},
		Media:         media,
		kfCacheConfig: cacheConfig,
	}

	// Only enable caching for video codecs that support keyframes
	isVideoWithKeyframes := codec != nil && (codec.Name == CodecH264 || codec.Name == CodecH265)
	cacheEnabled := cacheConfig.Enabled && isVideoWithKeyframes

	r.Input = func(packet *Packet) {
		r.Bytes += len(packet.Payload)
		r.Packets++

		// Cache packets if enabled for this receiver
		if cacheEnabled {
			r.cachePacket(packet)
		}

		for _, child := range r.childs {
			child.Input(packet)
		}
	}

	// Set up callback to send cached keyframes to new consumers
	if cacheEnabled {
		r.OnChildAdded = func(child *Node) {
			// Send cached keyframe packets to the new child for instant playback
			count := r.SendCachedKeyframe(child)
			if count > 0 {
				// Log at debug level - uncomment for debugging
				// log.Debug().Msgf("[keyframe-cache] Sent %d cached packets to new consumer", count)
			}
		}
	}

	return r
}

// cachePacket adds a packet to the keyframe cache
// If it's a keyframe, it resets the cache; otherwise appends to existing cache
func (r *Receiver) cachePacket(packet *Packet) {
	r.kfCacheMu.Lock()
	defer r.kfCacheMu.Unlock()

	isKeyframe := r.isKeyframe(packet)

	if isKeyframe {
		// New keyframe - reset cache
		r.kfCache = []*Packet{packet}
		r.kfCacheBytes = len(packet.Payload)
	} else if len(r.kfCache) > 0 {
		// Add to existing cache if we have a keyframe and within limits
		if len(r.kfCache) < r.kfCacheConfig.MaxPackets &&
			r.kfCacheBytes+len(packet.Payload) < r.kfCacheConfig.MaxBytes {
			r.kfCache = append(r.kfCache, packet)
			r.kfCacheBytes += len(packet.Payload)
		}
	}
}

// isKeyframe checks if packet contains a keyframe based on codec
func (r *Receiver) isKeyframe(packet *Packet) bool {
	if r.Codec == nil || len(packet.Payload) < 1 {
		return false
	}

	switch r.Codec.Name {
	case CodecH265:
		return isH265Keyframe(packet.Payload)
	case CodecH264:
		return isH264Keyframe(packet.Payload)
	}
	return false
}

// isH265Keyframe checks for H.265/HEVC keyframe (IDR NAL units: 19, 20, 21)
func isH265Keyframe(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	// H.265 NAL unit type is in bits 1-6 of second byte
	// For RTP, first byte is the NAL unit header
	nalType := (payload[0] >> 1) & 0x3F

	// Check for IDR frames (types 19, 20, 21) or VPS/SPS/PPS (32, 33, 34)
	switch nalType {
	case 19, 20, 21: // IDR_W_RADL, IDR_N_LP, CRA_NUT
		return true
	case 32, 33, 34: // VPS, SPS, PPS - start of new sequence
		return true
	case 49: // Fragmentation Unit (FU) - check inner NAL type
		if len(payload) >= 3 {
			fuType := payload[2] & 0x3F
			startBit := (payload[2] >> 7) & 1
			if startBit == 1 && (fuType == 19 || fuType == 20 || fuType == 21) {
				return true
			}
		}
	}
	return false
}

// isH264Keyframe checks for H.264/AVC keyframe (IDR NAL unit type 5)
func isH264Keyframe(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	// H.264 NAL unit type is in bits 0-4 of first byte
	nalType := payload[0] & 0x1F

	switch nalType {
	case 5: // IDR slice
		return true
	case 7, 8: // SPS, PPS - start of new sequence
		return true
	case 28: // FU-A fragmentation
		if len(payload) >= 2 {
			fuType := payload[1] & 0x1F
			startBit := (payload[1] >> 7) & 1
			if startBit == 1 && fuType == 5 {
				return true
			}
		}
	}
	return false
}

// GetCachedKeyframe returns a copy of the cached keyframe packets
// Call this when adding a new consumer to send them the cached data
func (r *Receiver) GetCachedKeyframe() []*Packet {
	r.kfCacheMu.RLock()
	defer r.kfCacheMu.RUnlock()

	if len(r.kfCache) == 0 {
		return nil
	}

	// Return a copy to avoid race conditions
	cached := make([]*Packet, len(r.kfCache))
	copy(cached, r.kfCache)
	return cached
}

// SendCachedKeyframe sends cached keyframe packets to a new child node
// This enables instant playback without waiting for the next keyframe
func (r *Receiver) SendCachedKeyframe(child *Node) int {
	cached := r.GetCachedKeyframe()
	if cached == nil {
		return 0
	}

	for _, packet := range cached {
		child.Input(packet)
	}
	return len(cached)
}

// CacheStats returns current cache statistics
func (r *Receiver) CacheStats() (packets int, bytes int) {
	r.kfCacheMu.RLock()
	defer r.kfCacheMu.RUnlock()
	return len(r.kfCache), r.kfCacheBytes
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
