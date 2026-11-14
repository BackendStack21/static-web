package handler

// Regression tests from the 2026-11-14 adversarial review.

import (
	"testing"
	"time"
)

// The rate cap saturates instead of wrapping into the past for huge bodies
// or an absurd write_timeout.
func TestStreamRateCap_Saturates(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		n  int64
		wt time.Duration
	}{
		{303465209266176, 10 * time.Second},                 // ~276 TiB
		{1 << 62, 10 * time.Second},                         // far beyond
		{1 << 30, time.Duration(1<<63 - 1)},                 // max write_timeout
		{1 << 30, 292 * 365 * 24 * time.Hour},               // ~292 years
		{int64(1<<63-1) / 2, time.Duration(1<<63-1) - 1000}, // both large
		{0, 0},                                 // degenerate
		{streamMinRate * 10, 10 * time.Second}, // ordinary
		{streamMinRate*10 + streamMinRate/2, 10 * time.Second}, // fractional second
		{streamMinRate * 3600 * 24 * 365, 10 * time.Second},    // a year of streaming
		{streamMinRate * 3600 * 24 * 365 * 200, 0},             // 200 years
		{streamMinRate * 3600 * 24 * 365 * 300, 0},             // 300 years
		{streamMinRate * 3600 * 24 * 365 * 200, 100 * 365 * 24 * time.Hour},
	} {
		got := streamRateCap(t0, tc.n, tc.wt)
		if tc.n > 0 && !got.After(t0) {
			t.Errorf("n=%d wt=%v: cap %v is not after the start %v", tc.n, tc.wt, got, t0)
		}
		// A body that needs more than a century at the minimum rate (or a
		// century-long write_timeout) gets at least a century, not a value
		// that wrapped around to something shorter.
		const century = 100 * 365 * 24 * time.Hour
		if (tc.n/streamMinRate > int64(century/time.Second) || tc.wt > century) && got.Before(t0.Add(century)) {
			t.Errorf("n=%d wt=%v: cap %v is less than a century after the start", tc.n, tc.wt, got)
		}
	}
	// Ordinary values are exact: start + wt + n/minRate.
	if got, want := streamRateCap(t0, streamMinRate*10, 10*time.Second), t0.Add(20*time.Second); !got.Equal(want) {
		t.Errorf("ordinary cap = %v, want %v", got, want)
	}
}
