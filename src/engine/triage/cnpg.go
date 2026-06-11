package triage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	Register("cnpg", func(ep provider.EngineProvider) (Triager, error) {
		p, ok := ep.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("cnpg triager requires *provider.CNPGProvider, got %T", ep)
		}
		return &cnpgTriage{p: p}, nil
	})
}

// cnpgTriage implements Triager for CNPG (CloudNativePG PostgreSQL) clusters.
type cnpgTriage struct {
	p    *provider.CNPGProvider
	data *cnpgTriageData
}

func (t *cnpgTriage) Name() string { return "cnpg" }

// --- Types ---

// controlData holds parsed pg_controldata fields for one instance.
type controlData struct {
	Pod                string
	Source             string // "exec", "pvc_probe", "none"
	Reachable          bool
	ClusterState       string
	Timeline           string
	CheckpointLocation string
	CheckpointTime     string
	MinRecoveryEnd     string
	CrashReason        string
}

// replicaInfo holds parsed pg_stat_replication row for one replica.
type replicaInfo struct {
	ClientAddr      string
	State           string
	SentLSN         string
	WriteLSN        string
	FlushLSN        string
	ReplayLSN       string
	WriteLag        string
	FlushLag        string
	ReplayLag       string
	ApplicationName string
}

// cnpgTriageData holds all data collected during the triage collection phase.
type cnpgTriageData struct {
	expectedInstances  []string
	runningPods        []corev1.Pod
	nonRunningPods     []corev1.Pod
	missingInstances   []string
	crashloopPods      []corev1.Pod
	controlData        []controlData
	streamingReplicas  []string
	replicationInfo    []replicaInfo
	diskUsage          map[string]int // pod -> percent used (legacy)
	diskStats          map[string]*model.DiskStats
	pvcCapacity        map[string]int64 // pod -> PVC capacity bytes (always available)
	pvcStates          map[string]string
	danglingPVCs       []string
	healthyPVCs        []string
	primaryIsRunning   bool
	primaryControlData *controlData
	primaryTimeline    string
	crashReasons       map[string]string
	walInfo            string
	slotInfo           []string
}

// cnpgProbeTarget identifies an instance whose PVC should be probed.
type cnpgProbeTarget struct {
	Name string
	Node string
}

// --- Collect ---

func (t *cnpgTriage) Collect(ctx context.Context) error {
	t.displayClusterStatus()

	data, err := t.triageCollect(ctx)
	if err != nil {
		return fmt.Errorf("triage collect failed: %w", err)
	}
	t.data = data
	return nil
}

