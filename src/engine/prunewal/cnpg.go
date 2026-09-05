package prunewal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/cnpgjob"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	Register("cnpg", func(ep provider.EngineProvider) (Pruner, error) {
		p, ok := ep.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("cnpg pruner requires *provider.CNPGProvider, got %T", ep)
		}
		t, err := triage.Get(p)
		if err != nil {
			return nil, fmt.Errorf("cnpg pruner: failed to get triager: %w", err)
		}
		return &cnpgPruner{p: p, triager: t}, nil
	})
}

// cnpgPruner implements Pruner for CloudNativePG PostgreSQL clusters.
type cnpgPruner struct {
	p       *provider.CNPGProvider
	triager triage.Triager
}

func (w *cnpgPruner) Name() string { return "cnpg" }

// PruneWAL clears accumulated WAL from a disk-full CNPG instance.
//
// This is a destructive operation. It is only safe when the WAL being removed is
// deadweight — no live consumer still needs it. That precondition is ENFORCED (not just
// asserted in prose): every ready replica's replay LSN is checked against the checkpoint
// REDO LSN before any segment is deleted, and the prune aborts if any replica lags behind
// (see assertReplicasCaughtUp). --force overrides the gate.
//
// Flow: triage -> safety check -> fence -> mount PVC -> (Go-driven) verify LSN + clear
// pg_wal -> unfence. All prune logic runs in Go, exec'd into a dumb helper pod.
func (w *cnpgPruner) PruneWAL(ctx context.Context) (*model.PruneWALResult, error) {
	cfg := w.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	if cfg.InstanceNumber == nil {
		return nil, fmt.Errorf("prune wal requires --instance/-i to specify which instance to clear")
	}
	instanceNum := *cfg.InstanceNumber
	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, instanceNum)

	// Serialize against other HASteward mutations on this cluster (shared reconcile
	// switch + read-modify-write fencedInstances annotation).
	release, lockErr := cnpgjob.AcquireClusterLock(ctx, ns, cfg.ClusterName, "prune-wal")
	if lockErr != nil {
		return nil, lockErr
	}
	defer release()

	result := &model.PruneWALResult{
		Engine:   "cnpg",
		Cluster:  model.ObjectRef{Name: cfg.ClusterName, Namespace: ns},
		Instance: int64(instanceNum),
	}

	// Phase 1: Triage to understand cluster state
	output.Section("Phase 1: Triage")
	triageResult, err := triage.Run(ctx, w.triager, engine.NopSink{})
	if err != nil {
		return nil, fmt.Errorf("triage failed: %w", err)
	}

	// Find the target instance assessment
	var targetAssessment *model.InstanceAssessment
	for i := range triageResult.Assessments {
		if triageResult.Assessments[i].Pod == targetPod {
			targetAssessment = &triageResult.Assessments[i]
			break
		}
	}
	if targetAssessment == nil {
		return nil, fmt.Errorf("instance %s not found in triage", targetPod)
	}

	// Safety checks
	output.Section("Phase 2: Safety Checks")

	primary := k8s.GetNestedString(w.p.Cluster(), "status", "currentPrimary")
	isPrimary := primary == targetPod

	// A ready instance never needs WAL relief — both the primary and the trapped-
	// authority paths require the target to be crash-looping / disk-full.
	if targetAssessment.IsReady {
		return nil, fmt.Errorf("ABORT: %s is running and ready. WAL pruning is for disk-full/crash-looping instances", targetPod)
	}

	// P3.4: the target need not be the current primary. WAL accumulates on the primary,
	// but a trapped DATA AUTHORITY on a non-primary — a crash-looping golden replica whose
	// PVC filled with WAL (boundary-postgres-2) — must be relievable too, or the one node
	// we most need to bring up for inspection/promotion is the one node the tool refuses
	// to help. Relief deletes only WAL older than the target's OWN checkpoint REDO (safe
	// for its recovery; committed data past the checkpoint lives in WAL we KEEP), and
	// nothing streams from a crash-looping non-primary, so the primary-only replica-
	// caughtup gate does not apply. Eligibility is authority-gated below.
	verifyReplicas := isPrimary
	var readyReplicas []string
	if isPrimary {
		output.Success("Target %s is the primary and not ready — proceeding", targetPod)
		// ReadyCount includes the primary; the primary is NOT ready (checked above), so it
		// equals the number of healthy replicas we can verify WAL safety against.
		if triageResult.ReadyCount == 0 {
			if !cfg.Force {
				return nil, fmt.Errorf("ABORT: no ready replicas found. Cannot verify data safety without at least one healthy replica. Re-run with --force to override")
			}
			common.WarnLog("force=true — proceeding with WAL prune despite no ready replicas. Data safety cannot be verified by a replica.")
		} else {
			output.Success("Found %d ready replica(s)", triageResult.ReadyCount)
		}
		// The ready replicas are the peers we verify against — they must not lag behind the
		// checkpoint. The primary is the target and is not ready, so it never appears here.
		for _, a := range triageResult.Assessments {
			if a.Pod != targetPod && a.IsReady {
				readyReplicas = append(readyReplicas, a.Pod)
			}
		}
	} else if err := w.assertReliefEligible(targetPod, targetAssessment, triageResult); err != nil {
		return nil, err
	}

	// Resolve PVC name for the target instance
	targetPVC, err := w.resolvePVC(ctx, targetPod)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PVC for %s: %w", targetPod, err)
	}

	// P3.6 — deadlock-recover: replay + recycle WAL IN PLACE for an instance that is
	// disk-full DEADLOCKED (too full to start → can't checkpoint → can't recycle WAL).
	// DISTINCT from the delete-only path below (which removes PRE-checkpoint deadweight
	// while the instance stays down): this one starts postgres single-user to REPLAY the
	// committed WAL (no data loss), checkpoints, then archivecleans the recycled segments.
	// It is the fix when plain prune-wal finds nothing to trim because all WAL is
	// post-checkpoint recyclable (the boundary-postgres-2 case). Same eligibility gates
	// (not-ready + authority-gated for non-primary) already passed above.
	if cfg.DeadlockRecover {
		return w.deadlockRecover(ctx, targetPod, targetPVC, isPrimary, triageResult, result)
	}

	// DRY RUN stops HERE — after triage and every safety gate (target eligibility,
	// not-ready, replica-caughtup / authority relief), but BEFORE the first mutation
	// (fence → clear pg_wal). Previously --dry-run was silently ignored and prune-wal
	// fenced + deleted WAL regardless; this makes the preview real.
	// A trapped non-primary AUTHORITY must NOT be unfenced back to the operator after
	// relief: its target primary is a divergent/stale lineage, so CNPG would pg_rewind the
	// freed authority onto that lineage and destroy the golden data. Keep it fenced
	// (isolated) for a controlled promotion. A disk-full PRIMARY is handed back normally.
	keepFenced := !isPrimary

	if cfg.DryRun {
		gate, tail := "primary", "then unfence and hand it back to the operator"
		if !isPrimary {
			gate = "trapped non-primary authority"
			tail = "then KEEP IT FENCED (isolated from operator reconcile so it is NOT pg_rewound onto the stale " +
				"primary's lineage) for a controlled promotion"
		}
		output.Info("DRY RUN: %s passed all safety gates (%s). Would fence it, mount PVC %s, delete WAL segments "+
			"OLDER than its own checkpoint REDO (committed data past the checkpoint is KEPT; .history preserved), %s. "+
			"No changes made.", targetPod, gate, targetPVC, tail)
		return result, nil
	}

	// Discover postgres image and UID/GID from a healthy replica
	imageName, postgresUID, postgresGID, err := w.discoverPostgresInfo(ctx, triageResult)
	if err != nil {
		return nil, fmt.Errorf("failed to discover postgres info: %w", err)
	}

	// Phase 3: Fence and clear WAL
	output.Section("Phase 3: Fence and Clear WAL")
	walPodName := fmt.Sprintf("%s-prune-wal-%d-%d", cfg.ClusterName, instanceNum, time.Now().Unix())

	// The prune runs as Go-driven exec into a dumb helper pod that just holds the PVC
	// (see OnPVCAcquired below): all decisions — the checkpoint boundary and the LSN
	// safety gate — are made in Go, exec'd into the helper, and unit-testable.
	// readyReplicas / verifyReplicas were resolved above per the primary vs trapped-
	// authority path.
	uid, gid := parseInt64(postgresUID), parseInt64(postgresGID)

	walPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      walPodName,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			Containers: []corev1.Container{{
				Name:  "wal-prune",
				Image: imageName,
				// Dumb exec target: just mount the PVC and stay alive. All prune logic
				// runs from Go via OnPVCAcquired, exec'd into this container.
				Command: []string{"sh", "-c", "sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "pgdata",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: targetPVC,
						},
					},
				},
			},
		},
	}
	// Runs as the postgres uid (non-root); pg_archivecleanup / WAL relocation write to the
	// data disk, so treat it as a DB engine writing outside its mounts.
	k8s.ApplyHelperHardening(walPod, k8s.HelperHardening{WritableRootFS: true})

	// Fence → disable reconcile → acquire the PVC → prune WAL → re-enable → unfence,
	// via the shared primitive repair uses. The reconcile bracket is what makes the
	// PVC handoff reliable on a responsive cluster (no unwinnable delete race).
	output.Bullet(0, "Fence, acquire the PVC, prune pg_wal, and restore")
	if err := cnpgjob.Run(ctx, cnpgjob.OfflinePVCJob{
		Namespace:        ns,
		ClusterName:      cfg.ClusterName,
		TargetPod:        targetPod,
		TargetPVC:        targetPVC,
		HelperPod:        walPod,
		HelperPodName:    walPodName,
		Label:            "wal-prune",
		DeleteTimeoutSec: cfg.DeleteTimeout,
		KeepFenced:       keepFenced,
		// Go-driven: prune runs here while the helper holds the PVC.
		OnPVCAcquired: func(ctx context.Context) error {
			return w.pruneWALOnPVC(ctx, walPodName, ns, readyReplicas, verifyReplicas)
		},
	}); err != nil {
		return nil, err
	}

	// A kept-fenced authority will NOT come back online (that is the point — it is isolated
	// from the operator until a controlled promotion). Report the safe end state and stop;
	// do not wait for a readiness that cannot happen by design.
	if keepFenced {
		output.Success("WAL relieved on %s — its disk is freed, but it was KEPT FENCED so CNPG will not start or "+
			"pg_rewind it onto the stale primary's lineage. Bring it up for inspection and promote it deliberately; "+
			"unfence ONLY as part of that promotion (kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-).",
			targetPod, cfg.ClusterName, ns)
		return result, nil
	}

	// Wait for the operator to recreate + restart the instance on its PVC.
	output.Bullet(0, "Waiting for %s to come back online", targetPod)
	for i := 0; i < 30; i++ {
		time.Sleep(10 * time.Second)
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{})
		if podErr == nil && k8s.PodReady(*pod, "postgres") {
			output.Success("Instance %s is back online!", targetPod)
			return result, nil
		}
	}

	common.WarnLog("%s did not become ready within timeout. CNPG may still be reconciling.", targetPod)
	return result, nil
}

