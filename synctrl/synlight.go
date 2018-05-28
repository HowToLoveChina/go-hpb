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

package synctrl

import (
	"crypto/rand"
	"fmt"
	"math"
	"github.com/hpb-project/go-hpb/blockchain"
	"github.com/hpb-project/go-hpb/blockchain/event"
	"github.com/hpb-project/go-hpb/blockchain/types"
	"github.com/hpb-project/go-hpb/common"
	"github.com/hpb-project/go-hpb/common/constant"
	"github.com/hpb-project/go-hpb/common/log"
	hpbinter "github.com/hpb-project/go-hpb/interface"
	"github.com/hpb-project/go-hpb/storage"
	"github.com/rcrowley/go-metrics"
	"math/big"
	"sync"
	"sync/atomic"
	"time"
)

type lightSync struct {
	mux     *event.TypeMux // Event multiplexer to announce sync operation events

	sch     *scheduler   // Scheduler for selecting the hashes to light sync
	peers   *peerSet // Set of active peers from which light sync can proceed
	stateDB hpbdb.Database

	fsPivotLock  *types.Header // Pivot header on critical section entry (cannot change between retries)
	fsPivotFails uint32        // Number of subsequent light sync failures in the critical section

	rttEstimate   uint64 // Round trip time to target for light sync requests
	rttConfidence uint64 // Confidence in the estimated RTT (unit: millionths to allow atomic ops)

	// Statistics
	syncStatsChainOrigin uint64 // Origin block number where syncing started at
	syncStatsChainHeight uint64 // Highest block number known when syncing started
	syncStatsState       stateSyncStats
	syncStatsLock        sync.RWMutex // Lock protecting the sync stats fields

	lightchain LightChain

	// Callbacks
	dropPeer peerDropFn // Drops a peer for misbehaving

	// Status
	synchroniseMock func(id string, hash common.Hash) error // Replacement for synchronise during testing
	synchronising   int32
	notified        int32

	// Channels
	headerCh      chan dataPack        // Channel receiving inbound block headers
	bodyCh        chan dataPack        // Channel receiving inbound block bodies
	receiptCh     chan dataPack        // Channel receiving inbound receipts
	bodyWakeCh    chan bool            // Channel to signal the block body fetcher of new tasks
	receiptWakeCh chan bool            // Channel to signal the receipt fetcher of new tasks
	headerProcCh  chan []*types.Header // Channel to feed the header processor new tasks

	// for stateFetcher
	stateSyncStart chan *stateSync
	trackStateReq  chan *stateReq
	stateCh        chan dataPack // Channel receiving inbound node state data

	// Cancellation and termination
	cancelPeer string        // Identifier of the peer currently being used as the master (cancel on drop)
	cancelCh   chan struct{} // Channel to cancel mid-flight syncs
	cancelLock sync.RWMutex  // Lock to protect the cancel channel and peer in delivers

	quitCh   chan struct{} // Quit channel to signal termination
	quitLock sync.RWMutex  // Lock to prevent double closes

	// Testing hooks
	syncInitHook     func(uint64, uint64)  // Method to call upon initiating a new sync run
	bodyFetchHook    func([]*types.Header) // Method to call upon starting a block body fetch
	receiptFetchHook func([]*types.Header) // Method to call upon starting a receipt fetch
	chainInsertHook  func([]*fetchResult)  // Method to call upon inserting a chain of blocks (possibly in multiple invocations)
}

func newLightsync(stateDb hpbdb.Database, mux *event.TypeMux, lightchain LightChain,
	dropPeer peerDropFn) *lightSync {
	if lightchain == nil {
		lightchain = core.InstanceBlockChain()
	}

	light := &lightSync{
		stateDB:        stateDb,
		mux:            mux,
		sch:          newScheduler(),
		peers:          newPeerSet(),
		rttEstimate:    uint64(rttMaxEstimate),
		rttConfidence:  uint64(1000000),
		lightchain:     lightchain,
		dropPeer:       dropPeer,
		headerCh:       make(chan dataPack, 1),
		bodyCh:         make(chan dataPack, 1),
		receiptCh:      make(chan dataPack, 1),
		bodyWakeCh:     make(chan bool, 1),
		receiptWakeCh:  make(chan bool, 1),
		headerProcCh:   make(chan []*types.Header, 1),
		quitCh:         make(chan struct{}),
		stateCh:        make(chan dataPack),
		stateSyncStart: make(chan *stateSync),
		trackStateReq:  make(chan *stateReq),
	}
	go light.qosTuner()
	go light.stateFetcher()
	return light
}

// DeliverHeaders injects a new batch of block headers received from a remote
// node into the light sync schedule.
func (this *lightSync) deliverHeaders(id string, headers []*types.Header) (err error) {
	return this.deliver(id, this.headerCh, &headerPack{id, headers}, headerInMeter, headerDropMeter)
}

// DeliverBodies injects a new batch of block bodies received from a remote node.
func (this *lightSync) deliverBodies(id string, transactions [][]*types.Transaction, uncles [][]*types.Header) (err error) {
	return this.deliver(id, this.bodyCh, &bodyPack{id, transactions, uncles}, bodyInMeter, bodyDropMeter)
}

// DeliverReceipts injects a new batch of receipts received from a remote node.
func (this *lightSync) deliverReceipts(id string, receipts [][]*types.Receipt) (err error) {
	return this.deliver(id, this.receiptCh, &receiptPack{id, receipts}, receiptInMeter, receiptDropMeter)
}

// DeliverNodeData injects a new batch of node state data received from a remote node.
func (this *lightSync) deliverNodeData(id string, data [][]byte) (err error) {
	return this.deliver(id, this.stateCh, &statePack{id, data}, stateInMeter, stateDropMeter)
}

// Synchronise tries to sync up our local block chain with a remote peer, both
// adding various sanity checks as well as wrapping it with various log entries.
func (this *lightSync) start(id string, head common.Hash, td *big.Int, mode SyncMode) error {
	err := this.syn(id, head, td, mode)
	switch err {
	case nil:
	case errBusy:

	case errTimeout, errBadPeer, errStallingPeer,
		errEmptyHeaderSet, errPeersUnavailable, errProVLowerBase,
		errInvalidAncestor, errInvalidChain:
		log.Warn("Synchronisation failed, dropping peer", "peer", id, "err", err)
		this.dropPeer(id)

	default:
		log.Warn("Synchronisation failed, retrying", "err", err)
	}
	return err
}


// Cancel cancels all of the operations and resets the sch. It returns true
// if the cancel operation was completed.
func (this *lightSync) cancel() {
	// Close the current cancel channel
	this.cancelLock.Lock()
	if this.cancelCh != nil {
		select {
		case <-this.cancelCh:
			// Channel was already closed
		default:
			close(this.cancelCh)
		}
	}
	this.cancelLock.Unlock()
}

// terminate interrupts the light syncer, canceling all pending operations.
// The light syncer cannot be reused after calling Terminate.
func (this *lightSync) terminate() {
	// Close the termination channel (make sure double close is allowed)
	this.quitLock.Lock()
	select {
	case <-this.quitCh:
	default:
		close(this.quitCh)
	}
	this.quitLock.Unlock()

	// Cancel any pending light sync requests
	this.cancel()
}

