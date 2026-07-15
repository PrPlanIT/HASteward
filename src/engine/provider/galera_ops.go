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
	deadline := int64(150)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "heal-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ServiceAccountName:    sa,
			ActiveDeadlineSeconds: &deadline,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &uid,
				FSGroup:    &uid,
			},
			Containers: []corev1.Container{{
				Name:  "wsrep-recover",
				Image: image,
				Command: []string{"sh", "-c", fmt.Sprintf(
					"mariadbd --wsrep-recover --datadir=/var/lib/mysql "+
						"--wsrep-on=ON --wsrep-provider=%s --wsrep-cluster-address=gcomm:// "+
						"--log-error-verbosity=3 2>&1; exit 0", galeraProviderSO)},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/var/lib/mysql"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			}},
		},
	}

	_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return WsrepRecoverResult{}, fmt.Errorf("failed to create wsrep_recover pod %s: %w", helperName, err)
	}

	var podOutput string
	for i := 0; i < 30; i++ {
		common.Sleep(5 * time.Second)
		pd, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, helperName, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
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
		return WsrepRecoverResult{}, fmt.Errorf("wsrep_recover pod %s produced no output or timed out", helperName)
	}

	common.DebugLog("wsrep_recover output for %s:\n%s", podName, podOutput)
	return ParseWsrepRecoverOutput(podOutput)
}

// helperPodOutput fetches a helper pod's logs.
func (p *GaleraProvider) helperPodOutput(ctx context.Context, podName string) string {
	return k8s.PodLogs(ctx, p.Config().Namespace, podName)
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
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: spec.SA,
			SecurityContext:    &corev1.PodSecurityContext{RunAsUser: &root},
			Containers: []corev1.Container{{
				Name:         "helper",
				Image:        "docker.io/library/busybox:latest",
				Command:      spec.Command,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: spec.MountPath, ReadOnly: spec.ReadOnly}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: spec.PVCName},
				},
			}},
		},
	}
	if spec.NodeName != "" {
		pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": spec.NodeName}
	}

	if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", false, fmt.Errorf("failed to create helper pod %s: %w", spec.Name, err)
	}
	del := func() {
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, spec.Name, metav1.DeleteOptions{GracePeriodSeconds: common.Ptr(int64(0))})
	}

	for i := 0; i < 30; i++ {
		common.Sleep(5 * time.Second)
		pd, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, spec.Name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
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
