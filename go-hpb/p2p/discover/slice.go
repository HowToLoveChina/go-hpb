// Copyright 2018 The go-hpb Authors
// This file is part of the go-hpb.
//
// The go-hpb is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-hpb is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-hpb. If not, see <http://www.gnu.org/licenses/>.

// slice.go implements the CommNode and PreCommNode keep-live Protocol.
package discover

import(
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hpb-project/go-hpb/log"
)

const (
	maxConcurrencyPingPongs = 16
	pingInerval             = 10 * time.Second
)

type Slice struct {
	mutex     sync.Mutex    // Mutex for members
	members   []*Node
	roleType  uint8         // Slice's nodes type
	bondSlots chan struct{} // limits total number of active bonding processes
	bondmu    sync.Mutex
	bonding   map[NodeID]*bondproc
	db        *nodeDB
	net       transport
	self      *Node

	refreshReq chan chan struct{}
	closeReq   chan struct{}
	closed     chan struct{}
}

func (sl *Slice) Self() *Node {
	return sl.self
}

func (sl *Slice) Close() {
	select {
	case <-sl.closed:
		// already closed.
	case sl.closeReq <- struct{}{}:
		<-sl.closed // wait for keepLiveLoop to end.
	}
}

func (sl *Slice) Fetch() []*Node {
	sl.mutex.Lock()
	defer sl.mutex.Unlock()
	var slice []*Node
	for _, m := range sl.members {
		slice = append(slice, m)
	}

	return slice
}

func newSlice (t transport, ourID NodeID, ourRole uint8, roleType uint8, ourAddr *net.UDPAddr, initNodes []*Node, orgnode *Node, db *nodeDB) (*Slice, error) {
	slice := &Slice{
		net:        t,
		bondSlots:  make(chan struct{}, maxConcurrencyPingPongs),
		bonding:    make(map[NodeID]*bondproc),
		db:         db,
		roleType:   roleType,
		self:       NewNode(ourID, ourRole, ourAddr.IP, uint16(ourAddr.Port), uint16(ourAddr.Port)),

		refreshReq: make(chan chan struct{}),
		closeReq:   make(chan struct{}),
		closed:     make(chan struct{}),
	}

	for i := 0; i < cap(slice.bondSlots); i++ {
		slice.bondSlots <- struct{}{}
	}

	if 0 == len(initNodes) {
		// TODO by xujl: 传入slice为空，则从orgnode拉取，如果再失败则从本地db加载
		go slice.pullSlice(orgnode)
		slice.loadFromDB(db)
	}

	for _, n := range initNodes {
		if err := n.validateComplete(); err != nil {
			return nil, fmt.Errorf("bad slice node %q (%v)", n, err)
		}
		slice.members = append(slice.members, n)
	}

	go slice.keepLiveLoop()

	return slice, nil
}

// Guaranteed keepLive function scheduling.
func (sl *Slice) keepLiveLoop()  {
	var (
		timer   = time.NewTicker(pingInerval)
		waiting []chan struct{} // accumulates waiting callers while keepLiveLoop runs
		done    chan struct{}   // where keepLiveLoop reports completion
	)
loop:
	for {
		select {
		case <-timer.C:
			if done == nil {
				done = make(chan struct{})
				go sl.keepLive(done)
			}
		case req := <-sl.refreshReq:
			waiting = append(waiting, req)
			if done == nil {
				done = make(chan struct{})
				go sl.keepLive(done)
			}
		case <-done:
			for _, ch := range waiting {
				close(ch)
			}
			waiting = nil
			done = nil
		case <-sl.closeReq:
			break loop
		}
	}

	if sl.net != nil {
		sl.net.close()
	}
	if done != nil {
		<-done
	}
	for _, ch := range waiting {
		close(ch)
	}
	sl.db.close()
	close(sl.closed)
}

func (sl *Slice) refresh() <-chan struct{} {
	done := make(chan struct{})
	select {
	case sl.refreshReq <- done:
	case <-sl.closed:
		close(done)
	}
	return done
}

func (sl *Slice) keepLive(done chan struct{}) {
	defer close(done)

	sl.mutex.Lock()
	defer sl.mutex.Unlock()

	rc := make(chan *Node, len(sl.members))
	for _, n := range sl.members {
		go func(node * Node) {
			nn, _ := sl.test(false, n.ID, n.Role, n.addr(), uint16(n.TCP))
			rc <- nn
		} (n)
	}

	var sucMem []*Node

	for range sl.members {
		if node := <-rc; node != nil {
			if node != nil {
				//only pingPong success node be retained
				sucMem = append(sucMem, node)
			}
		}
	}

	sl.members = sucMem
}

func (sl *Slice) test(pinged bool, id NodeID, role uint8, addr *net.UDPAddr, tcpPort uint16) (*Node, error) {
	// This is unlikely to happen.
	if id == sl.self.ID {
		return nil, errors.New("is self")
	}

	var result error
	log.Trace("Starting bonding ping/pong", "id", id)

	sl.bondmu.Lock()
	w := sl.bonding[id]
	if w != nil {
		// Wait for an existing bonding process to complete.
		sl.bondmu.Unlock()
		<-w.done
	} else {
		// Register a new bonding process.
		w = &bondproc{done: make(chan struct{})}
		sl.bonding[id] = w
		sl.bondmu.Unlock()
		// Do the ping/pong. The result goes into w.
		sl.pingPong(w, pinged, id, role, addr, tcpPort)
		// Unregister the process after it's done.
		sl.bondmu.Lock()
		delete(sl.bonding, id)
		sl.bondmu.Unlock()
	}
	// Retrieve the bonding results
	result = w.err
	if result != nil {
		return nil, result
	}
	node := w.n
	if node != nil {
		sl.db.updateLastPong(id, nodeDBCommitteePong, time.Now())
	}
	return node, result
}

func (sl *Slice) pingPong(w *bondproc, pinged bool, id NodeID, role uint8, addr *net.UDPAddr, tcpPort uint16) {
	// bondSlots to limit network usage
	<-sl.bondSlots
	defer func() { sl.bondSlots <- struct{}{} }()

	// Ping the remote side and wait for a pong
	if w.err = sl.ping(id, role, addr); w.err != nil {
		close(w.done)
		return
	}
	if !pinged {
		// Give the remote node a chance to ping us before we start
		// sending findnode requests. If they still remember us,
		// waitping will simply time out.
		sl.net.waitping(id, role, sl.roleType)
	}
	// Bonding succeeded, update the node database.
	w.n = NewNode(id, role, addr.IP, uint16(addr.Port), tcpPort)
	sl.db.updateNode(w.n, nodeDBCommitteeRoot)
	close(w.done)
}

func (sl *Slice) ping(id NodeID, role uint8, addr *net.UDPAddr) error {
	sl.db.updateLastPing(id, nodeDBCommitteePing, time.Now())
	if err := sl.net.ping(id, role, sl.roleType, addr); err != nil {
		return err
	}
	sl.db.updateLastPong(id, nodeDBCommitteePong, time.Now())

	// TODO by xujl: Whether to reuse KAD DB timeout Mechanism
	sl.db.ensureExpirer(nodeDBCommitteeRoot, nodeDBCommitteePong)
	return nil
}

func (sl *Slice) loadFromDB(db *nodeDB) {

}

func (sl *Slice) pullSlice(node *Node) {

}