package mir

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/mir/pkg/checkpoint"
	"github.com/filecoin-project/mir/pkg/pb/requestpb"
	"github.com/filecoin-project/mir/pkg/systems/smr"
	t "github.com/filecoin-project/mir/pkg/types"
	"github.com/filecoin-project/mir/pkg/util/maputil"

	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/v1api"
	"github.com/filecoin-project/lotus/chain/actors/builtin"
	"github.com/filecoin-project/lotus/chain/types"
	ltypes "github.com/filecoin-project/lotus/chain/types"
)

var _ smr.AppLogic = &StateManager{}

type Message []byte

type Batch struct {
	Messages []Message
}

type StateManager struct {
	// parent context
	ctx context.Context

	// Lotus API
	api v1api.FullNode

	// The current epoch number.
	currentEpoch t.EpochNr

	// For each epoch number, stores the corresponding membership.
	memberships map[t.EpochNr]map[t.NodeID]t.NodeAddress

	MirManager *Manager

	reconfigurationVotes map[t.EpochNr]map[string]int

	prevCheckpoint ParentMeta

	// Channel to send checkpoints to assemble them in blocks
	NextCheckpoint chan *checkpoint.StableCheckpoint
}

func NewStateManager(ctx context.Context, initialMembership map[t.NodeID]t.NodeAddress, m *Manager, api v1api.FullNode) (*StateManager, error) {
	// Initialize the membership for the first epochs.
	// We use configOffset+2 memberships to account for:
	// - The first epoch (epoch 0)
	// - The configOffset epochs that already have a fixed membership (epochs 1 to configOffset)
	// - The membership of the following epoch (configOffset+1) initialized with the same membership,
	//   but potentially replaced during the first epoch (epoch 0) through special configuration requests.
	memberships := make(map[t.EpochNr]map[t.NodeID]t.NodeAddress, ConfigOffset+2)
	for e := 0; e < ConfigOffset+2; e++ {
		memberships[t.EpochNr(e)] = initialMembership
	}

	sm := StateManager{
		ctx:                  ctx,
		NextCheckpoint:       make(chan *checkpoint.StableCheckpoint, 1),
		MirManager:           m,
		memberships:          memberships,
		currentEpoch:         0,
		reconfigurationVotes: make(map[t.EpochNr]map[string]int),
		api:                  api,
	}

	// Initialize manager checkpoint state with the corresponding latest
	// checkpoint
	ch, err := sm.firstEpochCheckpoint()
	if err != nil {
		return nil, xerrors.Errorf("error getting checkpoint for epoch 0: %w", err)
	}
	c, err := ch.Cid()
	if err != nil {
		return nil, xerrors.Errorf("error getting cid for checkpoint: %w", err)
	}
	sm.prevCheckpoint = ParentMeta{Height: ch.Height, Cid: c}

	return &sm, nil
}

