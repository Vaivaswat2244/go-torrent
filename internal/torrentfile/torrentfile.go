package torrentfile

import (
	"crypto/sha1"
	"fmt"
	"os"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
)

type FileInfo struct {
	Length int
	Path   []string
}

// TorrentFile represents the parsed .torrent file
type TorrentFile struct {
	Announce     string
	AnnounceList [][]string
	InfoHash     [20]byte
	PieceHashes  [][20]byte
	PieceLength  int
	Length       int
	Name         string
	Files        []FileInfo
	Trackers     []string // flat list of all tracker URLs (magnet + announce-list)
}

// Open parses a .torrent file and returns a TorrentFile
func Open(path string) (*TorrentFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	value, err := bencode.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bencode: %w", err)
	}

	root, ok := value.(map[string]bencode.Value)
	if !ok {
		return nil, fmt.Errorf("root value is not a dictionary")
	}

	announce, err := bencode.GetString(root, "announce")
	if err != nil {
		announce = ""
	}

	var announceList [][]string
	if announceListVal, ok := root["announce-list"]; ok {
		announceList = parseAnnounceList(announceListVal)
	}

	infoDict, err := bencode.GetDict(root, "info")
	if err != nil {
		return nil, fmt.Errorf("failed to get info dict: %w", err)
	}

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

	length := int64(0)
	var files []FileInfo

	lengthVal, err := bencode.GetInt(infoDict, "length")
	if err == nil {
		length = lengthVal
		files = append(files, FileInfo{
			Length: int(length),
			Path:   []string{name},
		})
	} else {
		filesVal, ok := infoDict["files"]
		if !ok {
			return nil, fmt.Errorf("missing both length and files")
		}

		filesList, ok := filesVal.([]bencode.Value)
		if !ok {
			return nil, fmt.Errorf("files is not a list")
		}
		for _, f := range filesList {
			fileDict, ok := f.(map[string]bencode.Value)
			if !ok {
				continue
			}

			fileLen, err := bencode.GetInt(fileDict, "length")
			if err != nil {
				continue
			}

			length += fileLen

			pathVal, ok := fileDict["path"].([]bencode.Value)
			if !ok {
				continue
			}

			var path []string
			for _, p := range pathVal {
				if str, ok := p.(string); ok {
					path = append(path, str)
				}
			}

			files = append(files, FileInfo{
				Length: int(fileLen),
				Path:   path,
			})
		}
	}

	infoHash, err := calculateInfoHash(infoDict)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate info hash: %w", err)
	}

	pieceHashes, err := splitPieceHashes(pieces)
	if err != nil {
		return nil, fmt.Errorf("failed to split piece hashes: %w", err)
	}

	// Build flat tracker list from announce + announce-list
	trackers := buildTrackerList(announce, announceList)

	tf := &TorrentFile{
		Announce:     announce,
		AnnounceList: announceList,
		InfoHash:     infoHash,
		PieceHashes:  pieceHashes,
		PieceLength:  int(pieceLength),
		Length:       int(length),
		Name:         name,
		Files:        files,
		Trackers:     trackers,
	}

	return tf, nil
}

func ParseInfoDict(infoDict map[string]bencode.Value, infoHash [20]byte) (*TorrentFile, error) {
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

	length := int64(0)
	var files []FileInfo

	lengthVal, err := bencode.GetInt(infoDict, "length")
	if err == nil {
		length = lengthVal
		files = append(files, FileInfo{
			Length: int(length),
			Path:   []string{name},
		})
	} else {
		filesVal, ok := infoDict["files"]
		if !ok {
			return nil, fmt.Errorf("missing both length and files")
		}

		filesList, ok := filesVal.([]bencode.Value)
		if !ok {
			return nil, fmt.Errorf("files is not a list")
		}
		for _, f := range filesList {
			fileDict, ok := f.(map[string]bencode.Value)
			if !ok {
				continue
			}

			fileLen, err := bencode.GetInt(fileDict, "length")
			if err != nil {
				continue
			}

			length += fileLen

			pathVal, ok := fileDict["path"].([]bencode.Value)
			if !ok {
				continue
			}

			var path []string
			for _, p := range pathVal {
				if str, ok := p.(string); ok {
					path = append(path, str)
				}
			}

			files = append(files, FileInfo{
				Length: int(fileLen),
				Path:   path,
			})
		}
	}

	pieceHashes, err := splitPieceHashes(pieces)
	if err != nil {
		return nil, fmt.Errorf("failed to split piece hashes: %w", err)
	}

	return &TorrentFile{
		InfoHash:    infoHash,
		PieceHashes: pieceHashes,
		PieceLength: int(pieceLength),
		Length:      int(length),
		Name:        name,
		Files:       files,
		// Trackers will be set by the caller (main.go sets tf.Trackers = mag.Trackers)
	}, nil
}

// buildTrackerList builds a flat deduplicated list of all tracker URLs
func buildTrackerList(announce string, announceList [][]string) []string {
	seen := make(map[string]bool)
	var trackers []string

	add := func(url string) {
		if url != "" && !seen[url] {
			seen[url] = true
			trackers = append(trackers, url)
		}
	}

	add(announce)
	for _, tier := range announceList {
		for _, url := range tier {
			add(url)
		}
	}

	return trackers
}

func parseAnnounceList(val bencode.Value) [][]string {
	var result [][]string

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

func calculateInfoHash(infoDict map[string]bencode.Value) ([20]byte, error) {
	encoded, err := encodeDict(infoDict)
	if err != nil {
		return [20]byte{}, fmt.Errorf("failed to encode info dict: %w", err)
	}
	return sha1.Sum(encoded), nil
}

func encodeDict(dict map[string]bencode.Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'd')

	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}

	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	for _, key := range keys {
		buf = append(buf, encodeString(key)...)
		valBytes, err := encodeValue(dict[key])
		if err != nil {
			return nil, err
		}
		buf = append(buf, valBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}

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

func encodeString(s string) []byte {
	return []byte(fmt.Sprintf("%d:%s", len(s), s))
}

func encodeInt(i int64) []byte {
	return []byte(fmt.Sprintf("i%de", i))
}

func encodeList(list []bencode.Value) ([]byte, error) {
	var buf []byte
	buf = append(buf, 'l')

	for _, item := range list {
		itemBytes, err := encodeValue(item)
		if err != nil {
			return nil, err
		}
		buf = append(buf, itemBytes...)
	}

	buf = append(buf, 'e')
	return buf, nil
}

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
