package triage

import "github.com/PrPlanIT/HASteward/src/output/model"

// ClusterVerdict is the top-level triage classification: what KIND of response a
// cluster's state calls for. It is the auto-repairable-vs-operator boundary this
// tool exists to draw, derived purely from a completed TriageResult.
//
// (Named ClusterVerdict, not Classification, to avoid colliding with the
// per-instance model.Classification used by CNPG.)
type ClusterVerdict string

const (
	// VerdictHealthy — every node is consistent and serving; nothing to do.
	VerdictHealthy ClusterVerdict = "healthy"
	// VerdictAutoHeal — a healthy primary component exists and the flagged
	// node(s) can rejoin via SST. The repairable hiccup: safe to auto-heal
	// without an operator.
	VerdictAutoHeal ClusterVerdict = "auto-heal"
	// VerdictBootstrapRequired — no serving primary, but authority is provable
	// (single lineage, positions Known). A deterministic bootstrap can declare
	// it; no human authority call is needed.
	VerdictBootstrapRequired ClusterVerdict = "bootstrap-required"
	// VerdictOperatorRequired — authority is ambiguous (split-brain, divergent
	// lineage, or unread positions). A human must decide the source of truth;
	// auto-action here risks destroying diverged data.
	VerdictOperatorRequired ClusterVerdict = "operator-required"
)

// TriageVerdict is Classify's full result: the class, a human-readable reason,
// and — for auto-heal — the pods that need reseeding.
type TriageVerdict struct {
	Class   ClusterVerdict
	Reason  string
	Targets []string
}

// Classify folds a completed TriageResult into the top-level response taxonomy.
// It is PURE and side-effect free — the single readable statement of "which
// hiccups are auto-repairable vs. which need an operator."
//
// It mirrors the gates already enforced in repair.SafetyGate / repair.PlanTargets
// (galera.go:80/193/230) and the bootstrap belly-up authority path; it does not
// yet DRIVE them (see the testing ticket) — today it is an advisory summary and
// the tested spec of the boundary.
//
// Decision order (fail-safe: default toward operator):
//  1. No serving join target (all down, or no primary component):
//     - authority provable (SafeToHeal) → bootstrap-required
//     - otherwise (divergent / unread)  → operator-required
//  2. A primary is serving but data is not safe (split-brain w/ a live primary) → operator-required
//  3. Safe + a node needs reseed → auto-heal (with targets)
//  4. Safe + nothing to do       → healthy
func Classify(r *model.TriageResult) TriageVerdict {
	if !hasJoinTarget(r) {
		if r.DataComparison.SafeToHeal {
			return TriageVerdict{
				Class:  VerdictBootstrapRequired,
				Reason: "no serving primary, but authority is provable (single lineage, positions known) — bootstrap can declare it deterministically",
			}
		}
		return TriageVerdict{
			Class:  VerdictOperatorRequired,
			Reason: "no serving primary and authority is ambiguous (divergent lineage or unread positions) — an operator must declare the source of truth",
		}
	}

	if !r.DataComparison.SafeToHeal {
		return TriageVerdict{
			Class:  VerdictOperatorRequired,
			Reason: "a primary is serving but the data comparison is not safe (potential split-brain) — healing could destroy diverged data",
		}
	}

	var targets []string
	for _, a := range r.Assessments {
		if a.NeedsHeal {
			targets = append(targets, a.Pod)
		}
	}
	if len(targets) > 0 {
		return TriageVerdict{
			Class:   VerdictAutoHeal,
			Reason:  "a healthy primary component exists and the flagged node(s) can rejoin via SST — safe to auto-heal",
			Targets: targets,
		}
	}
	return TriageVerdict{Class: VerdictHealthy, Reason: "all nodes healthy and consistent — nothing to do"}
}

// hasJoinTarget reports whether some node can serve as an SST/streaming source —
// i.e. a repair (as opposed to a bootstrap) has somewhere to join to. Mirrors the
// repair SafetyGate hard-stop: AllNodesDown OR no primary component => bootstrap.
func hasJoinTarget(r *model.TriageResult) bool {
	if r.AllNodesDown {
		return false
	}
	if len(r.DataComparison.PrimaryMembers) > 0 {
		return true // Galera: a Primary component exists
	}
	for _, a := range r.Assessments {
		if a.IsPrimary && a.IsRunning && a.IsReady {
			return true // CNPG-style: a running/ready primary
		}
	}
	return false
}
