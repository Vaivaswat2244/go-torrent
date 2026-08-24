package engine

import (
	"math/rand"
	"sort"
	"sync"

	"github.com/Vaivaswat2244/go-torrent/internal/p2p"
)

const (
	// unchokeSlots is how many peers we serve at once. Four is the figure from
	// the original BitTorrent choke algorithm and is still what most clients
	// default to.
	unchokeSlots = 4

	// chokeInterval is how often the unchoke set is recomputed. Rechoking
	// faster than this lets peers game the algorithm.
	chokeInterval = 10

	// optimisticEvery is how many choke rounds pass between optimistic
	// unchokes, i.e. every 30s. Optimistic unchoking gives newcomers with
	// nothing to trade a way in, and lets us discover faster partners.
	optimisticEvery = 3
)

// swarm is the set of live peer sessions for a torrent, plus the choker.
type swarm struct {
	mu       sync.Mutex
	sessions map[string]*p2p.Session
	maxPeers int

	// prev holds the previous round's counters so the choker can work in rates
	// rather than totals.
	prev map[string]p2p.SessionStats

	// retiredUp carries the upload total of sessions that have since closed,
	// so the tracker figure does not go backwards as peers come and go.
	retiredUp int64

	round int

	// nudge asks for an out-of-band choke round when a peer becomes interested
	// and we have a slot going spare.
	nudge chan struct{}
}

func newSwarm(maxPeers int) *swarm {
	return &swarm{
		sessions: make(map[string]*p2p.Session),
		prev:     make(map[string]p2p.SessionStats),
		maxPeers: maxPeers,
		nudge:    make(chan struct{}, 1),
	}
}

// add registers a session. It reports false if we are already at the peer limit
// or already connected to that address, in which case the caller should close it.
func (sw *swarm) add(s *p2p.Session) bool {
	addr := s.Addr()
	if addr == "" {
		return false
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if len(sw.sessions) >= sw.maxPeers {
		return false
	}
	if _, dup := sw.sessions[addr]; dup {
		return false
	}

	sw.sessions[addr] = s
	return true
}

func (sw *swarm) remove(s *p2p.Session) {
	addr := s.Addr()

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if existing, ok := sw.sessions[addr]; ok && existing == s {
		sw.retiredUp += s.Stats().Uploaded
		delete(sw.sessions, addr)
		delete(sw.prev, addr)
	}
}

func (sw *swarm) count() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return len(sw.sessions)
}

// has reports whether we already hold a session for an address, so we do not
// dial a peer that dialed us.
func (sw *swarm) has(addr string) bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_, ok := sw.sessions[addr]
	return ok
}

// uploaded is the torrent's total uploaded bytes, live plus retired.
func (sw *swarm) uploaded() int64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	total := sw.retiredUp
	for _, s := range sw.sessions {
		total += s.Stats().Uploaded
	}
	return total
}

// unchokedCount reports how many peers we are currently serving.
func (sw *swarm) unchokedCount() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	n := 0
	for _, s := range sw.sessions {
		if !s.Stats().AmChoking {
			n++
		}
	}
	return n
}

// requestChokeRound asks the choke loop to run early. Never blocks.
//
// The 10-second round timer exists so peers cannot game the algorithm by
// varying their rate, so we only shortcut it when a slot is actually free —
// otherwise a peer would still have to wait its turn.
func (sw *swarm) requestChokeRound() {
	if sw.unchokedCount() >= unchokeSlots {
		return
	}
	select {
	case sw.nudge <- struct{}{}:
	default:
	}
}

// broadcastHave tells every connected peer we acquired a piece.
func (sw *swarm) broadcastHave(index int) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	for _, s := range sw.sessions {
		s.NotifyHave(index)
	}
}

func (sw *swarm) closeAll() {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	for _, s := range sw.sessions {
		s.Close()
	}
}

// chokeRound recomputes the unchoke set.
//
// While downloading we reciprocate: rank interested peers by the rate they are
// giving us and serve the best few. While seeding there is no such signal, so
// we rank by what we are managing to push to each peer and rotate the starting
// point so slower peers still get turns.
func (sw *swarm) chokeRound(seeding bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.round++

	type candidate struct {
		session *p2p.Session
		rate    int64
	}

	var interested []candidate
	var others []*p2p.Session

	for addr, s := range sw.sessions {
		stats := s.Stats()
		prev := sw.prev[addr]
		sw.prev[addr] = stats

		if !stats.PeerInterested {
			others = append(others, s)
			continue
		}

		rate := stats.Downloaded - prev.Downloaded
		if seeding {
			rate = stats.Uploaded - prev.Uploaded
		}
		interested = append(interested, candidate{session: s, rate: rate})
	}

	// Sort by address first so the order is deterministic when rates tie,
	// then by rate. Without the tiebreak, map iteration order would shuffle
	// the unchoke set every round for no reason.
	sort.Slice(interested, func(i, j int) bool {
		if interested[i].rate != interested[j].rate {
			return interested[i].rate > interested[j].rate
		}
		return interested[i].session.Addr() < interested[j].session.Addr()
	})

	// While seeding, rotate the head of the list each round so peers that are
	// all equally idle take turns rather than the same few always winning.
	if seeding && len(interested) > unchokeSlots {
		shift := sw.round % len(interested)
		interested = append(interested[shift:], interested[:shift]...)
	}

	unchoke := make(map[*p2p.Session]bool, unchokeSlots+1)
	for i := 0; i < len(interested) && i < unchokeSlots; i++ {
		unchoke[interested[i].session] = true
	}

	// Optimistic unchoke: every third round, also serve one random interested
	// peer we are currently choking.
	if sw.round%optimisticEvery == 0 {
		var choked []*p2p.Session
		for _, c := range interested {
			if !unchoke[c.session] {
				choked = append(choked, c.session)
			}
		}
		if len(choked) > 0 {
			unchoke[choked[rand.Intn(len(choked))]] = true
		}
	}

	for _, c := range interested {
		c.session.SetChoking(!unchoke[c.session])
	}
	// Peers that want nothing from us stay choked.
	for _, s := range others {
		s.SetChoking(true)
	}
}
