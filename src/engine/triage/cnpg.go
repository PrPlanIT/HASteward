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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// HistoryRaw is the concatenated content of the instance's pg_wal/*.history
	// files — the timeline fork points that make authority provable rather than
	// guessed. Empty for a timeline-1 instance (no history exists) or when history
	// could not be read (a timeline->1 instance with empty HistoryRaw is treated as
	// Unread — its lineage is unknown, so ranking it would be a guess).
	HistoryRaw string
	// PGDataPresent records whether a data directory was found on the volume:
	// "yes" (pg_control present), "no" (mounted and provably empty — nothing to
	// lose), or "" (not determined, e.g. a live exec where presence is implicit).
	PGDataPresent string
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
	writeActivityDB    string   // app DB the write ledger was sampled from
	writeActivity      []string // top write-activity tables on the running primary
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
			// Only a genuine NotFound proves the PVC is absent (no data on a claim to
			// lose). ANY other error — API timeout, throttle, network blip, RBAC — is a
			// transient UNKNOWN: it must NEVER be read as "absent", or a connectivity
			// failure would discredit a node that in fact holds the winning data. UNKNOWN
			// fails closed downstream (treated as Unread), exactly like Bound-but-unread.
			if apierrors.IsNotFound(err) {
				data.pvcStates[name] = "MISSING"
			} else {
				data.pvcStates[name] = "UNKNOWN"
			}
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
		if !k8s.ContainerReadyByName(pod, "postgres") {
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
		cd.PGDataPresent = "yes" // a live postgres implies a data directory
		// Read the timeline-history files: the fork points authority is decided on.
		// Best-effort — a timeline-1 instance legitimately has none.
		if hr, herr := k8s.ExecCommand(ctx, pod.Name, ns, "postgres",
			[]string{"sh", "-c", cnpgHistoryCmd}); herr == nil {
			cd.HistoryRaw = strings.TrimSpace(hr.Stdout)
		}
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

	// Belly-up escalation: if nothing is serving and an instance holds data we could
	// not read (a crash-looping pod holding its RWO PVC), fence it read-only and read
	// its position, turning UNREAD into READ before authority is decided. No-op on a
	// live cluster or when every present instance was already read.
	t.maybeDeepRecover(ctx, data)

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
		t.collectWriteActivity(ctx, data, currentPrimary, ns)
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

// collectWriteActivity samples the running primary's per-table write ledger — the
// "churn vs real" evidence that turns a raw WAL-past-fork volume (Part A) into a
// lineage judgement. During the boundary-postgres split-brain the stale branch had
// GBs of WAL past the fork that was 100% job-scheduler/heartbeat churn (job_run,
// server_controller, …) with ZERO business writes; that is invisible in the LSN delta
// but obvious here. Best-effort and READ-ONLY: it queries the largest non-system DB's
// pg_stat_user_tables and never classifies — surfacing the ledger is triage's job, the
// operator weighs which tables are "real". Only the primary is queried (a replica's
// counters just mirror replayed WAL); it is a diagnostic aid, not an authority input.
func (t *cnpgTriage) collectWriteActivity(ctx context.Context, data *cnpgTriageData, primary, ns string) {
	dbRes, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", "postgres", "-t", "-A", "-c",
		"SELECT datname FROM pg_database WHERE datname NOT IN ('template0','template1','postgres') " +
			"AND datallowconn ORDER BY pg_database_size(datname) DESC LIMIT 1",
	})
	if err != nil {
		return
	}
	db := strings.TrimSpace(dbRes.Stdout)
	if db == "" || !isSafeDBIdent(db) {
		return
	}
	res, err := k8s.ExecCommand(ctx, primary, ns, "postgres", []string{
		"psql", "-U", "postgres", "-d", db, "-t", "-A", "-F", "|", "-c",
		"SELECT relname, n_tup_ins, n_tup_upd, n_tup_del, n_live_tup FROM pg_stat_user_tables " +
			"WHERE n_tup_ins + n_tup_upd + n_tup_del > 0 " +
			"ORDER BY n_tup_ins + n_tup_upd + n_tup_del DESC LIMIT 10",
	})
	if err != nil {
		return
	}
	data.writeActivityDB = db
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if strings.TrimSpace(line) != "" {
			data.writeActivity = append(data.writeActivity, strings.TrimSpace(line))
		}
	}
}

