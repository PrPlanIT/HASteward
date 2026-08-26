package prunewal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/cnpgjob"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const scratchWALDir = "/scratch/wal"

var (
	volumeSnapshotGVR      = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
	volumeSnapshotClassGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
)

// deadlockRecover (P3.6) rehabilitates a CNPG instance that is disk-full DEADLOCKED: too
// full to start, so it can never checkpoint to recycle its own WAL, so it stays full —
// a self-sustaining freeze (boundary-postgres-2 sat here for weeks). The fix, proven by
// hand first: escrow the PVC (VolumeSnapshot), fence + isolate the instance, relocate
// pg_wal to ephemeral scratch to free the data disk, run a SINGLE-USER crash-recovery
// REPLAY + CHECKPOINT (which applies all committed WAL — NO data loss — then recycles the
// old segments), pg_archivecleanup the pre-checkpoint deadweight, and move the now-small
// WAL back. The datadir ends clean ("shut down"), de-bloated, on the SAME PV — nothing
// deleted or grown. A trapped non-primary AUTHORITY is KEPT FENCED afterwards (never
// handed back to the operator, which would pg_rewind it onto the stale primary).
func (w *cnpgPruner) deadlockRecover(ctx context.Context, targetPod, targetPVC string, isPrimary bool,
	tr *model.TriageResult, result *model.PruneWALResult) (*model.PruneWALResult, error) {

	cfg := w.p.Config()
	ns := cfg.Namespace
	keepFenced := !isPrimary

	output.Section("Phase 3: Disk-full deadlock recovery (replay + recycle in place)")

	if cfg.DryRun {
		tail := "then judge authority on the post-replay position: RELEASE it to the operator ONLY if a replica is provably caught up (else KEEP IT FENCED so CNPG cannot promote a behind replica over it) — never force targetPrimary"
		if keepFenced {
			tail = "then KEEP IT FENCED (isolated from operator reconcile so it is NOT pg_rewound onto the stale primary)"
		}
		output.Info("DRY RUN: deadlock-recover on %s (PVC %s) — would ESCROW it (VolumeSnapshot), fence + isolate it, "+
			"relocate pg_wal to scratch, run a single-user crash-recovery REPLAY + CHECKPOINT (applies all committed WAL, "+
			"no data loss), pg_archivecleanup the recycled segments, move the small WAL back, %s. No changes made.",
			targetPod, targetPVC, tail)
		return result, nil
	}

	// 1. ESCROW: snapshot the PVC before any mutation. This is the rollback point — the
	//    replay is non-destructive of committed data, but a durable escrow means even a
	//    mid-operation pod death (with WAL on ephemeral scratch) is recoverable. --force
	//    skips it (loudly); refuse rather than proceed blind if no snapshot class is found.
	if cfg.Force {
		common.WarnLog("force=true — SKIPPING the escrow VolumeSnapshot for %s. The replay is non-destructive of committed "+
			"data, but there is NO rollback point if the operation is interrupted.", targetPVC)
	} else {
		snap, err := w.escrowSnapshot(ctx, ns, targetPVC)
		if err != nil {
			return nil, fmt.Errorf("deadlock-recover REFUSED: could not escrow %s before mutating it (fix the "+
				"VolumeSnapshotClass, pass --snapshot-class, or --force to skip escrow): %w", targetPVC, err)
		}
		output.Success("Escrow VolumeSnapshot %s is ready — rollback point captured", snap)
	}

	// 2. Prerequisites. Image comes from the cluster spec (reliable even when no healthy
	//    peer exists); CNPG runs postgres as uid/gid 26. Scratch is sized to the PVC's
	//    capacity (+headroom): the WAL was ON that PVC, so it cannot exceed it.
	imageName := k8s.GetNestedString(w.p.Cluster(), "spec", "imageName")
	if imageName == "" {
		return nil, fmt.Errorf("deadlock-recover: cannot determine cluster imageName for the helper pod")
	}
	scratchSize, err := w.scratchSizeForPVC(ctx, ns, targetPVC)
	if err != nil {
		return nil, fmt.Errorf("deadlock-recover: %w", err)
	}

	uid := int64(26)
	podName := fmt.Sprintf("%s-deadlock-recover-%d", cfg.ClusterName, time.Now().Unix())
	helper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			// A sidecar would strand the helper Running so the PVC-acquire wait never
			// settles (the shared Istio-exemption the helper-pod builder applies).
			Annotations: map[string]string{"sidecar.istio.io/inject": "false"},
			Labels:      map[string]string{"hasteward": "deadlock-recover"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
			},
			Containers: []corev1.Container{{
				Name:    "rehab",
				Image:   imageName,
				Command: []string{"sh", "-c", "sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
					{Name: "scratch", MountPath: "/scratch"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "pgdata", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: targetPVC}}},
				{Name: "scratch", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratchSize}}},
			},
		},
	}

	// 3. Fence → disable reconcile → acquire PVC → (replay+recycle in Go) → ALWAYS keep it
	//    fenced through cnpgjob so its data is preserved while we judge authority. The
	//    release-or-guard decision is made below on the POST-replay position — never before
	//    (a disk-full instance is unreadable). While reconcile is disabled (peers stable), the
	//    OnPVCAcquired callback also reads the replicas for that decision.
	output.Bullet(0, "Fence, acquire the PVC, replay + recycle WAL, and restore")
	var recovered instancePos
	var replicas []instancePos
	if err := cnpgjob.Run(ctx, cnpgjob.OfflinePVCJob{
		Namespace:        ns,
		ClusterName:      cfg.ClusterName,
		TargetPod:        targetPod,
		TargetPVC:        targetPVC,
		HelperPod:        helper,
		HelperPodName:    podName,
		Label:            "deadlock-recover",
		DeleteTimeoutSec: cfg.DeleteTimeout,
		KeepFenced:       true, // unfence deliberately below, and only when releasing is proven safe
		OnPVCAcquired: func(ctx context.Context) error {
			tl, ckLSN, oerr := w.deadlockRecoverOnPVC(ctx, podName, ns)
			if oerr != nil {
				return oerr
			}
			recovered = instancePos{pod: targetPod, timeline: tl, lsn: ckLSN}
			if isPrimary {
				replicas = w.readReplicaPositions(ctx, ns, targetPod)
			}
			return nil
		},
	}); err != nil {
		return nil, err
	}

	// A recovered NON-primary authority is always kept fenced: handing it to a live operator
	// would let CNPG pg_rewind it onto the stale primary's lineage, destroying the data we
	// just preserved. Promote it deliberately.
	if !isPrimary {
		output.Success("Deadlock cleared on %s: WAL replayed + recycled, datadir clean and de-bloated on the SAME PV. "+
			"It is KEPT FENCED (isolated) so CNPG cannot pg_rewind it onto the stale primary. Promote it deliberately: "+
			"hasteward repair -e cnpg -c %s -n %s --instance <ordinal-of-%s> --promote.",
			targetPod, cfg.ClusterName, ns, targetPod)
		return result, nil
	}

	// A recovered PRIMARY: hand it back ONLY if a replica is provably caught up — then no
	// outcome the operator can reach loses data (it promotes an equal-or-newer peer, or
	// restarts the recovered instance as primary). Otherwise the recovered instance is the
	// authority a behind replica must NOT be promoted over: KEEP IT FENCED.
	output.Success("Deadlock cleared on %s: WAL replayed + recycled in place on its (same, now de-bloated) PVC (timeline %d, redo done at %s).",
		targetPod, recovered.timeline, formatLSNU(recovered.lsn))
	safe := safeToReleaseRecoveredPrimary(recovered, replicas)
	output.Bullet(0, "Authority check: %d readable replica(s), safe-to-release=%v", len(replicas), safe)
	if !safe {
		output.Info("No replica is provably caught up with %s — it holds committed data the replicas lack. KEEPING IT FENCED so CNPG cannot "+
			"promote a behind replica over it. Promote the recovered authority deliberately: hasteward repair -e cnpg -c %s -n %s "+
			"--instance <ordinal-of-%s> --promote.", targetPod, cfg.ClusterName, ns, targetPod)
		return result, nil
	}

	// Safe to release: a caught-up replica exists. Unfence and let CNPG settle — never force.
	output.Bullet(0, "A caught-up replica exists — releasing %s to the operator", targetPod)
	if err := cnpgjob.Unfence(ctx, ns, cfg.ClusterName, targetPod); err != nil {
		return result, fmt.Errorf("recovered %s is safe to release but unfencing it failed — it is still fenced and unmanaged by CNPG; "+
			"clear it manually (kubectl annotate cluster %s -n %s cnpg.io/fencedInstances-) then re-triage: %w",
			targetPod, cfg.ClusterName, ns, err)
	}
	w.watchAndReport(ctx, targetPod)
	return result, nil
}

