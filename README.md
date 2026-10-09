# go-torrent

A BitTorrent client written in Go, with a terminal UI. Everything below the TUI
is implemented from scratch: bencode, .torrent parsing, tracker protocols, the
peer wire protocol, and DHT peer discovery.

## Features

- `.torrent` files, single- and multi-file
- Magnet links (metadata fetched from peers via BEP 9/10)
- HTTP/HTTPS and UDP (BEP 15) trackers, with periodic re-announce
- DHT peer discovery (crawler; it does not announce itself or answer queries)
- **Uploading and seeding**, with the standard choke algorithm: four unchoke
  slots recomputed every 10s, plus an optimistic unchoke every 30s
- Resume: existing data is re-hashed on start, and valid pieces are skipped
- Terminal UI with progress, transfer rates, ETA, ratio, and a live event log

## Seeding and reachability

Peer connections are bidirectional, so the client uploads both to peers that
connect to it and to peers it connects to. Once a download finishes it keeps
running and seeds until you quit or a limit is reached.

There is **no UPnP or NAT-PMP**, so if you are behind NAT, peers cannot open
connections to you unless you forward the port yourself:

    go-torrent -port 6881

Without a forwarded port you will still upload on connections you initiate,
just to fewer peers.

## Encryption

Some networks — campuses and workplaces especially — reset any connection that
looks like BitTorrent. They spot it from the first bytes of a plain connection,
which are always the same.

The client supports **Message Stream Encryption** (MSE, also called protocol
encryption), the obfuscated handshake mainstream clients use. It is on by
default: connections are encrypted where the peer supports it, which nearly all
do, and fall back to plain BitTorrent otherwise.

    go-torrent -encryption prefer    # default: encrypt, fall back if needed
    go-torrent -encryption require   # never use a plain connection
    go-torrent -encryption off       # plain BitTorrent only

If downloads work elsewhere but not on a particular network, try `require`.

MSE hides BitTorrent from filters; it is **not** privacy. The key exchange is
unauthenticated, and your IP address is visible to every peer and tracker as
usual.

## Release notes

See [RELEASE_NOTES.md](RELEASE_NOTES.md) for what changed in each release.

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

    -output      Output directory (default: current directory)
    -port        TCP port to listen on for incoming peers (default 6881)
    -max-peers   Maximum simultaneous peer connections (default 50)
    -seed-ratio  Stop seeding at this upload/download ratio (0 = unlimited)
    -seed-time   Stop seeding after this long, e.g. 2h (0 = unlimited)
    -encryption  prefer, require, or off (default prefer)

## Development

    go build ./...
    go vet ./...
    go test -race ./...

The race detector matters here: our bitfield is shared between the download loop
and every peer goroutine.

Network tests are skipped unless you opt in:

    GOTORRENT_LIVE=1 go test -race ./internal/engine/ -v -timeout 10m

The seeding tests need no network — they run a seeder and a leecher against each
other over loopback, under every combination of encryption settings.

To check whether a network is blocking BitTorrent, compare plain and encrypted
handshakes against real peers:

    GOTORRENT_LIVE=1 go test ./internal/engine/ -run TestLiveHandshakes -v

If plain handshakes succeed for nobody but encrypted ones do, the network is
filtering BitTorrent and `-encryption require` is the setting to use.

## Not implemented

UPnP/NAT-PMP port mapping, DHT `announce_peer` (so DHT users cannot discover
you; trackers can), PEX, rarest-first and endgame piece selection, superseeding,
the fast extension, rate limiting, multiple simultaneous torrents, IPv6, and
base32 magnet links.

The DHT and UDP trackers are not covered by encryption, since MSE only applies
to peer connections. A network that blocks those will still limit peer
discovery; trackers over HTTPS are unaffected.

## License

MIT. See [LICENSE](LICENSE).
