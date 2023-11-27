package store

import (
	"errors"
	"fmt"
	"github.com/go-kit/kit/metrics"
	"strconv"
	"time"

	"github.com/cosmos/gogoproto/proto"
	"github.com/google/orderedcode"

	cmterrors "github.com/cometbft/cometbft/types/errors"

	dbm "github.com/cometbft/cometbft-db"

	"github.com/cometbft/cometbft/internal/evidence"
	sm "github.com/cometbft/cometbft/internal/state"
	cmtsync "github.com/cometbft/cometbft/internal/sync"
	cmtstore "github.com/cometbft/cometbft/proto/tendermint/store"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/cometbft/cometbft/types"
)

/*
BlockStore is a simple low level store for blocks.

There are three types of information stored:
  - BlockMeta:   Meta information about each block
  - Block part:  Parts of each block, aggregated w/ PartSet
  - Commit:      The commit part of each block, for gossiping precommit votes

Currently the precommit signatures are duplicated in the Block parts as
well as the Commit.  In the future this may change, perhaps by moving
the Commit data outside the Block. (TODO)

The store can be assumed to contain all contiguous blocks between base and height (inclusive).

// NOTE: BlockStore methods will panic if they encounter errors
// deserializing loaded data, indicating probable corruption on disk.
*/
type BlockStore struct {
	db      dbm.DB
	metrics *Metrics

	// mtx guards access to the struct fields listed below it. We rely on the database to enforce
	// fine-grained concurrency control for its data, and thus this mutex does not apply to
	// database contents. The only reason for keeping these fields in the struct is that the data
	// can't efficiently be queried from the database since the key encoding we use is not
	// lexicographically ordered (see https://github.com/tendermint/tendermint/issues/4567).
	mtx    cmtsync.RWMutex
	base   int64
	height int64
}

type BlockStoreOptions struct {
	Metrics *Metrics
}

// NewBlockStore returns a new BlockStore with the given DB,
// initialized to the last height that was committed to the DB.
func NewBlockStore(db dbm.DB, o BlockStoreOptions) *BlockStore {
	bs := LoadBlockStoreState(db)
	m := NopMetrics()
	if o.Metrics != nil {
		m = o.Metrics
	}
	return &BlockStore{
		metrics: m,
		base:    bs.Base,
		height:  bs.Height,
		db:      db,
	}
}

func (bs *BlockStore) IsEmpty() bool {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	return bs.base == bs.height && bs.base == 0
}

// Base returns the first known contiguous block height, or 0 for empty block stores.
func (bs *BlockStore) Base() int64 {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	return bs.base
}

// Height returns the last known contiguous block height, or 0 for empty block stores.
func (bs *BlockStore) Height() int64 {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	return bs.height
}

// Size returns the number of blocks in the block store.
func (bs *BlockStore) Size() int64 {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	if bs.height == 0 {
		return 0
	}
	return bs.height - bs.base + 1
}

// LoadBase atomically loads the base block meta, or returns nil if no base is found.
func (bs *BlockStore) LoadBaseMeta() *types.BlockMeta {
	bs.mtx.RLock()
	defer bs.mtx.RUnlock()
	if bs.base == 0 {
		return nil
	}
	return bs.LoadBlockMeta(bs.base)
}

// LoadBlock returns the block with the given height.
// If no block is found for that height, it returns nil.
func (bs *BlockStore) LoadBlock(height int64) (*types.Block, *types.BlockMeta) {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block"))()
	blockMeta := bs.LoadBlockMeta(height)
	if blockMeta == nil {
		return nil, nil
	}

	pbb := new(cmtproto.Block)
	buf := []byte{}
	for i := 0; i < int(blockMeta.BlockID.PartSetHeader.Total); i++ {
		part := bs.LoadBlockPart(height, i)
		// If the part is missing (e.g. since it has been deleted after we
		// loaded the block meta) we consider the whole block to be missing.
		if part == nil {
			return nil, nil
		}
		buf = append(buf, part.Bytes...)
	}
	err := proto.Unmarshal(buf, pbb)
	if err != nil {
		// NOTE: The existence of meta should imply the existence of the
		// block. So, make sure meta is only saved after blocks are saved.
		panic(fmt.Sprintf("Error reading block: %v", err))
	}

	block, err := types.BlockFromProto(pbb)
	if err != nil {
		panic(cmterrors.ErrMsgFromProto{MessageName: "Block", Err: err})
	}

	return block, blockMeta
}

// LoadBlockByHash returns the block with the given hash.
// If no block is found for that hash, it returns nil.
// Panics if it fails to parse height associated with the given hash.
func (bs *BlockStore) LoadBlockByHash(hash []byte) (*types.Block, *types.BlockMeta) {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block_by_hash"))()
	bz, err := bs.db.Get(calcBlockHashKey(hash))
	if err != nil {
		panic(err)
	}
	if len(bz) == 0 {
		return nil, nil
	}

	s := string(bz)
	height, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("failed to extract height from %s: %v", s, err))
	}
	return bs.LoadBlock(height)
}

// LoadBlockPart returns the Part at the given index
// from the block at the given height.
// If no part is found for the given height and index, it returns nil.
func (bs *BlockStore) LoadBlockPart(height int64, index int) *types.Part {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block_part"))()
	pbpart := new(cmtproto.Part)

	bz, err := bs.db.Get(blockPartKey(height, index))
	if err != nil {
		panic(err)
	}
	if len(bz) == 0 {
		return nil
	}

	err = proto.Unmarshal(bz, pbpart)
	if err != nil {
		panic(fmt.Errorf("unmarshal to cmtproto.Part failed: %w", err))
	}
	part, err := types.PartFromProto(pbpart)
	if err != nil {
		panic(fmt.Sprintf("Error reading block part: %v", err))
	}

	return part
}

// LoadBlockMeta returns the BlockMeta for the given height.
// If no block is found for the given height, it returns nil.
func (bs *BlockStore) LoadBlockMeta(height int64) *types.BlockMeta {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block_meta"))()
	pbbm := new(cmtproto.BlockMeta)
	bz, err := bs.db.Get(blockMetaKey(height))

	if err != nil {
		panic(err)
	}

	if len(bz) == 0 {
		return nil
	}

	err = proto.Unmarshal(bz, pbbm)
	if err != nil {
		panic(fmt.Errorf("unmarshal to cmtproto.BlockMeta: %w", err))
	}

	blockMeta, err := types.BlockMetaFromProto(pbbm)
	if err != nil {
		panic(cmterrors.ErrMsgFromProto{MessageName: "BlockMetadata", Err: err})
	}

	return blockMeta
}

// LoadBlockMetaByHash returns the blockmeta who's header corresponds to the given
// hash. If none is found, returns nil.
func (bs *BlockStore) LoadBlockMetaByHash(hash []byte) *types.BlockMeta {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block_meta_by_hash"))()
	bz, err := bs.db.Get(blockHashKey(hash))
	if err != nil {
		panic(err)
	}
	if len(bz) == 0 {
		return nil
	}

	s := string(bz)
	height, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("failed to extract height from %s: %v", s, err))
	}
	return bs.LoadBlockMeta(height)
}

// LoadBlockCommit returns the Commit for the given height.
// This commit consists of the +2/3 and other Precommit-votes for block at `height`,
// and it comes from the block.LastCommit for `height+1`.
// If no commit is found for the given height, it returns nil.
func (bs *BlockStore) LoadBlockCommit(height int64) *types.Commit {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_block_commit"))()
	pbc := new(cmtproto.Commit)
	bz, err := bs.db.Get(blockCommitKey(height))
	if err != nil {
		panic(err)
	}
	if len(bz) == 0 {
		return nil
	}
	err = proto.Unmarshal(bz, pbc)
	if err != nil {
		panic(fmt.Errorf("error reading block commit: %w", err))
	}
	commit, err := types.CommitFromProto(pbc)
	if err != nil {
		panic(cmterrors.ErrMsgToProto{MessageName: "Commit", Err: err})
	}
	return commit
}