// progress retrieves the synchronisation boundaries, specifically the origin
// block where synchronisation started at (may have failed/suspended); the block
// or header sync is currently at; and the latest known block which the sync targets.
//
// In addition, during the state light sync phase of light synchronisation the number
// of processed and the total number of known states are also returned. Otherwise
// these are zero.
func (this *lightSync) progress() hpbinter.SyncProgress {
	// Lock the current stats and return the progress
	this.syncStatsLock.RLock()
	defer this.syncStatsLock.RUnlock()

	current := uint64(0)
	switch this.mode {
	case FullSync:
		current = core.InstanceBlockChain().CurrentBlock().NumberU64()
	case FastSync:
		current = core.InstanceBlockChain().CurrentFastBlock().NumberU64()
	case LightSync:
		current = this.lightchain.CurrentHeader().Number.Uint64()
	}
	return hpbinter.SyncProgress{
		StartingBlock: this.syncStatsChainOrigin,
		CurrentBlock:  current,
		HighestBlock:  this.syncStatsChainHeight,
		PulledStates:  this.syncStatsState.processed,
		KnownStates:   this.syncStatsState.processed + this.syncStatsState.pending,
	}
}

// syning returns whether the light syncer is currently retrieving blocks.
func (this *lightSync) syning() bool {
	return atomic.LoadInt32(&this.synchronising) > 0
}

// registerPeer injects a new light sync peer into the set of block source to be
// used for fetching hashes and blocks from.
func (this *lightSync) registerPeer(id string, version uint, peer Peer) error {

	logger := log.New("peer", id)
	logger.Trace("Registering sync peer")
	if err := this.peers.Register(newPeerConnection(id, version, peer, logger)); err != nil {
		logger.Error("Failed to register sync peer", "err", err)
		return err
	}
	this.qosReduceConfidence()

	return nil
}

// registerLightPeer injects a light client peer, wrapping it so it appears as a regular peer.
func (this *lightSync) registerLightPeer(id string, version uint, peer LightPeer) error {
	return this.registerPeer(id, version, &lightPeerWrapper{peer})
}

// unregisterPeer remove a peer from the known list, preventing any action from
// the specified peer. An effort is also made to return any pending fetches into
// the sch.
func (this *lightSync) unregisterPeer(id string) error {
	// Unregister the peer from the active peer set and revoke any fetch tasks
	logger := log.New("peer", id)
	logger.Trace("Unregistering sync peer")
	if err := this.peers.Unregister(id); err != nil {
		logger.Error("Failed to unregister sync peer", "err", err)
		return err
	}
	this.sch.Revoke(id)

	// If this peer was the master peer, abort sync immediately
	this.cancelLock.RLock()
	master := id == this.cancelPeer
	this.cancelLock.RUnlock()

	if master {
		this.cancel()
	}
	return nil
}

// syn will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if it's TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
func (this *lightSync) syn(id string, hash common.Hash, td *big.Int, mode SyncMode) error {
	// Mock out the synchronisation if testing
	if this.synchroniseMock != nil {
		return this.synchroniseMock(id, hash)
	}
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&this.synchronising, 0, 1) {
		return errBusy
	}
	defer atomic.StoreInt32(&this.synchronising, 0)

	// Post a user notification of the sync (only once per session)
	if atomic.CompareAndSwapInt32(&this.notified, 0, 1) {
		log.Info("Block synchronisation started")
	}
	// Reset the sch, peer set and wake channels to clean any internal leftover state
	this.sch.Reset()
	this.peers.Reset()

	for _, ch := range []chan bool{this.bodyWakeCh, this.receiptWakeCh} {
		select {
		case <-ch:
		default:
		}
	}
	for _, ch := range []chan dataPack{this.headerCh, this.bodyCh, this.receiptCh} {
		for empty := false; !empty; {
			select {
			case <-ch:
			default:
				empty = true
			}
		}
	}
	for empty := false; !empty; {
		select {
		case <-this.headerProcCh:
		default:
			empty = true
		}
	}
	// Create cancel channel for aborting mid-flight and mark the master peer
	this.cancelLock.Lock()
	this.cancelCh = make(chan struct{})
	this.cancelPeer = id
	this.cancelLock.Unlock()

	defer this.cancel() // No matter what, we can't leave the cancel channel open

	// Set the requested sync mode, unless it's forbidden
	this.mode = mode
	if this.mode == FastSync && atomic.LoadUint32(&this.fsPivotFails) >= fsCriticalTrials {
		this.mode = FullSync
	}
	// Retrieve the origin peer and initiate the light syncing process
	p := this.peers.Peer(id)
	if p == nil {
		return errUnknownPeer
	}
	return this.syncWithPeer(p, hash, td)
}

// syncWithPeer starts a block synchronization based on the hash chain from the
// specified peer and head hash.
func (this *lightSync) syncWithPeer(p *peerConnection, hash common.Hash, td *big.Int) (err error) {
	this.mux.Post(StartEvent{})
	defer func() {
		// reset on error
		if err != nil {
			this.mux.Post(FailedEvent{err})
		} else {
			this.mux.Post(DoneEvent{})
		}
	}()
	if p.version < params.ProtocolV111  {
		return errProVLowerBase
	}

	log.Debug("Synchronising with the network", "peer", p.id, "hpb", p.version, "head", hash, "td", td, "mode", this.mode)
	defer func(start time.Time) {
		log.Debug("Synchronisation terminated", "elapsed", time.Since(start))
	}(time.Now())

	// Look up the sync boundaries: the common ancestor and the target block
	latest, err := this.fetchHeight(p)
	if err != nil {
		return err
	}
	height := latest.Number.Uint64()

	origin, err := this.findAncestor(p, height)
	if err != nil {
		return err
	}
	this.syncStatsLock.Lock()
	if this.syncStatsChainHeight <= origin || this.syncStatsChainOrigin > origin {
		this.syncStatsChainOrigin = origin
	}
	this.syncStatsChainHeight = height
	this.syncStatsLock.Unlock()

	// Initiate the sync using a concurrent header and content retrieval algorithm
	pivot := uint64(0)
	switch this.mode {
	case LightSync:
		pivot = height
	case FastSync:
		// Calculate the new fast/slow sync pivot point
		if this.fsPivotLock == nil {
			pivotOffset, err := rand.Int(rand.Reader, big.NewInt(int64(fsPivotInterval)))
			if err != nil {
				panic(fmt.Sprintf("Failed to access crypto random source: %v", err))
			}
			if height > uint64(fsMinFullBlocks)+pivotOffset.Uint64() {
				pivot = height - uint64(fsMinFullBlocks) - pivotOffset.Uint64()
			}
		} else {
			// Pivot point locked in, use this and do not pick a new one!
			pivot = this.fsPivotLock.Number.Uint64()
		}
		// If the point is below the origin, move origin back to ensure state light sync
		if pivot < origin {
			if pivot > 0 {
				origin = pivot - 1
			} else {
				origin = 0
			}
		}
		log.Debug("light syncing until pivot block", "pivot", pivot)
	}
	this.sch.Prepare(origin+1, this.mode, pivot, latest)
	if this.syncInitHook != nil {
		this.syncInitHook(origin, height)
	}

	fetchers := []func() error{
		func() error { return this.fetchHeaders(p, origin+1) }, // Headers are always retrieved
		func() error { return this.fetchBodies(origin + 1) },   // Bodies are retrieved during normal and light sync
		func() error { return this.fetchReceipts(origin + 1) }, // Receipts are retrieved during light sync
		func() error { return this.processHeaders(origin+1, td) },
	}
	if this.mode == FastSync {
		fetchers = append(fetchers, func() error { return this.processFastSyncContent(latest) })
	} else if this.mode == FullSync {
		fetchers = append(fetchers, this.processFullSyncContent)
	}
	err = this.spawnSync(fetchers)
	if err != nil && this.mode == FastSync && this.fsPivotLock != nil {
		// If sync failed in the critical section, bump the fail counter.
		atomic.AddUint32(&this.fsPivotFails, 1)
	}
	return err
}

