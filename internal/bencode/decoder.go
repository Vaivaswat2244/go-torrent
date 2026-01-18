package bencode

import (
	"fmt"
	"strconv"
)

// Value represents any bencode value
type Value interface{}

// Decode parses bencoded data and returns the decoded value
func Decode(data []byte) (Value, error) {
	pos := 0
	value, newPos, err := decodeValue(data, pos)
	if err != nil {
		return nil, err
	}
	if newPos != len(data) {
		return nil, fmt.Errorf("unexpected data after bencode value")
	}
	return value, nil
}

// decodeValue decodes a single bencode value starting at pos
func decodeValue(data []byte, pos int) (Value, int, error) {
	if pos >= len(data) {
		return nil, pos, fmt.Errorf("unexpected end of data")
	}

	switch data[pos] {
	case 'i':
		return decodeInteger(data, pos)
	case 'l':
		return decodeList(data, pos)
	case 'd':
		return decodeDict(data, pos)
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return decodeString(data, pos)
	default:
		return nil, pos, fmt.Errorf("invalid bencode value at position %d", pos)
	}
}

// decodeString decodes a bencoded string: <length>:<data>
func decodeString(data []byte, pos int) (string, int, error) {
	// Find the colon
	colonPos := pos
	for colonPos < len(data) && data[colonPos] != ':' {
		colonPos++
	}
	if colonPos >= len(data) {
		return "", pos, fmt.Errorf("invalid string: colon not found")
	}

	// Parse length
	lengthStr := string(data[pos:colonPos])
	length, err := strconv.Atoi(lengthStr)
	if err != nil {
		return "", pos, fmt.Errorf("invalid string length: %w", err)
	}

	// Extract string data
	start := colonPos + 1
	end := start + length
	if end > len(data) {
		return "", pos, fmt.Errorf("string data exceeds available data")
	}

	return string(data[start:end]), end, nil
}

// decodeInteger decodes a bencoded integer: i<number>e
func decodeInteger(data []byte, pos int) (int64, int, error) {
	pos++ // Skip 'i'

	// Find the 'e'
	endPos := pos
	for endPos < len(data) && data[endPos] != 'e' {
		endPos++
	}
	if endPos >= len(data) {
		return 0, pos, fmt.Errorf("invalid integer: 'e' not found")
	}

	// Parse integer
	numStr := string(data[pos:endPos])
	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, pos, fmt.Errorf("invalid integer value: %w", err)
	}

	return num, endPos + 1, nil
}

// decodeList decodes a bencoded list: l<values>e
func decodeList(data []byte, pos int) ([]Value, int, error) {
	pos++ // Skip 'l'

	list := []Value{}
	for pos < len(data) && data[pos] != 'e' {
		value, newPos, err := decodeValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		list = append(list, value)
		pos = newPos
	}

	if pos >= len(data) {
		return nil, pos, fmt.Errorf("invalid list: 'e' not found")
	}

	return list, pos + 1, nil // Skip 'e'
}

// decodeDict decodes a bencoded dictionary: d<key><value>...e
func decodeDict(data []byte, pos int) (map[string]Value, int, error) {
	pos++ // Skip 'd'

	dict := make(map[string]Value)
	for pos < len(data) && data[pos] != 'e' {
		// Decode key (must be a string)
		key, newPos, err := decodeString(data, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("invalid dict key: %w", err)
		}
		pos = newPos

		// Decode value
		value, newPos, err := decodeValue(data, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("invalid dict value: %w", err)
		}
		pos = newPos

		dict[key] = value
	}

	if pos >= len(data) {
		return nil, pos, fmt.Errorf("invalid dictionary: 'e' not found")
	}

	return dict, pos + 1, nil // Skip 'e'
}

// Helper functions to extract typed values from maps

// GetString safely extracts a string from a bencode dictionary
func GetString(dict map[string]Value, key string) (string, error) {
	val, ok := dict[key]
	if !ok {
		return "", fmt.Errorf("key %s not found", key)
	}
	str, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("key %s is not a string", key)
	}
	return str, nil
}

// GetInt safely extracts an integer from a bencode dictionary
func GetInt(dict map[string]Value, key string) (int64, error) {
	val, ok := dict[key]
	if !ok {
		return 0, fmt.Errorf("key %s not found", key)
	}
	num, ok := val.(int64)
	if !ok {
		return 0, fmt.Errorf("key %s is not an integer", key)
	}
	return num, nil
}

// GetDict safely extracts a dictionary from a bencode dictionary
func GetDict(dict map[string]Value, key string) (map[string]Value, error) {
	val, ok := dict[key]
	if !ok {
		return nil, fmt.Errorf("key %s not found", key)
	}
	d, ok := val.(map[string]Value)
	if !ok {
		return nil, fmt.Errorf("key %s is not a dictionary", key)
	}
	return d, nil
}
