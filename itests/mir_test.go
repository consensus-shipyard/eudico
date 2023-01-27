package itests

// These tests check that Eudico/Mir bundle operates normally.
//
// Notes:
//   - It is assumed that the first F of N nodes can be byzantine;
//   - In terms of Go, that means that nodes[:MirFaultyValidatorNumber] can be byzantine,
//     and nodes[MirFaultyValidatorNumber:] are honest nodes.

import (
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/chain/consensus/mir/validator"

	"github.com/filecoin-project/lotus/chain/consensus/mir"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/itests/kit"
)

const (
	MirTotalValidatorNumber  = 4 // N = 3F+1
	MirFaultyValidatorNumber = (MirTotalValidatorNumber - 1) / 3
	MirReferenceSyncingNode  = MirFaultyValidatorNumber // The first non-faulty node is a syncing node.
	MirHonestValidatorNumber = MirTotalValidatorNumber - MirFaultyValidatorNumber
	MirLearnersNumber        = MirFaultyValidatorNumber + 1
	TestedBlockNumber        = 10
	MaxDelay                 = 30
)

var mirTestOpts = []interface{}{kit.ThroughRPC(), kit.MirConsensus()}

func setupMangler(t *testing.T) {
	require.Greater(t, MirFaultyValidatorNumber, 0)
	require.Equal(t, MirTotalValidatorNumber, MirHonestValidatorNumber+MirFaultyValidatorNumber)

	err := mir.SetEnvManglerParams(200*time.Millisecond, 2*time.Second, 0)
	require.NoError(t, err)

	t.Cleanup(func() {
		err := os.Unsetenv(mir.ManglerEnv)
		require.NoError(t, err)
	})
}

// TestMirConsensusWithMangler tests that Mir operates normally when messaged are dropped or delayed.
func TestMirConsensusWithMangler(t *testing.T) {
	TestMirAllNodesMiningWithMangling(t)
	TestMirAllNodesMiningWithMessagingWithMangler(t)
	TestMirWhenLearnersJoinWithMangler(t)
}

func TestMirConsensusSmoke(t *testing.T) {
	TestMirOneNodeMining(t)
	TestMirAllNodesMining(t)
	TestMirStartStop(t)
	TestGenesisBlocksOfValidatorsAndLearners(t)
	TestMirFNodesNeverStart(t)
}

func TestMirAdvancedConsensus(t *testing.T) {
	TestMirWhenLearnersJoin(t)
	TestMirNodesStartWithRandomDelay(t)
	TestMirFNodesNeverStart(t)
	TestMirFNodesStartWithRandomDelay(t)
	TestMirAllNodesMiningWithMessaging(t)
	TestMirWithFOmissionNodes(t)
	TestMirWithFCrashedNodes(t)
	TestMirWithFCrashedAndRecoveredNodes(t)
	TestMirFNodesCrashLongTimeApart(t)
	TestMirFNodesHaveLongPeriodNoNetworkAccessButDoNotCrash(t)
	TestMirFNodesSleepAndThenOperate(t)
}

func TestMirConsensus(t *testing.T) {
	TestMirOneNodeMining(t)
	TestMirTwoNodesMining(t)
	TestMirAllNodesMining(t)
	TestGenesisBlocksOfValidatorsAndLearners(t)
	TestMirWhenLearnersJoin(t)
	TestMirNodesStartWithRandomDelay(t)
	TestMirFNodesNeverStart(t)
	TestMirFNodesStartWithRandomDelay(t)
	TestMirAllNodesMiningWithMessaging(t)
	TestMirWithFOmissionNodes(t)
	TestMirWithFCrashedNodes(t)
	TestMirWithFCrashedAndRecoveredNodes(t)
	TestMirStartStop(t)
	TestMirFNodesCrashLongTimeApart(t)
	TestMirFNodesHaveLongPeriodNoNetworkAccessButDoNotCrash(t)
	TestMirFNodesSleepAndThenOperate(t)
}

func TestMirConsensusWithReconfiguration(t *testing.T) {
	TestMirWithReconfiguration_AddAndRemoveOneNode(t)
	TestMirWithReconfiguration_AddOneNodeWithConfigurationRecovery(t)
	TestMirWithReconfiguration_AddOneNodeToMembershipFilesWithDelay(t)
	TestMirWithReconfiguration_AddThreeNodes(t)
	TestMirWithReconfiguration_AddThreeNodesOneByOne(t)
	TestMirWithReconfiguration_NewNodeFailsToJoin(t)
}

