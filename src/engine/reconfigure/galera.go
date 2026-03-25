package reconfigure

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/engine/triage"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	Register("galera", func(p provider.EngineProvider) (Reconfigurer, error) {
		gp, ok := p.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("galera reconfigure: expected *provider.GaleraProvider, got %T", p)
		}
		t, err := triage.Get(p)
		if err != nil {
			return nil, fmt.Errorf("galera reconfigure: triage init: %w", err)
		}
		return &galeraReconfigure{p: gp, triager: t}, nil
	})
}

type galeraReconfigure struct {
	p       *provider.GaleraProvider
	triager triage.Triager
}

func (g *galeraReconfigure) Name() string { return "galera" }

func (g *galeraReconfigure) Assess(ctx context.Context) (*model.TriageResult, error) {
	output.Section("Phase 1: Triage")
	return triage.Run(ctx, g.triager, engine.NopSink{})
}

// Validate checks all preconditions after triage.
func (g *galeraReconfigure) Validate(ctx context.Context, result *model.TriageResult) error {
	cfg := g.p.Config()
	c := k8s.GetClients()
	ns := cfg.Namespace

	// Ordinal bounds (upper — lower bound checked at CLI level)
	replicas := int(g.p.Replicas())
	if *cfg.InstanceNumber >= replicas {
		return fmt.Errorf("ABORT: instance %d out of bounds (cluster has instances 0–%d)", *cfg.InstanceNumber, replicas-1)
	}

	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, *cfg.InstanceNumber)

	// PVC validation (action-specific, hard fail)
	if cfg.FixBootstrap {
		storagePVC := fmt.Sprintf("storage-%s", targetPod)
		galeraPVC := fmt.Sprintf("galera-%s", targetPod)

		_, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, storagePVC, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("ABORT: --fix-bootstrap requires storage PVC %s but it was not found: %w", storagePVC, err)
		}
		_, err = c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, galeraPVC, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("ABORT: --fix-bootstrap requires galera PVC %s but it was not found: %w", galeraPVC, err)
		}
		common.InfoLog("Target PVCs validated: %s (storage), %s (galera)", storagePVC, galeraPVC)

		// Donor viability: at least one other node must be Galera-suitable for SST
		// Without this, reconfigure destroys local data with no recovery path
		donorAvailable := false
		targetPodName := targetPod
		for _, a := range result.Assessments {
			if a.Pod != targetPodName && a.WsrepReady == "ON" && a.WsrepConnected == "ON" && a.WsrepStateComment == "Synced" {
				donorAvailable = true
				common.InfoLog("SST donor available: %s (Synced)", a.Pod)
				break
			}
		}
		if !donorAvailable {
			return fmt.Errorf("ABORT: no valid donor available for SST — reconfigure would destroy data on %s without recovery path", targetPod)
		}
	}

	return nil
}

