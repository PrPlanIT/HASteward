package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// galeraPod builds a pod whose containerStatuses are in the ALPHABETICAL order
// Kubernetes actually reports — [agent, mariadb] — so index 0 is the agent
// sidecar. This is the shape that made the old ContainerStatuses[0] checks read
// the wrong container.
func galeraPod(name string, agentReady, mariadbReady bool, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{"app.kubernetes.io/instance": "c"},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", Ready: agentReady},
				{Name: "mariadb", Ready: mariadbReady},
			},
		},
	}
}

// TestReadinessByName pins the #3 fix at the helper level: readiness must be read
// from the mariadb container, never ContainerStatuses[0] (the agent).
func TestReadinessByName(t *testing.T) {
	// agent (index 0) Ready, mariadb NOT ready — the index-0 trap.
	pod := *galeraPod("c-0", true, false, corev1.PodRunning)
	if pod.Status.ContainerStatuses[0].Name != "agent" {
		t.Fatalf("precondition: index 0 should be the agent sidecar, got %q", pod.Status.ContainerStatuses[0].Name)
	}
	if ContainerReadyByName(pod, "mariadb") {
		t.Fatal("mariadb reported Ready when only the agent sidecar is ready (index-0 bug)")
	}
	if !ContainerReadyByName(pod, "agent") {
		t.Fatal("agent should be Ready")
	}
	if PodReady(pod, "mariadb") {
		t.Fatal("PodReady(mariadb) must be false when mariadb isn't ready")
	}
	if _, ok := ContainerStatusByName(pod, "mariadb"); !ok {
		t.Fatal("ContainerStatusByName should find the mariadb status")
	}
	if _, ok := ContainerStatusByName(pod, "nope"); ok {
		t.Fatal("ContainerStatusByName should not find an absent container")
	}

	// mariadb ready + Running -> PodReady true.
	if !PodReady(*galeraPod("c-1", true, true, corev1.PodRunning), "mariadb") {
		t.Fatal("PodReady(mariadb) should be true when mariadb ready + Running")
	}
	// mariadb ready but NOT Running -> PodReady false.
	if PodReady(*galeraPod("c-2", true, true, corev1.PodPending), "mariadb") {
		t.Fatal("PodReady must require the Running phase")
	}
}

func fakeClients(objs ...runtime.Object) func() {
	return SetClientsForTest(&Clients{Clientset: fake.NewSimpleClientset(objs...)})
}

// TestFindReadyPod_ByName proves FindReadyPod picks the node whose MARIADB is
// ready, not the one whose only-agent is ready — against a fake API server.
func TestFindReadyPod_ByName(t *testing.T) {
	// c-0: agent ready, mariadb NOT ready (index-0 logic would wrongly pick it).
	// c-1: mariadb ready (the correct donor/target).
	defer fakeClients(
		galeraPod("c-0", true, false, corev1.PodRunning),
		galeraPod("c-1", true, true, corev1.PodRunning),
	)()

	name, err := FindReadyPod(context.Background(), "ns", "app.kubernetes.io/instance=c", "mariadb")
	if err != nil {
		t.Fatalf("FindReadyPod: %v", err)
	}
	if name != "c-1" {
		t.Fatalf("FindReadyPod picked %q; want c-1 (mariadb ready), not c-0 (only agent ready)", name)
	}
}

func TestFindReadyPod_NoneReady(t *testing.T) {
	defer fakeClients(galeraPod("c-0", true, false, corev1.PodRunning))()
	if _, err := FindReadyPod(context.Background(), "ns", "app.kubernetes.io/instance=c", "mariadb"); err == nil {
		t.Fatal("expected an error when no pod's mariadb is ready")
	}
}

// TestWaitAllReady_ByName exercises the shared wait loop against a fake clientset
// with attempts=1, interval=0 (no real sleeping).
func TestWaitAllReady_ByName(t *testing.T) {
	t.Run("all mariadb ready -> true", func(t *testing.T) {
		defer fakeClients(
			galeraPod("c-0", true, true, corev1.PodRunning),
			galeraPod("c-1", true, true, corev1.PodRunning),
		)()
		if !WaitAllReady(context.Background(), "ns", "app.kubernetes.io/instance=c", 2, 1, 0, "mariadb") {
			t.Fatal("want true when both mariadb containers are ready")
		}
	})
	t.Run("one only-agent-ready -> false", func(t *testing.T) {
		defer fakeClients(
			galeraPod("c-0", true, true, corev1.PodRunning),
			galeraPod("c-1", true, false, corev1.PodRunning), // agent ready, mariadb not
		)()
		if WaitAllReady(context.Background(), "ns", "app.kubernetes.io/instance=c", 2, 1, 0, "mariadb") {
			t.Fatal("want false when a node's mariadb is not ready")
		}
	})
}

// TestExecHook proves the exec seam: both ExecCommand and the env-wrapping
// ExecCommandWithEnv route through the injected hook (which returns canned output
// so flow tests never need a real API server for exec).
func TestExecHook(t *testing.T) {
	defer SetExecHookForTest(func(ctx context.Context, pod, ns, container string, command []string) (*ExecResult, error) {
		joined := strings.Join(command, " ")
		switch {
		case strings.Contains(joined, "grastate.dat"):
			return &ExecResult{Stdout: "seqno: 42"}, nil
		case strings.Contains(joined, "SELECT 1"):
			return &ExecResult{Stdout: "1"}, nil
		default:
			return &ExecResult{Stdout: "unmatched"}, nil
		}
	})()

	res, err := ExecCommand(context.Background(), "c-0", "ns", "mariadb", []string{"cat", "/var/lib/mysql/grastate.dat"})
	if err != nil || res.Stdout != "seqno: 42" {
		t.Fatalf("ExecCommand did not route through the hook: %+v err=%v", res, err)
	}
	// ExecCommandWithEnv wraps argv as `sh -c <script> sh <realcmd...>`; the hook
	// still sees "SELECT 1" in the joined command.
	res2, err := ExecCommandWithEnv(context.Background(), "c-0", "ns", "mariadb",
		map[string]string{"MYSQL_PWD": "secret"}, []string{"mariadb", "-e", "SELECT 1"})
	if err != nil || res2.Stdout != "1" {
		t.Fatalf("ExecCommandWithEnv did not route through the hook: %+v err=%v", res2, err)
	}
}
