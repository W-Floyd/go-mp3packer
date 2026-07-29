package main

import "testing"

// TestDeltaCellAsymmetry pins the rule that an increase needs more evidence than
// a decrease. It is easy to "simplify" that back to a single threshold, and the
// result would be a table that occasionally accuses a commit of a regression it
// did not cause.
func TestDeltaCellAsymmetry(t *testing.T) {
	const tol = 0.015 // a 4.5% bar down, 9% up
	for _, c := range []struct {
		name    string
		prev, v float64
		want    string
	}{
		{"first row has nothing to compare with", 0, 100, "—"},
		{"a decrease past the bar is a result", 100, 94, "−6.0%"},
		{"a decrease inside it is not", 100, 97, "(−3.0%)"},
		{"an increase of the same size is not, either", 100, 106, "(+6.0%)"},
		{"an increase needs twice as much", 100, 110, "+10.0%"},
	} {
		if got := deltaCell(c.prev, c.v, tol); got != c.want {
			t.Errorf("%s: deltaCell(%g, %g) = %q, want %q", c.name, c.prev, c.v, got, c.want)
		}
	}
}
