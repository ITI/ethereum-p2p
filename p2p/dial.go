// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"container/heap"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/teamnsrg/go-ethereum/log"
	"github.com/teamnsrg/go-ethereum/p2p/discover"
	"github.com/teamnsrg/go-ethereum/p2p/netutil"
)

const (
	// This is the amount of time spent waiting in between
	// redialing a certain node.
	dialHistoryExpiration = 30 * time.Second
)

// NodeDialer is used to connect to nodes in the network, typically by using
// an underlying net.Dialer but also using net.Pipe in tests
type NodeDialer interface {
	Dial(*discover.Node) (net.Conn, error)
}

// TCPDialer implements the NodeDialer interface by using a net.Dialer to
// create TCP connections to nodes in the network
type TCPDialer struct {
	*net.Dialer
}

// Dial creates a TCP connection to the node
func (t TCPDialer) Dial(dest *discover.Node) (net.Conn, error) {
	addr := &net.TCPAddr{IP: dest.IP, Port: int(dest.TCP)}
	return t.Dialer.Dial("tcp", addr.String())
}

// dialstate schedules dials and discovery lookups.
// it get's a chance to compute new tasks on every iteration
// of the main loop in Server.run.
type dialstate struct {
	selfId      discover.NodeID
	netrestrict *netutil.Netlist

	dialing map[discover.NodeID]connFlag
	static  map[discover.NodeID]*dialTask
	hist    *dialHistory

	start     time.Time        // time when the dialer was first used
	bootnodes []*discover.Node // default dials when there are no peers
}

type discoverTable interface {
	Self() *discover.Node
	Close()
	Resolve(target discover.NodeID) *discover.Node
	Lookup(target discover.NodeID) []*discover.Node
	ReadRandomNodes([]*discover.Node) int
}

// the dial history remembers recent dials.
type dialHistory []pastDial

// pastDial is an entry in the dial history.
type pastDial struct {
	id  discover.NodeID
	exp time.Time
}

type task interface {
	Do(*Server)
}

// A dialTask is generated for each node that is dialed. Its
// fields cannot be accessed while the task is running.
type dialTask struct {
	flags        connFlag
	dest         *discover.Node
	lastResolved time.Time
	resolveDelay time.Duration
}

// A waitExpireTask is generated if there are no other tasks
// to keep the loop in Server.run ticking.
type waitExpireTask struct {
	time.Duration
}

func newDialState(static []*discover.Node, selfId discover.NodeID, netrestrict *netutil.Netlist) *dialstate {
	s := &dialstate{
		selfId:      selfId,
		netrestrict: netrestrict,
		static:      make(map[discover.NodeID]*dialTask),
		dialing:     make(map[discover.NodeID]connFlag),
		hist:        new(dialHistory),
	}
	for _, n := range static {
		s.addStatic(n)
	}
	return s
}

func (s *dialstate) addStatic(n *discover.Node) *dialTask {
	// This overwites the task instead of updating an existing
	// entry, giving users the opportunity to force a resolve operation.
	t := &dialTask{flags: staticDialedConn, dest: n}
	s.static[n.ID] = t
	return t
}

func (s *dialstate) removeStatic(n *discover.Node) {
	// This removes a task so future attempts to connect will not be made.
	delete(s.static, n.ID)
}

func (s *dialstate) newTasks(nRunning int, peers map[discover.NodeID]*Peer, now time.Time) []task {
	if s.start == (time.Time{}) {
		s.start = now
	}

	var newtasks []task

	// Expire the dial history on every invocation.
	s.hist.expire(now)

	// Create dials for static nodes if they are not connected.
	for id, t := range s.static {
		err := s.checkDial(t.dest, peers)
		switch err {
		case errNotWhitelisted, errSelf:
			log.Warn("Removing static dial candidate", "id", t.dest.ID, "addr", &net.TCPAddr{IP: t.dest.IP, Port: int(t.dest.TCP)}, "err", err)
			delete(s.static, t.dest.ID)
		case nil:
			s.dialing[id] = t.flags
			newtasks = append(newtasks, t)
		}
	}

	// Launch a timer to wait for the next node to expire if all
	// candidates have been tried and no task is currently active.
	// This should prevent cases where the dialer logic is not ticked
	// because there are no pending events.
	if nRunning == 0 && len(newtasks) == 0 && s.hist.Len() > 0 {
		t := &waitExpireTask{s.hist.min().exp.Sub(now)}
		newtasks = append(newtasks, t)
	}
	return newtasks
}

var (
	errSelf             = errors.New("is self")
	errAlreadyDialing   = errors.New("already dialing")
	errAlreadyConnected = errors.New("already connected")
	errRecentlyDialed   = errors.New("recently dialed")
	errNotWhitelisted   = errors.New("not contained in netrestrict whitelist")
)

func (s *dialstate) checkDial(n *discover.Node, peers map[discover.NodeID]*Peer) error {
	_, dialing := s.dialing[n.ID]
	switch {
	case dialing:
		return errAlreadyDialing
	case peers[n.ID] != nil:
		return errAlreadyConnected
	case n.ID == s.selfId:
		return errSelf
	case s.netrestrict != nil && !s.netrestrict.Contains(n.IP):
		return errNotWhitelisted
	case s.hist.contains(n.ID):
		return errRecentlyDialed
	}
	return nil
}

func (s *dialstate) taskDone(t task, now time.Time) {
	switch t := t.(type) {
	case *dialTask:
		s.hist.add(t.dest.ID, now.Add(dialHistoryExpiration))
		delete(s.dialing, t.dest.ID)
	}
}

func (t *dialTask) Do(srv *Server) {
	t.dial(srv, t.dest)
}

// dial performs the actual connection attempt.
func (t *dialTask) dial(srv *Server, dest *discover.Node) bool {
	fd, err := srv.Dialer.Dial(dest)
	if err != nil {
		log.Trace("Dial error", "task", t, "err", err)
		return false
	}
	mfd := newMeteredConn(fd, false)
	srv.SetupConn(mfd, t.flags, dest)
	return true
}

func (t *dialTask) String() string {
	return fmt.Sprintf("%v %x %v:%d", t.flags, t.dest.ID[:8], t.dest.IP, t.dest.TCP)
}

func (t waitExpireTask) Do(*Server) {
	time.Sleep(t.Duration)
}
func (t waitExpireTask) String() string {
	return fmt.Sprintf("wait for dial hist expire (%v)", t.Duration)
}

// Use only these methods to access or modify dialHistory.
func (h dialHistory) min() pastDial {
	return h[0]
}
func (h *dialHistory) add(id discover.NodeID, exp time.Time) {
	heap.Push(h, pastDial{id, exp})
}
func (h dialHistory) contains(id discover.NodeID) bool {
	for _, v := range h {
		if v.id == id {
			return true
		}
	}
	return false
}
func (h *dialHistory) expire(now time.Time) {
	for h.Len() > 0 && h.min().exp.Before(now) {
		heap.Pop(h)
	}
}

// heap.Interface boilerplate
func (h dialHistory) Len() int           { return len(h) }
func (h dialHistory) Less(i, j int) bool { return h[i].exp.Before(h[j].exp) }
func (h dialHistory) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dialHistory) Push(x interface{}) {
	*h = append(*h, x.(pastDial))
}
func (h *dialHistory) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