func (t *cnpgTriage) triageCollect(ctx context.Context) (*cnpgTriageData, error) {
	c := k8s.GetClients()
	ns := t.p.Config().Namespace
	data := &cnpgTriageData{
		diskUsage:    make(map[string]int),
		diskStats:    make(map[string]*model.DiskStats),
		pvcCapacity:  make(map[string]int64),
		pvcStates:    make(map[string]string),
		crashReasons: make(map[string]string),
	}

	// Build expected instance list
	if names := k8s.GetNestedSlice(t.p.Cluster(), "status", "instanceNames"); len(names) > 0 {
		for _, n := range names {
			if s, ok := n.(string); ok {
				data.expectedInstances = append(data.expectedInstances, s)
			}
		}
	} else {
		for i := int64(1); i <= t.p.Instances(); i++ {
			data.expectedInstances = append(data.expectedInstances, fmt.Sprintf("%s-%d", t.p.Config().ClusterName, i))
		}
	}

	// Get all cluster pods
	podList, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("cnpg.io/cluster=%s", t.p.Config().ClusterName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	foundPodNames := make(map[string]bool)
	for i := range podList.Items {
		pod := podList.Items[i]
		foundPodNames[pod.Name] = true
		if pod.Status.Phase == corev1.PodRunning {
			data.runningPods = append(data.runningPods, pod)
		} else {
			data.nonRunningPods = append(data.nonRunningPods, pod)
		}
	}

	// Missing instances
	for _, name := range data.expectedInstances {
		if !foundPodNames[name] {
			data.missingInstances = append(data.missingInstances, name)
		}
	}

	// Check PVCs
	for _, name := range data.expectedInstances {
		pvc, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			data.pvcStates[name] = "MISSING"
		} else {
			data.pvcStates[name] = string(pvc.Status.Phase)
			if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
				data.pvcCapacity[name] = q.Value()
			}
		}
	}

	// Parse dangling/healthy PVCs from cluster status
	if dpvcs := k8s.GetNestedSlice(t.p.Cluster(), "status", "danglingPVC"); dpvcs != nil {
		for _, v := range dpvcs {
			if s, ok := v.(string); ok {
				data.danglingPVCs = append(data.danglingPVCs, s)
			}
		}
	}
	if hpvcs := k8s.GetNestedSlice(t.p.Cluster(), "status", "healthyPVC"); hpvcs != nil {
		for _, v := range hpvcs {
			if s, ok := v.(string); ok {
				data.healthyPVCs = append(data.healthyPVCs, s)
			}
		}
	}

	// Display pod overview
	cnpgDisplayPodOverview(data)

	// Identify crash-looping pods
	for _, pod := range data.runningPods {
		if len(pod.Status.ContainerStatuses) > 0 && !pod.Status.ContainerStatuses[0].Ready {
			data.crashloopPods = append(data.crashloopPods, pod)
		}
	}

	// Display non-running and crashloop pods
	cnpgDisplayPodDetails(data)

	// Fetch crash reasons from logs for crashloop pods
	for _, pod := range data.crashloopPods {
		logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "postgres",
		})
		logBytes, err := logReq.DoRaw(ctx)
		if err != nil {
			continue
		}
		logText := string(logBytes)
		if strings.Contains(logText, "low-disk space condition") || strings.Contains(logText, "low disk space") {
			data.crashReasons[pod.Name] = "disk_full"
		}
	}

	// pg_controldata on healthy running instances
	crashloopNames := podNameSet(data.crashloopPods)
	var healthyControlData []controlData

	output.Section("Timeline Analysis")

	for _, pod := range data.runningPods {
		if crashloopNames[pod.Name] {
			continue
		}
		result, err := k8s.ExecCommand(ctx, pod.Name, ns, "postgres",
			[]string{"pg_controldata", "/var/lib/postgresql/data/pgdata"})
		if err != nil {
			common.DebugLog("pg_controldata exec failed on %s: %v", pod.Name, err)
			continue
		}
		cd := parseControlData(pod.Name, "exec", result.Stdout)
		cd.Reachable = true
		healthyControlData = append(healthyControlData, cd)
	}

	// Identify instances needing PVC probe
	healthyNames := make(map[string]bool)
	for _, cd := range healthyControlData {
		healthyNames[cd.Pod] = true
	}

	// Build pod-to-node map for probe scheduling
	podNodes := make(map[string]string)
	for _, pod := range podList.Items {
		podNodes[pod.Name] = pod.Spec.NodeName
	}

	var probeInstances []cnpgProbeTarget
	for _, name := range data.expectedInstances {
		if !healthyNames[name] && data.pvcStates[name] == "Bound" {
			probeInstances = append(probeInstances, cnpgProbeTarget{Name: name, Node: podNodes[name]})
		}
	}

	// Create probe pods for stranded PVCs
	if len(probeInstances) > 0 {
		common.InfoLog("Probing PVC data for stranded instances: %s",
			joinCNPGProbeNames(probeInstances))

		imageName := k8s.GetNestedString(t.p.Cluster(), "spec", "imageName")
		sa := k8s.ServiceAccountFromPods(data.runningPods)
		probeResults, probeDisks := t.runPVCProbes(ctx, probeInstances, imageName, ns, sa)

		for name, cd := range probeResults {
			cd.CrashReason = data.crashReasons[name]
			healthyControlData = append(healthyControlData, cd)
		}
		for name, ds := range probeDisks {
			cnpgFillTotal(ds, data.pvcCapacity[name])
			data.diskStats[name] = ds
		}
	}

	// Add entries for instances we couldn't probe at all
	probedNames := make(map[string]bool)
	for _, cd := range healthyControlData {
		probedNames[cd.Pod] = true
	}
	for _, name := range data.expectedInstances {
		if !probedNames[name] {
			healthyControlData = append(healthyControlData, controlData{
				Pod:                name,
				Source:             "none",
				Reachable:          false,
				ClusterState:       "unknown",
				Timeline:           "unknown",
				CheckpointLocation: "unknown",
				CheckpointTime:     "unknown",
				MinRecoveryEnd:     "unknown",
				CrashReason:        data.crashReasons[name],
			})
		}
	}

	data.controlData = healthyControlData

	// Display per-instance controldata
	for _, cd := range data.controlData {
		displayControlData(cd)
	}

	// Identify primary controldata
	currentPrimary := k8s.GetNestedString(t.p.Cluster(), "status", "currentPrimary")
	for i := range data.controlData {
		if data.controlData[i].Pod == currentPrimary {
			data.primaryControlData = &data.controlData[i]
			data.primaryTimeline = strings.TrimSpace(data.controlData[i].Timeline)
			break
		}
	}
	if data.primaryControlData == nil {
		data.primaryControlData = &controlData{Timeline: "unknown", CheckpointLocation: "unknown"}
		data.primaryTimeline = "unknown"
	}

	// Check if primary is running
	data.primaryIsRunning = false
	for _, pod := range data.runningPods {
		if pod.Name == currentPrimary {
			data.primaryIsRunning = true
			break
		}
	}

	// Replication status from primary
	output.Section(fmt.Sprintf("Replication Status (from %s)", currentPrimary))
	if data.primaryIsRunning {
		t.collectReplicationStatus(ctx, data, currentPrimary, ns)
	} else {
		output.Warn("Primary is not running - cannot query replication status")
	}

	// Replication slots
	output.Section("Replication Slots")
	if data.primaryIsRunning {
		t.collectReplicationSlots(ctx, data, currentPrimary, ns)
	}

	// WAL info
	if data.primaryIsRunning {
		t.collectWALInfo(ctx, data, currentPrimary, ns)
	}

	// Disk breakdown on running instances (fast path via exec).
	output.Section("Disk Space")
	for _, pod := range data.runningPods {
		result, err := k8s.ExecCommand(ctx, pod.Name, ns, "postgres",
			[]string{"sh", "-c", cnpgDiskScript})
		if err != nil {
			output.Printf("%s: unable to check\n", pod.Name)
			continue
		}
		ds := parseDiskStats(result.Stdout, "exec")
		cnpgFillTotal(ds, data.pvcCapacity[pod.Name])
		data.diskStats[pod.Name] = ds
		data.diskUsage[pod.Name] = ds.UsedPercent
		output.Printf("%s: %s used / %s total (%d%%) — wal %s, data %s, %d segs\n",
			pod.Name, output.FormatBytes(ds.UsedBytes), output.FormatBytes(ds.TotalBytes),
			ds.UsedPercent, output.FormatBytes(ds.WALBytes), output.FormatBytes(ds.DataBytes),
			ds.WALSegments)
	}

	// Backfill: any instance not covered by exec or PVC probe still reports its true
	// capacity (from the PVC object) — never a silent zero.
	for _, name := range data.expectedInstances {
		if data.diskStats[name] != nil {
			continue
		}
		if capBytes := data.pvcCapacity[name]; capBytes > 0 {
			data.diskStats[name] = &model.DiskStats{Source: "pvc_capacity_only", TotalBytes: capBytes}
		} else {
			data.diskStats[name] = &model.DiskStats{Source: "none"}
		}
	}

	return data, nil
}

