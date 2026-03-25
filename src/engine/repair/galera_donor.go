package repair

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DonorSelection holds the resolved donor decision for a repair run.
// Once resolved, it is immutable for the duration of the repair.
type DonorSelection struct {
	Pod     string           `json:"pod"`
	Ordinal int              `json:"ordinal"`
	Mode    string           `json:"mode"` // "auto" or "explicit"
	Probe   DonorProbeResult `json:"probe"`
}

// DonorProbeResult holds Galera-specific donor suitability signals.
type DonorProbeResult struct {
	PodExists      bool     `json:"podExists"`
	PodRunning     bool     `json:"podRunning"`
	ExecOK         bool     `json:"execOK"`
	WsrepReady     *bool    `json:"wsrepReady,omitempty"`
	WsrepConnected *bool    `json:"wsrepConnected,omitempty"`
	StateComment   string   `json:"stateComment,omitempty"`
	ClusterSize    int      `json:"clusterSize,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// resolveRepairDonor determines the donor for this repair run.
// Called once, result cached on galeraRepair.donorSelection.
func (g *galeraRepair) resolveRepairDonor(ctx context.Context, result *model.TriageResult) (*DonorSelection, error) {
	cfg := g.p.Config()

	// Explicit donor: operator declared authority
	if cfg.DonorInstance != nil {
		return g.resolveExplicitDonor(ctx, *cfg.DonorInstance, result)
	}

	// Auto mode: infer donor from triage data
	return g.resolveAutoDonor(ctx, result)
}

// resolveExplicitDonor validates an operator-declared donor.
func (g *galeraRepair) resolveExplicitDonor(ctx context.Context, ordinal int, result *model.TriageResult) (*DonorSelection, error) {
	cfg := g.p.Config()
	donorPod := fmt.Sprintf("%s-%d", cfg.ClusterName, ordinal)

	// Structural validation: ordinal in range
	replicas := int(g.p.Replicas())
	if ordinal >= replicas {
		return nil, fmt.Errorf("ABORT: donor ordinal %d is out of range (cluster has instances 0–%d)", ordinal, replicas-1)
	}

	// Structural validation: pod exists and is running
	c := k8s.GetClients()
	pod, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, donorPod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("ABORT: declared donor %s not found: %w", donorPod, err)
	}
	if pod.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("ABORT: declared donor %s is not Running (phase: %s)", donorPod, pod.Status.Phase)
	}

	// Suitability probe: Galera-specific wsrep check
	probe := g.probeWsrep(ctx, donorPod)

	// No bypass — donor must be verifiably suitable. Explicit intent ≠ valid donor.
	if !probe.ExecOK {
		return nil, fmt.Errorf("ABORT: Donor %s probe failed — cannot verify Galera suitability. "+
			"Ensure the donor pod is running and the mariadb container is accessible", donorPod)
	}
	if probe.WsrepReady == nil || !*probe.WsrepReady ||
		probe.WsrepConnected == nil || !*probe.WsrepConnected ||
		probe.StateComment != "Synced" {
		return nil, fmt.Errorf("ABORT: Donor %s is not Galera-suitable "+
			"(ready=%v connected=%v state=%s)", donorPod, probe.WsrepReady, probe.WsrepConnected, probe.StateComment)
	}

	common.InfoLog("Using operator-declared donor %s: wsrep_ready=ON, state=Synced — valid donor.", donorPod)

	for _, w := range probe.Warnings {
		common.WarnLog("Donor probe: %s", w)
	}

	return &DonorSelection{
		Pod:     donorPod,
		Ordinal: ordinal,
		Mode:    "explicit",
		Probe:   probe,
	}, nil
}

// resolveAutoDonor attempts to find an unambiguous donor from triage data.
func (g *galeraRepair) resolveAutoDonor(ctx context.Context, result *model.TriageResult) (*DonorSelection, error) {
	cfg := g.p.Config()
	ambiguous := !result.DataComparison.SafeToHeal

	// Find candidates: running + Galera-suitable (wsrep facts, not K8s readiness)
	var candidates []model.InstanceAssessment
	for _, a := range result.Assessments {
		if a.IsRunning && a.WsrepReady == "ON" && a.WsrepConnected == "ON" && a.WsrepStateComment == "Synced" {
			candidates = append(candidates, a)
		}
	}

	// No candidates at all
	if len(candidates) == 0 {
		if ambiguous {
			return nil, fmt.Errorf("ABORT: No healthy donor found and authority is ambiguous (divergent UUIDs/split-brain). " +
				"Use `--force --donor <ordinal>` to declare the authoritative source node")
		}
		return nil, fmt.Errorf("ABORT: No healthy donor nodes found. " +
			"All nodes are down or unhealthy. Cannot heal without a running donor to provide SST")
	}

	// Ambiguous authority: refuse auto-selection even with candidates
	if ambiguous {
		if cfg.Force {
			return nil, fmt.Errorf("ABORT: Authority is ambiguous (divergent UUIDs/split-brain) — cannot auto-select donor. " +
				"Use `--force --donor <ordinal>` to declare the authoritative source node")
		}
		return nil, fmt.Errorf("ABORT: Authority is ambiguous (divergent UUIDs/split-brain). " +
			"Review triage output and use `--force --donor <ordinal>` to declare the authoritative source node")
	}

	// Unambiguous: probe the first candidate via wsrep to confirm suitability
	donor := candidates[0]
	probe := g.probeWsrep(ctx, donor.Pod)

	if cfg.Force {
		common.WarnLog("Auto-selected donor %s based on unambiguous healthy state (--force active).", donor.Pod)
	} else {
		common.InfoLog("Auto-selected donor %s based on unambiguous healthy state.", donor.Pod)
	}

	return &DonorSelection{
		Pod:     donor.Pod,
		Ordinal: donor.Instance,
		Mode:    "auto",
		Probe:   probe,
	}, nil
}

// probeWsrep executes a wsrep status query directly on a donor pod.
func (g *galeraRepair) probeWsrep(ctx context.Context, podName string) DonorProbeResult {
	cfg := g.p.Config()
	result := DonorProbeResult{PodExists: true, PodRunning: true}

	c := k8s.GetClients()
	// Verify pod exists
	pod, err := c.Clientset.CoreV1().Pods(cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		result.PodExists = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("pod %s not found: %v", podName, err))
		return result
	}
	if pod.Status.Phase != corev1.PodRunning {
		result.PodRunning = false
		result.Warnings = append(result.Warnings, fmt.Sprintf("pod %s phase is %s, not Running", podName, pod.Status.Phase))
		return result
	}

	// Execute wsrep query
	execResult, err := k8s.ExecCommandWithEnv(ctx, podName, cfg.Namespace, "mariadb",
		map[string]string{"MYSQL_PWD": g.p.RootPassword()},
		[]string{"mariadb", "-u", "root", "--batch", "--skip-column-names", "-e",
			"SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS " +
				"WHERE VARIABLE_NAME IN (" +
				"'wsrep_local_state_comment', " +
				"'wsrep_connected', 'wsrep_ready', " +
				"'wsrep_cluster_size'" +
				") ORDER BY VARIABLE_NAME"})
	if err != nil {
		result.ExecOK = false
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("wsrep query exec failed on %s: %v — suitability check incomplete", podName, err))
		return result
	}
	result.ExecOK = true

	// Parse results
	for _, line := range strings.Split(execResult.Stdout, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
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
			result.ClusterSize, _ = strconv.Atoi(val)
		}
	}

	// Generate warnings for concerning probe results
	if result.WsrepReady != nil && !*result.WsrepReady {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s: wsrep_ready=OFF", podName))
	}
	if result.WsrepConnected != nil && !*result.WsrepConnected {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s: wsrep_connected=OFF", podName))
	}
	if result.StateComment != "" && result.StateComment != "Synced" {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s: state=%s (not Synced)", podName, result.StateComment))
	}

	return result
}

// displayDonorSelection prints the donor decision to output.
func displayDonorSelection(ds *DonorSelection) {
	output.Section("Donor Selection")
	output.Field("Mode", ds.Mode)
	output.Field("Donor", fmt.Sprintf("%s (ordinal %d)", ds.Pod, ds.Ordinal))
	if ds.Probe.ExecOK {
		ready := "unknown"
		if ds.Probe.WsrepReady != nil {
			if *ds.Probe.WsrepReady {
				ready = "ON"
			} else {
				ready = "OFF"
			}
		}
		output.Field("wsrep_ready", ready)
		output.Field("state", ds.Probe.StateComment)
		output.Field("cluster_size", fmt.Sprintf("%d", ds.Probe.ClusterSize))
	} else {
		output.Field("probe", "incomplete (exec failed)")
	}
}