// TestMirWithReconfiguration_AddAndRemoveOneNode tests that the reconfiguration mechanism operates normally
// if a new validator joins the network and then leaves it.
func TestMirWithReconfiguration_AddAndRemoveOneNode(t *testing.T) {
	var wg sync.WaitGroup

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		os.Remove(membershipFileName) // nolint
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)
	ens.SaveMirValidatorsToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(0), membership.GetConfigurationNumber())

	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[:MirTotalValidatorNumber])

	err = kit.AdvanceChain(ctx, 2*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveMirValidatorsToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	require.Equal(t, uint64(1), membership.GetConfigurationNumber())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[MirTotalValidatorNumber:])

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)

	t.Log(">>> remove the last added validator from membership")
	ens.SaveMirValidatorsToFile(2, membershipFileName, miners[:MirTotalValidatorNumber]...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(2), membership.GetConfigurationNumber())

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	// Core validators must send 2 messages.
	for _, m := range miners[:MirTotalValidatorNumber] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.SentConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(2), binary.LittleEndian.Uint64(nonce))

		nonce, err = db.Get(ctx, mir.ExecutedConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1), binary.LittleEndian.Uint64(nonce))
	}

	// Added validators must send 1 message.
	for _, m := range miners[MirTotalValidatorNumber:] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.SentConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1), binary.LittleEndian.Uint64(nonce))
	}
}

// TestMirWithReconfiguration_AddOneNodeWithConfigurationRecovery tests that the reconfiguration mechanism operates normally
// if a new validator join the network and after recovery.
func TestMirWithReconfiguration_AddOneNodeWithConfigurationRecovery(t *testing.T) {
	var wg sync.WaitGroup

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		os.Remove(membershipFileName) // nolint
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)
	ens.SaveMirValidatorsToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	bn := make([]byte, 8)
	recoveredNonce := uint64(4)
	binary.LittleEndian.PutUint64(bn, recoveredNonce)

	var dbs []*kit.TestDB
	for i := range miners[:MirTotalValidatorNumber] {
		_ = i
		db := kit.NewTestDB()
		err := db.Put(ctx, mir.SentConfigurationNumberKey, bn)
		require.NoError(t, err)
		err = db.Put(ctx, mir.ExecutedConfigurationNumberKey, bn)
		require.NoError(t, err)

		dbs = append(dbs, db)
	}
	for i := range miners[MirTotalValidatorNumber:] {
		_ = i
		dbs = append(dbs, kit.NewTestDB())
	}

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(0), membership.GetConfigurationNumber())

	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFileAndDB(ctx, membershipFileName, &wg, dbs[:MirTotalValidatorNumber], miners[:MirTotalValidatorNumber])

	err = kit.AdvanceChain(ctx, 2*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveMirValidatorsToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	require.Equal(t, uint64(1), membership.GetConfigurationNumber())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFileAndDB(ctx, membershipFileName, &wg, dbs[MirTotalValidatorNumber:], miners[MirTotalValidatorNumber:])

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)

	// Core validators must send 1 message with recovered "nonce" .
	for _, m := range miners[:MirTotalValidatorNumber] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.SentConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1)+recoveredNonce, binary.LittleEndian.Uint64(nonce))

		nonce, err = db.Get(ctx, mir.ExecutedConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, recoveredNonce, binary.LittleEndian.Uint64(nonce))
	}
}