// resolvePVC finds the PVC name for a given CNPG pod.
func (w *cnpgPruner) resolvePVC(ctx context.Context, podName string) (string, error) {
	c := k8s.GetClients()
	cfg := w.p.Config()
	pod, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod might be gone (fenced/deleted), try naming convention
		// CNPG PVC name = pod name
		return podName, nil
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "pgdata" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName, nil
		}
	}
	// Fallback: CNPG convention is PVC name = pod name
	return podName, nil
}

// discoverPostgresInfo finds the postgres image, UID, and GID from a healthy instance.
func (w *cnpgPruner) discoverPostgresInfo(ctx context.Context, triageResult *model.TriageResult) (image, uid, gid string, err error) {
	c := k8s.GetClients()
	cfg := w.p.Config()
	ns := cfg.Namespace
	primary := k8s.GetNestedString(w.p.Cluster(), "status", "currentPrimary")

	// Find a non-primary pod that is Running and Ready
	for _, a := range triageResult.Assessments {
		if a.Pod == primary {
			continue
		}
		pod, podErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, a.Pod, metav1.GetOptions{})
		if podErr != nil {
			continue
		}
		if !k8s.PodReady(*pod, "postgres") {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "postgres" {
				image = container.Image
				break
			}
		}
		if image == "" {
			continue
		}

		// Get UID/GID from running process
		uidResult, uidErr := k8s.ExecCommand(ctx, a.Pod, ns, "postgres", []string{"id", "-u"})
		gidResult, gidErr := k8s.ExecCommand(ctx, a.Pod, ns, "postgres", []string{"id", "-g"})
		if uidErr == nil && gidErr == nil {
			uid = strings.TrimSpace(uidResult.Stdout)
			gid = strings.TrimSpace(gidResult.Stdout)
			if uid != "" && gid != "" {
				return image, uid, gid, nil
			}
		}
	}

	// Fallback to cluster spec image
	image = k8s.GetNestedString(w.p.Cluster(), "status", "image")
	if image == "" {
		return "", "", "", fmt.Errorf("could not determine postgres image from cluster")
	}
	return image, "26", "26", nil // default postgres UID/GID
}

