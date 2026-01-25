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
	infoHash, err := calculateInfoHash(data)
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
func calculateInfoHash(data []byte) ([20]byte, error) {
	// Find the info dictionary in the bencoded data
	// The info dict starts after "4:info" and ends at the matching 'e'

	infoStart := findInfoDictStart(data)
	if infoStart == -1 {
		return [20]byte{}, fmt.Errorf("info dictionary not found")
	}

	infoEnd := findInfoDictEnd(data, infoStart)
	if infoEnd == -1 {
		return [20]byte{}, fmt.Errorf("info dictionary end not found")
	}

	infoData := data[infoStart:infoEnd]
	return sha1.Sum(infoData), nil
}

// findInfoDictStart finds the start position of the info dictionary
func findInfoDictStart(data []byte) int {
	// Look for "4:infod"
	needle := []byte("4:info")
	for i := 0; i < len(data)-len(needle); i++ {
		if string(data[i:i+len(needle)]) == string(needle) {
			return i + len(needle)
		}
	}
	return -1
}

// findInfoDictEnd finds the end of a dictionary starting at pos
func findInfoDictEnd(data []byte, start int) int {
	depth := 1
	pos := start

	for pos < len(data) && depth > 0 {
		switch data[pos] {
		case 'd', 'l':
			depth++
		case 'e':
			depth--
			if depth == 0 {
				return pos + 1
			}
		case 'i':
			// Skip integer
			pos++
			for pos < len(data) && data[pos] != 'e' {
				pos++
			}
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// Skip string
			colonPos := pos
			for colonPos < len(data) && data[colonPos] != ':' {
				colonPos++
			}
			if colonPos >= len(data) {
				return -1
			}
			lengthStr := string(data[pos:colonPos])
			length := 0
			fmt.Sscanf(lengthStr, "%d", &length)
			pos = colonPos + 1 + length - 1
		}
		pos++
	}

	return -1
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