// TestMirWithReconfiguration_AddOneNodeToMembershipFilesWithDelay tests that the reconfiguration mechanism operates normally
// if a new validator is added to the membership file of validators with delays.
func TestMirWithReconfiguration_AddOneNodeToMembershipFilesWithDelay(t *testing.T) {
	var wg sync.WaitGroup

	membershipFiles := make([]string, MirTotalValidatorNumber+1)
	for i := 0; i < MirTotalValidatorNumber+1; i++ {
		membershipFiles[i] = kit.TempFileName("membership")
	}

	t.Cleanup(func() {
		for i := 0; i < MirTotalValidatorNumber+1; i++ {
			os.Remove(membershipFiles[i]) // nolint
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)

	// Append initial validators.
	for i := 0; i < MirTotalValidatorNumber; i++ {
		ens.SaveMirValidatorsToFile(0, membershipFiles[i], miners[:MirTotalValidatorNumber]...)
	}
	// Add all validators to the membership file of the new validator.
	ens.SaveMirValidatorsToFile(0, membershipFiles[MirTotalValidatorNumber], miners[:MirTotalValidatorNumber+1]...)

	// Run validators, including the added validator.
	ens.InterconnectFullNodes()
	for i := 0; i < MirTotalValidatorNumber; i++ {
		ens.BeginMirMiningWithMembershipFromFile(ctx, membershipFiles[i], &wg, 0, []*kit.TestMiner{miners[i]})
	}
	ens.BeginMirMiningWithMembershipFromFile(ctx, membershipFiles[MirTotalValidatorNumber], &wg, 0, miners[MirTotalValidatorNumber:])

	err := kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	// Add the new validator to the membership file of all other validators.
	t.Log(">>> new validator is being added to the membership files")
	for i := 0; i < MirTotalValidatorNumber; i++ {
		kit.RandomDelay(i + 10)
		ens.SaveMirValidatorsToFile(1, membershipFiles[i], miners...)

		membership, err := validator.NewValidatorSetFromFile(membershipFiles[i])
		require.NoError(t, err)
		require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	}

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)
}

// TestMirWithReconfiguration_AddThreeNodes tests that the reconfiguration mechanism operates normally
// if 3 new validators join the network at the same time. To add 3 validators we have to have a network with 7 validators.
func TestMirWithReconfiguration_AddThreeNodes(t *testing.T) {
	mirTotalValidatorNumber := 7
	mirAddedValidatorNumber := 3
	var wg sync.WaitGroup

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		os.Remove(membershipFileName) // nolint
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, mirTotalValidatorNumber+mirAddedValidatorNumber, mirTestOpts...)
	ens.SaveMirValidatorsToFile(0, membershipFileName, miners[:mirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, mirTotalValidatorNumber, membership.Size())

	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[:mirTotalValidatorNumber])

	err = kit.AdvanceChain(ctx, 20, nodes[:mirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:mirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> all new validators have been added to the membership")
	ens.SaveMirValidatorsToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, mirTotalValidatorNumber+mirAddedValidatorNumber, membership.Size())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[mirTotalValidatorNumber:])

	err = kit.AdvanceChain(ctx, 100, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)
}

// TestMirWithReconfiguration_AddThreeNodesOneByOne tests that the reconfiguration mechanism operates normally
// if 3 new validators join the network one by one.
func TestMirWithReconfiguration_AddThreeNodesOneByOne(t *testing.T) {
	mirAddedValidatorNumber := 3
	var wg sync.WaitGroup

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		os.Remove(membershipFileName) // nolint
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+mirAddedValidatorNumber, mirTestOpts...)
	ens.SaveMirValidatorsToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())

	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[:MirTotalValidatorNumber])

	err = kit.AdvanceChain(ctx, 30, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")

	for i := 0; i < mirAddedValidatorNumber; i++ {
		ens.SaveMirValidatorsToFile(uint64(i+1), membershipFileName, miners[:MirTotalValidatorNumber+i+1]...)
		membership, err = validator.NewValidatorSetFromFile(membershipFileName)
		require.NoError(t, err)
		require.Equal(t, MirTotalValidatorNumber+i+1, membership.Size())
		// Start new miners.
		ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, []*kit.TestMiner{miners[MirTotalValidatorNumber+i]})

		err = kit.AdvanceChain(ctx, 20, nodes[:MirTotalValidatorNumber+i+1]...)
		require.NoError(t, err)
	}

	err = kit.AdvanceChain(ctx, 30, nodes...)
	require.NoError(t, err)

	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirWithReconfiguration_NewNodeFailsToJoin tests that the reconfiguration mechanism operates normally
// if a new validator cannot join the network.
// In this test we don't stop the faulty validator explicitly, instead, we don't spawn it.
func TestMirWithReconfiguration_NewNodeFailsToJoin(t *testing.T) {
	var wg sync.WaitGroup

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		os.Remove(membershipFileName) // nolint
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+MirFaultyValidatorNumber, mirTestOpts...)
	ens.SaveMirValidatorsToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)
	ens.InterconnectFullNodes().BeginMirMiningWithMembershipFromFile(ctx, membershipFileName, &wg, 0, miners[:MirTotalValidatorNumber])

	err := kit.AdvanceChain(ctx, 3*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveMirValidatorsToFile(1, membershipFileName, miners...)

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
}

