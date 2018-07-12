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

package p2p

import (
	"sync"
	"errors"
	"math/big"
	"time"
	"fmt"
	"github.com/hpb-project/go-hpb/common"
	"github.com/hpb-project/go-hpb/common/log"
	"gopkg.in/fatih/set.v0"
	"github.com/hpb-project/go-hpb/config"
	"github.com/hpb-project/go-hpb/network/rpc"
	"sync/atomic"
	"github.com/hpb-project/go-hpb/network/p2p/discover"
	"path/filepath"
)

var (
	errClosed            = errors.New("peer set is closed")
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
	errIncomplete        = errors.New("PeerManager is incomplete creation")
)

const (
	maxKnownTxs      = 1000000 // Maximum transactions hashes to keep in the known list (prevent DOS) //for testnet
	maxKnownBlocks   = 100000  // Maximum block hashes to keep in the known list (prevent DOS)  //for testnet
)

// PeerInfo represents a short summary of the Hpb sub-protocol metadata known
// about a connected peer.
type HpbPeerInfo struct {
	Version    uint     `json:"version"`    // Hpb protocol version negotiated
	Difficulty *big.Int `json:"difficulty"` // Total difficulty of the peer's blockchain
	Head       string   `json:"head"`       // SHA3 hash of the peer's best owned block
}

type Peer struct {
	*PeerBase
	rw MsgReadWriter

	id        string
	version   uint
	txsRate   uint
	bandwidth float32

	head common.Hash
	td   *big.Int
	lock sync.RWMutex

	knownTxs    *set.Set // Set of transaction hashes known to be known by this peer
	knownBlocks *set.Set // Set of block hashes known to be known by this peer
}


type PeerManager struct {
	peers  map[string]*Peer
	lock   sync.RWMutex
	closed bool

	rpcmgr *RpcMgr
	server *Server
	hpbpro *HpbProto
}

var INSTANCE = atomic.Value{}

func PeerMgrInst() *PeerManager {
	if INSTANCE.Load() == nil {
		pm :=&PeerManager{
			peers:  make(map[string]*Peer),
			server: &Server{},
			rpcmgr: &RpcMgr{},
			hpbpro: NewProtos(),
		}
		INSTANCE.Store(pm)
	}

	return INSTANCE.Load().(*PeerManager)
}

func (prm *PeerManager)Start() error {

	config, err :=config.GetHpbConfigInstance()
	if err != nil {
		log.Error("Peer manager get config error","error",err)
		return err
	}

	prm.server.Config = Config{
			PrivateKey: config.Node.PrivateKey,
			Name: config.Network.Name,
			SelfNodeType: config.Network.RoleType,
			BootstrapNodes: config.Network.BootstrapNodes,
			//StaticNodes: config.,
			NetRestrict: config.Network.NetRestrict,
			NodeDatabase: config.Network.NodeDatabase,
			ListenAddr: config.Network.ListenAddr,
			NAT: config.Network.NAT,
			EnableMsgEvents: config.Network.EnableMsgEvents,
			NetworkId: config.Node.NetworkId,
			Protocols: prm.hpbpro.Protocols(),
	}

	prm.hpbpro.networkId = config.Node.NetworkId
	copy(prm.server.Protocols, prm.hpbpro.Protocols())

	prm.server.localType = discover.InitNode
	//prm.server.localType = discover.BootNode

	// for-test
	//PrivateKey is not set by node
	log.Info("para from config","PrivateKey",config.Node.PrivateKey)
	prm.server.PrivateKey =config.Node.NodeKeyTemp()
	log.Info("server","PrivateKey",prm.server.PrivateKey)


	if prm.server.PrivateKey == nil {
		log.Error("PrivateKey is nil")
	}

	if err := prm.server.Start(); err != nil {
		log.Error("Hpb protocol","error",err)
		return err
	}

	log.Info("para from config","IpcEndpoint",config.Network.IpcEndpoint,"HttpEndpoint",config.Network.HttpEndpoint,"WsEndpoint",config.Network.WsEndpoint)

	absdatadir, _ := filepath.Abs(config.Node.DataDir)
	config.Node.DataDir = absdatadir
	config.Node.IPCPath = "ghpb.ipc"
	ipcEndpoint:=  config.Node.IPCEndpoint()
	httpEndpoint:= ""
	wsEndpoint  := ""

	prm.rpcmgr    = &RpcMgr{
		ipcEndpoint:  ipcEndpoint,
		httpEndpoint: httpEndpoint,
		wsEndpoint:   wsEndpoint,

		httpCors:     config.Network.HTTPCors,
		httpModules:  config.Network.HTTPModules,

		wsOrigins:    config.Network.WSOrigins,
		wsModules:    config.Network.WSModules,
		wsExposeAll:  config.Network.WSExposeAll,
	}

	prm.rpcmgr.startRPC(config.Node.RpcAPIs)

	return nil
}



