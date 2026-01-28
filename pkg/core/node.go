package core

import (
	"sync"

	"github.com/pion/rtp"
)

//type Packet struct {
//	Payload     []byte
//	Timestamp   uint32 // PTS if DTS == 0 else DTS
//	Composition uint32 // CTS = PTS-DTS (for support B-frames)
//	Sequence    uint16
//}

type Packet = rtp.Packet

// HandlerFunc - process input packets (just like http.HandlerFunc)
type HandlerFunc func(packet *Packet)

// Filter - a decorator for any HandlerFunc
type Filter func(handler HandlerFunc) HandlerFunc

// Node - Receiver or Sender or Filter (transform)
type Node struct {
	Codec  *Codec
	Input  HandlerFunc
	Output HandlerFunc

	// OnChildAdded is called when a new child is added to this node
	// Used by Receiver to send cached keyframes to new consumers
	OnChildAdded func(child *Node)

	// OnChildRemoved is called when a child is removed from this node
	// Used by Receiver to cleanup per-child state
	OnChildRemoved func(child *Node)

	// SkipTimeshift - if true, this node bypasses time-shift buffering
	// and receives live packets immediately. Use for recorders that don't
	// need instant playback from cached keyframes.
	SkipTimeshift bool

	// ReadySignal - if set, the timeshift pump will wait for this channel
	// to be closed before sending buffered packets. Use for WebRTC consumers
	// that need the connection to be established before receiving packets.
	ReadySignal chan struct{}

	id     uint32
	childs []*Node
	parent *Node

	mu sync.Mutex
}

func (n *Node) WithParent(parent *Node) *Node {
	parent.AppendChild(n)
	return n
}

func (n *Node) AppendChild(child *Node) {
	n.mu.Lock()
	n.childs = append(n.childs, child)
	n.mu.Unlock()

	child.parent = n

	// Notify parent that a child was added (used for keyframe cache)
	if n.OnChildAdded != nil {
		n.OnChildAdded(child)
	}
}

func (n *Node) RemoveChild(child *Node) {
	n.mu.Lock()
	for i, ch := range n.childs {
		if ch == child {
			n.childs = append(n.childs[:i], n.childs[i+1:]...)
			break
		}
	}
	n.mu.Unlock()

	// Notify parent that a child was removed (used for cleanup)
	if n.OnChildRemoved != nil {
		n.OnChildRemoved(child)
	}
}

func (n *Node) Close() {
	if parent := n.parent; parent != nil {
		parent.RemoveChild(n)

		if len(parent.childs) == 0 {
			parent.Close()
		}
	} else {
		for _, childs := range n.childs {
			childs.Close()
		}
	}
}

func MoveNode(dst, src *Node) {
	src.mu.Lock()
	childs := src.childs
	src.childs = nil
	src.mu.Unlock()

	dst.mu.Lock()
	dst.childs = childs
	dst.mu.Unlock()

	for _, child := range childs {
		child.parent = dst
	}
}
