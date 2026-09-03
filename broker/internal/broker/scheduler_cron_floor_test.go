package broker

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// The cron frequency floor is a spend guard, not an ergonomics one: every fire
// is an unattended billable agent run and `workflow_schedule` is a Pi tool an
// agent can invoke, so "* * * * *" is 1440 paid runs a day nobody asked for.
// These tests pin the boundary at validateScheduleTiming — the single write-time
// chokepoint both north create and update paths reach.
func TestValidateScheduleTiming_CronFrequencyFloor(t *testing.T) {
	now := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	const floor = 5 * time.Minute

	cases := []struct {
		name        string
		cron        string
		minInterval time.Duration
		wantReject  bool
	}{
		{"every minute is below the floor", "* * * * *", floor, true},
		{"every 2 minutes is below the floor", "*/2 * * * *", floor, true},
		{"exactly at the floor is allowed", "*/5 * * * *", floor, false},
		{"well above the floor is allowed", "0 9 * * *", floor, false},
		// A burst spec whose *first* gap looks innocent (9:01 → next day 9:00)
		// but which still fires twice a minute apart. Sampling only one gap
		// would wave this through.
		{"burst within an otherwise sparse day", "0,1 9 * * *", floor, true},
		{"zero min_interval disables the floor", "* * * * *", 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := validateScheduleTiming(
				brokerv1.ScheduleKind_SCHEDULE_KIND_CRON, c.cron, nil, now, c.minInterval)
			if c.wantReject {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("cron %q under a %s floor: want InvalidArgument, got %v", c.cron, c.minInterval, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cron %q under a %s floor: unexpected rejection %v", c.cron, c.minInterval, err)
			}
		})
	}
}

// A one-off schedule fires exactly once, so no frequency floor can apply to it
// — a ONCE run 10 seconds out must survive even the strictest floor.
func TestValidateScheduleTiming_OnceExemptFromFloor(t *testing.T) {
	now := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	runAt := timestamppb.New(now.Add(10 * time.Second))

	kind, cronExpr, next, err := validateScheduleTiming(
		brokerv1.ScheduleKind_SCHEDULE_KIND_ONCE, "", runAt, now, time.Hour)
	if err != nil {
		t.Fatalf("ONCE must be exempt from the frequency floor, got %v", err)
	}
	if kind != db.ScheduleKindOnce {
		t.Errorf("kind = %s, want ONCE", kind)
	}
	if cronExpr != "" {
		t.Errorf("ONCE must resolve no cron, got %q", cronExpr)
	}
	if !next.Equal(runAt.AsTime()) {
		t.Errorf("next = %v, want the requested run_at %v", next, runAt.AsTime())
	}
}

// The floor message must name the configured minimum so an admin (or an agent
// reading the tool error) can tell what to change.
func TestEnforceCronFloor_MessageNamesTheMinimum(t *testing.T) {
	first := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	err := enforceCronFloor("* * * * *", first, 15*time.Minute)
	if err == nil {
		t.Fatal("want a rejection for a per-minute cron")
	}
	if got := status.Convert(err).Message(); !strings.Contains(got, "15m") {
		t.Errorf("message must name the configured minimum, got %q", got)
	}
}
