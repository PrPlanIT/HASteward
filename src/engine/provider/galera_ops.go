package provider

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
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
		time.Sleep(5 * time.Second)
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
		time.Sleep(5 * time.Second)
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
			time.Sleep(2 * time.Second)
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
	c := k8s.GetClients()
	cfg := p.Config()
	req := c.Clientset.CoreV1().Pods(cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	return string(data)
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
func (p *GaleraProvider) RunHelperPod(ctx context.Context, name, pvcName, mountPath, script, sa string) error {
	cfg := p.Config()
	ns := cfg.Namespace
	c := k8s.GetClients()

	rootUser := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"hasteward": "heal-helper"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: sa,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &rootUser,
			},
			Containers: []corev1.Container{{
				Name:    "healer",
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
						ClaimName: pvcName,
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
		pd, pErr := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if pErr != nil {
			continue
		}
		phase := string(pd.Status.Phase)
		if phase == "Succeeded" {
			p.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: common.Ptr(int64(0)),
			})
			time.Sleep(2 * time.Second)
			return nil
		}
		if phase == "Failed" {
			p.logHelperPodOutput(ctx, name)
			_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
				GracePeriodSeconds: common.Ptr(int64(0)),
			})
			return fmt.Errorf("helper pod %s failed", name)
		}
	}

	_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: common.Ptr(int64(0)),
	})
	return fmt.Errorf("helper pod %s timed out", name)
}

// logHelperPodOutput debug-logs a helper pod's output.
func (p *GaleraProvider) logHelperPodOutput(ctx context.Context, podName string) {
	c := k8s.GetClients()
	cfg := p.Config()
	req := c.Clientset.CoreV1().Pods(cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		common.DebugLog("Failed to get helper pod logs: %v", err)
		return
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	if len(data) > 0 {
		common.DebugLog("Helper pod output:\n%s", string(data))
	}
}
