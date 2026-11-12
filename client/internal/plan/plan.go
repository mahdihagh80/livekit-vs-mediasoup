package plan

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Plan is one load. The same file runs against either SFU.
//
// Rooms are named room_1, room_2, … (prefixed with loadgen.name_prefix when
// several generators share the SFU). Each room holds at most maxParticipants
// people, named par_<room>_<n>, and `publishers` of them send audio and/or
// video. Every `every`, each room gains `add` people, publishers of that
// batch first, until the room is full. The run then stays for `hold` and
// disconnects.
//
// With addRooms > 0 the ramp is by rooms instead: every `every`, the next
// addRooms rooms join in full (publishers first) and `add` is ignored.
//
// subscribe sets which simulcast/SVC layer each participant asks the SFU for,
// as percentages of participants (default 20/50/30). Real clients request the
// layer that fits the tile, so most of them don't pull the top layer.
//
// Optional behaviour (all off by default, so older plans run unchanged):
//
//   - codec: vp8 or vp9, overrides loadgen.media.codec for this plan.
//   - screenShare: publisher 1 of each room also shares a screen (a second
//     video track from the same media).
//   - speakers: audio-only publishers after the video publishers (a classroom's
//     students with a mic). `unmuted` of them talk at a time and the talker set
//     rotates every `rotate`; the others send DTX silence.
//   - churn: every churn.every, churn.percent of the viewers leave and rejoin.
//   - events: disturbances during hold (see Event).
//   - network: network conditions on what people receive (see NetProfile),
//     for the viewers only unless networkTarget says otherwise.
//   - networkTarget: whose subscribe connection gets a network profile: viewers
//     (default), publishers or all. With all, a 1:1 call (everyone publishes)
//     can be impaired; the viewers keep the same profiles as with viewers.
//     A publisher's publish connection is always clean.
//   - rejoin: rejoin after an unexpected disconnect (default: on when the plan
//     has events, off otherwise).
//   - steps: people added per room at each step, for uneven ramps, e.g.
//     [1503, 1500, 1000, 1000]; replaces add and must add up to maxParticipants.
//   - red: LiveKit only. Audio goes as audio/red (RFC 2198) like a browser's:
//     publishers send each Opus frame plus the previous one, and every client
//     accepts audio/red, so the SFU forwards RED to all viewers.
type Plan struct {
	Name            string   `yaml:"name" json:"name"`
	Rooms           int      `yaml:"rooms" json:"rooms"`
	MaxParticipants int      `yaml:"maxParticipants" json:"maxParticipants"`
	Publishers      int      `yaml:"publishers" json:"publishers"`
	Audio           bool     `yaml:"audio" json:"audio"`
	Video           bool     `yaml:"video" json:"video"`
	Add             int      `yaml:"add" json:"add"`
	AddRooms        int      `yaml:"addRooms" json:"addRooms,omitempty"`
	Steps           []int    `yaml:"steps" json:"steps,omitempty"`
	Every           Duration `yaml:"every" json:"every"`
	Hold            Duration `yaml:"hold" json:"hold"`
	Subscribe       LayerMix `yaml:"subscribe" json:"subscribe"`

	Codec       string       `yaml:"codec" json:"codec,omitempty"`
	ScreenShare bool         `yaml:"screenShare" json:"screenShare,omitempty"`
	Speakers    int          `yaml:"speakers" json:"speakers,omitempty"`
	Unmuted     int          `yaml:"unmuted" json:"unmuted,omitempty"`
	Rotate      Duration     `yaml:"rotate" json:"rotate,omitempty"`
	Churn       Churn        `yaml:"churn" json:"churn,omitempty"`
	Events      []Event      `yaml:"events" json:"events,omitempty"`
	Network     []NetProfile `yaml:"network" json:"network,omitempty"`
	NetTarget   string       `yaml:"networkTarget" json:"networkTarget,omitempty"`
	Rejoin      *bool        `yaml:"rejoin" json:"rejoin,omitempty"`
	Red         bool         `yaml:"red" json:"red,omitempty"`
}

// Churn makes viewers leave and come back while the run goes on.
type Churn struct {
	Percent int      `yaml:"percent" json:"percent"` // of the viewers present, per tick
	Every   Duration `yaml:"every" json:"every"`
}

// Event types.
const (
	EventNetLoss            = "netloss"            // all traffic of the targets is dropped for `for`
	EventMassRejoin         = "massRejoin"         // the targets leave and rejoin at once
	EventPublisherReconnect = "publisherReconnect" // publishers leave and rejoin at once
)