// RestoreState is called by Mir when the validator goes out-of-sync, and it requires
// lotus to sync from the latest checkpoint. Mir provides lotus with the latest
// checkpoint and from this:
// - The latest membership and configuration for the consensus is recovered.
// - We clean all previous outdated checkpoints and configurations we may have
// received while trying to sync.
// - If there is a snapshot in the checkpoint, we poll our connections to sync
// to the latest block determined by the checkpoint.
// - We deliver the checkpoint to the mining process, so it can be included in the next
// block (Mir provides the latest checkpoint, which hasn't been included in a block
// yet)
// - And we flag the mining process that we are synced, and it can start accepting new
// batches from Mir and assembling new blocks.
func (sm *StateManager) RestoreState(checkpoint *checkpoint.StableCheckpoint) error {
	log.Debugf("Calling RestoreState from Mir for epoch %d", sm.currentEpoch)
	// release any previous checkpoint delivered and pending
	// to sync, as we are syncing again. This prevents a deadlock.
	sm.releaseNextCheckpointChan()

	config := checkpoint.Snapshot.EpochData.EpochConfig
	sm.currentEpoch = t.EpochNr(config.EpochNr)
	sm.memberships = make(map[t.EpochNr]map[t.NodeID]t.NodeAddress, len(config.Memberships))

	for e, membership := range config.Memberships {
		// skew membership to current epoch, we are starting from a checkpoint
		// sm.memberships[t.EpochNr(e)+sm.currentEpoch] = make(map[t.NodeID]t.NodeAddress)
		// sm.memberships[t.EpochNr(e)+sm.currentEpoch] = t.Membership(membership)
		sm.memberships[t.EpochNr(e)] = make(map[t.NodeID]t.NodeAddress)
		sm.memberships[t.EpochNr(e)] = t.Membership(membership)
	}

	newMembership := maputil.Copy(sm.memberships[t.EpochNr(config.EpochNr+ConfigOffset)])
	sm.memberships[t.EpochNr(config.EpochNr+ConfigOffset+1)] = newMembership

	// if mir provides a snapshot
	snapshot := checkpoint.Snapshot.AppData
	ch := &Checkpoint{}
	if len(snapshot) > 0 {
		// get checkpoint from snapshot.
		err := ch.FromBytes(snapshot)
		if err != nil {
			return xerrors.Errorf("error getting checkpoint from snapshot bytes: %w", err)
		}

		log.Infof("Restoring state from checkpoint at height: %d", ch.Height)

		// purge any state previous to the checkpoint
		if err = sm.api.SyncPurgeForRecovery(sm.ctx, ch.Height); err != nil {
			return xerrors.Errorf("couldn't purge state to recover from checkpoint: %w", err)
		}

		internalSync := false
		// From all the peers of my daemon try to get the latest tipset.
		connPeers, err := sm.api.NetPeers(sm.ctx)
		if err != nil {
			return xerrors.Errorf("error getting list of peers from daemon: %w", err)
		}
		if len(connPeers) == 0 {
			return xerrors.Errorf("no connection with other filecoin peers, can't sync my daemon")
		}

		log.Debugf("Restoring from checkpoint at height %d ", ch.Height)
		for _, addr := range connPeers {
			log.Debugf("Trying to sync up to height %d from peer %s", ch.Height, addr.ID)
			ts, err := sm.api.SyncFetchTipSetFromPeer(sm.ctx, addr.ID, types.NewTipSetKey(ch.BlockCids[0]))
			if err != nil {
				log.Errorf("error fetching latest tipset from peer %s: %v", addr.ID, err)
				continue
			}
			// wait for full-sync before returning from restoreState.
			err = sm.waitForBlock(ts.Height())
			if err != nil {
				return xerrors.Errorf("error waiting for next block %d: %w", ts.Height(), err)
			}
			internalSync = true
		}

		// if we couldn't find any valid peer or validator to sync from, just abort.
		if !internalSync {
			return xerrors.Errorf("couldn't find any good peers to sync from")
		}

		// once synced we deliver the checkpoint to our mining process, so it can be
		// included in the next block (as the rest of Mir validators will do before
		// accepting the next batch), and we persist it locally.
		log.Debugf("Delivering checkpoint for height %d to mining process after sync", ch.Height)
		err = sm.deliverCheckpoint(checkpoint, ch)
		if err != nil {
			return xerrors.Errorf("error delivering checkpoint to lotus from mir after restoreState: %w", err)
		}
	}

	return nil
}

