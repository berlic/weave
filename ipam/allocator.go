package ipam

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"sort"
	"time"

	"github.com/weaveworks/mesh"

	"github.com/weaveworks/weave/common"
	"github.com/weaveworks/weave/db"
	"github.com/weaveworks/weave/ipam/monitor"
	"github.com/weaveworks/weave/ipam/paxos"
	"github.com/weaveworks/weave/ipam/ring"
	"github.com/weaveworks/weave/ipam/space"
	"github.com/weaveworks/weave/net/address"
)

// Kinds of message we can unicast to other peers
const (
	msgSpaceRequest = iota
	msgRingUpdate
	msgSpaceRequestDenied

	tickInterval         = time.Second * 5
	MinSubnetSize        = 4 // first and last addresses are excluded, so 2 would be too small
	containerDiedTimeout = time.Second * 30
)

// operation represents something which Allocator wants to do, but
// which may need to wait until some other message arrives.
type operation interface {
	// Try attempts this operations and returns false if needs to be tried again.
	Try(alloc *Allocator) bool

	Cancel()

	// Does this operation pertain to the given container id?
	// Used for tidying up pending operations when containers die.
	ForContainer(ident string) bool
}

// Allocator brings together Ring and space.Set, and does the
// necessary plumbing.  Runs as a single-threaded Actor, so no locks
// are used around data structures.
type Allocator struct {
	actionChan        chan<- func()
	ourName           mesh.PeerName
	universe          address.Range             // superset of all ranges
	ring              *ring.Ring                // information on ranges owned by all peers
	space             space.Space               // more detail on ranges owned by us
	owned             map[string][]address.CIDR // who owns what addresses, indexed by container-ID
	nicknames         map[mesh.PeerName]string  // so we can map nicknames for rmpeer
	pendingAllocates  []operation               // held until we get some free space
	pendingClaims     []operation               // held until we know who owns the space
	dead              map[string]time.Time      // containers we heard were dead, and when
	db                db.DB                     // persistence
	gossip            mesh.Gossip               // our link to the outside world for sending messages
	paxos             paxos.Participant
	awaitingConsensus bool
	ticker            *time.Ticker
	shuttingDown      bool // to avoid doing any requests while trying to shut down
	isKnownPeer       func(mesh.PeerName) bool
	now               func() time.Time
	isCIDRAligned     bool            // should donate only CIDR aligned ranges
	monitor           monitor.Monitor // monitor tracks changes in address ranges owned by us
}

// NewAllocator creates and initialises a new Allocator
func NewAllocator(ourName mesh.PeerName, ourUID mesh.PeerUID, ourNickname string, universe address.Range, quorum uint, db db.DB, isKnownPeer func(name mesh.PeerName) bool, isCIDRAligned bool, mon monitor.Monitor) *Allocator {
	return &Allocator{
		ourName:       ourName,
		universe:      universe,
		ring:          ring.New(universe.Start, universe.End, ourName),
		owned:         make(map[string][]address.CIDR),
		db:            db,
		paxos:         paxos.NewParticipant(ourName, ourUID, quorum),
		nicknames:     map[mesh.PeerName]string{ourName: ourNickname},
		isKnownPeer:   isKnownPeer,
		dead:          make(map[string]time.Time),
		now:           time.Now,
		isCIDRAligned: isCIDRAligned,
		monitor:       mon,
	}
}

// Start runs the allocator goroutine
func (alloc *Allocator) Start() {
	alloc.loadPersistedData()
	actionChan := make(chan func(), mesh.ChannelSize)
	alloc.actionChan = actionChan
	alloc.ticker = time.NewTicker(tickInterval)
	go alloc.actorLoop(actionChan)
}

// Stop makes the actor routine exit, for test purposes ONLY because any
// calls after this is processed will hang. Async.
func (alloc *Allocator) Stop() {
	alloc.ticker.Stop()
	alloc.actionChan <- nil
}

// Operation life cycle

