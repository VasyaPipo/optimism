package actions

import (
	"math/rand"
	"path"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils"
	"github.com/ethereum-optimism/optimism/op-node/client"
	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum-optimism/optimism/op-node/sources"
	"github.com/ethereum-optimism/optimism/op-node/testlog"
)

func setupReorgTest(t Testing) (*e2eutils.SetupData, *L1Miner, *L2Sequencer, *L2Engine, *L2Verifier, *L2Engine, *L2Batcher) {
	dp := e2eutils.MakeDeployParams(t, defaultRollupTestParams)
	sd := e2eutils.Setup(t, dp, defaultAlloc)
	log := testlog.Logger(t, log.LvlDebug)
	return setupReorgTestActors(t, dp, sd, log)
}

func setupReorgTestActors(t Testing, dp *e2eutils.DeployParams, sd *e2eutils.SetupData, log log.Logger) (*e2eutils.SetupData, *L1Miner, *L2Sequencer, *L2Engine, *L2Verifier, *L2Engine, *L2Batcher) {
	miner, seqEngine, sequencer := setupSequencerTest(t, sd, log)
	miner.ActL1SetFeeRecipient(common.Address{'A'})
	sequencer.ActL2PipelineFull(t)
	verifEngine, verifier := setupVerifier(t, sd, log, miner.L1Client(t, sd.RollupCfg))
	rollupSeqCl := sequencer.RollupClient()
	batcher := NewL2Batcher(log, sd.RollupCfg, &BatcherCfg{
		MinL1TxSize: 0,
		MaxL1TxSize: 128_000,
		BatcherKey:  dp.Secrets.Batcher,
	}, rollupSeqCl, miner.EthClient(), seqEngine.EthClient())
	return sd, miner, sequencer, seqEngine, verifier, verifEngine, batcher
}

func TestReorgOrphanBlock(gt *testing.T) {
	t := NewDefaultTesting(gt)
	sd, miner, sequencer, _, verifier, verifierEng, batcher := setupReorgTest(t)
	verifEngClient := verifierEng.EngineClient(t, sd.RollupCfg)

	sequencer.ActL2PipelineFull(t)
	verifier.ActL2PipelineFull(t)

	// build empty L1 block
	miner.ActEmptyBlock(t)

	// Create L2 blocks, and reference the L1 head as origin
	sequencer.ActL1HeadSignal(t)
	sequencer.ActBuildToL1Head(t)

	// submit all new L2 blocks
	batcher.ActSubmitAll(t)

	// new L1 block with L2 batch
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	batchTx := miner.l1Transactions[0]
	miner.ActL1EndBlock(t)

	// verifier picks up the L2 chain that was submitted
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Unsafe(), "verifier syncs from sequencer via L1")
	require.NotEqual(t, sequencer.L2Safe(), sequencer.L2Unsafe(), "sequencer has not processed L1 yet")

	// orphan the L1 block that included the batch tx, and build a new different L1 block
	miner.ActL1RewindToParent(t)
	miner.ActL1SetFeeRecipient(common.Address{'B'})
	miner.ActEmptyBlock(t)
	miner.ActEmptyBlock(t) // needs to be a longer chain for reorg to be applied. TODO: maybe more aggressively react to reorgs to shorter chains?

	// sync verifier again. The L1 reorg excluded the batch, so now the previous L2 chain should be unsafe again.
	// However, the L2 chain can still be canonical later, since it did not reference the reorged L1 block
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Safe(), "verifier rewinds safe when L1 reorgs out batch")
	ref, err := verifEngClient.L2BlockRefByLabel(t.Ctx(), eth.Safe)
	require.NoError(t, err)
	require.Equal(t, verifier.L2Safe(), ref, "verifier engine matches rollup client")

	// Now replay the batch tx in a new L1 block
	miner.ActL1StartBlock(12)(t)
	miner.ActL1SetFeeRecipient(common.Address{'C'})
	// note: the geth tx pool reorgLoop is too slow (responds to chain head events, but async),
	// and there's no way to manually trigger runReorg, so we re-insert it ourselves.
	require.NoError(t, miner.eth.TxPool().AddLocal(batchTx))
	// need to re-insert previously included tx into the block
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// sync the verifier again: now it should be safe again
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Unsafe(), "verifier syncs from sequencer via replayed batch on L1")
	ref, err = verifEngClient.L2BlockRefByLabel(t.Ctx(), eth.Safe)
	require.NoError(t, err)
	require.Equal(t, verifier.L2Safe(), ref, "verifier engine matches rollup client")

	sequencer.ActL1HeadSignal(t)
	sequencer.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Safe(), "verifier and sequencer see same safe L2 block, while only verifier dealt with the orphan and replay")
}

