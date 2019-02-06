package provider

import (
	"context"
	"fmt"
	"gx/ipfs/QmR8BauakNcBa3RbE4nbQu76PDiJgoQgz8AJdhJuiU4TAw/go-cid"
	bstore "gx/ipfs/QmS2aqUZLJp8kF1ihE5rvDGE5LvmKDPnx32w9Z1BW9xLV5/go-ipfs-blockstore"
	"gx/ipfs/QmZBH87CAPFHcc7cYmBqeSQ98zQ3SX9KUxiYgzPmLWNVKz/go-libp2p-routing"
	logging "gx/ipfs/QmcuXC5cxs79ro2cUuHs4HQ2bkDLJUYokwL8aivcX6HW3C/go-log"
	"time"
)

var (
	log = logging.Logger("provider")
)

const (
	provideOutgoingWorkerLimit = 8
	provideOutgoingTimeout     = 15 * time.Second
)

type Strategy func(context.Context, cid.Cid) <-chan cid.Cid

type Provider struct {
	ctx context.Context

	// strategy for deciding which CIDs, given a CID, should be provided
	strategy Strategy
	// keeps track of which CIDs have been provided already
	tracker *Tracker
	// the CIDs for which provide announcements should be made
	queue *Queue
	// where the blocks live
	blockstore bstore.Blockstore
	// used to announce providing to the network
	contentRouting routing.ContentRouting
}

func NewProvider(ctx context.Context, strategy Strategy, tracker *Tracker, queue *Queue, blockstore bstore.Blockstore, contentRouting routing.ContentRouting) *Provider {
	return &Provider{
		ctx:            ctx,
		strategy:       strategy,
		tracker:        tracker,
		queue:          queue,
		blockstore:   	blockstore,
		contentRouting: contentRouting,
	}
}

// Start workers to handle provide requests.
func (p *Provider) Run() {
	p.handleAnnouncements()
}

// Provider the given cid using specified strategy.
func (p *Provider) Provide(root cid.Cid) error {
	cids := p.strategy(p.ctx, root)

	for cid := range cids {
		isTracking, err := p.tracker.IsTracking(cid)
		if err != nil {
			return err
		}
		if isTracking {
			continue
		}
		if err := p.queue.Enqueue(cid); err != nil {
			return err
		}
	}

	return nil
}

func (p *Provider) Unprovide(cid cid.Cid) error {
	return p.tracker.Untrack(cid)
}

// Announce to the world that a block is provided.
func (p *Provider) announce(cid cid.Cid) error {
	ctx, cancel := context.WithTimeout(p.ctx, provideOutgoingTimeout)
	defer cancel()
	fmt.Println("provider - announce - start - ", cid)
	if err := p.contentRouting.Provide(ctx, cid, true); err != nil {
		log.Warningf("Failed to provide cid: %s", err)
		return err
	}
	fmt.Println("provider - announce - end - ", cid)
	return nil
}

// Handle all outgoing cids by providing (announcing) them
func (p *Provider) handleAnnouncements() {
	for workers := 0; workers < provideOutgoingWorkerLimit; workers++ {
		go func() {
			for {
				select {
				case <-p.ctx.Done():
					return
				case entry := <-p.queue.Dequeue():
					// skip if already tracking
					isTracking, err := p.tracker.IsTracking(entry.cid)
					if err != nil {
						log.Warningf("Unable to check provider tracking on outgoing: %s, %s", entry.cid, err)
						continue
					}
					if isTracking {
						if err := entry.Complete(); err != nil {
							log.Warningf("Unable to complete queue entry when already tracking: %s, %s", entry.cid, err)
						}
						continue
					}

					// if not in blockstore, skip and stop tracking
					inBlockstore, err := p.blockstore.Has(entry.cid)
					if err != nil {
						log.Warningf("Unable to check for presence in blockstore: %s, %s", entry.cid, err)
						continue
					}
					if !inBlockstore {
						if err := p.tracker.Untrack(entry.cid); err != nil {
							log.Warningf("Unable to untrack: %s, %s", entry.cid, err)
						}
						if err := entry.Complete(); err != nil {
							log.Warningf("Unable to complete queue entry when untracking: %s, %s", entry.cid, err)
						}
						continue
					}

					// announce
					if err := p.announce(entry.cid); err != nil {
						log.Warningf("Unable to announce providing: %s, %s", entry.cid, err)
						// TODO: Maybe put these failures onto a failures queue?
						if err := entry.Complete(); err != nil {
							log.Warningf("Unable to complete queue entry for failure: %s, %s", entry.cid, err)
						}
						continue
					}

					// track entry
					if err := p.tracker.Track(entry.cid); err != nil {
						log.Warningf("Unable to track: %s, %s", entry.cid, err)
						continue
					}

					// remove entry from queue
					if err := entry.Complete(); err != nil {
						log.Warningf("Unable to complete queue entry for success: %s, %s", entry.cid, err)
						continue
					}
				}
			}
		}()
	}
}

