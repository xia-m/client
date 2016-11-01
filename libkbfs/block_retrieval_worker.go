// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"reflect"

	"golang.org/x/net/context"
)

// blockRetrievalWorker processes blockRetrievalQueue requests
type blockRetrievalWorker struct {
	blockGetter
	stopCh chan struct{}
	queue  *blockRetrievalQueue
}

// run runs the worker loop until Shutdown is called
func (brw *blockRetrievalWorker) run() {
	for {
		select {
		case <-brw.stopCh:
			return
		default:
			brw.HandleRequest()
		}
	}
}

// newBlockRetrievalWorker returns a blockRetrievalWorker for a given
// blockRetrievalQueue, using the passed in blockGetter to obtain blocks for
// requests.
func newBlockRetrievalWorker(bg blockGetter, q *blockRetrievalQueue) *blockRetrievalWorker {
	brw := &blockRetrievalWorker{
		blockGetter: bg,
		stopCh:      make(chan struct{}),
		queue:       q,
	}
	go brw.run()
	return brw
}

// notifyBlockRequestor copies the source block into the request's block
// pointer, and notifies the channel that the request is waiting on. Should be
// called in a goroutine.
func notifyBlockRequestor(req *blockRetrievalRequest, source reflect.Value, err error) {
	// Copy the decrypted block to the caller
	dest := reflect.ValueOf(req.block).Elem()
	dest.Set(source)
	req.doneCh <- err
}

// finalizerRetrieval is the last step of a retrieval request once a block has
// been obtained. It removes the request from the blockRetrievalQueue (using
// FinalizeRequest), then calls notifyBlockRequestor for all subscribed
// requests.
func (brw *blockRetrievalWorker) finalizeRetrieval(retrieval *blockRetrieval, block Block, err error) {
	brw.queue.FinalizeRequest(retrieval.blockPtr)
	sourceVal := reflect.ValueOf(block).Elem()
	for _, req := range retrieval.requests {
		go notifyBlockRequestor(req, sourceVal, err)
	}
}

// HandleRequest is the main work method for the worker. It obtains a
// blockRetrieval from the queue, retrieves the block using
// blockGetter.getBlock, and responds to the subscribed requestors with the
// results.
func (brw *blockRetrievalWorker) HandleRequest() (err error) {
	retrieval := <-brw.queue.WorkOnRequest()
	// Create a new block of the same type as the first request
	typ := reflect.TypeOf(retrieval.requests[0].block).Elem()
	block := reflect.New(typ).Interface().(Block)
	defer func() {
		brw.finalizeRetrieval(retrieval, block, err)
	}()

	// Pick one of the still-active contexts to use
	// FIXME: this will be racy because retrieval.requests can mutate until
	// brw.queue.FinalizeRequest is called
	var ctx context.Context
	canceled := true
	for _, req := range retrieval.requests {
		// Handle canceled contexts
		select {
		case <-req.ctx.Done():
		default:
			ctx = req.ctx
			canceled = false
			break
		}
	}
	if canceled {
		return context.Canceled
	}

	return brw.getBlock(ctx, retrieval.kmd, retrieval.blockPtr, block)
}

// Shutdown shuts down the blockRetrievalWorker once its current work is done.
func (brw *blockRetrievalWorker) Shutdown() {
	select {
	case <-brw.stopCh:
	default:
		close(brw.stopCh)
	}
}
