package main

import "testing"

// TestDeltaCellThreshold pins that a step is printed only past two standard
// errors, and that the bar is the same in both directions. It was asymmetric once,
// an increase needing twice the evidence, and that hid real regressions: a
// regression nobody is shown is one nobody fixes.
func TestDeltaCellThreshold(t *testing.T) {
	est := func(pct, se float64) stepEstimate {
		return stepEstimate{pct: pct, se: se, sessions: 1, ok: true}
	}
	for _, c := range []struct {
		name string
		in   stepEstimate
		want string
	}{
		{"nothing to compare with", stepEstimate{}, "—"},
		{"a decrease past two sigma is a result", est(-3, 1), "−3.0%"},
		{"a decrease inside two sigma says nothing", est(-1.5, 1), "≈"},
		{"an increase past two sigma is equally a result", est(3, 1), "+3.0%"},
		{"an increase inside two sigma says nothing either", est(1.5, 1), "≈"},
		{"a wide error bar swallows a large step", est(-8, 5), "≈"},
	} {
		if got := deltaCell(c.in); got != c.want {
			t.Errorf("%s: deltaCell(%+v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestEstimateStepPoolsSessions checks that a step measured in two sittings comes
// out of both, and that a session which measured only one end is ignored. Pooling
// ratios rather than times is what makes runs from different sittings usable at
// all, since the times themselves drift.
func TestEstimateStepPoolsSessions(t *testing.T) {
	// Session 1 slow overall, session 2 fast, but a 10% step in both.
	cell := func(session int, ns ...float64) *cellRuns {
		c := &cellRuns{}
		for i, v := range ns {
			c.add(v, session, i+1)
		}
		return c
	}
	prev := cell(1, 100, 101, 99, 100, 100)
	for _, r := range cell(2, 50, 51, 49, 50, 50).Runs {
		prev.Runs = append(prev.Runs, r)
	}
	cur := cell(1, 90, 91, 89, 90, 90)
	for _, r := range cell(2, 45, 46, 44, 45, 45).Runs {
		cur.Runs = append(cur.Runs, r)
	}
	got := estimateStep(prev, cur, 5)
	if !got.ok || got.sessions != 2 {
		t.Fatalf("pooled %d sessions, want 2 (%+v)", got.sessions, got)
	}
	if got.pct < -10.5 || got.pct > -9.5 {
		t.Errorf("pooled step %.2f%%, want about −10%%", got.pct)
	}

	// A session present in only one of the pair cannot make a ratio.
	lone := cell(3, 10, 10, 10, 10, 10)
	if got := estimateStep(prev, lone, 5); got.ok {
		t.Errorf("made a step from sessions the two do not share: %+v", got)
	}
}