func TestReorgFlipFlop(gt *testing.T) {
	t := NewDefaultTesting(gt)
	sd, miner, sequencer, _, verifier, verifierEng, batcher := setupReorgTest(t)
	minerCl := miner.L1Client(t, sd.RollupCfg)
	verifEngClient := verifierEng.EngineClient(t, sd.RollupCfg)
	checkVerifEngine := func() {
		// TODO: geth preserves L2 chain with origin A1 after flip-flopping to B?
		//ref, err := verifEngClient.L2BlockRefByLabel(t.Ctx(), eth.Unsafe)
		//require.NoError(t, err)
		//t.Logf("l2 unsafe head %s with origin %s", ref, ref.L1Origin)
		//require.NotEqual(t, verifier.L2Unsafe().Hash, ref.ParentHash, "TODO off by one, engine syncs A0 after reorging back from B, while rollup node only inserts up to A0 (excl.)")
		//require.Equal(t, verifier.L2Unsafe(), ref, "verifier safe head of engine matches rollup client")

		ref, err := verifEngClient.L2BlockRefByLabel(t.Ctx(), eth.Safe)
		require.NoError(t, err)
		require.Equal(t, verifier.L2Safe(), ref, "verifier safe head of engine matches rollup client")
	}

	sequencer.ActL2PipelineFull(t)
	verifier.ActL2PipelineFull(t)

	// Start building chain A
	miner.ActL1SetFeeRecipient(common.Address{'A', 0})
	miner.ActEmptyBlock(t)
	blockA0, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)

	// Create L2 blocks, and reference the L1 head A0 as origin
	sequencer.ActL1HeadSignal(t)
	sequencer.ActBuildToL1Head(t)

	// submit all new L2 blocks
	batcher.ActSubmitAll(t)

	// new L1 block A1 with L2 batch
	miner.ActL1SetFeeRecipient(common.Address{'A', 1})
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	batchTxA := miner.l1Transactions[0]
	miner.ActL1EndBlock(t)

	// verifier picks up the L2 chain that was submitted
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe().L1Origin, blockA0.ID(), "verifier syncs L2 chain with L1 A0 origin")
	checkVerifEngine()

	// Flip to chain B!
	miner.ActL1RewindToParent(t) // undo A1
	miner.ActL1RewindToParent(t) // undo A0
	// build B0
	miner.ActL1SetFeeRecipient(common.Address{'B', 0})
	miner.ActEmptyBlock(t)
	blockB0, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)
	require.Equal(t, blockA0.Number, blockB0.Number, "same height")
	require.NotEqual(t, blockA0.Hash, blockB0.Hash, "different content")

	// re-include the batch tx that submitted L2 chain data that pointed to A0, in the new block B1
	miner.ActL1SetFeeRecipient(common.Address{'B', 1})
	miner.ActL1StartBlock(12)(t)
	require.NoError(t, miner.eth.TxPool().AddLocal(batchTxA))
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// make B2, the reorg is picked up when we have a new longer chain
	miner.ActL1SetFeeRecipient(common.Address{'B', 2})
	miner.ActEmptyBlock(t)
	blockB2, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)

	// now sync the verifier: some of the batches should be ignored:
	//	The safe head should have a genesis L1 origin, but past genesis, as some L2 blocks were built to get to A0 time
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, sd.RollupCfg.Genesis.L1, verifier.L2Safe().L1Origin, "expected to be back at genesis origin after losing A0 and A1")

	require.NotZero(t, verifier.L2Safe().Number, "still preserving old L2 blocks that did not reference reorged L1 chain (assuming more than one L2 block per L1 block)")
	require.Equal(t, verifier.L2Safe(), verifier.L2Unsafe(), "head is at safe block after L1 reorg")
	checkVerifEngine()

	// and sync the sequencer, then build some new L2 blocks, up to and including with L1 origin B2
	sequencer.ActL1HeadSignal(t)
	sequencer.ActL2PipelineFull(t)
	sequencer.ActBuildToL1Head(t)
	require.Equal(t, sequencer.L2Unsafe().L1Origin, blockB2.ID(), "B2 is the unsafe L1 origin of sequencer now")

	// submit all new L2 blocks for chain B, and include in new block B3
	batcher.ActSubmitAll(t)
	miner.ActL1SetFeeRecipient(common.Address{'B', 3})
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// sync the verifier to the L2 chain with origin B2
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe().L1Origin, blockB2.ID(), "B2 is the L1 origin of verifier now")
	checkVerifEngine()

	// Flop back to chain A!
	miner.ActL1RewindDepth(4)(t) // B3, B2, B1, B0
	pivotBlock, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)
	require.Equal(t, sd.RollupCfg.Genesis.L1, pivotBlock.ID(), "back at L1 genesis")
	miner.ActL1SetFeeRecipient(common.Address{'A', 0})
	miner.ActEmptyBlock(t)
	blockA0Again, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)
	require.Equal(t, blockA0, blockA0Again, "block A0 is back again after flip-flop")

	// Continue to build on A, and replay the old A-chain batch transaction (we can't replay B, since the nonce values would conflict)
	miner.ActL1SetFeeRecipient(common.Address{'A', 1})
	miner.ActEmptyBlock(t)

	miner.ActL1SetFeeRecipient(common.Address{'A', 2})
	miner.ActL1StartBlock(12)(t)
	require.NoError(t, miner.eth.TxPool().AddLocal(batchTxA)) // replay chain A batches, but now in A2 instead of A1
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// build more L1 blocks, so the chain is long enough for reorg to be picked up
	miner.ActL1SetFeeRecipient(common.Address{'A', 3})
	miner.ActEmptyBlock(t)
	miner.ActL1SetFeeRecipient(common.Address{'A', 4})
	miner.ActEmptyBlock(t)
	blockA4, err := minerCl.L1BlockRefByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)

	// sync verifier, and see if A0 is safe, using the old replayed batch for chain A
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe().L1Origin, blockA0.ID(), "B2 is the L1 origin of verifier now")
	checkVerifEngine()

	// sync sequencer to the replayed L1 chain A
	sequencer.ActL1HeadSignal(t)
	sequencer.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Safe(), "sequencer reorgs to match verifier again")

	// and adopt the rest of L1 chain A into L2
	sequencer.ActBuildToL1Head(t)

	// submit the new unsafe A blocks
	batcher.ActSubmitAll(t)
	miner.ActL1SetFeeRecipient(common.Address{'A', 5})
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// sync verifier to what ths sequencer submitted
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Safe(), sequencer.L2Unsafe(), "verifier syncs from sequencer")
	require.Equal(t, verifier.L2Safe().L1Origin, blockA4.ID(), "L2 chain origin is A4")
	checkVerifEngine()
}

