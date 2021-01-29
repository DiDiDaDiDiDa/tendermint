package evidence

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/google/orderedcode"
	dbm "github.com/tendermint/tm-db"

	clist "github.com/tendermint/tendermint/libs/clist"
	"github.com/tendermint/tendermint/libs/log"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
)

const (
	// prefixes are unique across all tm db's
	prefixCommitted = int64(9)
	prefixPending   = int64(10)
)

// Pool maintains a pool of valid evidence to be broadcasted and committed
type Pool struct {
	logger log.Logger

	evidenceStore dbm.DB
	evidenceList  *clist.CList // concurrent linked-list of evidence
	evidenceSize  uint32       // amount of pending evidence

	// needed to load validators to verify evidence
	stateDB sm.Store
	// needed to load headers and commits to verify evidence
	blockStore BlockStore

	mtx sync.Mutex
	// latest state
	state sm.State
	// evidence from consensus if buffered to this slice, awaiting until the next height
	// before being flushed to the pool. This prevents broadcasting and proposing of
	// evidence before the height with which the evidence happened is finished.
	consensusBuffer []types.Evidence

	pruningHeight int64
	pruningTime   time.Time
}

// NewPool creates an evidence pool. If using an existing evidence store,
// it will add all pending evidence to the concurrent list.
func NewPool(logger log.Logger, evidenceDB dbm.DB, stateDB sm.Store, blockStore BlockStore) (*Pool, error) {
	state, err := stateDB.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	pool := &Pool{
		stateDB:         stateDB,
		blockStore:      blockStore,
		state:           state,
		logger:          logger,
		evidenceStore:   evidenceDB,
		evidenceList:    clist.New(),
		consensusBuffer: make([]types.Evidence, 0),
	}

	// If pending evidence already in db, in event of prior failure, then check
	// for expiration, update the size and load it back to the evidenceList.
	pool.pruningHeight, pool.pruningTime = pool.removeExpiredPendingEvidence()
	evList, _, err := pool.listEvidence(prefixPending, -1)
	if err != nil {
		return nil, err
	}

	atomic.StoreUint32(&pool.evidenceSize, uint32(len(evList)))

	for _, ev := range evList {
		pool.evidenceList.PushBack(ev)
	}

	return pool, nil
}

// PendingEvidence is used primarily as part of block proposal and returns up to
// maxNum of uncommitted evidence.
func (evpool *Pool) PendingEvidence(maxBytes int64) ([]types.Evidence, int64) {
	if evpool.Size() == 0 {
		return []types.Evidence{}, 0
	}

	evidence, size, err := evpool.listEvidence(prefixPending, maxBytes)
	if err != nil {
		evpool.logger.Error("failed to retrieve pending evidence", "err", err)
	}

	return evidence, size
}

// Update pulls the latest state to be used for expiration and evidence params
// and then prunes all expired evidence.
func (evpool *Pool) Update(state sm.State, ev types.EvidenceList) {
	// sanity check
	if state.LastBlockHeight <= evpool.state.LastBlockHeight {
		panic(fmt.Sprintf(
			"failed EvidencePool.Update new state height is less than or equal to previous state height: %d <= %d",
			state.LastBlockHeight,
			evpool.state.LastBlockHeight,
		))
	}

	evpool.logger.Info(
		"updating evidence pool",
		"last_block_height", state.LastBlockHeight,
		"last_block_time", state.LastBlockTime,
	)

	evpool.mtx.Lock()
	// flush awaiting evidence from consensus into pool
	evpool.flushConsensusBuffer()
	// update state
	evpool.state = state
	evpool.mtx.Unlock()

	// move committed evidence out from the pending pool and into the committed pool
	evpool.markEvidenceAsCommitted(ev, state.LastBlockHeight)

	// Prune pending evidence when it has expired. This also updates when the next
	// evidence will expire.
	if evpool.Size() > 0 && state.LastBlockHeight > evpool.pruningHeight &&
		state.LastBlockTime.After(evpool.pruningTime) {
		evpool.pruningHeight, evpool.pruningTime = evpool.removeExpiredPendingEvidence()
	}
}