// LoadExtendedCommit returns the ExtendedCommit for the given height.
// The extended commit is not guaranteed to contain the same +2/3 precommits data
// as the commit in the block.
func (bs *BlockStore) LoadBlockExtendedCommit(height int64) *types.ExtendedCommit {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_seen_ext_commit"))()
	pbec := new(cmtproto.ExtendedCommit)
	bz, err := bs.db.Get(blockExtCommitKey(height))
	if err != nil {
		panic(fmt.Errorf("fetching extended commit: %w", err))
	}
	if len(bz) == 0 {
		return nil
	}
	err = proto.Unmarshal(bz, pbec)
	if err != nil {
		panic(fmt.Errorf("decoding extended commit: %w", err))
	}
	extCommit, err := types.ExtendedCommitFromProto(pbec)
	if err != nil {
		panic(fmt.Errorf("converting extended commit: %w", err))
	}
	return extCommit
}

// LoadSeenCommit returns the locally seen Commit for the given height.
// This is useful when we've seen a commit, but there has not yet been
// a new block at `height + 1` that includes this commit in its block.LastCommit.
func (bs *BlockStore) LoadSeenCommit(height int64) *types.Commit {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "load_seen_commit"))()
	pbc := new(cmtproto.Commit)
	bz, err := bs.db.Get(seenCommitKey(height))
	if err != nil {
		panic(err)
	}
	if len(bz) == 0 {
		return nil
	}
	err = proto.Unmarshal(bz, pbc)
	if err != nil {
		panic(fmt.Sprintf("error reading block seen commit: %v", err))
	}

	commit, err := types.CommitFromProto(pbc)
	if err != nil {
		panic(fmt.Errorf("converting seen commit: %w", err))
	}
	return commit
}

// PruneBlocks removes block up to (but not including) a height. It returns the
// number of blocks pruned and the evidence retain height - the height at which
// data needed to prove evidence must not be removed.
func (bs *BlockStore) PruneBlocks(height int64, state sm.State) (uint64, int64, error) {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "prune_blocks"))()
	if height <= 0 {
		return 0, -1, fmt.Errorf("height must be greater than 0")
	}
	bs.mtx.RLock()
	if height > bs.height {
		bs.mtx.RUnlock()
		return 0, -1, fmt.Errorf("cannot prune beyond the latest height %v", bs.height)
	}
	base := bs.base
	bs.mtx.RUnlock()
	if height < base {
		return 0, -1, fmt.Errorf("cannot prune to height %v, it is lower than base height %v",
			height, base)
	}

	pruned := uint64(0)
	batch := bs.db.NewBatch()
	defer batch.Close()
	flush := func(batch dbm.Batch, base int64) error {
		// We can't trust batches to be atomic, so update base first to make sure noone
		// tries to access missing blocks.
		bs.mtx.Lock()
		bs.base = base
		bs.mtx.Unlock()
		bs.saveState()

		err := batch.WriteSync()
		if err != nil {
			return fmt.Errorf("failed to prune up to height %v: %w", base, err)
		}
		batch.Close()
		return nil
	}

	evidencePoint := height
	for h := base; h < height; h++ {

		meta := bs.LoadBlockMeta(h)
		if meta == nil { // assume already deleted
			continue
		}

		// This logic is in place to protect data that proves malicious behavior.
		// If the height is within the evidence age, we continue to persist the header and commit data.

		if evidencePoint == height && !evidence.IsEvidenceExpired(state.LastBlockHeight, state.LastBlockTime, h, meta.Header.Time, state.ConsensusParams.Evidence) {
			evidencePoint = h
		}

		// if height is beyond the evidence point we dont delete the header
		if h < evidencePoint {
			if err := batch.Delete(blockMetaKey(h)); err != nil {
				return 0, -1, err
			}
		}
		if err := batch.Delete(blockHashKey(meta.BlockID.Hash)); err != nil {
			return 0, -1, err
		}
		// if height is beyond the evidence point we dont delete the commit data
		if h < evidencePoint {
			if err := batch.Delete(blockCommitKey(h)); err != nil {
				return 0, -1, err
			}
		}
		if err := batch.Delete(seenCommitKey(h)); err != nil {
			return 0, -1, err
		}
		for p := 0; p < int(meta.BlockID.PartSetHeader.Total); p++ {
			if err := batch.Delete(blockPartKey(h, p)); err != nil {
				return 0, -1, err
			}
		}
		pruned++

		// flush every 1000 blocks to avoid batches becoming too large
		if pruned%1000 == 0 && pruned > 0 {
			err := flush(batch, h)
			if err != nil {
				return 0, -1, err
			}
			batch = bs.db.NewBatch()
			defer batch.Close()
		}
	}

	err := flush(batch, height)
	if err != nil {
		return 0, -1, err
	}
	return pruned, evidencePoint, nil
}