type rpcWrapper struct {
	client.RPC
}

// TestRestartOpGeth tests that the sequencer can restart its execution engine without rollup-node restart,
// including recovering the finalized/safe state of L2 chain without reorging.
func TestRestartOpGeth(gt *testing.T) {
	t := NewDefaultTesting(gt)
	dbPath := path.Join(t.TempDir(), "testdb")
	dbOption := func(_ *ethconfig.Config, nodeCfg *node.Config) error {
		nodeCfg.DataDir = dbPath
		return nil
	}
	dp := e2eutils.MakeDeployParams(t, defaultRollupTestParams)
	sd := e2eutils.Setup(t, dp, defaultAlloc)
	log := testlog.Logger(t, log.LvlDebug)
	jwtPath := e2eutils.WriteDefaultJWT(t)
	// L1
	miner := NewL1Miner(t, log, sd.L1Cfg)
	l1F, err := sources.NewL1Client(miner.RPCClient(), log, nil, sources.L1ClientDefaultConfig(sd.RollupCfg, false, sources.RPCKindBasic))
	require.NoError(t, err)
	// Sequencer
	seqEng := NewL2Engine(t, log, sd.L2Cfg, sd.RollupCfg.Genesis.L1, jwtPath, dbOption)
	engRpc := &rpcWrapper{seqEng.RPCClient()}
	l2Cl, err := sources.NewEngineClient(engRpc, log, nil, sources.EngineClientDefaultConfig(sd.RollupCfg))
	require.NoError(t, err)
	sequencer := NewL2Sequencer(t, log, l1F, l2Cl, sd.RollupCfg, 0)

	batcher := NewL2Batcher(log, sd.RollupCfg, &BatcherCfg{
		MinL1TxSize: 0,
		MaxL1TxSize: 128_000,
		BatcherKey:  dp.Secrets.Batcher,
	}, sequencer.RollupClient(), miner.EthClient(), seqEng.EthClient())

	// start
	sequencer.ActL2PipelineFull(t)

	miner.ActEmptyBlock(t)

	buildAndSubmit := func() {
		// build some blocks
		sequencer.ActL1HeadSignal(t)
		sequencer.ActBuildToL1Head(t)
		// submit the blocks, confirm on L1
		batcher.ActSubmitAll(t)
		miner.ActL1StartBlock(12)(t)
		miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
		miner.ActL1EndBlock(t)
		sequencer.ActL2PipelineFull(t)
	}
	buildAndSubmit()

	// finalize the L1 data (first block, and the new block with batch)
	miner.ActL1SafeNext(t)
	miner.ActL1SafeNext(t)
	miner.ActL1FinalizeNext(t)
	miner.ActL1FinalizeNext(t)
	sequencer.ActL1FinalizedSignal(t)
	sequencer.ActL1SafeSignal(t)

	// build and submit more
	buildAndSubmit()
	// but only mark the L1 block with this batch as safe
	miner.ActL1SafeNext(t)
	sequencer.ActL1SafeSignal(t)

	// build some more, these stay unsafe
	miner.ActEmptyBlock(t)
	sequencer.ActL1HeadSignal(t)
	sequencer.ActBuildToL1Head(t)

	statusBeforeRestart := sequencer.SyncStatus()
	// before restart scenario: we have a distinct finalized, safe, and unsafe part of the L2 chain
	require.NotZero(t, statusBeforeRestart.FinalizedL2.L1Origin.Number)
	require.Less(t, statusBeforeRestart.FinalizedL2.L1Origin.Number, statusBeforeRestart.SafeL2.L1Origin.Number)
	require.Less(t, statusBeforeRestart.SafeL2.L1Origin.Number, statusBeforeRestart.UnsafeL2.L1Origin.Number)

	// close the sequencer engine
	require.NoError(t, seqEng.Close())
	// and start a new one with same db path
	seqEngNew := NewL2Engine(t, log, sd.L2Cfg, sd.RollupCfg.Genesis.L1, jwtPath, dbOption)
	// swap in the new rpc. This is as close as we can get to reconnecting to a new in-memory rpc connection
	engRpc.RPC = seqEngNew.RPCClient()

	// note: geth does not persist the safe block label, only the finalized block label
	safe, err := l2Cl.L2BlockRefByLabel(t.Ctx(), eth.Safe)
	require.NoError(t, err)
	finalized, err := l2Cl.L2BlockRefByLabel(t.Ctx(), eth.Finalized)
	require.NoError(t, err)
	require.Equal(t, statusBeforeRestart.FinalizedL2, safe, "expecting to revert safe head to finalized head upon restart")
	require.Equal(t, statusBeforeRestart.FinalizedL2, finalized, "expecting to keep same finalized head upon restart")

	// sequencer runs pipeline, but now attached to the restarted geth node
	sequencer.ActL2PipelineFull(t)
	require.Equal(t, statusBeforeRestart.UnsafeL2, sequencer.L2Unsafe(), "expecting to keep same unsafe head upon restart")
	require.Equal(t, statusBeforeRestart.SafeL2, sequencer.L2Safe(), "expecting the safe block to catch up to what it was before shutdown after syncing from L1, and not be stuck at the finalized block")
}

