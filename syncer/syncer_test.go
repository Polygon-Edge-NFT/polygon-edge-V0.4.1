package syncer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/0xPolygon/polygon-edge/blockchain"
	"github.com/0xPolygon/polygon-edge/helper/progress"
	"github.com/0xPolygon/polygon-edge/network/event"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/assert"
)

type mockProgression struct {
	startingBlock uint64
	highestBlock  uint64
}

func (m *mockProgression) StartProgression(startingBlock uint64, subscription blockchain.Subscription) {
	m.startingBlock = startingBlock
}

func (m *mockProgression) UpdateHighestProgression(highestBlock uint64) {
	m.highestBlock = highestBlock
}

func (m *mockProgression) GetProgression() *progress.Progression {
	// Syncer doesn't use this method. It just exports
	return nil
}

type mockBlockchain struct {
	subscription                blockchain.Subscription
	headerHandler               func() *types.Header
	getBlockByNumberHandler     func(uint64, bool) (*types.Block, bool)
	verifyFinalizedBlockHandler func(*types.Block) error
	writeBlockHandler           func(*types.Block) error
}

func (m *mockBlockchain) SubscribeEvents() blockchain.Subscription {
	return m.subscription
}

func (m *mockBlockchain) Header() *types.Header {
	return m.headerHandler()
}

func (m *mockBlockchain) GetBlockByNumber(number uint64, full bool) (*types.Block, bool) {
	return m.getBlockByNumberHandler(number, full)
}

func (m *mockBlockchain) VerifyFinalizedBlock(b *types.Block) error {
	return m.verifyFinalizedBlockHandler(b)
}

func (m *mockBlockchain) WriteBlock(b *types.Block) error {
	return m.writeBlockHandler(b)
}

func newSimpleHeaderHandler(num uint64) func() *types.Header {
	return func() *types.Header {
		return &types.Header{
			Number: num,
		}
	}
}

type mockSyncPeerService struct{}

func (m *mockSyncPeerService) Start() {}

func (m *mockProgression) StopProgression() {}

type mockSyncPeerClient struct {
	getPeerStatusHandler                  func(peer.ID) (*NoForkPeer, error)
	getConnectedPeerStatusesHandler       func() []*NoForkPeer
	getBlocksHandler                      func(context.Context, peer.ID, uint64) (<-chan *types.Block, error)
	getPeerStatusUpdateChHandler          func() <-chan *NoForkPeer
	getPeerConnectionUpdateEventChHandler func() <-chan *event.PeerEvent
}

func (m *mockSyncPeerClient) Start() error {
	return nil
}

func (m *mockSyncPeerClient) Close() {}

func (m *mockSyncPeerClient) GetPeerStatus(id peer.ID) (*NoForkPeer, error) {
	return m.getPeerStatusHandler(id)
}

func (m *mockSyncPeerClient) GetConnectedPeerStatuses() []*NoForkPeer {
	return m.getConnectedPeerStatusesHandler()
}

func (m *mockSyncPeerClient) GetBlocks(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
	return m.getBlocksHandler(ctx, id, start)
}

func (m *mockSyncPeerClient) GetPeerStatusUpdateCh() <-chan *NoForkPeer {
	return m.getPeerStatusUpdateChHandler()
}

func (m *mockSyncPeerClient) GetPeerConnectionUpdateEventCh() <-chan *event.PeerEvent {
	return m.getPeerConnectionUpdateEventChHandler()
}

func (m *mockSyncPeerClient) CloseStream(peerID peer.ID) error {
	return nil
}

func GetAllElementsFromPeerMap(t *testing.T, p *PeerMap) []*NoForkPeer {
	t.Helper()

	peers := make([]*NoForkPeer, 0, 3)

	p.Range(func(key, value interface{}) bool {
		peer, ok := value.(*NoForkPeer)
		assert.True(t, ok)

		peers = append(peers, peer)

		return true
	})

	return peers
}

func sortPeerStatuses(peerStatuses []*NoForkPeer) []*NoForkPeer {
	sort.Slice(peerStatuses, func(p, q int) bool {
		return peerStatuses[p].Number < peerStatuses[q].Number
	})

	return peerStatuses
}

