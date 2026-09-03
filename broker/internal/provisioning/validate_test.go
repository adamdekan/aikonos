package provisioning_test

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/provisioning"
)

// ── NormalizeMatcher ──────────────────────────────────────────────────────────

func TestNormalizeMatcher(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string // empty → expect error
		wantErr bool
	}{
		{
			name:  "valid exact lowercase",
			input: "alice@contoso.com",
			want:  "alice@contoso.com",
		},
		{
			name:  "valid exact uppercased → normalized",
			input: "Alice@Contoso.COM",
			want:  "alice@contoso.com",
		},
		{
			name:  "valid wildcard domain",
			input: "*@contoso.com",
			want:  "*@contoso.com",
		},
		{
			name:  "valid wildcard domain uppercased → normalized",
			input: "*@Contoso.COM",
			want:  "*@contoso.com",
		},
		{
			name:    "bare star rejected",
			input:   "*",
			wantErr: true,
		},
		{
			name:    "svc- local rejected",
			input:   "svc-agent@example.com",
			wantErr: true,
		},
		{
			name:    "svc- wildcard local is just star-at, not svc- — accepted (only exact svc- locals are rejected)",
			input:   "*@example.com",
			want:    "*@example.com",
		},
		{
			name:    "missing TLD rejected",
			input:   "alice@contoso",
			wantErr: true,
		},
		{
			name:    "single-char TLD rejected",
			input:   "alice@contoso.c",
			wantErr: true,
		},
		{
			name:  "valid TLD two chars",
			input: "alice@example.co",
			want:  "alice@example.co",
		},
		{
			name:    "empty string rejected",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no domain rejected",
			input:   "alice@",
			wantErr: true,
		},
		{
			name:    "no local rejected",
			input:   "@contoso.com",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := provisioning.NormalizeMatcher(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizeMatcher(%q) = %q, nil; want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeMatcher(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeMatcher(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ── ValidateGroups ────────────────────────────────────────────────────────────

func TestValidateGroups(t *testing.T) {
	cases := []struct {
		name    string
		groups  []string
		wantErr bool
	}{
		{name: "valid single", groups: []string{"security-team"}},
		{name: "valid multiple", groups: []string{"security-team", "devs", "a1"}},
		{name: "empty list rejected", groups: []string{}, wantErr: true},
		{name: "nil list rejected", groups: nil, wantErr: true},
		{name: "too many (21) rejected", groups: func() []string {
			g := make([]string, 21)
			for i := range g {
				g[i] = "group"
			}
			return g
		}(), wantErr: true},
		{name: "20 ok", groups: func() []string {
			g := make([]string, 20)
			for i := range g {
				g[i] = "g"
			}
			return g
		}()},
		{name: "starts with digit ok", groups: []string{"1team"}},
		{name: "starts with dash rejected", groups: []string{"-team"}, wantErr: true},
		{name: "starts with dot rejected", groups: []string{".team"}, wantErr: true},
		{name: "group: prefix rejected", groups: []string{"group:team"}, wantErr: true},
		{name: "uppercase rejected", groups: []string{"Security-Team"}, wantErr: true},
		{name: "wildcard hash rejected", groups: []string{"team#all"}, wantErr: true},
		{name: "space rejected", groups: []string{"sec team"}, wantErr: true},
		{name: "65 chars rejected", groups: []string{
			"a234567890123456789012345678901234567890123456789012345678901234x",
		}, wantErr: true},
		{name: "64 chars ok", groups: []string{
			"a23456789012345678901234567890123456789012345678901234567890123x",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := provisioning.ValidateGroups(tc.groups)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateGroups(%v) = nil; want error", tc.groups)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateGroups(%v) unexpected error: %v", tc.groups, err)
			}
		})
	}
}

// ── MatchRules ────────────────────────────────────────────────────────────────

func TestMatchRules(t *testing.T) {
	rules := []db.ProvisioningRule{
		{Matcher: "alice@contoso.com", Groups: []string{"admins", "security-team"}},
		{Matcher: "*@contoso.com", Groups: []string{"security-team", "devs"}},
		{Matcher: "bob@other.org", Groups: []string{"ops"}},
		{Matcher: "*@other.org", Groups: []string{"guests"}},
	}

	cases := []struct {
		name  string
		email string
		want  []string // sorted, deduped
	}{
		{
			name:  "exact match takes precedence; union with wildcard domain",
			email: "alice@contoso.com",
			// exact: admins, security-team; wildcard: security-team, devs → deduped sorted
			want: []string{"admins", "devs", "security-team"},
		},
		{
			name:  "wildcard only for unknown user at domain",
			email: "carol@contoso.com",
			want:  []string{"devs", "security-team"},
		},
		{
			name:  "exact match at other.org + wildcard union",
			email: "bob@other.org",
			want:  []string{"guests", "ops"},
		},
		{
			name:  "no match",
			email: "nobody@example.com",
			want:  []string{},
		},
		{
			name:  "case-insensitive input normalised before match",
			email: "Alice@Contoso.COM",
			want:  []string{"admins", "devs", "security-team"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := provisioning.MatchRules(tc.email, rules)
			if len(got) != len(tc.want) {
				t.Fatalf("MatchRules(%q) = %v; want %v", tc.email, got, tc.want)
			}
			for i, g := range got {
				if g != tc.want[i] {
					t.Errorf("MatchRules(%q)[%d] = %q; want %q", tc.email, i, g, tc.want[i])
				}
			}
		})
	}
}