// TestConflictingL2Blocks tests that a second copy of the sequencer stack cannot introduce an alternative
// L2 block (compared to something already secured by the first sequencer):
// the alt block is not synced by the verifier, in unsafe and safe sync modes.
func TestConflictingL2Blocks(gt *testing.T) {
	t := NewDefaultTesting(gt)
	dp := e2eutils.MakeDeployParams(t, defaultRollupTestParams)
	sd := e2eutils.Setup(t, dp, defaultAlloc)
	log := testlog.Logger(t, log.LvlDebug)

	sd, miner, sequencer, seqEng, verifier, _, batcher := setupReorgTestActors(t, dp, sd, log)

	// Extra setup: a full alternative sequencer, sequencer engine, and batcher
	jwtPath := e2eutils.WriteDefaultJWT(t)
	altSeqEng := NewL2Engine(t, log, sd.L2Cfg, sd.RollupCfg.Genesis.L1, jwtPath)
	altSeqEngCl, err := sources.NewEngineClient(altSeqEng.RPCClient(), log, nil, sources.EngineClientDefaultConfig(sd.RollupCfg))
	require.NoError(t, err)
	l1F, err := sources.NewL1Client(miner.RPCClient(), log, nil, sources.L1ClientDefaultConfig(sd.RollupCfg, false, sources.RPCKindBasic))
	require.NoError(t, err)
	altSequencer := NewL2Sequencer(t, log, l1F, altSeqEngCl, sd.RollupCfg, 0)
	altBatcher := NewL2Batcher(log, sd.RollupCfg, &BatcherCfg{
		MinL1TxSize: 0,
		MaxL1TxSize: 128_000,
		BatcherKey:  dp.Secrets.Batcher,
	}, altSequencer.RollupClient(), miner.EthClient(), altSeqEng.EthClient())

	// And set up user Alice, using the alternative sequencer endpoint
	l2Cl := altSeqEng.EthClient()
	addresses := e2eutils.CollectAddresses(sd, dp)
	l2UserEnv := &BasicUserEnv[*L2Bindings]{
		EthCl:          l2Cl,
		Signer:         types.LatestSigner(sd.L2Cfg.Config),
		AddressCorpora: addresses,
		Bindings:       NewL2Bindings(t, l2Cl, altSeqEng.GethClient()),
	}
	alice := NewCrossLayerUser(log, dp.Secrets.Alice, rand.New(rand.NewSource(1234)))
	alice.L2.SetUserEnv(l2UserEnv)

	sequencer.ActL2PipelineFull(t)
	verifier.ActL2PipelineFull(t)
	altSequencer.ActL2PipelineFull(t)

	// build empty L1 block
	miner.ActEmptyBlock(t)

	// Create L2 blocks, and reference the L1 head as origin
	sequencer.ActL1HeadSignal(t)
	sequencer.ActBuildToL1Head(t)

	// submit all new L2 blocks
	batcher.ActSubmitAll(t)

	// new L1 block with L2 batch
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)

	// verifier picks up the L2 chain that was submitted
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	verifierHead := verifier.L2Unsafe()
	require.Equal(t, verifier.L2Safe(), sequencer.L2Unsafe(), "verifier syncs from sequencer via L1")
	require.Equal(t, verifier.L2Safe(), verifierHead, "verifier head is the same as that what was derived from L1")
	require.NotEqual(t, sequencer.L2Safe(), sequencer.L2Unsafe(), "sequencer has not processed L1 yet")

	require.Less(t, altSequencer.L2Unsafe().L1Origin.Number, sequencer.L2Unsafe().L1Origin.Number, "alt-sequencer is behind")

	// produce a conflicting L2 block with the alt sequencer:
	// a new unsafe block that should not replace the existing safe block at the same height
	altSequencer.ActL2StartBlock(t)
	// include tx to force the L2 block to really be different than the previous empty block
	alice.L2.ActResetTxOpts(t)
	alice.L2.ActSetTxToAddr(&dp.Addresses.Bob)(t)
	alice.L2.ActMakeTx(t)
	altSeqEng.ActL2IncludeTx(alice.Address())(t)
	altSequencer.ActL2EndBlock(t)

	conflictBlock := seqEng.l2Chain.GetBlockByNumber(altSequencer.L2Unsafe().Number)
	require.NotEqual(t, conflictBlock.Hash(), altSequencer.L2Unsafe().Hash, "alt sequencer has built a conflicting block")

	// give the unsafe block to the verifier, and see if it reorgs because of any unsafe inputs
	head, err := altSeqEngCl.PayloadByLabel(t.Ctx(), eth.Unsafe)
	require.NoError(t, err)
	verifier.ActL2UnsafeGossipReceive(head)

	// make sure verifier has processed everything
	verifier.ActL2PipelineFull(t)

	// check if verifier is still following safe chain
	require.Equal(t, verifier.L2Unsafe(), verifierHead, "verifier must not accept the unsafe payload that orphans the safe payload")

	// now submit it to L1, and see if the verifier respects the inclusion order and preserves the original block
	altBatcher.ActSubmitAll(t)
	// include it in L1
	miner.ActL1StartBlock(12)(t)
	miner.ActL1IncludeTx(sd.RollupCfg.Genesis.SystemConfig.BatcherAddr)(t)
	miner.ActL1EndBlock(t)
	l1Number := miner.l1Chain.CurrentHeader().Number.Uint64()

	// show latest L1 block with new batch data to verifier, and make it sync.
	verifier.ActL1HeadSignal(t)
	verifier.ActL2PipelineFull(t)
	require.Equal(t, verifier.SyncStatus().CurrentL1.Number, l1Number, "verifier has synced all new L1 blocks")
	require.Equal(t, verifier.L2Unsafe(), verifierHead, "verifier sticks to first included L2 block")

	// Now make the alt sequencer aware of the L1 chain and derive the L2 chain like the verifier;
	// it should reorg out its conflicting blocks to get back in harmony with the verifier.
	altSequencer.ActL1HeadSignal(t)
	altSequencer.ActL2PipelineFull(t)
	require.Equal(t, verifier.L2Unsafe(), altSequencer.L2Unsafe(), "alt-sequencer gets back in harmony with verifier by reorging out its conflicting data")
	require.Equal(t, sequencer.L2Unsafe(), altSequencer.L2Unsafe(), "and gets back in harmony with original sequencer")
}
