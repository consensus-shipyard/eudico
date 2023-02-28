package itests

// These tests check that Eudico/Mir bundle operates normally.
//
// Notes:
//   - It is assumed that the first F of N nodes can be byzantine;
//   - In terms of Go, that means that nodes[:MirFaultyValidatorNumber] can be byzantine,
//     and nodes[MirFaultyValidatorNumber:] are honest nodes.

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/chain/consensus/mir"
	"github.com/filecoin-project/lotus/chain/consensus/mir/validator"
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
	MaxDelay                 = 15
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

// TestMirReconfiguration_AddAndRemoveOneValidator tests that the reconfiguration mechanism operates normally
// if a new validator joins the network and then leaves it.
func TestMirReconfiguration_AddAndRemoveOneValidator(t *testing.T) {
	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(0), membership.GetConfigurationNumber())

	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[:MirTotalValidatorNumber],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 2*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveValidatorSetToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	require.Equal(t, uint64(1), membership.GetConfigurationNumber())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[MirTotalValidatorNumber:],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	t.Log(">>> AdvanceChain")
	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	t.Log(">>> CheckNodesInSync")
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)

	t.Log(">>> remove the last added validator from membership")
	ens.SaveValidatorSetToFile(2, membershipFileName, miners[:MirTotalValidatorNumber]...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(2), membership.GetConfigurationNumber())

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	// Check the configuration client persistent DB state.
	// Core validators have sent 2 messages.
	for _, m := range miners[:MirTotalValidatorNumber] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.NextConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(2), binary.LittleEndian.Uint64(nonce))

		nonce, err = db.Get(ctx, mir.NextAppliedConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(2), binary.LittleEndian.Uint64(nonce))
	}

	// Added validators must send 1 message.
	for _, m := range miners[MirTotalValidatorNumber:] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.NextConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1), binary.LittleEndian.Uint64(nonce))
	}
}

// TestMirReconfiguration_AddOneValidatorAtHeight tests that the reconfiguration mechanism operates normally
// if a new validator joins the network that have produced 100 blocks.
func TestMirReconfiguration_AddOneValidatorAtHeight(t *testing.T) {
	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())
	require.Equal(t, uint64(0), membership.GetConfigurationNumber())

	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[:MirTotalValidatorNumber],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 10*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveValidatorSetToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	require.Equal(t, uint64(1), membership.GetConfigurationNumber())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[MirTotalValidatorNumber:],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)
}