// instancePos is one instance's data position for the release/guard decision: its timeline
// and its LSN (the recovered instance's recovery "redo done at" endpoint; a running replica's
// REPLAY LSN — both are "how far its committed data goes", on the same timeline and units).
type instancePos struct {
	pod      string
	timeline int64
	lsn      uint64
}

// safeToReleaseRecoveredPrimary decides whether a deadlock-recovered PRIMARY can be handed
// back to the operator WITHOUT risking data loss. The operator, seeing the recovered
// instance fenced, may promote the most-advanced healthy replica — so releasing is safe
// ONLY when some replica is provably not behind the recovered instance: same timeline AND
// replayed at least as far as the recovered instance's "redo done at" endpoint. Then whichever
// replica CNPG promotes holds everything the recovered instance holds — no loss.
//
// Fail-safe to GUARD (false): a replica on a DIFFERENT timeline is a divergence (a failover
// already forked away — comparing LSNs across the fork is not "caught up"); a replica behind
// on the same timeline would lose the recovered instance's unrecovered commits; and no
// readable replica at all means we cannot prove safety. In every unprovable case we keep the
// recovered authority fenced rather than let a behind replica win. Pure for unit testing.
func safeToReleaseRecoveredPrimary(recovered instancePos, replicas []instancePos) bool {
	for _, r := range replicas {
		if r.timeline == recovered.timeline && r.lsn >= recovered.lsn {
			return true
		}
	}
	return false
}

