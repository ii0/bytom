package p2p

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	crypto "github.com/tendermint/go-crypto"
	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"

	cfg "github.com/bytom/config"
	"github.com/bytom/errors"
	"github.com/bytom/p2p/connection"
	"github.com/bytom/p2p/trust"
)

const (
	bannedPeerKey      = "BannedPeer"
	defaultBanDuration = time.Hour * 1
)

//pre-define errors for connecting fail
var (
	ErrDuplicatePeer     = errors.New("Duplicate peer")
	ErrConnectSelf       = errors.New("Connect self")
	ErrConnectBannedPeer = errors.New("Connect banned peer")
)

// An AddrBook represents an address book from the pex package, which is used to store peer addresses.
type AddrBook interface {
	AddAddress(*NetAddress, *NetAddress) error
	AddOurAddress(*NetAddress)
	MarkGood(*NetAddress)
	RemoveAddress(*NetAddress)
	SaveToFile() error
}

//-----------------------------------------------------------------------------

// Switch handles peer connections and exposes an API to receive incoming messages
// on `Reactors`.  Each `Reactor` is responsible for handling incoming messages of one
// or more `Channels`.  So while sending outgoing messages is typically performed on the peer,
// incoming messages are received on the reactor.
type Switch struct {
	cmn.BaseService

	Config       *cfg.P2PConfig
	peerConfig   *PeerConfig
	listeners    []Listener
	reactors     map[string]Reactor
	chDescs      []*connection.ChannelDescriptor
	reactorsByCh map[byte]Reactor
	peers        *PeerSet
	dialing      *cmn.CMap
	nodeInfo     *NodeInfo             // our node info
	nodePrivKey  crypto.PrivKeyEd25519 // our node privkey
	addrBook     AddrBook
	bannedPeer   map[string]time.Time
	db           dbm.DB
	mtx          sync.Mutex
}

// NewSwitch creates a new Switch with the given config.
func NewSwitch(config *cfg.P2PConfig, addrBook AddrBook, trustHistoryDB dbm.DB) *Switch {
	sw := &Switch{
		Config:       config,
		peerConfig:   DefaultPeerConfig(config),
		reactors:     make(map[string]Reactor),
		chDescs:      make([]*connection.ChannelDescriptor, 0),
		reactorsByCh: make(map[byte]Reactor),
		peers:        NewPeerSet(),
		dialing:      cmn.NewCMap(),
		nodeInfo:     nil,
		addrBook:     addrBook,
		db:           trustHistoryDB,
	}
	sw.BaseService = *cmn.NewBaseService(nil, "P2P Switch", sw)
	sw.bannedPeer = make(map[string]time.Time)
	if datajson := sw.db.Get([]byte(bannedPeerKey)); datajson != nil {
		if err := json.Unmarshal(datajson, &sw.bannedPeer); err != nil {
			return nil
		}
	}
	trust.Init()
	return sw
}

// OnStart implements BaseService. It starts all the reactors, peers, and listeners.
func (sw *Switch) OnStart() error {
	for _, reactor := range sw.reactors {
		if _, err := reactor.Start(); err != nil {
			return err
		}
	}
	for _, listener := range sw.listeners {
		go sw.listenerRoutine(listener)
	}
	return nil
}

// OnStop implements BaseService. It stops all listeners, peers, and reactors.
func (sw *Switch) OnStop() {
	for _, listener := range sw.listeners {
		listener.Stop()
	}
	sw.listeners = nil

	for _, peer := range sw.peers.List() {
		peer.Stop()
		sw.peers.Remove(peer)
	}

	for _, reactor := range sw.reactors {
		reactor.Stop()
	}
}

//AddBannedPeer add peer to blacklist
func (sw *Switch) AddBannedPeer(peer *Peer) error {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()

	key := peer.NodeInfo.RemoteAddrHost()
	sw.bannedPeer[key] = time.Now().Add(defaultBanDuration)
	datajson, err := json.Marshal(sw.bannedPeer)
	if err != nil {
		return err
	}

	sw.db.Set([]byte(bannedPeerKey), datajson)
	return nil
}