// spawnSync runs process and all given fetcher functions to completion in
// separate goroutines, returning the first error that appears.
func (this *lightSync) spawnSync(fetchers []func() error) error {
	var wg sync.WaitGroup
	errc := make(chan error, len(fetchers))
	wg.Add(len(fetchers))
	for _, fn := range fetchers {
		fn := fn
		go func() { defer wg.Done(); errc <- fn() }()
	}
	// Wait for the first error, then terminate the others.
	var err error
	for i := 0; i < len(fetchers); i++ {
		if i == len(fetchers)-1 {
			// Close the sch when all fetchers have exited.
			// This will cause the block processor to end when
			// it has processed the sch.
			this.sch.Close()
		}
		if err = <-errc; err != nil {
			break
		}
	}
	this.sch.Close()
	this.cancel()
	wg.Wait()
	return err
}

// fetchHeight retrieves the head header of the remote peer to aid in estimating
// the total time a pending synchronisation would take.
func (this *lightSync) fetchHeight(p *peerConnection) (*types.Header, error) {
	p.log.Debug("Retrieving remote chain height")

	// Request the advertised remote head block and wait for the response
	head, _ := p.peer.Head()
	go p.peer.RequestHeadersByHash(head, 1, 0, false)

	ttl := this.requestTTL()
	timeout := time.After(ttl)
	for {
		select {
		case <-this.cancelCh:
			return nil, errCancelBlockFetch

		case packet := <-this.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) != 1 {
				p.log.Debug("Multiple headers for single request", "headers", len(headers))
				return nil, errBadPeer
			}
			head := headers[0]
			p.log.Debug("Remote head header identified", "number", head.Number, "hash", head.Hash())
			return head, nil

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return nil, errTimeout

		case <-this.bodyCh:
		case <-this.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
}

// findAncestor tries to locate the common ancestor link of the local chain and
// a remote peers blockchain. In the general case when our node was in sync and
// on the correct chain, checking the top N links should already get us a match.
// In the rare scenario when we ended up on a long reorganisation (i.e. none of
// the head links match), we do a binary search to find the common ancestor.
func (this *lightSync) findAncestor(p *peerConnection, height uint64) (uint64, error) {
	// Figure out the valid ancestor range to prevent rewrite attacks
	floor, ceil := int64(-1), this.lightchain.CurrentHeader().Number.Uint64()

	p.log.Debug("Looking for common ancestor", "local", ceil, "remote", height)
	if this.mode == FullSync {
		ceil = core.InstanceBlockChain().CurrentBlock().NumberU64()
	} else if this.mode == FastSync {
		ceil = core.InstanceBlockChain().CurrentFastBlock().NumberU64()
	}
	if ceil >= MaxForkAncestry {
		floor = int64(ceil - MaxForkAncestry)
	}
	// Request the topmost blocks to short circuit binary ancestor lookup
	head := ceil
	if head > height {
		head = height
	}
	from := int64(head) - int64(MaxHeaderFetch)
	if from < 0 {
		from = 0
	}
	// Span out with 15 block gaps into the future to catch bad head reports
	limit := 2 * MaxHeaderFetch / 16
	count := 1 + int((int64(ceil)-from)/16)
	if count > limit {
		count = limit
	}
	go p.peer.RequestHeadersByNumber(uint64(from), count, 15, false)

	// Wait for the remote response to the head fetch
	number, hash := uint64(0), common.Hash{}

	ttl := this.requestTTL()
	timeout := time.After(ttl)

	for finished := false; !finished; {
		select {
		case <-this.cancelCh:
			return 0, errCancelHeaderFetch

		case packet := <-this.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) == 0 {
				p.log.Warn("Empty head header set")
				return 0, errEmptyHeaderSet
			}
			// Make sure the peer's reply conforms to the request
			for i := 0; i < len(headers); i++ {
				if number := headers[i].Number.Int64(); number != from+int64(i)*16 {
					p.log.Warn("Head headers broke chain ordering", "index", i, "requested", from+int64(i)*16, "received", number)
					return 0, errInvalidChain
				}
			}
			// Check if a common ancestor was found
			finished = true
			for i := len(headers) - 1; i >= 0; i-- {
				// Skip any headers that underflow/overflow our requested set
				if headers[i].Number.Int64() < from || headers[i].Number.Uint64() > ceil {
					continue
				}
				// Otherwise check if we already know the header or not
				if (this.mode == FullSync && core.InstanceBlockChain().HasBlockAndState(headers[i].Hash())) ||
					(this.mode != FullSync && this.lightchain.HasHeader(headers[i].Hash(), headers[i].Number.Uint64())) {
					number, hash = headers[i].Number.Uint64(), headers[i].Hash()

					// If every header is known, even future ones, the peer straight out lied about its head
					if number > height && i == limit-1 {
						p.log.Warn("Lied about chain head", "reported", height, "found", number)
						return 0, errStallingPeer
					}
					break
				}
			}

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return 0, errTimeout

		case <-this.bodyCh:
		case <-this.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
	// If the head fetch already found an ancestor, return
	if !common.EmptyHash(hash) {
		if int64(number) <= floor {
			p.log.Warn("Ancestor below allowance", "number", number, "hash", hash, "allowance", floor)
			return 0, errInvalidAncestor
		}
		p.log.Debug("Found common ancestor", "number", number, "hash", hash)
		return number, nil
	}
	// Ancestor not found, we need to binary search over our chain
	start, end := uint64(0), head
	if floor > 0 {
		start = uint64(floor)
	}
	for start+1 < end {
		// Split our chain interval in two, and request the hash to cross check
		check := (start + end) / 2

		ttl := this.requestTTL()
		timeout := time.After(ttl)

		go p.peer.RequestHeadersByNumber(uint64(check), 1, 0, false)

		// Wait until a reply arrives to this request
		for arrived := false; !arrived; {
			select {
			case <-this.cancelCh:
				return 0, errCancelHeaderFetch

			case packer := <-this.headerCh:
				// Discard anything not from the origin peer
				if packer.PeerId() != p.id {
					log.Debug("Received headers from incorrect peer", "peer", packer.PeerId())
					break
				}
				// Make sure the peer actually gave something valid
				headers := packer.(*headerPack).headers
				if len(headers) != 1 {
					p.log.Debug("Multiple headers for single request", "headers", len(headers))
					return 0, errBadPeer
				}
				arrived = true

				// Modify the search interval based on the response
				if (this.mode == FullSync && !core.InstanceBlockChain().HasBlockAndState(headers[0].Hash())) || (this.mode != FullSync && !this.lightchain.HasHeader(headers[0].Hash(), headers[0].Number.Uint64())) {
					end = check
					break
				}
				header := this.lightchain.GetHeaderByHash(headers[0].Hash()) // Independent of sync mode, header surely exists
				if header.Number.Uint64() != check {
					p.log.Debug("Received non requested header", "number", header.Number, "hash", header.Hash(), "request", check)
					return 0, errBadPeer
				}
				start = check

			case <-timeout:
				p.log.Debug("Waiting for search header timed out", "elapsed", ttl)
				return 0, errTimeout

			case <-this.bodyCh:
			case <-this.receiptCh:
				// Out of bounds delivery, ignore
			}
		}
	}
	// Ensure valid ancestry and return
	if int64(start) <= floor {
		p.log.Warn("Ancestor below allowance", "number", start, "hash", hash, "allowance", floor)
		return 0, errInvalidAncestor
	}
	p.log.Debug("Found common ancestor", "number", start, "hash", hash)
	return start, nil
}

// fetchHeaders keeps retrieving headers concurrently from the number
// requested, until no more are returned, potentially throttling on the way. To
// facilitate concurrency but still protect against malicious nodes sending bad
// headers, we construct a header chain skeleton using the "origin" peer we are
// syncing with, and fill in the missing headers using anyone else. Headers from
// other peers are only accepted if they map cleanly to the skeleton. If no one
// can fill in the skeleton - not even the origin peer - it's assumed invalid and
// the origin is dropped.
func (this *lightSync) fetchHeaders(p *peerConnection, from uint64) error {
	p.log.Debug("Directing header light syncs", "origin", from)
	defer p.log.Debug("Header light sync terminated")

	// Create a timeout timer, and the associated header fetcher
	skeleton := true            // Skeleton assembly phase or finishing up
	request := time.Now()       // time of the last skeleton fetch request
	timeout := time.NewTimer(0) // timer to dump a non-responsive active peer
	<-timeout.C                 // timeout channel should be initially empty
	defer timeout.Stop()

	var ttl time.Duration
	getHeaders := func(from uint64) {
		request = time.Now()

		ttl = this.requestTTL()
		timeout.Reset(ttl)

		if skeleton {
			p.log.Trace("Fetching skeleton headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from+uint64(MaxHeaderFetch)-1, MaxSkeletonSize, MaxHeaderFetch-1, false)
		} else {
			p.log.Trace("Fetching full headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from, MaxHeaderFetch, 0, false)
		}
	}
	// Start pulling the header chain skeleton until all is done
	getHeaders(from)

	for {
		select {
		case <-this.cancelCh:
			return errCancelHeaderFetch

		case packet := <-this.headerCh:
			// Make sure the active peer is giving us the skeleton headers
			if packet.PeerId() != p.id {
				log.Debug("Received skeleton from incorrect peer", "peer", packet.PeerId())
				break
			}
			headerReqTimer.UpdateSince(request)
			timeout.Stop()

			// If the skeleton's finished, pull any remaining head headers directly from the origin
			if packet.Items() == 0 && skeleton {
				skeleton = false
				getHeaders(from)
				continue
			}
			// If no more headers are inbound, notify the content fetchers and return
			if packet.Items() == 0 {
				p.log.Debug("No more headers available")
				select {
				case this.headerProcCh <- nil:
					return nil
				case <-this.cancelCh:
					return errCancelHeaderFetch
				}
			}
			headers := packet.(*headerPack).headers

			// If we received a skeleton batch, resolve internals concurrently
			if skeleton {
				filled, proced, err := this.fillHeaderSkeleton(from, headers)
				if err != nil {
					p.log.Debug("Skeleton chain invalid", "err", err)
					return errInvalidChain
				}
				headers = filled[proced:]
				from += uint64(proced)
			}
			// Insert all the new headers and fetch the next batch
			if len(headers) > 0 {
				p.log.Trace("Scheduling new headers", "count", len(headers), "from", from)
				select {
				case this.headerProcCh <- headers:
				case <-this.cancelCh:
					return errCancelHeaderFetch
				}
				from += uint64(len(headers))
			}
			getHeaders(from)

		case <-timeout.C:
			// Header retrieval timed out, consider the peer bad and drop
			p.log.Debug("Header request timed out", "elapsed", ttl)
			headerTimeoutMeter.Mark(1)
			this.dropPeer(p.id)

			// Finish the sync gracefully instead of dumping the gathered data though
			for _, ch := range []chan bool{this.bodyWakeCh, this.receiptWakeCh} {
				select {
				case ch <- false:
				case <-this.cancelCh:
				}
			}
			select {
			case this.headerProcCh <- nil:
			case <-this.cancelCh:
			}
			return errBadPeer
		}
	}
}

// fillHeaderSkeleton concurrently retrieves headers from all our available peers
// and maps them to the provided skeleton header chain.
//
// Any partial results from the beginning of the skeleton is (if possible) forwarded
// immediately to the header processor to keep the rest of the pipeline full even
// in the case of header stalls.
//
// The method returs the entire filled skeleton and also the number of headers
// already forwarded for processing.
func (this *lightSync) fillHeaderSkeleton(from uint64, skeleton []*types.Header) ([]*types.Header, int, error) {
	log.Debug("Filling up skeleton", "from", from)
	this.sch.ScheduleSkeleton(from, skeleton)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*headerPack)
			return this.sch.DeliverHeaders(pack.peerId, pack.headers, this.headerProcCh)
		}
		expire   = func() map[string]int { return this.sch.ExpireHeaders(this.requestTTL()) }
		throttle = func() bool { return false }
		reserve  = func(p *peerConnection, count int) (*fetchRequest, bool, error) {
			return this.sch.ReserveHeaders(p, count), false, nil
		}
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchHeaders(req.From, MaxHeaderFetch) }
		capacity = func(p *peerConnection) int { return p.HeaderCapacity(this.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetHeadersIdle(accepted) }
	)
	err := this.fetchParts(errCancelHeaderFetch, this.headerCh, deliver, this.sch.headerContCh, expire,
		this.sch.PendingHeaders, this.sch.InFlightHeaders, throttle, reserve,
		nil, fetch, this.sch.CancelHeaders, capacity, this.peers.HeaderIdlePeers, setIdle, "headers")

	log.Debug("Skeleton fill terminated", "err", err)

	filled, proced := this.sch.RetrieveHeaders()
	return filled, proced, err
}

// fetchBodies iteratively light syncs the scheduled block bodies, taking any
// available peers, reserving a chunk of blocks for each, waiting for delivery
// and also periodically checking for timeouts.
func (this *lightSync) fetchBodies(from uint64) error {
	log.Debug("light syncing block bodies", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*bodyPack)
			return this.sch.DeliverBodies(pack.peerId, pack.transactions, pack.uncles)
		}
		expire   = func() map[string]int { return this.sch.ExpireBodies(this.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchBodies(req) }
		capacity = func(p *peerConnection) int { return p.BlockCapacity(this.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetBodiesIdle(accepted) }
	)
	err := this.fetchParts(errCancelBodyFetch, this.bodyCh, deliver, this.bodyWakeCh, expire,
		this.sch.PendingBlocks, this.sch.InFlightBlocks, this.sch.ShouldThrottleBlocks, this.sch.ReserveBodies,
		this.bodyFetchHook, fetch, this.sch.CancelBodies, capacity, this.peers.BodyIdlePeers, setIdle, "bodies")

	log.Debug("Block body light sync terminated", "err", err)
	return err
}

// fetchReceipts iteratively light syncs the scheduled block receipts, taking any
// available peers, reserving a chunk of receipts for each, waiting for delivery
// and also periodically checking for timeouts.
func (this *lightSync) fetchReceipts(from uint64) error {
	log.Debug("light syncing transaction receipts", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*receiptPack)
			return this.sch.DeliverReceipts(pack.peerId, pack.receipts)
		}
		expire   = func() map[string]int { return this.sch.ExpireReceipts(this.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchReceipts(req) }
		capacity = func(p *peerConnection) int { return p.ReceiptCapacity(this.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetReceiptsIdle(accepted) }
	)
	err := this.fetchParts(errCancelReceiptFetch, this.receiptCh, deliver, this.receiptWakeCh, expire,
		this.sch.PendingReceipts, this.sch.InFlightReceipts, this.sch.ShouldThrottleReceipts, this.sch.ReserveReceipts,
		this.receiptFetchHook, fetch, this.sch.CancelReceipts, capacity, this.peers.ReceiptIdlePeers, setIdle, "receipts")

	log.Debug("Transaction receipt light sync terminated", "err", err)
	return err
}

// fetchParts iteratively light syncs scheduled block parts, taking any available
// peers, reserving a chunk of fetch requests for each, waiting for delivery and
// also periodically checking for timeouts.
//
// As the scheduling/timeout logic mostly is the same for all light synced data
// types, this method is used by each for data gathering and is instrumented with
// various callbacks to handle the slight differences between processing them.
//
// The instrumentation parameters:
//  - errCancel:   error type to return if the fetch operation is cancelled (mostly makes logging nicer)
//  - deliveryCh:  channel from which to retrieve light synced data packets (merged from all concurrent peers)
//  - deliver:     processing callback to deliver data packets into type specific light sync schs (usually within `sch`)
//  - wakeCh:      notification channel for waking the fetcher when new tasks are available (or sync completed)
//  - expire:      task callback method to abort requests that took too long and return the faulty peers (traffic shaping)
//  - pending:     task callback for the number of requests still needing light sync (detect completion/non-completability)
//  - inFlight:    task callback for the number of in-progress requests (wait for all active light syncs to finish)
//  - throttle:    task callback to check if the processing sch is full and activate throttling (bound memory use)
//  - reserve:     task callback to reserve new light sync tasks to a particular peer (also signals partial completions)
//  - fetchHook:   tester callback to notify of new tasks being initiated (allows testing the scheduling logic)
//  - fetch:       network callback to actually send a particular light sync request to a physical remote peer
//  - cancel:      task callback to abort an in-flight light sync request and allow rescheduling it (in case of lost peer)
//  - capacity:    network callback to retrieve the estimated type-specific bandwidth capacity of a peer (traffic shaping)
//  - idle:        network callback to retrieve the currently (type specific) idle peers that can be assigned tasks
//  - setIdle:     network callback to set a peer back to idle and update its estimated capacity (traffic shaping)
//  - kind:        textual label of the type being light synced to display in log mesages
func (this *lightSync) fetchParts(errCancel error, deliveryCh chan dataPack, deliver func(dataPack) (int, error), wakeCh chan bool,
	expire func() map[string]int, pending func() int, inFlight func() bool, throttle func() bool, reserve func(*peerConnection, int) (*fetchRequest, bool, error),
	fetchHook func([]*types.Header), fetch func(*peerConnection, *fetchRequest) error, cancel func(*fetchRequest), capacity func(*peerConnection) int,
	idle func() ([]*peerConnection, int), setIdle func(*peerConnection, int), kind string) error {

	// Create a ticker to detect expired retrieval tasks
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	update := make(chan struct{}, 1)

	// Prepare the sch and fetch block parts until the block header fetcher's done
	finished := false
	for {
		select {
		case <-this.cancelCh:
			return errCancel

		case packet := <-deliveryCh:
			// If the peer was previously banned and failed to deliver it's pack
			// in a reasonable time frame, ignore it's message.
			if peer := this.peers.Peer(packet.PeerId()); peer != nil {
				// Deliver the received chunk of data and check chain validity
				accepted, err := deliver(packet)
				if err == errInvalidChain {
					return err
				}
				// Unless a peer delivered something completely else than requested (usually
				// caused by a timed out request which came through in the end), set it to
				// idle. If the delivery's stale, the peer should have already been idlethis.
				if err != errStaleDelivery {
					setIdle(peer, accepted)
				}
				// Issue a log to the user to see what's going on
				switch {
				case err == nil && packet.Items() == 0:
					peer.log.Trace("Requested data not delivered", "type", kind)
				case err == nil:
					peer.log.Trace("Delivered new batch of data", "type", kind, "count", packet.Stats())
				default:
					peer.log.Trace("Failed to deliver retrieved data", "type", kind, "err", err)
				}
			}
			// Blocks assembled, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case cont := <-wakeCh:
			// The header fetcher sent a continuation flag, check if it's done
			if !cont {
				finished = true
			}
			// Headers arrive, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-ticker.C:
			// Sanity check update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-update:
			// Short circuit if we lost all our peers
			if this.peers.Len() == 0 {
				return errNoPeers
			}
			// Check for fetch request timeouts and demote the responsible peers
			for pid, fails := range expire() {
				if peer := this.peers.Peer(pid); peer != nil {
					// If a lot of retrieval elements expired, we might have overestimated the remote peer or perhaps
					// ourselves. Only reset to minimal throughput but don't drop just yet. If even the minimal times
					// out that sync wise we need to get rid of the peer.
					//
					// The reason the minimum threshold is 2 is because the light syncer tries to estimate the bandwidth
					// and latency of a peer separately, which requires pushing the measures capacity a bit and seeing
					// how response times reacts, to it always requests one more than the minimum (i.e. min 2).
					if fails > 2 {
						peer.log.Trace("Data delivery timed out", "type", kind)
						setIdle(peer, 0)
					} else {
						peer.log.Debug("Stalling delivery, dropping", "type", kind)
						this.dropPeer(pid)
					}
				}
			}
			// If there's nothing more to fetch, wait or terminate
			if pending() == 0 {
				if !inFlight() && finished {
					log.Debug("Data fetching completed", "type", kind)
					return nil
				}
				break
			}
			// Send a light sync request to all idle peers, until throttled
			progressed, throttled, running := false, false, inFlight()
			idles, total := idle()

			for _, peer := range idles {
				// Short circuit if throttling activated
				if throttle() {
					throttled = true
					break
				}
				// Short circuit if there is no more available task.
				if pending() == 0 {
					break
				}
				// Reserve a chunk of fetches for a peer. A nil can mean either that
				// no more headers are available, or that the peer is known not to
				// have them.
				request, progress, err := reserve(peer, capacity(peer))
				if err != nil {
					return err
				}
				if progress {
					progressed = true
				}
				if request == nil {
					continue
				}
				if request.From > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "from", request.From)
				} else if len(request.Headers) > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Headers), "from", request.Headers[0].Number)
				} else {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Hashes))
				}
				// Fetch the chunk and make sure any errors return the hashes to the sch
				if fetchHook != nil {
					fetchHook(request.Headers)
				}
				if err := fetch(peer, request); err != nil {
					// Although we could try and make an attempt to fix this, this error really
					// means that we've double allocated a fetch task to a peer. If that is the
					// case, the internal state of the light syncer and the sch is very wrong so
					// better hard crash and note the error instead of silently accumulating into
					// a much bigger issue.
					panic(fmt.Sprintf("%v: %s fetch assignment failed", peer, kind))
				}
				running = true
			}
			// Make sure that we have peers available for fetching. If all peers have been tried
			// and all failed throw an error
			if !progressed && !throttled && !running && len(idles) == total && pending() > 0 {
				return errPeersUnavailable
			}
		}
	}
}

