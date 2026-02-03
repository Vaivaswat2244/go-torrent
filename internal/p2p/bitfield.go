package p2p

// Bitfield represents which pieces a peer has
type Bitfield []byte

// HasPiece checks if a peer has a particular piece
func (bf Bitfield) HasPiece(index int) bool {
	byteIndex := index / 8
	offset := index % 8

	if byteIndex < 0 || byteIndex >= len(bf) {
		return false
	}

	return bf[byteIndex]>>(7-offset)&1 != 0
}

// SetPiece marks that we have a piece
func (bf Bitfield) SetPiece(index int) {
	byteIndex := index / 8
	offset := index % 8

	if byteIndex < 0 || byteIndex >= len(bf) {
		return
	}

	bf[byteIndex] |= 1 << (7 - offset)
}
