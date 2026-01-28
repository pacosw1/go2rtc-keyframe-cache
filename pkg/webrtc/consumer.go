package webrtc

import (
	"errors"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h265"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
	"github.com/rs/zerolog/log"
)

// sequenceRewriter tracks and rewrites RTP sequence numbers per-sender
// This ensures browsers receive contiguous sequence numbers even when
// cached keyframe packets (with old sequence numbers) are injected
type sequenceRewriter struct {
	mu            sync.Mutex
	initialized   bool
	nextSeqNum    uint16 // Next sequence number to use
	lastOrigSeq   uint16 // Last original sequence number seen
	seqOffset     int32  // Offset to apply (can wrap around)
}

func (c *Conn) GetMedias() []*core.Media {
	return WithResampling(c.Medias)
}

func (c *Conn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	core.Assert(media.Direction == core.DirectionSendonly)

	for _, sender := range c.Senders {
		if sender.Codec == codec {
			sender.Bind(track)
			return nil
		}
	}

	switch c.Mode {
	case core.ModePassiveConsumer: // video/audio for browser
	case core.ModeActiveProducer: // go2rtc as WebRTC client (backchannel)
	case core.ModePassiveProducer: // WebRTC/WHIP
	default:
		panic(core.Caller())
	}

	localTrack := c.GetSenderTrack(media.ID)
	if localTrack == nil {
		return errors.New("webrtc: can't get track")
	}

	payloadType := codec.PayloadType

	// Debug: Log the payload type being used for this codec
	log.Info().
		Str("codec", codec.Name).
		Uint8("payloadType", payloadType).
		Str("mediaID", media.ID).
		Msg("[webrtc-consumer] Setting up track with payload type")

	sender := core.NewSender(media, codec)

	// Create sequence rewriter to handle cached keyframe packets
	// Without this, cached packets have old sequence numbers (e.g., 1000)
	// while live packets have current numbers (e.g., 5000), causing
	// browsers to discard cached packets as "lost"
	seqRewriter := &sequenceRewriter{}

	sender.Handler = func(packet *rtp.Packet) {
		c.Send += packet.MarshalSize()

		// Rewrite sequence number to ensure contiguous delivery
		rewrittenSeq := seqRewriter.rewrite(packet.SequenceNumber)

		// Clone packet to avoid modifying shared data
		rewrittenPacket := &rtp.Packet{
			Header: rtp.Header{
				Version:        packet.Version,
				Padding:        packet.Padding,
				Extension:      packet.Extension,
				Marker:         packet.Marker,
				PayloadType:    packet.PayloadType,
				SequenceNumber: rewrittenSeq,
				Timestamp:      packet.Timestamp,
				SSRC:           packet.SSRC,
				CSRC:           packet.CSRC,
			},
			Payload: packet.Payload,
		}

		// Copy extensions if present
		if packet.Extension {
			rewrittenPacket.ExtensionProfile = packet.ExtensionProfile
			rewrittenPacket.Extensions = packet.Extensions
		}

		//important to send with remote PayloadType
		_ = localTrack.WriteRTP(payloadType, rewrittenPacket)
	}

	switch track.Codec.Name {
	case core.CodecH264:
		sender.Handler = h264.RTPPay(1200, sender.Handler)
		if track.Codec.IsRTP() {
			sender.Handler = h264.RTPDepay(track.Codec, sender.Handler)
		} else {
			sender.Handler = h264.RepairAVCC(track.Codec, sender.Handler)
		}

	case core.CodecH265:
		sender.Handler = h265.RTPPay(1200, sender.Handler)
		if track.Codec.IsRTP() {
			sender.Handler = h265.RTPDepay(track.Codec, sender.Handler)
		} else {
			sender.Handler = h265.RepairAVCC(track.Codec, sender.Handler)
		}

	case core.CodecPCMA, core.CodecPCMU, core.CodecPCM, core.CodecPCML:
		// Fix audio quality https://github.com/AlexxIT/WebRTC/issues/500
		// should be before ResampleToG711, because it will be called last
		sender.Handler = pcm.RepackG711(false, sender.Handler)

		if codec.ClockRate == 0 {
			if codec.Name == core.CodecPCM || codec.Name == core.CodecPCML {
				codec.Name = core.CodecPCMA
			}
			codec.ClockRate = 8000
			sender.Handler = pcm.TranscodeHandler(codec, track.Codec, sender.Handler)
		}
	}

	// TODO: rewrite this dirty logic
	// maybe not best solution, but ActiveProducer connected before AddTrack
	if c.Mode != core.ModeActiveProducer {
		sender.Bind(track)
	} else {
		sender.HandleRTP(track)
	}

	c.Senders = append(c.Senders, sender)
	return nil
}

// rewrite converts an original RTP sequence number to a contiguous sequence
// This handles the case where cached keyframe packets have old sequence numbers
// while live packets have much higher numbers
func (r *sequenceRewriter) rewrite(origSeq uint16) uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.initialized {
		// First packet - start our own sequence from this point
		r.initialized = true
		r.nextSeqNum = origSeq
		r.lastOrigSeq = origSeq
		r.seqOffset = 0
		log.Trace().
			Uint16("origSeq", origSeq).
			Uint16("newSeq", r.nextSeqNum).
			Msg("[webrtc-seq] First packet")
		result := r.nextSeqNum
		r.nextSeqNum++
		return result
	}

	// Calculate the gap between this packet and the last one
	// Use int32 to handle wraparound correctly
	gap := int32(origSeq) - int32(r.lastOrigSeq)

	// Handle wraparound (sequence numbers are 16-bit)
	if gap > 32768 {
		gap -= 65536
	} else if gap < -32768 {
		gap += 65536
	}

	// Detect large gaps (cached packets being injected)
	// If gap is significantly negative (old cached packets) or very large positive,
	// we need to adjust to keep sequence numbers contiguous
	if gap < -100 || gap > 100 {
		// Large gap detected - likely cached keyframe injection
		// Don't jump sequence numbers, just continue from where we were
		log.Trace().
			Uint16("origSeq", origSeq).
			Uint16("lastOrigSeq", r.lastOrigSeq).
			Int32("gap", gap).
			Uint16("newSeq", r.nextSeqNum).
			Msg("[webrtc-seq] Large gap")
	}

	r.lastOrigSeq = origSeq
	result := r.nextSeqNum
	r.nextSeqNum++
	return result
}