func NewTestSyncer(
	network Network,
	blockchain Blockchain,
	blockTimeout time.Duration,
	mockSyncPeerClient *mockSyncPeerClient,
	mockProgression Progression,
) *syncer {
	return &syncer{
		logger:          hclog.NewNullLogger(),
		blockchain:      blockchain,
		syncProgression: mockProgression,
		syncPeerService: &mockSyncPeerService{},
		syncPeerClient:  mockSyncPeerClient,
		blockTimeout:    blockTimeout,
		newStatusCh:     make(chan struct{}),
		peerMap:         new(PeerMap),
	}
}

// Test whether Syncer calls GetConnectedPeerStatuses and initialize peerMap
func Test_initializePeerMap(t *testing.T) {
	t.Parallel()

	peerStatuses := []*NoForkPeer{
		{
			ID:       peer.ID("A"),
			Number:   10,
			Distance: big.NewInt(10),
		},
		{
			ID:       peer.ID("B"),
			Number:   20,
			Distance: big.NewInt(20),
		},
		{
			ID:       peer.ID("C"),
			Number:   30,
			Distance: big.NewInt(30),
		},
	}

	tests := []struct {
		name                   string
		initializePeerStatuses []*NoForkPeer
		newPeerStatuses        []*NoForkPeer
	}{
		{
			name:                   "initialize peerMap by GetConnectedPeerStatuses",
			initializePeerStatuses: peerStatuses,
			newPeerStatuses:        nil,
		},
		{
			name:                   "update peerMap by GetPeerStatusUpdateCh",
			initializePeerStatuses: nil,
			newPeerStatuses:        peerStatuses,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			syncer := NewTestSyncer(
				nil,
				nil,
				0,
				&mockSyncPeerClient{
					getConnectedPeerStatusesHandler: func() []*NoForkPeer {
						return test.initializePeerStatuses
					},
					getPeerStatusUpdateChHandler: func() <-chan *NoForkPeer {
						ch := make(chan *NoForkPeer, len(test.newPeerStatuses))

						for _, s := range test.newPeerStatuses {
							ch <- s
						}

						close(ch)

						return ch
					},
				},
				&mockProgression{},
			)

			syncer.initializePeerMap()

			peerMapStatuses := sortPeerStatuses(
				GetAllElementsFromPeerMap(t, syncer.peerMap),
			)

			assert.Equal(t, peerStatuses, peerMapStatuses)
		})
	}
}

func Test_startPeerDisconnectEventProcess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		events          []*event.PeerEvent
		statuses        map[peer.ID]*NoForkPeer
		expectedPeerMap []*NoForkPeer
	}{
		{
			name: "should add peer to PeerMap after PeerConnected",
			events: []*event.PeerEvent{
				{
					PeerID: peer.ID("A"),
					Type:   event.PeerConnected,
				},
				{
					PeerID: peer.ID("B"),
					Type:   event.PeerConnected,
				},
			},
			statuses: map[peer.ID]*NoForkPeer{
				peer.ID("A"): {
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(10),
				},
				peer.ID("B"): {
					ID:       peer.ID("B"),
					Number:   20,
					Distance: big.NewInt(20),
				},
			},
			expectedPeerMap: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(10),
				},
				{
					ID:       peer.ID("B"),
					Number:   20,
					Distance: big.NewInt(20),
				},
			},
		},
		{
			name: "should remove peer to PeerMap after PeerDisconnected",
			events: []*event.PeerEvent{
				{
					PeerID: peer.ID("A"),
					Type:   event.PeerConnected,
				},
				{
					PeerID: peer.ID("A"),
					Type:   event.PeerDisconnected,
				},
			},
			statuses: map[peer.ID]*NoForkPeer{
				peer.ID("A"): {
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(10),
				},
			},
			expectedPeerMap: []*NoForkPeer{},
		},
		{
			name: "should happen nothing in case of PeerFailedToConnect, PeerDialCompleted, PeerAddedToDialQueue",
			events: []*event.PeerEvent{
				{
					PeerID: peer.ID("A"),
					Type:   event.PeerFailedToConnect,
				},
				{
					PeerID: peer.ID("B"),
					Type:   event.PeerDialCompleted,
				},
				{
					PeerID: peer.ID("C"),
					Type:   event.PeerAddedToDialQueue,
				},
			},
			statuses:        map[peer.ID]*NoForkPeer{},
			expectedPeerMap: []*NoForkPeer{},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			syncer := NewTestSyncer(
				nil,
				nil,
				0,
				&mockSyncPeerClient{
					getPeerConnectionUpdateEventChHandler: func() <-chan *event.PeerEvent {
						ch := make(chan *event.PeerEvent, len(test.events))

						go func() {
							for _, e := range test.events {
								ch <- e

								// add delay to simulate real event emission
								time.Sleep(500 * time.Millisecond)
							}

							close(ch)
						}()

						return ch
					},
					getPeerStatusHandler: func(i peer.ID) (*NoForkPeer, error) {
						status, ok := test.statuses[i]
						if !ok {
							return nil, fmt.Errorf("peer %s didn't return status", i)
						}

						return status, nil
					},
				},
				&mockProgression{},
			)

			syncer.startPeerConnectionEventProcess()

			peerMapStatuses := GetAllElementsFromPeerMap(t, syncer.peerMap)

			// no need to check order
			peerMapStatuses = sortPeerStatuses(peerMapStatuses)

			assert.Equal(t, test.expectedPeerMap, peerMapStatuses)
		})
	}
}