func (t *cnpgTriage) collectReplicationStatus(ctx context.Context, data *cnpgTriageData, primary, ns string) {
	result, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", "postgres", "-t", "-A", "-F", "|", "-c",
		"SELECT client_addr, state, sent_lsn, write_lsn, flush_lsn, replay_lsn, " +
			"write_lag, flush_lag, replay_lag, application_name " +
			"FROM pg_stat_replication ORDER BY application_name",
	})
	if err != nil {
		output.Warn("Could not query replication status: %v", err)
		return
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		output.Warn("No active replication connections found")
		return
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		output.Println(line)
		parts := strings.Split(line, "|")
		if len(parts) >= 10 && parts[1] == "streaming" {
			data.streamingReplicas = append(data.streamingReplicas, parts[9])
		}
		if len(parts) >= 10 {
			data.replicationInfo = append(data.replicationInfo, replicaInfo{
				ClientAddr: parts[0], State: parts[1], SentLSN: parts[2],
				WriteLSN: parts[3], FlushLSN: parts[4], ReplayLSN: parts[5],
				WriteLag: parts[6], FlushLag: parts[7], ReplayLag: parts[8],
				ApplicationName: parts[9],
			})
		}
	}
}

func (t *cnpgTriage) collectReplicationSlots(ctx context.Context, data *cnpgTriageData, primary, ns string) {
	result, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", "postgres", "-t", "-A", "-F", "|", "-c",
		"SELECT slot_name, slot_type, active, restart_lsn, " +
			"confirmed_flush_lsn, " +
			"pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS bytes_behind " +
			"FROM pg_replication_slots ORDER BY slot_name",
	})
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		output.Println("No replication slots found")
		return
	}
	for _, line := range lines {
		if line != "" {
			output.Println(line)
			data.slotInfo = append(data.slotInfo, line)
		}
	}
}

func (t *cnpgTriage) collectWALInfo(ctx context.Context, data *cnpgTriageData, primary, ns string) {
	result, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", "postgres", "-t", "-A", "-F", "|", "-c",
		"SELECT pg_current_wal_lsn() AS current_lsn, " +
			"current_setting('max_slot_wal_keep_size') AS max_slot_wal_keep_size, " +
			"current_setting('wal_keep_size') AS wal_keep_size",
	})
	if err != nil {
		return
	}
	output.Section("WAL Info")
	data.walInfo = strings.TrimSpace(result.Stdout)
	output.Println(data.walInfo)
}

// runPVCProbes creates ephemeral probe pods to read pg_controldata from PVCs
// of non-running instances.
func (t *cnpgTriage) runPVCProbes(ctx context.Context, targets []cnpgProbeTarget, imageName, ns, sa string) (map[string]controlData, map[string]*model.DiskStats) {
	c := k8s.GetClients()
	results := make(map[string]controlData)
	disks := make(map[string]*model.DiskStats)
	uid := int64(26)

	for _, tgt := range targets {
		probeName := tgt.Name + "-triage-probe"

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probeName,
				Namespace: ns,
				Labels:    map[string]string{"cnpg-triage": "probe"},
			},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: sa,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser:  &uid,
					RunAsGroup: &uid,
					FSGroup:    &uid,
				},
				Containers: []corev1.Container{{
					Name:  "probe",
					Image: imageName,
					// Read pg_controldata AND the disk breakdown in one read-only pass,
					// so stranded (down / crash-looping) instances still report true usage.
					Command: []string{"sh", "-c", "pg_controldata /var/lib/postgresql/data/pgdata 2>/dev/null; " + cnpgDiskScript},
					VolumeMounts: []corev1.VolumeMount{{
						Name:      "pgdata",
						MountPath: "/var/lib/postgresql/data",
						ReadOnly:  true,
					}},
				}},
				Volumes: []corev1.Volume{{
					Name: "pgdata",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: tgt.Name,
						},
					},
				}},
			},
		}

		if tgt.Node != "" {
			pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": tgt.Node}
		}

		_, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			common.WarnLog("Failed to create probe pod for %s: %v", tgt.Name, err)
			continue
		}

		// Wait for probe to complete
		cd, ds := t.waitAndCollectProbe(ctx, probeName, tgt.Name, ns)
		results[tgt.Name] = cd
		if ds != nil {
			disks[tgt.Name] = ds
		}

		// Cleanup
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, probeName, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
	}

	return results, disks
}

