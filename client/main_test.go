package main

import "testing"

func TestReadRunPlanOldAndNew(t *testing.T) {
	for _, dir := range []string{"internal/report/testdata/run-v1", "internal/report/testdata/run-v2"} {
		p, err := readRunPlan(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if p.Name != "bwe" || p.Rooms != 2 || p.Subscribe.High != 100 {
			t.Fatalf("%s: plan %+v", dir, p)
		}
	}
}
