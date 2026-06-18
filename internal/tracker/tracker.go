package tracker

import (
	"sync"
)

// FlowKey uniquely identifies a traffic flow.
type FlowKey struct {
	IP        string
	Direction string // "upload" / "download"
	Protocol  string // "tcp" / "udp" / "icmp" / "other" / "all"
}

// FlowDelta represents the delta since last poll.
type FlowDelta struct {
	Key     FlowKey
	Bytes   uint64
	Packets uint64
}

// Tracker maintains a shadow map of previous counters and computes deltas.
type Tracker struct {
	mu       sync.Mutex
	shadow   map[FlowKey]shadowEntry
}

type shadowEntry struct {
	Bytes   uint64
	Packets uint64
}

// New creates a new tracker.
func New() *Tracker {
	return &Tracker{
		shadow: make(map[FlowKey]shadowEntry),
	}
}

// Update computes deltas for a batch of current counters and returns the deltas.
// It updates the shadow map atomically.
func (t *Tracker) Update(elements []FlowElement) []FlowDelta {
	t.mu.Lock()
	defer t.mu.Unlock()

	deltas := make([]FlowDelta, 0, len(elements))

	for _, elem := range elements {
		key := FlowKey{
			IP:        elem.IP,
			Direction: elem.Direction,
			Protocol:  elem.Protocol,
		}

		prev, exists := t.shadow[key]
		if !exists {
			// New flow: delta = current (starting from 0)
			t.shadow[key] = shadowEntry{
				Bytes:   elem.Bytes,
				Packets: elem.Packets,
			}
			deltas = append(deltas, FlowDelta{
				Key:     key,
				Bytes:   elem.Bytes,
				Packets: elem.Packets,
			})
		} else {
			// Existing flow: compute delta
			deltaBytes := elem.Bytes - prev.Bytes
			deltaPackets := elem.Packets - prev.Packets

			// Handle counter reset (nftables set was cleared or overflow)
			if elem.Bytes < prev.Bytes {
				deltaBytes = elem.Bytes
			}
			if elem.Packets < prev.Packets {
				deltaPackets = elem.Packets
			}

			t.shadow[key] = shadowEntry{
				Bytes:   elem.Bytes,
				Packets: elem.Packets,
			}

			if deltaBytes > 0 || deltaPackets > 0 {
				deltas = append(deltas, FlowDelta{
					Key:     key,
					Bytes:   deltaBytes,
					Packets: deltaPackets,
				})
			}
		}
	}

	return deltas
}

// CleanStale removes shadow entries for IPs that no longer appear in the current poll.
// Returns the list of removed FlowKeys so the caller can clean up Prometheus counters.
func (t *Tracker) CleanStale(currentKeys map[FlowKey]bool) []FlowKey {
	t.mu.Lock()
	defer t.mu.Unlock()

	var removed []FlowKey
	for k := range t.shadow {
		if !currentKeys[k] {
			delete(t.shadow, k)
			removed = append(removed, k)
		}
	}
	return removed
}

// Size returns the number of tracked flows.
func (t *Tracker) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.shadow)
}

// SizeByDirection returns the number of tracked flows split by direction.
func (t *Tracker) SizeByDirection() (inbound, outbound int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.shadow {
		if k.Direction == "download" || k.Direction == "inbound" {
			inbound++
		} else {
			outbound++
		}
	}
	return
}

// FlowElement is an input element from nftables poll.
type FlowElement struct {
	IP        string
	Direction string
	Protocol  string
	Bytes     uint64
	Packets   uint64
}
