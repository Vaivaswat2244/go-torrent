package peers

import (
	"encoding/binary"
	"fmt"
	"io"
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
)

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
