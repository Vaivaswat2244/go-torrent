package p2p

import (
	"context"
	"net"
	"time"

	"github.com/Vaivaswat2244/go-torrent/internal/peers"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

// Worker connects to a single peer and pulls pieces off the shared work queue
// until the queue is exhausted, the peer proves useless, or ctx is cancelled.
func Worker(
	ctx context.Context,
	peer torrentfile.Peer,
	tf *torrentfile.TorrentFile,
	peerID [20]byte,
	ourBitfield *SafeBitfield,
	workQueue chan *PieceWork,
	results chan *PieceResult,
) {
	numPieces := len(tf.PieceHashes)

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", peer.String())
	if err != nil {
		return
	}
	defer conn.Close()

	// Cancelling the context unblocks any in-flight read or write.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	client, err := peers.CompleteHandshake(conn, tf.InfoHash, peerID)
	if err != nil {
		return
	}

	// Allocate the peer's bitfield up front so Have messages can be recorded
	// even when they arrive before (or instead of) a bitfield message.
	client.Bitfield = NewBitfield(numPieces)
	peerHas := Bitfield(client.Bitfield)

	// Snapshot rather than sharing our live bitfield with this goroutine.
	client.SendBitfield(ourBitfield.Snapshot())

	// Read until we get a Bitfield or give up after a few messages
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 10; i++ {
		msg, err := client.ReadMessage()
		if err != nil {
			break
		}
		if msg == nil {
			continue
		}
		if msg.ID == peers.MsgBitfield {
			// Keep our correctly sized slice; copy in what the peer sent.
			copy(peerHas, msg.Payload)
			break
		}
		if msg.ID == peers.MsgHave {
			if index, err := peers.ParseHave(msg); err == nil {
				peerHas.SetPiece(index)
			}
		}
	}
	conn.SetReadDeadline(time.Time{})

	if err := client.SendInterested(); err != nil {
		return
	}

	// Wait for unchoke
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for client.Choked {
		msg, err := client.ReadMessage()
		if err != nil {
			return
		}
		if msg == nil {
			continue
		}
		switch msg.ID {
		case peers.MsgUnchoke:
			client.Choked = false
		case peers.MsgChoke:
			return
		case peers.MsgHave:
			if index, err := peers.ParseHave(msg); err == nil {
				peerHas.SetPiece(index)
			}
		}
	}
	conn.SetReadDeadline(time.Time{})

	// misses counts consecutive pieces this peer could not supply. Once we have
	// cycled the whole queue without a match, the peer has nothing we need.
	// The previous code slept 1ms and retried forever, spinning the queue at
	// roughly a thousand rotations a second against a single peer.
	misses := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var work *PieceWork
		select {
		case <-ctx.Done():
			return
		case w, ok := <-workQueue:
			if !ok {
				return
			}
			work = w
		}

		if !peerHas.HasPiece(work.Index) {
			if !requeue(ctx, workQueue, work) {
				return
			}
			misses++
			if misses > len(workQueue)+1 {
				return
			}
			continue
		}
		misses = 0

		buf, err := work.Download(client)
		if err != nil {
			requeue(ctx, workQueue, work)
			return
		}

		if err := work.CheckIntegrity(buf); err != nil {
			requeue(ctx, workQueue, work)
			return
		}

		select {
		case <-ctx.Done():
			return
		case results <- &PieceResult{Index: work.Index, Buf: buf}:
		}
	}
}

// requeue returns a piece to the queue. The queue is never closed (a worker
// racing a close would panic on send), so cancellation is the only way out.
func requeue(ctx context.Context, workQueue chan *PieceWork, work *PieceWork) bool {
	select {
	case workQueue <- work:
		return true
	case <-ctx.Done():
		return false
	}
}
