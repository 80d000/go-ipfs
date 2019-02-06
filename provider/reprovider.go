package provider

import (
	"context"
	"gx/ipfs/QmS2aqUZLJp8kF1ihE5rvDGE5LvmKDPnx32w9Z1BW9xLV5/go-ipfs-blockstore"
	"gx/ipfs/QmZBH87CAPFHcc7cYmBqeSQ98zQ3SX9KUxiYgzPmLWNVKz/go-libp2p-routing"
	"time"
)

var (
	reprovideOutgoingWorkerLimit = 8
)

type doneFunc func(error)

type Reprovider struct {
	ctx context.Context
	queue *Queue
	tracker *Tracker
	tick time.Duration
	blockstore blockstore.Blockstore
	contentRouting routing.ContentRouting
	trigger chan doneFunc
}

func NewReprovider(ctx context.Context, queue *Queue, tracker *Tracker, tick time.Duration, blockstore blockstore.Blockstore, contentRouting routing.ContentRouting) *Reprovider {
	return &Reprovider{
		ctx: ctx,
		queue: queue,
		tracker: tracker,
		tick: tick,
		blockstore: blockstore,
		contentRouting: contentRouting,
		trigger: make(chan doneFunc),
	}
}

func (rp *Reprovider) Run() {
	go rp.handleTriggers()
	go rp.handleAnnouncements()
}

func (rp *Reprovider) Reprovide() error {
	cids, err := rp.tracker.Tracking(rp.ctx)
	if err != nil {
		return err
	}
	for c := range cids {
		if err := rp.queue.Enqueue(c); err != nil {
			log.Warningf("unable to enqueue cid: %s, %s", c, err)
			continue
		}
	}
	return nil
}

// Trigger starts reprovision process in rp.Run and waits for it
func (rp *Reprovider) Trigger(ctx context.Context) error {
	progressCtx, cancel := context.WithCancel(ctx)

	var err error
	done := func(e error) {
		err = e
		cancel()
	}

	select {
	case <-rp.ctx.Done():
		return context.Canceled
	case <-ctx.Done():
		return context.Canceled
	case rp.trigger <- done:
		select {
		case <-progressCtx.Done():
			return err
		case <-ctx.Done():
			return context.Canceled
		}
	}
}

func (rp *Reprovider) handleTriggers() {
	// dont reprovide immediately.
	// may have just started the daemon and shutting it down immediately.
	// probability( up another minute | uptime ) increases with uptime.
	after := time.After(time.Minute)
	var done doneFunc
	for {
		if rp.tick == 0 {
			after = make(chan time.Time)
		}

		select {
		case <-rp.ctx.Done():
			return
		case done = <-rp.trigger:
		case <-after:
		}

		err := rp.Reprovide()
		if err != nil {
			log.Debug(err)
		}

		if done != nil {
			done(err)
		}

		after = time.After(rp.tick)
	}
}

func (rp *Reprovider) handleAnnouncements() {
	for workers := 0; workers < reprovideOutgoingWorkerLimit; workers++ {
		go func() {
			for {
				select {
				case <-rp.ctx.Done():
					return
				case entry := <-rp.queue.Dequeue():
					if err := doProvide(rp.ctx, rp.tracker, rp.blockstore, rp.contentRouting, entry.cid); err != nil {
						log.Warningf("Unable to reprovide entry: %s, %s", entry.cid, err)
					}
					if err := entry.Complete(); err != nil {
						log.Warningf("Unable to complete queue entry when reproviding: %s, %s", entry.cid, err)
					}
				}
			}
		}()
	}
}
