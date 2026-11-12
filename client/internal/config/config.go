// Package config loads bench.yaml. loadgen reads hosts (the SFU address and
// Prometheus selectors), livekit/mediasoup ports (to build SFU URLs), and the
// loadgen section. No environment variables are read.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"sfubench/loadgen/internal/media"
)

type Config struct {
	Path string `yaml:"-"` // absolute path of the loaded file
	Root string `yaml:"-"` // directory of the file (repo root): results/ lives here

	Hosts struct {
		SFUIP                      string   `yaml:"sfu_ip"`
		GenIPs                     []string `yaml:"gen_ips"`
		NodeExporterMonitoringPort int      `yaml:"node_exporter_monitoring_port"`
	} `yaml:"hosts"`

	NIC struct {
		SFU NIC `yaml:"sfu"`
		Gen NIC `yaml:"gen"`
	} `yaml:"nic"`

	Monitoring struct {
		PrometheusURL string `yaml:"prometheus_url"`
	} `yaml:"monitoring"`

	LiveKit struct {
		URL          string `yaml:"url"` // ws:// or wss:// via HAProxy. Empty = ws://sfu_ip:http_port_base
		APIKey       string `yaml:"api_key"`
		APISecret    string `yaml:"api_secret"`
		HTTPPortBase int    `yaml:"http_port_base"`
	} `yaml:"livekit"`

	Mediasoup struct {
		Signaling struct {
			Listen string `yaml:"listen"`
			URL    string `yaml:"url"` // full ws:// or wss:// URL; overrides sfu_ip + listen
		} `yaml:"signaling"`
	} `yaml:"mediasoup"`

	Loadgen struct {
		// NamePrefix goes in front of every room and participant name, so several
		// generators can run the same plan against one SFU without colliding.
		// Set a different one in each generator's bench.yaml ("g1-", "g2-", …).
		NamePrefix string `yaml:"name_prefix"`
		// SharedRooms makes several generators fill the SAME rooms instead of
		// separate ones: every generator runs the same plan with the same
		// prefix, and shard "k/N" takes slice k of N. Generator 1/N creates the
		// rooms (publishers and speakers); the others add viewers to them.
		// Unset = the name_prefix isolation above.
		SharedRooms      *SharedRooms `yaml:"shared_rooms"`
		MonitoringPort   int          `yaml:"monitoring_port"`
		FreezeThreshold  Duration     `yaml:"freeze_threshold"`
		MaxInflightJoins int          `yaml:"max_inflight_joins"`
		ICELoopback      bool         `yaml:"ice_loopback"`
		ICELocalIPs      []string     `yaml:"ice_local_ips"`
		DebugSDP         bool         `yaml:"debug_sdp"`
		Media            Media        `yaml:"media"`
		Report           Report       `yaml:"report"`
	} `yaml:"loadgen"`
}

type SharedRooms struct {
	Prefix string `yaml:"prefix"` // room prefix, the same on every generator
	Shard  string `yaml:"shard"`  // "k/N"
	// ViewerDelay: shards 2..N wait this long before their first step so the
	// publishers on shard 1 are in the rooms first; shard 1 holds this much longer.
	ViewerDelay Duration `yaml:"viewer_delay"`

	K, N int `yaml:"-"`
}

type Media struct {
	Dir    string            `yaml:"dir"`
	Codec  string            `yaml:"codec"` // vp8 (simulcast, default) or vp9 (spatial SVC, L<n>T1)
	Layers []media.LayerSpec `yaml:"layers"`
}

type NIC struct {
	Name   string   `yaml:"name"`
	Slaves []string `yaml:"slaves"`
}

