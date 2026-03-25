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
	Register("galera", func(ep provider.EngineProvider) (Triager, error) {
		p, ok := ep.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("galera triager requires *provider.GaleraProvider, got %T", ep)
		}
		return &galeraTriage{p: p}, nil
	})
}

// galeraTriage implements Triager for Galera (MariaDB) clusters.
type galeraTriage struct {
	p    *provider.GaleraProvider
	data *galeraTriageData
}

func (t *galeraTriage) Name() string { return "galera" }

// --- Types ---

// grastate holds parsed grastate.dat fields for one instance.
type grastate struct {
	Pod             string
	Source          string // "exec", "exec_crashloop", "pvc_probe", "none"
	Reachable       bool
	UUID            string
	Seqno           string
	SafeToBootstrap string
}

// wsrepStatus holds parsed wsrep GLOBAL_STATUS variables for one instance.
type wsrepStatus struct {
	LocalState        int
	LocalStateComment string
	ClusterStatus     string
	ClusterSize       string
	Connected         string
	Ready             string
	ClusterStateUUID  string
	LastCommitted     int64
	FlowControlPaused string
}

// effectiveSeqno holds the best seqno from multiple sources for one instance.
type effectiveSeqno struct {
	Value  int64
	Source string
}

// galeraTriageData holds all data collected during the triage collection phase.
type galeraTriageData struct {
	expectedNodes   []string
	runningPods     []corev1.Pod
	nonRunningPods  []corev1.Pod
	missingNodes    []string
	crashloopPods   []corev1.Pod
	grastateData    []grastate
	wsrepMap        map[string]*wsrepStatus
	effectiveSeqnos map[string]*effectiveSeqno
	diskUsage       map[string]int
	pvcStates       map[string]map[string]string // node -> {"storage": "Bound", "galera": "Bound"}
	crashReasons    map[string]string
	allNodesDown    bool
	anyNodeReady    bool
	primaryMembers  []string
	bestSeqnoNode   string
	bestSeqnoValue  int64
}

// galeraProbeTarget identifies a node whose PVC should be probed.
type galeraProbeTarget struct {
	Name string
	Node string
}

// --- Collect ---

func (t *galeraTriage) Collect(ctx context.Context) error {
	t.displayClusterStatus()

	data, err := t.triageCollect(ctx)
	if err != nil {
		return fmt.Errorf("triage collect failed: %w", err)
	}
	t.data = data
	return nil
}

