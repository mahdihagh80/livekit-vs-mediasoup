package plan

import (
	"fmt"
	"testing"
	"time"
)

func TestGroupsPerRoom(t *testing.T) {
	p := &Plan{Name: "t", Rooms: 2, MaxParticipants: 4, Publishers: 2, Audio: true, Video: true, Add: 2, Every: 1, Hold: 1}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	groups := p.Groups("", "")
	if len(groups) != 2 || groups[0].Name != "room_1" || groups[1].Name != "room_2" {
		t.Fatalf("rooms %+v", groups)
	}
	for ri, g := range groups {
		if len(g.People) != 4 {
			t.Fatalf("room %s has %d people", g.Name, len(g.People))
		}
		for i, person := range g.People {
			want := fmt.Sprintf("par_%d_%d", ri+1, i+1)
			if person.Identity != want || person.Publish != (i < 2) {
				t.Fatalf("room %s person %d: %+v", g.Name, i, person)
			}
		}
	}
	if levels := p.Levels(); len(levels) != 2 || levels[0] != 4 || levels[1] != 8 {
		t.Fatalf("levels %v", levels)
	}
}

func TestLevelsAddRooms(t *testing.T) {
	p := &Plan{Name: "t", Rooms: 5000, MaxParticipants: 2, Publishers: 2, Video: true, AddRooms: 1000}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if levels := p.Levels(); fmt.Sprint(levels) != "[2000 4000 6000 8000 10000]" {
		t.Fatalf("levels %v", levels)
	}
	p = &Plan{Name: "t", Rooms: 5, MaxParticipants: 2, Publishers: 2, Video: true, AddRooms: 2}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if levels := p.Levels(); fmt.Sprint(levels) != "[4 8 10]" {
		t.Fatalf("levels %v", levels)
	}
}

func TestGroupsPrefix(t *testing.T) {
	p := &Plan{Name: "t", Rooms: 2, MaxParticipants: 2, Publishers: 2, Audio: true, Add: 2, Every: 1, Hold: 1}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	g := p.Groups("g2-", "g2-")
	if g[1].Name != "g2-room_2" || g[1].People[0].Identity != "g2-par_2_1" {
		t.Fatalf("prefixed names %+v", g[1])
	}
}

func TestSubscribeMix(t *testing.T) {
	for _, tc := range []struct {
		rooms, max, add int
		mix             LayerMix
	}{
		{1000, 2, 2, LayerMix{}},
		{100, 6, 3, LayerMix{}},
		{2, 2000, 200, LayerMix{}},
		{50, 20, 2, LayerMix{High: 100}},
		{100, 10, 5, LayerMix{High: 10, Medium: 30, Low: 60}},
	} {
		p := &Plan{Name: "t", Rooms: tc.rooms, MaxParticipants: tc.max, Publishers: 1, Video: true, Add: tc.add, Subscribe: tc.mix}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		want := map[string]float64{QualityHigh: float64(p.Subscribe.High), QualityMedium: float64(p.Subscribe.Medium), QualityLow: float64(p.Subscribe.Low)}
		groups := p.Groups("", "")
		// Every ramp step (only the first `per` people of each room are present)
		// should be within 3 points of the requested split.
		for _, level := range p.Levels() {
			per := level / p.Rooms
			got := map[string]float64{}
			for _, g := range groups {
				for _, person := range g.People[:per] {
					got[person.Quality]++
				}
			}
			for q, w := range want {
				pct := 100 * got[q] / float64(level)
				if pct < w-3 || pct > w+3 {
					t.Errorf("rooms=%d max=%d step %d: %s = %.1f%%, want %.0f%%", tc.rooms, tc.max, level, q, pct, w)
				}
			}
		}
	}
	bad := &Plan{Name: "t", Rooms: 1, MaxParticipants: 2, Video: true, Add: 1, Subscribe: LayerMix{High: 50, Low: 40}}
	if bad.Validate() == nil {
		t.Fatal("want error for mix not summing to 100")
	}
}

func TestShardsSplitViewersEvenly(t *testing.T) {
	p := &Plan{Name: "t", Rooms: 3, MaxParticipants: 103, Publishers: 2, Speakers: 1, Audio: true, Video: true, Add: 10}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, g := range p.Groups("s-", "g1-") {
		owners := map[int]int{}
		for _, who := range g.People {
			n := 0
			for k := 1; k <= 2; k++ {
				if p.Owns(Shard{K: k, N: 2}, who) {
					n++
					owners[k]++
					if (who.Publish || who.Speaker) && k != 1 {
						t.Fatalf("%s: publisher/speaker on shard %d", who.Identity, k)
					}
				}
			}
			if n != 1 {
				t.Fatalf("%s owned by %d shards", who.Identity, n)
			}
		}
		// 3 on shard 1 + 50 viewers each.
		if owners[1] != 53 || owners[2] != 50 {
			t.Fatalf("room %s split %v", g.Name, owners)
		}
	}
}