func (t *cnpgTriage) waitAndCollectProbe(ctx context.Context, probeName, instanceName, ns string) (controlData, *model.DiskStats) {
	c := k8s.GetClients()

	// Poll for completion (30 retries, 5s delay = 150s max)
	for attempt := 0; attempt < 30; attempt++ {
		pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, probeName, metav1.GetOptions{})
		if err != nil {
			break
		}
		phase := pod.Status.Phase
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			break
		}
		time.Sleep(5 * time.Second)
	}

	// Get logs
	logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(probeName, &corev1.PodLogOptions{
		Container: "probe",
	})
	logBytes, err := logReq.DoRaw(ctx)
	if err != nil || len(logBytes) == 0 {
		return controlData{Pod: instanceName, Source: "none", ClusterState: "unknown",
			Timeline: "unknown", CheckpointLocation: "unknown", CheckpointTime: "unknown",
			MinRecoveryEnd: "unknown"}, nil
	}

	cd := parseControlData(instanceName, "pvc_probe", string(logBytes))
	ds := parseDiskStats(string(logBytes), "pvc_probe")
	return cd, ds
}

// --- Analyze ---

func (t *cnpgTriage) Analyze(_ context.Context) (*model.TriageResult, error) {
	data := t.data
	if data == nil {
		return nil, fmt.Errorf("Analyze called before Collect")
	}

	currentPrimary := k8s.GetNestedString(t.p.Cluster(), "status", "currentPrimary")

	// Cross-instance comparison
	comparison := cnpgCrossInstanceComparison(data, currentPrimary)

	// Display comparison
	output.Section("Data Freshness Check")
	for _, w := range comparison.Warnings {
		output.Println(w)
	}
	if !comparison.SafeToHeal {
		output.Println()
		output.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		output.Println("  CRITICAL: POTENTIAL SPLIT-BRAIN DETECTED")
		output.Println("  A non-primary instance has MORE RECENT data than the primary!")
		output.Printf("  Most advanced instance: %s\n", comparison.MostAdvanced)
		output.Println("  DO NOT blindly heal - review the data above and decide manually.")
		output.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		output.Println()
	}

	// Flag timeline divergence
	for _, cd := range data.controlData {
		if data.primaryTimeline != "unknown" && cd.Timeline != "unknown" &&
			cd.Timeline != data.primaryTimeline && cd.Pod != currentPrimary {
			output.Printf("DIVERGENCE: %s is on timeline %s but primary is on timeline %s\n",
				cd.Pod, cd.Timeline, data.primaryTimeline)
		}
	}

	// Build per-instance assessments
	assessments := t.buildAssessments(data, &comparison, currentPrimary)

	readyCount := 0
	if v := k8s.GetNestedInt64(t.p.Cluster(), "status", "readyInstances"); v > 0 {
		readyCount = int(v)
	}

	result := &model.TriageResult{
		Engine: t.Name(),
		Cluster: model.ObjectRef{
			Namespace: t.p.Config().Namespace,
			Name:      t.p.Config().ClusterName,
		},
		Assessments:    assessments,
		DataComparison: comparison,
		ClusterPhase:   getMapString(t.p.Status(), "phase"),
		ReadyCount:     readyCount,
		TotalCount:     int(t.p.Instances()),
	}

	// CNPG authority status (mirrors the Galera pattern) + recovery projection.
	if comparison.SafeToHeal && currentPrimary != "" {
		result.AuthorityStatus = "unambiguous"
		result.RecommendedDonor = cnpgOrdinal(currentPrimary)
	} else {
		result.AuthorityStatus = "ambiguous"
		result.RecommendedDonor = "none"
	}
	result.Recovery = deriveRecovery(assessments, comparison, currentPrimary, result.ClusterPhase, data.primaryIsRunning)

	// Display
	t.triageDisplay(data, result)

	return result, nil
}

func cnpgCrossInstanceComparison(data *cnpgTriageData, primaryName string) model.DataComparison {
	pTL := int64(0)
	pLSN := "unknown"
	if data.primaryControlData != nil {
		pTL = parseTimelineInt(data.primaryTimeline)
		pLSN = data.primaryControlData.CheckpointLocation
	}

	mostAdvanced := primaryName
	mostAdvancedTL := pTL
	mostAdvancedLSN := pLSN

	var warnings []string
	var splitBrain []string

	for _, inst := range data.controlData {
		if inst.Pod == primaryName || inst.Timeline == "unknown" {
			continue
		}
		instTL := parseTimelineInt(inst.Timeline)
		if instTL > mostAdvancedTL {
			mostAdvanced = inst.Pod
			mostAdvancedTL = instTL
			mostAdvancedLSN = inst.CheckpointLocation
			splitBrain = append(splitBrain,
				fmt.Sprintf("%s has timeline %s > primary timeline %d", inst.Pod, inst.Timeline, pTL))
		} else if instTL == mostAdvancedTL && inst.CheckpointLocation != "unknown" && pLSN != "unknown" {
			instLSNVal := parseLSNValue(inst.CheckpointLocation)
			pLSNVal := parseLSNValue(pLSN)
			if instLSNVal > pLSNVal {
				splitBrain = append(splitBrain,
					fmt.Sprintf("%s LSN %s ahead of primary %s on same timeline",
						inst.Pod, inst.CheckpointLocation, pLSN))
			}
		}
	}

	safe := len(splitBrain) == 0
	if safe {
		warnings = append(warnings,
			fmt.Sprintf("OK: Primary %s has the most recent data (timeline %d, LSN %s)",
				primaryName, pTL, pLSN))
	} else {
		for _, sb := range splitBrain {
			warnings = append(warnings, "SPLIT-BRAIN RISK: "+sb)
		}
	}

	return model.DataComparison{
		MostAdvanced:       mostAdvanced,
		MostAdvancedValue:  mostAdvancedTL,
		CheckpointLocation: mostAdvancedLSN,
		SafeToHeal:         safe,
		Warnings:           warnings,
		SplitBrainDetails:  splitBrain,
	}
}

