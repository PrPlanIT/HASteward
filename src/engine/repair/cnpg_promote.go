package repair

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/escrow"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// errPromotePrepared is returned by promotePrepare to STOP the repair pipeline cleanly
// after it has validated the choice, escrowed the recovery set, persisted the proof, and
// printed the swap runbook. The remaining in-place swap is a guided manual step (P3.2c),
// so the run must not fall through to Assess/Heal (a normal heal-from-primary).
var errPromotePrepared = errors.New("promote: prepared (escrow + proof done); complete the swap per the runbook")

// promotePrepare is the P3.2b operation (repair --promote --instance N). It validates the
// authority choice against triage (planPromotion), then — reusing the exact escrow + proof
// machinery the deadlock breaker uses — escrows the full recovery set, verifies it, and
// persists a promotion RecoveryProof, so the promotion is REVERSIBLE before anything is
// touched. It then prints the cluster-specific swap runbook and stops. It deliberately
// does NOT execute the in-place timeline swap (P3.2c): CNPG has no safe single-command
// primitive for promoting a divergent/behind replica, and that rebuild-based sequence must
// be proven against a scratch cluster before it runs live — shipping it untested would be
// the exact data-destroying guess this machinery exists to prevent.
func (r *cnpgRepair) promotePrepare(ctx context.Context) (*model.TriageResult, error) {
	cfg := r.p.Config()
	if cfg.InstanceNumber == nil {
		return nil, fmt.Errorf("--promote requires --instance <N> naming the authority to promote")
	}

	t, err := triage.Run(ctx, r.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("promote: triage failed: %w", err)
	}
	plan, err := planPromotion(t, *cfg.InstanceNumber, cfg.Force)
	if err != nil {
		return nil, err
	}

	output.Section("Phase 0: Promote authority (--promote)")
	output.Field("Authority to promote", plan.Authority)
	output.Field("Rebuild from it", strings.Join(plan.Rebuild, ", "))
	output.Field("Recovery set (escrow-reversible)", strings.Join(plan.RecoverySet, ", "))
	if plan.Diverged {
		common.WarnLog("DIVERGED cluster — %s is the operator-designated survivor (--force). The other lineages are discarded once rebuilt.", plan.Authority)
	}

	if cfg.DryRun {
		output.Info("DRY RUN: would escrow %v, persist a promotion proof, and print the swap runbook — no changes made", plan.RecoverySet)
		output.Println(cnpgPromotionRunbook(cfg.ClusterName, cfg.Namespace, plan))
		return t, errDryRunPreview
	}

	// Escrow the full recovery set + verify (same fail-closed machinery as --unwedge), so
	// the promotion is reversible before ANY mutation — including the manual swap to come.
	prov, err := escrow.Select(ctx, cfg, plan.RecoverySet)
	if err != nil {
		return nil, fmt.Errorf("promote REFUSED: %w", err)
	}
	est := prov.EstimateCaptureBytes(plan.RecoverySet, usedBytesByPVC(t, plan.RecoverySet))
	avail, aerr := prov.AvailableBytes()
	if aerr != nil {
		return nil, fmt.Errorf("promote REFUSED: cannot determine escrow free space: %w", aerr)
	}
	if est+breakerReserveBytes > avail {
		return nil, fmt.Errorf("promote REFUSED: escrow (%s) requires %s + %s reserve, only %s available in the escrow store",
			prov.Name(), output.FormatBytes(est), output.FormatBytes(breakerReserveBytes), output.FormatBytes(avail))
	}
	refs, err := prov.Capture(ctx, plan.RecoverySet)
	if err != nil {
		return nil, fmt.Errorf("promote REFUSED: escrow capture failed: %w", err)
	}
	if err := prov.Verify(ctx, refs); err != nil {
		return nil, fmt.Errorf("promote REFUSED: escrow verification failed (rollback unproven): %w", err)
	}

	// Persist a promotion decision record BEFORE the operator performs the swap, so months
	// later "why was the cluster rebuilt around %s?" is answerable. Authority = the promoted
	// instance; Disposable = the instances to be re-cloned from it.
	proof := RecoveryProof{
		AssessmentHash: hashAssessment(t),
		Authority:      plan.Authority,
		Disposable:     plan.Rebuild,
		EscrowRefs:     refs,
		EscrowVerified: true,
	}
	if err := r.persistRecoveryProof(ctx, proof); err != nil {
		return nil, fmt.Errorf("promote REFUSED: cannot persist the promotion decision record: %w", err)
	}

	output.Success("Escrow captured + verified, promotion proof persisted — the recovery set is reversible")
	output.Println(cnpgPromotionRunbook(cfg.ClusterName, cfg.Namespace, plan))
	return t, errPromotePrepared
}

// promotionPlan is the validated, escrow-first plan to make a chosen instance the
// cluster's authority: which instance to promote and which to rebuild from it, plus the
// full recovery set that must be escrow-reversible before anything is touched.
type promotionPlan struct {
	Authority   string   // the instance to promote (becomes the source of truth)
	Rebuild     []string // the other instances, re-cloned FROM the authority
	RecoverySet []string // every instance — must be escrow-reversible before any mutation
	Diverged    bool     // true when chosen under divergence (operator-adjudicated survivor)
}

