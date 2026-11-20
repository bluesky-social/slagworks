package httpboot

import (
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// takeOutbox empties a site worker cache's outbox the way a global that
// commits and acknowledges every entry would, and returns what was queued,
// split by kind. It drains and acks until nothing is due, because the
// outbox hands out an op's next update only once the one before it is
// acknowledged.
func takeOutbox(c *worker.Cache) (obs []worker.PendingOutbox, updates []model.OperationUpdate) {
	for {
		entries := c.DrainOutbox()
		if len(entries) == 0 {
			return obs, updates
		}
		for _, e := range entries {
			c.AckOutbox(e)
			if e.Update != nil {
				updates = append(updates, *e.Update)
			} else {
				obs = append(obs, e)
			}
		}
	}
}