// ApplyTXs applies transactions received from the availability layer to the app state
// and creates a Lotus block from the delivered batch.
func (sm *StateManager) ApplyTXs(txs []*requestpb.Request) error {
	fmt.Println(">>>>>>>> ApplyTXs current epoch", sm.currentEpoch)
	var mirMsgs []Message

	// For each request in the batch
	for _, req := range txs {
		switch req.Type {
		case TransportType:
			mirMsgs = append(mirMsgs, req.Data)
		case ReconfigurationType:
			err := sm.applyConfigMsg(req)
			if err != nil {
				return err
			}
		}
	}

	if len(sm.memberships) == 5 {
		// panic(222)
	}

	batch := &Batch{
		Messages: mirMsgs,
	}

	base, err := sm.api.ChainHead(sm.ctx)
	if err != nil {
		return xerrors.Errorf("failed to get chain head: %w", err)
	}
	log.With("miner", sm.MirManager.Addr).Debugf("Trying to mine new block over base: %s", base.Key())

	nextHeight := base.Height() + 1
	log.With("miner", sm.MirManager.Addr).Debugf("Getting new batch from Mir to assemble a new block for height: %d", nextHeight)

	msgs := sm.MirManager.GetMessages(batch)
	log.With("miner", sm.MirManager.Addr).With("epoch", nextHeight).
		Infof("try to create a block: msgs - %d", len(msgs))

	// include checkpoint in VRF proof field?
	vrfCheckpoint := &ltypes.Ticket{VRFProof: nil}
	eproofCheckpoint := &ltypes.ElectionProof{}
	if ch := sm.pollCheckpoint(); ch != nil {
		eproofCheckpoint, err = CertAsElectionProof(ch)
		if err != nil {
			return xerrors.Errorf("error setting eproof from checkpoint certificate: %w", err)
		}
		vrfCheckpoint, err = CheckpointAsVRFProof(ch)
		if err != nil {
			return xerrors.Errorf("error setting vrfproof from checkpoint: %w", err)
		}
		log.With("miner", sm.MirManager.Addr).Infof("Including Mir checkpoint for in block %d", nextHeight)
	}

	bh, err := sm.api.MinerCreateBlock(sm.ctx, &lapi.BlockTemplate{
		// mir blocks are created by all miners. We use system actor as miner of the block
		Miner:            builtin.SystemActorAddr,
		Parents:          base.Key(),
		BeaconValues:     nil,
		Ticket:           vrfCheckpoint,
		Eproof:           eproofCheckpoint,
		Epoch:            base.Height() + 1,
		Timestamp:        uint64(time.Now().Unix()),
		WinningPoStProof: nil,
		Messages:         msgs,
	})
	if err != nil {
		return xerrors.Errorf("creating a block failed: %w", err)
	}
	if bh == nil {
		log.With("miner", sm.MirManager.Addr).With("epoch", nextHeight).Debug("created a nil block")
		return nil
	}

	err = sm.api.SyncSubmitBlock(sm.ctx, &types.BlockMsg{
		Header:        bh.Header,
		BlsMessages:   bh.BlsMessages,
		SecpkMessages: bh.SecpkMessages,
	})
	if err != nil {
		return xerrors.Errorf("unable to sync a block: %w", err)
	}

	log.With("miner", sm.MirManager.Addr).With("epoch", nextHeight).Infof("mined a block at %d", bh.Header.Height)
	return nil
}

func (sm *StateManager) applyConfigMsg(in *requestpb.Request) error {
	var newValSet ValidatorSet
	if err := newValSet.UnmarshalCBOR(bytes.NewReader(in.Data)); err != nil {
		return err
	}
	voted, err := sm.UpdateAndCheckVotes(&newValSet)
	if err != nil {
		return err
	}
	if voted {
		err = sm.UpdateNextMembership(&newValSet)
		if err != nil {
			return err
		}
	}
	return nil
}

func (sm *StateManager) NewEpoch(nr t.EpochNr) (map[t.NodeID]t.NodeAddress, error) {
	log.Infof(" >>>> NEW EPOCH: current epoch triggered in new epoch: %d\n", sm.currentEpoch)
	// Sanity check.
	if nr != sm.currentEpoch+1 {
		return nil, xerrors.Errorf("expected next epoch to be %d, got %d", sm.currentEpoch+1, nr)
	}

	// The base membership is the last one membership.
	newMembership := maputil.Copy(sm.memberships[nr+ConfigOffset])

	// Append a new membership data structure to be modified throughout the new epoch.
	sm.memberships[nr+ConfigOffset+1] = newMembership

	// Update current epoch number.
	// oldEpoch := sm.currentEpoch
	sm.currentEpoch = nr

	// Remove old membership.
	// delete(sm.memberships, oldEpoch)
	// delete(sm.reconfigurationVotes, oldEpoch)

	// reconfigure node.
	// if err := sm.MirManager.ReconfigureMirNode(newMembership); err != nil {
	//	return nil, xerrors.Errorf("error reconfiguring mir node: %w", err)
	// }

	return newMembership, nil
}