func (t *cnpgTriage) buildAssessments(data *cnpgTriageData, comparison *model.DataComparison,
	primaryName string) []model.InstanceAssessment {

	pTL := data.primaryTimeline
	pLSN := "unknown"
	if data.primaryControlData != nil {
		pLSN = strings.TrimSpace(data.primaryControlData.CheckpointLocation)
	}
	pLSNVal := parseLSNValue(pLSN)

	missingSet := setFromSlice(data.missingInstances)
	crashloopSet := podNameSet(data.crashloopPods)
	streamingSet := setFromSlice(data.streamingReplicas)

	var assessments []model.InstanceAssessment

	for _, inst := range data.controlData {
		isPrimary := inst.Pod == primaryName
		isMissing := missingSet[inst.Pod]
		isCrashloop := crashloopSet[inst.Pod]
		isStreaming := streamingSet[inst.Pod]
		diskFull := inst.CrashReason == "disk_full"
		diskPct := data.diskUsage[inst.Pod]
		hasData := inst.Source != "none"
		instTL := strings.TrimSpace(inst.Timeline)
		instLSN := strings.TrimSpace(inst.CheckpointLocation)
		instLSNVal := parseLSNValue(instLSN)

		sameTL := instTL == pTL && instTL != "unknown"
		behindTL := instTL != "unknown" && pTL != "unknown" && parseTimelineInt(instTL) < parseTimelineInt(pTL)
		behindLSN := sameTL && instLSNVal < pLSNVal
		aheadLSN := sameTL && instLSNVal > pLSNVal
		aheadTL := instTL != "unknown" && pTL != "unknown" && parseTimelineInt(instTL) > parseTimelineInt(pTL)

		isAuthority := inst.Pod == comparison.MostAdvanced
		classification := classifyInstance(isPrimary, isAuthority, hasData, comparison.SafeToHeal, behindTL, sameTL)

		var notes []string
		var recommendation string
		needsHeal := false

		// Extract instance number for heal command
		parts := strings.Split(inst.Pod, "-")
		replicaNum := parts[len(parts)-1]
		healCmd := fmt.Sprintf("hasteward repair -e cnpg -c %s -n %s --instance %s --backups-path /backups",
			t.p.Config().ClusterName, t.p.Config().Namespace, replicaNum)

		switch {
		case isPrimary:
			if diskFull || diskPct >= 90 {
				notes = append(notes, "PRIMARY - disk full/low")
				recommendation = "Primary disk is full. Expand PVC storage in the Cluster spec."
			} else {
				notes = append(notes, "PRIMARY - healthy")
				recommendation = "No action needed."
			}

		case !comparison.SafeToHeal:
			if aheadTL || aheadLSN {
				notes = append(notes, "AHEAD OF PRIMARY - potential split-brain")
				recommendation = "MANUAL REVIEW REQUIRED. This instance has data ahead of the primary. " +
					"Do NOT heal without understanding the data state. " +
					"Consider promoting this instance or performing manual data recovery."
			} else if !hasData {
				notes = append(notes, "NO DATA - cannot assess during split-brain")
				recommendation = "MANUAL REVIEW REQUIRED. Cannot determine this instance state. Resolve split-brain first."
			} else {
				notes = append(notes, "behind primary but split-brain detected elsewhere")
				recommendation = "MANUAL REVIEW REQUIRED. Split-brain detected in cluster. Resolve the split-brain before healing any replicas."
			}

		case !hasData:
			notes = append(notes, "NO DATA - could not probe PVC")
			pvcSt := data.pvcStates[inst.Pod]
			notes = append(notes, "PVC: "+pvcSt)
			if pvcSt == "MISSING" {
				recommendation = "PVC is missing. Check CNPG operator logs."
			} else {
				recommendation = "Could not probe PVC data. Check if pod can be scheduled and PVC can be mounted."
			}

		case behindTL:
			needsHeal = true
			notes = append(notes, fmt.Sprintf("behind: timeline %s < primary %s", instTL, pTL))
			if diskFull {
				notes = append(notes, "disk full (WAL accumulation from being stuck)")
			}
			recommendation = fmt.Sprintf("Needs heal (pg_basebackup). Cannot catch up via streaming - different timeline.\n\n  %s", healCmd)

		case sameTL && behindLSN && isStreaming:
			notes = append(notes, "healthy (streaming, checkpoint LSN slightly behind - normal)")
			if diskPct >= 90 {
				notes = append(notes, fmt.Sprintf("disk low (%d%%)", diskPct))
				recommendation = "Streaming OK but disk usage is high. Consider expanding PVC storage."
			} else {
				recommendation = "No action needed."
			}

		case sameTL && behindLSN:
			notes = append(notes, fmt.Sprintf("same timeline, behind by LSN (%s < %s), not streaming", instLSN, pLSN))
			switch {
			case diskFull:
				needsHeal = true
				notes = append(notes, "disk full (WAL accumulation from being stuck)")
				recommendation = fmt.Sprintf("Needs heal. Same timeline but disk full prevents catch-up.\n\n  %s", healCmd)
			case isMissing:
				notes = append(notes, "no pod running")
				recommendation = "Pod missing but data is on correct timeline. " +
					"CNPG should recreate the pod. If it does not, check cluster phase. " +
					"May catch up via streaming if WAL is still available."
			case isCrashloop:
				notes = append(notes, "crash-looping")
				recommendation = fmt.Sprintf("Same timeline but crash-looping. Check pod logs for root cause. "+
					"If WAL is still available, may recover on restart. "+
					"Otherwise needs heal.\n\n  %s", healCmd)
			default:
				needsHeal = true
				recommendation = fmt.Sprintf("Not streaming. May catch up if WAL is still available. "+
					"Check replication slots above - if the slot has no restart_lsn, "+
					"WAL has been discarded and a heal is needed.\n\n  %s", healCmd)
			}

		case sameTL && !behindLSN:
			switch {
			case isMissing:
				notes = append(notes, "data current but no pod")
				recommendation = "Data is current. CNPG should recreate the pod. If it does not, check cluster phase."
			case isCrashloop:
				notes = append(notes, "data current but crash-looping")
				recommendation = "Data is current but pod is crash-looping. Check pod logs for root cause."
			case diskPct >= 90:
				notes = append(notes, fmt.Sprintf("healthy but disk low (%d%%)", diskPct))
				recommendation = "Healthy but disk usage is high. Consider expanding PVC storage."
			default:
				notes = append(notes, "healthy")
				recommendation = "No action needed."
			}

		default:
			notes = append(notes, "timeline unknown")
			recommendation = "Could not determine timeline. Check instance manually."
		}

		assessments = append(assessments, model.InstanceAssessment{
			Pod:            inst.Pod,
			IsPrimary:      isPrimary,
			Timeline:       parseTimelineInt(instTL),
			LSN:            instLSN,
			Classification: classification,
			Notes:          notes,
			Recommendation: recommendation,
			NeedsHeal:      needsHeal,
			DiskPct:        diskPct,
			Disk:           data.diskStats[inst.Pod],
		})
	}

	return assessments
}