func (t *galeraTriage) triageCollect(ctx context.Context) (*galeraTriageData, error) {
	c := k8s.GetClients()
	ns := t.p.Config().Namespace
	data := &galeraTriageData{
		wsrepMap:        make(map[string]*wsrepStatus),
		effectiveSeqnos: make(map[string]*effectiveSeqno),
		diskUsage:       make(map[string]int),
		pvcStates:       make(map[string]map[string]string),
		crashReasons:    make(map[string]string),
		bestSeqnoValue:  -2,
	}

	// Build expected node list
	for i := int64(0); i < t.p.Replicas(); i++ {
		data.expectedNodes = append(data.expectedNodes, fmt.Sprintf("%s-%d", t.p.Config().ClusterName, i))
	}

	// Get all MariaDB pods
	podList, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s", t.p.Config().ClusterName),
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

	for _, name := range data.expectedNodes {
		if !foundPodNames[name] {
			data.missingNodes = append(data.missingNodes, name)
		}
	}

	// Identify crashloop pods
	for _, pod := range data.runningPods {
		if len(pod.Status.ContainerStatuses) > 0 && !pod.Status.ContainerStatuses[0].Ready {
			data.crashloopPods = append(data.crashloopPods, pod)
		}
	}

	// Pod overview
	output.Section("Pod Overview")
	output.Field("Expected nodes", strings.Join(data.expectedNodes, ", "))
	output.Field("Running", joinPodNames(data.runningPods))
	output.Field("Non-running", joinPodNames(data.nonRunningPods))
	output.Field("Missing (no pod)", joinOrNone(data.missingNodes))
	output.Field("Crash-looping", joinPodNames(data.crashloopPods))

	displayNonRunning(data)

	// Check PVCs (storage and galera)
	for _, name := range data.expectedNodes {
		data.pvcStates[name] = map[string]string{"storage": "MISSING", "galera": "MISSING"}
		if _, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "storage-"+name, metav1.GetOptions{}); err == nil {
			data.pvcStates[name]["storage"] = "Bound"
		}
		if _, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "galera-"+name, metav1.GetOptions{}); err == nil {
			data.pvcStates[name]["galera"] = "Bound"
		}
	}

	// Display PVC state
	for name, state := range data.pvcStates {
		output.Printf("%s: storage=%s galera=%s\n", name, state["storage"], state["galera"])
	}

	// Fail if storage PVCs missing
	var missingStorage []string
	for name, state := range data.pvcStates {
		if state["storage"] == "MISSING" {
			missingStorage = append(missingStorage, name)
		}
	}
	if len(missingStorage) > 0 {
		return nil, fmt.Errorf("ABORTING: Missing storage PVCs: %s. Resolve before proceeding",
			strings.Join(missingStorage, ", "))
	}

	// --- grastate.dat reads ---
	crashloopNames := podNameSet(data.crashloopPods)
	var allGrastate []grastate
	haveData := make(map[string]bool)

	output.Section("Grastate Analysis")

	// Healthy running pods
	for _, pod := range data.runningPods {
		if crashloopNames[pod.Name] {
			continue
		}
		result, err := k8s.ExecCommand(ctx, pod.Name, ns, "mariadb",
			[]string{"cat", "/var/lib/mysql/grastate.dat"})
		if err != nil {
			common.DebugLog("grastate read failed on %s: %v", pod.Name, err)
			continue
		}
		gs := parseGrastate(pod.Name, "exec", result.Stdout)
		gs.Reachable = true
		allGrastate = append(allGrastate, gs)
		haveData[pod.Name] = true
	}

	// Crashloop pods
	for _, pod := range data.crashloopPods {
		result, err := k8s.ExecCommand(ctx, pod.Name, ns, "mariadb",
			[]string{"cat", "/var/lib/mysql/grastate.dat"})
		if err != nil {
			continue
		}
		gs := parseGrastate(pod.Name, "exec_crashloop", result.Stdout)
		allGrastate = append(allGrastate, gs)
		haveData[pod.Name] = true
	}

	// PVC probes for stranded instances
	podNodes := make(map[string]string)
	for _, pod := range podList.Items {
		podNodes[pod.Name] = pod.Spec.NodeName
	}

	var probeNodes []galeraProbeTarget
	for _, name := range data.expectedNodes {
		if !haveData[name] && data.pvcStates[name]["storage"] == "Bound" {
			probeNodes = append(probeNodes, galeraProbeTarget{Name: name, Node: podNodes[name]})
		}
	}

	if len(probeNodes) > 0 {
		common.InfoLog("Probing PVC data for stranded nodes: %s",
			joinGaleraProbeNames(probeNodes))
		sa := k8s.ServiceAccountFromPods(data.runningPods)
		probeResults := t.runPVCProbes(ctx, probeNodes, ns, sa)
		for name, gs := range probeResults {
			allGrastate = append(allGrastate, gs)
			haveData[name] = true
		}
	}

	// Fill in missing instances with no data
	for _, name := range data.expectedNodes {
		if !haveData[name] {
			allGrastate = append(allGrastate, grastate{
				Pod: name, Source: "none", UUID: "unknown", Seqno: "-1", SafeToBootstrap: "0",
			})
		}
	}
	data.grastateData = allGrastate

	// Display grastate
	for _, gs := range data.grastateData {
		displayGrastate(gs)
	}

	// --- wsrep status ---
	output.Section("Wsrep Status")
	for _, pod := range data.runningPods {
		if crashloopNames[pod.Name] {
			continue
		}
		// Use MYSQL_PWD env var to avoid password in process args
		result, err := k8s.ExecCommandWithEnv(ctx, pod.Name, ns, "mariadb",
			map[string]string{"MYSQL_PWD": t.p.RootPassword()},
			[]string{"mariadb", "-u", "root", "--batch", "--skip-column-names", "-e",
				"SELECT VARIABLE_NAME, VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS " +
					"WHERE VARIABLE_NAME IN (" +
					"'wsrep_local_state', 'wsrep_local_state_comment', " +
					"'wsrep_cluster_status', 'wsrep_cluster_size', " +
					"'wsrep_connected', 'wsrep_ready', " +
					"'wsrep_local_recv_queue', 'wsrep_local_send_queue', " +
					"'wsrep_cluster_state_uuid', " +
					"'wsrep_last_committed', " +
					"'wsrep_flow_control_paused'" +
					") ORDER BY VARIABLE_NAME"})
		if err != nil {
			common.WarnLog("wsrep query failed on %s: %v — wsrep data for this node will be incomplete", pod.Name, err)
			data.wsrepMap[pod.Name] = &wsrepStatus{}
			continue
		}
		ws := parseWsrepStatus(result.Stdout)
		data.wsrepMap[pod.Name] = ws
		displayWsrep(pod.Name, ws)
	}

	if len(data.wsrepMap) == 0 {
		output.Warn("No running+ready pods to query wsrep status from")
	}

	// --- Crash reasons ---
	for _, pod := range data.crashloopPods {
		logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "mariadb"})
		logBytes, err := logReq.DoRaw(ctx)
		if err != nil {
			continue
		}
		logText := string(logBytes)
		if strings.Contains(logText, "No space left on device") ||
			strings.Contains(logText, "Disk is full") ||
			strings.Contains(logText, "disk full") {
			data.crashReasons[pod.Name] = "disk_full"
		}
	}

	// --- Effective seqno ---
	output.Section("Effective Seqno (data freshness)")
	crRecovered := getRecoveryMap(t.p.GaleraRecovery(), "recovered")
	crState := getRecoveryMap(t.p.GaleraRecovery(), "state")

	for _, gs := range data.grastateData {
		ws := data.wsrepMap[gs.Pod]
		wsCommitted := int64(-1)
		if ws != nil {
			wsCommitted = ws.LastCommitted
		}
		crRecSeqno := getRecoverySeqno(crRecovered, gs.Pod)
		crStateSeqno := getRecoverySeqno(crState, gs.Pod)
		grastateSeqno := parseInt64(gs.Seqno, -1)

		candidates := []struct {
			val    int64
			source string
		}{
			{wsCommitted, "wsrep_last_committed"},
			{crRecSeqno, "cr_recovered"},
			{crStateSeqno, "cr_state"},
			{grastateSeqno, "grastate"},
		}

		best := int64(-1)
		bestSource := "none"
		for _, c := range candidates {
			if c.val > best {
				best = c.val
				bestSource = c.source
			}
		}

		data.effectiveSeqnos[gs.Pod] = &effectiveSeqno{Value: best, Source: bestSource}
		output.Printf("%s: effective_seqno=%d (source=%s, wsrep_last_committed=%d, cr_recovered=%d, grastate=%d)\n",
			gs.Pod, best, bestSource, wsCommitted, crRecSeqno, grastateSeqno)

		if best > data.bestSeqnoValue {
			data.bestSeqnoValue = best
			data.bestSeqnoNode = gs.Pod
		}
	}

	// --- Disk space ---
	output.Section("Disk Space")
	for _, pod := range data.runningPods {
		result, err := k8s.ExecCommand(ctx, pod.Name, ns, "mariadb",
			[]string{"df", "-h", "/var/lib/mysql"})
		if err != nil {
			output.Printf("%s: unable to check\n", pod.Name)
			continue
		}
		output.Printf("%s:\n%s\n", pod.Name, result.Stdout)
		data.diskUsage[pod.Name] = parseDiskPercent(result.Stdout)
	}

	// --- Cluster state ---
	data.allNodesDown = len(data.runningPods) == 0
	healthyRunning := 0
	for _, pod := range data.runningPods {
		if !crashloopNames[pod.Name] {
			healthyRunning++
		}
	}
	data.anyNodeReady = healthyRunning > 0

	// Primary members
	for name, ws := range data.wsrepMap {
		if ws.ClusterStatus == "Primary" {
			data.primaryMembers = append(data.primaryMembers, name)
		}
	}

	return data, nil
}