// TestMirOneNodeMining tests that a Mir node can mine blocks.
func TestMirOneNodeMining(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	full, miner, ens := kit.EnsembleMinimalMir(t, mirTestOpts...)
	ens.BeginMirMining(ctx, &wg, miner)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, full)
	require.NoError(t, err)
}

// TestMirTwoNodesMining tests that two Mir nodes can mine blocks.
//
// NOTE: The peculiarity of this test is that it uses other mechanisms to instantiate testing
// comparing to the main tests here.
func TestMirTwoNodesMining(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	n1, n2, m1, m2, ens := kit.EnsembleTwoMirNodes(t, mirTestOpts...)

	// Fail if genesis blocks are different
	gen1, err := n1.ChainGetGenesis(ctx)
	require.NoError(t, err)
	gen2, err := n2.ChainGetGenesis(ctx)
	require.NoError(t, err)
	require.Equal(t, gen1.String(), gen2.String())

	// Fail if nodes have peers
	p, err := n1.NetPeers(ctx)
	require.NoError(t, err)
	require.Empty(t, p, "node one has peers")

	p, err = n2.NetPeers(ctx)
	require.NoError(t, err)
	require.Empty(t, p, "node two has peers")

	ens.Connect(n1, n2).BeginMirMining(ctx, &wg, m1, m2)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, n1, n2)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, n1, n2)
	require.NoError(t, err)
}

// TestMirAllNodesMining tests that n nodes can mine blocks normally.
func TestMirAllNodesMining(t *testing.T) {
	t.Run("TestMirAllNodesMining", func(t *testing.T) {
		var wg sync.WaitGroup

		ctx, cancel := context.WithCancel(context.Background())
		defer func() {
			t.Logf("[*] defer: cancelling %s context", t.Name())
			cancel()
			wg.Wait()
		}()

		nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
		ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

		err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
		require.NoError(t, err)
		err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
		require.NoError(t, err)
	})
}

// TestMirAllNodesMiningWithMangling run TestMirAllNodesMining with mangler.
func TestMirAllNodesMiningWithMangling(t *testing.T) {
	setupMangler(t)
	TestMirAllNodesMining(t)
}

// TestMirFNodesNeverStart tests that n − f nodes operate normally if f nodes never start.
func TestMirFNodesNeverStart(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirHonestValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirWhenLearnersJoin tests that all nodes operate normally
// if new learner joins when the network is already started and syncs the whole network.
func TestMirWhenLearnersJoin(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Log(">>> learners join")

	var learners []*kit.TestFullNode
	for i := 0; i < MirLearnersNumber; i++ {
		var learner kit.TestFullNode
		ens.FullNode(&learner, kit.LearnerNode())
		require.Equal(t, true, learner.IsLearner())
		learners = append(learners, &learner)
	}

	ens.Start().InterconnectFullNodes()

	err = kit.AdvanceChain(ctx, TestedBlockNumber, learners...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], append(nodes[1:], learners...)...)
	require.NoError(t, err)
}

// TestMirWhenLearnersJoinWithMangler runs TestMirWhenLearnersJoin with mangler.
func TestMirWhenLearnersJoinWithMangler(t *testing.T) {
	setupMangler(t)
	TestMirWhenLearnersJoin(t)
}

// TestGenesisBlocksOfValidatorsAndLearners tests that genesis for validators and learners are correct.
func TestGenesisBlocksOfValidatorsAndLearners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
	}()

	nodes, _, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.Bootstrapped()

	genesis, err := nodes[0].ChainGetGenesis(ctx)
	require.NoError(t, err)
	for i := range nodes[1:] {
		gen, err := nodes[i].ChainGetGenesis(ctx)
		require.NoError(t, err)
		require.Equal(t, genesis.String(), gen.String())
	}

	var learners []*kit.TestFullNode
	for i := 0; i < MirLearnersNumber; i++ {
		var learner kit.TestFullNode
		ens.FullNode(&learner, kit.LearnerNode()).Start()
		require.Equal(t, true, learner.IsLearner())
		learners = append(learners, &learner)
	}

	ens.Start()

	for i := range learners {
		gen, err := learners[i].ChainGetGenesis(ctx)
		require.NoError(t, err)
		require.Equal(t, genesis.String(), gen.String())
	}
}

