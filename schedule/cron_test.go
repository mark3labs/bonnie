package schedule

import (
	"testing"
	"time"
)

func TestParseScheduleFiveFieldsAndZone(t *testing.T) {
	t.Parallel()
	s, loc, err := parseSchedule("*/15 * * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(time.Date(2026, 1, 1, 0, 1, 0, 0, loc))
	if got.Minute() != 15 || got.Location() != loc {
		t.Fatalf("next = %v; want 00:15 in %v", got, loc)
	}
}

func TestParseScheduleRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ expr, zone string }{{"* * *", "UTC"}, {"* * * * *", "Not/AZone"}} {
		if _, _, err := parseSchedule(tc.expr, tc.zone); err == nil {
			t.Errorf("parseSchedule(%q, %q) succeeded", tc.expr, tc.zone)
		}
	}
}