func TestHasSyncPeer(t *testing.T) {
	t.Parallel()

	peerStatuses := []*NoForkPeer{
		{
			ID:       peer.ID("A"),
			Number:   10,
			Distance: big.NewInt(10),
		},
		{
			ID:       peer.ID("B"),
			Number:   20,
			Distance: big.NewInt(20),
		},
	}

	tests := []struct {
		name        string
		localLatest uint64
		peers       []*NoForkPeer
		result      bool
	}{
		{
			name:        "should return true when peerMap has elements",
			localLatest: 0,
			peers:       peerStatuses,
			result:      true,
		},
		{
			name:        "should return false when peerMap is empty",
			localLatest: 0,
			peers:       nil,
			result:      false,
		},
		{
			name:        "should return false when local latest is greater than any peers in peerMap",
			localLatest: 30,
			peers:       peerStatuses,
			result:      false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			syncer := NewTestSyncer(
				nil,
				&mockBlockchain{
					headerHandler: newSimpleHeaderHandler(test.localLatest),
				},
				0,
				&mockSyncPeerClient{},
				&mockProgression{},
			)

			syncer.peerMap.PutPeers(test.peers)

			assert.Equal(t, test.result, syncer.HasSyncPeer())
		})
	}
}

func blocksToCh(blocks []*types.Block, delay time.Duration) <-chan *types.Block {
	ch := make(chan *types.Block)

	go func() {
		for _, b := range blocks {
			time.Sleep(delay)

			ch <- b
		}

		close(ch)
	}()

	return ch
}

func createMockBlocks(num int) []*types.Block {
	blocks := make([]*types.Block, num)
	for i := 0; i < num; i++ {
		blocks[i] = &types.Block{
			Header: &types.Header{
				Number: uint64(i + 1),
			},
		}
	}

	return blocks
}

