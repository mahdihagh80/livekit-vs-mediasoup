package rooms

import (
	"context"
	"log"
	"math/rand/v2"
	"time"

	"sfubench/loadgen/internal/plan"
	"sfubench/loadgen/internal/stats"
)

// eventLabels lists the bench_events_total types and bench_recovery_seconds
// events this plan can produce, so their series exist from the start.
func eventLabels(p *plan.Plan) (types, recovery []string) {
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	if p.RejoinOn() {
		add(&recovery, "drop")
	}
	if p.Churn.Percent > 0 {
		add(&recovery, "churn")
	}
	for _, e := range p.Events {
		add(&types, e.Type)
		switch e.Type {
		case plan.EventNetLoss:
			add(&recovery, netlossLabel(e))
		case plan.EventMassRejoin:
			add(&recovery, "mass_rejoin")
		case plan.EventPublisherReconnect:
			add(&recovery, "publisher_reconnect")
		}
	}
	return types, recovery
}

func netlossLabel(e plan.Event) string { return "netloss_" + e.For.D().String() }

// sweep times out recoveries that never came.
func (r *Run) sweep(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, rm := range r.list {
				for _, who := range rm.people {
					who.mon.Sweep(now)
				}
			}
		}
	}
}

// rotateSpeakers keeps `unmuted` speakers of each room talking; the talking
// set moves on by `unmuted` every `rotate`, like a class taking turns.
func (r *Run) rotateSpeakers(ctx context.Context) {
	p := &r.opts.Plan
	if p.Speakers == 0 {
		return
	}
	set := func(k int) {
		for _, rm := range r.list {
			n := len(rm.speakers)
			for i, who := range rm.speakers {
				who.talk.Store(((i-k*p.Unmuted)%n+n)%n < p.Unmuted)
			}
		}
	}
	set(0)
	t := time.NewTicker(p.Rotate.D())
	defer t.Stop()
	for k := 1; ; k++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			set(k)
		}
	}
}

// churn makes churn.percent of the connected viewers leave and rejoin every
// churn.every. Fractions carry over, so small rooms still churn on average.
func (r *Run) churn(ctx context.Context) {
	c := r.opts.Plan.Churn
	if c.Percent == 0 {
		return
	}
	t := time.NewTicker(c.Every.D())
	defer t.Stop()
	carry := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var viewers []*person
		for _, rm := range r.list {
			rm.mu.Lock()
			for _, who := range rm.people {
				if who.want && who.state == stateConnected && !who.Publish && !who.Speaker {
					viewers = append(viewers, who)
				}
			}
			rm.mu.Unlock()
		}
		carry += float64(len(viewers)) * float64(c.Percent) / 100
		n := min(int(carry), len(viewers))
		carry -= float64(n)
		rand.Shuffle(len(viewers), func(i, j int) { viewers[i], viewers[j] = viewers[j], viewers[i] })
		done := 0
		for _, who := range viewers[:n] {
			if who.room.rejoin(who, "churn", "churn") {
				done++
			}
		}
		if done > 0 {
			log.Printf("churn: %d of %d viewers rejoin", done, len(viewers))
		}
	}
}

// events fires the plan's events, each `at` after hold start (+delay).
func (r *Run) events(ctx context.Context, delay time.Duration) {
	start := time.Now().Add(delay)
	for _, e := range r.opts.Plan.Events {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(start.Add(e.At.D()))):
			}
			r.fire(e)
		}()
	}
}

func (r *Run) fire(e plan.Event) {
	var targets []*person
	for _, rm := range r.list {
		rm.mu.Lock()
		var match []*person
		for _, who := range rm.people {
			if who.want && targeted(e.Target, who.Person) {
				match = append(match, who)
			}
		}
		rm.mu.Unlock()
		targets = append(targets, pick(match, e.Percent)...)
	}
	stats.M.Events.WithLabelValues(e.Type).Inc()
	n := 0
	switch e.Type {
	case plan.EventNetLoss:
		end := time.Now().Add(e.For.D())
		for _, who := range targets {
			if who.shaper == nil {
				continue
			}
			who.shaper.Outage(e.For.D())
			who.mon.Disturbed(netlossLabel(e), end, false)
			n++
		}
	case plan.EventMassRejoin:
		for _, who := range targets {
			if who.room.rejoin(who, "mass_rejoin", "mass_rejoin") {
				n++
			}
		}
	case plan.EventPublisherReconnect:
		// Viewers see it as a publisher_reconnect recovery (stats.Monitor);
		// the publisher's own receive side is not measured.
		for _, who := range targets {
			if who.room.rejoin(who, "publisher_reconnect", "") {
				n++
			}
		}
	}
	log.Printf("event %s (target %s, %d%%, for %s): %d participants", e.Type, e.Target, e.Percent, e.For.D(), n)
}

// pick returns percent of list, spread evenly over it.
func pick(list []*person, percent int) []*person {
	if percent >= 100 {
		return list
	}
	var out []*person
	for j := range list {
		if (j+1)*percent/100 > j*percent/100 {
			out = append(out, list[j])
		}
	}
	return out
}
