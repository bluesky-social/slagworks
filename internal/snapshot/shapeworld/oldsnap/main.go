// Command oldsnap is the differential-testing half of the snapshot
// production-shape property tests: it builds site snapshots with whatever
// version of internal/snapshot it is compiled against. Copy
// internal/snapshot/shapeworld into a checkout of an older release, build
// this command there, and point SLAGWORKS_SHAPE_OLDSNAP at the binary; the
// tests on the current tree then send it the same worlds they build
// themselves and compare the answers.
//
// It reads one JSON request per line on stdin and writes one JSON response
// per line on stdout. It performs no I/O beyond that.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/snapshot"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

type request struct {
	World   *shapeworld.World     `json:"world"`
	Store   *shapeworld.StoreData `json:"store"`
	Slug    string                `json:"slug"`
	Shuffle bool                  `json:"shuffle"`
	Seed    int64                 `json:"seed"`
}

type response struct {
	OK       bool                `json:"ok"`
	Err      string              `json:"err,omitempty"`
	Snapshot *model.SiteSnapshot `json:"snapshot,omitempty"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			fmt.Fprintln(os.Stderr, "oldsnap: decode request:", err)
			os.Exit(1)
		}
		nb := &shapeworld.NB{W: req.World, Shuffle: req.Shuffle, Seed: req.Seed}
		st := &shapeworld.Store{D: req.Store, Shuffle: req.Shuffle, Seed: req.Seed}
		d := snapshot.NewDispatcher(nb, st, shapeworld.Sink{})
		var resp response
		snap, err := d.BuildSnapshot(context.Background(), req.Slug)
		if err != nil {
			resp.Err = err.Error()
		} else {
			resp.OK = true
			resp.Snapshot = &snap
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "oldsnap: encode response:", err)
			os.Exit(1)
		}
		if err := out.Flush(); err != nil {
			os.Exit(1)
		}
	}
}