func TestBulkSync(t *testing.T) {
	t.Parallel()

	blocks := createMockBlocks(10)

	tests := []struct {
		name string

		// local
		beginningHeight uint64
		blockCallback   func(*types.Block) bool

		// peers
		peerStatuses []*NoForkPeer
		peerBlocksCh map[peer.ID]<-chan *types.Block

		// handlers
		// a function to return a callback to use closure
		createVerifyFinalizedBlockHandler func() func(*types.Block) error

		// results
		blocks             []*types.Block
		progressionStart   uint64
		progressionHighest uint64
		err                error
	}{
		{
			name:            "should sync blocks to the latest successfully",
			beginningHeight: 0,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			peerStatuses: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(0),
				},
			},
			peerBlocksCh: map[peer.ID]<-chan *types.Block{
				peer.ID("A"): blocksToCh(blocks[:10], 0),
			},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				return func(b *types.Block) error {
					return nil
				}
			},
			blocks:             blocks[:10],
			progressionStart:   1,
			progressionHighest: 10,
			err:                nil,
		},
		{
			name:            "should not sync in case of empty peer map",
			beginningHeight: 0,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			peerStatuses: []*NoForkPeer{},
			peerBlocksCh: map[peer.ID]<-chan *types.Block{},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				return func(b *types.Block) error {
					return nil
				}
			},
			blocks:             []*types.Block{},
			progressionStart:   1,
			progressionHighest: 0,
			err:                nil,
		},
		{
			name:            "should not sync when best peer's number is less than local one",
			beginningHeight: 20,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			peerStatuses: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(0),
				},
			},
			peerBlocksCh: map[peer.ID]<-chan *types.Block{},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				return func(b *types.Block) error {
					return nil
				}
			},
			blocks:             []*types.Block{},
			progressionStart:   21,
			progressionHighest: 0,
			err:                nil,
		},
		{
			name:            "should sync with multiple peers if sync is terminated in the middle",
			beginningHeight: 0,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			peerStatuses: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(0),
				},
				{
					ID:       peer.ID("B"),
					Number:   8,
					Distance: big.NewInt(0),
				},
			},
			peerBlocksCh: map[peer.ID]<-chan *types.Block{
				peer.ID("A"): blocksToCh(blocks[:10], 0),
				peer.ID("B"): blocksToCh(blocks[4:8], 0),
			},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				count := 0

				return func(b *types.Block) error {
					count++

					// fail block verification in the middle of sync with peer A
					if count == 5 {
						return errors.New("invalid block")
					}

					return nil
				}
			},
			blocks:             blocks[:8],
			progressionStart:   1,
			progressionHighest: 8,
			err:                nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				syncedBlocks      = make([]*types.Block, 0, len(test.blocks))
				latestBlockNumber = test.beginningHeight
				progression       = &mockProgression{}

				syncer = NewTestSyncer(
					nil,
					&mockBlockchain{
						headerHandler:               newSimpleHeaderHandler(latestBlockNumber),
						verifyFinalizedBlockHandler: test.createVerifyFinalizedBlockHandler(),
						writeBlockHandler: func(b *types.Block) error {
							syncedBlocks = append(syncedBlocks, b)
							latestBlockNumber = b.Number()

							return nil
						},
					},
					time.Second,
					&mockSyncPeerClient{
						getBlocksHandler: func(ctx context.Context, i peer.ID, u uint64) (<-chan *types.Block, error) {
							return test.peerBlocksCh[i], nil
						},
					},
					progression,
				)
			)

			syncer.peerMap.PutPeers(test.peerStatuses)

			err := syncer.BulkSync(context.Background(), test.blockCallback)

			assert.Equal(t, test.blocks, syncedBlocks)
			assert.Equal(t, test.progressionStart, progression.startingBlock)
			assert.Equal(t, test.progressionHighest, progression.highestBlock)
			assert.ErrorIs(t, err, test.err)
		})
	}
}