func (prm *PeerManager)Stop(){

	prm.Close()
	prm.rpcmgr.stopRPC()

	prm.server.Stop()
	prm.server = nil

}

func (prm *PeerManager)P2pSvr() *Server {
	return prm.server
}

func (prm *PeerManager)IpcHandle() *rpc.Server {
	return prm.rpcmgr.inprocHandler
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
func (prm *PeerManager) Register(p *Peer) error {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	if prm.closed {
		return errClosed
	}
	if _, ok := prm.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	prm.peers[p.id] = p
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (prm *PeerManager) Unregister(id string) error {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	if _, ok := prm.peers[id]; !ok {
		return errNotRegistered
	}
	delete(prm.peers, id)
	return nil
}

// Peer retrieves the registered peer with the given id.
func (prm *PeerManager) Peer(id string) *Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	return prm.peers[id]
}

func (prm *PeerManager) PeersAll() []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		list = append(list, p)
	}
	return list
}

// Len returns if the current number of peers in the set.
func (prm *PeerManager) Len() int {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	return len(prm.peers)
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (prm *PeerManager) PeersWithoutBlock(hash common.Hash) []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		if !p.knownBlocks.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (prm *PeerManager) PeersWithoutTx(hash common.Hash) []*Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	list := make([]*Peer, 0, len(prm.peers))
	for _, p := range prm.peers {
		if !p.knownTxs.Has(hash) {
			list = append(list, p)
		}
	}
	return list
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
func (prm *PeerManager) BestPeer() *Peer {
	prm.lock.RLock()
	defer prm.lock.RUnlock()

	var (
		bestPeer *Peer
		bestTd   *big.Int
	)
	for _, p := range prm.peers {
		if _, td := p.Head(); bestPeer == nil || td.Cmp(bestTd) > 0 {
			bestPeer, bestTd = p, td
		}
	}
	return bestPeer
}

// Close disconnects all peers.
// No new peers can be registered after Close has returned.
func (prm *PeerManager) Close() {
	prm.lock.Lock()
	defer prm.lock.Unlock()

	for _, p := range prm.peers {
		p.Disconnect(DiscQuitting)
	}
	prm.closed = true
}

func (api *PeerManager) Peers() []*PeerInfo {
	return nil
}

func (api *PeerManager) NodeInfo() *NodeInfo {
	return nil
}

func NewPeer(version uint, pr *PeerBase, rw MsgReadWriter) *Peer {
	id := pr.ID()

	return &Peer{
		PeerBase:    pr,
		rw:          rw,
		version:     version,
		id:          fmt.Sprintf("%x", id[:8]),
		knownTxs:    set.New(),
		knownBlocks: set.New(),
	}
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *Peer) Info() *HpbPeerInfo {
	hash, td := p.Head()

	return &HpbPeerInfo{
		Version:    p.version,
		Difficulty: td,
		Head:       hash.Hex(),
	}
}

func (p *Peer) GetID() string {
	return  p.id
}
// Head retrieves a copy of the current head hash and total difficulty of the
// peer.
func (p *Peer) Head() (hash common.Hash, td *big.Int) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	copy(hash[:], p.head[:])
	return hash, new(big.Int).Set(p.td)
}