// Given an operation, try it, and add it to the pending queue if it didn't succeed
func (alloc *Allocator) doOperation(op operation, ops *[]operation) {
	alloc.actionChan <- func() {
		if alloc.shuttingDown {
			op.Cancel()
			return
		}
		if !op.Try(alloc) {
			*ops = append(*ops, op)
		}
	}
}

// Given an operation, remove it from the pending queue
//  Note the op may not be on the queue; it may have
//  already succeeded.  If it is on the queue, we call
//  cancel on it, allowing callers waiting for the resultChans
//  to unblock.
func (alloc *Allocator) cancelOp(op operation, ops *[]operation) {
	for i, op := range *ops {
		if op == op {
			*ops = append((*ops)[:i], (*ops)[i+1:]...)
			op.Cancel()
			break
		}
	}
}

// Cancel all operations in a queue
func (alloc *Allocator) cancelOps(ops *[]operation) {
	for _, op := range *ops {
		op.Cancel()
	}
	*ops = []operation{}
}

// Cancel all operations for a given container id, returns true
// if we found any.
func (alloc *Allocator) cancelOpsFor(ops *[]operation, ident string) bool {
	var found bool
	for i := 0; i < len(*ops); {
		if op := (*ops)[i]; op.ForContainer(ident) {
			found = true
			op.Cancel()
			*ops = append((*ops)[:i], (*ops)[i+1:]...)
		} else {
			i++
		}
	}
	return found
}

// Try all operations in a queue
func (alloc *Allocator) tryOps(ops *[]operation) {
	for i := 0; i < len(*ops); {
		op := (*ops)[i]
		if !op.Try(alloc) {
			i++
			continue
		}
		*ops = append((*ops)[:i], (*ops)[i+1:]...)
	}
}

// Try all pending operations
func (alloc *Allocator) tryPendingOps() {
	// Process existing claims before servicing new allocations
	alloc.tryOps(&alloc.pendingClaims)
	alloc.tryOps(&alloc.pendingAllocates)
}

func (alloc *Allocator) spaceRequestDenied(sender mesh.PeerName, r address.Range) {
	for i := 0; i < len(alloc.pendingClaims); {
		claim := alloc.pendingClaims[i].(*claim)
		if r.Contains(claim.cidr.Addr) {
			claim.deniedBy(alloc, sender)
			alloc.pendingClaims = append(alloc.pendingClaims[:i], alloc.pendingClaims[i+1:]...)
			continue
		}
		i++
	}
}

type errorCancelled struct {
	kind  string
	ident string
}

func (e *errorCancelled) Error() string {
	return fmt.Sprintf("%s request for %s cancelled", e.kind, e.ident)
}

// Actor client API

// Allocate (Sync) - get new IP address for container with given name in range
// if there isn't any space in that range we block indefinitely
func (alloc *Allocator) Allocate(ident string, r address.CIDR, hasBeenCancelled func() bool) (address.Address, error) {
	resultChan := make(chan allocateResult)
	op := &allocate{resultChan: resultChan, ident: ident, r: r, hasBeenCancelled: hasBeenCancelled}
	alloc.doOperation(op, &alloc.pendingAllocates)
	result := <-resultChan
	return result.addr, result.err
}

// Lookup (Sync) - get existing IP addresses for container with given name in range
func (alloc *Allocator) Lookup(ident string, r address.Range) ([]address.CIDR, error) {
	resultChan := make(chan []address.CIDR)
	alloc.actionChan <- func() {
		resultChan <- alloc.ownedInRange(ident, r)
	}
	return <-resultChan, nil
}

// Claim an address that we think we should own (Sync)
func (alloc *Allocator) Claim(ident string, cidr address.CIDR, noErrorOnUnknown bool) error {
	resultChan := make(chan error)
	op := &claim{resultChan: resultChan, ident: ident, cidr: cidr, noErrorOnUnknown: noErrorOnUnknown}
	alloc.doOperation(op, &alloc.pendingClaims)
	return <-resultChan
}

