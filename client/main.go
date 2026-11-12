// loadgen runs one plan against an SFU. Settings come from bench.yaml.
//
//	loadgen -config bench.yaml mediasoup plans/00-smoke.yaml
//	loadgen -config bench.yaml livekit plans/02-group-6.yaml
//
// One process runs the whole plan. A second machine is another invocation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"sfubench/loadgen/internal/client"
	"sfubench/loadgen/internal/config"
	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/plan"
	"sfubench/loadgen/internal/report"
	"sfubench/loadgen/internal/rooms"
	"sfubench/loadgen/internal/stats"
)

func main() {
	// The LiveKit SDK logger setup (client package init) redirects the standard
	// logger into zap at warn level, which would hide all of our progress lines.
	log.SetOutput(os.Stderr)
	log.SetPrefix("")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	configPath := flag.String("config", "", "path to bench.yaml")
	snapshotPath := flag.String("sfu-snapshot", "", "path to the SFU host's preflight sfu.json (host/91-sfu-snapshot.sh), recorded in run.json")
	flag.Usage = usage
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -config is required")
		usage()
	}
	args := flag.Args()
	if len(args) != 2 && len(args) != 3 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "report" {
		rereport(ctx, mustConfig(*configPath), args[1])
		return
	}
	sfu := args[0]
	if sfu != "mediasoup" && sfu != "livekit" {
		log.Fatalf("sfu must be mediasoup or livekit, got %q", sfu)
	}
	run := ""
	if len(args) == 3 {
		run = args[2]
	}
	runPlan(ctx, mustConfig(*configPath), sfu, args[1], run, *snapshotPath)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: loadgen -config <bench.yaml> [-sfu-snapshot <sfu.json>] <mediasoup|livekit> <plan.yaml> [run]")
	fmt.Fprintln(os.Stderr, "       loadgen -config <bench.yaml> report <results/run>   (rebuild report.md from Prometheus)")
	os.Exit(2)
}

func mustConfig(path string) *config.Config {
	c, err := config.Load(path)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("config %s", c.Path)
	return c
}

// loadMedia reads the IVF files this run publishes. codec (plan.codec) may
// override loadgen.media.codec.
func loadMedia(c *config.Config, codec string) *media.Video {
	lg := c.Loadgen
	client.DebugSDP = lg.DebugSDP
	dir := lg.Media.Dir
	if !filepath.IsAbs(dir) {
		for _, base := range []string{filepath.Join(c.Root, "client"), exeDir()} {
			if p := filepath.Join(base, dir); base != "" && exists(p) {
				dir = p
				break
			}
		}
	}
	if codec == "" {
		codec = lg.Media.Codec
	}
	v, err := media.LoadVideo(dir, codec, lg.Media.Layers)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("media %s, ICE local IPs %v, loopback %v", dir, lg.ICELocalIPs, lg.ICELoopback)
	return v
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return ""
}

// runPlan executes the plan in this process.
func runPlan(ctx context.Context, c *config.Config, sfu, planPath, run, snapshot string) {
	urls, err := c.SFUURLs(sfu)
	if err != nil {
		log.Fatal(err)
	}
	p, err := plan.LoadPlan(planPath)
	if err != nil {
		log.Fatal(err)
	}
	if run == "" {
		run = fmt.Sprintf("%s-%s-%s", sfu, p.Name, time.Now().Format("20060102-1504"))
		if pre := strings.TrimRight(c.Loadgen.NamePrefix, "-_"); pre != "" {
			run += "-" + pre
		}
		if sr := c.Loadgen.SharedRooms; sr != nil && sr.N > 1 {
			run += fmt.Sprintf("-shard%dof%d", sr.K, sr.N)
		}
	}

	lg := c.Loadgen
	outDir := filepath.Join(c.ResultsDir(), run)
	go serveMetrics(lg.MonitoringPort)
	log.Printf("GOMAXPROCS=%d", runtime.GOMAXPROCS(0))
	log.Printf("run %s: sfu=%s url=%s -> %s", run, sfu, urls[0], outDir)
	video := loadMedia(c, p.Codec)
	log.Printf("subscribed layers: high %d%%, medium %d%%, low %d%%", p.Subscribe.High, p.Subscribe.Medium, p.Subscribe.Low)
	opts := rooms.Options{
		SFU: sfu, URL: urls[0], APIKey: c.LiveKit.APIKey, APISecret: c.LiveKit.APISecret,
		RunName: run, NamePrefix: lg.NamePrefix, Plan: *p, Media: video,
		Recv:            stats.Options{FreezeThreshold: lg.FreezeThreshold.D()},
		IncludeLoopback: lg.ICELoopback, LocalIPs: lg.ICELocalIPs, MaxInflightJoin: lg.MaxInflightJoins,
		OutDir: outDir, Loadgen: loadgenInfo(), SFUSnapshot: snapshot,
	}
	if sr := lg.SharedRooms; sr != nil {
		opts.Shared, opts.RoomPrefix, opts.Shard, opts.ViewerDelay = true, sr.Prefix, plan.Shard{K: sr.K, N: sr.N}, sr.ViewerDelay.D()
	}
	session, err := rooms.New(opts)
	if err != nil {
		log.Fatal(err)
	}
	recs, err := session.Start(ctx)
	if err != nil {
		log.Printf("run ended: %v", err)
	}
	for _, r := range recs {
		log.Printf("step %d target=%d connected=%d failed=%d dropped=%d window=%s..%s", r.Step, r.Target, r.Connected, r.Failed, r.Disconn,
			r.Settled.Format(time.TimeOnly), r.End.Format(time.TimeOnly))
	}
	log.Printf("results: %s", outDir)
	printLocalSummary()
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	writeReport(ctx, c, outDir, expectedKbps(p, video))
}

