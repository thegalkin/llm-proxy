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
			// Last entry must be a terminal router. smol is the one
			// exempt role: it is free-first, so its LAST row must be the
			// funded paid primary instead — a free tier must not have the
			// last word, or a drained pool hard-fails the role. Asserted
			// positionally: "a go row exists somewhere" would stay green
			// if someone appended a row after it.
			last := ladder[len(ladder)-1]
			if role == RoleSmol {
				if last.Family != FamOpencodeGo || last.Model != "deepseek-v4.1-flash" {
					t.Errorf("role %q terminal = %s, want opencode-go:deepseek-v4.1-flash",
						role, formatTarget(last))
				}
				return
			}
			if !strings.HasPrefix(last.Model, "openrouter/") {
				t.Errorf("role %q terminal %q is not an openrouter router",
					role, formatTarget(last))
			}
		})
	}
}

// TestNoZenRows — no ladder may reference the opencode-zen family at all.
// The zen free tier is 403-gated server-side ("OpenCode's free tier can
// only be used in OpenCode"), and its keys are byte-identical to the
// opencode-go keys, so a zen row is dead weight plus a guaranteed wasted
// round-trip. This replaces the older TestNoPaidZen, which only banned
// paid zen rows.
func TestNoZenRows(t *testing.T) {
	for _, role := range AllRoles {
		for i, entry := range Cached(role) {
			if entry.Family == FamOpencodeZen {
				t.Errorf("role %q step %d is a zen row (%s); zen cannot serve",
					role, i+1, formatTarget(entry))
			}
		}
	}
}

// TestOpencodeGoPrimaryExceptSmol — opencode-go is the funded paid-primary
// family: every ladder except smol must lead with the deepseek primary and
// the glm fallback. smol is exempt by design (the user made it free-first);
// the paid row it must keep instead is asserted as its terminal in
// TestAllRolesCovered.
func TestOpencodeGoPrimaryExceptSmol(t *testing.T) {
	for _, role := range AllRoles {
		if role == RoleSmol {
			continue
		}
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

// TestNoDuplicateRows — a ladder must never list the same (family, model)
// twice: the duplicate is a guaranteed wasted round-trip on the hot path,
// and it means the ladder was edited without checking what was already
// there. This replaces the old openrouter/zen pairing test, whose premise
// (zen as a redundant second provider) was false — the zen keys are the
// same credentials as the go keys, so zen contributed no capacity.
func TestNoDuplicateRows(t *testing.T) {
	for _, role := range AllRoles {
		seen := map[string]int{}
		for i, entry := range Cached(role) {
			key := formatTarget(entry)
			if prev, dup := seen[key]; dup {
				t.Errorf("role %q lists %s twice (steps %d and %d)", role, key, prev+1, i+1)
			}
			seen[key] = i
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

// TestMinimaxPaidAnchor — roles that called for a paid minimax anchor
// (default, plan, task) must include at least one `FamMinimax` row before
// the terminal router. smol is no longer in this list: it is free-first by
// user decision, and its paid anchor is opencode-go.
func TestMinimaxPaidAnchor(t *testing.T) {
	needs := []string{RoleDefault, RolePlan, RoleTask}
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

// TestNoDeadFreeIDs — free IDs that cannot serve must not appear in any
// ladder. Two distinct classes are pinned here, and only one of them is
// visible in OpenRouter's catalog (GET /v1/models, snapshot at startup):
//
//   - retired: absent from the catalog. minimax/minimax-m2.7:free,
//     minimax/minimax-m3:free and z-ai/glm-5.2:free — OR answers 404
//     "unavailable for free", the ladder advances, and the row is a pure
//     wasted round-trip. `default` was paying six of them per request.
//   - listed but cannot serve: thinkingmachines/inkling:free is in the
//     catalog yet the 30-day journal shows 2389 attempts, 2380 of them 403
//     and zero deliveries ever. The zen rows fail the same way —
//     nemotron-3.5-lightning-free 2068 attempts / 0 deliveries (403/400/
//     429), hy3-free and x-preview-f-free 401, zero deliveries. A live probe
//     cannot always settle this class: probing inkling on 2026-09-15 just
//     returned "all openrouter keys exhausted" (429), because the whole free
//     tier rides one key. Journal history is the stronger authority here.
//
// poolside/laguna-s-2.1:free deliberately is NOT listed despite 2983
// historical 404s: it is a flapping free pool, not a retired slug — 1952
// deliveries in the same 30-day journal, plus a live 200 serving itself on
// 2026-09-15. It sits at the tail of ladderSmol() where a flap costs one
// fast advance and nothing else.
//
// Beware the position confound when reading per-row delivery rates: a row
// is only attempted after the rows above it failed, and on this proxy those
// failures are mostly key-level 429s (one openrouter key serves every free
// row). Rows low in a ladder therefore look worse than they are, which is
// exactly why the dead list is a small curated set rather than a threshold.
//
// Raw (non-synthetic) requests for a retired slug are rescued upstream of
// this: forward.go rewrites the model to openrouter/free and retries on the
// same key, so a bare 200 there proves nothing unless the body's model
// equals the requested id. The ladder path does not rewrite — it logs
// "status=404, advancing".
func TestNoDeadFreeIDs(t *testing.T) {
	dead := map[string]bool{
		"minimax/minimax-m2.7:free":     true,
		"minimax/minimax-m3:free":       true,
		"z-ai/glm-5.2:free":             true,
		"thinkingmachines/inkling:free": true,
		"nemotron-3.5-lightning-free":   true,
		"hy3-free":                      true,
		"x-preview-f-free":              true,
	}
	for _, role := range AllRoles {
		for i, entry := range Cached(role) {
			if dead[entry.Model] {
				t.Errorf("role %q step %d uses retired free id %s", role, i+1, formatTarget(entry))
			}
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

// formatTarget renders a Target for stable error messages.
func formatTarget(t Target) string {
	return t.Family + ":" + t.Model
}