const (
	pgDataDir = "/var/lib/postgresql/data/pgdata"
	pgWALDir  = pgDataDir + "/pg_wal"
)

// pruneWALOnPVC performs the WAL prune as Go-driven exec into the helper pod that holds
// the target's PVC. Every decision — the checkpoint boundary and the replica-LSN safety
// gate (#23) — is made here in Go and is unit-testable via the exec hook; the helper pod
// is a dumb `sleep infinity` exec target.
func (w *cnpgPruner) pruneWALOnPVC(ctx context.Context, helperPod, ns string, readyReplicas []string, verifyReplicas bool) error {
	// 1. Read the checkpoint REDO boundary from pg_controldata on the mounted PVC. This
	//    is the TARGET's own boundary, so the deletion is always safe for the target's
	//    own crash recovery, primary or not.
	redoWAL, redoLSN, err := w.readCheckpointBoundary(ctx, helperPod, ns)
	if err != nil {
		return err
	}
	output.Success("Checkpoint REDO WAL file: %s (LSN %s)", redoWAL, redoLSN)

	// 2. Safety gate (#23), primary-target only: no ready replica may lag behind the
	//    checkpoint — a replica that still needs pre-checkpoint WAL streams FROM the
	//    primary and would break if those segments vanish. A trapped non-primary
	//    authority (P3.4) has no downstream streamer (it is crash-looping), so this gate
	//    is inapplicable and the own-REDO boundary from step 1 is the safety.
	if verifyReplicas {
		if err := w.assertReplicasCaughtUp(ctx, ns, readyReplicas, redoLSN); err != nil {
			return err
		}
	} else {
		output.Success("Non-primary authority relief: skipping the replica-caughtup gate " +
			"(nothing streams from a crash-looping non-primary); deleting only WAL older than the target's own checkpoint REDO")
	}

	// 3. Delete WAL segments older than the checkpoint.
	return w.deleteWALOlderThan(ctx, helperPod, ns, redoWAL)
}

