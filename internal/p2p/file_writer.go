package p2p

import (
	"fmt"
	"os"
)

// FileWriter handles writing pieces to a file
type FileWriter struct {
	file        *os.File
	pieceLength int
}

// NewFileWriter creates a new file writer
func NewFileWriter(path string, totalSize int, pieceLength int) (*FileWriter, error) {
	// Create/open file
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}

	// Pre-allocate the file to the correct size
	err = file.Truncate(int64(totalSize))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to allocate file: %w", err)
	}

	return &FileWriter{
		file:        file,
		pieceLength: pieceLength,
	}, nil
}

// WritePiece writes a piece at the correct offset in the file
func (fw *FileWriter) WritePiece(index int, data []byte) error {
	// Calculate offset in file
	offset := int64(index) * int64(fw.pieceLength)

	// Seek to the correct position
	_, err := fw.file.Seek(offset, 0)
	if err != nil {
		return fmt.Errorf("failed to seek: %w", err)
	}

	// Write the data
	bytesWritten, err := fw.file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write: %w", err)
	}

	if bytesWritten != len(data) {
		return fmt.Errorf("incomplete write: wrote %d/%d bytes", bytesWritten, len(data))
	}

	// Sync to disk (ensure it's written)
	err = fw.file.Sync()
	if err != nil {
		return fmt.Errorf("failed to sync: %w", err)
	}

	return nil
}

// Close closes the file
func (fw *FileWriter) Close() error {
	return fw.file.Close()
}