// ContainerDied called from the updater interface.  Async.
func (alloc *Allocator) ContainerDied(ident string) {
	alloc.actionChan <- func() {
		if alloc.hasOwned(ident) {
			alloc.debugln("Container", ident, "died; noting to remove later")
			alloc.dead[ident] = alloc.now()
		}
		// Also remove any pending ops
		alloc.cancelOpsFor(&alloc.pendingAllocates, ident)
		alloc.cancelOpsFor(&alloc.pendingClaims, ident)
	}
}

// ContainerDestroyed called from the updater interface.  Async.
func (alloc *Allocator) ContainerDestroyed(ident string) {
	alloc.actionChan <- func() {
		if alloc.hasOwned(ident) {
			alloc.debugln("Container", ident, "destroyed; removing addresses")
			alloc.delete(ident)
			delete(alloc.dead, ident)
		}
	}
}

func (alloc *Allocator) removeDeadContainers() {
	cutoff := alloc.now().Add(-containerDiedTimeout)
	for ident, timeOfDeath := range alloc.dead {
		if timeOfDeath.Before(cutoff) {
			if err := alloc.delete(ident); err == nil {
				alloc.debugln("Removed addresses for container", ident)
			}
			delete(alloc.dead, ident)
		}
	}
}

func (alloc *Allocator) ContainerStarted(ident string) {
	alloc.actionChan <- func() {
		delete(alloc.dead, ident) // delete is no-op if key not in map
	}
}

func (alloc *Allocator) AllContainerIDs(ids []string) {
	alloc.actionChan <- func() {
		idmap := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			idmap[id] = struct{}{}
		}
		alloc.syncOwned(idmap)
	}
}

// Delete (Sync) - release all IP addresses for container with given name
func (alloc *Allocator) Delete(ident string) error {
	errChan := make(chan error)
	alloc.actionChan <- func() {
		errChan <- alloc.delete(ident)
	}
	return <-errChan
}

func (alloc *Allocator) delete(ident string) error {
	cidrs := alloc.removeAllOwned(ident)
	if len(cidrs) == 0 {
		return fmt.Errorf("Delete: no addresses for %s", ident)
	}
	for _, cidr := range cidrs {
		alloc.space.Free(cidr.Addr)
	}
	return nil
}

// Free (Sync) - release single IP address for container
func (alloc *Allocator) Free(ident string, addrToFree address.Address) error {
	errChan := make(chan error)
	alloc.actionChan <- func() {
		if alloc.removeOwned(ident, addrToFree) {
			alloc.debugln("Freed", addrToFree, "for", ident)
			alloc.space.Free(addrToFree)
			errChan <- nil
			return
		}

		errChan <- fmt.Errorf("Free: address %s not found for %s", addrToFree, ident)
	}
	return <-errChan
}

func (alloc *Allocator) pickPeerFromNicknames(isValid func(mesh.PeerName) bool) mesh.PeerName {
	for name := range alloc.nicknames {
		if name != alloc.ourName && isValid(name) {
			return name
		}
	}
	return mesh.UnknownPeerName
}

func (alloc *Allocator) pickPeerForTransfer() mesh.PeerName {
	// first try alive peers that actively participate in IPAM (i.e. have entries)
	if heir := alloc.ring.PickPeerForTransfer(alloc.isKnownPeer); heir != mesh.UnknownPeerName {
		return heir
	}
	// next try alive peers that have IPAM enabled but have no entries
	if heir := alloc.pickPeerFromNicknames(alloc.isKnownPeer); heir != mesh.UnknownPeerName {
		return heir
	}
	// next try disappeared peers that still have entries
	t := func(mesh.PeerName) bool { return true }
	if heir := alloc.ring.PickPeerForTransfer(t); heir != mesh.UnknownPeerName {
		return heir
	}
	// finally, disappeared peers that passively participated in IPAM
	return alloc.pickPeerFromNicknames(t)
}