func (t *galeraTriage) runPVCProbes(ctx context.Context, targets []galeraProbeTarget, ns, sa string) map[string]grastate {
	c := k8s.GetClients()
	results := make(map[string]grastate)
	uid := int64(0)

	for _, tgt := range targets {
		probeName := tgt.Name + "-triage-probe"
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: probeName, Namespace: ns,
				Labels: map[string]string{"hasteward-triage": "probe"},
			},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: sa,
				SecurityContext:    &corev1.PodSecurityContext{RunAsUser: &uid},
				Containers: []corev1.Container{{
					Name: "probe", Image: "docker.io/library/busybox:latest",
					Command: []string{"cat", "/var/lib/mysql/grastate.dat"},
					VolumeMounts: []corev1.VolumeMount{{
						Name: "storage", MountPath: "/var/lib/mysql", ReadOnly: true,
					}},
				}},
				Volumes: []corev1.Volume{{
					Name: "storage",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "storage-" + tgt.Name,
						},
					},
				}},
			},
		}
		if tgt.Node != "" {
			pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": tgt.Node}
		}

		if _, err := c.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			common.WarnLog("Failed to create probe pod for %s: %v", tgt.Name, err)
			continue
		}

		// Wait for completion
		for attempt := 0; attempt < 30; attempt++ {
			p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, probeName, metav1.GetOptions{})
			if err != nil {
				break
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				break
			}
			time.Sleep(5 * time.Second)
		}

		// Get logs
		logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(probeName, &corev1.PodLogOptions{Container: "probe"})
		logBytes, err := logReq.DoRaw(ctx)
		if err == nil && len(logBytes) > 0 {
			results[tgt.Name] = parseGrastate(tgt.Name, "pvc_probe", string(logBytes))
		}

		// Cleanup
		_ = c.Clientset.CoreV1().Pods(ns).Delete(ctx, probeName, metav1.DeleteOptions{
			GracePeriodSeconds: ptr(int64(0)),
		})
	}
	return results
}

