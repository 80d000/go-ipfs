package provider

import (
	"context"
	"fmt"
	"time"
	"gx/ipfs/QmS2aqUZLJp8kF1ihE5rvDGE5LvmKDPnx32w9Z1BW9xLV5/go-ipfs-blockstore"
	"gx/ipfs/QmR8BauakNcBa3RbE4nbQu76PDiJgoQgz8AJdhJuiU4TAw/go-cid"
	"gx/ipfs/QmZBH87CAPFHcc7cYmBqeSQ98zQ3SX9KUxiYgzPmLWNVKz/go-libp2p-routing"
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
	progressCtx, done := context.WithCancel(ctx)

	var err error
	df := func(e error) {
		err = e
		done()
	}

	select {
	case <-rp.ctx.Done():
		return context.Canceled
	case <-ctx.Done():
		return context.Canceled
	case rp.trigger <- df:
		<-progressCtx.Done()
		return err
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

		//'mute' the trigger channel so when `ipfs bitswap reprovide` is called
		//a 'reprovider is already running' error is returned
		unmute := rp.muteTrigger()

		err := rp.Reprovide()
		if err != nil {
			log.Debug(err)
		}

		if done != nil {
			done(err)
		}

		unmute()

		after = time.After(rp.tick)
	}
}

func (rp *Reprovider) muteTrigger() context.CancelFunc {
	ctx, cf := context.WithCancel(rp.ctx)
	go func() {
		defer cf()
		for {
			select {
			case <-ctx.Done():
				return
			case done := <-rp.trigger:
				done(fmt.Errorf("reprovider is already running"))
			}
		}
	}()

	return cf
}

func (rp *Reprovider) handleAnnouncements() {
	for workers := 0; workers < reprovideOutgoingWorkerLimit; workers++ {
		go func() {
			for {
				select {
				case <-rp.ctx.Done():
					return
				case entry := <-rp.queue.Dequeue():
					// if not in blockstore, skip and stop tracking
                	inBlockstore, err := rp.blockstore.Has(entry.cid)
					if err != nil {
						log.Warningf("Unable to check for presence in blockstore: %s, %s", entry.cid, err)
						continue
					}
					if !inBlockstore {
						if err := rp.tracker.Untrack(entry.cid); err != nil {
							log.Warningf("Unable to untrack: %s, %s", entry.cid, err)
						}
						if err := entry.Complete(); err != nil {
							log.Warningf("Unable to complete queue entry when untracking: %s, %s", entry.cid, err)
						}
						continue
					}

					// announce
					if err := rp.announce(entry.cid); err != nil {
						log.Warningf("Unable to announce providing: %s, %s", entry.cid, err)
						// TODO: Maybe put these failures onto a failures queue?
						if err := entry.Complete(); err != nil {
							log.Warningf("Unable to complete queue entry for failure: %s, %s", entry.cid, err)
						}
						continue
					}

					// track entry
					if err := rp.tracker.Track(entry.cid); err != nil {
						log.Warningf("Unable to track: %s, %s", entry.cid, err)
						continue
					}

					// remove entry from queue
					if err := entry.Complete(); err != nil {
						log.Warningf("Unable to complete entry: %s, %s", entry, err)
						continue
					}
				}
			}
		}()
	}
}

// Announce to the world that a block is provided.
func (rp *Reprovider) announce(cid cid.Cid) error {
	ctx, cancel := context.WithTimeout(rp.ctx, provideOutgoingTimeout)
	defer cancel()
	fmt.Println("reprovider - announce - start - ", cid)
	if err := rp.contentRouting.Provide(ctx, cid, true); err != nil {
		log.Warningf("Failed to provide cid: %s", err)
		return err
	}
	fmt.Println("reprovider - announce - end - ", cid)
	return nil
}