// Event is one disturbance, `at` after hold starts.
type Event struct {
	At      Duration `yaml:"at" json:"at"`
	Type    string   `yaml:"type" json:"type"`
	Percent int      `yaml:"percent" json:"percent"` // of the targets (default 100)
	For     Duration `yaml:"for" json:"for,omitempty"`
	// Target is viewers (everyone but the video publishers, the default),
	// publishers or all. publisherReconnect always targets publishers.
	Target string `yaml:"target" json:"target,omitempty"`
}

// NetProfile is one network condition, applied inside the load generator to
// percent of the people networkTarget names (default: the viewers, people who
// don't publish video). It shapes only the person's subscribe connection, on
// the downlink (SFU -> person). The publish connection (what a publisher sends,
// and the SFU's NACK/PLI/congestion feedback on it) stays clean; only outage
// events reach it.
type NetProfile struct {
	Name    string   `yaml:"name" json:"name"`
	Percent int      `yaml:"percent" json:"percent"`
	Delay   Duration `yaml:"delay" json:"delay,omitempty"`   // one-way, on the downlink
	Jitter  Duration `yaml:"jitter" json:"jitter,omitempty"` // delay varies by ±jitter
	Loss    float64  `yaml:"loss" json:"loss,omitempty"`     // downlink loss, percent
	Kbps    int      `yaml:"kbps" json:"kbps,omitempty"`     // downlink rate limit
	Dip     Dip      `yaml:"dip" json:"dip,omitempty"`
	Burst   Burst    `yaml:"burst" json:"burst,omitempty"`
}

// Burst is bursty downlink loss (a time-based Gilbert-Elliott model): the link
// alternates between a good state (no loss) and a bad state (every packet
// lost). Bad periods last `length` on average (exponential), good periods are
// sized so the link is bad for `loss` percent of the time, which is the
// average packet loss. Independent of the uniform `loss`.
type Burst struct {
	Loss   float64  `yaml:"loss" json:"loss"`
	Length Duration `yaml:"length" json:"length"`
}

// Dip drops the downlink to kbps for `for`, every `every`.
type Dip struct {
	Kbps  int      `yaml:"kbps" json:"kbps"`
	For   Duration `yaml:"for" json:"for"`
	Every Duration `yaml:"every" json:"every"`
}

// CleanProfile is the profile name of people without a network condition.
const CleanProfile = "clean"

// LayerMix is the share (percent) of participants that subscribe to each layer.
type LayerMix struct {
	High   int `yaml:"high" json:"high"`
	Medium int `yaml:"medium" json:"medium"`
	Low    int `yaml:"low" json:"low"`
}

// Subscribed video quality a participant requests from the SFU.
const (
	QualityLow    = "low"
	QualityMedium = "medium"
	QualityHigh   = "high"
)

// Qualities lists the quality labels, low -> high.
var Qualities = []string{QualityLow, QualityMedium, QualityHigh}

// Person is one participant. Publishers come first, then speakers, then viewers.
type Person struct {
	Identity string
	Pos      int // 1-based place in the room's join order
	Publish  bool
	Speaker  bool   // publishes audio only (plan.speakers)
	Quality  string // layer this participant subscribes to (QualityLow/Medium/High)
	Profile  string // network profile name (CleanProfile if none)
}

// Group is one room's membership.
type Group struct {
	Name   string
	People []Person
}

func (p *Plan) Validate() error {
	if p.Rooms < 1 {
		return fmt.Errorf("plan %q: rooms must be >= 1", p.Name)
	}
	if p.MaxParticipants < 1 || p.Publishers < 0 || p.Publishers > p.MaxParticipants {
		return fmt.Errorf("plan %q: need maxParticipants >= 1 and 0 <= publishers <= maxParticipants", p.Name)
	}
	if !p.Audio && !p.Video {
		return fmt.Errorf("plan %q: audio and/or video must be true", p.Name)
	}
	if p.AddRooms < 0 || p.AddRooms > p.Rooms {
		return fmt.Errorf("plan %q: need 0 <= addRooms <= rooms", p.Name)
	}
	if len(p.Steps) > 0 {
		if p.AddRooms > 0 {
			return fmt.Errorf("plan %q: steps cannot be used with addRooms", p.Name)
		}
		sum := 0
		for _, n := range p.Steps {
			if n < 1 {
				return fmt.Errorf("plan %q: every entry of steps must be >= 1", p.Name)
			}
			sum += n
		}
		if sum != p.MaxParticipants {
			return fmt.Errorf("plan %q: steps add up to %d, maxParticipants is %d", p.Name, sum, p.MaxParticipants)
		}
	} else if p.Add < 1 && p.AddRooms == 0 {
		return fmt.Errorf("plan %q: add must be >= 1", p.Name)
	}
	if p.Every == 0 {
		p.Every = Duration(10 * time.Minute)
	}
	if p.Hold == 0 {
		p.Hold = Duration(10 * time.Minute)
	}
	s := &p.Subscribe
	if *s == (LayerMix{}) {
		*s = LayerMix{High: 20, Medium: 50, Low: 30}
	}
	if s.High < 0 || s.Medium < 0 || s.Low < 0 || s.High+s.Medium+s.Low != 100 {
		return fmt.Errorf("plan %q: subscribe high+medium+low must be 100, got %d+%d+%d", p.Name, s.High, s.Medium, s.Low)
	}
	return p.validateExtras()
}

var profileNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (p *Plan) validateExtras() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("plan %q: "+format, append([]any{p.Name}, args...)...)
	}
	if p.Codec != "" && p.Codec != "vp8" && p.Codec != "vp9" {
		return bad("codec: want vp8 or vp9, got %q", p.Codec)
	}
	if p.ScreenShare && (!p.Video || p.Publishers < 1) {
		return bad("screenShare needs video and at least one publisher")
	}
	if p.Speakers < 0 || p.Publishers+p.Speakers > p.MaxParticipants {
		return bad("need speakers >= 0 and publishers+speakers <= maxParticipants")
	}
	if p.Speakers > 0 {
		if !p.Audio {
			return bad("speakers need audio: true")
		}
		if p.Unmuted == 0 {
			p.Unmuted = p.Speakers
		}
		if p.Unmuted < 0 || p.Unmuted > p.Speakers {
			return bad("need 0 <= unmuted <= speakers")
		}
		if p.Rotate == 0 {
			p.Rotate = Duration(20 * time.Second)
		}
		if p.Rotate < Duration(time.Second) {
			return bad("rotate must be >= 1s")
		}
	} else if p.Unmuted != 0 {
		return bad("unmuted needs speakers")
	}
	if c := p.Churn; c != (Churn{}) {
		if c.Percent < 1 || c.Percent > 100 || c.Every < Duration(time.Second) {
			return bad("churn needs 1 <= percent <= 100 and every >= 1s")
		}
	}
	for i := range p.Events {
		e := &p.Events[i]
		if e.Percent == 0 {
			e.Percent = 100
		}
		if e.Percent < 0 || e.Percent > 100 {
			return bad("events[%d]: percent must be 1..100", i)
		}
		if e.At < 0 || e.At >= p.Hold {
			return bad("events[%d]: at must be within hold (0 <= at < %s)", i, p.Hold.D())
		}
		switch e.Type {
		case EventNetLoss:
			if e.For <= 0 {
				return bad("events[%d]: netloss needs for > 0", i)
			}
		case EventMassRejoin:
		case EventPublisherReconnect:
			if e.Target != "" && e.Target != "publishers" {
				return bad("events[%d]: publisherReconnect always targets publishers", i)
			}
			e.Target = "publishers"
		default:
			return bad("events[%d]: type must be %s, %s or %s", i, EventNetLoss, EventMassRejoin, EventPublisherReconnect)
		}
		switch e.Target {
		case "":
			e.Target = "viewers"
		case "viewers", "publishers", "all":
		default:
			return bad("events[%d]: target must be viewers, publishers or all", i)
		}
	}
	switch p.NetTarget {
	case "":
		p.NetTarget = "viewers"
	case "viewers", "publishers", "all":
	default:
		return bad("networkTarget must be viewers, publishers or all")
	}
	if len(p.Network) > 0 {
		sum := 0
		seen := map[string]bool{}
		for i, n := range p.Network {
			if !profileNameRE.MatchString(n.Name) || seen[n.Name] {
				return bad("network[%d]: name %q must be unique, letters/digits/-/_", i, n.Name)
			}
			seen[n.Name] = true
			if n.Percent < 0 || n.Loss < 0 || n.Loss > 100 || n.Kbps < 0 || n.Delay < 0 || n.Jitter < 0 {
				return bad("network[%d]: negative value or loss > 100", i)
			}
			if d := n.Dip; d != (Dip{}) && (d.Kbps < 1 || d.For <= 0 || d.Every <= d.For) {
				return bad("network[%d]: dip needs kbps >= 1 and 0 < for < every", i)
			}
			if b := n.Burst; b != (Burst{}) && (b.Loss <= 0 || b.Loss >= 100 || b.Length <= 0) {
				return bad("network[%d]: burst needs 0 < loss < 100 and length > 0", i)
			}
			sum += n.Percent
		}
		if sum != 100 {
			return bad("network percents must sum to 100, got %d", sum)
		}
	}
	return nil
}

// RejoinOn reports whether dropped participants rejoin on their own.
func (p *Plan) RejoinOn() bool {
	if p.Rejoin != nil {
		return *p.Rejoin
	}
	return len(p.Events) > 0
}

// NeedsShaper reports whether participants need an in-process network shaper.
func (p *Plan) NeedsShaper() bool {
	if len(p.Network) > 0 {
		return true
	}
	for _, e := range p.Events {
		if e.Type == EventNetLoss {
			return true
		}
	}
	return false
}