// --- Display ---

func (t *cnpgTriage) displayClusterStatus() {
	output.Section("Cluster Status")
	output.Field("Phase", getMapString(t.p.Status(), "phase"))
	output.Field("Instances", fmt.Sprintf("%d", t.p.Instances()))
	output.Field("Ready instances", fmt.Sprintf("%v", t.p.Status()["readyInstances"]))
	output.Field("Current primary", getMapString(t.p.Status(), "currentPrimary"))
	output.Field("Target primary", getMapString(t.p.Status(), "targetPrimary"))
	output.Field("Timeline ID", fmt.Sprintf("%v", t.p.Status()["timelineID"]))
	output.Field("PostgreSQL image", getMapString(t.p.Spec(), "imageName"))
	output.Field("Fenced instances", fmt.Sprintf("%v", t.p.FencedInstances()))
}

func cnpgDisplayPodOverview(data *cnpgTriageData) {
	output.Section("Pod Overview")
	output.Field("Expected instances", strings.Join(data.expectedInstances, ", "))
	output.Field("Running", joinPodNames(data.runningPods))
	output.Field("Non-running", joinPodNames(data.nonRunningPods))
	output.Field("Missing (no pod)", strings.Join(data.missingInstances, ", "))

	if len(data.danglingPVCs) > 0 || len(data.missingInstances) > 0 {
		output.Section("PVC State")
		output.Field("Healthy PVCs", strings.Join(data.healthyPVCs, ", "))
		output.Field("Dangling PVCs", strings.Join(data.danglingPVCs, ", "))
	}
}

func cnpgDisplayPodDetails(data *cnpgTriageData) {
	for _, pod := range data.nonRunningPods {
		reason := "N/A"
		restarts := int32(0)
		if len(pod.Status.ContainerStatuses) > 0 {
			cs := pod.Status.ContainerStatuses[0]
			restarts = cs.RestartCount
			if cs.State.Waiting != nil {
				reason = cs.State.Waiting.Reason
			} else if cs.State.Terminated != nil {
				reason = cs.State.Terminated.Reason
			}
		}
		output.Printf("%s: phase=%s reason=%s restarts=%d\n", pod.Name, pod.Status.Phase, reason, restarts)
	}
	for _, pod := range data.crashloopPods {
		restarts := int32(0)
		if len(pod.Status.ContainerStatuses) > 0 {
			restarts = pod.Status.ContainerStatuses[0].RestartCount
		}
		output.Printf("CRASH-LOOP: %s: phase=Running ready=false restarts=%d\n", pod.Name, restarts)
	}
}