// AddPeer performs the P2P handshake with a peer
// that already has a SecretConnection. If all goes well,
// it starts the peer and adds it to the switch.
// NOTE: This performs a blocking handshake before the peer is added.
// CONTRACT: If error is returned, peer is nil, and conn is immediately closed.
func (sw *Switch) AddPeer(pc *peerConn) error {
	peerNodeInfo, err := pc.HandshakeTimeout(sw.nodeInfo, time.Duration(sw.peerConfig.HandshakeTimeout*time.Second))
	if err != nil {
		return err
	}

	if err := sw.nodeInfo.CompatibleWith(peerNodeInfo); err != nil {
		return err
	}

	peer := newPeer(pc, peerNodeInfo, sw.reactorsByCh, sw.chDescs, sw.StopPeerForError)
	if err := sw.filterConnByPeer(peer); err != nil {
		return err
	}

	// Start peer
	if sw.IsRunning() {
		if err := sw.startInitPeer(peer); err != nil {
			return err
		}
	}
	return sw.peers.Add(peer)
}

// AddReactor adds the given reactor to the switch.
// NOTE: Not goroutine safe.
func (sw *Switch) AddReactor(name string, reactor Reactor) Reactor {
	// Validate the reactor.
	// No two reactors can share the same channel.
	for _, chDesc := range reactor.GetChannels() {
		chID := chDesc.ID
		if sw.reactorsByCh[chID] != nil {
			cmn.PanicSanity(fmt.Sprintf("Channel %X has multiple reactors %v & %v", chID, sw.reactorsByCh[chID], reactor))
		}
		sw.chDescs = append(sw.chDescs, chDesc)
		sw.reactorsByCh[chID] = reactor
	}
	sw.reactors[name] = reactor
	reactor.SetSwitch(sw)
	return reactor
}

// AddListener adds the given listener to the switch for listening to incoming peer connections.
// NOTE: Not goroutine safe.
func (sw *Switch) AddListener(l Listener) {
	sw.listeners = append(sw.listeners, l)
}

//DialPeerWithAddress dial node from net address
func (sw *Switch) DialPeerWithAddress(addr *NetAddress) error {
	log.Debug("Dialing peer address:", addr)
	sw.dialing.Set(addr.IP.String(), addr)
	defer sw.dialing.Delete(addr.IP.String())
	if err := sw.filterConnByIP(addr.IP.String()); err != nil {
		return err
	}

	pc, err := newOutboundPeerConn(addr, sw.nodePrivKey, sw.peerConfig)
	if err != nil {
		log.WithFields(log.Fields{"address": addr, " err": err}).Debug("DialPeer fail on newOutboundPeerConn")
		return err
	}

	if err = sw.AddPeer(pc); err != nil {
		log.WithFields(log.Fields{"address": addr, " err": err}).Debug("DialPeer fail on switch AddPeer")
		pc.CloseConn()
		return err
	}
	log.Debug("DialPeer added peer:", addr)
	return nil
}

//IsDialing prevent duplicate dialing
func (sw *Switch) IsDialing(addr *NetAddress) bool {
	return sw.dialing.Has(addr.IP.String())
}

// IsListening returns true if the switch has at least one listener.
// NOTE: Not goroutine safe.
func (sw *Switch) IsListening() bool {
	return len(sw.listeners) > 0
}

// Listeners returns the list of listeners the switch listens on.
// NOTE: Not goroutine safe.
func (sw *Switch) Listeners() []Listener {
	return sw.listeners
}

// NumPeers Returns the count of outbound/inbound and outbound-dialing peers.
func (sw *Switch) NumPeers() (outbound, inbound, dialing int) {
	peers := sw.peers.List()
	for _, peer := range peers {
		if peer.outbound {
			outbound++
		} else {
			inbound++
		}
	}
	dialing = sw.dialing.Size()
	return
}

// NodeInfo returns the switch's NodeInfo.
// NOTE: Not goroutine safe.
func (sw *Switch) NodeInfo() *NodeInfo {
	return sw.nodeInfo
}

//Peers return switch peerset
func (sw *Switch) Peers() *PeerSet {
	return sw.peers
}

// SetNodeInfo sets the switch's NodeInfo for checking compatibility and handshaking with other nodes.
// NOTE: Not goroutine safe.
func (sw *Switch) SetNodeInfo(nodeInfo *NodeInfo) {
	sw.nodeInfo = nodeInfo
}

