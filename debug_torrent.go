package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackpal/bencode-go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run debug_torrent.go <torrent-file>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		os.Exit(1)
	}

	// Decode into a generic map
	var result interface{}
	err = bencode.Unmarshal(bytes.NewReader(data), &result)
	if err != nil {
		fmt.Printf("Error decoding bencode: %v\n", err)
		os.Exit(1)
	}

	// Pretty print as JSON
	jsonData, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(jsonData))
}