// processHeaders takes batches of retrieved headers from an input channel and
// keeps processing and scheduling them into the header chain and light syncer's
// sch until the stream ends or a failure occurs.
func (this *lightSync) processHeaders(origin uint64, td *big.Int) error {
	// Calculate the pivoting point for switching from fast to slow sync
	pivot := this.sch.FastSyncPivot()

	// Keep a count of uncertain headers to roll back
	rollback := []*types.Header{}
	defer func() {
		if len(rollback) > 0 {
			// Flatten the headers and roll them back
			hashes := make([]common.Hash, len(rollback))
			for i, header := range rollback {
				hashes[i] = header.Hash()
			}
			lastHeader, lastFastBlock, lastBlock := this.lightchain.CurrentHeader().Number, common.Big0, common.Big0
			if this.mode != LightSync {
				lastFastBlock = core.InstanceBlockChain().CurrentFastBlock().Number()
				lastBlock = core.InstanceBlockChain().CurrentBlock().Number()
			}
			this.lightchain.Rollback(hashes)
			curFastBlock, curBlock := common.Big0, common.Big0
			if this.mode != LightSync {
				curFastBlock = core.InstanceBlockChain().CurrentFastBlock().Number()
				curBlock = core.InstanceBlockChain().CurrentBlock().Number()
			}
			log.Warn("Rolled back headers", "count", len(hashes),
				"header", fmt.Sprintf("%d->%d", lastHeader, this.lightchain.CurrentHeader().Number),
				"fast", fmt.Sprintf("%d->%d", lastFastBlock, curFastBlock),
				"block", fmt.Sprintf("%d->%d", lastBlock, curBlock))

			// If we're already past the pivot point, this could be an attack, thread carefully
			if rollback[len(rollback)-1].Number.Uint64() > pivot {
				// If we didn't ever fail, lock in the pivot header (must! not! change!)
				if atomic.LoadUint32(&this.fsPivotFails) == 0 {
					for _, header := range rollback {
						if header.Number.Uint64() == pivot {
							log.Warn("Fast-sync pivot locked in", "number", pivot, "hash", header.Hash())
							this.fsPivotLock = header
						}
					}
				}
			}
		}
	}()

	// Wait for batches of headers to process
	gotHeaders := false

	for {
		select {
		case <-this.cancelCh:
			return errCancelHeaderProcessing

		case headers := <-this.headerProcCh:
			// Terminate header processing if we synced up
			if len(headers) == 0 {
				// Notify everyone that headers are fully processed
				for _, ch := range []chan bool{this.bodyWakeCh, this.receiptWakeCh} {
					select {
					case ch <- false:
					case <-this.cancelCh:
					}
				}
				// If no headers were retrieved at all, the peer violated it's TD promise that it had a
				// better chain compared to ours. The only exception is if it's promised blocks were
				// already imported by other means (e.g. fecher):
				//
				// R <remote peer>, L <local node>: Both at block 10
				// R: Mine block 11, and propagate it to L
				// L: sch block 11 for import
				// L: Notice that R's head and TD increased compared to ours, start sync
				// L: Import of block 11 finishes
				// L: Sync begins, and finds common ancestor at 11
				// L: Request new headers up from 11 (R's TD was higher, it must have something)
				// R: Nothing to give
				if this.mode != LightSync {
					if !gotHeaders && td.Cmp(core.InstanceBlockChain().GetTdByHash(core.InstanceBlockChain().CurrentBlock().Hash())) > 0 {
						return errStallingPeer
					}
				}
				// If fast or light syncing, ensure promised headers are indeed delivered. This is
				// needed to detect scenarios where an attacker feeds a bad pivot and then bails out
				// of delivering the post-pivot blocks that would flag the invalid content.
				//
				// This check cannot be executed "as is" for full imports, since blocks may still be
				// schd for processing when the header light sync completes. However, as long as the
				// peer gave us something useful, we're already happy/progressed (above check).
				if this.mode == FastSync || this.mode == LightSync {
					if td.Cmp(this.lightchain.GetTdByHash(this.lightchain.CurrentHeader().Hash())) > 0 {
						return errStallingPeer
					}
				}
				// Disable any rollback and return
				rollback = nil
				return nil
			}
			// Otherwise split the chunk of headers into batches and process them
			gotHeaders = true

			for len(headers) > 0 {
				// Terminate if something failed in between processing chunks
				select {
				case <-this.cancelCh:
					return errCancelHeaderProcessing
				default:
				}
				// Select the next chunk of headers to import
				limit := maxHeadersProcess
				if limit > len(headers) {
					limit = len(headers)
				}
				chunk := headers[:limit]

				// In case of header only syncing, validate the chunk immediately
				if this.mode == FastSync || this.mode == LightSync {
					// Collect the yet unknown headers to mark them as uncertain
					unknown := make([]*types.Header, 0, len(headers))
					for _, header := range chunk {
						if !this.lightchain.HasHeader(header.Hash(), header.Number.Uint64()) {
							unknown = append(unknown, header)
						}
					}
					// If we're importing pure headers, verify based on their recentness
					frequency := fsHeaderCheckFrequency
					if chunk[len(chunk)-1].Number.Uint64()+uint64(fsHeaderForceVerify) > pivot {
						frequency = 1
					}
					if n, err := this.lightchain.InsertHeaderChain(chunk, frequency); err != nil {
						// If some headers were inserted, add them too to the rollback list
						if n > 0 {
							rollback = append(rollback, chunk[:n]...)
						}
						log.Debug("Invalid header encountered", "number", chunk[n].Number, "hash", chunk[n].Hash(), "err", err)
						return errInvalidChain
					}
					// All verifications passed, store newly found uncertain headers
					rollback = append(rollback, unknown...)
					if len(rollback) > fsHeaderSafetyNet {
						rollback = append(rollback[:0], rollback[len(rollback)-fsHeaderSafetyNet:]...)
					}
				}
				// If we're light syncing and just pulled in the pivot, make sure it's the one locked in
				if this.mode == FastSync && this.fsPivotLock != nil && chunk[0].Number.Uint64() <= pivot && chunk[len(chunk)-1].Number.Uint64() >= pivot {
					if pivot := chunk[int(pivot-chunk[0].Number.Uint64())]; pivot.Hash() != this.fsPivotLock.Hash() {
						log.Warn("Pivot doesn't match locked in one", "remoteNumber", pivot.Number, "remoteHash", pivot.Hash(), "localNumber", this.fsPivotLock.Number, "localHash", this.fsPivotLock.Hash())
						return errInvalidChain
					}
				}
				// Unless we're doing light chains, schedule the headers for associated content retrieval
				if this.mode == FullSync || this.mode == FastSync {
					// If we've reached the allowed number of pending headers, stall a bit
					for this.sch.PendingBlocks() >= maxQueuedHeaders || this.sch.PendingReceipts() >= maxQueuedHeaders {
						select {
						case <-this.cancelCh:
							return errCancelHeaderProcessing
						case <-time.After(time.Second):
						}
					}
					// Otherwise insert the headers for content retrieval
					inserts := this.sch.Schedule(chunk, origin)
					if len(inserts) != len(chunk) {
						log.Debug("Stale headers")
						return errBadPeer
					}
				}
				headers = headers[limit:]
				origin += uint64(limit)
			}
			// Signal the content light syncers of the availablility of new tasks
			for _, ch := range []chan bool{this.bodyWakeCh, this.receiptWakeCh} {
				select {
				case ch <- true:
				default:
				}
			}
		}
	}
}