// Profiles lists every network profile name people can have.
func (p *Plan) Profiles() []string {
	if len(p.Network) == 0 {
		return []string{CleanProfile}
	}
	out := []string{CleanProfile}
	for _, n := range p.Network {
		if n.Name != CleanProfile {
			out = append(out, n.Name)
		}
	}
	return out
}

// NetProfile returns the profile with that name (zero value for CleanProfile).
func (p *Plan) NetProfile(name string) NetProfile {
	for _, n := range p.Network {
		if n.Name == name {
			return n
		}
	}
	return NetProfile{Name: CleanProfile}
}

// netTargeted reports whether networkTarget gives who a network profile.
// Speakers (audio only) count as viewers, as for events.
func (p *Plan) netTargeted(who Person) bool {
	switch p.NetTarget {
	case "all":
		return true
	case "publishers":
		return who.Publish
	}
	return !who.Publish
}

// profileAt picks the network profile for participant number i, like
// qualityAt but with a different stride so layer and network are independent.
func (p *Plan) profileAt(i int) string {
	pct := (i*53 + 11) % 100
	for _, n := range p.Network {
		if pct < n.Percent {
			return n.Name
		}
		pct -= n.Percent
	}
	return CleanProfile
}

// qualityAt picks the subscribed layer for participant number i (0-based,
// across all rooms). 37 is coprime with 100, so every 100 consecutive
// participants hit each percentile exactly once: the split is exact per 100
// and spread evenly over rooms and join order. Both SFUs get the same split.
func (p *Plan) qualityAt(i int) string {
	switch pct := (i * 37) % 100; {
	case pct < p.Subscribe.High:
		return QualityHigh
	case pct < p.Subscribe.High+p.Subscribe.Medium:
		return QualityMedium
	default:
		return QualityLow
	}
}

// Levels is the total participant count after each add.
// Each room grows by `add` until maxParticipants, or with addRooms, the
// number of full rooms grows by addRooms until all rooms are in. With steps,
// each room grows by steps[i] people at step i (uneven ramps).
func (p *Plan) Levels() []int {
	var out []int
	if len(p.Steps) > 0 {
		n := 0
		for _, s := range p.Steps {
			n += s
			out = append(out, n*p.Rooms)
		}
		return out
	}
	if p.AddRooms > 0 {
		for r := p.AddRooms; r < p.Rooms; r += p.AddRooms {
			out = append(out, r*p.MaxParticipants)
		}
		return append(out, p.Rooms*p.MaxParticipants)
	}
	for n := p.Add; n < p.MaxParticipants; n += p.Add {
		out = append(out, n*p.Rooms)
	}
	out = append(out, p.MaxParticipants*p.Rooms)
	return out
}

// Groups builds the rooms in order: <roomPrefix>room_1, <roomPrefix>room_2, …
// People in a room are <idPrefix>par_<room>_<n>: publishers, then speakers,
// then viewers. With one generator both prefixes are loadgen.name_prefix,
// which keeps rooms from different generators apart. With shared rooms the
// room prefix is the same on every generator and only the identities differ.
func (p *Plan) Groups(roomPrefix, idPrefix string) []Group {
	out := make([]Group, p.Rooms)
	for r := 1; r <= p.Rooms; r++ {
		g := Group{Name: fmt.Sprintf("%sroom_%d", roomPrefix, r)}
		for n := 1; n <= p.MaxParticipants; n++ {
			i := (r-1)*p.MaxParticipants + n - 1
			who := Person{
				Identity: fmt.Sprintf("%spar_%d_%d", idPrefix, r, n),
				Pos:      n,
				Publish:  n <= p.Publishers,
				Speaker:  n > p.Publishers && n <= p.Publishers+p.Speakers,
				Quality:  p.qualityAt(i),
				Profile:  CleanProfile,
			}
			if p.netTargeted(who) {
				who.Profile = p.profileAt(i)
			}
			g.People = append(g.People, who)
		}
		out[r-1] = g
	}
	return out
}

// Shard is generator k of n sharing the same rooms (1 <= k <= n).
type Shard struct{ K, N int }

// Owns reports whether this shard runs the person. Shard 1 runs every
// publisher and speaker (it creates the rooms); the viewers of each room are
// dealt out round-robin, so every shard gets an even slice of every room.
func (p *Plan) Owns(s Shard, who Person) bool {
	if s.N <= 1 {
		return true
	}
	first := p.Publishers + p.Speakers
	if who.Pos <= first {
		return s.K == 1
	}
	return (who.Pos-first-1)%s.N+1 == s.K
}

func LoadPlan(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, p.Validate()
}

// Duration accepts "10m" / "90s" in YAML and JSON.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	*d = Duration(v)
	return err
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", time.Duration(d).String())), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}
