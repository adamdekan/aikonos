package db

import (
	"testing"
	"time"
)

func TestNextCronFire(t *testing.T) {
	// Wed 2026-06-03 08:00 UTC.
	base := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		expr string
		want time.Time
	}{
		{"every minute", "* * * * *", base.Add(time.Minute)},
		{"top of next hour", "0 * * * *", time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)},
		{"weekday 9am still today", "0 9 * * 1-5", time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NextCronFire(c.expr, base)
			if err != nil {
				t.Fatalf("NextCronFire(%q): %v", c.expr, err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("NextCronFire(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestNextCronFireTimezone(t *testing.T) {
	// Guided webui prepends robfig's CRON_TZ token so fields evaluate in the
	// creator's zone. "14 15 * * *" = 15:14 wall-clock in Europe/Vienna.
	cases := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		// Summer: CEST is UTC+2, so 15:14 local == 13:14 UTC, same day.
		{"vienna summer CEST", "CRON_TZ=Europe/Vienna 14 15 * * *",
			time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 21, 13, 14, 0, 0, time.UTC)},
		// Winter: CET is UTC+1, so 15:14 local == 14:14 UTC — DST-correct.
		{"vienna winter CET", "CRON_TZ=Europe/Vienna 14 15 * * *",
			time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 1, 15, 14, 14, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NextCronFire(c.expr, c.after)
			if err != nil {
				t.Fatalf("NextCronFire(%q): %v", c.expr, err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("NextCronFire(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestNextCronFireSkipsToWeekday(t *testing.T) {
	// Sat 2026-06-06 12:00 UTC → next weekday 9am is Mon 2026-06-08.
	sat := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	got, err := NextCronFire("0 9 * * 1-5", sat)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestValidCronExpr(t *testing.T) {
	valid := []string{"* * * * *", "0 9 * * 1-5", "*/15 0 1 * *",
		"CRON_TZ=Europe/Vienna 0 9 * * *", "TZ=UTC 0 9 * * *"}
	for _, e := range valid {
		if !ValidCronExpr(e) {
			t.Errorf("expected %q to be valid", e)
		}
	}
	// Rejected: empty, 6-field (seconds), garbage, out-of-range. The space-free
	// cases (CRON_TZ=, CRON_TZ=Europe/Vienna, and the all-tab case) panic
	// robfig v3.0.1 unless the parseCronSpec guard fires — a tab is not a space.
	invalid := []string{"", "0 0 * * * *", "not a cron", "99 0 * * *",
		"CRON_TZ=", "CRON_TZ=Europe/Vienna", "CRON_TZ=Bad/Zone 0 9 * * *",
		"CRON_TZ=UTC\t0\t9\t*\t*\t*"}
	for _, e := range invalid {
		if ValidCronExpr(e) {
			t.Errorf("expected %q to be invalid", e)
		}
	}
}
