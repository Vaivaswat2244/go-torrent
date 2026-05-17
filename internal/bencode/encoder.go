package bencode

import (
	"fmt"
	"sort"
	"strconv"
)

// Encode serializes standard Go types and bencode.Value into bencoded byte slices.
func Encode(val interface{}) ([]byte, error) {
	switch v := val.(type) {
	case string:
		return encodeString(v), nil
	case []byte:
		// Often byte arrays (like InfoHashes or NodeIDs) are treated as strings in bencode
		return encodeString(string(v)), nil
	case int:
		return encodeInt(int64(v)), nil
	case int64:
		return encodeInt(v), nil
	case []Value:
		return encodeList(v)
	case []interface{}: // Generic slice support
		return encodeGenericList(v)
	case map[string]Value:
		return encodeDict(v)
	case map[string]interface{}: // Generic map support (used by our DHT crawler)
		return encodeGenericDict(v)
	default:
		return nil, fmt.Errorf("unsupported type for bencode encoding: %T", v)
	}
}

// encodeString encodes a string as <length>:<string>
func encodeString(s string) []byte {
	return []byte(strconv.Itoa(len(s)) + ":" + s)
}

// encodeInt encodes an integer as i<num>e
func encodeInt(i int64) []byte {
	return []byte("i" + strconv.FormatInt(i, 10) + "e")
}

// encodeList encodes a slice of bencode.Values
func encodeList(list []Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'l')

	for _, item := range list {
		itemBytes, err := Encode(item)
		if err != nil {
			return nil, err
		}
		buf = append(buf, itemBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}

// encodeGenericList encodes a standard Go slice
func encodeGenericList(list []interface{}) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'l')

	for _, item := range list {
		itemBytes, err := Encode(item)
		if err != nil {
			return nil, err
		}
		buf = append(buf, itemBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}

// encodeDict encodes a bencode dictionary.
// REQUIRED: Keys must be sorted alphabetically.
func encodeDict(dict map[string]Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'd')

	// Extract and sort keys
	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Append key-value pairs in sorted order
	for _, key := range keys {
		buf = append(buf, encodeString(key)...)
		valBytes, err := Encode(dict[key])
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}

// encodeGenericDict encodes a standard Go map (like our DHT get_peers query)
func encodeGenericDict(dict map[string]interface{}) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'd')

	// Extract and sort keys
	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Append key-value pairs in sorted order
	for _, key := range keys {
		buf = append(buf, encodeString(key)...)
		valBytes, err := Encode(dict[key])
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}