// formatLSNU renders a pg_lsn value as the canonical hex "X/Y" for log lines.
func formatLSNU(lsn uint64) string { return fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF) }

// readReplicaPositions queries every RUNNING instance except excludePod for its timeline and
// data LSN (replay LSN on a standby, current WAL LSN on a primary) — the "how far its data
// goes" that safeToReleaseRecoveredPrimary compares against the recovered instance. Called
// while reconcile is DISABLED (peers stable, no failover in flight). Unreadable/not-ready
// instances are skipped: their absence makes the release check fail closed to GUARD.
func (w *cnpgPruner) readReplicaPositions(ctx context.Context, ns, excludePod string) []instancePos {
	c := k8s.GetClients()
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: w.p.PodSelector()})
	if err != nil {
		common.WarnLog("release check: could not list instance pods (%v) — will GUARD", err)
		return nil
	}
	const q = "SELECT (SELECT timeline_id FROM pg_control_checkpoint())::text || ' ' || " +
		"COALESCE(pg_last_wal_replay_lsn(), pg_current_wal_lsn())::text;"
	var out []instancePos
	for i := range pods.Items {
		pod := pods.Items[i]
		if pod.Name == excludePod || !k8s.PodReady(pod, "postgres") {
			continue
		}
		res, execErr := k8s.ExecCommand(ctx, pod.Name, ns, "postgres", []string{"psql", "-U", "postgres", "-tAqc", q})
		if execErr != nil {
			common.WarnLog("release check: could not read position of %s (%v) — skipping", pod.Name, execErr)
			continue
		}
		fields := strings.Fields(strings.TrimSpace(res.Stdout))
		if len(fields) != 2 {
			continue
		}
		tl, tErr := strconv.ParseInt(fields[0], 10, 64)
		lsn, lErr := parseLSN(fields[1])
		if tErr != nil || lErr != nil {
			continue
		}
		out = append(out, instancePos{pod: pod.Name, timeline: tl, lsn: lsn})
	}
	return out
}

