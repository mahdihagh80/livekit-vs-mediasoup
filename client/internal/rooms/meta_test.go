package rooms

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sfubench/loadgen/internal/plan"
)

var testStart = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func writeSnap(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sfu.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteMeta(t *testing.T) {
	snap := writeSnap(t, `{"schema":1,"taken":"2026-11-05T14:00:00+03:30","sfu":"livekit","run":"r1","livekit":{"version":"1.13.11"}}`)
	r := &Run{opts: Options{
		SFU: "livekit", URL: "ws://sfu.test:7880", RunName: "r1", OutDir: t.TempDir(),
		Plan: plan.Plan{Name: "smoke"}, Shard: plan.Shard{K: 1, N: 1},
		Loadgen: map[string]any{"nack_generator": "sfubench-nackgen/1"}, SFUSnapshot: snap,
	}}
	if err := r.writeMeta(testStart); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.opts.OutDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run", "sfu", "url", "plan", "started", "name_prefix", "room_prefix", "shared_rooms", "shard",
		"schema", "loadgen", "sfu_snapshot", "sfu_snapshot_problems"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("run.json lacks %q", k)
		}
	}
	if meta["schema"] != 2.0 {
		t.Errorf("schema = %v", meta["schema"])
	}
	if lg, _ := meta["loadgen"].(map[string]any); lg["nack_generator"] != "sfubench-nackgen/1" {
		t.Errorf("loadgen = %v", meta["loadgen"])
	}
	if s, _ := meta["sfu_snapshot"].(map[string]any); s["run"] != "r1" {
		t.Errorf("sfu_snapshot = %v", meta["sfu_snapshot"])
	}
	if p, ok := meta["sfu_snapshot_problems"].([]any); !ok || len(p) != 0 {
		t.Errorf("sfu_snapshot_problems = %v", meta["sfu_snapshot_problems"])
	}

	r.opts.SFUSnapshot = ""
	if err := r.writeMeta(testStart); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(r.opts.OutDir, "run.json"))
	if !strings.Contains(string(b), `"sfu_snapshot": null`) || !strings.Contains(string(b), `"missing"`) {
		t.Errorf("no snapshot: %s", b)
	}

	r.opts.OutDir = filepath.Join(t.TempDir(), "absent")
	if err := r.writeMeta(testStart); err == nil {
		t.Error("write into a missing dir succeeded")
	}
}

func TestReadSnapshot(t *testing.T) {
	ok := `{"taken":"2026-11-05T14:00:00+03:30","sfu":"mediasoup","run":"r1"}`
	for _, c := range []struct {
		name, body, path string
		want             []string
	}{
		{"fine", ok, "", nil},
		{"no flag", "", "-", []string{"missing"}},
		{"not found", "", filepath.Join(t.TempDir(), "nope.json"), []string{"missing"}},
		{"array", `[1]`, "", []string{"invalid json"}},
		{"null", `null`, "", []string{"invalid json"}},
		{"garbage", `{`, "", []string{"invalid json"}},
		{"sfu", strings.Replace(ok, "mediasoup", "livekit", 1), "", []string{"sfu mismatch: snapshot=livekit run=mediasoup"}},
		{"old", strings.Replace(ok, "2026-11-05T14:00", "2026-11-04T23:00", 1), "", []string{"stale: "}},
		{"future", strings.Replace(ok, "2026-11-05T14:00", "2026-11-05T16:00", 1), "", []string{"stale: "}},
		{"no taken", `{"sfu":"mediasoup","run":"r1"}`, "", []string{"stale: "}},
		{"run", strings.Replace(ok, `"r1"`, `"r0"`, 1), "", []string{"run name mismatch"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.path
			switch path {
			case "":
				path = writeSnap(t, c.body)
			case "-":
				path = ""
			}
			snap, got := readSnapshot(path, "mediasoup", "r1", testStart)
			if len(got) != len(c.want) {
				t.Fatalf("problems %q, want %q", got, c.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], c.want[i]) {
					t.Fatalf("problems %q, want %q", got, c.want)
				}
			}
			if c.want != nil && (c.want[0] == "missing" || c.want[0] == "invalid json") != (snap == nil) {
				t.Fatalf("snapshot %s with problems %q", snap, got)
			}
		})
	}
}
