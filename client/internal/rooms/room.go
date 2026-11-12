package rooms

import (
	"context"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"sfubench/loadgen/internal/client"
	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
	"sfubench/loadgen/internal/plan"
	"sfubench/loadgen/internal/stats"
)

// session is what a room needs to connect its participants.
type session struct {
	SFU             string
	URL             string
	APIKey          string
	APISecret       string
	Plan            *plan.Plan
	Media           *media.Video
	Recv            stats.Options
	IncludeLoopback bool
	LocalIPs        []string
	queue           chan<- joinReq
}

// Rejoin after an unexpected drop: up to maxAttempts joins, backoff between them.
const maxAttempts = 5

var backoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}

func newRoom(sess session, g plan.Group, shard plan.Shard) *Room {
	p := sess.Plan
	rm := &Room{sess: sess, name: g.Name}
	for _, who := range g.People {
		if !p.Owns(shard, who) {
			continue
		}
		ps := &person{room: rm, Person: who, mon: stats.NewMonitor(p.Video)}
		if needsShaper(p, who) {
			np := p.NetProfile(who.Profile)
			ps.shaper = netem.NewShaper(netem.Profile{
				Name: np.Name, Delay: np.Delay.D(), Jitter: np.Jitter.D(), Loss: np.Loss / 100, Kbps: np.Kbps,
				Dip:   netem.Dip{Kbps: np.Dip.Kbps, For: np.Dip.For.D(), Every: np.Dip.Every.D()},
				Burst: netem.Burst{Loss: np.Burst.Loss / 100, Length: np.Burst.Length.D()},
			})
		}
		stats.Links.Set(who.Identity, who.Profile, ps.shaper)
		if who.Speaker {
			ps.talk = &atomic.Bool{}
			rm.speakers = append(rm.speakers, ps)
		}
		rm.people = append(rm.people, ps)
	}
	return rm
}

// needsShaper: the person has a network profile or a netloss event can hit them.
func needsShaper(p *plan.Plan, who plan.Person) bool {
	if who.Profile != plan.CleanProfile {
		return true
	}
	for _, e := range p.Events {
		if e.Type == plan.EventNetLoss && targeted(e.Target, who) {
			return true
		}
	}
	return false
}

// targeted reports whether an event with this target applies to who.
func targeted(target string, who plan.Person) bool {
	switch target {
	case "publishers":
		return who.Publish
	case "all":
		return true
	}
	return !who.Publish
}

// Room owns one room's participants (the ones this generator runs) and when
// they join or leave.
type Room struct {
	sess     session
	name     string
	people   []*person // in join order
	speakers []*person

	mu      sync.Mutex
	present int // people with Pos <= present should be in the room
}

// Participant states; each has a bench_participants gauge except left and
// queued-for-first-join, which counts as joining.
const (
	stateNone         = ""
	stateJoining      = "joining"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateFailed       = "failed"       // first join failed (stays counted)
	stateDisconnected = "disconnected" // dropped for good (stays counted)
	stateLeft         = "left"
)

type person struct {
	room *Room
	plan.Person
	mon    *stats.Monitor
	shaper *netem.Shaper // nil: no network impairment
	talk   *atomic.Bool  // speakers: unmuted now

	// guarded by room.mu
	want      bool // should be in the room
	gen       int  // bumped on every leave/rejoin; stale join requests are skipped
	state     string
	p         client.Participant
	cancel    context.CancelFunc
	attempt   int  // failed rejoin attempts since the last drop
	connected bool // connected at least once
}

type joinReq struct {
	who *person
	gen int
}

// setStateLocked moves who to state s and keeps the gauges in step. failed and
// disconnected stay counted after the person leaves, like before.
func (who *person) setStateLocked(s string) {
	if who.state == s {
		return
	}
	switch who.state {
	case stateJoining, stateConnected, stateReconnecting:
		stats.M.Participants.WithLabelValues(who.state).Dec()
	case stateFailed, stateDisconnected:
		if s != stateLeft {
			stats.M.Participants.WithLabelValues(who.state).Dec()
		}
	}
	switch s {
	case stateJoining, stateConnected, stateReconnecting, stateFailed, stateDisconnected:
		stats.M.Participants.WithLabelValues(s).Inc()
	}
	who.state = s
}

// growTo makes people with Pos <= n join and people above n leave.
func (rm *Room) growTo(n int) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, who := range rm.people {
		switch {
		case who.Pos <= n && !who.want:
			who.want, who.attempt = true, 0
			who.gen++
			who.setStateLocked(stateJoining)
			rm.sess.queue <- joinReq{who, who.gen}
		case who.Pos > n && who.want:
			rm.leaveLocked(who)
		}
	}
	rm.present = n
}

// local is how many people this generator runs in the room now.
func (rm *Room) local() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	n := 0
	for _, who := range rm.people {
		if who.want {
			n++
		}
	}
	return n
}

