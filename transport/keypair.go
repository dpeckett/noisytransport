/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 * Copyright (c) 2024 Damian Peckett <damian@pecke.tt>.
 */

package transport

import (
	"crypto/cipher"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpeckett/noisytransport/replay"
)

/* Due to limitations in Go and /x/crypto there is currently
 * no way to ensure that key material is securely ereased in memory.
 *
 * Since this may harm the forward secrecy property,
 * we plan to resolve this issue; whenever Go allows us to do so.
 */

type Keypair struct {
	sendNonce    atomic.Uint64
	send         cipher.AEAD
	receive      cipher.AEAD
	replayFilter replay.Filter
	isInitiator  bool
	created      time.Time
	localIndex   uint32
	remoteIndex  uint32
}

type Keypairs struct {
	sync.RWMutex
	current  *Keypair
	previous *Keypair
	next     atomic.Pointer[Keypair]
}

func (kp *Keypairs) Current() *Keypair {
	kp.RLock()
	defer kp.RUnlock()
	return kp.current
}

func (transport *Transport) DeleteKeypair(key *Keypair) {
	if key != nil {
		transport.indexTable.Delete(key.localIndex)
	}
}