// assertReliefEligible authorizes WAL relief on a NON-primary target (P3.4). It is only
// for a trapped DATA AUTHORITY: the proven authority (or, in a divergence the operator
// has adjudicated, a --force-designated survivor). A non-primary that is neither is a
// disposable replica — the fix there is to re-clone it (`hasteward repair`), not to
// prune its WAL — so this refuses, closing the door on pruning a node by mistake.
func (w *cnpgPruner) assertReliefEligible(targetPod string, a *model.InstanceAssessment, tr *model.TriageResult) error {
	dc := tr.DataComparison
	isAuthority := dc.MostAdvanced == targetPod || a.Classification == model.ClassAuthoritative
	switch {
	case isAuthority:
		common.WarnLog("P3.4: %s is a non-primary DATA AUTHORITY and not ready — relieving its OWN pre-checkpoint WAL "+
			"so it can start for inspection/promotion. Only WAL older than its own checkpoint REDO is deleted; nothing streams from it.", targetPod)
		return nil
	case dc.Authority == model.AuthorityDiverged:
		if !w.p.Config().Force {
			return fmt.Errorf("ABORT: %s is not the primary and the cluster is DIVERGED (no single proven authority). "+
				"If you have reviewed the divergence evidence and chosen %s as the surviving lineage, re-run with --force "+
				"to relieve its WAL; otherwise escrow and inspect first", targetPod, targetPod)
		}
		common.WarnLog("force=true — relieving WAL on %s in a DIVERGED cluster (operator-designated survivor)", targetPod)
		return nil
	default:
		return fmt.Errorf("ABORT: %s is not the primary and not the data authority (classification=%q). WAL relief for a "+
			"non-primary is only for a trapped AUTHORITY — a disposable replica should be re-cloned (hasteward repair), not WAL-pruned",
			targetPod, a.Classification)
	}
}