// planPromotion validates an operator's choice to promote instance `instanceNum` against
// triage's authority verdict, and builds the promotion plan. It is the safety core of
// P3.2b: it makes it impossible to promote the WRONG instance (which would discard the
// real authority's data). It is pure — no cluster access — so every refusal is unit-
// tested. It NEVER promotes automatically; the operator must ask for a specific instance.
//
// Rules, mirroring the triage authority verdict:
//   - leader_not_primary → the target MUST equal the proven authority; any other pick is
//     refused (you'd promote a non-authority and lose the real data).
//   - diverged → no single proven authority; allowed only with --force (the operator has
//     reviewed the evidence and is designating the surviving lineage).
//   - provable → the primary already holds the winning data; there is nothing to promote,
//     and promoting a behind replica would lose data → refused (even with --force).
//   - undeterminable → a data-bearing node could not be read; refuse until it is inspected.
func planPromotion(tr *model.TriageResult, instanceNum int, force bool) (*promotionPlan, error) {
	cluster := tr.Cluster.Name
	target := fmt.Sprintf("%s-%d", cluster, instanceNum)

	var found bool
	var others []string
	for _, a := range tr.Assessments {
		if a.Pod == target {
			found = true
			continue
		}
		others = append(others, a.Pod)
	}
	if !found {
		return nil, fmt.Errorf("REFUSING to promote %s: it is not among the cluster's instances — check --instance", target)
	}

	dc := tr.DataComparison
	switch dc.Authority {
	case model.AuthorityLeaderNotPrimary:
		if target != dc.MostAdvanced {
			return nil, fmt.Errorf("REFUSING to promote %s: triage proves the authority is %s, not %s. Promote the "+
				"authority (--instance %s), or re-triage if you believe the verdict is wrong",
				target, dc.MostAdvanced, target, ordinalOf(dc.MostAdvanced))
		}
		return newPromotionPlan(target, others, false), nil

	case model.AuthorityDiverged:
		if !force {
			return nil, fmt.Errorf("REFUSING to promote %s: the cluster is DIVERGED — no single proven authority. Review "+
				"the WAL-past-fork volumes and the primary's write-activity ledger in `hasteward triage`, escrow every "+
				"instance, then re-run with --force to designate %s as the surviving lineage", target, target)
		}
		return newPromotionPlan(target, others, true), nil

	case model.AuthorityProvable:
		if target == dc.MostAdvanced {
			return nil, fmt.Errorf("REFUSING to promote %s: it is already the authority/primary — nothing to promote", target)
		}
		return nil, fmt.Errorf("REFUSING to promote %s: the primary %s already holds the winning data (safe to heal "+
			"normally). Promoting a behind replica would DISCARD the primary's newer data — --force cannot override. Use "+
			"`hasteward repair` to heal replicas from the primary instead", target, dc.MostAdvanced)

	default: // AuthorityUndeterminable or unset
		return nil, fmt.Errorf("REFUSING to promote %s: authority is UNDETERMINABLE — a node that could hold data could "+
			"not be read, so promoting anything now risks discarding unseen data. Bring every instance up for inspection "+
			"(re-triage) first", target)
	}
}

func newPromotionPlan(authority string, others []string, diverged bool) *promotionPlan {
	set := make([]string, 0, len(others)+1)
	set = append(set, authority)
	set = append(set, others...)
	return &promotionPlan{Authority: authority, Rebuild: others, RecoverySet: set, Diverged: diverged}
}

// ordinalOf extracts the trailing instance ordinal from a CNPG pod name ("pg-2" → "2").
func ordinalOf(pod string) string {
	for i := len(pod) - 1; i >= 0; i-- {
		if pod[i] == '-' {
			return pod[i+1:]
		}
	}
	return pod
}

// cnpgPromotionRunbook renders the exact remaining steps to complete a promotion for THIS
// cluster, after HASteward has escrowed the recovery set and persisted the proof. The
// in-place timeline SWAP (step 3) is the one step HASteward does not yet execute (P3.2c):
// CNPG has no single safe command to make a divergent/behind replica the primary, and the
// rebuild-based sequence must be proven against a scratch cluster before it runs live.
func cnpgPromotionRunbook(cluster, ns string, plan *promotionPlan) string {
	return fmt.Sprintf(
		"Promotion of %s prepared. Escrow captured + proof persisted; the recovery set is reversible.\n"+
			"Remaining steps (the in-place swap is not yet automated — P3.2c):\n"+
			"  1. Confirm %s is up and consistent: `hasteward triage -e cnpg -c %s -n %s` (relieve it first with "+
			"`hasteward prune wal -e cnpg -c %s -n %s --instance %s` if it is disk-wedged).\n"+
			"  2. Fence the instances to rebuild (%v) and clear their datadirs so they cannot win a race.\n"+
			"  3. Make %s the primary — the manual step: a clean CNPG switchover only if the topology is healthy, "+
			"otherwise a rebuild-based promotion. Verify %s serves as primary before continuing.\n"+
			"  4. Re-clone the rebuilt instances FROM %s: `hasteward repair -e cnpg -c %s -n %s`.\n"+
			"  Escrow is RETAINED until you confirm the cluster is healthy — it is your rollback.",
		plan.Authority,
		plan.Authority, cluster, ns, cluster, ns, ordinalOf(plan.Authority),
		plan.Rebuild,
		plan.Authority, plan.Authority,
		plan.Authority, cluster, ns)
}
