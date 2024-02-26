/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 * Copyright (c) 2024 Damian Peckett <damian@pecke.tt>.
 */

package transport

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpeckett/noisytransport/conn"
	"github.com/dpeckett/noisytransport/ratelimiter"
)

type Transport struct {
	state struct {
		// state holds the transport's state. It is accessed atomically.
		// Use the transport.transportState method to read it.
		// transport.transportState does not acquire the mutex, so it captures only a snapshot.
		// During state transitions, the state variable is updated before the transport itself.
		// The state is thus either the current state of the transport or
		// the intended future state of the transport.
		// For example, while executing a call to Up, state will be transportStateUp.
		// There is no guarantee that that intended future state of the transport
		// will become the actual state; Up can fail.
		// The transport can also change state multiple times between time of check and time of use.
		// Unsynchronized uses of state must therefore be advisory/best-effort only.
		state atomic.Uint32 // actually a transportState, but typed uint32 for convenience
		// stopping blocks until all inputs to Transport have been closed.
		stopping sync.WaitGroup
		// mu protects state changes.
		sync.Mutex
	}

	net struct {
		stopping sync.WaitGroup
		sync.RWMutex
		bind conn.Bind // bind interface
		port uint16    // listening port
	}

	staticIdentity struct {
		sync.RWMutex
		privateKey NoisePrivateKey
		publicKey  NoisePublicKey
	}

	peers struct {
		sync.RWMutex // protects keyMap
		keyMap       map[NoisePublicKey]*Peer
	}

	rate struct {
		underLoadUntil atomic.Int64
		limiter        ratelimiter.Ratelimiter
	}

	indexTable    IndexTable
	cookieChecker CookieChecker

	pool struct {
		inboundElementsContainer  *WaitPool
		outboundElementsContainer *WaitPool
		messageBuffers            *WaitPool
		inboundElements           *WaitPool
		outboundElements          *WaitPool
	}

	queue struct {
		encryption *outboundQueue
		decryption *inboundQueue
		handshake  *handshakeQueue
	}

	sourceSink SourceSink

	closed chan struct{}
	log    *slog.Logger
}

// transportState represents the state of a Transport.
// There are three states: down, up, closed.
// Transitions:
//
//	down -----+
//	  ↑↓      ↓
//	  up -> closed
type transportState uint32

const (
	transportStateDown transportState = iota
	transportStateUp
	transportStateClosed
)

