package torrentfile

import (
	"crypto/sha1"
	"fmt"
	"os"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
)

// TorrentFile represents the parsed .torrent file
type TorrentFile struct {
	Announce     string
	AnnounceList [][]string // Tiers of backup trackers
	InfoHash     [20]byte
	PieceHashes  [][20]byte
	PieceLength  int
	Length       int
	Name         string
}

// Open parses a .torrent file and returns a TorrentFile
func Open(path string) (*TorrentFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Decode bencode
	value, err := bencode.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bencode: %w", err)
	}

	// The root should be a dictionary
	root, ok := value.(map[string]bencode.Value)
	if !ok {
		return nil, fmt.Errorf("root value is not a dictionary")
	}

	// Extract announce URL
	announce, err := bencode.GetString(root, "announce")
	if err != nil {
		// Some torrents only have announce-list, no single announce
		announce = ""
	}

	// Extract announce-list (optional, array of arrays of strings)
	var announceList [][]string
	if announceListVal, ok := root["announce-list"]; ok {
		announceList = parseAnnounceList(announceListVal)
	}

	// Extract info dictionary
	infoDict, err := bencode.GetDict(root, "info")
	if err != nil {
		return nil, fmt.Errorf("failed to get info dict: %w", err)
	}

	// Extract info fields
	name, err := bencode.GetString(infoDict, "name")
	if err != nil {
		return nil, fmt.Errorf("failed to get name: %w", err)
	}

	pieceLength, err := bencode.GetInt(infoDict, "piece length")
	if err != nil {
		return nil, fmt.Errorf("failed to get piece length: %w", err)
	}

	pieces, err := bencode.GetString(infoDict, "pieces")
	if err != nil {
		return nil, fmt.Errorf("failed to get pieces: %w", err)
	}

	// Get length (single file) or calculate from files (multi-file)
	length := int64(0)
	lengthVal, err := bencode.GetInt(infoDict, "length")
	if err == nil {
		// Single file torrent
		length = lengthVal
	} else {
		// Multi-file torrent
		filesVal, ok := infoDict["files"]
		if ok {
			filesList, ok := filesVal.([]bencode.Value)
			if ok {
				for _, f := range filesList {
					fileDict, ok := f.(map[string]bencode.Value)
					if ok {
						fileLen, err := bencode.GetInt(fileDict, "length")
						if err == nil {
							length += fileLen
						}
					}
				}
			}
		}
	}

	// Calculate info hash
	infoHash, err := calculateInfoHash(infoDict)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate info hash: %w", err)
	}

	// Split pieces into hashes
	pieceHashes, err := splitPieceHashes(pieces)
	if err != nil {
		return nil, fmt.Errorf("failed to split piece hashes: %w", err)
	}

	tf := &TorrentFile{
		Announce:     announce,
		AnnounceList: announceList,
		InfoHash:     infoHash,
		PieceHashes:  pieceHashes,
		PieceLength:  int(pieceLength),
		Length:       int(length),
		Name:         name,
	}

	return tf, nil
}

// parseAnnounceList converts bencode value to [][]string
func parseAnnounceList(val bencode.Value) [][]string {
	var result [][]string

	// announce-list is a list of lists
	outerList, ok := val.([]bencode.Value)
	if !ok {
		return result
	}

	for _, tierVal := range outerList {
		tier, ok := tierVal.([]bencode.Value)
		if !ok {
			continue
		}

		var trackers []string
		for _, trackerVal := range tier {
			tracker, ok := trackerVal.(string)
			if ok {
				trackers = append(trackers, tracker)
			}
		}

		if len(trackers) > 0 {
			result = append(result, trackers)
		}
	}

	return result
}

// calculateInfoHash computes the SHA-1 hash of the info dictionary
func calculateInfoHash(infoDict map[string]bencode.Value) ([20]byte, error) {
	// Re-encode the info dictionary to bencode
	// This ensures we get the exact bytes that were in the original torrent
	encoded, err := encodeDict(infoDict)
	if err != nil {
		return [20]byte{}, fmt.Errorf("failed to encode info dict: %w", err)
	}

	// Calculate SHA-1 hash
	return sha1.Sum(encoded), nil
}

// encodeDict encodes a bencode dictionary back to bytes
func encodeDict(dict map[string]bencode.Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'd') // Start dictionary

	// Bencode requires keys to be sorted
	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}

	// Sort keys alphabetically (bencode requirement)
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	// Encode each key-value pair
	for _, key := range keys {
		// Encode key (string)
		keyBytes := encodeString(key)
		buf = append(buf, keyBytes...)

		// Encode value
		valBytes, err := encodeValue(dict[key])
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}

	buf = append(buf, 'e') // End dictionary
	return buf, nil
}

// encodeValue encodes any bencode value
func encodeValue(val bencode.Value) ([]byte, error) {
	switch v := val.(type) {
	case string:
		return encodeString(v), nil
	case int64:
		return encodeInt(v), nil
	case []bencode.Value:
		return encodeList(v)
	case map[string]bencode.Value:
		return encodeDict(v)
	default:
		return nil, fmt.Errorf("unsupported type: %T", v)
	}
}

// encodeString encodes a string to bencode format
func encodeString(s string) []byte {
	return []byte(fmt.Sprintf("%d:%s", len(s), s))
}

// encodeInt encodes an integer to bencode format
func encodeInt(i int64) []byte {
	return []byte(fmt.Sprintf("i%de", i))
}

// encodeList encodes a list to bencode format
func encodeList(list []bencode.Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'l') // Start list

	for _, item := range list {
		itemBytes, err := encodeValue(item)
		if err != nil {
			return nil, err
		}
		buf = append(buf, itemBytes...)
	}

	buf = append(buf, 'e') // End list
	return buf, nil
}

// splitPieceHashes splits the pieces string into 20-byte hashes
func splitPieceHashes(pieces string) ([][20]byte, error) {
	const hashLen = 20
	buf := []byte(pieces)

	if len(buf)%hashLen != 0 {
		return nil, fmt.Errorf("invalid pieces length: %d", len(buf))
	}

	numHashes := len(buf) / hashLen
	hashes := make([][20]byte, numHashes)

	for i := 0; i < numHashes; i++ {
		copy(hashes[i][:], buf[i*hashLen:(i+1)*hashLen])
	}

	return hashes, nil
}
