package rooms

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Step is one point on the ramp, written to steps.csv for the report.
type Step struct {
	Step      int
	Target    int
	Start     time.Time
	Settled   time.Time
	End       time.Time
	Connected int64
	Failed    int64
	Disconn   int64
}

func writeSteps(path string, recs []Step) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"step", "target", "start", "settled", "end", "connected", "failed", "disconnected"})
	for _, r := range recs {
		w.Write([]string{
			strconv.Itoa(r.Step), strconv.Itoa(r.Target),
			r.Start.UTC().Format(time.RFC3339), r.Settled.UTC().Format(time.RFC3339), r.End.UTC().Format(time.RFC3339),
			fmt.Sprint(r.Connected), fmt.Sprint(r.Failed), fmt.Sprint(r.Disconn),
		})
	}
	w.Flush()
	return w.Error()
}

// ReadSteps loads steps.csv written by Run.Start.
func ReadSteps(path string) ([]Step, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []Step
	for _, r := range rows[1:] {
		var s Step
		s.Step, _ = strconv.Atoi(r[0])
		s.Target, _ = strconv.Atoi(r[1])
		s.Start, _ = time.Parse(time.RFC3339, r[2])
		s.Settled, _ = time.Parse(time.RFC3339, r[3])
		s.End, _ = time.Parse(time.RFC3339, r[4])
		s.Connected, _ = strconv.ParseInt(r[5], 10, 64)
		s.Failed, _ = strconv.ParseInt(r[6], 10, 64)
		s.Disconn, _ = strconv.ParseInt(r[7], 10, 64)
		out = append(out, s)
	}
	return out, nil
}