// --- Analyze ---

func (t *galeraTriage) Analyze(_ context.Context) (*model.TriageResult, error) {
	data := t.data
	if data == nil {
		return nil, fmt.Errorf("Analyze called before Collect")
	}

	comparison := t.crossInstanceComparison(data)

	output.Section("Data Freshness Check")
	for _, w := range comparison.Warnings {
		output.Println(w)
	}
	if !comparison.SafeToHeal {
		output.Println()
		output.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		output.Println("  CRITICAL: POTENTIAL SPLIT-BRAIN DETECTED")
		output.Println("  A non-primary node has MORE RECENT data than the primary component!")
		output.Printf("  Most advanced node: %s (seqno: %d)\n", comparison.MostAdvanced, comparison.MostAdvancedValue)
		output.Println("  DO NOT blindly heal - review the data above and decide manually.")
		output.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		output.Println()
	}

	assessments := t.buildAssessments(data, &comparison)

	var bestSeqnoAssessment *model.InstanceAssessment
	for i := range assessments {
		if assessments[i].Pod == data.bestSeqnoNode {
			bestSeqnoAssessment = &assessments[i]
			break
		}
	}

	// Compute authority status for donor recommendation using Galera facts only.
	// IsReady (K8s readiness) is deliberately NOT used here — authority is about
	// history consistency, not operator/probe readiness.
	authorityStatus := "ambiguous"
	recommendedDonor := "none"
	if comparison.SafeToHeal {
		authorityStatus = "unambiguous"
		for _, a := range assessments {
			if a.WsrepReady == "ON" && a.WsrepConnected == "ON" && a.WsrepStateComment == "Synced" {
				recommendedDonor = fmt.Sprintf("%d", a.Instance)
				break
			}
		}
	}

	result := &model.TriageResult{
		Engine: t.Name(),
		Cluster: model.ObjectRef{
			Namespace: t.p.Config().Namespace,
			Name:      t.p.Config().ClusterName,
		},
		Assessments:      assessments,
		DataComparison:   comparison,
		ReadyCount:       len(data.primaryMembers),
		TotalCount:       int(t.p.Replicas()),
		AllNodesDown:     data.allNodesDown,
		BestSeqnoNode:    bestSeqnoAssessment,
		AuthorityStatus:  authorityStatus,
		RecommendedDonor: recommendedDonor,
	}

	t.triageDisplay(data, result)

	return result, nil
}

func (t *galeraTriage) crossInstanceComparison(data *galeraTriageData) model.DataComparison {
	var warnings, splitBrain []string

	// UUID divergence check
	uuidSet := make(map[string]bool)
	for _, gs := range data.grastateData {
		if gs.UUID != "unknown" && gs.UUID != "00000000-0000-0000-0000-000000000000" {
			uuidSet[gs.UUID] = true
		}
	}
	for _, ws := range data.wsrepMap {
		if ws.ClusterStateUUID != "" {
			uuidSet[ws.ClusterStateUUID] = true
		}
	}
	if len(uuidSet) > 1 {
		uuids := make([]string, 0, len(uuidSet))
		for u := range uuidSet {
			uuids = append(uuids, u)
		}
		splitBrain = append(splitBrain, "Multiple cluster UUIDs detected: "+strings.Join(uuids, ", "))
	}

	// Non-primary component check
	for name, ws := range data.wsrepMap {
		if ws.ClusterStatus != "" && ws.ClusterStatus != "Primary" {
			splitBrain = append(splitBrain, name+" cluster_status="+ws.ClusterStatus)
		}
	}

	// Seqno comparison
	bestPrimarySeqno := int64(-2)
	bestPrimaryPod := ""
	for _, pm := range data.primaryMembers {
		if es, ok := data.effectiveSeqnos[pm]; ok && es.Value > bestPrimarySeqno {
			bestPrimarySeqno = es.Value
			bestPrimaryPod = pm
		}
	}

	// Only check for split-brain if we have primary members
	if len(data.primaryMembers) > 0 {
		pmSet := setFromSlice(data.primaryMembers)
		for name, es := range data.effectiveSeqnos {
			if !pmSet[name] && es.Value > bestPrimarySeqno && es.Value > 0 {
				splitBrain = append(splitBrain,
					fmt.Sprintf("%s has seqno %d > primary best %d (%s)",
						name, es.Value, bestPrimarySeqno, bestPrimaryPod))
			}
		}
	}

	safe := len(splitBrain) == 0
	if safe {
		if len(data.primaryMembers) > 0 {
			warnings = append(warnings,
				fmt.Sprintf("OK: Primary component (%s) has the most recent data (best seqno: %d)",
					strings.Join(data.primaryMembers, ", "), bestPrimarySeqno))
		} else {
			warnings = append(warnings,
				fmt.Sprintf("WARNING: No nodes in Primary component. Most advanced: %s (seqno: %d)",
					data.bestSeqnoNode, data.bestSeqnoValue))
		}
	} else {
		for _, sb := range splitBrain {
			warnings = append(warnings, "SPLIT-BRAIN RISK: "+sb)
		}
	}

	return model.DataComparison{
		MostAdvanced:      data.bestSeqnoNode,
		MostAdvancedValue: data.bestSeqnoValue,
		SafeToHeal:        safe,
		Warnings:          warnings,
		SplitBrainDetails: splitBrain,
		PrimaryMembers:    data.primaryMembers,
		BestPrimarySeqno:  bestPrimarySeqno,
	}
}

