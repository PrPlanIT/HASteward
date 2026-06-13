package repair

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/output"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clearDatadirOffline clears a disposable instance's pgdata IN PLACE while it is
// down. It is the offline half of healInstance — clear pgdata on the PVC — with
// pg_basebackup and the TLS cert mounts removed, because the deadlock breaker runs
// BEFORE any primary exists. The PVC is preserved; only its contents are cleared,
// which frees the disk that froze the cluster. Once unfenced and the primary boots,
// CNPG re-clones the now-empty replica (or the normal repair heal does).
//
// It builds a clear-only helper pod and hands the dangerous PVC handoff to the shared
// runOfflinePVCJob primitive (fence → disable reconcile → single delete → acquire PVC
// → re-enable reconcile → unfence) — the SAME acquisition model and the same
// fenced-on-failure / always-re-enable-reconcile safety contract as healInstance.
//
// CALLER CONTRACT: the breaker MUST gate every call behind
// RecoveryProof.AuthorizesClear(targetPod). clearDatadirOffline does not
// re-derive disposability or authority; it clears exactly the pod/PVC it is given.
func (r *cnpgRepair) clearDatadirOffline(ctx context.Context, targetPod, targetPVC string, hcfg *healConfig) error {
	cfg := r.p.Config()
	ns := cfg.Namespace

	parts := strings.Split(targetPod, "-")
	instanceSuffix := parts[len(parts)-1]
	clearPodName := fmt.Sprintf("%s-unwedge-%s-%d", cfg.ClusterName, instanceSuffix, time.Now().Unix())

	output.Section("Clearing datadir offline: " + targetPod)
	output.Bullet(0, "1. Fence instance (CNPG stops managing it)")
	output.Bullet(0, "2. Disable reconcile loop so the operator yields the PVC")
	output.Bullet(0, "3. Clear pgdata on PVC %s (PVC preserved; no basebackup)", targetPVC)
	output.Bullet(0, "4. Remove fence + re-enable reconcile (CNPG re-clones once a primary is up)")

	uid, _ := strconv.ParseInt(hcfg.postgresUID, 10, 64)
	gid, _ := strconv.ParseInt(hcfg.postgresGID, 10, 64)

	clearScript := `set -e
echo "=== Clearing pgdata ==="
if [ -f /var/lib/postgresql/data/pgdata/PG_VERSION ]; then
  echo "WARNING: Found existing PG_VERSION file. Proceeding with clear..."
fi
rm -rf /var/lib/postgresql/data/pgdata/*
rm -rf /var/lib/postgresql/data/pgdata/.[!.]*
rm -rf /var/lib/postgresql/data/lost+found 2>/dev/null || true
echo "pgdata cleared."`

	clearPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clearPodName,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "unwedge-clear"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: hcfg.serviceAccount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			Containers: []corev1.Container{{
				Name:    "clearer",
				Image:   hcfg.imageName,
				Command: []string{"sh", "-c", clearScript},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pgdata", MountPath: "/var/lib/postgresql/data"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "pgdata",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: targetPVC,
					},
				},
			}},
		},
	}

	if err := r.runOfflinePVCJob(ctx, offlinePVCJob{
		targetPod:     targetPod,
		targetPVC:     targetPVC,
		helperPod:     clearPod,
		helperPodName: clearPodName,
		label:         "clear",
		completeSec:   300,
	}); err != nil {
		return err
	}

	output.Success("Cleared pgdata for %s (PVC %s preserved, now empty)", targetPod, targetPVC)
	return nil
}