func (sm *StateManager) UpdateNextMembership(valSet *ValidatorSet) error {
	_, mbs, err := validatorsMembership(valSet.GetValidators())
	if err != nil {
		return err
	}
	sm.memberships[sm.currentEpoch+ConfigOffset+1] = mbs
	id, _ := sm.api.ID(context.Background())
	fmt.Println(">>>>> UpdateNextMembership - id:", id, "epoch:", sm.currentEpoch)
	for i, v := range sm.memberships {
		fmt.Println(i, len(v))
	}
	return nil
}

// UpdateAndCheckVotes votes for the valSet and returns true if it has enough votes for this valSet.
func (sm *StateManager) UpdateAndCheckVotes(valSet *ValidatorSet) (bool, error) {
	h, err := valSet.Hash()
	if err != nil {
		return false, err
	}
	_, ok := sm.reconfigurationVotes[sm.currentEpoch]
	if !ok {
		sm.reconfigurationVotes[sm.currentEpoch] = make(map[string]int)
	}
	sm.reconfigurationVotes[sm.currentEpoch][string(h)]++
	votes := sm.reconfigurationVotes[sm.currentEpoch][string(h)]
	nodes := len(sm.memberships[sm.currentEpoch])

	if votes < weakQuorum(nodes) {
		return false, nil
	}
	return true, nil
}

// Snapshot is called by Mir every time a checkpoint period has
// passed and is time to create a new checkpoint. This function waits
// for the latest batch before the checkpoint to be synced is committed
// in our local state, and it collects the cids for all the blocks verified
// by the checkpoint.
func (sm *StateManager) Snapshot() ([]byte, error) {
	fmt.Println("Snapshot started in epoch", sm.MirManager.MirID, sm.currentEpoch)
	defer fmt.Println("Snapshot stopped in epoch", sm.MirManager.MirID, sm.currentEpoch)

	nextHeight := sm.prevCheckpoint.Height + sm.GetCheckpointPeriod()
	fmt.Println("nextHeight", nextHeight, sm.prevCheckpoint.Height, sm.GetCheckpointPeriod())
	log.With("miner", sm.MirManager.Addr).Infof("Mir requesting checkpoint snapshot for epoch %d and block height %d", sm.currentEpoch, nextHeight)
	log.With("miner", sm.MirManager.Addr).Infof("Previous checkpoint in snapshot: %v", sm.prevCheckpoint)

	// populating checkpoint template
	ch := Checkpoint{
		Height:    nextHeight,
		Parent:    sm.prevCheckpoint,
		BlockCids: make([]cid.Cid, 0),
	}

	// put blocks in descending order.
	i := nextHeight - 1

	// wait the last block to sync for the snapshot before
	// populating snapshot.
	log.Infof("waiting for latest block (%d) before checkpoint to be synced to assemble the snapshot", i)
	err := sm.waitForBlock(i)
	if err != nil {
		return nil, xerrors.Errorf("error waiting for next block %d: %w", i, err)
	}

	for i >= sm.prevCheckpoint.Height {
		ts, err := sm.api.ChainGetTipSetByHeight(sm.ctx, i, types.EmptyTSK)
		if err != nil {
			return nil, xerrors.Errorf("error getting tipset of height: %d: %w", i, err)
		}
		// In Mir tipsets have a single block, so we can access directly the block for
		// the tipset by accessing the first position.
		ch.BlockCids = append(ch.BlockCids, ts.Blocks()[0].Cid())
		i--
		log.Infof("Getting Cid for block height %d and cid %s to include in snapshot", i, ts.Blocks()[0].Cid())
	}

	return ch.Bytes()
}

// Checkpoint is triggered by Mir when the committee agrees on the next checkpoint.
// We persist the checkpoint locally so we can restore from it after a restart
// or a crash and delivers it to the mining process to include it in the next
// block
// TODO: RestoreState and the persistence of the latest checkpoint locally may
// be redundant, we may be able to remove the latter.
func (sm *StateManager) Checkpoint(checkpoint *checkpoint.StableCheckpoint) error {
	// deserialize checkpoint data from Mir checkpoint to check that is the
	// right format.
	ch := &Checkpoint{}
	if err := ch.FromBytes(checkpoint.Snapshot.AppData); err != nil {
		return xerrors.Errorf("error getting checkpoint data from mir checkpoint: %w", err)
	}
	log.With("miner", sm.MirManager.Addr).Debugf("Mir generated new checkpoint for height: %d", ch.Height)

	if err := sm.deliverCheckpoint(checkpoint, ch); err != nil {
		return err
	}

	// reset fifo between checkpoints to avoid requests getting stuck
	// see https://github.com/consensus-shipyard/lotus/issues/28
	sm.MirManager.Pool.Purge()
	return nil
}

