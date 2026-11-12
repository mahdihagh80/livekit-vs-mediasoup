package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

// RecoveryTimeout: a disturbance with no media this long after it is missed.
const RecoveryTimeout = 60 * time.Second

// Monitor follows one simulated person across rejoins and measures how long
// media takes to come back after a disturbance (network outage, rejoin, a
// publisher reconnecting). Every track the person receives reports to it.
type Monitor struct {
	kind string // "video" when the plan has video, else "audio": the media that counts as recovered

	session       atomic.Int64
	pendingN      atomic.Int32
	lastDisturbed atomic.Int64 // unix nanos

	mu      sync.Mutex
	pending []disturbance
	tracks  map[string][]*trackEntry // publisher|kind
}

type disturbance struct {
	event      string
	at         time.Time
	newSession bool  // only media from a later session counts (rejoins)
	session    int64 // session when it was recorded
}

type trackEntry struct {
	last  atomic.Int64 // unix nanos of the latest packet
	ended atomic.Bool
}

// NewMonitor returns a monitor that waits for video (video=true) or audio.
func NewMonitor(video bool) *Monitor {
	k := "audio"
	if video {
		k = "video"
	}
	return &Monitor{kind: k, tracks: map[string][]*trackEntry{}}
}

// NewSession is called before each join attempt. Tracks of earlier sessions
// stop counting for rejoin disturbances and for publisher-reconnect matching.
func (m *Monitor) NewSession() int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	clear(m.tracks)
	m.mu.Unlock()
	return m.session.Add(1)
}

// Session is the current session number.
func (m *Monitor) Session() int64 {
	if m == nil {
		return 0
	}
	return m.session.Load()
}

// Disturbed records a disturbance at time at (it may be in the future, e.g.
// the end of an outage). newSession: only media of a later session resolves it.
func (m *Monitor) Disturbed(event string, at time.Time, newSession bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pending = append(m.pending, disturbance{event: event, at: at, newSession: newSession, session: m.session.Load()})
	m.pendingN.Store(int32(len(m.pending)))
	m.mu.Unlock()
	if n := at.UnixNano(); n > m.lastDisturbed.Load() {
		m.lastDisturbed.Store(n)
	}
}

// Pending reports whether a disturbance waits for media (cheap; per packet).
func (m *Monitor) Pending() bool { return m != nil && m.pendingN.Load() > 0 }

// media resolves pending disturbances. keyStart: the packet starts a video keyframe.
func (m *Monitor) media(now time.Time, kind string, keyStart bool, session int64) {
	if kind != m.kind || (kind == "video" && !keyStart) {
		return
	}
	m.mu.Lock()
	kept := m.pending[:0]
	for _, d := range m.pending {
		if now.Before(d.at) || (d.newSession && session <= d.session) {
			kept = append(kept, d)
			continue
		}
		M.Recovery.WithLabelValues(d.event).Observe(now.Sub(d.at).Seconds())
	}
	m.pending = kept
	m.pendingN.Store(int32(len(kept)))
	m.mu.Unlock()
}

// track registers a received track of publisher pub. A publisher that
// reconnects shows up as a new track that replaces its old one: the gap
// between the old one's last packet and the new one's first is observed as
// recovery{event="publisher_reconnect"}, unless this subscriber was disturbed
// itself in the last 60 s. With a source (track name) the key names exactly
// one track, so any older entry was replaced. Without one, camera and screen
// share have the same key: an older entry was replaced only if it ended or
// went quiet for over 1 s (a live one is the publisher's other video).
func (m *Monitor) track(pub, kind, source string, now time.Time) *trackEntry {
	e := &trackEntry{}
	e.last.Store(now.UnixNano())
	if m == nil || pub == "" {
		return e
	}
	key := pub + "|" + kind + "|" + source
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.tracks[key]
	for i, old := range list {
		gap := now.Sub(time.Unix(0, old.last.Load()))
		replaced := source != "" || old.ended.Load() || gap > time.Second
		if replaced && gap < RecoveryTimeout {
			if now.Sub(time.Unix(0, m.lastDisturbed.Load())) > RecoveryTimeout {
				M.Recovery.WithLabelValues("publisher_reconnect").Observe(gap.Seconds())
			}
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	m.tracks[key] = append(list, e)
	return e
}

// Sweep times out old disturbances and forgets long-quiet tracks. Call it
// every few seconds.
func (m *Monitor) Sweep(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.pending[:0]
	for _, d := range m.pending {
		if now.Sub(d.at) > RecoveryTimeout {
			M.RecoveryMissed.WithLabelValues(d.event).Inc()
			continue
		}
		kept = append(kept, d)
	}
	m.pending = kept
	m.pendingN.Store(int32(len(kept)))
	for key, list := range m.tracks {
		out := list[:0]
		for _, e := range list {
			if !(e.ended.Load() && now.Sub(time.Unix(0, e.last.Load())) > RecoveryTimeout) {
				out = append(out, e)
			}
		}
		if len(out) == 0 {
			delete(m.tracks, key)
		} else {
			m.tracks[key] = out
		}
	}
}

// Forget drops pending disturbances without counting them (the person left).
func (m *Monitor) Forget() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pending = m.pending[:0]
	m.pendingN.Store(0)
	m.mu.Unlock()
}