func (rm *Room) spec(who *person) client.Spec {
	s := rm.sess
	return client.Spec{
		SFU: s.SFU, URL: s.URL, APIKey: s.APIKey, APISecret: s.APISecret,
		Room: rm.name, Identity: who.Identity, Publish: who.Publish || who.Speaker, Quality: who.Quality,
		Audio: s.Plan.Audio, Video: s.Plan.Video && !who.Speaker, Media: s.Media, Recv: s.Recv,
		ScreenShare: s.Plan.ScreenShare && who.Publish && who.Pos == 1, Red: s.Plan.Red,
		Talk: who.talk, Profile: who.Profile, Shaper: who.shaper, Mon: who.mon,
		OnResume:        func() { stats.M.Rejoins.WithLabelValues("sdk_resume").Inc() },
		IncludeLoopback: s.IncludeLoopback, LocalIPs: s.LocalIPs,
	}
}

func (rm *Room) join(runCtx context.Context, req joinReq, joined func()) {
	who := req.who
	rm.mu.Lock()
	if !who.want || who.gen != req.gen {
		rm.mu.Unlock()
		return
	}
	who.mon.NewSession()
	p := client.New(rm.spec(who))
	ctx, cancel := context.WithCancel(runCtx)
	who.p, who.cancel = p, cancel
	rm.mu.Unlock()

	start := time.Now()
	jctx, jcancel := context.WithTimeout(ctx, 30*time.Second)
	err := p.Join(jctx)
	jcancel()
	joined()

	rm.mu.Lock()
	if !who.want || who.gen != req.gen { // left or rejoined meanwhile; they closed p
		rm.mu.Unlock()
		p.Close()
		return
	}
	if err != nil {
		stats.M.JoinSeconds.WithLabelValues("fail").Observe(time.Since(start).Seconds())
		if ctx.Err() == nil {
			log.Printf("join %s/%s failed: %v", rm.name, who.Identity, err)
		}
		who.p = nil
		if who.connected && rm.sess.Plan.RejoinOn() {
			rm.retryLocked(who, p)
		} else {
			who.setStateLocked(stateFailed)
			go p.Close()
		}
		rm.mu.Unlock()
		return
	}
	stats.M.JoinSeconds.WithLabelValues("ok").Observe(time.Since(start).Seconds())
	who.setStateLocked(stateConnected)
	who.connected, who.attempt = true, 0
	rm.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-p.Done():
		rm.mu.Lock()
		if who.want && who.gen == req.gen {
			stats.M.Disconnects.Inc()
			log.Printf("%s/%s disconnected", rm.name, who.Identity)
			who.p = nil
			if rm.sess.Plan.RejoinOn() {
				stats.M.Rejoins.WithLabelValues("drop").Inc()
				if !who.mon.Pending() { // a netloss already waits for this person's media
					who.mon.Disturbed("drop", time.Now(), true)
				}
				rm.retryLocked(who, p)
			} else {
				who.setStateLocked(stateDisconnected)
				go p.Close()
			}
		}
		rm.mu.Unlock()
	}
}

// retryLocked closes p and joins who again after a backoff, or gives up
// after maxAttempts.
func (rm *Room) retryLocked(who *person, p client.Participant) {
	if who.attempt >= maxAttempts {
		log.Printf("%s/%s: giving up after %d rejoin attempts", rm.name, who.Identity, who.attempt)
		who.setStateLocked(stateDisconnected)
		go p.Close()
		return
	}
	d := backoff[min(who.attempt, len(backoff)-1)]
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	who.attempt++
	who.gen++
	who.setStateLocked(stateReconnecting)
	req := joinReq{who, who.gen}
	go func() {
		p.Close()
		time.Sleep(d)
		rm.sess.queue <- req
	}()
}

// rejoin makes a connected person leave and join again (churn, mass rejoin,
// publisher reconnect). event (may be empty) is the recovery label.
func (rm *Room) rejoin(who *person, reason, event string) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if !who.want || who.state != stateConnected {
		return false
	}
	stats.M.Rejoins.WithLabelValues(reason).Inc()
	if event != "" {
		who.mon.Disturbed(event, time.Now(), true)
	}
	old, cancel := who.p, who.cancel
	who.p, who.attempt = nil, 0
	who.gen++
	who.setStateLocked(stateJoining)
	req := joinReq{who, who.gen}
	cancel()
	go func() {
		if old != nil {
			old.Close() // before the new join, so the SFU never sees the identity twice
		}
		rm.sess.queue <- req
	}()
	return true
}

func (rm *Room) leaveLocked(who *person) {
	who.want = false
	who.gen++
	who.setStateLocked(stateLeft)
	who.mon.Forget()
	if who.cancel != nil {
		who.cancel()
	}
	if who.p != nil {
		go who.p.Close()
		who.p = nil
	}
}

// leaveAll returns everyone who should be in the room and marks the room empty.
func (rm *Room) leaveAll() []*person {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	var out []*person
	for _, who := range rm.people {
		if who.want {
			out = append(out, who)
		}
	}
	rm.present = 0
	return out
}

func (rm *Room) leave(who *person) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if who.want {
		rm.leaveLocked(who)
	}
}
