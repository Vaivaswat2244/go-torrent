package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

func main() {

	torrentPath := flag.String("torrent", "", "Path to the .torrent file")
	outputDir := flag.String("output", ".", "Output directory for downloaded files")
	flag.Parse()

	if *torrentPath == "" {
		fmt.Println("Usage: torrent -torrent <path> [-output <dir>]")
		os.Exit(1)
	}
	tf, err := torrentfile.Open(*torrentPath)
	if err != nil {
		log.Fatalf("Failed to parse torrent file: %v", err)
	}

	fmt.Printf("Torrent: %s\n", tf.Name)
	fmt.Printf("Info Hash: %x\n", tf.InfoHash)
	fmt.Printf("Piece Length: %d bytes\n", tf.PieceLength)
	fmt.Printf("Total Size: %d bytes\n", tf.Length)
	fmt.Printf("Tracker: %s\n", tf.Announce)
	fmt.Printf("Number of Pieces: %d\n", len(tf.PieceHashes))

	fmt.Printf("\nOutput directory: %s\n", *outputDir)
	fmt.Println("Ready to download! (Download logic will be added in Phase 2)")
}
