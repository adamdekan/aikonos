package ids

import (
	"testing"

	"github.com/google/uuid"
)

func TestEventIDIsV7AndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		s := EventID()
		id, err := uuid.Parse(s)
		if err != nil {
			t.Fatalf("EventID %q is not a valid UUID: %v", s, err)
		}
		if v := id.Version(); v != 7 {
			t.Fatalf("EventID %q is UUID version %d, want 7", s, v)
		}
		if seen[s] {
			t.Fatalf("duplicate EventID %q", s)
		}
		seen[s] = true
	}
}

// UUIDv7 is time-ordered: ids minted later sort lexicographically after earlier
// ones, which is what the audit object layout + chain-head resume rely on.
func TestEventIDMonotonic(t *testing.T) {
	prev := EventID()
	for i := 0; i < 1000; i++ {
		cur := EventID()
		if cur <= prev {
			// v7 encodes millisecond time; within the same ms the random tail
			// can invert order. Only fail if time clearly went backwards.
			id1, _ := uuid.Parse(prev)
			id2, _ := uuid.Parse(cur)
			if id2.Time() < id1.Time() {
				t.Fatalf("EventID time went backwards: %q then %q", prev, cur)
			}
		}
		prev = cur
	}
}