// TestMirMessageFromLearner tests that messages can be sent from learners and validators,
// and successfully proposed by validators
func TestMirMessageFromLearner(t *testing.T) {
	t.Skip()
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	// immediately start learners
	var learners []*kit.TestFullNode
	for i := 0; i < MirLearnersNumber; i++ {
		var learner kit.TestFullNode
		ens.FullNode(&learner, kit.LearnerNode())
		require.Equal(t, true, learner.IsLearner())
		learners = append(learners, &learner)
	}

	ens.Start().InterconnectFullNodes()

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	// send funds to learners so they can send a message themselves
	for _, l := range learners {
		src, err := nodes[0].WalletDefaultAddress(ctx)
		require.NoError(t, err)
		dst, err := l.WalletDefaultAddress(ctx)
		require.NoError(t, err)

		t.Logf(">>> node %s is sending a message to node %s", src, dst)

		smsg, err := nodes[0].MpoolPushMessage(ctx, &types.Message{
			From:  src,
			To:    dst,
			Value: types.FromFil(10),
		}, nil)
		require.NoError(t, err)

		err = kit.MirNodesWaitMsg(ctx, smsg.Cid(), nodes...)
		require.NoError(t, err)
	}

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	for range learners {
		rand.Seed(time.Now().UnixNano())
		j := rand.Intn(len(learners))
		src, err := learners[j].WalletDefaultAddress(ctx)
		require.NoError(t, err)

		dst, err := learners[(j+1)%len(learners)].WalletDefaultAddress(ctx)
		require.NoError(t, err)

		t.Logf(">>> learner %s is sending a message to node %s", src, dst)

		smsg, err := learners[j].MpoolPushMessage(ctx, &types.Message{
			From:  src,
			To:    dst,
			Value: types.FromFil(1),
		}, nil)
		require.NoError(t, err)

		err = kit.MirNodesWaitMsg(ctx, smsg.Cid(), nodes...)
		require.NoError(t, err)

		// no message pending in message pool
		pend, err := learners[j].MpoolPending(ctx, types.EmptyTSK)
		require.NoError(t, err)
		require.Equal(t, len(pend), 0)
	}
}

