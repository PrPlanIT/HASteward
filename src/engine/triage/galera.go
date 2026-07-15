package triage

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
// Known is true ONLY when Value came from a trustworthy, positive source. A
// node whose position could not be read (belly-up/wedged, all sources absent)
// has Known=false and MUST NOT be treated as seqno 0 — it is undeterminable,
// and authority cannot be declared while any node is unknown.
type effectiveSeqno struct {
	Value  int64
	Source string
	Known  bool
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
	logRecovered    map[string]int64                       // pod -> seqno scraped from mariadbd log (GCache estimate — HINT only)
	logRecUUID      map[string]string                      // pod -> cluster UUID scraped from mariadbd log
	wsrepRecovered  map[string]provider.WsrepRecoverResult // pod -> authoritative uuid:seqno from a fresh fenced --wsrep-recover
	effectiveSeqnos map[string]*effectiveSeqno
	diskUsage       map[string]int
	pvcStates       map[string]map[string]string // node -> {"storage": "Bound", "galera": "Bound"}
	crashReasons    map[string]string
	allNodesDown    bool
	anyNodeReady    bool
	primaryMembers  []string
	recoveryUUIDs   []string // non-zero cluster UUIDs seen in the operator's galeraRecovery
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
		logRecovered:    make(map[string]int64),
		logRecUUID:      make(map[string]string),
		wsrepRecovered:  make(map[string]provider.WsrepRecoverResult),
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
		LabelSelector: t.p.PodSelector(),
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

	// Identify crashloop pods — by the mariadb container, NOT container index 0
	// (which is the alphabetically-first `agent` sidecar; a crashlooping mariadb
	// behind a Ready agent would otherwise go unflagged).
	for _, pod := range data.runningPods {
		if !k8s.ContainerReadyByName(pod, "mariadb") {
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
		if _, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, t.p.DataPVCName(name), metav1.GetOptions{}); err == nil {
			data.pvcStates[name]["storage"] = "Bound"
		}
		if _, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, t.p.GaleraPVCName(name), metav1.GetOptions{}); err == nil {
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
		m, err := t.p.QueryWsrep(ctx, pod.Name)
		if err != nil {
			common.WarnLog("wsrep query failed on %s: %v — wsrep data for this node will be incomplete", pod.Name, err)
			// LastCommitted -1 = unknown. NEVER let a failed query masquerade as seqno 0.
			data.wsrepMap[pod.Name] = &wsrepStatus{LastCommitted: -1}
			continue
		}
		ws := parseWsrepStatus(m)
		data.wsrepMap[pod.Name] = ws
		displayWsrep(pod.Name, ws)
	}

	if len(data.wsrepMap) == 0 {
		output.Warn("No running+ready pods to query wsrep status from")
	}

	// --- Belly-up seqno recovery (running pod, but wsrep unreadable) ---
	// A pod can be Running yet have mysqld wedged (non-Primary, socket dead),
	// holding the datadir so --wsrep-recover cannot run and grastate is -1.
	// Its true position was logged at startup ("WSREP: Recovered position:")
	// or is bounded by the GCache ("found gapless sequence X-Y"). Scrape it —
	// read-only — so the node isn't silently treated as blind/zero.
	for _, pod := range data.runningPods {
		if crashloopNames[pod.Name] {
			continue
		}
		ws := data.wsrepMap[pod.Name]
		if ws != nil && ws.LastCommitted >= 0 {
			continue // live query worked; no scrape needed
		}
		if gsSeqno := grastateSeqnoFor(data.grastateData, pod.Name); gsSeqno >= 0 {
			continue // clean grastate already gives a position
		}
		seqno, uuid := t.scrapeRecoveredPosition(ctx, ns, pod.Name)
		if seqno >= 0 {
			data.logRecovered[pod.Name] = seqno
			if uuid != "" {
				data.logRecUUID[pod.Name] = uuid
			}
			common.InfoLog("Recovered %s position from mariadbd log: seqno=%d uuid=%s (belly-up fallback)",
				pod.Name, seqno, uuid)
		} else {
			common.WarnLog("%s is running-but-wedged and its position is UNREAD (log rotated); "+
				"authority will be marked ambiguous until it is fenced + wsrep-recovered", pod.Name)
		}
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

	// --- Belly-up escalation (Heimlich): evaluate authoritatively before deriving ---
	// If nothing is serving, read each node's TRUE position via a fenced
	// wsrep_recover now, so the derivation below and the split-brain check use
	// Known truth instead of hints. Non-destructive: it fences, reads, restores —
	// it never declares authority or touches safe_to_bootstrap, so it cannot
	// hinder recovery. Doing this is triage's job; leaving repair to guess is not.
	t.maybeDeepRecover(ctx, data)

	// --- Effective seqno ---
	output.Section("Effective Seqno (data freshness)")
	crRecovered := getRecoveryMap(t.p.GaleraRecovery(), "recovered")
	crState := getRecoveryMap(t.p.GaleraRecovery(), "state")
	for _, m := range []map[string]interface{}{crRecovered, crState} {
		data.recoveryUUIDs = append(data.recoveryUUIDs, recoveryUUIDsOf(m)...)
	}

	for _, gs := range data.grastateData {
		ws := data.wsrepMap[gs.Pod]
		wsCommitted := int64(-1)
		if ws != nil {
			wsCommitted = ws.LastCommitted
		}
		crRecSeqno := getRecoverySeqno(crRecovered, gs.Pod)
		crStateSeqno := getRecoverySeqno(crState, gs.Pod)
		grastateSeqno := parseInt64(gs.Seqno, -1)
		logSeqno := int64(-1)
		if v, ok := data.logRecovered[gs.Pod]; ok {
			logSeqno = v
		}
		recSeqno := int64(-1)
		if v, ok := data.wsrepRecovered[gs.Pod]; ok && v.Valid {
			recSeqno = v.Seqno
		}

		es := deriveEffectiveSeqno(wsCommitted, crRecSeqno, crStateSeqno, grastateSeqno, logSeqno, recSeqno)
		data.effectiveSeqnos[gs.Pod] = &es

		knownStr := "KNOWN"
		if !es.Known {
			knownStr = "UNKNOWN(no authoritative source — hint only)"
		}
		output.Printf("%s: effective_seqno=%d [%s] (src=%s | authoritative: recover=%d live=%d grastate=%d | hints: gcache=%d cr_rec=%d)\n",
			gs.Pod, es.Value, knownStr, es.Source, recSeqno, wsCommitted, grastateSeqno, logSeqno, crRecSeqno)

		// Only a KNOWN (authoritative) position may set the most-advanced node.
		// A node whose only data is the operator snapshot or a GCache estimate
		// must never become the bootstrap target by default.
		if es.Known && es.Value > data.bestSeqnoValue {
			data.bestSeqnoValue = es.Value
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
	results := make(map[string]grastate)
	for _, tgt := range targets {
		// Read-only probe of grastate.dat via the shared helper-pod runner.
		logs, _, err := t.p.RunHelperPodSpec(ctx, provider.HelperPodSpec{
			Name:      tgt.Name + "-triage-probe",
			PVCName:   t.p.DataPVCName(tgt.Name),
			MountPath: "/var/lib/mysql",
			Command:   []string{"cat", "/var/lib/mysql/grastate.dat"},
			SA:        sa,
			ReadOnly:  true,
			NodeName:  tgt.Node,
			Label:     map[string]string{"hasteward-triage": "probe"},
		})
		if err != nil {
			common.WarnLog("PVC probe for %s failed: %v", tgt.Name, err)
			continue
		}
		if len(logs) > 0 {
			results[tgt.Name] = parseGrastate(tgt.Name, "pvc_probe", logs)
		}
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
		output.Println("  CRITICAL: NOT SAFE TO HEAL — authority is undeterminable")
		output.Println("  Triage could not prove a single, consistent most-advanced history.")
		output.Println("  Reason(s):")
		for _, sb := range comparison.SplitBrainDetails {
			output.Printf("    - %s\n", sb)
		}
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

	// UUID divergence check. Only AUTHORITATIVE cluster-identity evidence may
	// declare a split-brain; stale hints must never drive the verdict.
	uuidSet := make(map[string]bool)
	addUUID := func(u string) {
		if u != "" && u != "unknown" && u != provider.ZeroUUID {
			uuidSet[u] = true
		}
	}
	// Per node, take the SINGLE most-authoritative UUID and compare across nodes:
	//   fenced wsrep_recover  >  live wsrep_cluster_state_uuid  >  grastate.dat
	// A lower source is consulted only when the higher one is absent, so a stale
	// on-disk grastate can never contradict a fresh recover for the same node.
	nodes := make(map[string]bool)
	for _, gs := range data.grastateData {
		nodes[gs.Pod] = true
	}
	for pod := range data.wsrepMap {
		nodes[pod] = true
	}
	for pod := range data.wsrepRecovered {
		nodes[pod] = true
	}
	for pod := range nodes {
		if rr, ok := data.wsrepRecovered[pod]; ok && rr.Valid {
			addUUID(rr.UUID)
		} else if ws := data.wsrepMap[pod]; ws != nil && ws.ClusterStateUUID != "" {
			addUUID(ws.ClusterStateUUID)
		} else {
			addUUID(grastateUUIDFor(data.grastateData, pod))
		}
	}
	// HINT-only UUID sources — the operator's galeraRecovery snapshot and mariadbd-
	// log scrapes — go STALE. Observed on osticket while perfectly healthy: the
	// operator snapshot still carried the pre-bootstrap incarnation 57a2c75c… long
	// after the cluster reformed as b86ff01c…, and folding the stale hint in
	// manufactured a phantom split-brain. So a hint may never contradict an
	// authoritative reading; we consult hints ONLY when NO node yielded an
	// authoritative UUID at all (a fully belly-up cluster not yet fenced), where a
	// log scrape is the sole remaining way to see a wedged node on a divergent
	// history. (When blind, the UNREAD fail-closed check below already blocks any
	// heal — the hint only sharpens the reported reason.)
	if len(uuidSet) == 0 {
		for _, u := range data.logRecUUID {
			addUUID(u)
		}
		for _, u := range data.recoveryUUIDs {
			addUUID(u)
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
			if !pmSet[name] && es.Known && es.Value > bestPrimarySeqno && es.Value > 0 {
				splitBrain = append(splitBrain,
					fmt.Sprintf("%s has seqno %d > primary best %d (%s)",
						name, es.Value, bestPrimarySeqno, bestPrimaryPod))
			}
		}
	}

	// FAIL-CLOSED: any node whose position could not be determined makes the
	// authority undeterminable. We must NEVER pick a bootstrap target while
	// blind to a node — it may hold the most-advanced (or a divergent) history,
	// and bootstrapping past it silently discards committed transactions. This
	// is the safety net that turns "no evidence of a problem" into "prove it's
	// safe" — the default must be closed, not open.
	var unread []string
	for _, gs := range data.grastateData {
		if es, ok := data.effectiveSeqnos[gs.Pod]; !ok || !es.Known {
			unread = append(unread, gs.Pod)
		}
	}
	if len(unread) > 0 {
		splitBrain = append(splitBrain,
			"UNREAD SEQNO — authority undeterminable for: "+strings.Join(unread, ", ")+
				" (fence + --wsrep-recover required before ANY bootstrap)")
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
		if cs, ok := k8s.ContainerStatusByName(pod, "mariadb"); ok {
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

// deriveEffectiveSeqno picks a node's effective seqno from its candidate sources
// and marks whether it is authoritative (Known). Only a fresh --wsrep-recover, a
// live wsrep_last_committed, or a CLEAN grastate (>0) may establish Known and
// drive the authority/bootstrap decision. The operator's galeraRecovery snapshot
// (cr_recovered/cr_state) and the GCache log figure are HINTS only — display,
// never authoritative — because the operator's numbers are stale/unreliable
// (they read 3292/3289 when --wsrep-recover proved every node at 552481).
func deriveEffectiveSeqno(wsCommitted, crRec, crState, grastate, logSeqno, recSeqno int64) effectiveSeqno {
	type cand struct {
		val    int64
		source string
	}
	authoritative := []cand{
		{recSeqno, "wsrep_recover"},
		{wsCommitted, "wsrep_last_committed"},
		{grastate, "grastate"},
	}
	hints := []cand{
		{logSeqno, "log_gcache_estimate"},
		{crRec, "cr_recovered"},
		{crState, "cr_state"},
	}
	best := int64(-1)
	source := "none"
	known := false
	for _, c := range authoritative {
		if c.val > 0 && c.val > best {
			best, source, known = c.val, c.source, true
		}
	}
	if !known { // no authoritative read — a hint may fill in a DISPLAY value only
		for _, c := range hints {
			if c.val > 0 && c.val > best {
				best, source = c.val, c.source
			}
		}
	}
	return effectiveSeqno{Value: best, Source: source, Known: known}
}

// isAnythingAlive is the fail-SAFE gate for the fenced deep-recovery. It returns
// true if ANY node is serving or making recovery progress — in which case triage
// must NOT fence/touch the cluster (you don't defibrillate a conscious patient).
// Only when this is false — provably nothing live — may triage escalate to the
// fenced --wsrep-recover. Unsure ⇒ treat as alive (fail-safe): the function
// returns false only on positive evidence that no member is participating.
// maybeDeepRecover is triage's on-the-spot, non-destructive escalation for a
// PROVABLY belly-up cluster. When nothing is serving it fences the cluster,
// reads each node's authoritative uuid:seqno with wsrep_recover, then restores.
// It never declares authority, sets safe_to_bootstrap, or SSTs a node — so it
// cannot leave the datadir in a state that hinders recovery; those mutations stay
// reserved for repair/bootstrap. Gathering this truth is triage's job: it turns
// hint-only guesses into Known positions so repair acts informed.
//
// The gate is paranoid and fail-closed: escalate ONLY when isAnythingAlive is
// false AND a fresh authenticated SELECT 1 finds nothing serving on any pod. If
// anything answers — or we cannot authenticate to prove it dead — we stay
// hands-off.
func (t *galeraTriage) maybeDeepRecover(ctx context.Context, data *galeraTriageData) {
	if isAnythingAlive(data) {
		return // a Primary/Ready/participating node exists — never fence a live cluster
	}
	if len(data.grastateData) == 0 {
		return // no on-disk state to recover from
	}
	if t.anyServing(ctx, data) {
		output.Printf("Belly-up escalation SKIPPED: a fresh probe found a live server (or the root password is unavailable to prove otherwise) — staying hands-off.\n")
		return
	}
	t.deepRecover(ctx, data)
}

// anyServing freshly probes every running pod with an authenticated SELECT 1.
// It returns true if ANY mysqld answers — belt-and-suspenders over the collect-
// time snapshot so a transiently-missed live node is never fenced. If the root
// password is unavailable we cannot prove a node is dead, so we fail closed and
// report the cluster as serving (no fence).
func (t *galeraTriage) anyServing(ctx context.Context, data *galeraTriageData) bool {
	pw := t.p.RootPassword()
	if pw == "" {
		return true // cannot authenticate to disprove life → assume serving
	}
	ns := t.p.Config().Namespace
	for _, pod := range data.runningPods {
		res, err := k8s.ExecCommandWithEnv(ctx, pod.Name, ns, "mariadb",
			map[string]string{"MYSQL_PWD": pw},
			[]string{"mariadb", "-u", "root", "--batch", "--skip-column-names", "-e", "SELECT 1"})
		if err == nil && strings.TrimSpace(res.Stdout) == "1" {
			return true
		}
	}
	return false
}

// deepRecover fences the belly-up cluster, runs wsrep_recover on every node to
// read its authoritative position, then restores. Restoration (scale back up +
// resume the operator) is deferred so it runs on EVERY exit path. If pods refuse
// to terminate we abort before any recover — we must never wsrep_recover a
// datadir a live mysqld may still hold open (concurrent-open corruption).
func (t *galeraTriage) deepRecover(ctx context.Context, data *galeraTriageData) {
	cfg := t.p.Config()
	sa := k8s.ServiceAccountFromPods(data.runningPods)
	origReplicas := int32(t.p.Replicas())

	common.WarnLog("Belly-up cluster, nothing serving — escalating to a fenced wsrep_recover to read authoritative positions (triage evaluation only; the cluster is restored and no authority is declared).")
	output.Section("Belly-up Escalation (fenced wsrep_recover)")

	common.InfoLog("Suspending MariaDB CR")
	if err := t.p.SuspendCR(ctx); err != nil {
		common.WarnLog("deep-recover: suspend CR failed: %v — skipping escalation", err)
		return
	}
	defer func() {
		if err := t.p.ResumeCR(ctx); err != nil {
			common.WarnLog("deep-recover: resume CR failed: %v — operator may need a manual resume", err)
		}
	}()

	t.p.DeleteRecoveryPods(ctx)

	common.InfoLog("Scaling StatefulSet to 0")
	if err := t.p.ScaleStatefulSet(ctx, 0); err != nil {
		common.WarnLog("deep-recover: scale to 0 failed: %v — restoring", err)
		return
	}
	defer func() {
		common.InfoLog("Restoring StatefulSet to %d replicas", origReplicas)
		if err := t.p.ScaleStatefulSet(ctx, origReplicas); err != nil {
			common.WarnLog("deep-recover: restore scale failed: %v — SCALE MANUALLY", err)
		}
	}()

	if err := t.p.WaitPodsTerminated(ctx, cfg.DeleteTimeout); err != nil {
		common.WarnLog("deep-recover: %v — aborting recover (a live mysqld may still hold the datadir; refusing to wsrep_recover)", err)
		return
	}

	common.InfoLog("Running wsrep_recover on all nodes")
	for _, gs := range data.grastateData {
		rr, err := t.p.RunWsrepRecover(ctx, gs.Pod, sa)
		if err != nil {
			common.WarnLog("deep-recover: wsrep_recover %s failed: %v", gs.Pod, err)
			continue
		}
		data.wsrepRecovered[gs.Pod] = rr
		common.InfoLog("wsrep_recover %s: uuid=%s seqno=%d lastCommitted=%d", gs.Pod, rr.UUID, rr.Seqno, rr.LastCommitted)
		output.Printf("  %s: uuid=%s seqno=%d (authoritative)\n", gs.Pod, rr.UUID, rr.Seqno)
	}
	t.p.DeleteRecoveryPods(ctx)
}

func isAnythingAlive(data *galeraTriageData) bool {
	if len(data.primaryMembers) > 0 {
		return true // a Primary component exists — cluster is (at least partly) serving
	}
	// Any pod whose mariadb container is Ready is serving, even if our live query
	// happened to hiccup — do not fence it.
	for _, pod := range data.runningPods {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "mariadb" && cs.Ready {
				return true
			}
		}
	}
	// wsrep local_state >= 1 (Joining/Donor/Joined/Synced) or a Primary status =
	// participating/progressing.
	for _, ws := range data.wsrepMap {
		if ws != nil && (ws.ClusterStatus == "Primary" || ws.LocalState >= 1) {
			return true
		}
	}
	return false
}

// scrapeRecoveredPosition reads a running pod's mariadbd log (read-only) and
// extracts its WSREP position when live queries fail — the belly-up-but-running
// case where mysqld is wedged non-Primary, holding the datadir so --wsrep-recover
// cannot run and grastate is -1. Prefers the explicit startup line
// "WSREP: Recovered position: <uuid>:<seqno>"; falls back to the high end of
// "Recovering GCache ring buffer: found gapless sequence X-Y" as an upper bound.
// Returns (-1, "") when the log has rotated past both.
func (t *galeraTriage) scrapeRecoveredPosition(ctx context.Context, ns, pod string) (int64, string) {
	c := k8s.GetClients()
	logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: "mariadb"})
	logBytes, err := logReq.DoRaw(ctx)
	if err != nil || len(logBytes) == 0 {
		return -1, ""
	}
	seqno := int64(-1)
	uuid := ""
	for _, line := range strings.Split(string(logBytes), "\n") {
		if i := strings.Index(line, "Recovered position:"); i >= 0 {
			rest := strings.TrimSpace(line[i+len("Recovered position:"):])
			if colon := strings.LastIndex(rest, ":"); colon > 0 {
				if fields := strings.Fields(rest[colon+1:]); len(fields) > 0 {
					if s := parseInt64(fields[0], -1); s > seqno {
						seqno = s
						uuid = strings.TrimSpace(rest[:colon])
					}
				}
			}
		} else if i := strings.Index(line, "found gapless sequence"); i >= 0 {
			rest := strings.TrimSpace(line[i+len("found gapless sequence"):])
			if dash := strings.LastIndex(rest, "-"); dash > 0 {
				if fields := strings.Fields(rest[dash+1:]); len(fields) > 0 {
					if y := parseInt64(fields[0], -1); y > seqno {
						seqno = y // GCache upper bound — enough to un-blind & compare
					}
				}
			}
		}
	}
	return seqno, uuid
}

// grastateSeqnoFor returns the parsed grastate seqno for a pod, or -1 if
// absent/unclean.
func grastateSeqnoFor(all []grastate, pod string) int64 {
	for _, gs := range all {
		if gs.Pod == pod {
			return parseInt64(gs.Seqno, -1)
		}
	}
	return -1
}

func grastateUUIDFor(all []grastate, pod string) string {
	for _, gs := range all {
		if gs.Pod == pod {
			return gs.UUID
		}
	}
	return ""
}

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

// parseWsrepStatus maps a wsrep_* name->value map (from provider.QueryWsrep) into
// a wsrepStatus. LastCommitted defaults to -1 (unknown) when absent.
func parseWsrepStatus(m map[string]string) *wsrepStatus {
	ws := &wsrepStatus{LastCommitted: -1}
	if v, ok := m["wsrep_local_state"]; ok {
		ws.LocalState, _ = strconv.Atoi(v)
	}
	ws.LocalStateComment = m["wsrep_local_state_comment"]
	ws.ClusterStatus = m["wsrep_cluster_status"]
	ws.ClusterSize = m["wsrep_cluster_size"]
	ws.Connected = m["wsrep_connected"]
	ws.Ready = m["wsrep_ready"]
	ws.ClusterStateUUID = m["wsrep_cluster_state_uuid"]
	if v, ok := m["wsrep_last_committed"]; ok {
		ws.LastCommitted, _ = strconv.ParseInt(v, 10, 64)
	}
	ws.FlowControlPaused = m["wsrep_flow_control_paused"]
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

// recoveryUUIDsOf extracts the distinct non-zero cluster UUIDs from a galera
// recovery map ({pod: {uuid, seqno}}). Zero/empty UUIDs are placeholders.
func recoveryUUIDsOf(m map[string]interface{}) []string {
	var out []string
	for _, v := range m {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		u, ok := entry["uuid"]
		if !ok {
			continue
		}
		us := strings.TrimSpace(fmt.Sprintf("%v", u))
		if us != "" && us != provider.ZeroUUID {
			out = append(out, us)
		}
	}
	return out
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