// processFullSyncContent takes fetch results from the sch and imports them into the chain.
func (this *lightSync) processFullSyncContent() error {
	for {
		results := this.sch.WaitResults()
		if len(results) == 0 {
			return nil
		}
		if this.chainInsertHook != nil {
			this.chainInsertHook(results)
		}
		if err := this.importBlockResults(results); err != nil {
			return err
		}
	}
}

func (this *lightSync) importBlockResults(results []*fetchResult) error {
	for len(results) != 0 {
		// Check for any termination requests. This makes clean shutdown faster.
		select {
		case <-this.quitCh:
			return errCancelContentProcessing
		default:
		}
		// Retrieve the a batch of results to import
		items := int(math.Min(float64(len(results)), float64(maxResultsProcess)))
		first, last := results[0].Header, results[items-1].Header
		log.Debug("Inserting light synced chain", "items", len(results),
			"firstnum", first.Number, "firsthash", first.Hash(),
			"lastnum", last.Number, "lasthash", last.Hash(),
		)
		blocks := make([]*types.Block, items)
		for i, result := range results[:items] {
			blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
		}
		if index, err := core.InstanceBlockChain().InsertChain(blocks); err != nil {
			log.Debug("light synced item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
			return errInvalidChain
		}
		// Shift the results to the next batch
		results = results[items:]
	}
	return nil
}

// processFastSyncContent takes fetch results from the sch and writes them to the
// database. It also controls the synchronisation of state nodes of the pivot block.
func (this *lightSync) processFastSyncContent(latest *types.Header) error {
	// Start syncing state of the reported head block.
	// This should get us most of the state of the pivot block.
	stateSync := this.syncState(latest.Root)
	defer stateSync.Cancel()
	go func() {
		if err := stateSync.Wait(); err != nil {
			this.sch.Close() // wake up WaitResults
		}
	}()

	pivot := this.sch.FastSyncPivot()
	for {
		results := this.sch.WaitResults()
		if len(results) == 0 {
			return stateSync.Cancel()
		}
		if this.chainInsertHook != nil {
			this.chainInsertHook(results)
		}
		P, beforeP, afterP := splitAroundPivot(pivot, results)
		if err := this.commitFastSyncData(beforeP, stateSync); err != nil {
			return err
		}
		if P != nil {
			stateSync.Cancel()
			if err := this.commitPivotBlock(P); err != nil {
				return err
			}
		}
		if err := this.importBlockResults(afterP); err != nil {
			return err
		}
	}
}

func (this *lightSync) commitFastSyncData(results []*fetchResult, stateSync *stateSync) error {
	for len(results) != 0 {
		// Check for any termination requests.
		select {
		case <-this.quitCh:
			return errCancelContentProcessing
		case <-stateSync.done:
			if err := stateSync.Wait(); err != nil {
				return err
			}
		default:
		}
		// Retrieve the a batch of results to import
		items := int(math.Min(float64(len(results)), float64(maxResultsProcess)))
		first, last := results[0].Header, results[items-1].Header
		log.Debug("Inserting fast-sync blocks", "items", len(results),
			"firstnum", first.Number, "firsthash", first.Hash(),
			"lastnumn", last.Number, "lasthash", last.Hash(),
		)
		blocks := make([]*types.Block, items)
		receipts := make([]types.Receipts, items)
		for i, result := range results[:items] {
			blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
			receipts[i] = result.Receipts
		}
		if index, err := core.InstanceBlockChain().InsertReceiptChain(blocks, receipts); err != nil {
			log.Debug("light synced item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
			return errInvalidChain
		}
		// Shift the results to the next batch
		results = results[items:]
	}
	return nil
}

func (this *lightSync) commitPivotBlock(result *fetchResult) error {
	b := types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	// Sync the pivot block state. This should complete reasonably quickly because
	// we've already synced up to the reported head block state earlier.
	if err := this.syncState(b.Root()).Wait(); err != nil {
		return err
	}
	log.Debug("Committing light sync pivot as new head", "number", b.Number(), "hash", b.Hash())
	if _, err := core.InstanceBlockChain().InsertReceiptChain([]*types.Block{b}, []types.Receipts{result.Receipts}); err != nil {
		return err
	}
	return core.InstanceBlockChain().FastSyncCommitHead(b.Hash())
}

// deliver injects a new batch of data received from a remote node.
func (this *lightSync) deliver(id string, destCh chan dataPack, packet dataPack, inMeter, dropMeter metrics.Meter) (err error) {
	// Update the delivery metrics for both good and failed deliveries
	inMeter.Mark(int64(packet.Items()))
	defer func() {
		if err != nil {
			dropMeter.Mark(int64(packet.Items()))
		}
	}()
	// Deliver or abort if the sync is canceled while queuing
	this.cancelLock.RLock()
	cancel := this.cancelCh
	this.cancelLock.RUnlock()
	if cancel == nil {
		return errNoSyncActive
	}
	select {
	case destCh <- packet:
		return nil
	case <-cancel:
		return errNoSyncActive
	}
}

// qosTuner is the quality of service tuning loop that occasionally gathers the
// peer latency statistics and updates the estimated request round trip time.
func (this *lightSync) qosTuner() {
	for {
		// Retrieve the current median RTT and integrate into the previoust target RTT
		rtt := time.Duration(float64(1-qosTuningImpact)*float64(atomic.LoadUint64(&this.rttEstimate)) + qosTuningImpact*float64(this.peers.medianRTT()))
		atomic.StoreUint64(&this.rttEstimate, uint64(rtt))

		// A new RTT cycle passed, increase our confidence in the estimated RTT
		conf := atomic.LoadUint64(&this.rttConfidence)
		conf = conf + (1000000-conf)/2
		atomic.StoreUint64(&this.rttConfidence, conf)

		// Log the new QoS values and sleep until the next RTT
		log.Debug("Recalculated light syncer QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", this.requestTTL())
		select {
		case <-this.quitCh:
			return
		case <-time.After(rtt):
		}
	}
}

// qosReduceConfidence is meant to be called when a new peer joins the light syncer's
// peer set, needing to reduce the confidence we have in out QoS estimates.
func (this *lightSync) qosReduceConfidence() {
	// If we have a single peer, confidence is always 1
	peers := uint64(this.peers.Len())
	if peers == 0 {
		// Ensure peer connectivity races don't catch us off guard
		return
	}
	if peers == 1 {
		atomic.StoreUint64(&this.rttConfidence, 1000000)
		return
	}
	// If we have a ton of peers, don't drop confidence)
	if peers >= uint64(qosConfidenceCap) {
		return
	}
	// Otherwise drop the confidence factor
	conf := atomic.LoadUint64(&this.rttConfidence) * (peers - 1) / peers
	if float64(conf)/1000000 < rttMinConfidence {
		conf = uint64(rttMinConfidence * 1000000)
	}
	atomic.StoreUint64(&this.rttConfidence, conf)

	rtt := time.Duration(atomic.LoadUint64(&this.rttEstimate))
	log.Debug("Relaxed light syncer QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", this.requestTTL())
}

// requestRTT returns the current target round trip time for a light sync request
// to complete in.
//
// Note, the returned RTT is .9 of the actually estimated RTT. The reason is that
// the light syncer tries to adapt queries to the RTT, so multiple RTT values can
// be adapted to, but smaller ones are preffered (stabler light sync stream).
func (this *lightSync) requestRTT() time.Duration {
	return time.Duration(atomic.LoadUint64(&this.rttEstimate)) * 9 / 10
}

// requestTTL returns the current timeout allowance for a single light sync request
// to finish under.
func (this *lightSync) requestTTL() time.Duration {
	var (
		rtt  = time.Duration(atomic.LoadUint64(&this.rttEstimate))
		conf = float64(atomic.LoadUint64(&this.rttConfidence)) / 1000000.0
	)
	ttl := time.Duration(ttlScaling) * time.Duration(float64(rtt)/conf)
	if ttl > ttlLimit {
		ttl = ttlLimit
	}
	return ttl
}


// syncState starts light syncing state with the given root hash.
func (this *lightSync) syncState(root common.Hash) *stateSync {
	s := newStateSync(this, root)
	select {
	case this.stateSyncStart <- s:
	case <-this.quitCh:
		s.err = errCancelStateFetch
		close(s.done)
	}
	return s
}

// stateFetcher manages the active state sync and accepts requests
// on its behalf.
func (this *lightSync) stateFetcher() {
	for {
		select {
		case s := <-this.stateSyncStart:
			for next := s; next != nil; {
				next = this.runStateSync(next)
			}
		case <-this.stateCh:
			// Ignore state responses while no sync is running.
		case <-this.quitCh:
			return
		}
	}
}

// runStateSync runs a state synchronisation until it completes or another root
// hash is requested to be switched over to.
func (this *lightSync) runStateSync(s *stateSync) *stateSync {
	var (
		active   = make(map[string]*stateReq) // Currently in-flight requests
		finished []*stateReq                  // Completed or failed requests
		timeout  = make(chan *stateReq)       // Timed out active requests
	)
	defer func() {
		// Cancel active request timers on exit. Also set peers to idle so they're
		// available for the next sync.
		for _, req := range active {
			req.timer.Stop()
			req.peer.SetNodeDataIdle(len(req.items))
		}
	}()
	// Run the state sync.
	go s.run()
	defer s.Cancel()

	// Listen for peer departure events to cancel assigned tasks
	peerDrop := make(chan *peerConnection, 1024)
	peerSub := s.syn.sch.SubscribePeerDrops(peerDrop)
	defer peerSub.Unsubscribe()

	for {
		// Enable sending of the first buffered element if there is one.
		var (
			deliverReq   *stateReq
			deliverReqCh chan *stateReq
		)
		if len(finished) > 0 {
			deliverReq = finished[0]
			deliverReqCh = s.deliver
		}

		select {
		// The stateSync lifecycle:
		case next := <-this.stateSyncStart:
			return next

		case <-s.done:
			return nil

			// Send the next finished request to the current sync:
		case deliverReqCh <- deliverReq:
			finished = append(finished[:0], finished[1:]...)

			// Handle incoming state packs:
		case pack := <-this.stateCh:
			// Discard any data not requested (or previsouly timed out)
			req := active[pack.PeerId()]
			if req == nil {
				log.Debug("Unrequested node data", "peer", pack.PeerId(), "len", pack.Items())
				continue
			}
			// Finalize the request and queue up for processing
			req.timer.Stop()
			req.response = pack.(*statePack).states

			finished = append(finished, req)
			delete(active, pack.PeerId())

			// Handle dropped peer connections:
		case p := <-peerDrop:
			// Skip if no request is currently pending
			req := active[p.id]
			if req == nil {
				continue
			}
			// Finalize the request and queue up for processing
			req.timer.Stop()
			req.dropped = true

			finished = append(finished, req)
			delete(active, p.id)

			// Handle timed-out requests:
		case req := <-timeout:
			// If the peer is already requesting something else, ignore the stale timeout.
			// This can happen when the timeout and the delivery happens simultaneously,
			// causing both pathways to trigger.
			if active[req.peer.id] != req {
				continue
			}
			// Move the timed out data back into the light sync queue
			finished = append(finished, req)
			delete(active, req.peer.id)

			// Track outgoing state requests:
		case req := <-this.trackStateReq:
			// If an active request already exists for this peer, we have a problem. In
			// theory the trie node schedule must never assign two requests to the same
			// peer. In practive however, a peer might receive a request, disconnect and
			// immediately reconnect before the previous times out. In this case the first
			// request is never honored, alas we must not silently overwrite it, as that
			// causes valid requests to go missing and sync to get stuck.
			if old := active[req.peer.id]; old != nil {
				log.Warn("Busy peer assigned new state fetch", "peer", old.peer.id)

				// Make sure the previous one doesn't get siletly lost
				old.timer.Stop()
				old.dropped = true

				finished = append(finished, old)
			}
			// Start a timer to notify the sync loop if the peer stalled.
			req.timer = time.AfterFunc(req.timeout, func() {
				select {
				case timeout <- req:
				case <-s.done:
					// Prevent leaking of timer goroutines in the unlikely case where a
					// timer is fired just before exiting runStateSync.
				}
			})
			active[req.peer.id] = req
		}
	}
}