// TestMirNodesStartWithRandomDelay tests that all nodes eventually operate normally
// if all nodes start with large, random delays (1-2 minutes).
func TestMirNodesStartWithRandomDelay(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithDelay(ctx, &wg, MaxDelay, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirFNodesStartWithRandomDelay tests that all nodes eventually operate normally
// if f nodes start with large, random delays (1-2 minutes).
func TestMirFNodesStartWithRandomDelay(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithDelayForFaultyNodes(ctx, &wg, MaxDelay, miners[MirFaultyValidatorNumber:], miners[:MirFaultyValidatorNumber]...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirAllNodesMiningWithMessaging tests that sending messages mechanism operates normally for all nodes when there are not any faults.
func TestMirAllNodesMiningWithMessaging(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	for range nodes {
		rand.Seed(time.Now().UnixNano())
		j := rand.Intn(len(nodes))
		src, err := nodes[j].WalletDefaultAddress(ctx)
		require.NoError(t, err)

		dst, err := nodes[(j+1)%len(nodes)].WalletDefaultAddress(ctx)
		require.NoError(t, err)

		t.Logf(">>> node %s is sending a message to node %s", src, dst)

		smsg, err := nodes[j].MpoolPushMessage(ctx, &types.Message{
			From:  src,
			To:    dst,
			Value: big.Zero(),
		}, nil)
		require.NoError(t, err)

		err = kit.MirNodesWaitMsg(ctx, smsg.Cid(), nodes...)
		require.NoError(t, err)
	}
}

// TestMirAllNodesMiningWithMessagingWithMangler runs TestMirAllNodesMiningWithMessaging with mangler.
func TestMirAllNodesMiningWithMessagingWithMangler(t *testing.T) {
	setupMangler(t)
	TestMirAllNodesMiningWithMessaging(t)
}

// TestMirWithFOmissionNodes tests that n − f nodes operate normally and can recover
// if f nodes do not have access to network at the same time.
func TestMirWithFOmissionNodes(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> disconnecting %d Mir miners", MirFaultyValidatorNumber)

	restoreConnections := ens.DisconnectMirMiners(miners[:MirFaultyValidatorNumber])

	err = kit.ChainHeightCheckWithFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> reconnecting %d Mir miners", MirFaultyValidatorNumber)
	restoreConnections()

	// err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	// require.NoError(t, err)
	time.Sleep(10 * time.Second)
	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirWithFCrashedNodes tests that n − f nodes operate normally and can recover
// if f nodes crash at the same time.
func TestMirWithFCrashedNodes(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d miners", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, 0, miners[:MirFaultyValidatorNumber]...)

	err = kit.ChainHeightCheckWithFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d miners", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithState(ctx, miners[:MirFaultyValidatorNumber]...)

	// FIXME: Consider using advance chain instead of a time.Sleep here if possible.
	// err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	// require.NoError(t, err)
	time.Sleep(10 * time.Second)
	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirStartStop tests that Mir nodes can be stopped.
func TestMirStartStop(t *testing.T) {
	t.Run("TestMirStartStop", func(t *testing.T) {
		var wg sync.WaitGroup
		wait := make(chan struct{})

		ctx, cancel := context.WithCancel(context.Background())
		defer func() {
			t.Logf("[*] defer: cancelling %s context", t.Name())
			cancel()
			select {
			case <-time.After(10 * time.Second):
				t.Fatalf("fail to stop Mir nodes")
			case <-wait:
			}
		}()

		go func() {
			// This goroutine is leaking after time.After(x) seconds with panicking.
			select {
			case <-time.After(200 * time.Second):
				panic("test time exceeded")
			case <-ctx.Done():
				return
			}
		}()

		go func() {
			// This goroutine is leaking after time.After(x) seconds with panicking.
			wg.Wait()
			close(wait)
		}()

		nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
		ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

		err := kit.AdvanceChain(ctx, 20, nodes...)
		require.NoError(t, err)
	})
}

// TestMirWithFCrashedAndRecoveredNodes tests that n − f nodes operate normally without significant interruption,
// and recovered nodes eventually operate normally
// if f nodes crash and then recover (with only initial state) after a long delay (few minutes).
func TestMirWithFCrashedAndRecoveredNodes(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d miners", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, 0, miners[:MirFaultyValidatorNumber]...)

	err = kit.ChainHeightCheckWithFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d miners from scratch", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithEmptyState(ctx, miners[:MirFaultyValidatorNumber]...)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Log(">>> checking nodes are in sync")
	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirFNodesCrashLongTimeApart tests that n − f nodes operate normally
// if f nodes crash, long time apart (few minutes).
func TestMirFNodesCrashLongTimeApart(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d nodes", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, MaxDelay, miners[:MirFaultyValidatorNumber]...)

	err = kit.ChainHeightCheckWithFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d nodes", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithState(ctx, miners[:MirFaultyValidatorNumber]...)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirFNodesHaveLongPeriodNoNetworkAccessButDoNotCrash tests that n − f nodes operate normally
// and partitioned nodes eventually catch up
// if f nodes have a long period of no network access, but do not crash.
func TestMirFNodesHaveLongPeriodNoNetworkAccessButDoNotCrash(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		wg.Wait()
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &wg, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> disconnecting %d Mir miners", MirFaultyValidatorNumber)
	restoreConnections := ens.DisconnectMirMiners(miners[:MirFaultyValidatorNumber])

	t.Logf(">>> delay")
	kit.RandomDelay(MaxDelay)

	err = kit.ChainHeightCheckWithFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> restoring network connections")
	restoreConnections()

	// FIXME: Consider using advance chain instead of a time.Sleep here if possible.
	// err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	// require.NoError(t, err)
	time.Sleep(10 * time.Second)
	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirFNodesSleepAndThenOperate tests that n − f nodes operate normally without significant interruption
// and woken up nodes eventually operate normally
// if f  nodes sleep for a significant amount of time and then continue operating but keep network connection.
func TestMirFNodesSleepAndThenOperate(t *testing.T) {
	// TBD
	t.Skip()
}