func (t *galeraTriage) buildAssessments(data *galeraTriageData, comparison *model.DataComparison) []model.InstanceAssessment {
	missingSet := setFromSlice(data.missingNodes)
	crashloopSet := podNameSet(data.crashloopPods)
	runningSet := podNameSet(data.runningPods)
	pmSet := setFromSlice(data.primaryMembers)
	bestPrimarySeqno := comparison.BestPrimarySeqno

	var assessments []model.InstanceAssessment

	for _, gs := range data.grastateData {
		isMissing := missingSet[gs.Pod]
		isCrashloop := crashloopSet[gs.Pod]
		isRunning := runningSet[gs.Pod] && !isCrashloop
		isInPrimary := pmSet[gs.Pod]
		hasData := gs.Source != "none"

		ws := data.wsrepMap[gs.Pod]
		wsState := 0
		wsStateComment := "unknown"
		wsConnected := "OFF"
		wsReady := "OFF"
		wsClusterStatus := "unknown"
		if ws != nil {
			wsState = ws.LocalState
			wsStateComment = ws.LocalStateComment
			wsConnected = ws.Connected
			wsReady = ws.Ready
			wsClusterStatus = ws.ClusterStatus
		}

		es := data.effectiveSeqnos[gs.Pod]
		nodeSeqno := int64(-1)
		seqnoSource := "none"
		if es != nil {
			nodeSeqno = es.Value
			seqnoSource = es.Source
		}
		seqnoLag := int64(-1)
		if bestPrimarySeqno > 0 && nodeSeqno > 0 {
			seqnoLag = bestPrimarySeqno - nodeSeqno
		}
		diskPct := data.diskUsage[gs.Pod]
		diskFull := data.crashReasons[gs.Pod] == "disk_full"
		dataCurrent := nodeSeqno > 0 && bestPrimarySeqno > 0 && seqnoLag <= 0

		parts := strings.Split(gs.Pod, "-")
		nodeNum := parts[len(parts)-1]
		healCmd := fmt.Sprintf("hasteward repair -e galera -c %s -n %s --instance %s --backups-path /backups",
			t.p.Config().ClusterName, t.p.Config().Namespace, nodeNum)

		var notes []string
		var recommendation string
		needsHeal := false

		switch {
		case !comparison.SafeToHeal:
			if !isInPrimary && nodeSeqno > bestPrimarySeqno && nodeSeqno > 0 {
				notes = append(notes, fmt.Sprintf("AHEAD OF PRIMARY COMPONENT (seqno %d > %d)", nodeSeqno, bestPrimarySeqno))
				recommendation = "MANUAL REVIEW REQUIRED. This node has data ahead of the primary component. Do NOT heal without understanding the data state."
			} else if !hasData {
				notes = append(notes, "NO DATA - cannot assess during split-brain")
				recommendation = "MANUAL REVIEW REQUIRED. Cannot determine this node state. Resolve split-brain first."
			} else {
				notes = append(notes, "split-brain detected in cluster")
				recommendation = fmt.Sprintf("MANUAL REVIEW REQUIRED. Split-brain detected. Resolve before healing.\n\n  %s --force", healCmd)
			}

		case isRunning && wsState == 4 && wsConnected == "ON" && wsReady == "ON" && wsClusterStatus == "Primary":
			if diskFull || diskPct >= 90 {
				notes = append(notes, fmt.Sprintf("healthy but disk low (%d%%)", diskPct))
				recommendation = "Synced and healthy but disk usage is high. Consider expanding PVC storage."
			} else {
				notes = append(notes, "healthy (Synced, connected, ready)")
				if seqnoLag > 0 {
					notes = append(notes, fmt.Sprintf("seqno lag: %d behind best", seqnoLag))
				}
				recommendation = "No action needed."
			}

		case isRunning && wsState >= 1 && wsState <= 3 && wsConnected == "ON":
			notes = append(notes, fmt.Sprintf("transitional (%s) - catching up", wsStateComment))
			if seqnoLag > 0 {
				notes = append(notes, fmt.Sprintf("seqno lag: %d", seqnoLag))
			}
			recommendation = fmt.Sprintf("Node is in transitional state (%s). Wait for Synced (state 4). If stuck, may need heal.\n\n  %s", wsStateComment, healCmd)

		case isRunning && (wsConnected == "OFF" || wsReady == "OFF"):
			needsHeal = true
			notes = append(notes, fmt.Sprintf("disconnected (connected=%s, ready=%s)", wsConnected, wsReady))
			if diskFull {
				notes = append(notes, "disk full (possible cause of disconnect)")
			}
			if nodeSeqno > 0 {
				notes = append(notes, fmt.Sprintf("last known seqno: %d", nodeSeqno))
			}
			recommendation = fmt.Sprintf("Node is disconnected from cluster. Needs heal (grastate wipe + SST rejoin).\n\n  %s", healCmd)

		case isRunning && wsClusterStatus != "Primary" && wsClusterStatus != "unknown":
			needsHeal = true
			notes = append(notes, fmt.Sprintf("non-primary component (%s)", wsClusterStatus))
			recommendation = fmt.Sprintf("Node is in non-primary component. Needs heal.\n\n  %s", healCmd)

		case isRunning && ws == nil:
			needsHeal = true
			notes = append(notes, "running but wsrep query failed")
			if nodeSeqno > 0 {
				notes = append(notes, fmt.Sprintf("last known seqno: %d", nodeSeqno))
			}
			recommendation = fmt.Sprintf("Could not query wsrep status. MariaDB may not be accepting connections. Needs heal.\n\n  %s", healCmd)

		case isCrashloop:
			notes = append(notes, "crash-looping")
			switch {
			case diskFull:
				needsHeal = true
				notes = append(notes, "disk full (cause of crash)")
				recommendation = fmt.Sprintf("Crash-looping due to disk full. Needs heal or PVC expansion.\n\n  %s", healCmd)
			case dataCurrent:
				notes = append(notes, fmt.Sprintf("data current (seqno: %d)", nodeSeqno))
				recommendation = fmt.Sprintf("Data is current but pod is crash-looping. Check pod logs for root cause. May recover on restart. Otherwise needs heal.\n\n  %s", healCmd)
			default:
				needsHeal = true
				if nodeSeqno > 0 {
					notes = append(notes, fmt.Sprintf("last known seqno: %d", nodeSeqno))
				}
				recommendation = fmt.Sprintf("Pod is crash-looping with stale data. Needs heal.\n\n  %s", healCmd)
			}

		case isMissing && hasData:
			notes = append(notes, "no pod running")
			if dataCurrent {
				notes = append(notes, fmt.Sprintf("data current (seqno: %d)", nodeSeqno))
				recommendation = "Data is current. MariaDB operator should recreate the pod. If stuck, check MariaDB CR status."
			} else {
				needsHeal = true
				if nodeSeqno > 0 {
					notes = append(notes, fmt.Sprintf("last known seqno: %d", nodeSeqno))
				}
				recommendation = fmt.Sprintf("Pod missing with stale data. MariaDB operator should recreate. If stuck, needs heal.\n\n  %s", healCmd)
			}

		case isMissing:
			notes = append(notes, "NO DATA - no pod, could not probe PVC")
			recommendation = "Could not determine state. Check if PVC can be mounted."

		default:
			notes = append(notes, "unknown state")
			recommendation = "Could not determine state. Check node manually."
		}

		assessments = append(assessments, model.InstanceAssessment{
			Pod:                gs.Pod,
			IsRunning:          isRunning,
			IsReady:            isRunning && wsState == 4,
			NeedsHeal:          needsHeal,
			Notes:              notes,
			Recommendation:     recommendation,
			IsInPrimary:        isInPrimary,
			Seqno:              parseInt64(gs.Seqno, -1),
			EffectiveSeqno:     nodeSeqno,
			SeqnoSource:        seqnoSource,
			SeqnoLag:           seqnoLag,
			UUID:               gs.UUID,
			SafeToBootstrap:    gs.SafeToBootstrap,
			WsrepState:         wsState,
			WsrepStateComment:  wsStateComment,
			WsrepConnected:     wsConnected,
			WsrepReady:         wsReady,
			WsrepClusterStatus: wsClusterStatus,
			CrashReason:        data.crashReasons[gs.Pod],
			DiskPct:            diskPct,
		})
	}

	return assessments
}

