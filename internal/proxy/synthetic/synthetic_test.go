package synthetic

import (
	"strings"
	"testing"
)

// Every role has a paid terminal; smol and tiny end on funded go models.
func TestAllRolesCovered(t *testing.T) {
	for _, role := range AllRoles {
		t.Run(role, func(t *testing.T) {
			ladder := Cached(role)
			if len(ladder) == 0 {
				t.Fatalf("role %q returned empty ladder", role)
			}
			last := ladder[len(ladder)-1]
			if role == RoleSmol || role == RoleTiny {
				model := "deepseek-v4.1-flash"
				if role == RoleTiny {
					model = "glm-5.3-flash"
				}
				if last.Family != FamOpencodeGo || last.Model != model {
					t.Errorf("role %q terminal = %s, want opencode-go:%s",
						role, formatTarget(last), model)
				}
				return
			}
			// The model prefix alone would accept an "openrouter/..." id
			// routed through a family that cannot serve it, so the family
			// is pinned too.
			if last.Family != FamOpenrouter || !strings.HasPrefix(last.Model, "openrouter/") {
				t.Errorf("role %q terminal %s is not an openrouter router",
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

// reasoningRoles are the three roles whose ladder must buy reasoning
// capacity instead of borrowing it from the free tier.
var reasoningRoles = map[string]bool{RolePlan: true, RoleAdvisor: true, RoleSlow: true}

// TestStealthPrimaryForEveryRole — all 10 ladders lead with the openrouter
// stealth model, and what follows it splits by role. The stealth row is
// free-of-charge upstream (pricing 0/0, hence no ":free" suffix), so
// position 0 costs nothing when it serves and the rows below it exist for
// when it does not. Asserted positionally, not by membership: a ladder that
// pushed the stealth row behind a paid row would still "contain" it.
func TestStealthPrimaryForEveryRole(t *testing.T) {
	for _, role := range AllRoles {
		ladder := Cached(role)
		if len(ladder) < 2 {
			t.Fatalf("role %q ladder has %d entries, want >=2", role, len(ladder))
		}
		if got := ladder[0]; got.Family != FamOpenrouter || got.Model != "stealth/union-alpha" {
			t.Errorf("role %q primary = %s, want openrouter:stealth/union-alpha",
				role, formatTarget(got))
		}
		second := ladder[1]
		if reasoningRoles[role] {
			// Reasoning roles pay for their second attempt: the funded go
			// primary follows the stealth row directly.
			if second.Family != FamOpencodeGo || second.Model != "deepseek-v4.1-flash" {
				t.Errorf("role %q step 2 = %s, want opencode-go:deepseek-v4.1-flash",
					role, formatTarget(second))
			}
			continue
		}
		paidSeen := false
		for i, entry := range ladder {
			if isFreeRow(entry) {
				if paidSeen {
					t.Errorf("role %q step %d is free after a paid row: %s", role, i+1, formatTarget(entry))
				}
			} else {
				paidSeen = true
			}
		}
	}
}

// Reasoning roles skip every free alternative after the stealth primary.
func TestReasoningRolesHaveNoFreeRows(t *testing.T) {
	for _, role := range AllRoles {
		if !reasoningRoles[role] {
			continue
		}
		for i, entry := range Cached(role) {
			if i > 0 && isFreeRow(entry) {
				t.Errorf("role %q step %d is a free row (%s); reasoning roles stay on paid rows",
					role, i+1, formatTarget(entry))
			}
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
// the terminal router. smol is not in this list: it is free-first by user
// decision, and the paid anchor it keeps is its terminal go primary
// (TestAllRolesCovered).
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
// ladder. The 30-day journal puts them in three classes, and only the first
// is visible in OpenRouter's catalog (GET /v1/models, snapshot at startup):
//
//   - retired: absent from the catalog. minimax/minimax-m2.7:free,
//     minimax/minimax-m3:free and z-ai/glm-5.2:free — upstream 404s
//     "unavailable for free", the ladder advances, and the row is a pure
//     wasted round-trip. `default` was paying six of them per request.
//   - unsupported slug: the upstream rejects the id outright —
//     "Model hy3-free is not supported", "Model minimax/x-preview-f-free is
//     not supported" (401, 237 each), and nemotron-3.5-lightning-free on the
//     same family (428x400 / 1050x403 / 586x429). Zero deliveries ever.
//   - gated by app identity: thinkingmachines/inkling:free IS in the catalog
//     and still cannot deliver — 2389 attempts, 2380x403, zero deliveries,
//     with the body spelling out why: "only available on agentic harnesses.
//     Try plugging it into a coding agent or productivity app", failed
//     routing step "Gate Free Endpoints by Agentic Harness". The harness
//     identity OR sees is our own attribution, X-Title: llm-proxy
//     (forward.go:731) — not a listed app, so the endpoint stays gated. A
//     listed app name there could unlock this whole class; that is a claim of
//     app identity, so it stays a user decision and nothing here sends it.
//
// An upstream 403 on a gated slug does not surface as 403. The family treats
// 401/403/429/400 as key failures (forward.go:818), 403 cools the key for 30
// minutes (rotation.go:102 cooldownAuth), and with a single openrouter key
// the family then answers 429 "all openrouter keys exhausted" (forward.go:877).
// So that 429 is the aggregate of the slug's own 403, not quota. The cooldown
// it records is only a hint at this scale: when every key of a family is
// cooling, buildAttemptOrder returns them as-is (rotation.go:56) instead of
// bricking the family, so the single openrouter key keeps being tried and the
// real cost of probing a gated row is one failed round-trip. A second OR key
// is what would turn that hint into a 30-minute sideline.
//
// 429 rows are deliberately NOT listed — they recover. nemotron free and
// laguna both take heavy 429s and still deliver, and the position confound
// makes low rows look worse than they are: a row is only attempted after the
// rows above it failed, and those failures are mostly key-level 429s from the
// single openrouter key that serves the whole free tier. Hence a small
// curated list, not a delivery-rate threshold.
//
// poolside/laguna-s-2.1:free is the cautionary case: 2983x404 in the journal
// yet 1952 deliveries and a live 200 serving itself on 2026-09-15, so it is a
// flapping pool, not a retired slug — it sits at the tail of ladderSmol()
// where a flap costs one fast advance and nothing else.
//
// Raw (non-synthetic) requests for a retired slug are rescued upstream of
// this: forward.go rewrites the model to openrouter/free and retries on the
// same key, so a bare 200 there proves nothing unless the body's model equals
// the requested id. The ladder path does not rewrite — it logs
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
				t.Errorf("role %q step %d uses unservable free id %s", role, i+1, formatTarget(entry))
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

// isFreeRow includes the zero-priced stealth model despite its unsuffixed ID.
func isFreeRow(t Target) bool {
	if t.Family != FamOpenrouter {
		return false
	}
	return strings.HasSuffix(t.Model, ":free") || t.Model == "openrouter/free" || t.Model == "stealth/union-alpha"
}