// TestMirReconfiguration_AddOneValidatorWithConfigurationRecovery tests that the reconfiguration mechanism operates normally
// if a new validator join the network and after recovery.
// TODO: refactor this test by separating DB test primitives.
func TestMirReconfiguration_AddOneValidatorWithConfigurationRecovery(t *testing.T) {
	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	bn := make([]byte, 8)
	recoveredRequestNonce := uint64(4)
	binary.LittleEndian.PutUint64(bn, recoveredRequestNonce)

	var dbs []*kit.TestDB
	for i := range miners[:MirTotalValidatorNumber] {
		_ = i
		db := kit.NewTestDB()
		err := db.Put(ctx, mir.NextConfigurationNumberKey, bn)
		require.NoError(t, err)
		err = db.Put(ctx, mir.NextAppliedConfigurationNumberKey, bn)
		require.NoError(t, err)

		// -- store fake votes
		recs := []mir.VoteRecord{{
			ConfigurationNumber: 0, ValSetHash: "hash", VotedValidators: []mir.VotedValidator{{ID: "id1"}},
		}}
		r := mir.VoteRecords{
			Records: recs,
		}

		br := new(bytes.Buffer)
		err = r.MarshalCBOR(br)
		require.NoError(t, err)
		err = db.Put(ctx, mir.ReconfigurationVotesKey, br.Bytes())
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

	ens.InterconnectFullNodes()
	ens.BeginMirMiningWithConfig(ctx, g, miners[:MirTotalValidatorNumber], &kit.MiningConfig{
		MembershipType:     kit.FileMembership,
		MembershipFileName: membershipFileName,
		Databases:          dbs[:MirTotalValidatorNumber],
	})

	t.Log(">>> check that nodes are advanced")
	err = kit.AdvanceChain(ctx, 2*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> check that persisted votes restored")
	for _, m := range miners[:MirTotalValidatorNumber] {
		db := m.GetDB()

		nonce, err := db.Get(ctx, mir.NextConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, recoveredRequestNonce, binary.LittleEndian.Uint64(nonce))

		nonce, err = db.Get(ctx, mir.NextAppliedConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, recoveredRequestNonce, binary.LittleEndian.Uint64(nonce))

		b, err := db.Get(ctx, mir.ReconfigurationVotesKey)
		require.NoError(t, err)
		var r mir.VoteRecords
		err = r.UnmarshalCBOR(bytes.NewReader(b))
		require.NoError(t, err)
		for _, v := range r.Records {
			require.Equal(t, uint64(0), v.ConfigurationNumber)
			require.Equal(t, "hash", v.ValSetHash)
		}
		votes := mir.GetConfigurationVotes(r.Records)
		require.Greater(t, MirTotalValidatorNumber, len(votes))
	}

	var cn uint64 = 1

	t.Log(">>> new validators have been added to the membership")
	ens.SaveValidatorSetToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	require.Equal(t, cn, membership.GetConfigurationNumber())
	// Start new miners.
	ens.InterconnectFullNodes()

	ens.BeginMirMiningWithConfig(ctx, g, miners[MirTotalValidatorNumber:], &kit.MiningConfig{
		MembershipType:     kit.FileMembership,
		MembershipFileName: membershipFileName,
		Databases:          dbs[MirTotalValidatorNumber:],
	})

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)

	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)

	// Core validators must send 1 message with recovered "nonce".
	for _, m := range miners[:MirTotalValidatorNumber] {
		db := m.GetDB()
		nonce, err := db.Get(ctx, mir.NextConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1)+recoveredRequestNonce, binary.LittleEndian.Uint64(nonce))

		nonce, err = db.Get(ctx, mir.NextAppliedConfigurationNumberKey)
		require.NoError(t, err)
		require.Equal(t, uint64(1)+recoveredRequestNonce, binary.LittleEndian.Uint64(nonce))

		b, err := db.Get(ctx, mir.ReconfigurationVotesKey)
		require.NoError(t, err)
		var r mir.VoteRecords
		err = r.UnmarshalCBOR(bytes.NewReader(b))
		require.NoError(t, err)
		for _, v := range r.Records {
			require.Equal(t, cn, v.ConfigurationNumber)
		}
		votes := mir.GetConfigurationVotes(r.Records)
		require.Greater(t, MirTotalValidatorNumber, len(votes))
	}
}

