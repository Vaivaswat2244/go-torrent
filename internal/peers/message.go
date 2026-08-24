package peers

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// MessageID represents the type of message
type MessageID uint8

const (
	MsgChoke         MessageID = 0
	MsgUnchoke       MessageID = 1
	MsgInterested    MessageID = 2
	MsgNotInterested MessageID = 3
	MsgHave          MessageID = 4
	MsgBitfield      MessageID = 5
	MsgRequest       MessageID = 6
	MsgPiece         MessageID = 7
	MsgCancel        MessageID = 8
	MsgExtended      MessageID = 20
)

// MaxMessageSize caps how large a single peer message may claim to be. The
// length prefix is peer-supplied, so without a ceiling a hostile peer can make
// us allocate up to 4 GiB with one 4-byte header. The largest legitimate
// message is a bitfield (one bit per piece; ~50 KB even for a 100 GB torrent)
// or a piece block (16 KB), so 1 MiB is generous.
const MaxMessageSize = 1 << 20

// Message represents a peer wire protocol message
type Message struct {
	ID      MessageID
	Payload []byte
}

// Serialize converts message to bytes
func (m *Message) Serialize() []byte {
	if m == nil {
		return make([]byte, 4) // Keep-alive message
	}

	length := uint32(len(m.Payload) + 1) // +1 for ID
	buf := make([]byte, 4+length)

	binary.BigEndian.PutUint32(buf[0:4], length)
	buf[4] = byte(m.ID)
	copy(buf[5:], m.Payload)

	return buf
}

// Read reads a message from a connection
func ReadMessage(r io.Reader) (*Message, error) {
	// Read message length (4 bytes)
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(r, lengthBuf)
	if err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(lengthBuf)

	// Keep-alive message (length = 0)
	if length == 0 {
		return nil, nil
	}

	if length > MaxMessageSize {
		return nil, fmt.Errorf("peer announced an oversized message: %d bytes (max %d)", length, MaxMessageSize)
	}

	// Read message ID + payload
	messageBuf := make([]byte, length)
	_, err = io.ReadFull(r, messageBuf)
	if err != nil {
		return nil, err
	}

	msg := &Message{
		ID:      MessageID(messageBuf[0]),
		Payload: messageBuf[1:],
	}

	return msg, nil
}

// FormatRequest creates a Request message
func FormatRequest(index, begin, length int) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))

	return &Message{
		ID:      MsgRequest,
		Payload: payload,
	}
}

// FormatInterested creates an Interested message
func FormatInterested() *Message {
	return &Message{ID: MsgInterested}
}

// FormatNotInterested creates a NotInterested message
func FormatNotInterested() *Message {
	return &Message{ID: MsgNotInterested}
}

// FormatHave creates a Have message
func FormatHave(index int) *Message {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(index))
	return &Message{
		ID:      MsgHave,
		Payload: payload,
	}
}

// FormatChoke creates a Choke message
func FormatChoke() *Message {
	return &Message{ID: MsgChoke}
}

// FormatUnchoke creates an Unchoke message
func FormatUnchoke() *Message {
	return &Message{ID: MsgUnchoke}
}

// FormatCancel creates a Cancel message, withdrawing an outstanding request.
func FormatCancel(index, begin, length int) *Message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))

	return &Message{ID: MsgCancel, Payload: payload}
}

// FormatPiece creates a Piece message carrying one block of data.
func FormatPiece(index, begin int, data []byte) *Message {
	payload := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	copy(payload[8:], data)

	return &Message{ID: MsgPiece, Payload: payload}
}

// BlockRequest is a peer asking us for one block.
type BlockRequest struct {
	Index  int
	Begin  int
	Length int
}

// ParseRequest parses a Request or Cancel message.
//
// This is peer-controlled input that decides what we read off disk, so the
// caller must still validate the values against the torrent's geometry.
func ParseRequest(msg *Message) (BlockRequest, error) {
	var req BlockRequest

	if msg.ID != MsgRequest && msg.ID != MsgCancel {
		return req, fmt.Errorf("expected Request or Cancel, got ID %d", msg.ID)
	}
	if len(msg.Payload) != 12 {
		return req, fmt.Errorf("expected payload length 12, got %d", len(msg.Payload))
	}

	index := binary.BigEndian.Uint32(msg.Payload[0:4])
	begin := binary.BigEndian.Uint32(msg.Payload[4:8])
	length := binary.BigEndian.Uint32(msg.Payload[8:12])

	// Guard the conversion to int before anyone does arithmetic on these.
	if index > math.MaxInt32 || begin > math.MaxInt32 || length > math.MaxInt32 {
		return req, fmt.Errorf("request field out of range: index=%d begin=%d length=%d", index, begin, length)
	}

	req.Index = int(index)
	req.Begin = int(begin)
	req.Length = int(length)
	return req, nil
}

// ParsePiece parses a Piece message
func ParsePiece(index int, buf []byte, msg *Message) (int, error) {
	if msg.ID != MsgPiece {
		return 0, fmt.Errorf("expected Piece (ID %d), got ID %d", MsgPiece, msg.ID)
	}
	if len(msg.Payload) < 8 {
		return 0, fmt.Errorf("payload too short: %d bytes", len(msg.Payload))
	}

	parsedIndex := int(binary.BigEndian.Uint32(msg.Payload[0:4]))
	if parsedIndex != index {
		return 0, fmt.Errorf("expected index %d, got %d", index, parsedIndex)
	}

	begin := int(binary.BigEndian.Uint32(msg.Payload[4:8]))
	if begin >= len(buf) {
		return 0, fmt.Errorf("begin offset too high: %d >= %d", begin, len(buf))
	}

	data := msg.Payload[8:]
	if begin+len(data) > len(buf) {
		return 0, fmt.Errorf("data too long for offset %d", begin)
	}

	copy(buf[begin:], data)
	return len(data), nil
}

// ParseHave parses a Have message
func ParseHave(msg *Message) (int, error) {
	if msg.ID != MsgHave {
		return 0, fmt.Errorf("expected Have (ID %d), got ID %d", MsgHave, msg.ID)
	}
	if len(msg.Payload) != 4 {
		return 0, fmt.Errorf("expected payload length 4, got %d", len(msg.Payload))
	}

	index := int(binary.BigEndian.Uint32(msg.Payload))
	return index, nil
}

// String returns a readable representation of the message
func (m *Message) String() string {
	if m == nil {
		return "KeepAlive"
	}

	switch m.ID {
	case MsgChoke:
		return "Choke"
	case MsgUnchoke:
		return "Unchoke"
	case MsgInterested:
		return "Interested"
	case MsgNotInterested:
		return "NotInterested"
	case MsgHave:
		return fmt.Sprintf("Have [%d bytes]", len(m.Payload))
	case MsgBitfield:
		return fmt.Sprintf("Bitfield [%d bytes]", len(m.Payload))
	case MsgRequest:
		return fmt.Sprintf("Request [%d bytes]", len(m.Payload))
	case MsgPiece:
		return fmt.Sprintf("Piece [%d bytes]", len(m.Payload))
	case MsgCancel:
		return "Cancel"
	default:
		return fmt.Sprintf("Unknown [ID=%d]", m.ID)
	}
}
