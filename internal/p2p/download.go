package p2p

import (
	"crypto/sha1"
	"fmt"
)

// MaxBlockSize is the standard block size peers request. It is also the largest
// block we are willing to serve.
const MaxBlockSize = 16384

// MaxBacklog is how many block requests we keep in flight per peer. Five is the
// conventional figure and is enough to keep the pipe full.
const MaxBacklog = 5

// PieceWork represents a work item: download this piece
type PieceWork struct {
	Index  int
	Hash   [20]byte
	Length int
}

// PieceResult is the result of downloading a piece
type PieceResult struct {
	Index int
	Buf   []byte
}

// CheckIntegrity verifies the downloaded piece matches the hash
func (pw *PieceWork) CheckIntegrity(buf []byte) error {
	hash := sha1.Sum(buf)
	if hash != pw.Hash {
		return fmt.Errorf("piece %d failed integrity check", pw.Index)
	}
	return nil
}
