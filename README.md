# go-torrent

A BitTorrent client written in Go, with a terminal UI. Everything below the TUI
is implemented from scratch: bencode, .torrent parsing, tracker protocols, the
peer wire protocol, and DHT peer discovery.

## Status

**Download-only.** This client does not seed: it has no listening socket and
does not serve pieces to other peers. Please don't rely on it as a good swarm
citizen.

## Features

- `.torrent` files, single- and multi-file
- Magnet links (metadata fetched from peers via BEP 9/10)
- HTTP/HTTPS and UDP (BEP 15) trackers, with periodic re-announce
- DHT peer discovery (crawler; it does not announce itself or answer queries)
- Resume: existing data is re-hashed on start, and valid pieces are skipped
- Terminal UI with progress, transfer rate, ETA, and a live event log

## Install

Build from source:

    go install github.com/Vaivaswat2244/go-torrent/cmd/client@latest

That installs a binary named `client`. To build it under its own name:

    go build -o go-torrent ./cmd/client

## Usage

    go-torrent [-output <dir>]

Then choose a torrent file or magnet link from the menu.

Keys: `↑`/`↓` and `enter` to navigate, `esc` to go back or cancel a metadata
fetch, `q` or `ctrl+c` to quit.

## Options

    -output   Output directory (default: current directory)

## Development

    go build ./...
    go vet ./...
    go test -race ./...

The race detector matters here: our bitfield is shared between the download loop
and every peer goroutine.

## Not implemented

Seeding/upload, choke-unchoke and tit-for-tat, rarest-first and endgame piece
selection, rate limiting, multiple simultaneous torrents, IPv6, base32 magnet
links, peer encryption (MSE), and UPnP/NAT-PMP port mapping.

## License

MIT. See [LICENSE](LICENSE).
