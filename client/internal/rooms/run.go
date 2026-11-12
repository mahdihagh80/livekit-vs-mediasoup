// Package rooms opens the plan's rooms and joins their participants.
package rooms

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/plan"
	"sfubench/loadgen/internal/stats"
)

type Options struct {
	SFU        string
	URL        string
	APIKey     string
	APISecret  string
	RunName    string
	NamePrefix string // in front of every participant name, and room name unless RoomPrefix is set
	// Shared rooms: several generators fill the same rooms. RoomPrefix is the
	// same on all of them; Shard says which slice of the people this one runs.
	Shared          bool
	RoomPrefix      string
	Shard           plan.Shard
	ViewerDelay     time.Duration
	Plan            plan.Plan
	Media           *media.Video
	Recv            stats.Options
	IncludeLoopback bool
	LocalIPs        []string
	MaxInflightJoin int
	OutDir          string
	Loadgen         map[string]any // run.json "loadgen"
	SFUSnapshot     string         // the SFU host's sfu.json, copied into run.json
}

// Run is one plan. It owns the rooms; each room owns its participants.
type Run struct {
	opts Options

	list   []*Room
	queue  chan joinReq
	runCtx context.Context
	stop   context.CancelFunc
}

func New(opts Options) (*Run, error) {
	if err := opts.Plan.Validate(); err != nil {
		return nil, err
	}
	if opts.URL == "" {
		return nil, fmt.Errorf("missing sfu url")
	}
	if opts.OutDir == "" {
		return nil, fmt.Errorf("missing results dir")
	}
	if opts.MaxInflightJoin == 0 {
		opts.MaxInflightJoin = 200
	}
	if !opts.Shared {
		opts.RoomPrefix, opts.Shard, opts.ViewerDelay = opts.NamePrefix, plan.Shard{K: 1, N: 1}, 0
	}
	if opts.Shard.N < 1 || opts.Shard.K < 1 || opts.Shard.K > opts.Shard.N {
		return nil, fmt.Errorf("bad shard %d/%d", opts.Shard.K, opts.Shard.N)
	}
	return &Run{opts: opts}, nil
}