func (state transportState) String() string {
	switch state {
	case transportStateDown:
		return "down"
	case transportStateUp:
		return "up"
	case transportStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// transportState returns transport.state.state as a transportState
// See those docs for how to interpret this value.
func (transport *Transport) transportState() transportState {
	return transportState(transport.state.state.Load())
}

// isClosed reports whether the transport is closed (or is closing).
// See transport.state.state comments for how to interpret this value.
func (transport *Transport) isClosed() bool {
	return transport.transportState() == transportStateClosed
}

// isUp reports whether the transport is up (or is attempting to come up).
// See transport.state.state comments for how to interpret this value.
func (transport *Transport) isUp() bool {
	return transport.transportState() == transportStateUp
}

// Must hold transport.peers.Lock()
func removePeerLocked(transport *Transport, peer *Peer, key NoisePublicKey) {
	// stop routing and processing of packets
	peer.Stop()

	// remove from peer map
	delete(transport.peers.keyMap, key)
}

// changeState attempts to change the transport state to match want.
func (transport *Transport) changeState(want transportState) (err error) {
	transport.state.Lock()
	defer transport.state.Unlock()
	old := transport.transportState()
	if old == transportStateClosed {
		// once closed, always closed
		transport.log.Debug("Interface closed, ignored requested state", "transportState", want)
		return nil
	}
	switch want {
	case old:
		return nil
	case transportStateUp:
		transport.state.state.Store(uint32(transportStateUp))
		err = transport.upLocked()
		if err == nil {
			break
		}
		fallthrough // up failed; bring the transport all the way back down
	case transportStateDown:
		transport.state.state.Store(uint32(transportStateDown))
		errDown := transport.downLocked()
		if err == nil {
			err = errDown
		}
	}
	transport.log.Debug("Interface state change requested", "old", old, "want", want, "now", transport.transportState())
	return
}

// upLocked attempts to bring the transport up and reports whether it succeeded.
// The caller must hold transport.state.mu and is responsible for updating transport.state.state.
func (transport *Transport) upLocked() error {
	if err := transport.BindUpdate(); err != nil {
		transport.log.Error("Unable to update bind", "error", err)
		return err
	}

	transport.peers.RLock()
	for _, peer := range transport.peers.keyMap {
		peer.Start()
		if peer.persistentKeepaliveInterval.Load() > 0 {
			if err := peer.SendKeepalive(); err != nil {
				transport.log.Error("Failed to send keepalive", "peer", peer, "error", err)
			}
		}
	}
	transport.peers.RUnlock()
	return nil
}

// downLocked attempts to bring the transport down.
// The caller must hold transport.state.mu and is responsible for updating transport.state.state.
func (transport *Transport) downLocked() error {
	err := transport.BindClose()
	if err != nil {
		transport.log.Error("Bind close failed", "error", err)
	}

	transport.peers.RLock()
	for _, peer := range transport.peers.keyMap {
		peer.Stop()
	}
	transport.peers.RUnlock()
	return err
}

func (transport *Transport) Up() error {
	return transport.changeState(transportStateUp)
}

func (transport *Transport) Down() error {
	return transport.changeState(transportStateDown)
}

func (transport *Transport) IsUnderLoad() bool {
	// check if currently under load
	now := time.Now()
	underLoad := len(transport.queue.handshake.c) >= QueueHandshakeSize/8
	if underLoad {
		transport.rate.underLoadUntil.Store(now.Add(UnderLoadAfterTime).UnixNano())
		return true
	}
	// check if recently under load
	return transport.rate.underLoadUntil.Load() > now.UnixNano()
}

func (transport *Transport) SetPrivateKey(sk NoisePrivateKey) {
	// lock required resources

	transport.staticIdentity.Lock()
	defer transport.staticIdentity.Unlock()

	if sk.Equals(transport.staticIdentity.privateKey) {
		return
	}

	transport.peers.Lock()
	defer transport.peers.Unlock()

	lockedPeers := make([]*Peer, 0, len(transport.peers.keyMap))
	for _, peer := range transport.peers.keyMap {
		peer.handshake.mutex.RLock()
		lockedPeers = append(lockedPeers, peer)
	}

	// remove peers with matching public keys

	publicKey := sk.PublicKey()
	for key, peer := range transport.peers.keyMap {
		if peer.handshake.remoteStatic.Equals(publicKey) {
			peer.handshake.mutex.RUnlock()
			removePeerLocked(transport, peer, key)
			peer.handshake.mutex.RLock()
		}
	}

	// update key material

	transport.staticIdentity.privateKey = sk
	transport.staticIdentity.publicKey = publicKey
	transport.cookieChecker.Init(publicKey)

	// do static-static DH pre-computations

	expiredPeers := make([]*Peer, 0, len(transport.peers.keyMap))
	for _, peer := range transport.peers.keyMap {
		handshake := &peer.handshake
		handshake.precomputedStaticStatic, _ = transport.staticIdentity.privateKey.sharedSecret(handshake.remoteStatic)
		expiredPeers = append(expiredPeers, peer)
	}

	for _, peer := range lockedPeers {
		peer.handshake.mutex.RUnlock()
	}
	for _, peer := range expiredPeers {
		peer.ExpireCurrentKeypairs()
	}
}

func NewTransport(sourceSink SourceSink, bind conn.Bind, logger *slog.Logger) *Transport {
	t := new(Transport)
	t.state.state.Store(uint32(transportStateDown))
	t.closed = make(chan struct{})
	t.log = logger
	t.net.bind = bind
	t.sourceSink = sourceSink
	t.peers.keyMap = make(map[NoisePublicKey]*Peer)
	t.rate.limiter.Init()
	t.indexTable.Init()

	t.PopulatePools()

	// create queues

	t.queue.handshake = newHandshakeQueue()
	t.queue.encryption = newOutboundQueue()
	t.queue.decryption = newInboundQueue()

	// start workers

	cpus := runtime.NumCPU()
	t.state.stopping.Wait()
	t.queue.encryption.wg.Add(cpus) // One for each RoutineHandshake
	for i := 0; i < cpus; i++ {
		go t.RoutineEncryption(i + 1)
		go t.RoutineDecryption(i + 1)
		go t.RoutineHandshake(i + 1)
	}

	t.state.stopping.Add(1)
	t.queue.encryption.wg.Add(1)
	go t.RoutineReadFromSourceSink()

	return t
}

// BatchSize returns the BatchSize for the transport as a whole which is the max of
// the bind batch size and the sink batch size. The batch size reported by transport
// is the size used to construct memory pools, and is the allowed batch size for
// the lifetime of the transport.
func (transport *Transport) BatchSize() int {
	size := transport.net.bind.BatchSize()
	dSize := transport.sourceSink.BatchSize()
	if size < dSize {
		size = dSize
	}
	return size
}

func (transport *Transport) LookupPeer(pk NoisePublicKey) *Peer {
	transport.peers.RLock()
	defer transport.peers.RUnlock()

	return transport.peers.keyMap[pk]
}

func (transport *Transport) RemovePeer(key NoisePublicKey) {
	transport.peers.Lock()
	defer transport.peers.Unlock()
	// stop peer and remove from routing

	peer, ok := transport.peers.keyMap[key]
	if ok {
		removePeerLocked(transport, peer, key)
	}
}

func (transport *Transport) RemoveAllPeers() {
	transport.peers.Lock()
	defer transport.peers.Unlock()

	for key, peer := range transport.peers.keyMap {
		removePeerLocked(transport, peer, key)
	}

	transport.peers.keyMap = make(map[NoisePublicKey]*Peer)
}

func (transport *Transport) Close() error {
	transport.state.Lock()
	defer transport.state.Unlock()
	if transport.isClosed() {
		return nil
	}
	transport.state.state.Store(uint32(transportStateClosed))
	transport.log.Debug("Transport closing")

	_ = transport.sourceSink.Close()
	_ = transport.downLocked()

	// Remove peers before closing queues,
	// because peers assume that queues are active.
	transport.RemoveAllPeers()

	// We kept a reference to the encryption and decryption queues,
	// in case we started any new peers that might write to them.
	// No new peers are coming; we are done with these queues.
	transport.queue.encryption.wg.Done()
	transport.queue.decryption.wg.Done()
	transport.queue.handshake.wg.Done()
	transport.state.stopping.Wait()

	if err := transport.rate.limiter.Close(); err != nil {
		return fmt.Errorf("failed to close rate limiter: %w", err)
	}

	transport.log.Debug("Transport closed")
	close(transport.closed)

	return nil
}

func (transport *Transport) Wait() chan struct{} {
	return transport.closed
}

func (transport *Transport) SendKeepalivesToPeersWithCurrentKeypair() {
	if !transport.isUp() {
		return
	}

	transport.peers.RLock()
	for _, peer := range transport.peers.keyMap {
		peer.keypairs.RLock()
		sendKeepalive := peer.keypairs.current != nil && !peer.keypairs.current.created.Add(RejectAfterTime).Before(time.Now())
		peer.keypairs.RUnlock()
		if sendKeepalive {
			if err := peer.SendKeepalive(); err != nil {
				transport.log.Error("Failed to send keepalive", "peer", peer, "error", err)
			}
		}
	}
	transport.peers.RUnlock()
}

// closeBindLocked closes the transport's net.bind.
// The caller must hold the net mutex.
func closeBindLocked(transport *Transport) error {
	var err error
	netc := &transport.net
	if netc.bind != nil {
		err = netc.bind.Close()
	}
	netc.stopping.Wait()
	return err
}

func (transport *Transport) Bind() conn.Bind {
	transport.net.Lock()
	defer transport.net.Unlock()
	return transport.net.bind
}

func (transport *Transport) BindUpdate() error {
	transport.net.Lock()
	defer transport.net.Unlock()

	// close existing sockets
	if err := closeBindLocked(transport); err != nil {
		return err
	}

	// open new sockets
	if !transport.isUp() {
		return nil
	}

	// bind to new port
	var err error
	var recvFns []conn.ReceiveFunc
	netc := &transport.net

	recvFns, netc.port, err = netc.bind.Open(netc.port)
	if err != nil {
		netc.port = 0
		return err
	}

	// start receiving routines
	transport.net.stopping.Add(len(recvFns))
	transport.queue.decryption.wg.Add(len(recvFns)) // each RoutineReceiveIncoming goroutine writes to transport.queue.decryption
	transport.queue.handshake.wg.Add(len(recvFns))  // each RoutineReceiveIncoming goroutine writes to transport.queue.handshake
	batchSize := netc.bind.BatchSize()
	for _, fn := range recvFns {
		go transport.RoutineReceiveIncoming(batchSize, fn)
	}

	transport.log.Debug("UDP bind has been updated")
	return nil
}

func (transport *Transport) BindClose() error {
	transport.net.Lock()
	err := closeBindLocked(transport)
	transport.net.Unlock()
	return err
}

func (transport *Transport) UpdatePort(port uint16) error {
	transport.net.Lock()
	transport.net.port = port
	transport.net.Unlock()

	if err := transport.BindUpdate(); err != nil {
		return fmt.Errorf("failed to update port: %w", err)
	}

	return nil
}