// TestMirReconfiguration_AddOneValidatorToMembershipWithDelay tests that the reconfiguration mechanism operates normally
// if a new validator is added to the membership files with delays.
func TestMirReconfiguration_AddOneValidatorToMembershipWithDelay(t *testing.T) {
	membershipFiles := make([]string, MirTotalValidatorNumber+1)
	for i := 0; i < MirTotalValidatorNumber+1; i++ {
		membershipFiles[i] = kit.TempFileName("membership")
	}

	t.Cleanup(func() {
		for i := 0; i < MirTotalValidatorNumber+1; i++ {
			err := os.Remove(membershipFiles[i])
			require.NoError(t, err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+1, mirTestOpts...)

	// Append initial validators.
	for i := 0; i < MirTotalValidatorNumber; i++ {
		ens.SaveValidatorSetToFile(0, membershipFiles[i], miners[:MirTotalValidatorNumber]...)
	}
	// Add all validators to the membership file of the new validator.
	ens.SaveValidatorSetToFile(0, membershipFiles[MirTotalValidatorNumber], miners[:MirTotalValidatorNumber+1]...)

	// Run validators, including the added validator.
	ens.InterconnectFullNodes()
	for i := 0; i < MirTotalValidatorNumber; i++ {
		ens.BeginMirMiningWithConfig(ctx, g, []*kit.TestMiner{miners[i]}, &kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFiles[i],
		})
	}

	ens.BeginMirMiningWithConfig(ctx, g, miners[MirTotalValidatorNumber:], &kit.MiningConfig{
		MembershipType:     kit.FileMembership,
		MembershipFileName: membershipFiles[MirTotalValidatorNumber],
	})

	err := kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	// Add the new validator to the membership file of all other validators.
	t.Log(">>> new validator is being added to the membership files")
	for i := 0; i < MirTotalValidatorNumber; i++ {
		kit.RandomDelay(i + 10)
		ens.SaveValidatorSetToFile(1, membershipFiles[i], miners...)

		membership, err := validator.NewValidatorSetFromFile(membershipFiles[i])
		require.NoError(t, err)
		require.Equal(t, MirTotalValidatorNumber+1, membership.Size())
	}

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)
}

// TestMirReconfiguration_AddValidatorsOnce tests that the reconfiguration mechanism operates normally
// if new validators join the network at the same time.
func TestMirReconfiguration_AddValidatorsOnce(t *testing.T) {
	initialValidatorNumber := 4
	addedValidatorNumber := 2

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, initialValidatorNumber+addedValidatorNumber, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:initialValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, initialValidatorNumber, membership.Size())

	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[:initialValidatorNumber],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 20, nodes[:initialValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:initialValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> all new validators have been added to the membership")
	ens.SaveValidatorSetToFile(1, membershipFileName, miners...)
	membership, err = validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, initialValidatorNumber+addedValidatorNumber, membership.Size())
	// Start new miners.
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[initialValidatorNumber:],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 40, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes...)
	require.NoError(t, err)
}

// TestMirReconfiguration_AddValidatorsOneByOne tests that the reconfiguration mechanism operates normally
// if validators join the network one by one.
func TestMirReconfiguration_AddValidatorsOneByOne(t *testing.T) {
	addedValidatorNumber := 3

	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+addedValidatorNumber, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)

	membership, err := validator.NewValidatorSetFromFile(membershipFileName)
	require.NoError(t, err)
	require.Equal(t, MirTotalValidatorNumber, membership.Size())

	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[:MirTotalValidatorNumber],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err = kit.AdvanceChain(ctx, 20, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	for i := 1; i <= addedValidatorNumber; i++ {
		t.Logf(">>> new validator %d is being added to the membership", i)
		ens.SaveValidatorSetToFile(uint64(i), membershipFileName, miners[:MirTotalValidatorNumber+i]...)
		membership, err = validator.NewValidatorSetFromFile(membershipFileName)
		require.NoError(t, err)

		require.Equal(t, MirTotalValidatorNumber+i, membership.Size())
		require.Equal(t, MirTotalValidatorNumber+i, len(miners[:MirTotalValidatorNumber+i]))
		require.Equal(t, MirTotalValidatorNumber+i-1, len(nodes[1:MirTotalValidatorNumber+i]))

		// Start new miners.
		ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, []*kit.TestMiner{miners[MirTotalValidatorNumber+i-1]},
			&kit.MiningConfig{
				MembershipType:     kit.FileMembership,
				MembershipFileName: membershipFileName,
			})

		t.Logf(">>> advancing the chain after adding validator %d", i)
		err = kit.AdvanceChain(ctx, 20, nodes[:MirTotalValidatorNumber+i]...)
		require.NoError(t, err)
	}

	err = kit.AdvanceChain(ctx, 30, nodes...)
	require.NoError(t, err)

	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirReconfiguration_NewNodeFailsToJoin tests that the reconfiguration mechanism operates normally
