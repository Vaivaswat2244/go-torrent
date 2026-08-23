package p2p

import (
	"sync"
	"testing"
)

// TestSafeBitfieldConcurrent reproduces the access pattern that used to be a
// data race: every peer goroutine reads our bitfield to send it in the
// handshake while the download loop sets bits on it as pieces complete.
//
// Run with -race; on the unsynchronised []byte this fails.
func TestSafeBitfieldConcurrent(t *testing.T) {
	const numPieces = 512

	sbf := NewSafeBitfield(numPieces)
	var wg sync.WaitGroup

	// Writers: the collector marking pieces done.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := offset; i < numPieces; i += 4 {
				sbf.Set(i)
			}
		}(w)
	}

	// Readers: peer workers snapshotting to send a bitfield message.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				snap := sbf.Snapshot()
				if len(snap) != (numPieces+7)/8 {
					t.Errorf("snapshot length %d", len(snap))
					return
				}
				_ = sbf.Has(i % numPieces)
				_ = sbf.Count()
			}
		}()
	}

	wg.Wait()

	if got := sbf.Count(); got != numPieces {
		t.Errorf("Count() = %d, want %d", got, numPieces)
	}
	for i := 0; i < numPieces; i++ {
		if !sbf.Has(i) {
			t.Fatalf("piece %d not set", i)
		}
	}
}

// A snapshot must not alias the live bitfield, or handing it to a peer
// goroutine would reintroduce the race.
func TestSnapshotIsACopy(t *testing.T) {
	sbf := NewSafeBitfield(16)
	snap := sbf.Snapshot()

	sbf.Set(0)
	if snap.HasPiece(0) {
		t.Error("snapshot changed after a later Set: it aliases the live bitfield")
	}

	snap.SetPiece(5)
	if sbf.Has(5) {
		t.Error("writing to a snapshot mutated the live bitfield")
	}
}