// SetNodePrivKey sets the switch's private key for authenticated encryption.
// NOTE: Not goroutine safe.
func (sw *Switch) SetNodePrivKey(nodePrivKey crypto.PrivKeyEd25519) {
	sw.nodePrivKey = nodePrivKey
	if sw.nodeInfo != nil {
		sw.nodeInfo.PubKey = nodePrivKey.PubKey().Unwrap().(crypto.PubKeyEd25519)
	}
}

// StopPeerForError disconnects from a peer due to external error.
func (sw *Switch) StopPeerForError(peer *Peer, reason interface{}) {
	log.WithFields(log.Fields{"peer": peer, " err": reason}).Debug("stopping peer for error")
	sw.stopAndRemovePeer(peer, reason)
}

// StopPeerGracefully disconnect from a peer gracefully.
func (sw *Switch) StopPeerGracefully(peer *Peer) {
	sw.stopAndRemovePeer(peer, nil)
}

func (sw *Switch) addPeerWithConnection(conn net.Conn) error {
	peerConn, err := newInboundPeerConn(conn, sw.nodePrivKey, sw.Config)
	if err != nil {
		conn.Close()
		return err
	}

	if err = sw.AddPeer(peerConn); err != nil {
		conn.Close()
		return err
	}
	return nil
}

func (sw *Switch) addrBookDelSelf() error {
	addr, err := NewNetAddressString(sw.nodeInfo.ListenAddr)
	if err != nil {
		return err
	}

	sw.addrBook.RemoveAddress(addr)
	sw.addrBook.AddOurAddress(addr)
	return nil
}

func (sw *Switch) checkBannedPeer(peer string) error {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()

	if banEnd, ok := sw.bannedPeer[peer]; ok {
		if time.Now().Before(banEnd) {
			return ErrConnectBannedPeer
		}
		sw.delBannedPeer(peer)
	}
	return nil
}

func (sw *Switch) delBannedPeer(addr string) error {
	sw.mtx.Lock()
	defer sw.mtx.Unlock()

	delete(sw.bannedPeer, addr)
	datajson, err := json.Marshal(sw.bannedPeer)
	if err != nil {
		return err
	}

	sw.db.Set([]byte(bannedPeerKey), datajson)
	return nil
}

func (sw *Switch) filterConnByIP(ip string) error {
	if ip == sw.nodeInfo.ListenHost() {
		sw.addrBookDelSelf()
		return ErrConnectSelf
	}
	return sw.checkBannedPeer(ip)
}

func (sw *Switch) filterConnByPeer(peer *Peer) error {
	if err := sw.checkBannedPeer(peer.RemoteAddrHost()); err != nil {
		return err
	}

	if sw.nodeInfo.PubKey.Equals(peer.PubKey().Wrap()) {
		sw.addrBookDelSelf()
		return ErrConnectSelf
	}

	if sw.peers.Has(peer.Key) {
		return ErrDuplicatePeer
	}
	return nil
}

func (sw *Switch) listenerRoutine(l Listener) {
	for {
		inConn, ok := <-l.Connections()
		if !ok {
			break
		}

		// disconnect if we alrady have 2 * MaxNumPeers, we do this because we wanna address book get exchanged even if
		// the connect is full. The pex will disconnect the peer after address exchange, the max connected peer won't
		// be double of MaxNumPeers
		if sw.peers.Size() >= sw.Config.MaxNumPeers*2 {
			inConn.Close()
			log.Info("Ignoring inbound connection: already have enough peers.")
			continue
		}

		// New inbound connection!
		if err := sw.addPeerWithConnection(inConn); err != nil {
			log.Info("Ignoring inbound connection: error while adding peer.", " address:", inConn.RemoteAddr().String(), " error:", err)
			continue
		}
	}
}

func (sw *Switch) startInitPeer(peer *Peer) error {
	peer.Start() // spawn send/recv routines
	for _, reactor := range sw.reactors {
		if err := reactor.AddPeer(peer); err != nil {
			return err
		}
	}
	return nil
}

func (sw *Switch) stopAndRemovePeer(peer *Peer, reason interface{}) {
	for _, reactor := range sw.reactors {
		reactor.RemovePeer(peer, reason)
	}
	sw.peers.Remove(peer)
	peer.Stop()
}