// if a new validator cannot join the network.
// In this test we don't stop the faulty validator explicitly, instead, we don't spawn it.
func TestMirReconfiguration_NewNodeFailsToJoin(t *testing.T) {
	membershipFileName := kit.TempFileName("membership")
	t.Cleanup(func() {
		err := os.Remove(membershipFileName)
		require.NoError(t, err)
	})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber+MirFaultyValidatorNumber, mirTestOpts...)
	ens.SaveValidatorSetToFile(0, membershipFileName, miners[:MirTotalValidatorNumber]...)
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners[:MirTotalValidatorNumber],
		&kit.MiningConfig{
			MembershipType:     kit.FileMembership,
			MembershipFileName: membershipFileName,
		})

	err := kit.AdvanceChain(ctx, 3*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:MirTotalValidatorNumber]...)
	require.NoError(t, err)

	t.Log(">>> new validators have been added to the membership")
	ens.SaveValidatorSetToFile(1, membershipFileName, miners...)

	err = kit.AdvanceChain(ctx, 4*TestedBlockNumber, nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[:MirTotalValidatorNumber]...)
	require.NoError(t, err)
}

// TestMirSmoke_OneNodeMines tests that a Mir node can mine blocks.
func TestMirSmoke_OneNodeMines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	full, miner, ens := kit.EnsembleMinimalMir(t, mirTestOpts...)
	ens.BeginMirMining(ctx, g, miner)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, full)
	require.NoError(t, err)
}

// TestMirBasic_TwoNodesMining tests that two Mir nodes can mine blocks.
//
// NOTE: The peculiarity of this test is that it uses other mechanisms to instantiate testing
// comparing to the main tests here.
func TestMirBasic_TwoNodesMining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
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

	ens.Connect(n1, n2).BeginMirMining(ctx, g, m1, m2)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, n1, n2)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, n1, n2)
	require.NoError(t, err)
}

// TestMirSmoke_AllNodesMine tests that n nodes can mine blocks normally.
func TestMirSmoke_AllNodesMine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChainNew(ctx, 10*TestedBlockNumber, miners, nodes)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)

}

// TestMirWithMangler_AllNodesMining run TestMirBasic_AllNodesMining with mangler.
func TestMirWithMangler_AllNodesMining(t *testing.T) {
	setupMangler(t)
	TestMirSmoke_AllNodesMine(t)
}

// TestMirBasic_FNodesNeverStart tests that n − f nodes operate normally if f nodes never start.
func TestMirBasic_FNodesNeverStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirHonestValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirBasic_WhenLearnersJoin tests that all nodes operate normally
// if new learner joins when the network is already started and syncs the whole network.
func TestMirBasic_WhenLearnersJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

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

// TestMirWithMangler_WhenLearnersJoin runs TestMir_WhenLearnersJoin with mangler.
func TestMirWithMangler_WhenLearnersJoin(t *testing.T) {
	setupMangler(t)
	TestMirBasic_WhenLearnersJoin(t)
}

// TestMirSmoke_GenesisBlocksOfValidatorsAndLearners tests that genesis for validators and learners are correct.
func TestMirSmoke_GenesisBlocksOfValidatorsAndLearners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
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

// TestMirBasic_MessageFromLearner tests that messages can be sent from learners and validators,
// and successfully proposed by validators
func TestMirBasic_MessageFromLearner(t *testing.T) {
	t.Skip()

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

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

	// Send funds to learners, so they can send a message themselves
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

// TestMirBasic_NodesStartWithRandomDelay tests that all nodes eventually operate normally
// if all nodes start with large, random delays (1-2 minutes).
func TestMirBasic_NodesStartWithRandomDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithDelay(ctx, g, MaxDelay, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirBasic_FNodesStartWithRandomDelay tests that all nodes eventually operate normally
// if f nodes start with large, random delays (1-2 minutes).
func TestMirBasic_FNodesStartWithRandomDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithDelayForFaultyNodes(ctx, g, MaxDelay, miners[MirFaultyValidatorNumber:], miners[:MirFaultyValidatorNumber]...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirBasic_AllNodesMiningWithMessaging tests that sending messages mechanism operates normally for all nodes when there are not any faults.
func TestMirBasic_AllNodesMiningWithMessaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)

	var cids []cid.Cid
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
		cids = append(cids, smsg.Cid())
	}

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)

	for _, id := range cids {
		err = kit.MirNodesWaitMsg(ctx, id, nodes[0])
		require.NoError(t, err)
	}
}