func displayControlData(cd controlData) {
	srcLabel := ""
	switch cd.Source {
	case "pvc_probe":
		srcLabel = " (from PVC probe - pod not running)"
	case "none":
		srcLabel = " (NO DATA - could not probe)"
	}
	diskLabel := ""
	if cd.CrashReason == "disk_full" {
		diskLabel = " [DISK FULL]"
	}
	output.Printf("%s%s%s\n", cd.Pod, srcLabel, diskLabel)
	output.Printf("  State: %s\n", cd.ClusterState)
	output.Printf("  Timeline: %s\n", cd.Timeline)
	output.Printf("  Checkpoint LSN: %s\n", cd.CheckpointLocation)
	output.Printf("  Checkpoint time: %s\n", cd.CheckpointTime)
	output.Printf("  Min recovery end: %s\n", cd.MinRecoveryEnd)
}

func (t *cnpgTriage) triageDisplay(data *cnpgTriageData, result *model.TriageResult) {
	output.Banner("TRIAGE SUMMARY")

	currentPrimary := k8s.GetNestedString(t.p.Cluster(), "status", "currentPrimary")
	output.Printf("Cluster: %s (%s)\n", t.p.Config().ClusterName, t.p.Config().Namespace)
	output.Printf("Primary: %s (timeline %s, LSN %s)\n",
		currentPrimary, data.primaryTimeline,
		data.primaryControlData.CheckpointLocation)
	output.Printf("Phase: %s\n", result.ClusterPhase)
	output.Printf("Ready: %d/%d\n", result.ReadyCount, result.TotalCount)
	if result.DataComparison.SafeToHeal {
		output.Println("Safe to heal replicas: YES - primary has most recent data")
	} else {
		output.Println("Safe to heal replicas: NO - SPLIT-BRAIN DETECTED - review data above")
	}
	output.Println()

	// Per-instance assessment
	for _, a := range result.Assessments {
		primaryTag := ""
		if a.IsPrimary {
			primaryTag = " [PRIMARY]"
		}
		classTag := ""
		if a.Classification != "" {
			classTag = " {" + string(a.Classification) + "}"
		}
		output.Printf("%s%s%s: %s\n", a.Pod, primaryTag, classTag, strings.Join(a.Notes, ", "))
		output.Printf("  Timeline: %d | LSN: %s\n", a.Timeline, a.LSN)
		if d := a.Disk; d != nil {
			output.Printf("  Disk: %s/%s (%d%%) — wal %s, data %s, %d segs [%s]\n",
				output.FormatBytes(d.UsedBytes), output.FormatBytes(d.TotalBytes), d.UsedPercent,
				output.FormatBytes(d.WALBytes), output.FormatBytes(d.DataBytes), d.WALSegments, d.Source)
		}
		output.Printf("  >> %s\n", a.Recommendation)
	}

	// Recovery assessment (classification projection)
	if r := result.Recovery; r != nil {
		output.Section("Recovery Assessment")
		auth := r.Authority
		if auth == "" {
			auth = "(ambiguous)"
		}
		output.Printf("Authority: %s\n", auth)
		if len(r.Disposable) > 0 {
			output.Printf("Disposable: %s\n", strings.Join(r.Disposable, ", "))
		}
		switch {
		case r.Blocked:
			output.Printf("Deadlock: BLOCKED (%s)\n", r.Reason)
			output.Printf("Recovery set (must be escrow-reversible): %s\n", strings.Join(r.RecoverySet, ", "))
			output.Printf(">> hasteward repair -e cnpg -c %s -n %s --unwedge\n",
				t.p.Config().ClusterName, t.p.Config().Namespace)
		case r.Reason == "ambiguous_authority":
			output.Println("Authority ambiguous — deadlock breaker unavailable (refuse).")
		}
	}

	// Suggested commands
	healCount := 0
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			healCount++
		}
	}
	if healCount > 0 {
		output.SuggestedCommands("cnpg", t.p.Config().ClusterName, t.p.Config().Namespace)
	}
}

// --- Helpers ---