// --- Display ---

func (t *galeraTriage) displayClusterStatus() {
	output.Section("MariaDB Status")
	output.Field("Ready", getConditionStatus(t.p.ReadyCondition()))
	output.Field("GaleraReady", getConditionStatus(t.p.GaleraCondition()))
	output.Field("Replicas", fmt.Sprintf("%d", t.p.Replicas()))
	output.Field("Image", getMapString(t.p.Spec(), "image"))
	output.Field("Suspended", fmt.Sprintf("%v", t.p.IsSuspended()))
	if t.p.GaleraRecovery() != nil && len(t.p.GaleraRecovery()) > 0 {
		output.Field("Galera recovery", fmt.Sprintf("%v", t.p.GaleraRecovery()))
	} else {
		output.Field("Galera recovery", "none")
	}
}

func displayNonRunning(data *galeraTriageData) {
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
}

func displayGrastate(gs grastate) {
	srcLabel := ""
	switch gs.Source {
	case "pvc_probe":
		srcLabel = " (from PVC probe - pod not running)"
	case "exec_crashloop":
		srcLabel = " (from crashloop pod)"
	case "none":
		srcLabel = " (NO DATA - could not probe)"
	}
	output.Printf("%s%s\n", gs.Pod, srcLabel)
	output.Printf("  UUID: %s\n", gs.UUID)
	output.Printf("  Seqno: %s\n", gs.Seqno)
	output.Printf("  Safe to bootstrap: %s\n", gs.SafeToBootstrap)
}