// TestMirBasic_MiningWithOneByzantineNode tests that sending messages mechanism operates normally
// in a presence of one byzantine node.
func TestMirBasic_MiningWithOneByzantineNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, twinNodes, miners, twinMiners, ens := kit.EnsembleMirNodesWithByzantineTwins(t, 4, mirTestOpts...)
	ens.Connect(nodes[0], nodes[3])
	ens.Connect(nodes[3], nodes[1])

	ens.Connect(nodes[1], twinNodes[0])
	ens.Connect(twinNodes[0], nodes[2])

	ens.BeginMirMining(ctx, g, append(miners, twinMiners...)...)

	err := kit.AdvanceChain(ctx, 10*TestedBlockNumber, append(nodes, twinNodes...)...)
	require.NoError(t, err)
	err = kit.CheckNodesInSync(ctx, 0, nodes[0], nodes[1:]...)
	require.NoError(t, err)
}

// TestMirWithMangler_AllNodesMiningWithMessaging runs TestMir_AllNodesMiningWithMessaging with mangler.
func TestMirWithMangler_AllNodesMiningWithMessaging(t *testing.T) {
	setupMangler(t)
	TestMirBasic_AllNodesMiningWithMessaging(t)
}

// TestMirBasic_WithFOmissionNodes tests that n − f nodes operate normally and the system can recover
// if f nodes do not have access to network at the same time.
func TestMirBasic_WithFOmissionNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners, &kit.MiningConfig{
		MembershipType:  kit.StringMembership,
		MockedTransport: true,
	})

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> disconnecting %d nodes", MirFaultyValidatorNumber)
	ens.DisconnectNodes(nodes[:MirFaultyValidatorNumber], nodes[MirFaultyValidatorNumber:])
	ens.DisconnectMirMiners(ctx, miners[:MirFaultyValidatorNumber])

	for _, node := range nodes[:MirFaultyValidatorNumber] {
		ps, err := node.NetPeers(ctx)
		require.NoError(t, err)
		require.Equal(t, 0, len(ps))
	}

	err = kit.NoProgressForFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> reconnecting %d nodes", MirFaultyValidatorNumber)
	ens.InterconnectFullNodes()
	ens.ConnectMirMiners(ctx, miners[:MirFaultyValidatorNumber])

	for i := 0; i < 15; i++ {
		time.Sleep(4 * time.Second)
		err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
		if err == nil {
			break
		}
	}
	require.NoError(t, err)
}

// TestMirBasic_WithFCrashedNodes tests that n − f nodes operate normally and can recover
// if f nodes crash at the same time.
func TestMirBasic_WithFCrashedNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d miners", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, 0, miners[:MirFaultyValidatorNumber]...)

	err = kit.NoProgressForFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d miners", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithState(ctx, miners[:MirFaultyValidatorNumber]...)

	for i := 0; i < 15; i++ {
		time.Sleep(4 * time.Second)
		err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
		if err == nil {
			break
		}
	}
	require.NoError(t, err)
}

// TestMirSmoke_StartStop tests that Mir nodes can be stopped.
func TestMirSmoke_StartStop(t *testing.T) {
	wait := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	g := errgroup.Group{}

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		after := time.NewTimer(10 * time.Second)
		cancel()
		select {
		case <-after.C:
			t.Fatalf("fail to stop Mir nodes")
		case <-wait:
		}
		after.Stop()
	}()

	go func() {
		// This goroutine is leaking after time.After(x) seconds with panicking.
		select {
		case <-time.After(20 * time.Second):
			panic("test time exceeded")
		case <-ctx.Done():
			return
		}
	}()

	go func() {
		// This goroutine is leaking after time.After(x) seconds with panicking.
		err := g.Wait()
		require.NoError(t, err)
		close(wait)
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, &g, miners...)

	err := kit.AdvanceChain(ctx, 10, nodes...)
	require.NoError(t, err)
}