// AddEvidence checks the evidence is valid and adds it to the pool.
func (evpool *Pool) AddEvidence(ev types.Evidence) error {
	evpool.logger.Debug("attempting to add evidence", "evidence", ev)

	// We have already verified this piece of evidence - no need to do it again
	if evpool.isPending(ev) {
		evpool.logger.Info("evidence already pending; ignoring", "evidence", ev)
		return nil
	}

	// check that the evidence isn't already committed
	if evpool.isCommitted(ev) {
		// This can happen if the peer that sent us the evidence is behind so we
		// shouldn't punish the peer.
		evpool.logger.Debug("evidence was already committed; ignoring", "evidence", ev)
		return nil
	}

	// 1) Verify against state.
	if err := evpool.verify(ev); err != nil {
		return err
	}

	// 2) Save to store.
	if err := evpool.addPendingEvidence(ev); err != nil {
		return fmt.Errorf("failed to add evidence to pending list: %w", err)
	}

	// 3) Add evidence to clist.
	evpool.evidenceList.PushBack(ev)

	evpool.logger.Info("verified new evidence of byzantine behavior", "evidence", ev)
	return nil
}

// AddEvidenceFromConsensus should be exposed only to the consensus reactor so
// it can add evidence to the pool directly without the need for verification.
func (evpool *Pool) AddEvidenceFromConsensus(ev types.Evidence) error {
	// we already have this evidence, log this but don't return an error.
	if evpool.isPending(ev) {
		evpool.logger.Info("evidence already pending; ignoring", "evidence", ev)
		return nil
	}

	// add evidence to a buffer which will pass the evidence to the pool at the following height.
	// This avoids the issue of some nodes verifying and proposing evidence at a height where the
	// block hasn't been committed on cause others to potentially fail.
	evpool.mtx.Lock()
	defer evpool.mtx.Unlock()
	evpool.consensusBuffer = append(evpool.consensusBuffer, ev)
	evpool.logger.Info("received new evidence of byzantine behavior from consensus", "evidence", ev)

	return nil
}

// CheckEvidence takes an array of evidence from a block and verifies all the evidence there.
// If it has already verified the evidence then it jumps to the next one. It ensures that no
// evidence has already been committed or is being proposed twice. It also adds any
// evidence that it doesn't currently have so that it can quickly form ABCI Evidence later.
func (evpool *Pool) CheckEvidence(evList types.EvidenceList) error {
	hashes := make([][]byte, len(evList))
	for idx, ev := range evList {

		ok := evpool.fastCheck(ev)

		if !ok {
			// check that the evidence isn't already committed
			if evpool.isCommitted(ev) {
				return &types.ErrInvalidEvidence{Evidence: ev, Reason: errors.New("evidence was already committed")}
			}

			err := evpool.verify(ev)
			if err != nil {
				return &types.ErrInvalidEvidence{Evidence: ev, Reason: err}
			}

			if err := evpool.addPendingEvidence(ev); err != nil {
				// Something went wrong with adding the evidence but we already know it is valid
				// hence we log an error and continue
				evpool.logger.Error("failed to add evidence to pending list", "err", err, "evidence", ev)
			}

			evpool.logger.Info("verified new evidence of byzantine behavior", "evidence", ev)
		}

		// check for duplicate evidence. We cache hashes so we don't have to work them out again.
		hashes[idx] = ev.Hash()
		for i := idx - 1; i >= 0; i-- {
			if bytes.Equal(hashes[i], hashes[idx]) {
				return &types.ErrInvalidEvidence{Evidence: ev, Reason: errors.New("duplicate evidence")}
			}
		}
	}

	return nil
}

// EvidenceFront goes to the first evidence in the clist
func (evpool *Pool) EvidenceFront() *clist.CElement {
	return evpool.evidenceList.Front()
}

// EvidenceWaitChan is a channel that closes once the first evidence in the list
// is there. i.e Front is not nil.
func (evpool *Pool) EvidenceWaitChan() <-chan struct{} {
	return evpool.evidenceList.WaitChan()
}

// Size returns the number of evidence in the pool.
func (evpool *Pool) Size() uint32 {
	return atomic.LoadUint32(&evpool.evidenceSize)
}

