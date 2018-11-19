package syncer

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type HeadersJob struct {
	block uint64
	count uint64
}

type BodiesJob struct {
	hashes []*types.Header
}

type ReceiptsJob struct {
	hashes []*types.Header
}

// Job is the syncer job
type Job struct {
	id      uint32
	payload interface{}
}

const (
	maxElements = 100
)

type queue struct {
	front, back *element
	seq         uint32
	head        common.Hash // head of the sync chain
	lock        *sync.Mutex
}

func newQueue() *queue {
	return &queue{lock: &sync.Mutex{}}
}

func (q *queue) addBack(block uint64) {
	q.back = q.newItem(block)
	q.front.next = q.back
	q.back.prev = q.front
}

func (q *queue) newItem(block uint64) *element {
	id := atomic.AddUint32(&q.seq, 1)
	return &element{
		id:             id,
		block:          block,
		headersStatus:  waitingX,
		bodiesStatus:   completedX,
		receiptsStatus: completedX,
	}
}

func (q *queue) deliverHeaders(id uint32, headers []*types.Header) error {
	q.lock.Lock()
	defer q.lock.Unlock()

	elem, err := q.findElement(id)
	if err != nil {
		return err
	}

	if elem.headersStatus == completedX {
		return fmt.Errorf("headers already completed")
	}

	if len(headers) == 0 {
		elem.headersStatus = waitingX
		return nil
	}

	if len(headers) > int(elem.Len())-int(elem.headersOffset) {
		return fmt.Errorf("received more headers than expected")
	}

	// values received, check headers with prev content
	if len(elem.headers) == 0 {
		if elem.prev == nil {
			// check with head
			if q.head != headers[0].ParentHash {
				return fmt.Errorf("the head and the parent hash should match")
			}
		} else if elem.prev.headersStatus == completedX {
			// check with previous batch if they already have valid headers
			if elem.prev.Last().Hash() != headers[0].ParentHash {
				return fmt.Errorf("hash should match with previous batch")
			}
		}
	} else {
		// check with last elem header
		if elem.headers[len(elem.headers)-1].Hash() != headers[0].ParentHash {
			return fmt.Errorf("hash should match the last local header hash")
		}
	}

	// add the new headers
	for _, h := range headers {
		elem.headers = append(elem.headers, h)
	}

	if len(elem.headers) != int(elem.Len()) {
		elem.headersOffset = uint32(len(elem.headers))
		elem.headersStatus = waitingX
		return nil
	}

	// check headers with next content if exists
	if elem.next != nil && elem.next.headersStatus == completedX {
		if elem.Last().Hash() != elem.next.headers[0].ParentHash {
			return fmt.Errorf("hash mismatch with next value")
		}
	}

	// header completed, check for receipts and bodies
	elem.headersStatus = completedX
	elem.bodiesStatus = completedX
	elem.receiptsStatus = completedX

	bodies := []*types.Header{}
	receipts := []*types.Header{}

	for _, h := range elem.headers {
		if hasBody(h) {
			bodies = append(bodies, h)
		}
		if hasReceipts(h) {
			receipts = append(receipts, h)
		}
	}

	if len(receipts) != 0 {
		elem.receiptsStatus = waitingX
		elem.receiptsHeaders = receipts
	}
	if len(bodies) != 0 {
		elem.bodiesStatus = waitingX
		elem.bodiesHeaders = bodies
	}

	// TODO, check the cases
	return nil
}

func (q *queue) deliverReceipts(id uint32, receipts [][]*types.Receipt) error {
	q.lock.Lock()
	defer q.lock.Unlock()

	elem, err := q.findElement(id)
	if err != nil {
		return err
	}

	if elem.receiptsStatus == completedX {
		return fmt.Errorf("headers already completed")
	}

	if len(receipts) == 0 {
		elem.receiptsStatus = waitingX
		return nil
	}

	if len(receipts) > len(elem.receiptsHeaders)-int(elem.receiptsOffset) {
		return fmt.Errorf("received more receipts than expected")
	}

	// check if the value is correct
	offset := elem.receiptsOffset
	for indx, receipt := range receipts {
		if types.DeriveSha(types.Receipts(receipt)) != elem.receiptsHeaders[offset+uint32(indx)].ReceiptHash {
			return fmt.Errorf("")
		}
	}

	// copy the values
	for _, receipt := range receipts {
		elem.receipts = append(elem.receipts, receipt)
	}

	if len(elem.receipts) == len(elem.receiptsHeaders) {
		elem.receiptsStatus = completedX
		return nil
	}

	elem.receiptsOffset = uint32(len(elem.receipts))
	elem.receiptsStatus = waitingX

	return nil
}