// SaveBlock persists the given block, blockParts, and seenCommit to the underlying db.
// blockParts: Must be parts of the block
// seenCommit: The +2/3 precommits that were seen which committed at height.
//
//	If all the nodes restart after committing a block,
//	we need this to reload the precommits to catch-up nodes to the
//	most recent height.  Otherwise they'd stall at H-1.
func (bs *BlockStore) SaveBlock(block *types.Block, blockParts *types.PartSet, seenCommit *types.Commit) {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "save_block"))()
	if block == nil {
		panic("BlockStore can only save a non-nil block")
	}
	if err := bs.saveBlockToBatch(block, blockParts, seenCommit); err != nil {
		panic(err)
	}

	// Save new BlockStoreState descriptor. This also flushes the database.
	bs.saveState()
}

// SaveBlockWithExtendedCommit persists the given block, blockParts, and
// seenExtendedCommit to the underlying db. seenExtendedCommit is stored under
// two keys in the database: as the seenCommit and as the ExtendedCommit data for the
// height. This allows the vote extension data to be persisted for all blocks
// that are saved.
func (bs *BlockStore) SaveBlockWithExtendedCommit(block *types.Block, blockParts *types.PartSet, seenExtendedCommit *types.ExtendedCommit) {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "save_seen_ext_commit"))()
	if block == nil {
		panic("BlockStore can only save a non-nil block")
	}
	if err := seenExtendedCommit.EnsureExtensions(true); err != nil {
		panic(fmt.Errorf("problems saving block with extensions: %w", err))
	}
	if err := bs.saveBlockToBatch(block, blockParts, seenExtendedCommit.ToCommit()); err != nil {
		panic(err)
	}
	height := block.Height

	pbec := seenExtendedCommit.ToProto()
	extCommitBytes := mustEncode(pbec)
	if err := bs.db.Set(blockExtCommitKey(height), extCommitBytes); err != nil {
		panic(err)
	}

	// Save new BlockStoreState descriptor. This also flushes the database.
	bs.saveState()
}