// PrintPlan displays the reconfigure plan with blast radius and warning.
func (g *galeraReconfigure) PrintPlan(ctx context.Context, result *model.TriageResult) {
	cfg := g.p.Config()
	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, *cfg.InstanceNumber)
	replicas := g.p.Replicas()

	output.Println("")
	output.Println("WARNING: This is a cluster-scoped operation.")
	output.Println("All database nodes will be stopped.")
	output.Println("Service disruption will occur.")
	output.Println("")

	output.Banner("Reconfigure Plan")
	output.Field("Operation", "reconfigure")
	output.Field("Scope", "cluster")
	output.Field("Target", fmt.Sprintf("%s (ordinal %d)", targetPod, *cfg.InstanceNumber))

	if cfg.FixBootstrap {
		output.Field("Action", "fix-bootstrap")
		output.Field("Blast radius", fmt.Sprintf("all %d Galera pods stopped, then restarted", replicas))
		output.Println("Data mutation:")
		output.Bullet(1, "storage PVC (storage-%s): grastate.dat cleared, gvwstate.dat + galera.cache + ib_logfile* removed", targetPod)
		output.Bullet(1, "galera PVC (galera-%s): 1-bootstrap.cnf will be removed", targetPod)
		output.Println("Data impact:")
		output.Bullet(1, "Local data on target instance will be discarded")
		output.Bullet(1, "Node will rejoin cluster via full SST from a healthy donor")
	}

	output.Field("Expected", "cluster restarts without stale bootstrap intent on target; all replicas converge to Synced")
	output.Println("")

	// Current state summary
	output.Println("Current state:")
	output.Bullet(1, "Replicas: %d", replicas)
	if len(result.DataComparison.PrimaryMembers) > 0 {
		output.Bullet(1, "Primary: %s (seqno: %d)",
			strings.Join(result.DataComparison.PrimaryMembers, ", "),
			result.DataComparison.BestPrimarySeqno)
	} else {
		output.Bullet(1, "Primary: NONE")
	}

	// Target assessment
	for _, a := range result.Assessments {
		if a.Pod == targetPod {
			notes := "unknown"
			if len(a.Notes) > 0 {
				notes = a.Notes[0]
			}
			output.Bullet(1, "Target: %s (%s)", targetPod, notes)
			break
		}
	}
	output.Println("")
}

// Execute performs the cluster-scoped reconfigure operation.
func (g *galeraReconfigure) Execute(ctx context.Context, result *model.TriageResult) error {
	cfg := g.p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()
	instanceNum := *cfg.InstanceNumber
	targetPod := fmt.Sprintf("%s-%d", cfg.ClusterName, instanceNum)
	originalReplicas := int32(g.p.Replicas())

	// Capture SA
	sa := "default"
	if targetPodObj, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, targetPod, metav1.GetOptions{}); err == nil {
		if targetPodObj.Spec.ServiceAccountName != "" {
			sa = targetPodObj.Spec.ServiceAccountName
		}
	}

	storagePVC := fmt.Sprintf("storage-%s", targetPod)
	galeraPVC := fmt.Sprintf("galera-%s", targetPod)
	storageHelper := fmt.Sprintf("%s-reconf-storage-%d-%d", cfg.ClusterName, instanceNum, time.Now().Unix())
	galeraHelper := fmt.Sprintf("%s-reconf-galera-%d-%d", cfg.ClusterName, instanceNum, time.Now().Unix())

	suspended := false
	scaledDown := false

	// Deterministic rescue — every step logged
	rescue := func() {
		output.Section("Rescue")

		// 1. Delete helpers
		delErr1 := c.Clientset.CoreV1().Pods(ns).Delete(ctx, storageHelper, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
		if delErr1 != nil && !apierrors.IsNotFound(delErr1) {
			common.WarnLog("Rescue: failed to delete storage helper: %v", delErr1)
		} else {
			common.InfoLog("Rescue: storage helper deleted or absent")
		}
		delErr2 := c.Clientset.CoreV1().Pods(ns).Delete(ctx, galeraHelper, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
		if delErr2 != nil && !apierrors.IsNotFound(delErr2) {
			common.WarnLog("Rescue: failed to delete galera helper: %v", delErr2)
		} else {
			common.InfoLog("Rescue: galera helper deleted or absent")
		}

		// 2. Wait for helpers gone
		if err := waitForPodGone(ctx, ns, storageHelper); err != nil {
			common.WarnLog("Rescue: storage helper still present: %v", err)
		}
		if err := waitForPodGone(ctx, ns, galeraHelper); err != nil {
			common.WarnLog("Rescue: galera helper still present: %v", err)
		}

		// 3. Scale restore
		if scaledDown {
			if err := g.scaleStatefulSet(ctx, originalReplicas); err != nil {
				common.WarnLog("Rescue: failed to restore scale: %v", err)
			} else {
				common.InfoLog("Rescue: scale restored to %d", originalReplicas)
			}
		}

		// 4. Resume CR
		if suspended {
			if err := g.resumeCR(ctx); err != nil {
				common.WarnLog("Rescue: failed to resume CR: %v", err)
			} else {
				common.InfoLog("Rescue: CR resumed")
			}
		}

		// 5. Print final system state
		output.Section("Rescue Final State")
		sts, stsErr := c.Clientset.AppsV1().StatefulSets(ns).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
		if stsErr == nil {
			output.Field("Replicas (spec)", fmt.Sprintf("%d", *sts.Spec.Replicas))
			output.Field("Replicas (ready)", fmt.Sprintf("%d", sts.Status.ReadyReplicas))
		}
		output.Field("CR suspended", fmt.Sprintf("%v", suspended && g.isCRSuspended(ctx)))

		for _, name := range []string{storageHelper, galeraHelper} {
			_, hErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if hErr != nil && apierrors.IsNotFound(hErr) {
				output.Field(name, "gone")
			} else {
				output.Field(name, "STILL PRESENT")
			}
		}
	}

	// STEP 1: Suspend CR
	output.Section("Executing Reconfigure")
	common.InfoLog("STEP 1: Suspending CR")
	if err := g.suspendCR(ctx); err != nil {
		return fmt.Errorf("failed to suspend CR: %w", err)
	}
	suspended = true
	time.Sleep(3 * time.Second)

	// STEP 2: Scale to 0
	common.InfoLog("STEP 2: Scaling StatefulSet to 0 (all pods)")
	if err := g.scaleStatefulSet(ctx, 0); err != nil {
		rescue()
		return fmt.Errorf("failed to scale to 0: %w", err)
	}
	scaledDown = true

	// STEP 3: Wait for ALL pods gone (strict NotFound)
	common.InfoLog("STEP 3: Waiting for all pods to terminate")
	for i := 0; i < int(originalReplicas); i++ {
		podName := fmt.Sprintf("%s-%d", cfg.ClusterName, i)
		if err := waitForPodGone(ctx, ns, podName); err != nil {
			rescue()
			return fmt.Errorf("ABORT: %s did not terminate: %w", podName, err)
		}
		common.InfoLog("Pod %s terminated (NotFound)", podName)
	}

	// STEP 4: Mount target PVCs and apply mutations
	if cfg.FixBootstrap {
		// Storage PVC: neutralize ALL Galera + InnoDB recovery state
		common.InfoLog("STEP 4a: Neutralizing Galera state on %s", storagePVC)
		storageScript := `set -e
echo "=== Neutralizing Galera state ==="

# 1. Reset grastate to null UUID, seqno -1, no bootstrap
cat > /var/lib/mysql/grastate.dat <<GSEOF
# GALERA saved state
version: 2.1
uuid:    00000000-0000-0000-0000-000000000000
seqno:   -1
safe_to_bootstrap: 0
GSEOF
echo "grastate.dat reset:"
cat /var/lib/mysql/grastate.dat

# 2. Remove Galera recovery artifacts
rm -f /var/lib/mysql/gvwstate.dat
rm -f /var/lib/mysql/galera.cache
echo "Removed gvwstate.dat + galera.cache"

# 3. Remove InnoDB redo logs ONLY (prevent --wsrep-recover from extracting seqno)
# DO NOT remove ibdata1 — required for engine startup and SST
rm -f /var/lib/mysql/ib_logfile*
echo "Removed ib_logfile*"

# 4. Verify state neutralization
grep -q "seqno:   -1" /var/lib/mysql/grastate.dat || { echo "FAIL: grastate seqno not reset"; exit 1; }
test ! -f /var/lib/mysql/gvwstate.dat || { echo "FAIL: gvwstate.dat still exists"; exit 1; }
test ! -f /var/lib/mysql/galera.cache || { echo "FAIL: galera.cache still exists"; exit 1; }
echo "=== State neutralized — operator cannot infer higher seqno ==="
`
		if err := g.runHelperWithRetry(ctx, storageHelper, ns, storagePVC, "/var/lib/mysql", storageScript, sa); err != nil {
			rescue()
			return err
		}

		// Galera PVC: remove bootstrap config
		common.InfoLog("STEP 4b: Removing 1-bootstrap.cnf from %s", galeraPVC)
		galeraScript := `set -e
echo "=== Current galera config ==="
ls -la /galera/
if [ -f /galera/1-bootstrap.cnf ]; then
  echo "=== Removing 1-bootstrap.cnf ==="
  rm -f /galera/1-bootstrap.cnf
  echo "Bootstrap config removed"
else
  echo "No 1-bootstrap.cnf found (OK)"
fi
echo "=== Final config ==="
ls -la /galera/
echo "=== Done ==="
`
		if err := g.runHelperWithRetry(ctx, galeraHelper, ns, galeraPVC, "/galera", galeraScript, sa); err != nil {
			rescue()
			return err
		}
	}

	// STEP 5: Confirm helpers gone (strict NotFound, fatal)
	common.InfoLog("STEP 5: Confirming helper pods are gone")
	if err := waitForPodGone(ctx, ns, storageHelper); err != nil {
		rescue()
		return fmt.Errorf("ABORT: storage helper still present — cannot safely scale up: %w", err)
	}
	if err := waitForPodGone(ctx, ns, galeraHelper); err != nil {
		rescue()
		return fmt.Errorf("ABORT: galera helper still present — cannot safely scale up: %w", err)
	}

	// STEP 6: Scale back up
	common.InfoLog("STEP 6: Scaling back up to %d", originalReplicas)
	if err := g.scaleStatefulSet(ctx, originalReplicas); err != nil {
		rescue()
		return fmt.Errorf("failed to scale back up: %w", err)
	}
	scaledDown = false

	// STEP 7: Resume CR
	common.InfoLog("STEP 7: Resuming CR")
	if err := g.resumeCR(ctx); err != nil {
		rescue()
		return fmt.Errorf("failed to resume CR: %w", err)
	}
	suspended = false

	// STEP 8: Wait for pods Running
	common.InfoLog("STEP 8: Waiting for pods to come back online")
	healTimeout := cfg.HealTimeout
	if healTimeout <= 0 {
		healTimeout = 600
	}
	allReady := false
	for i := 0; i < healTimeout/10; i++ {
		time.Sleep(10 * time.Second)
		ready := 0
		for j := 0; j < int(originalReplicas); j++ {
			podName := fmt.Sprintf("%s-%d", cfg.ClusterName, j)
			pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
			if err == nil && pod.Status.Phase == corev1.PodRunning &&
				len(pod.Status.ContainerStatuses) > 0 && pod.Status.ContainerStatuses[0].Ready {
				ready++
			}
		}
		if ready == int(originalReplicas) {
			allReady = true
			common.InfoLog("All %d pods are Running and Ready", originalReplicas)
			break
		}
	}
	if !allReady {
		common.WarnLog("Not all pods became ready within timeout")
	}

	// STEP 9: Verify Galera cluster formation
	output.Section("Post-Reconfigure Verification")
	if err := g.verifyCluster(ctx, targetPod, int(originalReplicas)); err != nil {
		return err
	}

	return nil
}

// verifyCluster polls for full cluster convergence after reconfigure.
// Returns error if cluster does not converge within timeout.
// Requires: target Synced + at least one other Synced + cluster_size == replicas.
func (g *galeraReconfigure) verifyCluster(ctx context.Context, targetPod string, expectedReplicas int) error {
	cfg := g.p.Config()
	ns := cfg.Namespace

	// Poll for up to 120s — SST can take time
	common.InfoLog("Polling for cluster convergence (up to 120s)...")
	for poll := 0; poll < 24; poll++ {
		targetSynced := false
		otherSynced := false
		var clusterSize int

		for i := 0; i < expectedReplicas; i++ {
			podName := fmt.Sprintf("%s-%d", cfg.ClusterName, i)
			probe := g.probeWsrep(ctx, podName, ns)
			if !probe.ExecOK {
				continue
			}
			synced := probe.WsrepReady != nil && *probe.WsrepReady &&
				probe.WsrepConnected != nil && *probe.WsrepConnected &&
				probe.StateComment == "Synced"

			if podName == targetPod && synced {
				targetSynced = true
			} else if podName != targetPod && synced {
				otherSynced = true
			}
			if probe.ClusterSize > clusterSize {
				clusterSize = probe.ClusterSize
			}
		}

		if targetSynced && otherSynced && clusterSize == expectedReplicas {
			// Log individual node status
			for i := 0; i < expectedReplicas; i++ {
				podName := fmt.Sprintf("%s-%d", cfg.ClusterName, i)
				probe := g.probeWsrep(ctx, podName, ns)
				if probe.ExecOK {
					output.Success("Node %s: Synced, cluster_size=%d", podName, probe.ClusterSize)
				}
			}
			output.Section("Verdict")
			output.Success("Reconfigure successful: cluster reformed with %d nodes, all Synced", clusterSize)
			return nil
		}

		if poll < 23 {
			time.Sleep(5 * time.Second)
		}
	}

	// Timeout — print final state for diagnostics
	output.Section("Verdict — FAILED")
	for i := 0; i < expectedReplicas; i++ {
		podName := fmt.Sprintf("%s-%d", cfg.ClusterName, i)
		probe := g.probeWsrep(ctx, podName, ns)
		if probe.ExecOK {
			common.WarnLog("%s: ready=%v connected=%v state=%s cluster_size=%d",
				podName, probe.WsrepReady, probe.WsrepConnected, probe.StateComment, probe.ClusterSize)
		} else {
			common.WarnLog("%s: wsrep probe failed", podName)
		}
	}
	return fmt.Errorf("FAIL: cluster did not converge within 120s — target may still be syncing or bootstrap intent was not fully neutralized")
}

// probeWsrep queries wsrep state on a pod (local to reconfigure, not shared with repair).
func (g *galeraReconfigure) probeWsrep(ctx context.Context, podName, ns string) struct {
	ExecOK         bool
	WsrepReady     *bool
	WsrepConnected *bool
	StateComment   string
	ClusterSize    int
} {
	type probeResult struct {
		ExecOK         bool
		WsrepReady     *bool
		WsrepConnected *bool
		StateComment   string
		ClusterSize    int
	}
	result := probeResult{}

	execResult, err := k8s.ExecCommandWithEnv(ctx, podName, ns, "mariadb",
		map[string]string{"MYSQL_PWD": g.p.RootPassword()},
		[]string{"mariadb", "-u", "root", "--batch", "--skip-column-names", "-e",
			"SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS " +
				"WHERE VARIABLE_NAME IN (" +
				"'wsrep_local_state_comment', " +
				"'wsrep_connected', 'wsrep_ready', " +
				"'wsrep_cluster_size'" +
				") ORDER BY VARIABLE_NAME"})
	if err != nil {
		return result
	}
	result.ExecOK = true

	for _, line := range strings.Split(execResult.Stdout, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "wsrep_ready":
			b := val == "ON"
			result.WsrepReady = &b
		case "wsrep_connected":
			b := val == "ON"
			result.WsrepConnected = &b
		case "wsrep_local_state_comment":
			result.StateComment = val
		case "wsrep_cluster_size":
			fmt.Sscanf(val, "%d", &result.ClusterSize)
		}
	}
	return result
}

// --- Shared primitives (local implementations, no import from repair) ---

func (g *galeraReconfigure) suspendCR(ctx context.Context) error {
	cfg := g.p.Config()
	c := k8s.GetClients()
	patch := `{"spec":{"suspend":true}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func (g *galeraReconfigure) resumeCR(ctx context.Context) error {
	cfg := g.p.Config()
	c := k8s.GetClients()
	patch := `{"spec":{"suspend":false}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func (g *galeraReconfigure) isCRSuspended(ctx context.Context) bool {
	cfg := g.p.Config()
	c := k8s.GetClients()
	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return k8s.GetNestedBool(obj, "spec", "suspend")
}

func (g *galeraReconfigure) scaleStatefulSet(ctx context.Context, replicas int32) error {
	cfg := g.p.Config()
	c := k8s.GetClients()
	scale, err := c.Clientset.AppsV1().StatefulSets(cfg.Namespace).GetScale(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	scale.Spec.Replicas = replicas
	_, err = c.Clientset.AppsV1().StatefulSets(cfg.Namespace).UpdateScale(
		ctx, cfg.ClusterName, scale, metav1.UpdateOptions{})
	return err
}

// waitForPodGone blocks until pod returns NotFound. Strict — only NotFound counts.
func waitForPodGone(ctx context.Context, ns, podName string) error {
	c := k8s.GetClients()
	for i := 0; i < 60; i++ {
		_, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			common.DebugLog("waitForPodGone(%s): transient error: %v", podName, err)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("pod %s did not terminate within 300s", podName)
}

// runHelperWithRetry runs a helper pod with mount retry for CSI detach lag.
func (g *galeraReconfigure) runHelperWithRetry(ctx context.Context, name, ns, pvc, mountPath, script, sa string) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := g.runHelperPod(ctx, name, ns, pvc, mountPath, script, sa)
		if err == nil {
			return nil
		}
		lastErr = err
		errMsg := err.Error()
		isMountError := strings.Contains(errMsg, "Multi-Attach") ||
			strings.Contains(errMsg, "already attached") ||
			strings.Contains(errMsg, "device busy") ||
			strings.Contains(errMsg, "FailedAttachVolume") ||
			strings.Contains(errMsg, "FailedMount") ||
			strings.Contains(errMsg, "timed out")
		if !isMountError {
			return fmt.Errorf("helper pod %s failed (non-retryable): %w", name, err)
		}
		if attempt < 3 {
			common.WarnLog("Helper pod %s mount failed (attempt %d/3): %v", name, attempt, err)
			time.Sleep(time.Duration(attempt*10) * time.Second)
		}
	}
	return fmt.Errorf("helper pod %s failed after 3 mount retries: %w", name, lastErr)
}

// runHelperPod creates a busybox pod that mounts a PVC and runs a script.
func (g *galeraReconfigure) runHelperPod(ctx context.Context, name, ns, pvc, mountPath, script, sa string) error {
	c := k8s.GetClients()
	rootUser := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "reconfigure-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sa,
			SecurityContext:    &corev1.PodSecurityContext{RunAsUser: &rootUser},
			Containers: []corev1.Container{{
				Name:    "reconfigure",
				Image:   "docker.io/library/busybox:latest",
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: mountPath},
				},
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

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create helper pod %s: %w", name, err)
	}

	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		p, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded {
			g.logHelperOutput(ctx, ns, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
			time.Sleep(2 * time.Second)
			return nil
		}
		if p.Status.Phase == corev1.PodFailed {
			g.logHelperOutput(ctx, ns, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
			return fmt.Errorf("helper pod %s failed", name)
		}
	}
	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	return fmt.Errorf("helper pod %s timed out", name)
}

func (g *galeraReconfigure) logHelperOutput(ctx context.Context, ns, podName string) {
	c := k8s.GetClients()
	req := c.Clientset.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return
	}
	defer stream.Close()
	var buf bytes.Buffer
	io.Copy(&buf, stream)
	if buf.Len() > 0 {
		common.DebugLog("Helper pod output:\n%s", buf.String())
	}
}

func ptr[T any](v T) *T { return &v }
