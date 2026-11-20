package worker

import (
	"sort"

	"github.com/bluesky-social/slagworks/internal/model"
)

// takeOutbox empties the outbox the way a global that commits and
// acknowledges every entry would, and returns what was queued, split by
// kind, in record order. It drains and acks until nothing is due, because
// the outbox releases the next entry of an op's ordered chain only once
// the one before it is acknowledged. Entries backed off after a rejection
// stay queued.
func takeOutbox(c *Cache) (obs []PendingOutbox, updates []model.OperationUpdate) {
	// Record order is the outbox's slice order; note it before draining.
	c.outbox.mu.Lock()
	pos := make(map[uint64]int, len(c.outbox.entries))
	for i, e := range c.outbox.entries {
		pos[e.seq] = i
	}
	c.outbox.mu.Unlock()

	var all []PendingOutbox
	for {
		entries := c.DrainOutbox()
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			c.AckOutbox(e)
		}
		all = append(all, entries...)
	}
	// Entries appended while draining were not in the outbox when it was
	// noted: they come after the rest, in append (seq) order.
	order := func(e PendingOutbox) int {
		if p, ok := pos[e.seq]; ok {
			return p
		}
		return len(pos) + int(e.seq)
	}
	sort.SliceStable(all, func(i, j int) bool { return order(all[i]) < order(all[j]) })
	for _, e := range all {
		if e.Update != nil {
			updates = append(updates, *e.Update)
		} else {
			obs = append(obs, e)
		}
	}
	return obs, updates
}