// readCheckpointBoundary execs pg_controldata and parses the checkpoint REDO WAL file
// and its LSN — the boundary before which WAL is deletable.
func (w *cnpgPruner) readCheckpointBoundary(ctx context.Context, helperPod, ns string) (redoWAL, redoLSN string, err error) {
	res, err := k8s.ExecCommand(ctx, helperPod, ns, "wal-prune", []string{"pg_controldata", pgDataDir})
	if err != nil {
		return "", "", fmt.Errorf("pg_controldata failed: %w", err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch {
		case strings.Contains(line, "REDO WAL file"):
			redoWAL = lastField(line)
		case strings.Contains(line, "REDO location"):
			redoLSN = lastField(line)
		}
	}
	if redoWAL == "" {
		return "", "", fmt.Errorf("could not determine checkpoint REDO WAL file from pg_controldata")
	}
	if redoLSN == "" {
		return "", "", fmt.Errorf("could not determine checkpoint REDO location (LSN) from pg_controldata")
	}
	return redoWAL, redoLSN, nil
}

// assertReplicasCaughtUp enforces the prune precondition: every ready replica must have
// replayed to at least the checkpoint LSN. Aborts otherwise (unless --force), naming the
// laggards. With no replica to verify against, it also refuses unless --force.
func (w *cnpgPruner) assertReplicasCaughtUp(ctx context.Context, ns string, replicas []string, redoLSN string) error {
	force := w.p.Config().Force
	if len(replicas) == 0 {
		if !force {
			return fmt.Errorf("ABORT: no ready replicas to verify WAL safety against — cannot confirm the WAL is deadweight. Re-run with --force to override")
		}
		common.WarnLog("force=true — pruning with NO replica to verify against; data safety cannot be confirmed")
		return nil
	}
	redo, err := parseLSN(redoLSN)
	if err != nil {
		return fmt.Errorf("cannot parse checkpoint REDO LSN: %w", err)
	}
	var lagging []string
	for _, r := range replicas {
		res, err := k8s.ExecCommand(ctx, r, ns, "postgres",
			[]string{"psql", "-U", "postgres", "-d", "postgres", "-tAc", "SELECT pg_last_wal_replay_lsn()"})
		if err != nil {
			return fmt.Errorf("could not query replay LSN on replica %s: %w", r, err)
		}
		replayStr := strings.TrimSpace(res.Stdout)
		if replayStr == "" {
			return fmt.Errorf("replica %s returned an empty replay LSN — cannot verify it is caught up", r)
		}
		replay, err := parseLSN(replayStr)
		if err != nil {
			return fmt.Errorf("replica %s replay LSN: %w", r, err)
		}
		if replay < redo {
			lagging = append(lagging, fmt.Sprintf("%s (replay %s < checkpoint %s)", r, replayStr, redoLSN))
		}
	}
	if len(lagging) > 0 {
		if !force {
			return fmt.Errorf("ABORT: %d replica(s) lag behind the checkpoint and still need pre-checkpoint WAL — pruning would break their replication: %s. Re-run with --force to override",
				len(lagging), strings.Join(lagging, "; "))
		}
		common.WarnLog("force=true — pruning despite lagging replica(s): %s", strings.Join(lagging, "; "))
		return nil
	}
	output.Success("All %d ready replica(s) are at or past the checkpoint — safe to prune", len(replicas))
	return nil
}

// deleteWALOlderThan lists the 24-hex-char WAL segments and deletes exactly those older
// than the checkpoint REDO segment (the delete decision is made in Go), preserving
// .history files (pg_rewind needs them) and clearing stale .partial/.backup bookkeeping.
func (w *cnpgPruner) deleteWALOlderThan(ctx context.Context, helperPod, ns, redoWAL string) error {
	res, err := k8s.ExecCommand(ctx, helperPod, ns, "wal-prune",
		[]string{"sh", "-c", fmt.Sprintf("find %s -maxdepth 1 -type f -regex '.*/[0-9A-F]\\{24\\}$'", pgWALDir)})
	if err != nil {
		return fmt.Errorf("listing WAL segments failed: %w", err)
	}
	var toDelete []string
	kept := 0
	for _, path := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		base := path[strings.LastIndex(path, "/")+1:]
		// WAL segment filenames are fixed-width hex, so a lexical compare matches
		// PostgreSQL's numeric ordering. Delete strictly older than the REDO segment;
		// keep the REDO segment and everything newer.
		if base < redoWAL {
			toDelete = append(toDelete, path)
		} else {
			kept++
		}
	}
	if len(toDelete) == 0 {
		output.Success("No WAL segments older than %s — nothing to prune (kept %d)", redoWAL, kept)
		output.Info("Nothing was pre-checkpoint deadweight. If this instance is disk-full and cannot start, its WAL is all "+
			"POST-checkpoint (needed for crash recovery, NOT deletable) — that is a disk-full DEADLOCK. Use " +
			"`prune-wal --deadlock-recover` to replay + recycle it in place.")
	} else {
		if _, err := k8s.ExecCommand(ctx, helperPod, ns, "wal-prune", append([]string{"rm", "-f"}, toDelete...)); err != nil {
			return fmt.Errorf("deleting %d WAL segment(s) failed: %w", len(toDelete), err)
		}
		output.Success("Deleted %d WAL segment(s) older than %s, kept %d", len(toDelete), redoWAL, kept)
	}
	// Stale bookkeeping files are safe to drop; .history is preserved for pg_rewind.
	_, _ = k8s.ExecCommand(ctx, helperPod, ns, "wal-prune",
		[]string{"sh", "-c", fmt.Sprintf(`find %s -maxdepth 1 -type f \( -name '*.partial' -o -name '*.backup' \) -delete`, pgWALDir)})
	return nil
}

// lastField returns the last whitespace-separated token of a line (the value column of
// pg_controldata's "key:   value" rows).
func lastField(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// parseLSN parses a PostgreSQL LSN ("XXX/YYY" hex) into a comparable uint64.
func parseLSN(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("malformed LSN %q", s)
	}
	hi, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed LSN %q: %w", s, err)
	}
	lo, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed LSN %q: %w", s, err)
	}
	return hi<<32 | lo, nil
}

// parseInt64 parses a string to int64, returning 0 on failure.
func parseInt64(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}
