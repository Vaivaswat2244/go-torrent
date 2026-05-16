# Go-Torrent

A high-performance, concurrent BitTorrent client built from scratch in Go. 

This project is a fully functional P2P Leech Engine that implements the core BitTorrent protocol (BEP-3) and UDP Tracker protocol (BEP-15) entirely from the ground up, without relying on third-party torrenting libraries.

## 🚀 Features

* **Custom Bencode Parser:** A robust, zero-dependency parser for decoding `.torrent` files and tracker responses.
* **Tracker Support:** Fully implements both HTTP/HTTPS and UDP tracker protocols.
* **Concurrent Worker Pool:** Uses Go channels and goroutines to manage a dynamic worker pool, downloading multiple pieces from dozens of peers simultaneously.
* **Multi-File Support:** Flawlessly handles complex directory structures, calculating global byte offsets to slice and distribute pieces across multiple files on disk.
* **State Resumption:** Safely pause and resume downloads. The engine hashes existing files on disk and automatically picks up exactly where it left off.
* **Pipelining & State Management:** Implements the official BitTorrent Peer Wire Protocol (Handshakes, Choke/Unchoke, Bitfields, and Interested states) with high-speed request pipelining.
* **Daemon Architecture:** A decoupled, stateful engine design ready to be hooked up to a Web UI, Desktop GUI, or rich Terminal UI.

## 📁 Project Structure

The codebase strictly follows standard Go project layout conventions:

* `/cmd/client/` - The entry point of the application (the CLI daemon).
* `/internal/bencode/` - Custom bencode decoding logic.
* `/internal/engine/` - The stateful orchestrator that manages torrent sessions, disk I/O, and UI statistics.
* `/internal/p2p/` - The core peer-to-peer logic, worker pools, piece hashing, and the `MultiFileWriter`.
* `/internal/peers/` - The Peer Wire Protocol (TCP handshakes, message framing, and state).
* `/internal/torrentfile/` - `.torrent` file parsing and Tracker communication (HTTP & UDP).

## 🛠️ Installation

Ensure you have Go installed (1.18+ recommended). Clone the repository and build the binary:

    git clone [https://github.com/Vaivaswat2244/go-torrent.git](https://github.com/Vaivaswat2244/go-torrent.git)
    cd go-torrent
    go build -o bin/torrent ./cmd/client

## 💻 Usage

Run the compiled binary and point it to a valid `.torrent` file.

Download to the current directory:

    ./bin/torrent -torrent path/to/debian.torrent

Download to a specific output directory:

    ./bin/torrent -torrent path/to/ubuntu.torrent -output /Downloads/ISOs/

### Stopping & Resuming
You can safely stop the daemon at any time using `Ctrl+C`. When you restart the command, `go-torrent` will scan your output directory, verify the SHA-1 hashes of the existing pieces, and resume downloading only the missing data.

## 🗺️ Roadmap / Future Enhancements

While the client is a highly capable downloader, there are several exciting protocol enhancements planned for the future:

- [ ] **Seeding (Upload Engine):** Opening a TCP listener to serve pieces back to the swarm (Tit-for-Tat compliance).
- [ ] **DHT (Distributed Hash Table):** Implementing BEP-5 for trackerless peer discovery.
- [ ] **Magnet Links:** Supporting metadata extension (BEP-9) to download torrents without `.torrent` files.
- [ ] **Rich Frontend:** Replacing the standard CLI output with a Web UI (WebSockets/React) or a Rich Terminal UI (Bubbletea).

## 📄 License

This project is open-source and available under the MIT License.