// watchAndReport is the tail after a deadlock-recovered PRIMARY is RELEASED (unfenced) —
// only reached when the authority check proved a replica is caught up, so no outcome CNPG
// can reach loses data. It does NOT force anything (the old code realigned targetPrimary,
// which fought CNPG's correct promotion of a caught-up replica). It watches the cluster
// settle and reports which instance leads: the recovered one returns as primary, or a
// caught-up peer is promoted and the recovered one re-clones as a replica — both loss-free.
// A slow settle is reported (informational), never forced — the escrow snapshot is intact.
func (w *cnpgPruner) watchAndReport(ctx context.Context, recovered string) {
	output.Section("Phase 4: Watching the cluster settle")
	output.Bullet(0, "Released %s to the operator (a caught-up replica exists) — watching for a healthy primary, not forcing one", recovered)

	primary, state, ok := w.waitClusterSettled(ctx, 18, 10) // ~3 min
	if ok {
		if primary == recovered {
			output.Success("Cluster settled: %s returned as primary (%s). No data loss, no PVC growth.", recovered, state)
		} else {
			output.Success("Cluster settled: %s is primary and %s re-cloned as a replica (%s) — CNPG promoted a caught-up peer, no data loss.", primary, recovered, state)
		}
		return
	}
	common.WarnLog("Cluster not yet healthy (%s). The WAL recovery is durable and the escrow snapshot is intact — a slow settle, not data loss. Re-triage if it persists.", state)
}

// waitClusterSettled polls until the cluster is healthy: all instances ready, currentPrimary
// == targetPrimary, and that primary's postgres container Ready. Returns the primary, the
// last status line, and whether it settled.
func (w *cnpgPruner) waitClusterSettled(ctx context.Context, attempts, intervalSec int) (string, string, bool) {
	cfg := w.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()
	last := "unknown"
	for i := 0; i < attempts; i++ {
		cl, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
		if err == nil {
			cur := k8s.GetNestedString(cl, "status", "currentPrimary")
			tgt := k8s.GetNestedString(cl, "status", "targetPrimary")
			phase := k8s.GetNestedString(cl, "status", "phase")
			ready := k8s.GetNestedInt64(cl, "status", "readyInstances")
			last = fmt.Sprintf("phase=%q currentPrimary=%s targetPrimary=%s readyInstances=%d", phase, cur, tgt, ready)
			if cur != "" && cur == tgt && ready >= w.p.Instances() {
				if pod, perr := c.Clientset.CoreV1().Pods(ns).Get(ctx, cur, metav1.GetOptions{}); perr == nil && k8s.PodReady(*pod, "postgres") {
					return cur, last, true
				}
			}
		}
		common.Sleep(time.Duration(intervalSec) * time.Second)
	}
	return "", last, false
}

