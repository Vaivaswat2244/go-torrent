# go-torrent

A BitTorrent client written in Go with a terminal UI.

## Features
- Magnet link support via DHT + UDP trackers
- .torrent file support  
- Terminal UI with progress bar
- Multi-file torrent support
- Resume downloads

## Install

Download the binary for your platform from [Releases](https://github.com/Vaivaswat2244/go-torrent/releases).

Or build from source:
    go install github.com/Vaivaswat2244/go-torrent/cmd/client@latest

## Usage

    go-torrent [-output <dir>]

Then choose torrent file or magnet link from the menu.

## Options

    -output   Output directory (default: current directory)