// TestMirSmoke_StopWithError tests that the tests can be stopped if an error occurred during mining.
func TestMirSmoke_StopWithError(t *testing.T) {
	wait := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		after := time.NewTimer(10 * time.Second)
		cancel()
		select {
		case <-after.C:
			t.Fatalf("fail to stop Mir nodes")
		case <-wait:
		}
		after.Stop()
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
		err := g.Wait()
		require.NoError(t, err)
		close(wait)
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners, &kit.MiningConfig{
		MembershipType: kit.FakeMembership,
	})

	err := kit.AdvanceChain(ctx, 10, nodes...)
	require.Error(t, err)
}

// TestMirBasic_WithFCrashedAndRecoveredNodes tests that n − f nodes operate normally without significant interruption,
// and recovered nodes eventually operate normally
// if f nodes crash and then recover (with only initial state) after a long delay (few minutes).
func TestMirBasic_WithFCrashedAndRecoveredNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d miners", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, 0, miners[:MirFaultyValidatorNumber]...)

	err = kit.NoProgressForFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d miners from scratch", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithEmptyState(ctx, miners[:MirFaultyValidatorNumber]...)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Log(">>> checking nodes are in sync")
	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirBasic_FNodesCrashLongTimeApart tests that n − f nodes operate normally
// if f nodes crash, long time apart (few minutes).
func TestMirBasic_FNodesCrashLongTimeApart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMining(ctx, g, miners...)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> crash %d nodes", MirFaultyValidatorNumber)
	ens.CrashMirMiners(ctx, MaxDelay, miners[:MirFaultyValidatorNumber]...)

	err = kit.NoProgressForFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> restore %d nodes", MirFaultyValidatorNumber)
	ens.RestoreMirMinersWithState(ctx, miners[:MirFaultyValidatorNumber]...)

	err = kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
	require.NoError(t, err)
}

// TestMirBasic_FNodesHaveLongPeriodNoNetworkAccessButDoNotCrash tests that n − f nodes operate normally
// and partitioned nodes eventually catch up
// if f nodes have a long period of no network access, but do not crash.
func TestMirBasic_FNodesHaveLongPeriodNoNetworkAccessButDoNotCrash(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	defer func() {
		t.Logf("[*] defer: cancelling %s context", t.Name())
		cancel()
		err := g.Wait()
		require.NoError(t, err)
		t.Logf("[*] defer: system %s stopped", t.Name())
	}()

	nodes, miners, ens := kit.EnsembleMirNodes(t, MirTotalValidatorNumber, mirTestOpts...)
	ens.InterconnectFullNodes().BeginMirMiningWithConfig(ctx, g, miners,
		&kit.MiningConfig{
			MembershipType:  kit.StringMembership,
			MockedTransport: true,
		},
	)

	err := kit.AdvanceChain(ctx, TestedBlockNumber, nodes...)
	require.NoError(t, err)

	t.Logf(">>> disconnecting %d nodes", MirFaultyValidatorNumber)
	ens.DisconnectNodes(nodes[:MirFaultyValidatorNumber], nodes[MirFaultyValidatorNumber:])
	ens.DisconnectMirMiners(ctx, miners[:MirFaultyValidatorNumber])

	t.Logf(">>> delay")
	kit.RandomDelay(MaxDelay)

	err = kit.NoProgressForFaultyNodes(ctx, TestedBlockNumber, nodes[MirFaultyValidatorNumber:], nodes[:MirFaultyValidatorNumber]...)
	require.NoError(t, err)

	t.Logf(">>> reconnecting %d nodes", MirFaultyValidatorNumber)
	ens.InterconnectFullNodes()
	ens.ConnectMirMiners(ctx, miners[:MirFaultyValidatorNumber])

	for i := 0; i < 15; i++ {
		time.Sleep(4 * time.Second)
		err = kit.CheckNodesInSync(ctx, 0, nodes[MirReferenceSyncingNode], nodes...)
		if err == nil {
			break
		}
	}
	require.NoError(t, err)
}