// deliver checkpoint receives a checkpoint, persists it locally in the local block store, and delivers
// it to the mining process to include it in a new block.
func (sm *StateManager) deliverCheckpoint(checkpoint *checkpoint.StableCheckpoint, snapshot *Checkpoint) error {
	// if we deserialized it correctly, we can persist it directly in the data store.
	if err := sm.MirManager.ds.Put(sm.ctx, LatestCheckpointKey, checkpoint.Snapshot.AppData); err != nil {
		return xerrors.Errorf("error flushing latest checkpoint in datastore: %w", err)
	}

	// persist the stable checkpoint to initialize mir from it if needed
	b, err := checkpoint.Serialize()
	if err != nil {
		return xerrors.Errorf("error marshaling stable checkpoint", err)
	}
	// store latest checkpoint.
	if err := sm.MirManager.ds.Put(sm.ctx, LatestCheckpointPbKey, b); err != nil {
		return xerrors.Errorf("error flushing latest checkpoint in datastore: %w", err)
	}
	// index checkpoints by epoch to enable Mir to start from a specific checkpoint if needed
	// (this is useful to perform catastrophic recoveries of the network).
	if err := sm.MirManager.ds.Put(sm.ctx, HeightCheckIndexKey(snapshot.Height), b); err != nil {
		return xerrors.Errorf("error flushing latest checkpoint in datastore: %w", err)
	}

	// also index checkpoint snapshots by cid
	c, err := snapshot.Cid()
	if err != nil {
		return xerrors.Errorf("error computing cid for checkpoint: %w", err)
	}
	sm.prevCheckpoint = ParentMeta{Height: snapshot.Height, Cid: c}

	// store metadata for previous snapshot in datastore and manager to
	// perform additional verifications
	if err := sm.MirManager.ds.Put(sm.ctx, CidCheckIndexKey(c), checkpoint.Snapshot.AppData); err != nil {
		return xerrors.Errorf("error flushing latest checkpoint in datastore: %w", err)
	}

	// optionally persist the checkpoint in a file
	// (this is a best-effort process, if it fails we shouldn't kill the process)
	// in the future we could add a flag that makes persistence STRICT to notify
	// that this process should fail if persisting to file fails.
	if sm.MirManager.checkpointRepo != "" {
		// wrapping it in a routine to take it out of the critical path.
		go func() {
			path := path.Join(sm.MirManager.checkpointRepo, "checkpoint-"+snapshot.Height.String()+".chkp")
			if err := serializedCheckToFile(b, path); err != nil {
				log.Errorf("error persisting checkpoint for height %d in path %s: %s", snapshot.Height, path, err)
			}
		}()
	}

	// Send the checkpoint to Lotus and handle it there
	log.With("miner", sm.MirManager.Addr).Debug("Sending checkpoint to mining process to include in block")
	sm.NextCheckpoint <- checkpoint
	return nil
}

func HeightCheckIndexKey(epoch abi.ChainEpoch) datastore.Key {
	return datastore.NewKey(CheckpointDBKeyPrefix + epoch.String())
}

func CidCheckIndexKey(c cid.Cid) datastore.Key {
	return datastore.NewKey(CheckpointDBKeyPrefix + c.String())
}

func maxFaulty(n int) int {
	// assuming n > 3f:
	//   return max f
	return (n - 1) / 3
}

func weakQuorum(n int) int {
	// assuming n > 3f:
	//   return min q: q > f
	return maxFaulty(n) + 1
}

// pollCheckpoint listens to new available checkpoints to be
// added in lotus blocks.
func (sm *StateManager) pollCheckpoint() *checkpoint.StableCheckpoint {
	select {
	case ch := <-sm.NextCheckpoint:
		log.Debugf("Polling checkpoint successful. Sending checkpoint for inclusion in block.")
		return ch
	default:
		return nil
	}
}

