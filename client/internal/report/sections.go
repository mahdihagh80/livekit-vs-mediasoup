package report

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cleanMatcher selects the viewers on a clean network. The empty alternative
// keeps runs made before the profile label existed readable.
const cleanMatcher = `profile=~"clean|"`

func readMeta(dir string) map[string]any {
	b, _ := os.ReadFile(filepath.Join(dir, "run.json"))
	var meta map[string]any
	json.Unmarshal(b, &meta)
	return meta
}

// sloProfile returns the plan's network profiles and the matcher the SLO
// columns use: clean viewers when the plan has any, else everyone (a plan
// where every viewer is impaired is judged on what those viewers get).
func sloProfile(meta map[string]any) (profiles []string, matcher string) {
	p, _ := meta["plan"].(map[string]any)
	nets, _ := p["network"].([]any)
	if len(nets) == 0 {
		return []string{"clean"}, cleanMatcher
	}
	profiles = []string{"clean"}
	hasClean := false
	for _, n := range nets {
		m, _ := n.(map[string]any)
		name, _ := m["name"].(string)
		pct, _ := m["percent"].(float64)
		if name == "clean" {
			hasClean = pct > 0
			continue
		}
		profiles = append(profiles, name)
	}
	if !hasClean {
		return profiles[1:], `profile=~".*"`
	}
	return profiles, cleanMatcher
}

// profileCols are the reportQueries columns repeated per network profile.
var profileCols = map[string]bool{
	"vdelay_p95": true, "adelay_p95": true, "jitter_p95": true, "loss": true, "freeze": true,
	"fps_bad": true, "fps_p5": true, "tloss_p95": true, "kbr_p5": true, "freeze_p95": true, "lower_pct": true,
}

type profileRow struct {
	step    int
	profile string
	V       map[string]float64
}

func renderProfiles(b *strings.Builder, rows []profileRow) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n## Per network profile\n\nThe same columns for each profile, measured on what its people receive (the viewers; publishers too when the plan's networkTarget is publishers or all, else publishers are clean). Not judged against the SLO: an impaired profile is expected to do worse; compare it between SFUs.\n\n| step | profile |")
	var cols []struct{ key, header string }
	for _, q := range reportQueries {
		if profileCols[q.key] {
			cols = append(cols, struct{ key, header string }{q.key, q.header})
			b.WriteString(" " + q.header + " |")
		}
	}
	b.WriteString("\n|---|---|" + strings.Repeat("---|", len(cols)) + "\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %d | %s |", r.step, r.profile)
		for _, c := range cols {
			fmt.Fprintf(b, " %s |", fmtNum(r.V[c.key]))
		}
		b.WriteString("\n")
	}
}

// distRow is one line of the disturbance table: a recovery event, a
// layer-switch direction, a rejoin reason or the unmute check.
type distRow struct {
	what, name            string
	count, p50, p95, miss float64
}

// disturbances reads the run-wide recovery, layer-switch, rejoin and unmute
// numbers over [from, to].
func disturbances(ctx context.Context, prom string, from, to time.Time) []distRow {
	r := fmt.Sprintf("%ds", int(to.Sub(from).Seconds()))
	vec := func(expr, label string) map[string]float64 {
		m, err := promVector(ctx, prom, strings.ReplaceAll(expr, "{R}", r), label, to)
		if err != nil {
			return nil
		}
		return m
	}
	var out []distRow
	histo := func(what, metric, missed, label string) {
		count := vec(fmt.Sprintf(`sum by (%s) (increase(%s_count[{R}]))`, label, metric), label)
		p50 := vec(fmt.Sprintf(`histogram_quantile(0.5, sum by (%s, le) (increase(%s_bucket[{R}])))`, label, metric), label)
		p95 := vec(fmt.Sprintf(`histogram_quantile(0.95, sum by (%s, le) (increase(%s_bucket[{R}])))`, label, metric), label)
		miss := map[string]float64{}
		if missed != "" {
			miss = vec(fmt.Sprintf(`sum by (%s) (increase(%s[{R}]))`, label, missed), label)
		}
		names := map[string]bool{}
		for k := range count {
			names[k] = true
		}
		for k := range miss {
			names[k] = true
		}
		for _, n := range sortedKeys(names) {
			row := distRow{what: what, name: n, count: count[n], p50: nan(p50, n), p95: nan(p95, n), miss: miss[n]}
			if missed == "" {
				row.miss = math.NaN()
			}
			if row.count > 0.5 || row.miss > 0.5 {
				out = append(out, row)
			}
		}
	}
	histo("recovery", "bench_recovery_seconds", "bench_recovery_missed_total", "event")
	histo("layer switch", "bench_layer_switch_seconds", "bench_layer_switch_missed_total", "direction")
	histo("unmute heard", "bench_unmute_heard_seconds", "", "none")
	rejoins := vec(`sum by (reason) (increase(bench_rejoins_total[{R}]))`, "reason")
	names := map[string]bool{}
	for k := range rejoins {
		names[k] = true
	}
	for _, n := range sortedKeys(names) {
		if rejoins[n] > 0.5 {
			out = append(out, distRow{what: "rejoins", name: n, count: rejoins[n], p50: math.NaN(), p95: math.NaN(), miss: math.NaN()})
		}
	}
	return out
}

func renderDisturbances(b *strings.Builder, rows []distRow) {
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n## Disturbances and recovery (whole run)\n\nRecovery: disturbance → media again (video keyframe, else audio); missed = nothing within 60 s. Layer switch: dip start → lower layer (down), dip end → the layer from before the dip back (up; at most the requested one, a capped link may never carry that); missed = no adaptation. Unmute heard: speaker unmute → first audio of it at a listener.\n\n| what | label | count | p50 s | p95 s | missed |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		name := r.name
		if name == "" {
			name = "–"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", r.what, name, fmtNum(r.count), fmtNum(r.p50), fmtNum(r.p95), fmtNum(r.miss))
	}
}

func nan(m map[string]float64, k string) float64 {
	if v, ok := m[k]; ok {
		return v
	}
	return math.NaN()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
