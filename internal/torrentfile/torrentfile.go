package torrentfile

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// IsMultiFile records whether the torrent used the "files" layout. It cannot
	// be inferred from len(Files), since a multi-file torrent may legitimately
	// contain exactly one file and still needs its containing directory.
	IsMultiFile bool
	Trackers    []string // flat list of all tracker URLs (magnet + announce-list)
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

	tf, err := parseInfo(infoDict)
	if err != nil {
		return nil, err
	}

	infoHash, err := calculateInfoHash(infoDict)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate info hash: %w", err)
	}

	tf.Announce = announce
	tf.AnnounceList = announceList
	tf.InfoHash = infoHash
	tf.Trackers = buildTrackerList(announce, announceList)

	return tf, nil
}

// ParseInfoDict builds a TorrentFile from an info dictionary fetched from peers
// via BEP 9. The caller supplies the info hash (from the magnet link) and is
// responsible for setting Trackers.
func ParseInfoDict(infoDict map[string]bencode.Value, infoHash [20]byte) (*TorrentFile, error) {
	tf, err := parseInfo(infoDict)
	if err != nil {
		return nil, err
	}
	tf.InfoHash = infoHash
	return tf, nil
}

// parseInfo extracts the fields common to .torrent files and magnet-fetched
// metadata. Open and ParseInfoDict previously duplicated this block verbatim,
// which meant every validation rule had to be written twice.
func parseInfo(infoDict map[string]bencode.Value) (*TorrentFile, error) {
	name, err := bencode.GetString(infoDict, "name")
	if err != nil {
		return nil, fmt.Errorf("failed to get name: %w", err)
	}

	// name becomes the containing directory for multi-file torrents, so it is
	// just as dangerous as an individual path component.
	if err := validatePathComponent(name); err != nil {
		return nil, fmt.Errorf("unsafe torrent name: %w", err)
	}

	pieceLength, err := bencode.GetInt(infoDict, "piece length")
	if err != nil {
		return nil, fmt.Errorf("failed to get piece length: %w", err)
	}
	if pieceLength <= 0 {
		return nil, fmt.Errorf("invalid piece length: %d", pieceLength)
	}

	pieces, err := bencode.GetString(infoDict, "pieces")
	if err != nil {
		return nil, fmt.Errorf("failed to get pieces: %w", err)
	}

	pieceHashes, err := splitPieceHashes(pieces)
	if err != nil {
		return nil, fmt.Errorf("failed to split piece hashes: %w", err)
	}

	files, length, isMulti, err := parseFiles(infoDict, name)
	if err != nil {
		return nil, err
	}

	return &TorrentFile{
		PieceHashes: pieceHashes,
		PieceLength: int(pieceLength),
		Length:      int(length),
		Name:        name,
		Files:       files,
		IsMultiFile: isMulti,
	}, nil
}

// parseFiles handles both the single-file ("length") and multi-file ("files")
// layouts, returning the file list and the total torrent length.
func parseFiles(infoDict map[string]bencode.Value, name string) ([]FileInfo, int64, bool, error) {
	// Single-file torrent: one "length" key, the file is named by "name".
	if lengthVal, err := bencode.GetInt(infoDict, "length"); err == nil {
		if lengthVal < 0 {
			return nil, 0, false, fmt.Errorf("negative torrent length: %d", lengthVal)
		}
		return []FileInfo{{Length: int(lengthVal), Path: []string{name}}}, lengthVal, false, nil
	}

	filesVal, ok := infoDict["files"]
	if !ok {
		return nil, 0, false, fmt.Errorf("missing both length and files")
	}

	filesList, ok := filesVal.([]bencode.Value)
	if !ok {
		return nil, 0, false, fmt.Errorf("files is not a list")
	}
	if len(filesList) == 0 {
		return nil, 0, false, fmt.Errorf("torrent contains no files")
	}

	var files []FileInfo
	var total int64

	// Malformed entries are rejected outright rather than skipped. Skipping them
	// produced a torrent that "parsed successfully" but had missing files and a
	// total length that disagreed with the piece hashes.
	for i, f := range filesList {
		fileDict, ok := f.(map[string]bencode.Value)
		if !ok {
			return nil, 0, false, fmt.Errorf("file %d is not a dictionary", i)
		}

		fileLen, err := bencode.GetInt(fileDict, "length")
		if err != nil {
			return nil, 0, false, fmt.Errorf("file %d: %w", i, err)
		}
		if fileLen < 0 {
			return nil, 0, false, fmt.Errorf("file %d has negative length: %d", i, fileLen)
		}

		pathVal, ok := fileDict["path"].([]bencode.Value)
		if !ok {
			return nil, 0, false, fmt.Errorf("file %d: path is not a list", i)
		}
		if len(pathVal) == 0 {
			return nil, 0, false, fmt.Errorf("file %d: path is empty", i)
		}

		path := make([]string, 0, len(pathVal))
		for _, p := range pathVal {
			str, ok := p.(string)
			if !ok {
				return nil, 0, false, fmt.Errorf("file %d: path component is not a string", i)
			}
			if err := validatePathComponent(str); err != nil {
				return nil, 0, false, fmt.Errorf("file %d: %w", i, err)
			}
			path = append(path, str)
		}

		total += fileLen
		files = append(files, FileInfo{Length: int(fileLen), Path: path})
	}

	return files, total, true, nil
}

// validatePathComponent rejects path elements that could escape the download
// directory. Torrent files are untrusted input: a path of
// ["..", "..", ".ssh", "authorized_keys"] would otherwise be joined onto the
// output directory and opened with O_CREATE.
func validatePathComponent(p string) error {
	if p == "" {
		return fmt.Errorf("empty path component")
	}
	if p == "." || p == ".." {
		return fmt.Errorf("path component %q traverses directories", p)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path component contains a NUL byte")
	}
	// Both separators are rejected regardless of host OS, since the same torrent
	// may be opened by the Windows build.
	if strings.ContainsAny(p, `/\`) {
		return fmt.Errorf("path component %q contains a path separator", p)
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return fmt.Errorf("path component %q is not relative", p)
	}
	return nil
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

	return FilterSupportedTrackers(trackers)
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
	encoded, err := bencode.Encode(infoDict)
	if err != nil {
		return [20]byte{}, fmt.Errorf("failed to encode info dict: %w", err)
	}
	return sha1.Sum(encoded), nil
}

func splitPieceHashes(pieces string) ([][20]byte, error) {
	const hashLen = 20
	buf := []byte(pieces)

	if len(buf) == 0 {
		return nil, fmt.Errorf("torrent has no piece hashes")
	}
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
