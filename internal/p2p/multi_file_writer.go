package p2p

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// fileEntry keeps track of an open file and its position in the global byte array
type fileEntry struct {
	file         *os.File
	length       int64
	globalOffset int64 // Where this file starts in the grand scheme of the torrent
}

// MultiFileWriter presents a torrent's files as one flat byte array.
//
// It is safe for concurrent use: all I/O goes through ReadAt/WriteAt, which are
// positional and hold no per-file cursor. The previous Seek-then-Read/Write
// pairs shared a cursor across callers, so a reader and a writer touching the
// same file would interleave and return the wrong bytes.
type MultiFileWriter struct {
	files       []fileEntry
	pieceLength int
}

// NewMultiFileWriter creates directories and opens all files
func NewMultiFileWriter(baseDir string, tf *torrentfile.TorrentFile) (*MultiFileWriter, error) {
	if len(tf.Files) == 0 {
		return nil, fmt.Errorf("FATAL: Torrent contains no files. Check your parser!")
	}
	var entries []fileEntry
	var currentGlobalOffset int64 = 0

	// If it's a multi-file torrent, the base folder is tf.Name
	// If it's a single file, tf.Name is just the file name, so we don't append it to the base dir
	targetDir := baseDir
	if tf.IsMultiFile {
		targetDir = filepath.Join(baseDir, tf.Name)
	}

	// Resolved once so every file can be checked against it below.
	cleanTarget := filepath.Clean(targetDir)
	prefix := cleanTarget + string(os.PathSeparator)

	for _, f := range tf.Files {
		// Build the full path (e.g., targetDir/subtitles/de.srt)
		fullPath := targetDir
		for _, p := range f.Path {
			fullPath = filepath.Join(fullPath, p)
		}

		// Defence in depth. torrentfile rejects unsafe path components at parse
		// time, but this writer opens files with O_CREATE, so it re-checks that
		// the resolved path is still inside the download directory rather than
		// trusting its caller.
		fullPath = filepath.Clean(fullPath)
		if fullPath != cleanTarget && !strings.HasPrefix(fullPath, prefix) {
			return nil, fmt.Errorf("refusing to write %q: outside download directory %q", fullPath, cleanTarget)
		}

		// Create parent directories
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			return nil, fmt.Errorf("failed to create dirs for %s: %w", fullPath, err)
		}

		// Create/Open the file
		file, err := os.OpenFile(fullPath, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return nil, fmt.Errorf("failed to open file %s: %w", fullPath, err)
		}

		// Pre-allocate space
		err = file.Truncate(int64(f.Length))
		if err != nil {
			return nil, fmt.Errorf("failed to truncate file %s: %w", fullPath, err)
		}

		entries = append(entries, fileEntry{
			file:         file,
			length:       int64(f.Length),
			globalOffset: currentGlobalOffset,
		})

		currentGlobalOffset += int64(f.Length)
	}

	return &MultiFileWriter{
		files:       entries,
		pieceLength: tf.PieceLength,
	}, nil
}

// forEachFileRange maps the global byte range [globalOffset, globalOffset+len(p))
// onto the files it spans, calling fn once per file with that file's local
// offset and the corresponding slice of p.
//
// A torrent is one flat byte array split across files, and a piece can straddle
// a file boundary, so both reads and writes need this mapping. It used to be
// duplicated between WritePiece and ReadPiece.
func (mw *MultiFileWriter) forEachFileRange(
	globalOffset int64,
	p []byte,
	fn func(f fileEntry, localOffset int64, chunk []byte) error,
) error {
	if globalOffset < 0 {
		return fmt.Errorf("negative offset %d", globalOffset)
	}

	end := globalOffset + int64(len(p))
	cur := globalOffset
	dataOffset := 0

	for _, f := range mw.files {
		fileStart := f.globalOffset
		fileEnd := f.globalOffset + f.length

		// This file ends before our range starts.
		if fileEnd <= cur {
			continue
		}
		// This file starts after our range ends; the rest cannot overlap.
		if fileStart >= end {
			break
		}

		localOffset := int64(0)
		if cur > fileStart {
			localOffset = cur - fileStart
		}

		n := int64(len(p) - dataOffset)
		if localOffset+n > f.length {
			n = f.length - localOffset
		}
		if n <= 0 {
			continue
		}

		if err := fn(f, localOffset, p[dataOffset:dataOffset+int(n)]); err != nil {
			return err
		}

		dataOffset += int(n)
		cur += n

		if dataOffset >= len(p) {
			break
		}
	}

	// Falling short means the caller asked for bytes past the end of the
	// torrent. Previously this returned short data with no error.
	if dataOffset < len(p) {
		return fmt.Errorf("range [%d,%d) extends past the end of the torrent", globalOffset, end)
	}
	return nil
}

// writeAt writes p at a global byte offset.
func (mw *MultiFileWriter) writeAt(globalOffset int64, p []byte) error {
	return mw.forEachFileRange(globalOffset, p, func(f fileEntry, localOffset int64, chunk []byte) error {
		if _, err := f.file.WriteAt(chunk, localOffset); err != nil {
			return fmt.Errorf("write failed: %w", err)
		}
		return nil
	})
}

// readAt fills p from a global byte offset.
func (mw *MultiFileWriter) readAt(globalOffset int64, p []byte) error {
	return mw.forEachFileRange(globalOffset, p, func(f fileEntry, localOffset int64, chunk []byte) error {
		// ReadAt reads len(chunk) bytes or returns an error, so it gives the
		// same guarantee io.ReadFull used to.
		if _, err := f.file.ReadAt(chunk, localOffset); err != nil {
			return fmt.Errorf("read failed: %w", err)
		}
		return nil
	})
}

// WritePiece stores a complete piece, distributing it across files as needed.
func (mw *MultiFileWriter) WritePiece(pieceIndex int, data []byte) error {
	if pieceIndex < 0 {
		return fmt.Errorf("negative piece index %d", pieceIndex)
	}
	return mw.writeAt(int64(pieceIndex)*int64(mw.pieceLength), data)
}

// ReadPiece reads a whole piece back, used by resume verification.
func (mw *MultiFileWriter) ReadPiece(pieceIndex int, expectedLength int) ([]byte, error) {
	if pieceIndex < 0 || expectedLength <= 0 {
		return nil, fmt.Errorf("invalid piece read: index %d length %d", pieceIndex, expectedLength)
	}

	data := make([]byte, expectedLength)
	if err := mw.readAt(int64(pieceIndex)*int64(mw.pieceLength), data); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadBlock reads a sub-range of a piece, which is what serving a peer request
// needs: blocks are 16 KB while a piece may be megabytes.
//
// Callers must validate the request against the torrent's geometry first; this
// only guards against reading outside the torrent itself.
func (mw *MultiFileWriter) ReadBlock(pieceIndex, begin, length int) ([]byte, error) {
	if pieceIndex < 0 || begin < 0 || length <= 0 {
		return nil, fmt.Errorf("invalid block read: piece %d begin %d length %d", pieceIndex, begin, length)
	}

	buf := make([]byte, length)
	offset := int64(pieceIndex)*int64(mw.pieceLength) + int64(begin)
	if err := mw.readAt(offset, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (mw *MultiFileWriter) Close() {
	for _, f := range mw.files {
		// Flush before closing; pieces are written with plain Write calls, so
		// without this a crash right after completion can lose recent pieces.
		f.file.Sync()
		f.file.Close()
	}
}