// Shutdown (Sync)
func (alloc *Allocator) Shutdown() {
	alloc.infof("Shutdown")
	doneChan := make(chan struct{})
	alloc.actionChan <- func() {
		alloc.shuttingDown = true
		alloc.cancelOps(&alloc.pendingClaims)
		alloc.cancelOps(&alloc.pendingAllocates)
		if heir := alloc.pickPeerForTransfer(); heir != mesh.UnknownPeerName {
			alloc.ring.Transfer(alloc.ourName, heir)
			alloc.space.Clear()
			alloc.gossip.GossipBroadcast(alloc.Gossip())
			time.Sleep(100 * time.Millisecond)
		}
		doneChan <- struct{}{}
	}
	<-doneChan
}

// AdminTakeoverRanges (Sync) - take over the ranges owned by a given peer.
// Only done on adminstrator command.
func (alloc *Allocator) AdminTakeoverRanges(peerNameOrNickname string) error {
	resultChan := make(chan error)
	alloc.actionChan <- func() {
		peername, err := alloc.lookupPeername(peerNameOrNickname)
		if err != nil {
			resultChan <- fmt.Errorf("Cannot find peer '%s'", peerNameOrNickname)
			return
		}

		alloc.debugln("AdminTakeoverRanges:", peername)
		if peername == alloc.ourName {
			resultChan <- fmt.Errorf("Cannot take over ranges from yourself!")
			return
		}

		newRanges, err := alloc.ring.Transfer(peername, alloc.ourName)
		alloc.space.AddRanges(newRanges)
		resultChan <- err
	}
	return <-resultChan
}

// Lookup a PeerName by nickname or stringified PeerName.  We can't
// call into the router for this because we are interested in peers
// that have gone away but are still in the ring, which is why we
// maintain our own nicknames map.
func (alloc *Allocator) lookupPeername(name string) (mesh.PeerName, error) {
	for peername, nickname := range alloc.nicknames {
		if nickname == name {
			return peername, nil
		}
	}

	return mesh.PeerNameFromString(name)
}

// Restrict the peers in "nicknames" to those in the ring plus peers known to the router
func (alloc *Allocator) pruneNicknames() {
	ringPeers := alloc.ring.PeerNames()
	for name := range alloc.nicknames {
		if _, ok := ringPeers[name]; !ok && !alloc.isKnownPeer(name) {
			delete(alloc.nicknames, name)
		}
	}
}

func (alloc *Allocator) annotatePeernames(names []mesh.PeerName) []string {
	var res []string
	for _, name := range names {
		if nickname, found := alloc.nicknames[name]; found {
			res = append(res, fmt.Sprint(name, "(", nickname, ")"))
		} else {
			res = append(res, name.String())
		}
	}
	return res
}

func decodeRange(msg []byte) (r address.Range, err error) {
	decoder := gob.NewDecoder(bytes.NewReader(msg))
	return r, decoder.Decode(&r)
}

// OnGossipUnicast (Sync)
func (alloc *Allocator) OnGossipUnicast(sender mesh.PeerName, msg []byte) error {
	alloc.debugln("OnGossipUnicast from", sender, ": ", len(msg), "bytes")
	resultChan := make(chan error)
	alloc.actionChan <- func() {
		switch msg[0] {
		case msgSpaceRequest:
			// some other peer asked us for space
			r, err := decodeRange(msg[1:])
			if err == nil {
				alloc.donateSpace(r, sender)
			}
			resultChan <- err
		case msgSpaceRequestDenied:
			r, err := decodeRange(msg[1:])
			if err == nil {
				alloc.spaceRequestDenied(sender, r)
			}
			resultChan <- err
		case msgRingUpdate:
			resultChan <- alloc.update(sender, msg[1:])
		}
	}
	return <-resultChan
}

// OnGossipBroadcast (Sync)
func (alloc *Allocator) OnGossipBroadcast(sender mesh.PeerName, msg []byte) (mesh.GossipData, error) {
	alloc.debugln("OnGossipBroadcast from", sender, ":", len(msg), "bytes")
	resultChan := make(chan error)
	alloc.actionChan <- func() {
		resultChan <- alloc.update(sender, msg)
	}
	return alloc.Gossip(), <-resultChan
}

