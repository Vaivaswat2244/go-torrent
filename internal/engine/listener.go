package engine

import (
	"fmt"
	"net"

	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
	"github.com/Vaivaswat2244/go-torrent/internal/peers"
)

// listen binds the peer port and starts accepting incoming connections.
//
// A bind failure is not fatal: the port may simply be in use. We log it and
// carry on, which leaves the client able to download and to upload on
// connections it dials, just not to accept new ones.
func (t *Torrent) listen(peerID [20]byte, port uint16) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.logf("Could not listen on port %d (%v) - inbound peers cannot reach us", port, err)
		return
	}

	t.mu.Lock()
	t.listener = ln
	t.mu.Unlock()

	t.logf("Listening for peers on %s", ln.Addr())

	// Cancellation unblocks Accept.
	go func() {
		<-t.ctx.Done()
		ln.Close()
	}()

	go t.acceptLoop(ln, peerID)
}

// ListenAddr reports the address we accept peers on, or nil if we never bound.
func (t *Torrent) ListenAddr() net.Addr {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.listener == nil {
		return nil
	}
	return t.listener.Addr()
}

func (t *Torrent) acceptLoop(ln net.Listener, peerID [20]byte) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Either we are shutting down or the listener broke; either way
			// there is nothing useful left to accept.
			return
		}

		select {
		case <-t.ctx.Done():
			conn.Close()
			return
		default:
		}

		go t.serveIncoming(conn, peerID)
	}
}

// serveIncoming handshakes a connection a peer opened to us and runs a session.
func (t *Torrent) serveIncoming(conn net.Conn, peerID [20]byte) {
	// Refuse early if we are already at the peer limit, before spending a
	// handshake on it.
	if t.swarm.count() >= t.maxPeers {
		conn.Close()
		return
	}

	// The inbound order is read-then-write: we cannot answer until the peer
	// tells us which torrent they want.
	client, err := peers.AcceptHandshake(conn, peerID, func(infoHash [20]byte) bool {
		return infoHash == t.TF.InfoHash
	})
	if err != nil {
		conn.Close()
		return
	}

	session := p2p.NewSession(
		t.ctx, client, t.TF, t.bitfield, t.Writer,
		t.workQueue, t.results, t.logf,
	)

	session.SetOnInterest(t.swarm.requestChokeRound)

	if !t.swarm.add(session) {
		conn.Close()
		return
	}
	defer t.swarm.remove(session)

	t.sessions.Add(1)
	defer t.sessions.Done()

	t.logf("Peer %s connected to us", session.Addr())
	session.Run()
}