// State returns the current state of the evpool.
func (evpool *Pool) State() sm.State {
	evpool.mtx.Lock()
	defer evpool.mtx.Unlock()
	return evpool.state
}

// fastCheck leverages the fact that the evidence pool may have already verified
// the evidence to see if it can quickly conclude that the evidence is already
// valid.
func (evpool *Pool) fastCheck(ev types.Evidence) bool {
	if lcae, ok := ev.(*types.LightClientAttackEvidence); ok {
		key := keyPending(ev)
		evBytes, err := evpool.evidenceStore.Get(key)
		if evBytes == nil { // the evidence is not in the nodes pending list
			return false
		}

		if err != nil {
			evpool.logger.Error("failed to load light client attack evidence", "err", err, "key(height/hash)", key)
			return false
		}

		var trustedPb tmproto.LightClientAttackEvidence

		if err = trustedPb.Unmarshal(evBytes); err != nil {
			evpool.logger.Error(
				"failed to convert light client attack evidence from bytes",
				"key(height/hash)", key,
				"err", err,
			)
			return false
		}

		trustedEv, err := types.LightClientAttackEvidenceFromProto(&trustedPb)
		if err != nil {
			evpool.logger.Error(
				"failed to convert light client attack evidence from protobuf",
				"key(height/hash)", key,
				"err", err,
			)
			return false
		}

		// Ensure that all the byzantine validators that the evidence pool has match
		// the byzantine validators in this evidence.
		if trustedEv.ByzantineValidators == nil && lcae.ByzantineValidators != nil {
			return false
		}

		if len(trustedEv.ByzantineValidators) != len(lcae.ByzantineValidators) {
			return false
		}

		byzValsCopy := make([]*types.Validator, len(lcae.ByzantineValidators))
		for i, v := range lcae.ByzantineValidators {
			byzValsCopy[i] = v.Copy()
		}

		// ensure that both validator arrays are in the same order
		sort.Sort(types.ValidatorsByVotingPower(byzValsCopy))

		for idx, val := range trustedEv.ByzantineValidators {
			if !bytes.Equal(byzValsCopy[idx].Address, val.Address) {
				return false
			}
			if byzValsCopy[idx].VotingPower != val.VotingPower {
				return false
			}
		}

		return true
	}

	// For all other evidence the evidence pool just checks if it is already in
	// the pending db.
	return evpool.isPending(ev)
}

// IsExpired checks whether evidence or a polc is expired by checking whether a height and time is older
// than set by the evidence consensus parameters
func (evpool *Pool) isExpired(height int64, time time.Time) bool {
	var (
		params       = evpool.State().ConsensusParams.Evidence
		ageDuration  = evpool.State().LastBlockTime.Sub(time)
		ageNumBlocks = evpool.State().LastBlockHeight - height
	)
	return ageNumBlocks > params.MaxAgeNumBlocks &&
		ageDuration > params.MaxAgeDuration
}

// IsCommitted returns true if we have already seen this exact evidence and it is already marked as committed.
func (evpool *Pool) isCommitted(evidence types.Evidence) bool {
	key := keyCommitted(evidence)
	ok, err := evpool.evidenceStore.Has(key)
	if err != nil {
		evpool.logger.Error("failed to find committed evidence", "err", err)
	}
	return ok
}

// IsPending checks whether the evidence is already pending. DB errors are passed to the logger.
func (evpool *Pool) isPending(evidence types.Evidence) bool {
	key := keyPending(evidence)
	ok, err := evpool.evidenceStore.Has(key)
	if err != nil {
		evpool.logger.Error("failed to find pending evidence", "err", err)
	}
	return ok
}

func (evpool *Pool) addPendingEvidence(ev types.Evidence) error {
	evpb, err := types.EvidenceToProto(ev)
	if err != nil {
		return fmt.Errorf("failed to convert to proto: %w", err)
	}

	evBytes, err := evpb.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal evidence: %w", err)
	}

	key := keyPending(ev)

	err = evpool.evidenceStore.Set(key, evBytes)
	if err != nil {
		return fmt.Errorf("failed to persist evidence: %w", err)
	}

	atomic.AddUint32(&evpool.evidenceSize, 1)
	return nil
}