func (q *queue) deliverBodies(id uint32, bodies []*types.Body) error {
	q.lock.Lock()
	defer q.lock.Unlock()

	elem, err := q.findElement(id)
	if err != nil {
		return err
	}

	if elem.bodiesStatus == completedX {
		return fmt.Errorf("headers already completed")
	}

	if len(bodies) == 0 {
		elem.bodiesStatus = waitingX
		return nil
	}

	if len(bodies) > len(elem.bodiesHeaders)-int(elem.bodiesOffset) {
		return fmt.Errorf("received more bodies than expected")
	}

	// check if the value is correct
	offset := elem.bodiesOffset
	for indx, body := range bodies {
		if types.DeriveSha(types.Transactions(body.Transactions)) != elem.bodiesHeaders[offset+uint32(indx)].TxHash {
			return fmt.Errorf("tx hash not correct")
		}
		if types.CalcUncleHash(body.Uncles) != elem.bodiesHeaders[offset+uint32(indx)].UncleHash {
			return fmt.Errorf("uncle hash not correct")
		}
	}

	// copy the values
	for _, body := range bodies {
		elem.bodies = append(elem.bodies, body)
	}

	if len(elem.bodies) == len(elem.bodiesHeaders) {
		elem.bodiesStatus = completedX
		return nil
	}

	elem.bodiesOffset = uint32(len(elem.bodies))
	elem.bodiesStatus = waitingX

	return nil
}

func (q *queue) findElement(id uint32) (*element, error) {
	elem := q.front
	for elem != nil {
		if elem.id == id {
			return elem, nil
		}
		elem = elem.next
	}
	return nil, fmt.Errorf("element %d not found", id)
}

func (q *queue) Dequeue() (*Job, error) {
	q.lock.Lock()
	defer q.lock.Unlock()

	elem := q.getNextElegibleSlot()
	if elem == nil {
		return nil, fmt.Errorf("All the jobs are different from waiting")
	}

	// headers job
	if elem.headersStatus == waitingX {
		elem.headersStatus = pendingX
		return &Job{
			id:      elem.id,
			payload: &HeadersJob{block: uint64(elem.headersOffset) + elem.block, count: uint64(100 - elem.headersOffset)},
		}, nil
	}

	// receipts job
	if elem.receiptsStatus == waitingX {
		elem.receiptsStatus = pendingX
		return &Job{
			id:      elem.id,
			payload: &ReceiptsJob{elem.receiptsHeaders[elem.receiptsOffset:]},
		}, nil
	}

	// bodies job
	if elem.bodiesStatus == waitingX {
		elem.bodiesStatus = pendingX
		return &Job{
			id:      elem.id,
			payload: &BodiesJob{elem.bodiesHeaders[elem.bodiesOffset:]},
		}, nil
	}

	return nil, fmt.Errorf("job selected was not elegible, fatal error")
}

func (q *queue) NumOfCompletedBatches() int {
	// returns the number of completed batches
	n := 0
	elem := q.front
	for elem != nil {
		if !elem.Completed() {
			break
		}
		n++
		elem = elem.next
	}
	return n
}

// FetchCompletedData returns the array of batches that have been completed and updates the head accordinly
func (q *queue) FetchCompletedData() []*element {
	elements := []*element{}

	elem := q.front
	for elem != nil {
		if !elem.Completed() {
			break
		}
		elements = append(elements, elem)
		elem = elem.next
	}

	if len(elements) != 0 {
		q.head = elements[len(elements)-1].Last().Hash()
	}

	q.front = elem
	return elements
}

func (q *queue) getNextElegibleSlot() *element {
	elem := q.front
	for elem != nil {
		if elem.headersStatus == waitingX || elem.receiptsStatus == waitingX || elem.bodiesStatus == waitingX {
			break
		}
		elem = elem.next
	}

	if elem.Len() <= maxElements {
		return elem
	}

	// split the item
	i := q.newItem(elem.block + maxElements)

	i.prev = elem
	i.next = elem.next

	elem.next = i
	elem.headersStatus = waitingX

	return elem
}

type elementStatus int

const (
	waitingX elementStatus = iota
	completedX
	pendingX
)

func (e elementStatus) String() string {
	switch e {
	case waitingX:
		return "Waiting"
	case completedX:
		return "Completed"
	case pendingX:
		return "Pending"
	default:
		panic(fmt.Errorf("Status %d not found", e))
	}
}

type element struct {
	id    uint32
	block uint64

	prev *element
	next *element

	// headers
	headers       []*types.Header
	headersStatus elementStatus
	headersOffset uint32

	// bodies
	bodies        []*types.Body
	bodiesHeaders []*types.Header
	bodiesOffset  uint32
	bodiesStatus  elementStatus

	// receipts
	receipts        []types.Receipts
	receiptsHeaders []*types.Header
	receiptsOffset  uint32
	receiptsStatus  elementStatus
}

func (e *element) Last() *types.Header {
	return e.headers[len(e.headers)-1]
}

// Completed returns true if all the data has been fetched
func (e *element) Completed() bool {
	return e.headersStatus == completedX && e.bodiesStatus == completedX && e.receiptsStatus == completedX
}

func (e *element) Len() uint64 {
	return e.next.block - e.block
}

func hasBody(h *types.Header) bool {
	return h.TxHash != types.EmptyRootHash || h.UncleHash != types.EmptyUncleHash
}

func hasReceipts(h *types.Header) bool {
	return h.ReceiptHash != types.EmptyRootHash
}