func (bs *BlockStore) saveBlockToBatch(block *types.Block, blockParts *types.PartSet, seenCommit *types.Commit) error {
	defer addTimeSample(bs.metrics.BlockStoreAccessDurationSeconds.With("method", "save_seen_commit"))()
	if block == nil {
		panic("BlockStore can only save a non-nil block")
	}

	height := block.Height
	hash := block.Hash()

	if g, w := height, bs.Height()+1; bs.Base() > 0 && g != w {
		return fmt.Errorf("BlockStore can only save contiguous blocks. Wanted %v, got %v", w, g)
	}
	if !blockParts.IsComplete() {
		return errors.New("BlockStore can only save complete block part sets")
	}
	if height != seenCommit.Height {
		return fmt.Errorf("BlockStore cannot save seen commit of a different height (block: %d, commit: %d)", height, seenCommit.Height)
	}

	// Save block parts. This must be done before the block meta, since callers
	// typically load the block meta first as an indication that the block exists
	// and then go on to load block parts - we must make sure the block is
	// complete as soon as the block meta is written.
	for i := 0; i < int(blockParts.Total()); i++ {
		part := blockParts.GetPart(i)
		bs.saveBlockPart(height, i, part)
	}

	// Save block meta
	blockMeta := types.NewBlockMeta(block, blockParts)
	pbm := blockMeta.ToProto()
	if pbm == nil {
		return errors.New("nil blockmeta")
	}
	metaBytes := mustEncode(pbm)
	if err := bs.db.Set(blockMetaKey(height), metaBytes); err != nil {
		return err
	}
	if err := bs.db.Set(blockHashKey(hash), []byte(fmt.Sprintf("%d", height))); err != nil {
		return err
	}

	// Save block commit (duplicate and separate from the Block)
	pbc := block.LastCommit.ToProto()
	blockCommitBytes := mustEncode(pbc)
	if err := bs.db.Set(blockCommitKey(height-1), blockCommitBytes); err != nil {
		return err
	}

	// Save seen commit (seen +2/3 precommits for block)
	// NOTE: we can delete this at a later height
	pbsc := seenCommit.ToProto()
	seenCommitBytes := mustEncode(pbsc)
	if err := bs.db.Set(seenCommitKey(height), seenCommitBytes); err != nil {
		return err
	}

	// Done!
	bs.mtx.Lock()
	bs.height = height
	if bs.base == 0 {
		bs.base = height
	}
	bs.mtx.Unlock()

	return nil
}

func (bs *BlockStore) saveBlockPart(height int64, index int, part *types.Part) {
	pbp, err := part.ToProto()
	if err != nil {
		panic(cmterrors.ErrMsgToProto{MessageName: "Part", Err: err})
	}
	partBytes := mustEncode(pbp)
	if err := bs.db.Set(blockPartKey(height, index), partBytes); err != nil {
		panic(err)
	}
}

func (bs *BlockStore) saveState() {
	bs.mtx.RLock()
	bss := cmtstore.BlockStoreState{
		Base:   bs.base,
		Height: bs.height,
	}
	bs.mtx.RUnlock()
	SaveBlockStoreState(&bss, bs.db)
}

// SaveSeenCommit saves a seen commit, used by e.g. the state sync reactor when bootstrapping node.
func (bs *BlockStore) SaveSeenCommit(height int64, seenCommit *types.Commit) error {
	pbc := seenCommit.ToProto()
	seenCommitBytes, err := proto.Marshal(pbc)
	if err != nil {
		return fmt.Errorf("unable to marshal commit: %w", err)
	}
	return bs.db.Set(seenCommitKey(height), seenCommitBytes)
}

func (bs *BlockStore) Close() error {
	return bs.db.Close()
}

//-----------------------------------------------------------------------------
//---------------------------------- KEY ENCODING -----------------------------------------

// key prefixes
const (
	// prefixes are unique across all tm db's
	prefixBlockMeta      = int64(0)
	prefixBlockPart      = int64(1)
	prefixBlockCommit    = int64(2)
	prefixExtBlockCommit = int64(3)
	prefixSeenCommit     = int64(4)
	prefixBlockHash      = int64(5)
)

func blockMetaKey(height int64) []byte {
	key, err := orderedcode.Append(nil, prefixBlockMeta, height)
	if err != nil {
		panic(err)
	}
	return key
}

func blockPartKey(height int64, partIndex int) []byte {
	key, err := orderedcode.Append(nil, prefixBlockPart, height, int64(partIndex))
	if err != nil {
		panic(err)
	}
	return key
}

