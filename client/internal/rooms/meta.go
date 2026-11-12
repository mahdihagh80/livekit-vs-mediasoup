package rooms

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// snapshotMaxAge is how old an SFU snapshot may be when the run starts.
const snapshotMaxAge = 12 * time.Hour

func (r *Run) writeMeta(started time.Time) error {
	snap, problems := readSnapshot(r.opts.SFUSnapshot, r.opts.SFU, r.opts.RunName, started)
	for _, p := range problems {
		log.Printf("WARNING: sfu snapshot %q: %s", r.opts.SFUSnapshot, p)
	}
	meta := map[string]any{
		"run": r.opts.RunName, "sfu": r.opts.SFU, "url": r.opts.URL, "plan": r.opts.Plan, "started": started,
		"name_prefix": r.opts.NamePrefix, "room_prefix": r.opts.RoomPrefix, "shared_rooms": r.opts.Shared,
		"shard":  fmt.Sprintf("%d/%d", r.opts.Shard.K, r.opts.Shard.N),
		"schema": 2, "loadgen": r.opts.Loadgen, "sfu_snapshot": snap, "sfu_snapshot_problems": problems,
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.opts.OutDir, "run.json"), b, 0o644)
}

// readSnapshot returns the sfu.json at path verbatim (nil if unusable) and
// what is wrong with it for this run.
func readSnapshot(path, sfu, run string, started time.Time) (json.RawMessage, []string) {
	problems := []string{}
	if path == "" {
		return nil, append(problems, "missing")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, append(problems, "missing")
	}
	var snap map[string]any
	if json.Unmarshal(b, &snap) != nil || snap == nil {
		return nil, append(problems, "invalid json")
	}
	if s := fmt.Sprint(snap["sfu"]); s != sfu {
		problems = append(problems, fmt.Sprintf("sfu mismatch: snapshot=%s run=%s", s, sfu))
	}
	taken, err := time.Parse(time.RFC3339, fmt.Sprint(snap["taken"]))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("stale: unreadable taken %q", fmt.Sprint(snap["taken"])))
	case taken.After(started):
		problems = append(problems, fmt.Sprintf("stale: taken %s, after the run started", taken.Format(time.RFC3339)))
	case started.Sub(taken) > snapshotMaxAge:
		problems = append(problems, fmt.Sprintf("stale: taken %s, %s before the run started", taken.Format(time.RFC3339), started.Sub(taken).Round(time.Minute)))
	}
	if fmt.Sprint(snap["run"]) != run {
		problems = append(problems, "run name mismatch")
	}
	return json.RawMessage(b), problems
}
