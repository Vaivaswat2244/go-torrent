# Release notes

Notable changes per release. For install and usage, see the [README](README.md).

## Unreleased

### Added

- **Protocol encryption (MSE).** Peer connections are now encrypted by
  default, so networks that reset BitTorrent traffic by recognising its
  handshake can no longer do so. Nearly every client supports it; for those
  that don't, the client falls back to a plain connection. Choose with
  `-encryption prefer|require|off`. This covers magnet metadata fetches too,
  which otherwise failed on filtering networks before a download could start.

  Verified against an independent implementation in both directions, and
  against real swarms, where peers accepted encrypted handshakes at the same
  rate as plain ones.

### Fixed

- Downloads could stall permanently just short of 100%. Several separate
  causes, all in the peer session code added in v0.2.0:
  - A peer that disconnected mid-piece took the piece with it, so it was never
    downloaded.
  - A peer that choked us kept its piece claimed, blocking everyone else.
  - Returning a piece could silently drop it when the connection was closing.
  - Idle connections only looked for work when a message arrived, so a piece
    handed back later could sit unclaimed indefinitely.
  - A peer that unchoked us and then stopped sending held its piece forever.
    Pieces are now reassigned after 30 seconds without data.
- A late block for a piece we had already given up no longer drops an
  otherwise healthy connection.
- The stall message now says how many remaining pieces are queued and how many
  are held by peers, which is most of what diagnosing a stall needs.

## v0.2.0 — 2026-08-24

Seeding support, plus a round of security and reliability fixes.

### Security

Both of these were present in v0.1.0. Anyone still running that build should upgrade.

- **Remotely triggerable crash.** A negative string length in a bencode value
  panicked the decoder. This was reachable from the DHT reader, which parses
  every UDP packet that arrives and had no recovery, so any host on the internet
  could kill the process with a single malformed packet.
- **Path traversal.** File paths inside a `.torrent` were used unvalidated, so a
  crafted torrent could write outside the download directory.
- Peer-supplied message lengths and metadata sizes are now bounded. A single
  4-byte header could previously make the client allocate up to 4 GiB.
- Bencode nesting depth is capped; deeply nested input previously recursed until
  the stack was exhausted.

### Added

The client now uploads. Peer connections are bidirectional, so it serves data
both to peers that connect to it and to peers it connects to. A finished
download stays running and seeds instead of shutting down.

- Choke algorithm: four unchoke slots recomputed every 10s, ranked by
  reciprocation while downloading and by upload rate while seeding, plus an
  optimistic unchoke every 30s
- Incoming peer connections, with requests validated before they reach the disk
- `Have` messages are broadcast to connected peers as pieces complete
- New flags: `-port`, `-max-peers`, `-seed-ratio`, `-seed-time`
- The interface shows upload rate, uploaded total, ratio, and how many peers are
  being served

There is no UPnP or NAT-PMP yet, so behind a router you need to forward the
listen port for peers to reach you. Uploading on connections the client itself
opens works either way.

### Fixed

- Downloads could hang at 99% forever: a failed disk write was silently dropped
  and the piece never retried. There was also no stall detection.
- Every HTTP tracker in a torrent was contacted over UDP, so all of them timed
  out. For magnet links this was the entire tracker path.
- Trackers were announced to exactly once per session; the re-announce interval
  they returned was discarded, so the peer supply went stale on long downloads.
- `Have` messages corrupted the peer's bitfield, causing requests for pieces the
  peer did not have.
- Pasted magnet links were truncated at 512 characters. The info hash survived,
  so downloads still started, but the tracker list was silently discarded.
- A data race on the piece bitfield, shared between the download loop and every
  peer goroutine.
- A crash that fired at the moment a download completed, when a worker returned
  a piece to a queue that had just been closed.
- `q` and `esc` did nothing on the download screen despite being advertised.
- Progress messages were printed to stdout while the terminal UI was active,
  corrupting the display.
- Resume verification ran on the UI thread, freezing the interface while it
  re-hashed existing data.

### Changed

- Peer connections are capped, 50 by default. The client previously connected to
  every peer a tracker returned.
- Piece storage uses positional reads and writes, so concurrent access cannot
  interleave and return the wrong bytes.
- The peer ID is now randomised per run; it was a fixed string, so every
  instance presented an identical identity to trackers and peers.
- Public trackers are no longer injected into torrents that did not list them.
- Tracker announces report real downloaded, uploaded and remaining figures, and
  send `started`, `completed` and `stopped` events.

### Removed

- The `anacrolix/torrent` dependency. The only remaining dependencies are the
  terminal UI libraries.

### Testing

The project had no tests before this release. It now has unit tests, a fuzz
target for the bencode decoder, regression tests for the concurrency bugs above,
and an end-to-end test that runs a seeder and a leecher against each other over
loopback. Tests that require network access are skipped unless `GOTORRENT_LIVE=1`
is set.

## v0.1.0 — 2026-05-30

Initial release: `.torrent` and magnet support, DHT and tracker peer discovery,
multi-file downloads, resume, and a terminal UI.

**Withdrawn.** This build contains the security issues listed under v0.2.0.
Use v0.2.0 or later.