// consume a value from a buffered channel to release
// it and support new values to be sent without blocking.
// (this is needed because Mir sometimes call RestoreData several
// times with outdated checkpoints before fully syncing)
func (sm *StateManager) releaseNextCheckpointChan() {
	select {
	case <-sm.NextCheckpoint:
		return
	default:
		return
	}
}

// waitForBlock waits for the syncer to see as the head of the chain
// the block for the height determined as an input.
//
// The timeout to determine how much to wait before aborting is
// determined by the number of blocks to sync.
func (sm *StateManager) waitForBlock(height abi.ChainEpoch) error {
	// get base to determine the gap to sync and configure timeout.
	base, err := sm.api.ChainHead(sm.ctx)
	if err != nil {
		return xerrors.Errorf("failed to get chain head: %w", err)
	}

	// one minute baseline timeout
	timeout := 60 * time.Second
	// add extra if the gap is big.
	if base.Height() < height {
		timeout = timeout + time.Duration(height-base.Height())*time.Second
	}
	log.With("miner", sm.MirManager.Addr).Debugf("waiting for block on height %d with timeout %v", height, timeout)
	ctx, cancel := context.WithTimeout(sm.ctx, timeout)
	defer cancel()

	out := make(chan bool, 1)
	go func() {
		head := abi.ChainEpoch(0)
		// poll until we get the desired height.
		// TODO: We may be able to add a slight sleep here if needed.
		for head != height {
			base, err := sm.api.ChainHead(sm.ctx)
			if err != nil {
				log.Errorf("failed to get chain head: %v", err)
				return
			}
			head = base.Height()
			if head > height {
				log.Warnf("we already have a larger head. waiting %d, head %d", height, head)
				break
			}
		}
		out <- true
	}()

	select {
	case <-out:
		return nil
	case <-ctx.Done():
		return ErrMirCtxCanceledWhileWaitingSnapshot
	}
}

// get first checkpoint from genesis when a validator is restarted from scratch.
func (sm *StateManager) firstEpochCheckpoint() (*Checkpoint, error) {
	// if we are restarting the peer we may have something in the
	// mir database, if not let's return the genesis one.
	chb, err := sm.MirManager.ds.Get(sm.ctx, LatestCheckpointKey)
	if err != nil {
		if err == datastore.ErrNotFound {
			genesis, err := sm.api.ChainGetGenesis(sm.ctx)
			if err != nil {
				return nil, xerrors.Errorf("error getting genesis block: %w", err)
			}
			// return genesis checkpoint
			return &Checkpoint{
				Height:    1,                                                     // we assume the genesis has been verified, we start from 1
				Parent:    ParentMeta{Height: 0, Cid: genesis.Blocks()[0].Cid()}, // genesis checkpoint
				BlockCids: make([]cid.Cid, 0),
			}, nil
		}
		return nil, err
	}
	ch := &Checkpoint{}
	if err := ch.FromBytes(chb); err != nil {
		return nil, err
	}
	return ch, nil
}

// GetCheckpointPeriod returns the checkpoint period for the current epoch.
//
// The checkpoint period is computed as the number of validator times the
// segment length.
func (sm *StateManager) GetCheckpointPeriod() abi.ChainEpoch {
	return abi.ChainEpoch(sm.MirManager.segmentLength * len(sm.memberships[sm.currentEpoch]))
}

func parseTx(tx []byte) (interface{}, error) {
	ln := len(tx)
	// This is very simple input validation to be protected against invalid messages.
	// TODO: Make this smarter.
	if ln <= 2 {
		return nil, fmt.Errorf("mir tx len %d is too small", ln)
	}

	var err error
	var msg interface{}

	// TODO: Consider taking it out to a MirMessageFromBytes function
	// into mir/types.go so that we have all msgType functionality in
	// the same place.
	lastByte := tx[ln-1]
	switch lastByte {
	case SignedMessageType:
		msg, err = types.DecodeSignedMessage(tx[:ln-1])
	case ConfigMessageType:
		return nil, fmt.Errorf("config message is not supported")
	default:
		err = fmt.Errorf("unknown message type %d", lastByte)
	}

	if err != nil {
		return nil, err
	}

	return msg, nil
}