// isSafeDBIdent guards the DB name (from pg_database, but interpolated into the exec
// argv) to a conservative identifier charset — belt-and-suspenders against an oddly
// named database, never a data check.
func isSafeDBIdent(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// formatWriteLedger turns the collected pg_stat_user_tables rows into human lines for
// the divergence guidance. Returns nil when nothing was sampled (primary down /
// unreadable / no writes) so callers simply omit the section.
func formatWriteLedger(data *cnpgTriageData) []string {
	if len(data.writeActivity) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("Write activity on the running primary (db=%s), top tables by ins+upd+del — "+
		"weigh CHURN (schedulers/heartbeats/queues) vs REAL business writes when choosing the lineage:", data.writeActivityDB)}
	for _, row := range data.writeActivity {
		p := strings.Split(row, "|")
		if len(p) < 4 {
			out = append(out, "  "+row)
			continue
		}
		live := ""
		if len(p) >= 5 {
			live = ", live=" + p[4]
		}
		out = append(out, fmt.Sprintf("  %s: ins=%s upd=%s del=%s%s", p[0], p[1], p[2], p[3], live))
	}
	return out
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

		// Read pg_controldata, the timeline-history files, a data-directory presence
		// marker, AND the disk breakdown in one read-only pass, so a stranded (down /
		// crash-looping) instance reports its full authority position and true usage
		// without a running postgres. Built via the shared helper-pod builder so the
		// Istio-injection exemption (a sidecar would strand it Running → Unread in a
		// mesh) is applied consistently.
		pod := k8s.BuildHelperPod(k8s.HelperPodOpts{
			Name: probeName, Namespace: ns, Image: imageName, ServiceAccount: sa,
			PVCName: tgt.Name, MountPath: "/var/lib/postgresql/data", ReadOnly: true,
			RunAsUID: &uid, RunAsGID: &uid, FSGroup: &uid,
			NodeName: tgt.Node, Labels: map[string]string{"cnpg-triage": "probe"},
			Command: []string{"sh", "-c",
				"pg_controldata /var/lib/postgresql/data/pgdata 2>/dev/null; " +
					cnpgHistoryScript + "; " + cnpgPGDataPresentScript + "; " + cnpgDiskScript},
		})

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
			GracePeriodSeconds: common.Ptr(int64(0)),
		})
	}

	return results, disks
}

func (t *cnpgTriage) waitAndCollectProbe(ctx context.Context, probeName, instanceName, ns string) (controlData, *model.DiskStats) {
	c := k8s.GetClients()

	// Poll for completion (30 retries, 5s delay = 150s max)
	var lastPod *corev1.Pod
	terminated := false
	for attempt := 0; attempt < 30; attempt++ {
		pod, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, probeName, metav1.GetOptions{})
		if err != nil {
			break
		}
		lastPod = pod
		phase := pod.Status.Phase
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			terminated = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	// A probe that never terminated is usually STUCK, not slow — surface why (node at
	// its pod cap, cordoned, NotReady) instead of silently reporting the instance
	// Unread, so the operator can free the node and re-triage.
	if !terminated {
		if sched := k8s.DescribePodScheduling(lastPod); sched != "" {
			common.WarnLog("probe for %s did not complete — %s", instanceName, sched)
		}
	}

	// Get logs
	logReq := c.Clientset.CoreV1().Pods(ns).GetLogs(probeName, &corev1.PodLogOptions{
		Container: "helper",
	})
	logBytes, err := logReq.DoRaw(ctx)
	if err != nil || len(logBytes) == 0 {
		return controlData{Pod: instanceName, Source: "none", ClusterState: "unknown",
			Timeline: "unknown", CheckpointLocation: "unknown", CheckpointTime: "unknown",
			MinRecoveryEnd: "unknown"}, nil
	}

	cd := parseControlData(instanceName, "pvc_probe", string(logBytes))
	cd.HistoryRaw = parseHistorySection(string(logBytes))
	cd.PGDataPresent = parsePGDataPresent(string(logBytes))
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
	renderAuthorityBanner(comparison)

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

	// CNPG authority status (shared projection) + recovery. Only a provable authority
	// that IS the primary (SafeToHeal) authorizes the automatic heal-from-primary path
	// in repair PreAssess; every other outcome fails closed. The specific reason lives
	// in comparison.Authority + SplitBrainDetails.
	result.AuthorityStatus = deriveAuthorityStatus(comparison.SafeToHeal)
	if comparison.SafeToHeal && currentPrimary != "" {
		result.RecommendedDonor = cnpgOrdinal(currentPrimary)
	} else {
		result.RecommendedDonor = "none"
	}
	result.Recovery = deriveRecovery(assessments, comparison, currentPrimary, result.ClusterPhase, data.primaryIsRunning)
	result.Diagnoses = t.diagnose(comparison, assessments, currentPrimary, data)

	// Display
	t.triageDisplay(data, result)

	return result, nil
}

