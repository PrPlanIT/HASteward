package escrow

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/restic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// escrowHelperImage runs the read-only tar that streams a PVC's files to the
// host-side restic. busybox tar is sufficient and matches the bootstrap helper.
const escrowHelperImage = "docker.io/library/busybox:latest"

// escrowMountPath is where the captured PVC is mounted (read-only) in the helper.
const escrowMountPath = "/escrow-data"

// resticPVCEscrow escrows PVCs as a files tarball in the configured restic repo —
// the fallback when no CSI snapshot driver matches. It is a PVC-FILES backup (raw
// datadir), NOT pg_dumpall: it works against a down/stranded instance.
//
// Verify is deliberately STRONG. This gates a DESTRUCTIVE datadir clear, so
// "Verified" must mean "I proved I can get the bytes back" — not "a repo entry
// exists". It therefore restores the actual archive bytes through restic and
// parses them as a tar (proving both retrieval and integrity), never a snapshot
// listing.
type resticPVCEscrow struct {
	cfg   *common.Config
	runID string
}

func newResticPVCEscrow(cfg *common.Config, runID string) *resticPVCEscrow {
	return &resticPVCEscrow{cfg: cfg, runID: runID}
}

func (e *resticPVCEscrow) Name() string { return "resticpvc" }

// newResticClient builds the restic client from config, mirroring backup/cnpg.go.
func (e *resticPVCEscrow) newResticClient() *restic.Client {
	return restic.NewClient(e.cfg.BackupsPath, e.cfg.ResticPassword)
}

func (e *resticPVCEscrow) Capture(ctx context.Context, recoverySet []string) ([]EscrowRef, error) {
	ns := e.cfg.Namespace
	rc := e.newResticClient()

	output.Section("Escrow Capture (resticpvc)")
	output.Field("Repository", e.cfg.BackupsPath)

	if err := rc.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize restic repository: %w", err)
	}

	sa := e.serviceAccount(ctx, ns)

	refs := make([]EscrowRef, 0, len(recoverySet))
	for _, pvc := range recoverySet {
		ref, err := e.captureOne(ctx, rc, pvc, sa)
		if err != nil {
			return refs, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// captureOne mounts one PVC read-only in a helper pod and streams its files
// (tar) into restic backup --stdin on the host — the pg_dumpall pattern from
// backup/cnpg.go, with tar of the raw datadir instead of a logical dump.
func (e *resticPVCEscrow) captureOne(ctx context.Context, rc *restic.Client, pvc, sa string) (EscrowRef, error) {
	c := k8s.GetClients()
	ns := e.cfg.Namespace
	now := time.Now()

	helperName := escrowName(pvc, e.runID)
	root := int64(0) // read every file regardless of owner uid (postgres/mysql)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "escrow-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sa,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &root,
			},
			// Stay alive so we can exec the tar stream; deleted in defer.
			Containers: []corev1.Container{{
				Name:    "escrow",
				Image:   escrowHelperImage,
				Command: []string{"sleep", "3600"},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: escrowMountPath,
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvc,
					},
				},
			}},
		},
	}

	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return EscrowRef{}, fmt.Errorf("failed to create escrow helper pod for pvc %s: %w", pvc, err)
	}
	defer func() {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, helperName, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
	}()

	if err := e.waitRunning(ctx, helperName); err != nil {
		return EscrowRef{}, fmt.Errorf("escrow helper pod for pvc %s not ready: %w", pvc, err)
	}

	// tar the read-only datadir → restic backup --stdin (host-side restic).
	reader, wait := k8s.ExecPipeOut(ctx, helperName, ns, "escrow",
		[]string{"tar", "-cf", "-", "-C", escrowMountPath, "."})

	tags := e.tags(pvc)
	summary, err := rc.BackupStdin(ctx, reader, escrowTarPath(ns, e.cfg.ClusterName, pvc), tags, now)
	execErr := wait()
	if err != nil {
		return EscrowRef{}, fmt.Errorf("restic backup of pvc %s failed: %w", pvc, err)
	}
	if execErr != nil {
		return EscrowRef{}, fmt.Errorf("tar of pvc %s failed: %w", pvc, execErr)
	}

	output.Success("Escrowed pvc %s → restic snapshot %s (%s)", pvc, summary.SnapshotID,
		output.FormatBytes(summary.TotalSize))

	return EscrowRef{
		Provider:  e.Name(),
		ID:        summary.SnapshotID,
		PVC:       pvc,
		Cluster:   e.cfg.ClusterName,
		Instance:  pvc, // CNPG pgdata PVC is named after its instance
		RunID:     e.runID,
		CreatedAt: now,
	}, nil
}

func (e *resticPVCEscrow) Verify(ctx context.Context, refs []EscrowRef) error {
	rc := e.newResticClient()
	ns := e.cfg.Namespace
	for _, ref := range refs {
		path := escrowTarPath(ns, ref.Cluster, ref.PVC)
		if err := proveRestorable(ctx, rc, ref.ID, path); err != nil {
			return fmt.Errorf("escrow %s (pvc %s) restorability proof failed — refuse to proceed: %w",
				ref.ID, ref.PVC, err)
		}
		common.InfoLog("Proved restorability of escrow %s (pvc %s)", ref.ID, ref.PVC)
	}
	return nil
}

// proveRestorable restores the actual archive bytes via `restic dump` and parses
// them as a tar. Reading at least one entry WITH content proves the bytes come
// back AND are not corrupt — the only thing that may gate a destructive clear.
func proveRestorable(ctx context.Context, rc *restic.Client, snapshotID, path string) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := rc.Dump(ctx, snapshotID, path, pw, nil)
		// CloseWithError(nil) closes the pipe cleanly (reader sees EOF).
		_ = pw.CloseWithError(err)
		errc <- err
	}()

	tr := tar.NewReader(pr)
	var entries int
	var bytesRead int64
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = pr.CloseWithError(err) // unblock the dump goroutine
			// Prefer the real cause: a dump failure surfaces here as a read error.
			if dumpErr := <-errc; dumpErr != nil {
				return fmt.Errorf("restic dump failed (bytes not recoverable): %w", dumpErr)
			}
			return fmt.Errorf("restored archive is not a readable tar (corrupt): %w", err)
		}
		entries++
		n, _ := io.Copy(io.Discard, tr) // pull file bytes through the restore
		bytesRead += n
	}
	if dumpErr := <-errc; dumpErr != nil {
		return fmt.Errorf("restic dump failed (bytes not recoverable): %w", dumpErr)
	}
	if entries == 0 || bytesRead == 0 {
		return fmt.Errorf("restored archive was empty (%d entries, %d bytes) — cannot prove datadir recovery",
			entries, bytesRead)
	}
	return nil
}

func (e *resticPVCEscrow) Cleanup(ctx context.Context, refs []EscrowRef) error {
	rc := e.newResticClient()
	// Best-effort: forget exactly the escrow snapshots we created. Releasing the
	// escrow is the whole point of cleanup, so we drop the specific entries by ID
	// rather than apply a keep-N retention.
	for _, ref := range refs {
		if err := rc.ForgetSnapshot(ctx, ref.ID); err != nil {
			common.WarnLog("Failed to forget escrow snapshot %s: %v", ref.ID, err)
		}
	}
	return nil
}

// EstimateCaptureBytes sums each PVC's used bytes: a PVC-files tarball is roughly
// the used size (restic dedup/compression only reduces it), so the sum is a safe
// upper-ish estimate for the pre-capture space guard.
func (e *resticPVCEscrow) EstimateCaptureBytes(recoverySet []string, usedBytes map[string]int64) int64 {
	var total int64
	for _, pvc := range recoverySet {
		total += usedBytes[pvc]
	}
	return total
}

// AvailableBytes statfs the restic repository path for free space.
func (e *resticPVCEscrow) AvailableBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(e.cfg.BackupsPath, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", e.cfg.BackupsPath, err)
	}
	return int64(st.Bavail) * st.Bsize, nil
}

// waitRunning polls until the helper pod is Running (exec needs a live pod),
// failing fast if it terminates first.
func (e *resticPVCEscrow) waitRunning(ctx context.Context, name string) error {
	c := k8s.GetClients()
	ns := e.cfg.Namespace
	for i := 0; i < 60; i++ {
		p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			switch p.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("helper pod %s reached phase %s before exec", name, p.Status.Phase)
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("helper pod %s did not reach Running within 120s", name)
}

// serviceAccount inherits a workload SA so the helper satisfies the namespace's
// PodSecurity/RBAC; falls back to "default".
func (e *resticPVCEscrow) serviceAccount(ctx context.Context, ns string) string {
	c := k8s.GetClients()
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "default"
	}
	return k8s.ServiceAccountFromPods(pods.Items)
}

func (e *resticPVCEscrow) tags(pvc string) map[string]string {
	return map[string]string{
		"engine":    e.cfg.Engine,
		"cluster":   e.cfg.ClusterName,
		"namespace": e.cfg.Namespace,
		"instance":  pvc,
		"type":      "escrow",
		"run-id":    e.runID,
	}
}

// escrowTarPath is the deterministic --stdin-filename for a PVC's tarball, so
// Verify can re-derive the dump path from a ref alone.
func escrowTarPath(ns, cluster, pvc string) string {
	return fmt.Sprintf("%s/%s/escrow-%s.tar", ns, cluster, pvc)
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }
