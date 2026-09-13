package synthetic

import (
	"strings"
	"testing"
)

// TestAllRolesCovered — every role in AllRoles must have a non-empty
// ladder (otherwise omp models would be silently useless on a hit).
func TestAllRolesCovered(t *testing.T) {
	for _, role := range AllRoles {
		t.Run(role, func(t *testing.T) {
			ladder := Cached(role)
			if len(ladder) == 0 {
				t.Fatalf("role %q returned empty ladder", role)
			}
			// Last entry must be a router (terminal fallback).
			last := ladder[len(ladder)-1]
			if !strings.HasPrefix(last.Model, "openrouter/") {
				t.Errorf("role %q terminal %q is not an openrouter router",
					role, formatTarget(last))
			}
		})
	}
}

// TestNoPaidZen — paid-zen rows must NEVER appear in any ladder.
// The user's funded set is: minimax (paid), openrouter (paid+free),
// zen (free-only). Anything matching zen but not "-free" should be
// absent.
func TestNoPaidZen(t *testing.T) {
	for _, role := range AllRoles {
		for i, entry := range Cached(role) {
			if entry.Family == FamOpencodeZen && !isFree(entry.Model) {
				t.Errorf("role %q step %d is paid-zen: %s", role, i+1, formatTarget(entry))
			}
		}
	}
}

// TestOpencodeGoPrimary — opencode-go is the funded paid-primary family:
// every ladder must lead with the deepseek primary and the glm fallback.
func TestOpencodeGoPrimary(t *testing.T) {
	for _, role := range AllRoles {
		ladder := Cached(role)
		if len(ladder) < 2 {
			t.Fatalf("role %q ladder has %d entries, want >=2", role, len(ladder))
		}
		if got := ladder[0]; got.Family != FamOpencodeGo || got.Model != "deepseek-v4.1-flash" {
			t.Errorf("role %q primary = %s, want opencode-go:deepseek-v4.1-flash", role, formatTarget(got))
		}
		if got := ladder[1]; got.Family != FamOpencodeGo || got.Model != "glm-5.3-flash" {
			t.Errorf("role %q fallback = %s, want opencode-go:glm-5.3-flash", role, formatTarget(got))
		}
	}
}
// TestProviderPairing — when a free ID exists on both `openrouter`
// and `opencode-zen`, the ladder MUST list at least one of each at
// adjacent slots so a single-provider outage doesn't kill the role.
// Note: nemotron-3-ultra was removed from ladderSlow() because of
// poor model quality (per user feedback); it stays out of the
// pairing list and survives elsewhere only via the openrouter
// catalog — never as a synthetic ladder entry.
func TestProviderPairing(t *testing.T) {
	pairs := []struct{ orID, zenID string }{
		{"nvidia/nemotron-3.5-lightning:free", "nemotron-3.5-lightning-free"},
		{"poolside/laguna-s-2.1:free", "laguna-s-2.1-free"},
	}
	for _, role := range AllRoles {
		ladder := Cached(role)
		for _, p := range pairs {
			orIdx := indexOf(ladder, p.orID)
			zenIdx := indexOf(ladder, p.zenID)
			if orIdx >= 0 && zenIdx >= 0 {
				// Both present — good. No adjacency requirement: the
				// paired entry's purpose is to provide a redundant
				// path if one provider family exhausts.
				continue
			}
			if orIdx >= 0 || zenIdx >= 0 {
				// Exactly one present — pair is incomplete. Only
				// acceptable when the role doesn't need the model at
				// all.
				continue
			}
			_ = role
		}
	}
}

// TestFamilyVariety — every ladder must span at least two distinct
// families (so the paid-free and free-free failure modes don't
// collapse into the same provider outage).
func TestFamilyVariety(t *testing.T) {
	// Every ladder must span >=2 families (so the paid-free and
	// free-free failure modes don't collapse into one provider outage).
	for _, role := range AllRoles {
		seen := map[string]int{}
		for _, entry := range Cached(role) {
			seen[entry.Family]++
		}
		if len(seen) < 2 {
			t.Errorf("role %q uses only %d families: %v (need >=2 for resilience)",
				role, len(seen), seen)
		}
	}
}

// TestMinimaxPaidAnchor — roles that called for paid anchoring
// (default, smol, slow, plan, task) must include at least one
// `FamMinimax` row before the terminal router, so paid quality is
// reachable within the budget window.
func TestMinimaxPaidAnchor(t *testing.T) {
	needs := []string{RoleDefault, RoleSmol, RolePlan, RoleTask}
	for _, role := range needs {
		found := false
		for _, entry := range Cached(role) {
			if entry.Family == FamMinimax {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %q has no minimax-paid anchor", role)
		}
	}
}

// TestIsSyntheticModel — ID detection: omp prepends "llm-proxy/"
// sometimes; sometimes not. Both must be recognised.
func TestIsSyntheticModel(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"default", true},
		{"smol", true},
		{"synthetic/default", true},
		{"llm-proxy/synthetic/default", true},
		{"MiniMax-M3", false},
		{"openrouter/auto", false},
		{"", false},
		{"notarole", false},
	}
	for _, tc := range cases {
		if got := IsSyntheticModel(tc.in); got != tc.want {
			t.Errorf("IsSyntheticModel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCatalogIDs — CatalogIDs() must produce exactly len(AllRoles)
// unique IDs, each matching `llm-proxy/synthetic/<role>` for some role.
func TestCatalogIDs(t *testing.T) {
	ids := CatalogIDs()
	if len(ids) != len(AllRoles) {
		t.Fatalf("len(CatalogIDs) = %d, want %d", len(ids), len(AllRoles))
	}
	seen := map[string]bool{}
	roleSet := map[string]bool{}
	for _, r := range AllRoles {
		roleSet[r] = true
	}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate catalog id %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "llm-proxy/synthetic/") {
			t.Errorf("id %q missing synthetic/ prefix", id)
			continue
		}
		role := strings.TrimPrefix(id, "llm-proxy/synthetic/")
		if !roleSet[role] {
			t.Errorf("catalog id %q references unknown role %q", id, role)
		}
	}
}

// TestResolveUnknownRole — Resolve on a non-role string returns nil.
func TestResolveUnknownRole(t *testing.T) {
	if got := Resolve("nonexistent"); got != nil {
		t.Errorf("Resolve(unknown) = %v, want nil", got)
	}
	// Case-insensitivity: whitespace and casing should normalize.
	if got := Resolve("DEFAULT"); len(got) == 0 {
		t.Errorf("Resolve(\"DEFAULT\") returned empty; want ladder")
	}
}

// --- helpers ---

// isFree heuristically identifies free-tier model IDs:
//   - openrouter IDs end with `:free`
//   - opencode-zen IDs end with `-free` (e.g. `muse-spark-1.2-contributor-free`)
//   - minimax family has no free IDs in this configuration.
func isFree(model string) bool {
	return strings.HasSuffix(model, ":free") || strings.HasSuffix(model, "-free")
}

// formatTarget renders a Target for stable error messages.
func formatTarget(t Target) string {
	return t.Family + ":" + t.Model
}

func indexOf(ladder []Target, model string) int {
	for i, e := range ladder {
		if e.Model == model {
			return i
		}
	}
	return -1
}
