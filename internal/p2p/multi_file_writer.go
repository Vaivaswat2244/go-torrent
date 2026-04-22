package p2p

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// fileEntry keeps track of an open file and its position in the global byte array
type fileEntry struct {
	file         *os.File
	length       int64
	globalOffset int64 // Where this file starts in the grand scheme of the torrent
}

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
	isMultiFile := len(tf.Files) > 1
	targetDir := baseDir
	if isMultiFile {
		targetDir = filepath.Join(baseDir, tf.Name)
	}

	for _, f := range tf.Files {
		// Build the full path (e.g., targetDir/subtitles/de.srt)
		fullPath := targetDir
		for _, p := range f.Path {
			fullPath = filepath.Join(fullPath, p)
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

// WritePiece slices the piece data and distributes it to the correct files
func (mw *MultiFileWriter) WritePiece(pieceIndex int, data []byte) error {
	pieceStart := int64(pieceIndex) * int64(mw.pieceLength)
	pieceEnd := pieceStart + int64(len(data))

	dataOffset := 0 // Tracks how much of the piece we've written so far

	for _, f := range mw.files {
		fileStart := f.globalOffset
		fileEnd := f.globalOffset + f.length

		// If this file ends before our piece starts, skip it
		if fileEnd <= pieceStart {
			continue
		}

		// If this file starts after our piece ends, we are completely done
		if fileStart >= pieceEnd {
			break
		}

		// There is an overlap! Calculate exactly where to write and how much.
		localSeekPos := int64(0)
		if pieceStart > fileStart {
			localSeekPos = pieceStart - fileStart
		}

		// Calculate how many bytes we can write to this file
		bytesToWrite := int64(len(data) - dataOffset)
		if localSeekPos+bytesToWrite > f.length {
			bytesToWrite = f.length - localSeekPos
		}

		// Seek and Write
		_, err := f.file.Seek(localSeekPos, 0)
		if err != nil {
			return fmt.Errorf("seek failed: %w", err)
		}

		chunkToWrite := data[dataOffset : dataOffset+int(bytesToWrite)]
		_, err = f.file.Write(chunkToWrite)
		if err != nil {
			return fmt.Errorf("write failed: %w", err)
		}

		// Update our data offset for the next file (if the piece spans boundaries)
		dataOffset += int(bytesToWrite)
		pieceStart += bytesToWrite

		if dataOffset >= len(data) {
			break // Entire piece written
		}
	}

	return nil
}

func (mw *MultiFileWriter) ReadPiece(pieceIndex int, expectedLength int) ([]byte, error) {
	data := make([]byte, expectedLength)
	pieceStart := int64(pieceIndex) * int64(mw.pieceLength)
	pieceEnd := pieceStart + int64(expectedLength)

	dataOffset := 0

	for _, f := range mw.files {
		fileStart := f.globalOffset
		fileEnd := f.globalOffset + f.length

		// Skip files that don't overlap with this piece
		if fileEnd <= pieceStart {
			continue
		}
		if fileStart >= pieceEnd {
			break
		}

		localSeekPos := int64(0)
		if pieceStart > fileStart {
			localSeekPos = pieceStart - fileStart
		}

		bytesToRead := int64(expectedLength - dataOffset)
		if localSeekPos+bytesToRead > f.length {
			bytesToRead = f.length - localSeekPos
		}

		_, err := f.file.Seek(localSeekPos, 0)
		if err != nil {
			return nil, err
		}

		// Use io.ReadFull to guarantee we read exactly what we need
		chunkToRead := data[dataOffset : dataOffset+int(bytesToRead)]
		_, err = io.ReadFull(f.file, chunkToRead)
		if err != nil {
			return nil, err
		} // File might not be fully written yet, that's fine

		dataOffset += int(bytesToRead)
		pieceStart += bytesToRead

		if dataOffset >= expectedLength {
			break
		}
	}

	return data, nil
}

func (mw *MultiFileWriter) Close() {
	for _, f := range mw.files {
		f.file.Close()
	}
}