func displayWsrep(name string, ws *wsrepStatus) {
	output.Printf("%s:\n", name)
	output.Printf("  local_state: %d (%s)\n", ws.LocalState, ws.LocalStateComment)
	output.Printf("  cluster_status: %s\n", ws.ClusterStatus)
	output.Printf("  cluster_size: %s\n", ws.ClusterSize)
	output.Printf("  connected: %s\n", ws.Connected)
	output.Printf("  ready: %s\n", ws.Ready)
	output.Printf("  cluster_uuid: %s\n", ws.ClusterStateUUID)
	output.Printf("  last_committed: %d\n", ws.LastCommitted)
	output.Printf("  flow_control_paused: %s\n", ws.FlowControlPaused)
}

func (t *galeraTriage) triageDisplay(data *galeraTriageData, result *model.TriageResult) {
	output.Banner("TRIAGE SUMMARY")

	output.Printf("Cluster: %s (%s)\n", t.p.Config().ClusterName, t.p.Config().Namespace)
	output.Printf("Ready: %s | GaleraReady: %s\n",
		getConditionStatus(t.p.ReadyCondition()), getConditionStatus(t.p.GaleraCondition()))
	output.Printf("Replicas: %d\n", t.p.Replicas())
	output.Printf("Most advanced node: %s (seqno: %d)\n",
		result.DataComparison.MostAdvanced, result.DataComparison.MostAdvancedValue)
	output.Printf("Primary component: %s (best seqno: %d)\n",
		joinOrNone(result.DataComparison.PrimaryMembers), result.DataComparison.BestPrimarySeqno)
	if result.DataComparison.SafeToHeal {
		output.Println("Safe to heal nodes: YES - primary component has most recent data")
	} else {
		output.Println("Safe to heal nodes: NO - SPLIT-BRAIN DETECTED - review data above")
	}
	output.Printf("All nodes down: %v\n", data.allNodesDown)
	output.Printf("Authority status: %s\n", result.AuthorityStatus)
	if result.RecommendedDonor != "none" {
		output.Printf("Recommended donor: %s\n", result.RecommendedDonor)
	} else {
		output.Println("Recommended donor: none (authority ambiguous; operator must choose)")
	}
	output.Println()

	// Per-instance assessment
	for _, a := range result.Assessments {
		primaryTag := ""
		if a.IsInPrimary {
			primaryTag = " [PRIMARY COMPONENT]"
		}
		diskTag := ""
		if a.CrashReason == "disk_full" {
			diskTag = " [DISK FULL]"
		}
		output.Printf("%s%s%s: %s\n", a.Pod, primaryTag, diskTag, strings.Join(a.Notes, ", "))
		output.Printf("  Wsrep: state=%d (%s) connected=%s ready=%s cluster=%s\n",
			a.WsrepState, a.WsrepStateComment, a.WsrepConnected, a.WsrepReady, a.WsrepClusterStatus)
		lagStr := ""
		if a.SeqnoLag >= 0 {
			lagStr = fmt.Sprintf(" lag=%d", a.SeqnoLag)
		}
		output.Printf("  Seqno: effective=%d (source=%s)%s\n", a.EffectiveSeqno, a.SeqnoSource, lagStr)
		output.Printf("  Grastate: uuid=%s seqno=%d safe_to_bootstrap=%s\n", a.UUID, a.Seqno, a.SafeToBootstrap)
		diskStr := "N/A"
		if a.DiskPct >= 0 {
			diskStr = fmt.Sprintf("%d%%", a.DiskPct)
		}
		output.Printf("  Disk: %s\n", diskStr)
		output.Printf("  >> %s\n", a.Recommendation)
	}

	healCount := 0
	for _, a := range result.Assessments {
		if a.NeedsHeal {
			healCount++
		}
	}
	if healCount > 0 {
		output.SuggestedCommands("galera", t.p.Config().ClusterName, t.p.Config().Namespace)
	}

	if data.allNodesDown {
		output.Println()
		output.Section("Full Cluster Down")
		output.Printf("All nodes are down. Best bootstrap candidate: %s (seqno: %d)\n",
			data.bestSeqnoNode, data.bestSeqnoValue)
		output.Println("The mariadb-operator should handle recovery automatically via galera.recovery.")
		output.Println("If stuck, check the MariaDB CR status.galeraRecovery field.")
		output.Println()
		output.Println("To manually bootstrap the cluster:")
		output.Printf("  hasteward bootstrap -e galera -c %s -n %s --dry-run\n", t.p.Config().ClusterName, t.p.Config().Namespace)
		output.Printf("  hasteward bootstrap -e galera -c %s -n %s\n", t.p.Config().ClusterName, t.p.Config().Namespace)
	}
}

