package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Shared Galera cluster operations used by both triage (fenced deep-recovery) and
// bootstrap (full recovery). They live on the provider — the single k8s-access
// layer every engine holds — so no engine has to duplicate them and there is no
// import cycle (bootstrap imports triage; both import provider).

const (
	// ZeroUUID is the all-zero cluster UUID a node reports when it never joined.
	ZeroUUID = "00000000-0000-0000-0000-000000000000"
	// MaxPhantomSeqno bounds a plausible seqno; anything larger is corrupt gcache.
	MaxPhantomSeqno = int64(1e12)
	// mariadbDataDirUID owns /var/lib/mysql in the mariadb image. The wsrep-recover
	// helper MUST run as this uid — mariadbd aborts if started as root.
	mariadbDataDirUID = int64(999)
	// galeraProviderSO is the Galera wsrep provider in the mariadb image. Without it
	// (and wsrep_on=ON) mariadbd logs "WSREP: disabled, skipping position recovery"
	// and returns no seqno.
	galeraProviderSO = "/usr/lib/galera/libgalera_smm.so"
)

var (
	reRecoveredPos = regexp.MustCompile(`Recovered position:\s*([0-9a-fA-F-]+):([0-9-]+)`)
	reLastCommit   = regexp.MustCompile(`Last committed:\s*([0-9]+)`)
)

// WsrepRecoverResult holds the parsed output from mariadbd --wsrep-recover — the
// authoritative last-committed position read straight from InnoDB.
type WsrepRecoverResult struct {
	UUID          string
	Seqno         int64
	LastCommitted int64
	Valid         bool
}

// SuspendCR patches the MariaDB CR to spec.suspend=true, stopping operator
// reconciliation so it cannot recreate pods during a fenced operation.
func (p *GaleraProvider) SuspendCR(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := p.Config()
	patch := `{"spec":{"suspend":true}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ResumeCR patches the MariaDB CR to spec.suspend=false.
func (p *GaleraProvider) ResumeCR(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := p.Config()
	patch := `{"spec":{"suspend":false}}`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ForceBootstrapLive atomically resumes the operator AND forces it to bootstrap the given
// pod — an ONLINE bootstrap for a data-healthy but recovery-deadlocked cluster (no
// scale-to-0). The operator reforms the cluster from that node, then clears its own
// recovery status and unsets forceClusterBootstrapInPod.
func (p *GaleraProvider) ForceBootstrapLive(ctx context.Context, pod string) error {
	c := k8s.GetClients()
	cfg := p.Config()
	patch := fmt.Sprintf(`{"spec":{"suspend":false,"galera":{"recovery":{"forceClusterBootstrapInPod":%q}}}}`, pod)
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ClearForceBootstrap removes spec.galera.recovery.forceClusterBootstrapInPod — a
// defensive unset in case the operator did not clear it itself (leaving it set would
// re-force a bootstrap on every restart). A no-op if the field is already absent.
func (p *GaleraProvider) ClearForceBootstrap(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := p.Config()
	patch := `[{"op":"remove","path":"/spec/galera/recovery/forceClusterBootstrapInPod"}]`
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.JSONPatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil && apierrors.IsInvalid(err) {
		return nil // field already absent — nothing to remove
	}
	return err
}

// FenceLockAnnotation serializes the disruptive fence->recover operations
// (bootstrap, triage deep-recover) so two cannot fight over suspend/scale/recover
// on the same cluster. Value format: "<holder>@<RFC3339 timestamp>".
const FenceLockAnnotation = "hasteward.prplanit.com/bootstrap-lock"

// AcquireFenceLock atomically takes the cluster fence lock for holder, using
// optimistic concurrency (the CR's resourceVersion) so two near-simultaneous
// acquirers cannot both win. Returns (false, nil) — not acquired — if the CR is
// already suspended (another HASteward op is fencing; they all suspend first), the
// lock is held fresh (<1h), or another writer won the compare-and-swap; (true,
// nil) if acquired (the caller MUST release via ClearFenceLock). This is the
// atomic version of the suspend-check + lock-set, closing the Get->write race.
// Setting the same annotation bootstrap's stale-lock check reads gives mutual
// exclusion across both fence operations.
func (p *GaleraProvider) AcquireFenceLock(ctx context.Context, holder string) (bool, error) {
	c := k8s.GetClients()
	cfg := p.Config()
	obj, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Get(ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if k8s.GetNestedBool(obj, "spec", "suspend") {
		return false, nil // another HASteward op is already fencing
	}
	if lock := obj.GetAnnotations()[FenceLockAnnotation]; lock != "" {
		if parts := strings.SplitN(lock, "@", 2); len(parts) == 2 {
			if ts, terr := time.Parse(time.RFC3339, parts[1]); terr == nil && time.Since(ts) < time.Hour {
				return false, nil // held by a fresh lock
			}
		}
	}
	// Compare-and-swap: Update carries the Get's resourceVersion, so a concurrent
	// writer (another acquirer, or the operator) yields a 409 Conflict => we lost.
	anns := obj.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	anns[FenceLockAnnotation] = holder + "@" + time.Now().UTC().Format(time.RFC3339)
	obj.SetAnnotations(anns)
	if _, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ClearFenceLock removes the fence-lock annotation.
func (p *GaleraProvider) ClearFenceLock(ctx context.Context) error {
	c := k8s.GetClients()
	cfg := p.Config()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, FenceLockAnnotation)
	_, err := c.Dynamic.Resource(k8s.MariaDBGVR).Namespace(cfg.Namespace).Patch(
		ctx, cfg.ClusterName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ScaleStatefulSet scales the cluster StatefulSet to the desired replica count.
func (p *GaleraProvider) ScaleStatefulSet(ctx context.Context, replicas int32) error {
	c := k8s.GetClients()
	cfg := p.Config()
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

// WaitPodsTerminated polls until NO cluster pods remain, or returns an error if
// any survive past the timeout. This is the DATA-SAFETY gate before wsrep_recover:
// a helper mariadbd must never open a datadir a real mysqld still holds (two
// processes in one InnoDB dir = corruption). Callers MUST abort the recover if
// this returns an error — do not proceed on a "close enough" timeout.
func (p *GaleraProvider) WaitPodsTerminated(ctx context.Context, timeoutSec int) error {
	c := k8s.GetClients()
	cfg := p.Config()
	if timeoutSec <= 0 {
		timeoutSec = common.DefaultDeleteTimeout
	}
	sel := p.PodSelector()
	for i := 0; i < timeoutSec/5; i++ {
		pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err == nil && len(pods.Items) == 0 {
			common.InfoLog("All %s pods terminated — datadir is unowned", cfg.ClusterName)
			return nil
		}
		common.Sleep(5 * time.Second)
	}
	pods, _ := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	return fmt.Errorf("pods did not terminate within %ds (%d remain) — refusing wsrep_recover to avoid a concurrent-open corruption", timeoutSec, len(pods.Items))
}

// DeleteRecoveryPods force-deletes any operator recovery pods that could compete
// with a fenced recover for the datadir.
func (p *GaleraProvider) DeleteRecoveryPods(ctx context.Context) {
	c := k8s.GetClients()
	cfg := p.Config()
	pods, err := c.Clientset.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: p.PodSelector() + ",k8s.mariadb.com/recovery=true",
	})
	if err != nil {
		return
	}
	for _, pd := range pods.Items {
		_ = c.Clientset.CoreV1().Pods(cfg.Namespace).Delete(ctx, pd.Name, metav1.DeleteOptions{
			GracePeriodSeconds: common.Ptr(int64(0)),
		})
	}
}

// QueryWsrep runs the wsrep GLOBAL_STATUS query on a serving node and returns
// every wsrep_* status variable as a lowercased name->value map. Shared by triage
// (collect), repair (donor probe), and reconfigure so the exec + query + tab-parse
// lives in one place; each caller reads the fields it needs.
func (p *GaleraProvider) QueryWsrep(ctx context.Context, pod string) (map[string]string, error) {
	res, err := k8s.ExecCommandWithEnv(ctx, pod, p.Config().Namespace, "mariadb",
		map[string]string{"MYSQL_PWD": p.RootPassword()},
		[]string{"mariadb", "-u", "root", "--batch", "--skip-column-names", "-e",
			"SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS " +
				"WHERE VARIABLE_NAME LIKE 'wsrep_%' ORDER BY VARIABLE_NAME"})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
	}
	return out, nil
}

// RunWsrepRecover runs `mariadbd --wsrep-recover` against a node's PVC via a
// short-lived helper pod and returns the authoritative recovered position. The
// caller MUST have fenced the cluster first (see WaitPodsTerminated).
func (p *GaleraProvider) RunWsrepRecover(ctx context.Context, podName, sa string) (WsrepRecoverResult, error) {
	cfg := p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	image := p.Image()
	if image == "" {
		return WsrepRecoverResult{}, fmt.Errorf("cannot determine MariaDB image from CR spec")
	}

	pvcName := p.DataPVCName(podName)
	helperName := fmt.Sprintf("%s-wsrep-%s-%d", cfg.ClusterName, podName, time.Now().Unix())

	uid := mariadbDataDirUID
	// Built via the shared helper-pod builder so the Istio-injection exemption is
	// applied — without it a sidecar keeps this pod Running forever and the authority
	// read below hangs to its deadline in every meshed namespace.
	pod := k8s.BuildHelperPod(k8s.HelperPodOpts{
		Name: helperName, Namespace: ns, Image: image, ServiceAccount: sa,
		PVCName: pvcName, MountPath: "/var/lib/mysql",
		RunAsUID: &uid, RunAsGID: &uid, FSGroup: &uid,
		Labels:                map[string]string{"hasteward": "heal-helper"},
		ActiveDeadlineSeconds: 150,
		Command: []string{"sh", "-c", wsrepRecoverCommand(galeraProviderSO)},
	})

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return WsrepRecoverResult{}, fmt.Errorf("failed to create wsrep_recover pod %s: %w", helperName, err)
	}

	var podOutput string
	var lastPod *corev1.Pod
	for i := 0; i < 30; i++ {
		common.Sleep(5 * time.Second)
		pd, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, helperName, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		lastPod = pd
		phase := string(pd.Status.Phase)
		if phase == "Succeeded" || phase == "Failed" {
			podOutput = p.helperPodOutput(ctx, helperName)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, helperName, metav1.DeleteOptions{
				GracePeriodSeconds: common.Ptr(int64(0)),
			})
			common.Sleep(2 * time.Second)
			break
		}
	}

	if podOutput == "" {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, helperName, metav1.DeleteOptions{
			GracePeriodSeconds: common.Ptr(int64(0)),
		})
		// Explain a stuck pod instead of a bare timeout — the node may be at its pod
		// cap, cordoned, or NotReady, which must not be mistaken for a data problem.
		if sched := k8s.DescribePodScheduling(lastPod); sched != "" {
			return WsrepRecoverResult{}, fmt.Errorf("wsrep_recover pod %s did not complete — %s", helperName, sched)
		}
		return WsrepRecoverResult{}, fmt.Errorf("wsrep_recover pod %s produced no output or timed out", helperName)
	}

	common.DebugLog("wsrep_recover output for %s:\n%s", podName, podOutput)
	return ParseWsrepRecoverOutput(podOutput)
}

// helperPodOutput fetches a helper pod's logs.
func (p *GaleraProvider) helperPodOutput(ctx context.Context, podName string) string {
	return k8s.PodLogs(ctx, p.Config().Namespace, podName)
}

// wsrepRecoverCommand builds the shell command that runs mariadbd --wsrep-recover to
// read a node's authoritative committed position.
//
// --binlog-format=ROW is REQUIRED: with --wsrep-on=ON, mariadbd refuses to start (and
// never prints "Recovered position") if binlog_format is anything but ROW — and modern
// MariaDB (11.x) defaults to MIXED. Without it, wsrep_recover aborts on EVERY node with
// "Only binlog_format='ROW' is currently supported", the position parse fails, the
// candidate comes back empty, and a belly-up bootstrap dies with "candidate pod not
// found". Galera only ever runs ROW, so forcing it here is always correct. (Found live on
// a MariaDB 11.8.8 cluster where the operator itself had missed the true most-advanced
// node — seqno 54784 vs the 50522 it reported — for exactly this reason.)
func wsrepRecoverCommand(providerSO string) string {
	return fmt.Sprintf(
		"mariadbd --wsrep-recover --datadir=/var/lib/mysql "+
			"--wsrep-on=ON --wsrep-provider=%s --wsrep-cluster-address=gcomm:// "+
			"--binlog-format=ROW --log-error-verbosity=3 2>&1; exit 0", providerSO)
}

// ParseWsrepRecoverOutput extracts UUID, seqno, and lastCommitted from mariadbd
// --wsrep-recover output.
func ParseWsrepRecoverOutput(output string) (WsrepRecoverResult, error) {
	result := WsrepRecoverResult{}

	posMatch := reRecoveredPos.FindStringSubmatch(output)
	if posMatch == nil {
		return result, fmt.Errorf("could not parse recovered position from wsrep_recover output")
	}

	result.UUID = posMatch[1]
	seqno, err := strconv.ParseInt(posMatch[2], 10, 64)
	if err != nil {
		return result, fmt.Errorf("could not parse seqno %q: %w", posMatch[2], err)
	}
	result.Seqno = seqno

	commitMatch := reLastCommit.FindStringSubmatch(output)
	if commitMatch != nil {
		lc, perr := strconv.ParseInt(commitMatch[1], 10, 64)
		if perr == nil {
			result.LastCommitted = lc
		} else {
			result.LastCommitted = seqno
		}
	} else {
		result.LastCommitted = seqno
	}

	if result.UUID == ZeroUUID {
		return result, fmt.Errorf("recovered zero UUID — node never joined cluster")
	}
	if result.Seqno < 0 || result.Seqno > MaxPhantomSeqno {
		return result, fmt.Errorf("recovered phantom seqno %d — corrupt gcache metadata", result.Seqno)
	}

	result.Valid = true
	return result, nil
}

// RunHelperPod creates a short-lived busybox pod that mounts a node's PVC and runs
// a script against it (e.g. reset grastate, clear safe_to_bootstrap), waits for
// completion, logs its output, and cleans up. Shared by repair and bootstrap.
// HelperPodSpec configures a short-lived busybox pod that mounts one PVC and runs
// a command against it. Shared by heal/reconfigure/bootstrap (run a script,
// read-write) and triage (read-only probe returning the file contents).
type HelperPodSpec struct {
	Name      string
	PVCName   string
	MountPath string
	Command   []string // e.g. ["sh","-c",script] or ["cat", path]
	SA        string
	ReadOnly  bool              // mount the PVC read-only
	NodeName  string            // optional: pin to kubernetes.io/hostname=<node>
	Label     map[string]string // pod labels (defaults to hasteward:heal-helper)
}

// RunHelperPodSpec creates the helper pod, waits for a terminal phase, returns its
// logs and whether it Succeeded, and deletes it on every exit path.
func (p *GaleraProvider) RunHelperPodSpec(ctx context.Context, spec HelperPodSpec) (string, bool, error) {
	ns := p.Config().Namespace
	c := k8s.GetClients()

	root := int64(0)
	labels := spec.Label
	if labels == nil {
		labels = map[string]string{"hasteward": "heal-helper"}
	}
	// Shared builder → Istio exemption applied. RunAsUID only (root); FSGroup is
	// deliberately omitted (it would recursively chown the mounted DB volume on start).
	pod := k8s.BuildHelperPod(k8s.HelperPodOpts{
		Name: spec.Name, Namespace: ns, Image: "docker.io/library/busybox:latest",
		Command: spec.Command, ServiceAccount: spec.SA,
		PVCName: spec.PVCName, MountPath: spec.MountPath, ReadOnly: spec.ReadOnly,
		RunAsUID: &root, NodeName: spec.NodeName, Labels: labels,
	})

	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", false, fmt.Errorf("failed to create helper pod %s: %w", spec.Name, err)
	}
	del := func() {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, spec.Name, metav1.DeleteOptions{GracePeriodSeconds: common.Ptr(int64(0))})
	}

	var lastPod *corev1.Pod
	for i := 0; i < 30; i++ {
		common.Sleep(5 * time.Second)
		pd, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, spec.Name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		lastPod = pd
		switch pd.Status.Phase {
		case corev1.PodSucceeded:
			logs := k8s.PodLogs(ctx, ns, spec.Name)
			del()
			return logs, true, nil
		case corev1.PodFailed:
			logs := k8s.PodLogs(ctx, ns, spec.Name)
			del()
			return logs, false, nil
		}
	}
	del()
	if sched := k8s.DescribePodScheduling(lastPod); sched != "" {
		return "", false, fmt.Errorf("helper pod %s did not complete — %s", spec.Name, sched)
	}
	return "", false, fmt.Errorf("helper pod %s timed out", spec.Name)
}

// RunHelperPod runs a script (sh -c) against a read-write PVC mount, debug-logs
// its output, and returns an error if the pod failed or timed out. Thin wrapper
// over RunHelperPodSpec for the heal/bootstrap/reconfigure call sites.
func (p *GaleraProvider) RunHelperPod(ctx context.Context, name, pvcName, mountPath, script, sa string) error {
	logs, ok, err := p.RunHelperPodSpec(ctx, HelperPodSpec{
		Name: name, PVCName: pvcName, MountPath: mountPath,
		Command: []string{"sh", "-c", script}, SA: sa,
	})
	if err != nil {
		return err
	}
	common.DebugLog("helper pod %s output:\n%s", name, logs)
	if !ok {
		return fmt.Errorf("helper pod %s failed", name)
	}
	return nil
}

// (logHelperPodOutput removed — RunHelperPodSpec returns logs directly.)