// markEvidenceAsCommitted processes all the evidence in the block, marking it as
// committed and removing it from the pending database.
func (evpool *Pool) markEvidenceAsCommitted(evidence types.EvidenceList, height int64) {
	blockEvidenceMap := make(map[string]struct{}, len(evidence))
	batch := evpool.evidenceStore.NewBatch()
	defer batch.Close()

	for _, ev := range evidence {
		if evpool.isPending(ev) {
			batch.Delete(keyPending(ev))
			blockEvidenceMap[evMapKey(ev)] = struct{}{}
		}

		// Add evidence to the committed list. As the evidence is stored in the block store
		// we only need to record the height that it was saved at.
		key := keyCommitted(ev)

		h := gogotypes.Int64Value{Value: height}
		evBytes, err := proto.Marshal(&h)
		if err != nil {
			evpool.logger.Error("failed to marshal committed evidence", "key(height/hash)", key, "err", err)
			continue
		}

		if err := evpool.evidenceStore.Set(key, evBytes); err != nil {
			evpool.logger.Error("failed to save committed evidence", "key(height/hash)", key, "err", err)
		}

		evpool.logger.Info("marked evidence as committed", "evidence", ev)
	}

	// check if we need to remove any pending evidence
	if len(blockEvidenceMap) == 0 {
		return 
	}

	// remove committed evidence from the clist
	evpool.removeEvidenceFromList(blockEvidenceMap)

	// remove committed evidence from pending bucket
	if err := batch.WriteSync(); err != nil {
		evpool.logger.Error("failed to batch delete pending evidence", "err", err)
		return
	}

	// update the evidence size
	atomic.AddUint32(&evpool.evidenceSize, ^uint32(len(blockEvidenceMap) - 1))

	if err := batch.Close(); err != nil {
		evpool.logger.Error("failed to close batch deletion", "err", err)
	} 
}

// listEvidence retrieves lists evidence from oldest to newest within maxBytes.
// If maxBytes is -1, there's no cap on the size of returned evidence.
func (evpool *Pool) listEvidence(prefixKey int64, maxBytes int64) ([]types.Evidence, int64, error) {
	var (
		evSize    int64
		totalSize int64
		evidence  []types.Evidence
		evList    tmproto.EvidenceList // used for calculating the bytes size
	)

	iter, err := dbm.IteratePrefix(evpool.evidenceStore, prefixToBytes(prefixKey))
	if err != nil {
		return nil, totalSize, fmt.Errorf("database error: %v", err)
	}

	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		var evpb tmproto.Evidence

		if err := evpb.Unmarshal(iter.Value()); err != nil {
			return evidence, totalSize, err
		}

		evList.Evidence = append(evList.Evidence, evpb)
		evSize = int64(evList.Size())

		if maxBytes != -1 && evSize > maxBytes {
			if err := iter.Error(); err != nil {
				return evidence, totalSize, err
			}
			return evidence, totalSize, nil
		}

		ev, err := types.EvidenceFromProto(&evpb)
		if err != nil {
			return nil, totalSize, err
		}

		totalSize = evSize
		evidence = append(evidence, ev)
	}

	if err := iter.Error(); err != nil {
		return evidence, totalSize, err
	}

	return evidence, totalSize, nil
}

func (evpool *Pool) removeExpiredPendingEvidence() (int64, time.Time) {
	batch := evpool.evidenceStore.NewBatch()
	defer batch.Close()

	height, time, blockEvidenceMap := evpool.batchExpiredPendingEvidence(batch)

	// if we haven't removed any evidence then return early
	if len(blockEvidenceMap) == 0 {
		return height, time
	}

	evpool.logger.Debug("removing expired evidence", 
		"height", evpool.State().LastBlockHeight, 
		"time", evpool.State().LastBlockTime,
		"expired evidence", len(blockEvidenceMap),
	)

	// remove evidence from the clist
	evpool.removeEvidenceFromList(blockEvidenceMap)

	// remove expired evidence from pending bucket
	if err := batch.WriteSync(); err != nil {
		evpool.logger.Error("failed to batch delete pending evidence", "err", err)
		return evpool.State().LastBlockHeight, evpool.State().LastBlockTime
	}

	// update the evidence size
	atomic.AddUint32(&evpool.evidenceSize, ^uint32(len(blockEvidenceMap) - 1))

	if err := batch.Close(); err != nil {
		evpool.logger.Error("failed to close batch deletion", "err", err)
		return height, time
	}

	return height, time
}

func (evpool *Pool) batchExpiredPendingEvidence(batch dbm.Batch) (int64, time.Time, map[string]struct{}) {
	blockEvidenceMap := make(map[string]struct{})
	iter, err := dbm.IteratePrefix(evpool.evidenceStore, prefixToBytes(prefixPending))
	if err != nil {
		evpool.logger.Error("failed to iterate over pending evidence", "err", err)
		return evpool.State().LastBlockHeight, evpool.State().LastBlockTime, blockEvidenceMap
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		ev, err := bytesToEv(iter.Value())
		if err != nil {
			evpool.logger.Error("failed to transition evidence from protobuf", "err", err, "ev", ev)
			continue
		}

		// if true, we have looped through all expired evidence
		if !evpool.isExpired(ev.Height(), ev.Time()) {
			// Return the height and time with which this evidence will have expired
			// so we know when to prune next.
			return ev.Height() + evpool.State().ConsensusParams.Evidence.MaxAgeNumBlocks + 1,
				ev.Time().Add(evpool.State().ConsensusParams.Evidence.MaxAgeDuration).Add(time.Second),
				blockEvidenceMap
		}

		evpool.logger.Debug("Adding evindence to be deleted", "ev", ev)

		// else add to the batch
		if err := batch.Delete(iter.Key()); err != nil {
			evpool.logger.Error("failed to batch evidence", "err", err, "ev", ev)
			continue
		}

		// and add to the map to remove the evidence from the clist
		blockEvidenceMap[evMapKey(ev)] = struct{}{}
	}

	return evpool.State().LastBlockHeight, evpool.State().LastBlockTime, blockEvidenceMap
}

func (evpool *Pool) removeEvidenceFromList(
	blockEvidenceMap map[string]struct{}) {

	for e := evpool.evidenceList.Front(); e != nil; e = e.Next() {
		// Remove from clist
		ev := e.Value.(types.Evidence)
		if _, ok := blockEvidenceMap[evMapKey(ev)]; ok {
			evpool.evidenceList.Remove(e)
			e.DetachPrev()
		}
	}
}

// flushConsensusBuffer moves the evidence produced from consensus into the evidence pool
// and list so that it can be broadcasted and proposed
func (evpool *Pool) flushConsensusBuffer() {
	for _, ev := range evpool.consensusBuffer {
		if err := evpool.addPendingEvidence(ev); err != nil {
			evpool.logger.Error("failed to flush evidence from consensus buffer to pending list: %w", err)
			continue
		}

		evpool.evidenceList.PushBack(ev)
	}
	// reset consensus buffer
	evpool.consensusBuffer = make([]types.Evidence, 0)
}

func bytesToEv(evBytes []byte) (types.Evidence, error) {
	var evpb tmproto.Evidence
	err := evpb.Unmarshal(evBytes)
	if err != nil {
		return &types.DuplicateVoteEvidence{}, err
	}

	return types.EvidenceFromProto(&evpb)
}

func evMapKey(ev types.Evidence) string {
	return string(ev.Hash())
}

func prefixToBytes(prefix int64) []byte {
	key, err := orderedcode.Append(nil, prefix)
	if err != nil {
		panic(err)
	}
	return key
}

func keyCommitted(evidence types.Evidence) []byte {
	var height int64 = evidence.Height()
	key, err := orderedcode.Append(nil, prefixCommitted, height, string(evidence.Hash()))
	if err != nil {
		panic(err)
	}
	return key
}

func keyPending(evidence types.Evidence) []byte {
	var height int64 = evidence.Height()
	key, err := orderedcode.Append(nil, prefixPending, height, string(evidence.Hash()))
	if err != nil {
		panic(err)
	}
	return key
}