type gossipState struct {
	// We send a timstamp along with the information to be
	// gossipped in order to detect skewed clocks
	Now       int64
	Nicknames map[mesh.PeerName]string

	Paxos paxos.GossipState
	Ring  *ring.Ring
}

func (alloc *Allocator) encode() []byte {
	data := gossipState{
		Now:       alloc.now().Unix(),
		Nicknames: alloc.nicknames,
	}

	// We're only interested in Paxos until we have a Ring.
	// Non-electing participants (e.g. observers) return
	// a nil gossip state in order to provoke a unicast ring
	// update from electing peers who have reached consensus.
	if alloc.ring.Empty() {
		data.Paxos = alloc.paxos.GossipState()
	} else {
		data.Ring = alloc.ring
	}
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	if err := enc.Encode(data); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Encode (Sync)
func (alloc *Allocator) Encode() []byte {
	resultChan := make(chan []byte)
	alloc.actionChan <- func() {
		resultChan <- alloc.encode()
	}
	return <-resultChan
}

// OnGossip (Sync)
func (alloc *Allocator) OnGossip(msg []byte) (mesh.GossipData, error) {
	alloc.debugln("Allocator.OnGossip:", len(msg), "bytes")
	resultChan := make(chan error)
	alloc.actionChan <- func() {
		resultChan <- alloc.update(mesh.UnknownPeerName, msg)
	}
	return nil, <-resultChan // for now, we never propagate updates. TBD
}

// GossipData implementation is trivial - we always gossip the latest
// data we have at time of sending
type ipamGossipData struct {
	alloc *Allocator
}

func (d *ipamGossipData) Merge(other mesh.GossipData) mesh.GossipData {
	return d // no-op
}

func (d *ipamGossipData) Encode() [][]byte {
	return [][]byte{d.alloc.Encode()}
}

// Gossip returns a GossipData implementation, which in this case always
// returns the latest ring state (and does nothing on merge)
func (alloc *Allocator) Gossip() mesh.GossipData {
	return &ipamGossipData{alloc}
}

// SetInterfaces gives the allocator two interfaces for talking to the outside world
func (alloc *Allocator) SetInterfaces(gossip mesh.Gossip) {
	alloc.gossip = gossip
}

// ACTOR server

func (alloc *Allocator) actorLoop(actionChan <-chan func()) {
	for {
		select {
		case action := <-actionChan:
			if action == nil {
				return
			}
			action()
		case <-alloc.ticker.C:
			if alloc.awaitingConsensus {
				alloc.propose()
			}
			alloc.removeDeadContainers()
			alloc.tryPendingOps()
		}

		alloc.assertInvariants()
		alloc.reportFreeSpace()
	}
}

// Helper functions

// Ensure we are making progress towards an established ring
func (alloc *Allocator) establishRing() {
	if !alloc.ring.Empty() || alloc.awaitingConsensus {
		return
	}

	alloc.awaitingConsensus = true
	alloc.propose()
	if ok, cons := alloc.paxos.Consensus(); ok {
		// If the quorum was 1, then proposing immediately
		// leads to consensus
		alloc.createRing(cons.Value)
	}
}

func (alloc *Allocator) createRing(peers []mesh.PeerName) {
	alloc.debugln("Paxos consensus:", peers)
	alloc.ring.ClaimForPeers(normalizeConsensus(peers), alloc.isCIDRAligned)
	// We assume that the peer has not possessed any address ranges before
	alloc.monitor.HandleUpdate(nil, alloc.ring.OwnedRanges())
	alloc.gossip.GossipBroadcast(alloc.Gossip())
	alloc.ringUpdated()
}

func (alloc *Allocator) ringUpdated() {
	// When we have a ring, we don't need paxos any more
	if alloc.awaitingConsensus {
		alloc.awaitingConsensus = false
		alloc.paxos = nil
	}

	alloc.persistRing()
	alloc.space.UpdateRanges(alloc.ring.OwnedRanges())
	alloc.tryPendingOps()
}

// For compatibility with sort.Interface
type peerNames []mesh.PeerName

func (a peerNames) Len() int           { return len(a) }
func (a peerNames) Less(i, j int) bool { return a[i] < a[j] }
func (a peerNames) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// When we get a consensus from Paxos, the peer names are not in a
// defined order and may contain duplicates.  This function sorts them
// and de-dupes.
func normalizeConsensus(consensus []mesh.PeerName) []mesh.PeerName {
	if len(consensus) == 0 {
		return nil
	}

	peers := make(peerNames, len(consensus))
	copy(peers, consensus)
	sort.Sort(peers)

	dst := 0
	for src := 1; src < len(peers); src++ {
		if peers[dst] != peers[src] {
			dst++
			peers[dst] = peers[src]
		}
	}

	return peers[:dst+1]
}

func (alloc *Allocator) propose() {
	alloc.debugf("Paxos proposing")
	alloc.paxos.Propose()
	alloc.gossip.GossipBroadcast(alloc.Gossip())
}

func encodeRange(r address.Range) []byte {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	if err := enc.Encode(r); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func (alloc *Allocator) sendSpaceRequest(dest mesh.PeerName, r address.Range) error {
	msg := append([]byte{msgSpaceRequest}, encodeRange(r)...)
	return alloc.gossip.GossipUnicast(dest, msg)
}

func (alloc *Allocator) sendSpaceRequestDenied(dest mesh.PeerName, r address.Range) error {
	msg := append([]byte{msgSpaceRequestDenied}, encodeRange(r)...)
	return alloc.gossip.GossipUnicast(dest, msg)
}

func (alloc *Allocator) sendRingUpdate(dest mesh.PeerName) {
	msg := append([]byte{msgRingUpdate}, alloc.encode()...)
	alloc.gossip.GossipUnicast(dest, msg)
}

func (alloc *Allocator) update(sender mesh.PeerName, msg []byte) error {
	reader := bytes.NewReader(msg)
	decoder := gob.NewDecoder(reader)
	var data gossipState
	var err error

	if err := decoder.Decode(&data); err != nil {
		return err
	}

	deltat := time.Unix(data.Now, 0).Sub(alloc.now())
	if deltat > time.Hour || -deltat > time.Hour {
		return fmt.Errorf("clock skew of %v detected, ignoring update", deltat)
	}

	// Merge nicknames
	for peer, nickname := range data.Nicknames {
		alloc.nicknames[peer] = nickname
	}

	var oldRanges []address.Range
	switch {
	// If someone sent us a ring, merge it into ours. Note this will move us
	// out of the awaiting-consensus state if we didn't have a ring already.
	case data.Ring != nil:
		oldRanges = alloc.ring.OwnedRanges()
		switch err = alloc.ring.Merge(*data.Ring); err {
		case nil:
			if !alloc.ring.Empty() {
				alloc.pruneNicknames()
				alloc.monitor.HandleUpdate(oldRanges, alloc.ring.OwnedRanges())
				alloc.ringUpdated()
			}
		case ring.ErrDifferentSeeds:
			return fmt.Errorf("IP allocation was seeded by different peers (received: %v, ours: %v)",
				alloc.annotatePeernames(data.Ring.Seeds), alloc.annotatePeernames(alloc.ring.Seeds))
		case ring.ErrDifferentRange:
			return fmt.Errorf("Incompatible IP allocation ranges (received: %s, ours: %s)",
				data.Ring.Range().AsCIDRString(), alloc.ring.Range().AsCIDRString())
		default:
			return err
		}

	// If we reach this point we know the sender is either an elector
	// broadcasting a paxos proposal to form a ring or a non-elector
	// broadcasting a ring request. If we have a ring already we can just send
	// it back regardless.
	case !alloc.ring.Empty():
		if sender != mesh.UnknownPeerName {
			alloc.sendRingUpdate(sender)
		}

	// Otherwise, we need to react according to whether or not we received a
	// paxos proposal.
	case data.Paxos != nil:
		// Process the proposal (this is a no-op if we're an observer)
		if alloc.paxos.Update(data.Paxos) {
			if alloc.paxos.Think() {
				// If something important changed, broadcast
				alloc.gossip.GossipBroadcast(alloc.Gossip())
			}

			if ok, cons := alloc.paxos.Consensus(); ok {
				alloc.createRing(cons.Value)
			}
		}

	// No paxos proposal present, so sender is a non-elector. We don't have a
	// ring to send, so attempt to establish one on their behalf. NB we only do
	// this:
	//
	// * On an explicit broadcast request triggered by a remote allocation attempt
	//   (if we did so on periodic gossip we would force consensus unnecessarily)
	// * If we are an elector (to avoid a broadcast storm of ring request messages)
	default:
		if _, ok := alloc.paxos.(*paxos.Node); ok && sender != mesh.UnknownPeerName {
			alloc.establishRing()
		}
	}

	return nil
}

func (alloc *Allocator) donateSpace(r address.Range, to mesh.PeerName) {
	// No matter what we do, we'll send a unicast gossip
	// of our ring back to the chap who asked for space.
	// This serves to both tell him of any space we might
	// have given him, or tell him where he might find some
	// more.
	defer alloc.sendRingUpdate(to)

	// TODO(mp) implement the following algo for CIDR=true donation:
	// Q: how does the asking peer decide on range?
	//
	// globalList := []
	// for r in ranges.cidrized().filter(askedRange) {
	// 	if r.IsFree() {
	// 		donate(r)
	// 		return
	// 	}
	//  a, b := r.SplitIntoTwo()
	//  if {a,b}.IsFree() {
	//    if better {
	//     append(globalList, (a, a.Size(), NewBlocksCount))
	//    }
	//	}
	//
	// }

	alloc.debugln("Peer", to, "asked me for space")
	chunk, ok := alloc.space.Donate(r, alloc.isCIDRAligned)
	if !ok {
		free := alloc.space.NumFreeAddressesInRange(r)
		common.Assert(free == 0)
		alloc.debugln("No space to give to peer", to)
		// separate message maintains backwards-compatibility:
		// down-level peers will ignore this and still get the ring update.
		alloc.sendSpaceRequestDenied(to, r)
		return
	}
	alloc.debugln("Giving range", chunk, "to", to)
	oldRanges := alloc.ring.OwnedRanges()
	alloc.ring.GrantRangeToHost(chunk.Start, chunk.End, to)
	alloc.persistRing()
	newRanges := alloc.ring.OwnedRanges()
	alloc.monitor.HandleUpdate(oldRanges, newRanges)
}

func (alloc *Allocator) assertInvariants() {
	// We need to ensure all ranges the ring thinks we own have
	// a corresponding space in the space set, and vice versa
	checkSpace := space.New()
	checkSpace.AddRanges(alloc.ring.OwnedRanges())
	ranges := checkSpace.OwnedRanges()
	spaces := alloc.space.OwnedRanges()

	common.Assert(len(ranges) == len(spaces))

	for i := 0; i < len(ranges); i++ {
		r := ranges[i]
		s := spaces[i]
		common.Assert(s.Start == r.Start && s.End == r.End)
	}
}

func (alloc *Allocator) reportFreeSpace() {
	ranges := alloc.ring.OwnedRanges()
	if len(ranges) == 0 {
		return
	}

	freespace := make(map[address.Address]address.Offset)
	for _, r := range ranges {
		freespace[r.Start] = alloc.space.NumFreeAddressesInRange(r)
	}
	alloc.ring.ReportFree(freespace)
}

// Persistent data
const (
	ringIdent  = "ring"
	nameIdent  = "peername"
	ownedIdent = "ownedAddresses"
)

func (alloc *Allocator) persistRing() {
	// It would be better if these two Save operations happened in the same transaction
	if err := alloc.db.Save(nameIdent, alloc.ourName); err != nil {
		alloc.fatalf("Error persisting ring data: %s", err)
		return
	}
	if err := alloc.db.Save(ringIdent, alloc.ring); err != nil {
		alloc.fatalf("Error persisting ring data: %s", err)
	}
}

func (alloc *Allocator) loadPersistedData() {
	checkPeerName := alloc.ourName
	if err := alloc.db.Load(nameIdent, &checkPeerName); err != nil {
		alloc.fatalf("Error loading persisted peer name: %s", err)
		return
	}
	if checkPeerName != alloc.ourName {
		alloc.infof("Deleting persisted data for peername %s", checkPeerName)
		alloc.persistRing()
		alloc.persistOwned()
		return
	}
	if err := alloc.db.Load(ringIdent, &alloc.ring); err != nil {
		alloc.fatalf("Error loading persisted ring data: %s", err)
	}
	if alloc.ring != nil {
		alloc.space.UpdateRanges(alloc.ring.OwnedRanges())
	}
	if err := alloc.db.Load(ownedIdent, &alloc.owned); err != nil {
		alloc.fatalf("Error loading persisted address data: %s", err)
	}
	for _, cidrs := range alloc.owned {
		for _, cidr := range cidrs {
			alloc.space.Claim(cidr.Addr)
		}
	}
}

func (alloc *Allocator) persistOwned() {
	if err := alloc.db.Save(ownedIdent, alloc.owned); err != nil {
		alloc.fatalf("Error persisting address data: %s", err)
	}
}

// Owned addresses

func (alloc *Allocator) hasOwned(ident string) bool {
	_, b := alloc.owned[ident]
	return b
}

// NB: addr must not be owned by ident already
func (alloc *Allocator) addOwned(ident string, cidr address.CIDR) {
	alloc.owned[ident] = append(alloc.owned[ident], cidr)
	alloc.persistOwned()
}

func (alloc *Allocator) removeAllOwned(ident string) []address.CIDR {
	a := alloc.owned[ident]
	delete(alloc.owned, ident)
	alloc.persistOwned()
	return a
}

func (alloc *Allocator) removeOwned(ident string, addrToFree address.Address) bool {
	cidrs, _ := alloc.owned[ident]
	for i, ownedCidr := range cidrs {
		if ownedCidr.Addr == addrToFree {
			if len(cidrs) == 1 {
				delete(alloc.owned, ident)
			} else {
				alloc.owned[ident] = append(cidrs[:i], cidrs[i+1:]...)
			}
			alloc.persistOwned()
			return true
		}
	}
	return false
}

func (alloc *Allocator) ownedInRange(ident string, r address.Range) []address.CIDR {
	var c []address.CIDR
	for _, cidr := range alloc.owned[ident] {
		if r.Contains(cidr.Addr) {
			c = append(c, cidr)
		}
	}
	return c
}

func (alloc *Allocator) findOwner(addr address.Address) string {
	for ident, cidrs := range alloc.owned {
		for _, candidate := range cidrs {
			if candidate.Addr == addr {
				return ident
			}
		}
	}
	return ""
}

// For each ID in the 'owned' map, remove the entry if it isn't in the map
func (alloc *Allocator) syncOwned(ids map[string]struct{}) {
	changed := false
	for ident, cidrs := range alloc.owned {
		if _, found := ids[ident]; !found {
			for _, cidr := range cidrs {
				alloc.space.Free(cidr.Addr)
			}
			delete(alloc.owned, ident)
			changed = true
		}
	}
	if changed {
		alloc.persistOwned()
	}
}

// Logging

func (alloc *Allocator) fatalf(fmt string, args ...interface{}) {
	common.Log.Fatalf("[allocator %s] "+fmt, append([]interface{}{alloc.ourName}, args...)...)
}
func (alloc *Allocator) infof(fmt string, args ...interface{}) {
	common.Log.Infof("[allocator %s] "+fmt, append([]interface{}{alloc.ourName}, args...)...)
}
func (alloc *Allocator) debugln(args ...interface{}) {
	common.Log.Debugln(append([]interface{}{fmt.Sprintf("[allocator %s]:", alloc.ourName)}, args...)...)
}
func (alloc *Allocator) debugf(fmt string, args ...interface{}) {
	common.Log.Debugf("[allocator %s] "+fmt, append([]interface{}{alloc.ourName}, args...)...)
}
