package lfring

import (
	atomic "sync/atomic"
)

// nodeBased defines a multi-producer multi-consumer ring buffer.
//
// Fully borrowed from here:
// http://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue
//
// Due to both producer and consumer become multi-thread, we must maintain atomicity
// of both head / tail and the stored value (namely element).
//
// Rather than store value directly to element[], the solution extract a node structure
// to hold "step" and "value". We can seen the "step" as node's stamp, stamp is a general
// solution to the problem of ABA at value (e.g. consumer read the old value that have not
// bean refreshed by recently producer).
//
// 1. Every time when a producer try to Offer a value, it first check the node that pointed
// by current tail, only if node.step == head, means the node.value has been polled and can
// be Offer. Then try to CAS add tail (claim to be the current Offer owner of node). Once CAS
// success, we can ensure the current thread has the full ownership of the tail node. After
// done Offer job, at last we set node.step to tail+1, to announce the Offer completed.
//
// 2. Every time when a consumer try to Poll a value, it first check the node that pointed
// by current head, only if node.step == head+1, means the node.value has been offered and
// can be Poll (why offer check node.step == tail but poll need to check head+1 ? We should
// keep 1 step gap between head and tail to make it sequentially). Then try to CAS add head
// (claim to be the current Poll owner of node). Once CAS success, we can ensure the current
// thread has the full ownership of the head node. After done Poll job, at last we set
// node.step to head + mask, to announce the Poll completed. The reason head + mask
// is to tell the next producer move to this node: "I'm available to be Offer", we can simply
// calculate the next producer should hold the tail of head + mask (tail moved over the
// ring back to here).
//
// The another difference between this to the mpsc is we no longer need isEmpty() and isFull()
// to check the buffer status, if buffer full / empty will lead the producer / consumer never
// pass the node.step check.
type nodeBased[T any] struct {
	head      uint64
	_padding0 [56]byte
	tail      uint64
	_padding1 [56]byte
	mask      uint64
	_padding2 [56]byte
	element   []*node[T]
}

type node[T any] struct {
	step     uint64
	value    T
	_padding [40]byte
}

func newNodeBased[T any](capacity uint64) RingBuffer[T] {
	nodes := make([]*node[T], capacity)
	for i := uint64(0); i < capacity; i++ {
		nodes[i] = &node[T]{step: i}
	}

	return &nodeBased[T]{
		head:    uint64(0),
		tail:    uint64(0),
		mask:    capacity - 1,
		element: nodes,
	}
}

// Offer a value pointer.
func (r *nodeBased[T]) Offer(value T) (success bool) {
	oldTail := atomic.LoadUint64(&r.tail)
	tailNode := r.element[oldTail&r.mask]
	oldStep := atomic.LoadUint64(&tailNode.step)
	// not published yet
	if oldStep != oldTail {
		return false
	}

	if !atomic.CompareAndSwapUint64(&r.tail, oldTail, oldTail+1) {
		return false
	}

	tailNode.value = value
	atomic.StoreUint64(&tailNode.step, tailNode.step+1)
	return true
}

// Poll head value pointer.
func (r *nodeBased[T]) Poll() (value T, success bool) {
	oldHead := atomic.LoadUint64(&r.head)
	headNode := r.element[oldHead&r.mask]
	oldStep := atomic.LoadUint64(&headNode.step)
	// not published yet
	if oldStep != oldHead+1 {
		return
	}

	if !atomic.CompareAndSwapUint64(&r.head, oldHead, oldHead+1) {
		return
	}

	value = headNode.value
	atomic.StoreUint64(&headNode.step, oldStep+r.mask)
	return value, true
}

func (r *nodeBased[T]) SingleProducerOffer(valueSupplier func() (v T, finish bool)) {
	// TODO: currently just wrapper
	for {
		v, finish := valueSupplier()
		if finish {
			return
		}

		for !r.Offer(v) {
		}
	}
}

func (r *nodeBased[T]) SingleConsumerPoll(valueConsumer func(T)) {
	// TODO: currently just wrapper
	for {
		v, success := r.Poll()
		if !success {
			return
		}
		valueConsumer(v)
	}
}

func (r *nodeBased[T]) SingleConsumerPollVec(ret []T) (end uint64) {
	// TODO: currently just wrapper
	var cnt int
	for ; cnt < len(ret); cnt++ {
		v, success := r.Poll()
		if !success {
			break
		}

		ret[cnt] = v
	}

	return uint64(cnt)
}

// Alternative optimized version that tries to batch claim multiple positions
// This is more complex but could be even faster under high contention
func (r *nodeBased[T]) PollNBatched(n uint64) (values []T, count uint64) {
	if n == 0 {
		return nil, 0
	}

	values = make([]T, 0, n)
	
	for count < n {
		oldHead := atomic.LoadUint64(&r.head)
		
		// Check how many consecutive values are available
		available := uint64(0)
		for i := uint64(0); i < n-count && available < 8; i++ { // Limit batch size to avoid long loops
			nodeIdx := (oldHead + i) & r.mask
			node := r.element[nodeIdx]
			step := atomic.LoadUint64(&node.step)
			
			if step != oldHead+i+1 {
				break // This value is not ready
			}
			available++
		}
		
		if available == 0 {
			break // No values available
		}
		
		// Try to claim this batch
		if !atomic.CompareAndSwapUint64(&r.head, oldHead, oldHead+available) {
			// Another consumer interfered, try again with single item
			continue
		}
		
		// Successfully claimed batch, extract values
		for i := uint64(0); i < available; i++ {
			nodeIdx := (oldHead + i) & r.mask
			node := r.element[nodeIdx]
			step := atomic.LoadUint64(&node.step)
			
			values = append(values, node.value)
			atomic.StoreUint64(&node.step, step+r.mask)
		}
		
		count += available
	}

	return values, count
}