func TestWatchSync(t *testing.T) {
	t.Parallel()

	blocks := createMockBlocks(10)

	tests := []struct {
		name string

		// local
		beginningHeight     uint64
		createBlockCallback func() func(*types.Block) bool

		// peers
		peerStatuses []*NoForkPeer

		peerBlocksCh   map[peer.ID]<-chan *types.Block
		newStatusDelay time.Duration

		// handlers
		// a function to return a callback to use closure
		createVerifyFinalizedBlockHandler func() func(*types.Block) error

		// results
		blocks             []*types.Block
		progressionStart   uint64
		progressionHighest uint64
		err                error
	}{
		{
			name:            "should sync blocks to the latest successfully",
			beginningHeight: 0,
			createBlockCallback: func() func(*types.Block) bool {
				return func(b *types.Block) bool {
					return b.Number() >= 10
				}
			},
			peerStatuses: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(0),
				},
			},
			newStatusDelay: 0,
			peerBlocksCh: map[peer.ID]<-chan *types.Block{
				peer.ID("A"): blocksToCh(blocks[:10], 0),
			},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				return func(b *types.Block) error {
					return nil
				}
			},
			blocks: blocks[:10],
			// TODO: need to fix implementation?
			progressionStart:   0,
			progressionHighest: 0,
			err:                nil,
		},
		{
			name:            "should sync blocks with multiple peers",
			beginningHeight: 0,
			createBlockCallback: func() func(*types.Block) bool {
				return func(b *types.Block) bool {
					return b.Number() >= 10
				}
			},
			peerStatuses: []*NoForkPeer{
				{
					ID:       peer.ID("A"),
					Number:   10,
					Distance: big.NewInt(0),
				},
				{
					ID:       peer.ID("B"),
					Number:   10,
					Distance: big.NewInt(1),
				},
			},
			newStatusDelay: 0,
			peerBlocksCh: map[peer.ID]<-chan *types.Block{
				peer.ID("A"): blocksToCh(blocks[:10], 0),
				peer.ID("B"): blocksToCh(blocks[4:10], 0),
			},
			createVerifyFinalizedBlockHandler: func() func(*types.Block) error {
				count := 0

				return func(b *types.Block) error {
					if b.Number() == 5 {
						count++

						if count == 1 {
							return errors.New("block verification failed")
						}
					}

					return nil
				}
			},
			blocks: blocks[:10],
			// TODO: need to fix implementation?
			progressionStart:   0,
			progressionHighest: 0,
			err:                nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				syncedBlocks      = make([]*types.Block, 0, len(test.blocks))
				latestBlockNumber = test.beginningHeight
				progression       = &mockProgression{}

				syncer = NewTestSyncer(
					nil,
					&mockBlockchain{
						headerHandler:               newSimpleHeaderHandler(latestBlockNumber),
						verifyFinalizedBlockHandler: test.createVerifyFinalizedBlockHandler(),
						writeBlockHandler: func(b *types.Block) error {
							syncedBlocks = append(syncedBlocks, b)
							latestBlockNumber = b.Number()

							return nil
						},
					},
					time.Second,
					&mockSyncPeerClient{
						getBlocksHandler: func(ctx context.Context, i peer.ID, u uint64) (<-chan *types.Block, error) {
							// should not panic
							peerCh := test.peerBlocksCh[i]

							return peerCh, nil
						},
					},
					progression,
				)
			)

			errCh := make(chan error, 1)

			go func() {
				errCh <- syncer.WatchSync(context.Background(), test.createBlockCallback())
			}()

			go func() {
				for _, p := range test.peerStatuses {
					syncer.peerMap.Put(p)

					syncer.newStatusCh <- struct{}{}

					time.Sleep(test.newStatusDelay)
				}
			}()

			err := <-errCh

			assert.Equal(t, test.blocks, syncedBlocks)
			assert.Equal(t, test.progressionStart, progression.startingBlock)
			assert.Equal(t, test.progressionHighest, progression.highestBlock)
			assert.ErrorIs(t, err, test.err)
		})
	}
}