type Report struct {
	Warmup    Duration `yaml:"warmup"`
	MaxGenCPU float64  `yaml:"max_gen_cpu"`
	// MaxGenDrop: % of packets received on the generator that its kernel may
	// drop (softnet backlog + UDP receive buffers) before a step is INVALID.
	MaxGenDrop float64 `yaml:"max_gen_drop"`
	// MaxGenSendLag: publisher tick -> write p99 above this means the generator
	// can't keep its own pace (bench_gen_send_lag_seconds): step INVALID.
	MaxGenSendLag Duration `yaml:"max_gen_send_lag"`
	SLO           struct {
		VideoDelayP95  Duration `yaml:"video_delay_p95"`
		AudioDelayP95  Duration `yaml:"audio_delay_p95"`
		VideoJitterP95 Duration `yaml:"video_jitter_p95"`
		LossRatio      float64  `yaml:"loss_ratio"`
		FreezeRatio    float64  `yaml:"freeze_ratio"`
		JoinSuccess    float64  `yaml:"join_success"`
		MinVideoRatio  float64  `yaml:"min_video_ratio"` // received kbps >= ratio * requested layer kbps
	} `yaml:"slo"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w (cp bench.yaml.example bench.yaml)", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path, _ = filepath.Abs(path)
	c.Root = filepath.Dir(c.Path)
	c.defaults()
	return &c, c.validate()
}

func (c *Config) defaults() {
	if c.Hosts.NodeExporterMonitoringPort == 0 {
		c.Hosts.NodeExporterMonitoringPort = 9100
	}
	if c.Monitoring.PrometheusURL == "" {
		c.Monitoring.PrometheusURL = "http://127.0.0.1:9090"
	}
	lk := &c.LiveKit
	if lk.HTTPPortBase == 0 {
		lk.HTTPPortBase = 7880
	}
	if c.Mediasoup.Signaling.Listen == "" {
		c.Mediasoup.Signaling.Listen = "0.0.0.0:8080"
	}
	lg := &c.Loadgen
	if lg.MonitoringPort == 0 {
		lg.MonitoringPort = 9200
	}
	if lg.Media.Dir == "" {
		lg.Media.Dir = "media"
	}
	if len(lg.Media.Layers) == 0 {
		lg.Media.Layers = []media.LayerSpec{
			{RID: "q", Width: 320, Height: 180, Kbps: 150},
			{RID: "h", Width: 640, Height: 360, Kbps: 400},
			{RID: "f", Width: 1280, Height: 720, Kbps: 1500},
		}
	}
	if lg.FreezeThreshold == 0 {
		lg.FreezeThreshold = Duration(200 * time.Millisecond)
	}
	if lg.MaxInflightJoins == 0 {
		lg.MaxInflightJoins = 200
	}
	if sr := lg.SharedRooms; sr != nil && sr.ViewerDelay == 0 {
		sr.ViewerDelay = Duration(20 * time.Second)
	}
	r := &lg.Report
	if r.Warmup == 0 {
		r.Warmup = Duration(60 * time.Second)
	}
	if r.MaxGenCPU == 0 {
		r.MaxGenCPU = 70
	}
	if r.MaxGenDrop == 0 {
		r.MaxGenDrop = 0.5
	}
	if r.MaxGenSendLag == 0 {
		r.MaxGenSendLag = Duration(20 * time.Millisecond)
	}
	s := &r.SLO
	if s.VideoDelayP95 == 0 {
		s.VideoDelayP95 = Duration(100 * time.Millisecond)
	}
	if s.AudioDelayP95 == 0 {
		s.AudioDelayP95 = Duration(60 * time.Millisecond)
	}
	if s.VideoJitterP95 == 0 {
		s.VideoJitterP95 = Duration(30 * time.Millisecond)
	}
	if s.LossRatio == 0 {
		s.LossRatio = 0.01
	}
	if s.FreezeRatio == 0 {
		s.FreezeRatio = 0.01
	}
	if s.JoinSuccess == 0 {
		s.JoinSuccess = 0.995
	}
	if s.MinVideoRatio == 0 {
		s.MinVideoRatio = 0.8
	}
}

var namePrefixRE = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

func (c *Config) validate() error {
	if c.Hosts.SFUIP == "" {
		return fmt.Errorf("%s: hosts.sfu_ip is required", c.Path)
	}
	if len(c.Hosts.GenIPs) == 0 {
		return fmt.Errorf("%s: hosts.gen_ips is required", c.Path)
	}
	if len(c.Loadgen.ICELocalIPs) == 0 {
		return fmt.Errorf("%s: loadgen.ice_local_ips is required", c.Path)
	}
	// Names end up in URLs, tokens and log lines: keep them plain.
	if !namePrefixRE.MatchString(c.Loadgen.NamePrefix) {
		return fmt.Errorf("%s: loadgen.name_prefix %q: use letters, digits, '-' and '_' only", c.Path, c.Loadgen.NamePrefix)
	}
	if sr := c.Loadgen.SharedRooms; sr != nil {
		if !namePrefixRE.MatchString(sr.Prefix) {
			return fmt.Errorf("%s: loadgen.shared_rooms.prefix %q: use letters, digits, '-' and '_' only", c.Path, sr.Prefix)
		}
		k, n, err := parseShard(sr.Shard)
		if err != nil {
			return fmt.Errorf("%s: loadgen.shared_rooms.shard: %w", c.Path, err)
		}
		sr.K, sr.N = k, n
	}
	return nil
}

// parseShard reads "k/N" with 1 <= k <= N.
func parseShard(s string) (k, n int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "/")
	if ok {
		k, err = strconv.Atoi(a)
		if err == nil {
			n, err = strconv.Atoi(b)
		}
	}
	if !ok || err != nil || n < 1 || k < 1 || k > n {
		return 0, 0, fmt.Errorf("want k/N with 1 <= k <= N (e.g. 1/2), got %q", s)
	}
	return k, n, nil
}

// ---- derived values ---------------------------------------------------------------

// SFUURLs returns the signaling URL clients connect to.
// LiveKit is one URL: livekit.url when set (HAProxy), otherwise ws://sfu_ip:http_port_base.
func (c *Config) SFUURLs(sfu string) ([]string, error) {
	switch sfu {
	case "mediasoup":
		if raw := strings.TrimSpace(c.Mediasoup.Signaling.URL); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
				return nil, fmt.Errorf("mediasoup.signaling.url: want ws:// or wss://host/ws, got %q", raw)
			}
			return []string{raw}, nil
		}
		_, port, err := net.SplitHostPort(c.Mediasoup.Signaling.Listen)
		if err != nil {
			return nil, fmt.Errorf("mediasoup.signaling.listen: %w", err)
		}
		return []string{fmt.Sprintf("ws://%s/ws", net.JoinHostPort(c.Hosts.SFUIP, port))}, nil
	case "livekit":
		if raw := strings.TrimSpace(c.LiveKit.URL); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
				return nil, fmt.Errorf("livekit.url: want ws:// or wss://host, got %q", raw)
			}
			return []string{raw}, nil
		}
		return []string{fmt.Sprintf("ws://%s", net.JoinHostPort(c.Hosts.SFUIP, strconv.Itoa(c.LiveKit.HTTPPortBase)))}, nil
	}
	return nil, fmt.Errorf("unknown sfu %q (livekit | mediasoup)", sfu)
}

// SFUNodeSelector / GenNodeSelector select node_exporter series in Prometheus.
func (c *Config) SFUNodeSelector() string {
	return fmt.Sprintf(`instance="%s"`, net.JoinHostPort(c.Hosts.SFUIP, strconv.Itoa(c.Hosts.NodeExporterMonitoringPort)))
}

func (c *Config) GenNodeSelector() string {
	var p []string
	for _, ip := range c.Hosts.GenIPs {
		p = append(p, strings.ReplaceAll(net.JoinHostPort(ip, strconv.Itoa(c.Hosts.NodeExporterMonitoringPort)), ".", `\\.`))
	}
	return fmt.Sprintf(`instance=~"%s"`, strings.Join(p, "|"))
}

// SFUNetDevRegex matches the SFU data NIC and its bond slaves.
func (c *Config) SFUNetDevRegex() string { return netDevRegex(c.NIC.SFU) }

// GenNetDevRegex matches the generator data NIC only (not its bond slaves, which
// would count every packet twice).
func (c *Config) GenNetDevRegex() string { return netDevRegex(NIC{Name: c.NIC.Gen.Name}) }

func netDevRegex(n NIC) string {
	devs := append([]string{n.Name}, n.Slaves...)
	var out []string
	for _, d := range devs {
		if d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return ".+"
	}
	return strings.Join(out, "|")
}

// ResultsDir is <repo root>/results.
func (c *Config) ResultsDir() string { return filepath.Join(c.Root, "results") }

// ---- Duration: "200ms", "60s" ---------------------------------------------------------

type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}