// Start creates the rooms. Each room creates its participants and joins them
// on the plan's schedule: `add` people every `every`, publishers first, until
// the room is full, then `hold`, then everyone leaves.
func (r *Run) Start(ctx context.Context) ([]Step, error) {
	if err := os.MkdirAll(r.opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	if err := r.writeMeta(time.Now()); err != nil {
		log.Fatalf("run.json: %v", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	r.runCtx = ctx
	r.stop = cancel
	r.queue = make(chan joinReq, 1<<20)
	p := &r.opts.Plan
	sess := session{
		SFU: r.opts.SFU, URL: r.opts.URL, APIKey: r.opts.APIKey, APISecret: r.opts.APISecret,
		Plan: p, Media: r.opts.Media, Recv: r.opts.Recv,
		IncludeLoopback: r.opts.IncludeLoopback, LocalIPs: r.opts.LocalIPs, queue: r.queue,
	}
	stats.Links.Reset()
	for _, g := range p.Groups(r.opts.RoomPrefix, r.opts.NamePrefix) {
		r.list = append(r.list, newRoom(sess, g, r.opts.Shard))
	}
	if err := r.writeLinks(); err != nil {
		log.Printf("links.jsonl: %v", err)
	}
	stats.M.InitProfiles(p.Profiles())
	types, recovery := eventLabels(p)
	stats.M.InitEvents(types, recovery)
	stats.M.Info.Reset()
	stats.M.Info.WithLabelValues(r.opts.SFU, p.Name, r.opts.RunName).Set(1)
	go r.joiner()
	go r.sweep(ctx)
	go r.rotateSpeakers(ctx)
	go r.churn(ctx)
	if r.opts.Shared {
		owned := 0
		for _, rm := range r.list {
			owned += len(rm.people)
		}
		log.Printf("rooms=%d url=%s shared rooms, room_prefix=%q name_prefix=%q shard %d/%d (%d of %d people on this shard)",
			len(r.list), r.opts.URL, r.opts.RoomPrefix, r.opts.NamePrefix, r.opts.Shard.K, r.opts.Shard.N, owned, p.Rooms*p.MaxParticipants)
	} else {
		log.Printf("rooms=%d url=%s name_prefix=%q", len(r.list), r.opts.URL, r.opts.NamePrefix)
	}
	defer func() {
		log.Printf("disconnecting")
		r.Stop()
	}()

	// Let Prometheus scrape the zero-valued series once before the first join,
	// otherwise increase() misses the first step's joins.
	log.Printf("waiting 15s for a baseline scrape")
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
	}

	// Shared rooms: the viewer shards start viewer_delay after shard 1 so its
	// publishers are in first, and shard 1 holds that much longer, so all
	// shards leave together. Events fire at the same wall-clock time everywhere.
	viewerShard := r.opts.Shard.N > 1 && r.opts.Shard.K != 1
	extraHold, eventDelay := time.Duration(0), time.Duration(0)
	if r.opts.Shard.N > 1 {
		if viewerShard {
			log.Printf("viewer shard: waiting %s for shard 1 to create the rooms", r.opts.ViewerDelay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(r.opts.ViewerDelay):
			}
		} else {
			extraHold, eventDelay = r.opts.ViewerDelay, r.opts.ViewerDelay
		}
	}

	var records []Step
	for stepIdx, target := range p.Levels() {
		step := stepIdx + 1
		rec := Step{Step: step, Target: target, Start: time.Now()}
		wait := p.Every.D()
		final := target == p.Rooms*p.MaxParticipants
		if final {
			wait = p.Hold.D() + extraHold
		}
		if p.AddRooms > 0 {
			log.Printf("== step %d: %d participants (%d full rooms), then wait %s ==", step, target, target/p.MaxParticipants, wait)
		} else {
			log.Printf("== step %d: %d participants (%d/room), then wait %s ==", step, target, target/p.Rooms, wait)
		}
		local, err := r.grow(target, step)
		if err != nil {
			return records, err
		}
		// Shared rooms: steps.csv counts this shard's people only, so its
		// target is this shard's share (the report sums the shards).
		if r.opts.Shard.N > 1 {
			rec.Target = local
		}
		rec.Settled = time.Now()
		if final && len(p.Events) > 0 {
			go r.events(ctx, eventDelay)
		}
		select {
		case <-ctx.Done():
			return records, ctx.Err()
		case <-time.After(wait):
		}
		rec.End = time.Now()
		_, joining, connected, failed, dropped := r.Counts()
		rec.Connected, rec.Failed, rec.Disconn = connected, failed, dropped
		records = append(records, rec)
		if err := writeSteps(filepath.Join(r.opts.OutDir, "steps.csv"), records); err != nil {
			log.Printf("write steps.csv: %v", err)
		}
		log.Printf("step %d done: joining=%d connected=%d failed=%d disconnected=%d", step, joining, connected, failed, dropped)
		if lost := rec.Failed + rec.Disconn; lost > int64(local)/2 {
			log.Printf("more than half of participants failed or dropped; stopping")
			break
		}
	}
	return records, nil
}

// grow brings every room up to n/len(rooms) participants, or with addRooms,
// fills the first n/maxParticipants rooms. It returns how many people this
// generator runs now. Each room joins its publishers before its subscribers.
func (r *Run) grow(n, step int) (int, error) {
	per := 0
	if p := r.opts.Plan; p.AddRooms > 0 {
		full := n / p.MaxParticipants
		for i, rm := range r.list {
			if i < full {
				rm.growTo(p.MaxParticipants)
			}
		}
		per = p.MaxParticipants
	} else {
		if len(r.list) == 0 || n%len(r.list) != 0 {
			return 0, fmt.Errorf("target %d is not a whole number of participants per room", n)
		}
		per = n / len(r.list)
		for _, rm := range r.list {
			rm.growTo(per)
		}
	}
	total := r.localPeople()
	stats.M.Target.Set(float64(total))
	stats.M.Step.Set(float64(step))
	if r.opts.Shard.N > 1 {
		log.Printf("step %d: %d participants in each room, %d of them on this shard", step, per, total)
	} else {
		log.Printf("step %d: %d participants, %d in each room", step, total, per)
	}
	return total, nil
}

// localPeople is how many people this generator runs now.
func (r *Run) localPeople() int {
	n := 0
	for _, rm := range r.list {
		n += rm.local()
	}
	return n
}

func (r *Run) joiner() {
	ctx := r.runCtx
	inflight := make(chan struct{}, r.opts.MaxInflightJoin)
	for {
		var req joinReq
		select {
		case <-ctx.Done():
			return
		case req = <-r.queue:
		}
		select {
		case <-ctx.Done():
			return
		case inflight <- struct{}{}:
		}
		go func() {
			var release sync.Once
			joined := func() { release.Do(func() { <-inflight }) }
			defer joined()
			req.who.room.join(ctx, req, joined)
		}()
	}
}

// Stop disconnects every room, at most 500 people per second.
func (r *Run) Stop() {
	var all []*person
	for _, rm := range r.list {
		all = append(all, rm.leaveAll()...)
	}
	for n, who := range all {
		who.room.leave(who)
		if n%500 == 499 {
			time.Sleep(time.Second)
		}
	}
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	for _, st := range []string{stateJoining, stateConnected, stateReconnecting, stateFailed, stateDisconnected} {
		stats.M.Participants.WithLabelValues(st).Set(0)
	}
	stats.M.Target.Set(0)
	stats.M.Step.Set(0)
	stats.M.Info.Reset()
	stats.Links.Reset()
}

// writeLinks writes links.jsonl to the run directory: one line per person on
// this generator with their network profile and, for shaped people, when the
// shaper clock started. With that the dip windows can be rebuilt offline:
// dip k (from 0) is [start_ms+phase_ms+k*dip_every_ms, +dip_for_ms).
func (r *Run) writeLinks() error {
	f, err := os.Create(filepath.Join(r.opts.OutDir, "links.jsonl"))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, rm := range r.list {
		for _, ps := range rm.people {
			line := map[string]any{"peer": ps.Identity, "room": rm.name, "profile": ps.Profile,
				"publish": ps.Publish || ps.Speaker, "quality": ps.Quality, "shaped": ps.shaper != nil}
			if ps.shaper != nil {
				np := ps.shaper.Profile()
				start, phase := ps.shaper.Clock()
				line["kbps"] = np.Kbps
				line["dip_kbps"] = np.Dip.Kbps
				line["dip_for_ms"] = np.Dip.For.Milliseconds()
				line["dip_every_ms"] = np.Dip.Every.Milliseconds()
				if np.Burst.Loss > 0 {
					line["burst_loss"] = np.Burst.Loss
					line["burst_ms"] = np.Burst.Length.Milliseconds()
				}
				line["start_ms"] = start.UnixMilli()
				line["phase_ms"] = phase.Milliseconds()
				line["delay_ms"] = np.Delay.Milliseconds()
				line["jitter_ms"] = np.Jitter.Milliseconds()
				line["loss"] = np.Loss
			}
			if err := enc.Encode(line); err != nil {
				f.Close()
				return err
			}
		}
	}
	return f.Close()
}

func (r *Run) Counts() (target int, joining, connected, failed, disconnected int64) {
	return r.localPeople(),
		int64(stats.M.Count(stateJoining)),
		int64(stats.M.Count(stateConnected)),
		int64(stats.M.Count(stateFailed)),
		int64(stats.M.Count(stateDisconnected))
}