// deadlockRecoverOnPVC runs the replay+recycle sequence via Go-driven exec into the helper
// (which holds the fenced PVC + scratch). Pure over ExecCommand so it is unit-testable with
// the exec hook. Order matters for safety: the WAL is only removed from scratch AFTER the
// replay+checkpoint has applied every committed record to the data files on the PVC.
func (w *cnpgPruner) deadlockRecoverOnPVC(ctx context.Context, helper, ns string) (int64, uint64, error) {
	const c = "rehab"
	// 1. Canonical datadir perms (0700) — an fsGroup mount loosens them and postgres refuses
	//    to start on "invalid permissions". Then relocate pg_wal → scratch (idempotent if a
	//    prior attempt already symlinked it), freeing the data disk for the replay.
	reloc := fmt.Sprintf(`set -e
chmod 700 %[1]s
if [ -L %[1]s/pg_wal ]; then
  echo "pg_wal already relocated"
else
  mkdir -p %[2]s && chmod 700 %[2]s
  mv %[1]s/pg_wal/* %[2]s/ 2>/dev/null || true
  rmdir %[1]s/pg_wal
  ln -s %[2]s %[1]s/pg_wal
fi
df -Pm %[1]s | tail -1`, pgDataDir, scratchWALDir)
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", reloc}); err != nil {
		return 0, 0, fmt.Errorf("relocating pg_wal to scratch failed: %w", err)
	}

	// 2. Single-user crash-recovery REPLAY + CHECKPOINT. Bounded WAL so recycling is tight.
	//    Replays every committed record into the data files (no data loss), then a clean
	//    shutdown checkpoint marks the pre-checkpoint WAL recyclable.
	replay := fmt.Sprintf(`echo "CHECKPOINT;" | postgres --single -D %s -c min_wal_size=32MB -c max_wal_size=128MB postgres`, pgDataDir)
	res, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", replay})
	if err != nil {
		return 0, 0, fmt.Errorf("single-user replay failed (datadir left fenced for inspection): %w", err)
	}
	if strings.Contains(res.Stdout+res.Stderr, "PANIC") || strings.Contains(res.Stdout+res.Stderr, "FATAL") {
		return 0, 0, fmt.Errorf("single-user replay reported PANIC/FATAL — refusing to trim WAL: %s", strings.TrimSpace(res.Stderr))
	}

	// The authority position is where crash recovery finished replaying committed WAL
	// ("redo done at X"), NOT the end-of-recovery checkpoint postgres writes just past it. That
	// checkpoint record sits one record beyond every replica's streamed position, so comparing
	// it would judge even a fully-caught-up replica "behind" and the release gate would never
	// fire. "redo done at" is on the same timeline and in the same units as a standby's
	// pg_last_wal_replay_lsn, so the two are directly comparable.
	authorityLSN, ok := parseRedoDoneLSN(res.Stdout + res.Stderr)
	if !ok {
		// Recovery reported no redo endpoint (nothing to replay) — the authority position
		// cannot be bounded, so fail safe: a max LSN makes the release gate GUARD.
		common.WarnLog("deadlock-recover: no \"redo done at\" in the replay output — cannot prove the authority position; the release gate will GUARD")
		authorityLSN = ^uint64(0)
	}

	// 3. Confirm the instance is now cleanly shut down, and read the new checkpoint REDO WAL
	//    file — the exact cutoff for archivecleanup (everything older is deadweight).
	cd, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"pg_controldata", pgDataDir})
	if err != nil {
		return 0, 0, fmt.Errorf("post-replay pg_controldata failed: %w", err)
	}
	state, redoWAL, timeline := parseControlState(cd.Stdout)
	if state != "shut down" {
		return 0, 0, fmt.Errorf("post-replay state is %q, not \"shut down\" — refusing to trim WAL (datadir left fenced)", state)
	}
	if redoWAL == "" {
		return 0, 0, fmt.Errorf("could not read the post-replay checkpoint REDO WAL file — refusing to trim WAL")
	}

	// 4. Remove pre-checkpoint WAL (keeps the REDO segment + newer, preserves .history).
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"pg_archivecleanup", scratchWALDir, redoWAL}); err != nil {
		return 0, 0, fmt.Errorf("pg_archivecleanup failed: %w", err)
	}

	// 5. Move the now-small WAL back onto the PVC and restore the normal layout.
	moveback := fmt.Sprintf(`set -e
rm %[1]s/pg_wal
mv %[2]s %[1]s/pg_wal
chmod 700 %[1]s/pg_wal
df -Pm %[1]s | tail -1`, pgDataDir, scratchWALDir)
	if _, err := k8s.ExecCommand(ctx, helper, ns, c, []string{"sh", "-c", moveback}); err != nil {
		return 0, 0, fmt.Errorf("moving recycled WAL back onto the PVC failed: %w", err)
	}
	return timeline, authorityLSN, nil
}

// parseControlState extracts, from pg_controldata output, the cluster state, the checkpoint
// REDO WAL file (the archivecleanup cutoff), and the recovered instance's TimeLineID (the
// authority TIMELINE the release/guard decision compares against — the authority LSN comes
// from the recovery "redo done at" endpoint, not the control file's checkpoint location).
func parseControlState(out string) (state, redoWAL string, timeline int64) {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Database cluster state:"):
			state = strings.TrimSpace(strings.SplitN(t, ":", 2)[1])
		case strings.Contains(t, "REDO WAL file"):
			redoWAL = lastField(t)
		case strings.HasPrefix(t, "Latest checkpoint's TimeLineID:"):
			timeline, _ = strconv.ParseInt(strings.TrimSpace(strings.SplitN(t, ":", 2)[1]), 10, 64)
		}
	}
	return state, redoWAL, timeline
}