func Test_bulkSyncWithPeer(t *testing.T) {
	t.Parallel()

	blockNum := 30
	blocks := make([]*types.Block, blockNum) // 1 to 30

	for i := 0; i < blockNum; i++ {
		blocks[i] = &types.Block{
			Header: &types.Header{
				Number: uint64(i + 1),
			},
		}
	}

	var (
		// mock errors
		errPeerNoResponse       = errors.New("peer is not responding")
		errInvalidBlock         = errors.New("invalid block")
		errBlockInsertionFailed = errors.New("failed to insert block")
	)

	tests := []struct {
		name string

		// local
		beginningHeight uint64
		blockTimeout    time.Duration
		blockCallback   func(*types.Block) bool

		// peers
		getBlocksHandler func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error)

		// handlers
		verifyFinalizedBlockHandler func(*types.Block) error
		writeBlockHandler           func(*types.Block) error

		// results
		blocks                []*types.Block
		lastSyncedBlockNumber uint64
		shouldTerminate       bool
		err                   error
	}{
		{
			name:            "should sync blocks to the latest successfully",
			beginningHeight: 0,
			blockTimeout:    time.Second,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			getBlocksHandler: func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
				return blocksToCh(blocks[:10], 0), nil
			},
			verifyFinalizedBlockHandler: func(b *types.Block) error {
				return nil
			},
			writeBlockHandler: func(b *types.Block) error {
				return nil
			},
			blocks:                blocks[:10],
			lastSyncedBlockNumber: 10,
			shouldTerminate:       false,
			err:                   nil,
		},
		{
			name:            "should return error if GetBlocks returns error",
			beginningHeight: 0,
			blockTimeout:    time.Second,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			getBlocksHandler: func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
				return nil, errPeerNoResponse
			},
			verifyFinalizedBlockHandler: func(b *types.Block) error {
				return nil
			},
			writeBlockHandler: func(b *types.Block) error {
				return nil
			},
			blocks:                []*types.Block{},
			lastSyncedBlockNumber: 0,
			shouldTerminate:       false,
			err:                   errPeerNoResponse,
		},
		{
			name:            "should return error if verification is failed",
			beginningHeight: 0,
			blockTimeout:    time.Second,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			getBlocksHandler: func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
				return blocksToCh(blocks[:10], 0), nil
			},
			verifyFinalizedBlockHandler: func(b *types.Block) error {
				if b.Number() > 5 {
					return errInvalidBlock
				}

				return nil
			},
			writeBlockHandler: func(b *types.Block) error {
				return nil
			},
			blocks:                blocks[:5],
			lastSyncedBlockNumber: 5,
			shouldTerminate:       false,
			err:                   errInvalidBlock,
		},
		{
			name:            "should return error if block insertion is failed",
			beginningHeight: 0,
			blockTimeout:    time.Second,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			getBlocksHandler: func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
				return blocksToCh(blocks[:10], 0), nil
			},
			verifyFinalizedBlockHandler: func(b *types.Block) error {
				return nil
			},
			writeBlockHandler: func(b *types.Block) error {
				if b.Number() > 5 {
					return errBlockInsertionFailed
				}

				return nil
			},
			blocks:                blocks[:5],
			lastSyncedBlockNumber: 5,
			shouldTerminate:       false,
			err:                   errBlockInsertionFailed,
		},
		{
			name:            "should return error in case of timeout",
			beginningHeight: 0,
			blockTimeout:    500 * time.Millisecond,
			blockCallback: func(b *types.Block) bool {
				return false
			},
			getBlocksHandler: func(ctx context.Context, id peer.ID, start uint64) (<-chan *types.Block, error) {
				return blocksToCh(blocks[:10], time.Second*1), nil
			},
			verifyFinalizedBlockHandler: func(b *types.Block) error {
				return nil
			},
			writeBlockHandler: func(b *types.Block) error {
				return nil
			},
			blocks:                []*types.Block{},
			lastSyncedBlockNumber: 0,
			shouldTerminate:       false,
			err:                   errTimeout,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				syncedBlocks = make([]*types.Block, 0, len(test.blocks))

				syncer = NewTestSyncer(
					nil,
					&mockBlockchain{
						headerHandler:               newSimpleHeaderHandler(test.beginningHeight),
						verifyFinalizedBlockHandler: test.verifyFinalizedBlockHandler,
						writeBlockHandler: func(b *types.Block) error {
							if err := test.writeBlockHandler(b); err != nil {
								return err
							}

							syncedBlocks = append(syncedBlocks, b)

							return nil
						},
					},
					test.blockTimeout,
					&mockSyncPeerClient{
						getBlocksHandler: test.getBlocksHandler,
					},
					&mockProgression{},
				)
			)

			lastSynced, shouldTerminate, err := syncer.bulkSyncWithPeer(peer.ID("X"), test.blockCallback)

			assert.Equal(t, test.lastSyncedBlockNumber, lastSynced)
			assert.Equal(t, test.shouldTerminate, shouldTerminate)
			assert.ErrorIs(t, err, test.err)
			assert.Equal(t, test.blocks, syncedBlocks)
		})
	}
}