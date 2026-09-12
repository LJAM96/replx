// Package requestid generates UUIDv7 request identifiers (stdlib only).
//
// Layout per RFC 9562: 48-bit big-endian millisecond timestamp, 4-bit
// version (0111), 62 random bits with the 2-bit variant (10) set.
package requestid

import (
	"crypto/rand"
	"fmt"
	"time"
)

// New returns a random UUIDv7 string.
func New() string {
	return NewAt(time.Now())
}

// NewAt returns a UUIDv7 for a fixed time (deterministic prefix, random
// suffix). Used by tests; production uses New.
func NewAt(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic("requestid: crypto/rand: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	)
}