// parseRedoDoneLSN pulls the "redo done at X/Y" LSN from single-user crash-recovery output —
// the end of the committed WAL the instance replayed, which is what a caught-up standby's
// pg_last_wal_replay_lsn must reach. Absent when recovery replayed nothing ("redo is not
// required"); the caller fails safe to GUARD in that case.
func parseRedoDoneLSN(out string) (uint64, bool) {
	const marker = "redo done at "
	i := strings.Index(out, marker)
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(out[i+len(marker):])
	if len(fields) == 0 {
		return 0, false
	}
	lsn, err := parseLSN(strings.TrimRight(fields[0], ",;"))
	if err != nil {
		return 0, false
	}
	return lsn, true
}

// scratchSizeForPVC sizes the ephemeral scratch volume to the target PVC's capacity plus
// headroom. The relocated WAL lived ON that PVC, so it cannot exceed the PVC's size.
func (w *cnpgPruner) scratchSizeForPVC(ctx context.Context, ns, pvcName string) (resource.Quantity, error) {
	pvc, err := k8s.GetClients().Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("cannot read PVC %s capacity for scratch sizing: %w", pvcName, err)
	}
	cap := pvc.Status.Capacity[corev1.ResourceStorage]
	if cap.IsZero() {
		cap = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	if cap.IsZero() {
		return resource.MustParse("2Gi"), nil // conservative fallback
	}
	out := cap.DeepCopy()
	out.Add(resource.MustParse("512Mi"))
	return out, nil
}

// escrowSnapshot creates a VolumeSnapshot of the PVC and waits until it is readyToUse —
// the rollback point for the in-place replay. The VolumeSnapshotClass is taken from
// --snapshot-class, or discovered by matching the PVC's storage provisioner.
func (w *cnpgPruner) escrowSnapshot(ctx context.Context, ns, pvcName string) (string, error) {
	cfg := w.p.Config()
	c := k8s.GetClients()

	class := cfg.SnapshotClass
	if class == "" {
		var err error
		class, err = w.discoverSnapshotClass(ctx, ns, pvcName)
		if err != nil {
			return "", err
		}
	}

	name := fmt.Sprintf("%s-deadlock-escrow-%d", pvcName, time.Now().Unix())
	snap := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata": map[string]interface{}{
			"name": name, "namespace": ns,
			"labels": map[string]interface{}{"hasteward": "deadlock-escrow", "hasteward.pvc": pvcName},
		},
		"spec": map[string]interface{}{
			"volumeSnapshotClassName": class,
			"source":                  map[string]interface{}{"persistentVolumeClaimName": pvcName},
		},
	}}
	if _, err := c.Dynamic.Resource(volumeSnapshotGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating VolumeSnapshot (class %s): %w", class, err)
	}

	output.Bullet(0, "Waiting for escrow snapshot %s (class %s) to be ready", name, class)
	for i := 0; i < 60; i++ { // up to 5 min
		common.Sleep(5 * time.Second)
		got, err := c.Dynamic.Resource(volumeSnapshotGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if k8s.GetNestedBool(got, "status", "readyToUse") {
			return name, nil
		}
	}
	return "", fmt.Errorf("VolumeSnapshot %s did not become readyToUse within 5m", name)
}

// discoverSnapshotClass finds a VolumeSnapshotClass whose driver matches the PVC's storage
// provisioner. Errors (rather than guessing) when none is found.
func (w *cnpgPruner) discoverSnapshotClass(ctx context.Context, ns, pvcName string) (string, error) {
	c := k8s.GetClients()
	pvc, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading PVC %s: %w", pvcName, err)
	}
	scName := ""
	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	if scName == "" {
		return "", fmt.Errorf("PVC %s has no StorageClass — pass --snapshot-class explicitly", pvcName)
	}
	sc, err := c.Clientset.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading StorageClass %s: %w", scName, err)
	}
	provisioner := sc.Provisioner

	list, err := c.Dynamic.Resource(volumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing VolumeSnapshotClasses (is the snapshot CRD installed?): %w", err)
	}
	for _, item := range list.Items {
		if driver, ok, _ := unstructured.NestedString(item.Object, "driver"); ok && driver == provisioner {
			return item.GetName(), nil
		}
	}
	return "", fmt.Errorf("no VolumeSnapshotClass matches the PVC provisioner %q — pass --snapshot-class", provisioner)
}
