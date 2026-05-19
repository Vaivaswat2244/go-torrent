package magnet

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// Magnet represents a parsed Magnet URI
type Magnet struct {
	InfoHash [20]byte
	Name     string
	Trackers []string
}

// Parse extracts the InfoHash, Name, and Trackers from a magnet link string
func Parse(uri string) (*Magnet, error) {
	if !strings.HasPrefix(uri, "magnet:?") {
		return nil, fmt.Errorf("invalid magnet link format")
	}

	// Parse the query parameters
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	m := &Magnet{}

	// 1. Extract InfoHash (xt=urn:btih:<hash>)
	xts := q["xt"]
	if len(xts) == 0 {
		return nil, fmt.Errorf("magnet link missing exact topic (xt)")
	}

	hashFound := false
	for _, xt := range xts {
		if strings.HasPrefix(xt, "urn:btih:") {
			hashStr := strings.TrimPrefix(xt, "urn:btih:")

			// It's usually a 40-character Hex string
			if len(hashStr) == 40 {
				hashBytes, err := hex.DecodeString(hashStr)
				if err != nil {
					continue
				}
				copy(m.InfoHash[:], hashBytes)
				hashFound = true
				break
			}
		}
	}

	if !hashFound {
		return nil, fmt.Errorf("could not find valid InfoHash in magnet link")
	}

	// 2. Extract Display Name (dn)
	if dns := q["dn"]; len(dns) > 0 {
		m.Name = dns[0]
	} else {
		m.Name = "Unknown_Magnet_Download"
	}

	// 3. Extract Trackers (tr)
	if trs := q["tr"]; len(trs) > 0 {
		m.Trackers = append(m.Trackers, trs...)
	}

	return m, nil
}

func (m *Magnet) ToTorrentFile() *torrentfile.TorrentFile {
	return &torrentfile.TorrentFile{
		Name:     m.Name,
		InfoHash: m.InfoHash,
		Announce: "", // We can use m.Trackers[0] if it exists
	}
}
