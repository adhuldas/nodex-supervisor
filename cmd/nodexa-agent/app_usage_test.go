package main

import "testing"

func TestPercent(t *testing.T) {
	cases := []struct{ part, whole, want float64 }{
		{50, 200, 25},
		{1, 3, 33.3},
		{0, 0, 0},
		{300, 200, 100}, // capped
	}
	for _, c := range cases {
		if got := percent(c.part, c.whole); got != c.want {
			t.Errorf("percent(%v, %v) = %v, want %v", c.part, c.whole, got, c.want)
		}
	}
}