// rereport rebuilds the report of a finished run with the current SLO, using
// the plan saved in its run.json. Prometheus must still hold the run's data.
func rereport(ctx context.Context, c *config.Config, dir string) {
	p, err := readRunPlan(dir)
	if err != nil {
		log.Fatal(err)
	}
	writeReport(ctx, c, dir, expectedKbps(p, loadMedia(c, p.Codec)))
}

// readRunPlan returns the plan saved in dir/run.json.
func readRunPlan(dir string) (*plan.Plan, error) {
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	var meta struct {
		Plan plan.Plan `json:"plan"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("%s/run.json: %v", dir, err)
	}
	return &meta.Plan, nil
}

func serveMetrics(port int) {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	log.Printf("metrics on %s", addr)
	mux := http.NewServeMux()
	mux.Handle("/", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("metrics: %v", err)
	}
}

// expectedKbps is the video bitrate each requested quality should receive,
// for the qualities this plan actually uses.
func expectedKbps(p *plan.Plan, v *media.Video) map[string]float64 {
	share := map[string]int{plan.QualityLow: p.Subscribe.Low, plan.QualityMedium: p.Subscribe.Medium, plan.QualityHigh: p.Subscribe.High}
	out := map[string]float64{}
	for q, pct := range share {
		if pct > 0 {
			out[q] = v.ExpectedKbps(q)
		}
	}
	return out
}

func writeReport(ctx context.Context, c *config.Config, dir string, expected map[string]float64) {
	r := c.Loadgen.Report
	md, err := report.Report(ctx, report.ReportConfig{
		PromURL: c.Monitoring.PrometheusURL, SFUNode: c.SFUNodeSelector(), GenNodes: c.GenNodeSelector(),
		NetDev: c.SFUNetDevRegex(), GenDev: c.GenNetDevRegex(), Dir: dir, Warmup: r.Warmup.D(), ExpectedKbps: expected,
		SLO: report.SLO{
			VideoDelayP95: r.SLO.VideoDelayP95.D().Seconds(), AudioDelayP95: r.SLO.AudioDelayP95.D().Seconds(),
			JitterP95: r.SLO.VideoJitterP95.D().Seconds(), LossRatio: r.SLO.LossRatio, FreezeRatio: r.SLO.FreezeRatio,
			JoinSuccess: r.SLO.JoinSuccess, MinVideoRatio: r.SLO.MinVideoRatio, MaxGenCPU: r.MaxGenCPU, MaxGenDrop: r.MaxGenDrop,
			MaxGenSendLag: r.MaxGenSendLag.D().Seconds(),
		},
	})
	if err != nil {
		log.Printf("report: %v", err)
		return
	}
	fmt.Print(md)
}

// printLocalSummary prints a few totals from the in-process registry (for `run`).
func printLocalSummary() {
	mfs, err := promGather()
	if err != nil {
		return
	}
	fmt.Println("\n--- local totals ---")
	for _, name := range []string{"bench_join_duration_seconds", "bench_ttff_seconds", "bench_pub_frames_total", "bench_pub_rtcp_total",
		"bench_sub_packets_total", "bench_sub_bytes_total", "bench_sub_lost_packets_total", "bench_sub_e2e_delay_seconds",
		"bench_sub_frames_total", "bench_sub_freezes_total", "bench_sub_layer_frames_total", "bench_join_failures_total",
		"bench_disconnects_total", "bench_rejoins_total", "bench_events_total", "bench_recovery_seconds",
		"bench_recovery_missed_total", "bench_layer_switch_seconds", "bench_layer_switch_missed_total", "bench_unmute_heard_seconds",
		"bench_sub_track_fps", "bench_sub_track_loss_ratio", "bench_sub_track_kbps_ratio", "bench_sub_freeze_duration_seconds",
		"bench_gen_send_lag_seconds"} {
		if s, ok := mfs[name]; ok {
			fmt.Println(s)
		}
	}
}
