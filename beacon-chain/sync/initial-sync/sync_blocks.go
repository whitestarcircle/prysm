package initialsync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/p2p"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

// processBlock is the main method that validates each block which is received
// for initial sync. It checks if the blocks are valid and then will continue to
// process and save it into the db.
func (s *InitialSync) processBlock(ctx context.Context, block *pb.BeaconBlock) {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.sync.initial-sync.processBlock")
	defer span.End()
	recBlock.Inc()

	if block.Slot == s.highestObservedSlot {
		s.currentSlot = s.highestObservedSlot
		if err := s.exitInitialSync(s.ctx, block); err != nil {
			log.Errorf("Could not exit initial sync: %v", err)
			return
		}
		return
	}

	if block.Slot < s.currentSlot {
		return
	}

	if err := s.validateAndSaveNextBlock(ctx, block); err != nil {
		// Debug error so as not to have noisy error logs
		if strings.HasPrefix(err.Error(), debugError) {
			log.Debug(strings.TrimPrefix(err.Error(), debugError))
			return
		}
		log.Errorf("Unable to save block: %v", err)
	}
}

// processBatchedBlocks processes all the received blocks from
// the p2p message.
func (s *InitialSync) processBatchedBlocks(msg p2p.Message) {
	ctx, span := trace.StartSpan(msg.Ctx, "beacon-chain.sync.initial-sync.processBatchedBlocks")
	defer span.End()
	batchedBlockReq.Inc()

	response := msg.Data.(*pb.BatchedBeaconBlockResponse)
	batchedBlocks := response.BatchedBlocks
	if len(batchedBlocks) == 0 {
		// Do not process empty responses.
		return
	}
	if msg.Peer != s.bestPeer {
		// Only process batch block responses that come from the best peer
		// we originally synced with.
		log.WithField("peerID", msg.Peer.Pretty()).Debug("Received batch blocks from a different peer")
		return
	}

	log.Debug("Processing batched block response")
	// Sort batchBlocks in ascending order.
	sort.Slice(batchedBlocks, func(i, j int) bool {
		return batchedBlocks[i].Slot < batchedBlocks[j].Slot
	})
	for _, block := range batchedBlocks {
		s.processBlock(ctx, block)
	}
	log.Debug("Finished processing batched blocks")
}

// requestBatchedBlocks sends out a request for multiple blocks that's between finalized roots
// and head roots.
func (s *InitialSync) requestBatchedBlocks(finalizedRoot []byte, canonicalRoot []byte) {
	ctx, span := trace.StartSpan(context.Background(), "beacon-chain.sync.initial-sync.requestBatchedBlocks")
	defer span.End()
	sentBatchedBlockReq.Inc()

	log.WithFields(logrus.Fields{
		"finalizedBlkRoot": fmt.Sprintf("%#x", bytesutil.Trunc(finalizedRoot[:])),
		"headBlkRoot":      fmt.Sprintf("%#x", bytesutil.Trunc(canonicalRoot[:]))},
	).Debug("Requesting batched blocks")
	if err := s.p2p.Send(ctx, &pb.BatchedBeaconBlockRequest{
		FinalizedRoot: finalizedRoot,
		CanonicalRoot: canonicalRoot,
	}, s.bestPeer); err != nil {
		log.Errorf("Could not send batch block request to peer %s: %v", s.bestPeer.Pretty(), err)
	}
}

// validateAndSaveNextBlock will validate whether blocks received from the blockfetcher
// routine can be added to the chain.
func (s *InitialSync) validateAndSaveNextBlock(ctx context.Context, block *pb.BeaconBlock) error {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.sync.initial-sync.validateAndSaveNextBlock")
	defer span.End()
	if block == nil {
		return errors.New("received nil block")
	}
	root, err := hashutil.HashBeaconBlock(block)
	if err != nil {
		return err
	}
	if err := s.checkBlockValidity(ctx, block); err != nil {
		return err
	}
	log.WithFields(logrus.Fields{
		"root": fmt.Sprintf("%#x", bytesutil.Trunc(root[:])),
		"slot": block.Slot - params.BeaconConfig().GenesisSlot,
	}).Info("Saving block")
	s.currentSlot = block.Slot

	s.mutex.Lock()
	defer s.mutex.Unlock()
	state, err := s.db.HeadState(ctx)
	if err != nil {
		return err
	}
	if err := s.chainService.VerifyBlockValidity(ctx, block, state); err != nil {
		return err
	}
	if err := s.db.SaveBlock(block); err != nil {
		return err
	}
	if err := s.db.SaveAttestationTarget(ctx, &pb.AttestationTarget{
		Slot:       block.Slot,
		BlockRoot:  root[:],
		ParentRoot: block.ParentRootHash32,
	}); err != nil {
		return fmt.Errorf("could not to save attestation target: %v", err)
	}
	state, err = s.chainService.ApplyBlockStateTransition(ctx, block, state)
	if err != nil {
		return fmt.Errorf("could not apply block state transition: %v", err)
	}
	if err := s.chainService.CleanupBlockOperations(ctx, block); err != nil {
		return err
	}
	return s.db.UpdateChainHead(ctx, block, state)
}