// SetHead updates the head hash and total difficulty of the peer.
func (p *Peer) SetHead(hash common.Hash, td *big.Int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	copy(p.head[:], hash[:])
	p.td.Set(td)
}

func (p *Peer) TxsRate() uint {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.txsRate
}

func (p *Peer) SetTxsRate(txs uint) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.txsRate = txs
}


func (p *Peer) Bandwidth() float32 {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.bandwidth
}

func (p *Peer) SetBandwidth(bw float32) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.bandwidth = bw
}


func (p *Peer) KnownBlockAdd(hash common.Hash){
	for p.knownBlocks.Size() >= maxKnownBlocks {
		p.knownBlocks.Pop()
	}
	p.knownBlocks.Add(hash)
}

func (p *Peer) KnownBlockHas(hash common.Hash) bool{
	return p.knownBlocks.Has(hash)
}

func (p *Peer) KnownBlockSize() int{
	return p.knownBlocks.Size()
}


func (p *Peer) KnownTxsAdd(hash common.Hash){
	for p.knownTxs.Size() >= maxKnownTxs {
		p.knownTxs.Pop()
	}
	p.knownTxs.Add(hash)
}

func (p *Peer) KnownTxsHas(hash common.Hash) bool{
	return p.knownTxs.Has(hash)
}

func (p *Peer) KnownTxsSize() int{
	return p.knownTxs.Size()
}

func (p *Peer) SendData(msgCode uint64, data interface{}) error {
	return Send(p.rw, msgCode, data)
}


// statusData is the network packet for the status message.
type statusData struct {
	ProtocolVersion uint32
	NetworkId       uint64
	TD              *big.Int
	CurrentBlock    common.Hash
	GenesisBlock    common.Hash
}

// Handshake executes the eth protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis blocks.
func (p *Peer) Handshake(network uint64, td *big.Int, head common.Hash, genesis common.Hash) error {
	// Send out own handshake in a new thread
	errc := make(chan error, 2)
	var status statusData // safe to read after two values have been received from errc

	go func() {
		p.log.Trace("handshake send","NetworkId",network,"TD",td,"CurrentBlock",head,"GenesisBlock",genesis)
		errc <- Send(p.rw, StatusMsg, &statusData{
			ProtocolVersion: uint32(p.version),
			NetworkId:       network,
			TD:              td,
			CurrentBlock:    head,
			GenesisBlock:    genesis,
		})
	}()
	go func() {
		errc <- p.readStatus(network, &status, genesis)
		p.log.Trace("handshake read","NetworkId",status.NetworkId,"TD",status.TD,"CurrentBlock",status.CurrentBlock,"GenesisBlock",status.GenesisBlock)
	}()

	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return DiscReadTimeout
		}
	}
	p.td, p.head = status.TD, status.CurrentBlock
	p.log.Trace("handshake over","td",p.td,"head", p.head)
	return nil
}

func (p *Peer) readStatus(network uint64, status *statusData, genesis common.Hash) (err error) {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != StatusMsg {
		return ErrResp(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, StatusMsg)
	}

	// Decode the handshake and make sure everything matches
	if err := msg.Decode(&status); err != nil {
		return ErrResp(ErrDecode, "msg %v: %v", msg, err)
	}
	if status.GenesisBlock != genesis {
		return ErrResp(ErrGenesisBlockMismatch, "%x (!= %x)", status.GenesisBlock[:8], genesis[:8])
	}
	if status.NetworkId != network {
		return ErrResp(ErrNetworkIdMismatch, "%d (!= %d)", status.NetworkId, network)
	}
	if uint(status.ProtocolVersion) != p.version {
		return ErrResp(ErrProtocolVersionMismatch, "%d (!= %d)", status.ProtocolVersion, p.version)
	}

	return nil
}

// String implements fmt.Stringer.
func (p *Peer) String() string {
	return fmt.Sprintf("Peer %s [%s]", p.id,
		fmt.Sprintf("hpb/%2d", p.version),
	)
}