func blockCommitKey(height int64) []byte {
	key, err := orderedcode.Append(nil, prefixBlockCommit, height)
	if err != nil {
		panic(err)
	}
	return key
}

func blockExtCommitKey(height int64) []byte {
	key, err := orderedcode.Append(nil, prefixExtBlockCommit, height)
	if err != nil {
		panic(err)
	}
	return key
}

func seenCommitKey(height int64) []byte {
	key, err := orderedcode.Append(nil, prefixSeenCommit, height)
	if err != nil {
		panic(err)
	}
	return key
}

func blockHashKey(hash []byte) []byte {
	key, err := orderedcode.Append(nil, prefixBlockHash, string(hash))
	if err != nil {
		panic(err)
	}
	return key
}

//-----------------------------------------------------------------------------

var blockStoreKey = []byte("blockStore")

// SaveBlockStoreState persists the blockStore state to the database.
func SaveBlockStoreState(bsj *cmtstore.BlockStoreState, db dbm.DB) {
	bytes, err := proto.Marshal(bsj)
	if err != nil {
		panic(fmt.Sprintf("Could not marshal state bytes: %v", err))
	}
	if err := db.SetSync(blockStoreKey, bytes); err != nil {
		panic(err)
	}
}

// LoadBlockStoreState returns the BlockStoreState as loaded from disk.
// If no BlockStoreState was previously persisted, it returns the zero value.
func LoadBlockStoreState(db dbm.DB) cmtstore.BlockStoreState {
	bytes, err := db.Get(blockStoreKey)
	if err != nil {
		panic(err)
	}

	if len(bytes) == 0 {
		return cmtstore.BlockStoreState{
			Base:   0,
			Height: 0,
		}
	}

	var bsj cmtstore.BlockStoreState
	if err := proto.Unmarshal(bytes, &bsj); err != nil {
		panic(fmt.Sprintf("Could not unmarshal bytes: %X", bytes))
	}

	// Backwards compatibility with persisted data from before Base existed.
	if bsj.Height > 0 && bsj.Base == 0 {
		bsj.Base = 1
	}
	return bsj
}

// mustEncode proto encodes a proto.message and panics if fails
func mustEncode(pb proto.Message) []byte {
	bz, err := proto.Marshal(pb)
	if err != nil {
		panic(fmt.Errorf("unable to marshal: %w", err))
	}
	return bz
}

//-----------------------------------------------------------------------------

// DeleteLatestBlock removes the block pointed to by height,
// lowering height by one.
func (bs *BlockStore) DeleteLatestBlock() error {
	bs.mtx.RLock()
	targetHeight := bs.height
	bs.mtx.RUnlock()

	batch := bs.db.NewBatch()
	defer batch.Close()

	// delete what we can, skipping what's already missing, to ensure partial
	// blocks get deleted fully.
	if meta := bs.LoadBlockMeta(targetHeight); meta != nil {
		if err := batch.Delete(blockHashKey(meta.BlockID.Hash)); err != nil {
			return err
		}
		for p := 0; p < int(meta.BlockID.PartSetHeader.Total); p++ {
			if err := batch.Delete(blockPartKey(targetHeight, p)); err != nil {
				return err
			}
		}
	}
	if err := batch.Delete(blockCommitKey(targetHeight)); err != nil {
		return err
	}
	if err := batch.Delete(seenCommitKey(targetHeight)); err != nil {
		return err
	}
	// delete last, so as to not leave keys built on meta.BlockID dangling
	if err := batch.Delete(blockMetaKey(targetHeight)); err != nil {
		return err
	}

	bs.mtx.Lock()
	bs.height = targetHeight - 1
	bs.mtx.Unlock()
	bs.saveState()

	err := batch.WriteSync()
	if err != nil {
		return fmt.Errorf("failed to delete height %v: %w", targetHeight, err)
	}
	return nil
}

func addTimeSample(h metrics.Histogram) func() {
	start := time.Now()
	return func() {
		h.Observe(time.Since(start).Seconds())
	}
}