// buildAuthorityInputs projects the collected control data into the pure authority
// inputs: it classifies each expected instance's ReadState — failing closed on
// anything that could hold data but was not read — and parses its timeline lineage.
func buildAuthorityInputs(data *cnpgTriageData, primaryName string) []authorityInput {
	var inputs []authorityInput
	for _, cd := range data.controlData {
		in := authorityInput{Pod: cd.Pod, IsPrimary: cd.Pod == primaryName}
		tl := strings.TrimSpace(cd.Timeline)
		pvcState := data.pvcStates[cd.Pod]

		switch {
		case tl != "" && tl != "unknown":
			// We read a position. Parse the lineage. An instance past timeline 1 with
			// NO readable history has unknown fork points — ranking it would be a guess,
			// so treat it as Unread rather than compare it blind.
			in.Timeline = parseTimelineInt(tl)
			in.CheckpointLSN = parseLSNValue(strings.TrimSpace(cd.CheckpointLocation))
			in.Switches = parseTimelineHistory(historyForTimeline(cd.HistoryRaw, in.Timeline))
			if in.Timeline > 1 && len(in.Switches) == 0 {
				in.ReadState = ReadStateUnread
				in.UnreadReason = fmt.Sprintf(
					"on timeline %d but its timeline-history is unread — fork points unknown; "+
						"re-probe (fenced inspection) before any heal", in.Timeline)
			} else {
				in.ReadState = ReadStateRead
			}

		case cd.PGDataPresent == "empty":
			// The probe POSITIVELY confirmed an empty data directory (ls succeeded and
			// the dir is empty) — nothing to lose; never blocks. A merely "unknown"
			// presence (mount glitch / permission error) is NOT proof and falls through.
			in.ReadState = ReadStateAbsentNoData

		case pvcState == "MISSING":
			// Proven NotFound (not a transient API error) → no data on a claim to lose.
			in.ReadState = ReadStateAbsentNoData

		default:
			// Anything not PROVEN empty/absent and not read — a PVC in any present state
			// (Bound/Pending/Lost/Released), an UNKNOWN (transient API-error) PVC, or an
			// "unknown" data-directory probe — blocks. Data may be here; refuse to decide
			// past it. A connectivity/mount failure can only land HERE, never in an
			// AbsentNoData branch, so it can never discredit a data-bearing node.
			in.ReadState = ReadStateUnread
			reason := "position UNREAD"
			if pvcState != "" {
				reason = fmt.Sprintf("PVC %s, position UNREAD", pvcState)
			}
			in.UnreadReason = reason + " — bring it up for inspection (fenced read) before any heal"
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// cnpgCrossInstanceComparison determines authority over the cluster by WAL lineage
// (see determineAuthority): unread instances that could hold data block the verdict,
// fork points — not the timeline NUMBER — decide recency, and true divergence
// refuses. It emits the SAME model.DataComparison shape as galera's sibling
// crossInstanceComparison — a splitBrain reason list that drives SafeToHeal — so the
// downstream assessments, AuthorityStatus, and Classify verdict are engine-agnostic.
func cnpgCrossInstanceComparison(data *cnpgTriageData, primaryName string) model.DataComparison {
	inputs := buildAuthorityInputs(data, primaryName)
	decision := determineAuthority(inputs)

	// MostAdvanced carries the decisive authority when there is one — the primary in
	// the normal case, or a lower-timeline replica in a decisive stale-restore
	// (mirrors galera surfacing bestSeqnoNode even when it is not the live primary).
	mostAdvanced := decision.Leader
	if mostAdvanced == "" && decision.Determinable && !decision.Diverged {
		mostAdvanced = primaryName
	}

	// Map the lineage decision onto the shared AuthorityOutcome + the splitBrain
	// reason list (the galera idiom). SafeToHeal is true only for AuthorityProvable —
	// the decisive leader IS the primary; every other outcome adds a reason and fails
	// closed.
	var splitBrain []string
	var outcome model.AuthorityOutcome
	switch {
	case !decision.Determinable:
		outcome = model.AuthorityUndeterminable
		splitBrain = append(splitBrain, decision.Blockers...)
	case decision.Diverged:
		outcome = model.AuthorityDiverged
		splitBrain = append(splitBrain, decision.Divergences...)
	case decision.Leader != "" && decision.Leader != primaryName:
		outcome = model.AuthorityLeaderNotPrimary
		splitBrain = append(splitBrain, fmt.Sprintf(
			"AUTHORITY IS NOT THE PRIMARY: %s holds the winning data, not %s — %s",
			decision.Leader, primaryName, decision.LeaderReason))
	case decision.Leader == primaryName && primaryName != "":
		outcome = model.AuthorityProvable
	default:
		// Leader == "" — every instance is provably empty, or there is no primary to
		// heal from. No authority to prove; refuse rather than heal from nothing.
		outcome = model.AuthorityUndeterminable
		splitBrain = append(splitBrain, "no instance holds readable data (all provably empty)")
	}

	// On a contested lineage (divergence, or an authority that is NOT the primary) the
	// operator has to CHOOSE which branch survives. Attach the running primary's
	// write-activity ledger so the churn-vs-real evidence rides alongside the WAL-volume
	// numbers instead of having to be gathered by hand. Diagnostic only — best-effort,
	// omitted when nothing was sampled.
	if outcome == model.AuthorityDiverged || outcome == model.AuthorityLeaderNotPrimary {
		splitBrain = append(splitBrain, formatWriteLedger(data)...)
	}

	var maVal int64
	var maLSN string
	for _, in := range inputs {
		if in.Pod == mostAdvanced {
			maVal, maLSN = in.Timeline, formatLSN(in.CheckpointLSN)
		}
	}
	okMsg := fmt.Sprintf("OK: primary %s is the decisive authority (timeline %d, checkpoint %s) — %s",
		primaryName, maVal, maLSN, decision.LeaderReason)

	// Shared assembly (SafeToHeal / Authority / Warnings / SplitBrainDetails); the
	// CNPG-specific CheckpointLocation is set on top.
	cmp := newAuthorityComparison(outcome, mostAdvanced, maVal, splitBrain, okMsg)
	cmp.CheckpointLocation = maLSN
	return cmp
}

// diagnose is CNPG triage's catalog of recognized conditions — the counterpart to
// Galera's diagnose(). It never mutates and never guesses: each entry is a NAMED
// condition paired with the safe remedy/plan a human drives. Currently: the two
// authority-recovery outcomes (leader_not_primary, diverged → an ordered escrow-first
// rebuild-around-the-authority plan; P3.2) and a pathological timeline rewind (P3.5).
func (t *cnpgTriage) diagnose(comparison model.DataComparison, assessments []model.InstanceAssessment, primaryName string, data *cnpgTriageData) []model.Diagnosis {
	cfg := t.p.Config()
	var out []model.Diagnosis
	if d := t.diagnoseAuthorityRecovery(comparison, assessments, primaryName); d != nil {
		out = append(out, *d)
	}
	// P3.4: a data-bearing authority crash-looping on a FULL disk is the urgent case —
	// the golden data is at risk while the node cannot even start. Raise it explicitly.
	if d := diagnoseTrappedAuthority(comparison, assessments, cfg.ClusterName, cfg.Namespace); d != nil {
		out = append(out, *d)
	}
	// P3.5: pathological timeline history is orthogonal to the authority outcome — a
	// cluster can be SafeToHeal now yet carry the fingerprint of a backwards restore.
	// Surface it regardless so a silent restore-loop doesn't go unnoticed.
	if d := diagnoseTimelineRewind(data, cfg.ClusterName, cfg.Namespace); d != nil {
		out = append(out, *d)
	}
	return out
}

// diagnoseAuthorityRecovery turns the two "the repair engine can't just heal" authority
// outcomes (leader_not_primary, diverged) into a NAMED condition with an ordered,
// escrow-first recovery plan. nil when the heal is safe / undeterminable.
func (t *cnpgTriage) diagnoseAuthorityRecovery(comparison model.DataComparison, assessments []model.InstanceAssessment, primaryName string) *model.Diagnosis {
	cfg := t.p.Config()
	switch comparison.Authority {
	case model.AuthorityLeaderNotPrimary:
		authority := comparison.MostAdvanced
		return &model.Diagnosis{
			ID: "cnpg-authority-not-primary",
			Summary: fmt.Sprintf("The data authority is %s (a replica), not the primary %s — recovery is to rebuild "+
				"the cluster AROUND %s, never to heal from the primary", authority, primaryName, authority),
			Detail: fmt.Sprintf("CNPG's normal heal clones replicas FROM the primary. Here the newest committed data is on "+
				"%s while the primary %s is an older/stale lineage, so a normal heal — or a --force targeted heal of %s — "+
				"would rm -rf the authority's pgdata and re-clone the stale data over it, DESTROYING the newest data. "+
				"Repair now refuses to heal the authority even with --force. Recovery, in order:\n%s",
				authority, primaryName, authority, cnpgRebuildAroundAuthoritySteps(cfg.ClusterName, cfg.Namespace, authority, assessments)),
			Remedy: cnpgAuthorityFirstStep(cfg.ClusterName, cfg.Namespace, authority, assessments),
			Target: authority,
		}
	case model.AuthorityDiverged:
		return &model.Diagnosis{
			ID: "cnpg-split-brain-diverged",
			Summary: "Committed data exists on more than one lineage past a shared fork — no automatic winner; " +
				"a human must choose the surviving lineage",
			Detail: "No instance is a safe authority. Review the divergence evidence in the authority verdict above — " +
				"the per-branch WAL-past-fork volume and the primary's write-activity ledger (churn vs real business writes) — " +
				"then ESCROW every instance before touching anything. Once you have chosen the surviving instance X, rebuild " +
				"the cluster AROUND X with the same ordered steps as `cnpg-authority-not-primary` (escrow → relieve → promote " +
				"X → heal the rest). HASteward will not choose for you: picking the wrong lineage is unrecoverable.",
			Remedy: fmt.Sprintf("hasteward backup -e cnpg -c %s -n %s   # escrow ALL before any mutation", cfg.ClusterName, cfg.Namespace),
		}
	default:
		return nil
	}
}

// cnpgRebuildAroundAuthoritySteps renders the ordered, escrow-first recovery plan for
// making a non-primary authority the cluster's source of truth. It prescribes only
// safe HASteward primitives (escrow, prune wal, the standard heal-from-primary once the
// authority IS primary); the promotion itself is flagged as the manual step HASteward
// does not yet automate (P3.2b) rather than glossed over.
func cnpgRebuildAroundAuthoritySteps(cluster, ns, authority string, assessments []model.InstanceAssessment) string {
	relief := ""
	if authorityIsDiskConstrained(authority, assessments) {
		relief = fmt.Sprintf(" (it is disk-full/crash-looping — relieve WAL first: "+
			"`hasteward prune wal -e cnpg -c %s -n %s --instance %s --dry-run`)", cluster, ns, cnpgOrdinal(authority))
	}
	return fmt.Sprintf(
		"  1. Escrow every instance (reversible) before touching anything: hasteward backup -e cnpg -c %s -n %s\n"+
			"  2. Bring the authority %s up Ready and inspect it%s\n"+
			"  3. Make %s the primary. CNPG has no single safe command to promote a divergent/behind replica; do this "+
			"deliberately (switchover only if the topology is healthy, otherwise a rebuild-based promotion) — HASteward "+
			"does not yet automate this step.\n"+
			"  4. Once %s is the primary, rebuild the stale ex-primary and replicas FROM it: hasteward repair -e cnpg -c %s -n %s",
		cluster, ns, authority, relief, authority, authority, cluster, ns)
}

// cnpgAuthorityFirstStep is the single safe command to run first: relieve the authority
// if it is wedged on disk, otherwise escrow. Kept surgical so the Remedy field is one
// actionable, --dry-run-able line.
func cnpgAuthorityFirstStep(cluster, ns, authority string, assessments []model.InstanceAssessment) string {
	if authorityIsDiskConstrained(authority, assessments) {
		return fmt.Sprintf("hasteward prune wal -e cnpg -c %s -n %s --instance %s --dry-run   # relieve the wedged authority first",
			cluster, ns, cnpgOrdinal(authority))
	}
	return fmt.Sprintf("hasteward backup -e cnpg -c %s -n %s   # escrow the authority before any promotion", cluster, ns)
}

// authorityIsDiskConstrained reports whether the authority instance is stuck on disk
// (disk_full crash, or a PVC ≥95% used) — the case that must be relieved before it can
// be brought up. Conservative: unknown disk → false (do not fabricate a relief step).
func authorityIsDiskConstrained(authority string, assessments []model.InstanceAssessment) bool {
	for _, a := range assessments {
		if a.Pod != authority {
			continue
		}
		if a.CrashReason == "disk_full" {
			return true
		}
		if a.Disk != nil && a.Disk.TotalBytes > 0 && a.Disk.UsedPercent >= 95 {
			return true
		}
	}
	return false
}

// diagnoseTrappedAuthority (P3.4) raises the urgent alarm: a data-bearing AUTHORITY
// (authoritative classification, or the proven MostAdvanced) that is not ready because
// its volume is FULL. Such a node cannot checkpoint or recycle WAL, so it never recovers
// on its own and the newest data sits at risk — the exact state boundary-postgres-2 was
// stuck in for days. The remedy is WAL relief, which HASteward can now run even for a
// non-primary authority (P3.4 in prunewal). Conservative: unknown disk → not flagged.
func diagnoseTrappedAuthority(comparison model.DataComparison, assessments []model.InstanceAssessment, cluster, ns string) *model.Diagnosis {
	for _, a := range assessments {
		isAuthorityNode := a.Classification == model.ClassAuthoritative ||
			(comparison.MostAdvanced != "" && a.Pod == comparison.MostAdvanced)
		trapped := !a.IsReady && (a.CrashReason == "disk_full" ||
			(a.Disk != nil && a.Disk.TotalBytes > 0 && a.Disk.UsedPercent >= 95))
		if isAuthorityNode && trapped {
			return &model.Diagnosis{
				ID: "cnpg-authority-wal-trapped",
				Summary: fmt.Sprintf("URGENT: the data authority %s is not ready on a FULL disk — the golden "+
					"data is at risk while the node cannot start", a.Pod),
				Detail: "A data-bearing authority stuck on a disk-full volume cannot checkpoint or recycle WAL, so it " +
					"never recovers on its own and the newest data sits at risk. Relieve it by pruning WAL older than its " +
					"OWN checkpoint REDO — committed data past the checkpoint is kept, so this is safe. HASteward can now " +
					"run this relief even when the authority is a non-primary replica. Escrow first if the data is irreplaceable.",
				Remedy: fmt.Sprintf("hasteward prune wal -e cnpg -c %s -n %s --instance %s --dry-run", cluster, ns, cnpgOrdinal(a.Pod)),
				Target: a.Pod,
			}
		}
	}
	return nil
}

// diagnoseTimelineRewind (P3.5) flags a REWIND in the timeline history: a timeline that
// forked at an LSN BEHIND an earlier fork point. WAL only ever moves forward, and a
// normal failover forks a new timeline at the current (higher) LSN — so a fork that
// goes backwards is the unambiguous fingerprint of a PITR / restore to an earlier point
// (the very thing that created boundary-postgres's stale TL9). It is a health SIGNAL,
// not an error: a restore may have been intentional, but a silent one that discarded
// committed WAL looks exactly like this, so triage surfaces it instead of coping. It
// inspects every instance's own-timeline lineage and reports the deepest rewind found.
func diagnoseTimelineRewind(data *cnpgTriageData, cluster, ns string) *model.Diagnosis {
	if data == nil {
		return nil
	}
	var worstPod string
	var worstTL, maxTL, from, to int64
	for _, cd := range data.controlData {
		tl := parseTimelineInt(strings.TrimSpace(cd.Timeline))
		if tl > maxTL {
			maxTL = tl
		}
		sps := parseTimelineHistory(historyForTimeline(cd.HistoryRaw, tl))
		for i := 1; i < len(sps); i++ {
			if sps[i].SwitchLSN < sps[i-1].SwitchLSN {
				// A fork behind the previous fork — a backwards restore. Keep the one on
				// the highest timeline (the most-restored lineage) as the headline.
				if tl >= worstTL {
					worstPod, worstTL = cd.Pod, tl
					from, to = sps[i-1].SwitchLSN, sps[i].SwitchLSN
				}
				break
			}
		}
	}
	if worstPod == "" {
		return nil
	}
	return &model.Diagnosis{
		ID: "cnpg-timeline-rewind",
		Summary: fmt.Sprintf("Timeline history shows a REWIND on %s — timeline %d forked at %s, BEHIND an earlier "+
			"fork at %s (a restore/PITR to an earlier point); the cluster spans %d timelines",
			worstPod, worstTL, formatLSN(to), formatLSN(from), maxTL),
		Detail: "WAL only moves forward and a normal failover forks at the current LSN, so a fork that goes BACKWARDS is " +
			"the fingerprint of a point-in-time restore. If that restore was intentional this is informational; if it was " +
			"not, the pre-rewind lineage held committed WAL that the restored timeline discarded — check the authority " +
			"verdict above, because the instance still on the pre-rewind timeline may be the real data authority.",
		Remedy: fmt.Sprintf("hasteward triage -e cnpg -c %s -n %s"+
			"   # review the authority verdict; escrow before acting on any divergence", cluster, ns),
		Target: worstPod,
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
		// Prefer the universal PVC-probe breakdown (real even for down instances)
		// over the running-pod df map (0 when the instance is down). One collector,
		// two consumers: the breakdown feeds both these notes and the Disk field.
		diskPct := data.diskUsage[inst.Pod]
		if ds := data.diskStats[inst.Pod]; ds != nil && (ds.Source == "exec" || ds.Source == "pvc_probe") {
			diskPct = ds.UsedPercent
		}
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
		// Same-timeline but behind AND not streaming = cannot catch up by replication:
		// the WAL it needs has been recycled (shows as crash-looping or idle-not-streaming),
		// so its only path home is a re-clone — disposable + needs heal, not "recoverable,
		// wait for streaming". Disk-full (the breaker's deadlock domain) and just-missing
		// pods (CNPG may recreate them and they may then stream) keep their own handling.
		stranded := sameTL && behindLSN && !isStreaming && !isMissing && !diskFull
		classification := classifyInstance(isPrimary, isAuthority, hasData, comparison.SafeToHeal, behindTL, sameTL, stranded)

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
				needsHeal = true
				notes = append(notes, "crash-looping, not streaming")
				recommendation = fmt.Sprintf("Same timeline but crash-looping and not streaming — the WAL needed to "+
					"catch up has most likely been recycled, so it cannot rejoin by replication. Check pod logs to "+
					"confirm the root cause, then heal (re-clone from primary).\n\n  %s", healCmd)
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
		if cs, ok := k8s.ContainerStatusByName(pod, "postgres"); ok {
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
		if cs, ok := k8s.ContainerStatusByName(pod, "postgres"); ok {
			restarts = cs.RestartCount
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

	renderDiagnoses(result.Diagnoses)
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

// cnpgHistoryCmd emits every timeline-history file, EACH under a "###<filename>"
// marker, so the parser can select the CURRENT timeline's file. A blind
// `cat *.history` concatenates every file — and each 0000000N.history REPEATS all
// earlier switch lines — which corrupts the reconstructed lineage: a real
// multi-restore cluster (many .history files) then looks divergent for the wrong
// reason (the fork LSN becomes an artifact of the next file's first line). ${f##*/}
// is a POSIX basename (no coreutils dependency).
const cnpgHistoryCmd = `for f in /var/lib/postgresql/data/pgdata/pg_wal/*.history; do [ -e "$f" ] || continue; echo "###${f##*/}"; cat "$f"; done`

// cnpgHistoryScript wraps cnpgHistoryCmd under the ===HISTORY=== section delimiter
// for the probe's single multi-section read.
const cnpgHistoryScript = `echo "===HISTORY==="; ` + cnpgHistoryCmd

// cnpgPGDataPresentScript reports the data directory's state with POSITIVE proof,
// three-valued: "yes" (pg_control present → data here), "empty" (the pgdata dir was
// listed successfully AND is genuinely empty → nothing to lose), or "unknown"
// (couldn't stat/list — a mount glitch or permission error). Only "empty" may make a
// node disposable; "unknown" must NEVER be read as empty, or a failed mount would
// discredit a node that holds data. The `ls` exit code gates the emptiness claim so a
// permission-denied or absent path degrades to "unknown", not "empty".
const cnpgPGDataPresentScript = `echo "===PGDATA_PRESENT==="
if [ -f /var/lib/postgresql/data/pgdata/global/pg_control ]; then echo yes
else ent=$(ls -A /var/lib/postgresql/data/pgdata 2>/dev/null); rc=$?
  if [ $rc -eq 0 ] && [ -z "$ent" ]; then echo empty; else echo unknown; fi
fi`

// extractSection returns the lines following a "===NAME===" marker up to the next
// "===...===" marker (or end of input).
func extractSection(raw, marker string) string {
	var out []string
	in := false
	for _, l := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(l)
		if t == marker {
			in = true
			continue
		}
		if in && strings.HasPrefix(t, "===") && strings.HasSuffix(t, "===") {
			break
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func parseHistorySection(raw string) string {
	return strings.TrimSpace(extractSection(raw, "===HISTORY==="))
}

// parsePGDataPresent returns "yes" | "empty" | "unknown" | "" (marker absent). Only
// "empty" is positive proof the volume holds nothing; "unknown" is treated as Unread.
func parsePGDataPresent(raw string) string {
	for _, line := range strings.Split(extractSection(raw, "===PGDATA_PRESENT==="), "\n") {
		switch strings.TrimSpace(line) {
		case "yes":
			return "yes"
		case "empty":
			return "empty"
		case "unknown":
			return "unknown"
		}
	}
	return ""
}

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
//
// stranded marks a same-timeline replica that is behind and cannot catch up by
// replication — the WAL it needs has been recycled (it shows as crash-looping or
// idle-not-streaming). Same timeline, but a re-clone is its only path home, so it
// is disposable, not recoverable.
func classifyInstance(isPrimary, isAuthority, hasData, safeToHeal, behindTL, sameTL, stranded bool) model.Classification {
	if !hasData || !safeToHeal {
		return model.ClassUnknown // unreadable, or ambiguous authority → refuse
	}
	if isPrimary || isAuthority {
		return model.ClassAuthoritative
	}
	if behindTL || stranded {
		return model.ClassDisposable // dead timeline, or same-TL but WAL-stranded; re-clone is its only path home
	}
	if sameTL {
		return model.ClassRecoverable // same timeline, streaming/current; rejoins without a wipe
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