func TestNetworkProfilesSplit(t *testing.T) {
	p := &Plan{Name: "t", Rooms: 10, MaxParticipants: 101, Publishers: 1, Video: true, Add: 101,
		Network: []NetProfile{{Name: "good", Percent: 70}, {Name: "bad", Percent: 30, Loss: 2}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, g := range p.Groups("", "") {
		for _, who := range g.People {
			if who.Publish && who.Profile != CleanProfile {
				t.Fatalf("publisher %s got profile %s", who.Identity, who.Profile)
			}
			if !who.Publish {
				got[who.Profile]++
			}
		}
	}
	if got["good"] < 690 || got["good"] > 710 || got["bad"] < 290 || got["bad"] > 310 {
		t.Fatalf("split %v", got)
	}
	if fmt.Sprint(p.Profiles()) != "[clean good bad]" {
		t.Fatalf("profiles %v", p.Profiles())
	}
}

// networkTarget all gives publishers profiles too, without moving any viewer
// to another profile; publishers gives them to the publishers only.
func TestNetworkTarget(t *testing.T) {
	mk := func(target string) *Plan {
		p := &Plan{Name: "t", Rooms: 50, MaxParticipants: 2, Publishers: 2, Video: true, Add: 2, NetTarget: target,
			Network: []NetProfile{{Name: "clean", Percent: 60}, {Name: "burst", Percent: 40, Burst: Burst{Loss: 5, Length: Duration(50 * time.Millisecond)}}}}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 1:1 calls: everyone publishes. Default (viewers): nobody is impaired.
	for _, g := range mk("").Groups("", "") {
		for _, who := range g.People {
			if who.Profile != CleanProfile {
				t.Fatalf("viewers target: publisher %s got %s", who.Identity, who.Profile)
			}
		}
	}
	got := map[string]int{}
	for _, g := range mk("all").Groups("", "") {
		for _, who := range g.People {
			got[who.Profile]++
		}
	}
	if got["burst"] != 40 || got["clean"] != 60 {
		t.Fatalf("all target split %v", got)
	}

	group := func(target string) []Group {
		p := &Plan{Name: "t", Rooms: 4, MaxParticipants: 30, Publishers: 6, Video: true, Add: 30, NetTarget: target,
			Network: []NetProfile{{Name: "clean", Percent: 50}, {Name: "bad", Percent: 50, Loss: 1}}}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		return p.Groups("", "")
	}
	v, a, pub := group("viewers"), group("all"), group("publishers")
	for r := range v {
		for i, who := range v[r].People {
			wa, wp := a[r].People[i], pub[r].People[i]
			if !who.Publish && (wa.Profile != who.Profile || wp.Profile != CleanProfile) {
				t.Fatalf("viewer %s: viewers=%s all=%s publishers=%s", who.Identity, who.Profile, wa.Profile, wp.Profile)
			}
			if who.Publish && (wa.Profile != wp.Profile || who.Profile != CleanProfile) {
				t.Fatalf("publisher %s: viewers=%s all=%s publishers=%s", who.Identity, who.Profile, wa.Profile, wp.Profile)
			}
		}
	}
}

func TestExtrasValidation(t *testing.T) {
	base := func() *Plan {
		return &Plan{Name: "t", Rooms: 1, MaxParticipants: 10, Publishers: 1, Audio: true, Video: true, Add: 10, Hold: Duration(10 * time.Minute)}
	}
	p := base()
	p.Events = []Event{{At: Duration(time.Minute), Type: EventNetLoss, For: Duration(5 * time.Second)}}
	if err := p.Validate(); err != nil || p.Events[0].Target != "viewers" || p.Events[0].Percent != 100 || !p.RejoinOn() || !p.NeedsShaper() {
		t.Fatalf("netloss defaults: %v %+v", err, p.Events[0])
	}
	for name, mut := range map[string]func(*Plan){
		"event after hold": func(p *Plan) { p.Events = []Event{{At: Duration(11 * time.Minute), Type: EventMassRejoin}} },
		"netloss no for":   func(p *Plan) { p.Events = []Event{{Type: EventNetLoss}} },
		"bad type":         func(p *Plan) { p.Events = []Event{{Type: "boom"}} },
		"network sum":      func(p *Plan) { p.Network = []NetProfile{{Name: "a", Percent: 50}} },
		"speakers no room": func(p *Plan) { p.Speakers = 10 },
		"codec":            func(p *Plan) { p.Codec = "h264" },
		"churn":            func(p *Plan) { p.Churn = Churn{Percent: 5} },
		"net target":       func(p *Plan) { p.NetTarget = "everyone" },
		"burst no length": func(p *Plan) {
			p.Network = []NetProfile{{Name: "a", Percent: 100, Burst: Burst{Loss: 5}}}
		},
		"burst loss 100": func(p *Plan) {
			p.Network = []NetProfile{{Name: "a", Percent: 100, Burst: Burst{Loss: 100, Length: Duration(time.Second)}}}
		},
		"dip": func(p *Plan) {
			p.Network = []NetProfile{{Name: "a", Percent: 100, Dip: Dip{Kbps: 100, For: 10, Every: 5}}}
		},
	} {
		p := base()
		mut(p)
		if p.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	p = base()
	p.Speakers = 4
	if err := p.Validate(); err != nil || p.Unmuted != 4 || p.Rotate.D() != 20*time.Second {
		t.Fatalf("speaker defaults: %v unmuted=%d rotate=%s", err, p.Unmuted, p.Rotate.D())
	}
}

func TestLevelsSteps(t *testing.T) {
	p := &Plan{Name: "s", Rooms: 2, MaxParticipants: 5003, Publishers: 3, Audio: true, Steps: []int{1503, 1500, 1000, 1000}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if levels := p.Levels(); fmt.Sprint(levels) != "[3006 6006 8006 10006]" {
		t.Fatalf("levels %v", levels)
	}
	bad := &Plan{Name: "s", Rooms: 1, MaxParticipants: 5003, Publishers: 3, Audio: true, Steps: []int{1500, 1500, 1000, 1000}}
	if err := bad.Validate(); err == nil {
		t.Fatal("steps that do not add up to maxParticipants were accepted")
	}
}
