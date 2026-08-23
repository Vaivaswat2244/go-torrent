package p2p

import (
	"math"
	"sync"
)

// Bitfield represents which pieces a peer has
type Bitfield []byte

// NewBitfield allocates a bitfield sized for numPieces.
func NewBitfield(numPieces int) Bitfield {
	return make(Bitfield, int(math.Ceil(float64(numPieces)/8.0)))
}

// HasPiece checks if a peer has a particular piece
func (bf Bitfield) HasPiece(index int) bool {
	byteIndex := index / 8
	offset := index % 8

	if index < 0 || byteIndex >= len(bf) {
		return false
	}

	return bf[byteIndex]>>(7-offset)&1 != 0
}

// SetPiece marks that we have a piece
func (bf Bitfield) SetPiece(index int) {
	byteIndex := index / 8
	offset := index % 8

	if index < 0 || byteIndex >= len(bf) {
		return
	}

	bf[byteIndex] |= 1 << (7 - offset)
}

// SafeBitfield is a Bitfield guarded by a mutex.
//
// Our own bitfield is read by every peer goroutine (to send in the handshake)
// while the download loop sets bits on it as pieces complete. Sharing the plain
// []byte across those goroutines was an unsynchronised read/write data race.
type SafeBitfield struct {
	mu sync.RWMutex
	bf Bitfield
}

func NewSafeBitfield(numPieces int) *SafeBitfield {
	return &SafeBitfield{bf: NewBitfield(numPieces)}
}

func (s *SafeBitfield) Has(index int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bf.HasPiece(index)
}

func (s *SafeBitfield) Set(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bf.SetPiece(index)
}

// Snapshot returns a copy safe to hand to another goroutine.
func (s *SafeBitfield) Snapshot() Bitfield {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(Bitfield, len(s.bf))
	copy(out, s.bf)
	return out
}

// Count returns how many pieces are set.
func (s *SafeBitfield) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := 0
	for _, b := range s.bf {
		for ; b != 0; b &= b - 1 {
			n++
		}
	}
	return n
}