func parseControlData(podName, source, raw string) controlData {
	cd := controlData{
		Pod:                podName,
		Source:             source,
		ClusterState:       "unknown",
		Timeline:           "unknown",
		CheckpointLocation: "unknown",
		CheckpointTime:     "unknown",
		MinRecoveryEnd:     "unknown",
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Database cluster state:") {
			cd.ClusterState = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "Latest checkpoint's TimeLineID:") {
			cd.Timeline = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "Latest checkpoint location:") {
			cd.CheckpointLocation = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "Time of latest checkpoint:") {
			cd.CheckpointTime = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "Min recovery ending location:") {
			cd.MinRecoveryEnd = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	return cd
}

func parseLSNValue(lsn string) int64 {
	if lsn == "" || lsn == "unknown" {
		return 0
	}
	parts := strings.Split(lsn, "/")
	if len(parts) != 2 {
		return 0
	}
	hi, err1 := strconv.ParseInt(parts[0], 16, 64)
	lo, err2 := strconv.ParseInt(parts[1], 16, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return hi*4294967296 + lo
}

func parseTimelineInt(tl string) int64 {
	tl = strings.TrimSpace(tl)
	if tl == "" || tl == "unknown" {
		return 0
	}
	n, err := strconv.ParseInt(tl, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func joinCNPGProbeNames(targets []cnpgProbeTarget) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// --- Disk breakdown ---

// cnpgDiskScript collects a df + du breakdown of a mounted pgdata volume. It is
// read-only and needs no running postgres, so it works on a stranded PVC probe
// (crash-looping / pod-gone instances) exactly as on a live pod — the universal
// disk collector. Combined with pg_controldata it reuses the same probe pod.
const cnpgDiskScript = `echo "===DF==="; df -k /var/lib/postgresql/data 2>/dev/null | tail -1
echo "===WAL==="; du -sk /var/lib/postgresql/data/pgdata/pg_wal 2>/dev/null | tail -1
echo "===PGDATA==="; du -sk /var/lib/postgresql/data/pgdata 2>/dev/null | tail -1
echo "===SEGMENTS==="; ls -1 /var/lib/postgresql/data/pgdata/pg_wal 2>/dev/null | grep -cE '^[0-9A-F]{24}$'`

// parseDiskStats parses cnpgDiskScript output into a DiskStats. Source records how
// the data was obtained so an unreadable instance is explicit, never a silent zero.
func parseDiskStats(raw, source string) *model.DiskStats {
	ds := &model.DiskStats{Source: source}
	secs := map[string]string{}
	cur := ""
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimSpace(line)
		switch l {
		case "===DF===", "===WAL===", "===PGDATA===", "===SEGMENTS===":
			cur = strings.Trim(l, "=")
			continue
		}
		if cur != "" && l != "" && secs[cur] == "" {
			secs[cur] = l
		}
	}
	if df := secs["DF"]; df != "" {
		// Filesystem 1K-blocks Used Available Use% Mounted-on
		if f := strings.Fields(df); len(f) >= 5 {
			ds.TotalBytes = kbToBytes(f[1])
			ds.UsedBytes = kbToBytes(f[2])
			ds.FreeBytes = kbToBytes(f[3])
			ds.UsedPercent = parsePct(f[4])
		}
	}
	if w := secs["WAL"]; w != "" {
		ds.WALBytes = kbToBytes(strings.Fields(w)[0])
	}
	if p := secs["PGDATA"]; p != "" {
		ds.DataBytes = kbToBytes(strings.Fields(p)[0]) - ds.WALBytes
		if ds.DataBytes < 0 {
			ds.DataBytes = 0
		}
	}
	if s := secs["SEGMENTS"]; s != "" {
		ds.WALSegments, _ = strconv.Atoi(strings.TrimSpace(s))
	}
	return ds
}

// cnpgFillTotal backfills TotalBytes from the PVC capacity when df didn't report it.
func cnpgFillTotal(ds *model.DiskStats, capacityBytes int64) {
	if ds != nil && ds.TotalBytes == 0 && capacityBytes > 0 {
		ds.TotalBytes = capacityBytes
	}
}

func kbToBytes(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * 1024
}

func parsePct(s string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if err != nil {
		return 0
	}
	return n
}

// --- Classification + recovery (projection over existing signals) ---

// classifyInstance answers "can this PVC ever be authoritative again?" as a
// projection over signals triage already computes. It fails closed: anything not
// provably disposable (unreadable data, split-brain, unknown timeline) is Unknown.
func classifyInstance(isPrimary, isAuthority, hasData, safeToHeal, behindTL, sameTL bool) model.Classification {
	if !hasData || !safeToHeal {
		return model.ClassUnknown // unreadable, or ambiguous authority → refuse
	}
	if isPrimary || isAuthority {
		return model.ClassAuthoritative
	}
	if behindTL {
		return model.ClassDisposable // dead timeline; re-clone is its only path home
	}
	if sameTL {
		return model.ClassRecoverable // same timeline; can rejoin without a wipe
	}
	return model.ClassUnknown
}

// deriveRecovery projects the per-instance classifications into a Recovery block:
// whether the cluster is in a breakable deadlock and what must be escrow-reversible.
// Returns nil for a healthy cluster (nothing disposable, no deadlock).
func deriveRecovery(assessments []model.InstanceAssessment, comparison model.DataComparison,
	primaryName, clusterPhase string, primaryRunning bool) *model.Recovery {

	if !comparison.SafeToHeal || primaryName == "" {
		// authority cannot be established unambiguously → refuse
		return &model.Recovery{Reason: "ambiguous_authority"}
	}

	var disposable []string
	diskFullDisposable := false
	for _, a := range assessments {
		if a.Classification != model.ClassDisposable {
			continue
		}
		disposable = append(disposable, a.Pod)
		for _, n := range a.Notes {
			if strings.Contains(n, "disk full") {
				diskFullDisposable = true
			}
		}
	}
	if len(disposable) == 0 {
		return nil // nothing disposable → no recovery action to describe
	}

	rec := &model.Recovery{Authority: primaryName, Disposable: disposable}

	// RecoverySet = what must be escrow-reversible before any clear. If the authority
	// is down, it must be reversible too (it has to boot after we free the replicas).
	set := make([]string, 0, len(disposable)+1)
	if !primaryRunning {
		set = append(set, primaryName)
	}
	set = append(set, disposable...)
	rec.RecoverySet = set

	frozen := strings.Contains(strings.ToLower(clusterPhase), "not enough disk space")
	if frozen && diskFullDisposable && !primaryRunning {
		rec.Blocked = true
		rec.Reason = "disk_full_disposable_replica"
	}
	return rec
}

// cnpgOrdinal extracts the trailing instance ordinal from a CNPG pod name.
func cnpgOrdinal(pod string) string {
	if pod == "" {
		return "none"
	}
	parts := strings.Split(pod, "-")
	return parts[len(parts)-1]
}