// --- Helpers ---

func parseGrastate(podName, source, raw string) grastate {
	gs := grastate{
		Pod: podName, Source: source,
		UUID: "unknown", Seqno: "-1", SafeToBootstrap: "0",
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "uuid:") {
			gs.UUID = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "seqno:") {
			gs.Seqno = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		} else if strings.HasPrefix(line, "safe_to_bootstrap:") {
			gs.SafeToBootstrap = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	return gs
}

func parseWsrepStatus(raw string) *wsrepStatus {
	ws := &wsrepStatus{LastCommitted: -1}
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(parts[0])
		val := parts[1]
		switch key {
		case "wsrep_local_state":
			ws.LocalState, _ = strconv.Atoi(val)
		case "wsrep_local_state_comment":
			ws.LocalStateComment = val
		case "wsrep_cluster_status":
			ws.ClusterStatus = val
		case "wsrep_cluster_size":
			ws.ClusterSize = val
		case "wsrep_connected":
			ws.Connected = val
		case "wsrep_ready":
			ws.Ready = val
		case "wsrep_cluster_state_uuid":
			ws.ClusterStateUUID = val
		case "wsrep_last_committed":
			ws.LastCommitted, _ = strconv.ParseInt(val, 10, 64)
		case "wsrep_flow_control_paused":
			ws.FlowControlPaused = val
		}
	}
	return ws
}

func parseDiskPercent(dfOutput string) int {
	lines := strings.Split(strings.TrimSpace(dfOutput), "\n")
	if len(lines) < 2 {
		return -1
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 5 {
		return -1
	}
	pctStr := strings.TrimSuffix(fields[4], "%")
	pct, err := strconv.Atoi(pctStr)
	if err != nil {
		return -1
	}
	return pct
}

func parseInt64(s string, fallback int64) int64 {
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func getConditionStatus(cond map[string]interface{}) string {
	if cond == nil {
		return "Unknown"
	}
	if v, ok := cond["status"]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "Unknown"
}

func getMapString(m map[string]interface{}, key string) string {
	if m == nil {
		return "Unknown"
	}
	if v, ok := m[key]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "Unknown"
}

func getRecoveryMap(recovery map[string]interface{}, key string) map[string]interface{} {
	if recovery == nil {
		return nil
	}
	if v, ok := recovery[key]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	return nil
}

func getRecoverySeqno(m map[string]interface{}, podName string) int64 {
	if m == nil {
		return -1
	}
	v, ok := m[podName]
	if !ok {
		return -1
	}
	switch val := v.(type) {
	case map[string]interface{}:
		if s, ok := val["seqno"]; ok {
			return parseInt64(fmt.Sprintf("%v", s), -1)
		}
	}
	return -1
}

func podNameSet(pods []corev1.Pod) map[string]bool {
	m := make(map[string]bool)
	for _, p := range pods {
		m[p.Name] = true
	}
	return m
}

func setFromSlice(s []string) map[string]bool {
	m := make(map[string]bool)
	for _, v := range s {
		m[v] = true
	}
	return m
}

func joinPodNames(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return "none"
	}
	names := make([]string, len(pods))
	for i, p := range pods {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "NONE"
	}
	return strings.Join(s, ", ")
}

func joinGaleraProbeNames(targets []galeraProbeTarget) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }
