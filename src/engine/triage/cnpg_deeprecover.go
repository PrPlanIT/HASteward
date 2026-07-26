package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/cnpgjob"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
)

// maybeDeepRecover is CNPG triage's non-destructive escalation for a PROVABLY
// belly-up cluster — the direct analog of Galera's maybeDeepRecover. When nothing is
// serving and an instance's position could not be read the normal way (a
// crash-looping pod holding its RWO PVC, so the read-only probe cannot mount it), it
// fences that instance, reads pg_controldata + timeline history from its now-free PVC
// via the shared cnpgjob.OfflinePVCJob (READ-ONLY), then restores. Like Galera it
// NEVER declares authority, heals, promotes, or writes — it only turns UNREAD
// positions into READ ones so the authority determination is informed instead of
// blocked. Gathering that truth is triage's job.
//
// The gate is paranoid and fail-safe (mirrors Galera): escalate ONLY when nothing is
// alive — no Ready postgres and no ready instances reported — AND a fresh pg_isready
// probe confirms no pod is accepting connections. If any instance is (or might be)
// serving, we stay hands-off: a live CNPG cluster is never fenced to inspect a
// replica; that single-stranded case is inspected in repair's already-fenced window.
func (t *cnpgTriage) maybeDeepRecover(ctx context.Context, data *cnpgTriageData) {
	readyInstances := k8s.GetNestedInt64(t.p.Cluster(), "status", "readyInstances")
	if cnpgIsAnythingAlive(data, readyInstances) {
		return // a Ready/serving instance exists — never fence a live cluster
	}
	targets := unreadBoundInstances(data)
	if len(targets) == 0 {
		return // nothing is both present-with-data AND unread — no blind spot to clear
	}
	if t.cnpgAnyServing(ctx, data) {
		output.Printf("Belly-up escalation SKIPPED: a fresh pg_isready found a server still accepting connections — staying hands-off.\n")
		return
	}

	output.Section("Belly-up Escalation (fenced read-only pg_controldata)")
	common.WarnLog("Belly-up CNPG cluster, %d instance(s) UNREAD — escalating to a fenced read-only inspection to read "+
		"authoritative positions (triage evaluation only; no authority is declared and nothing is written).", len(targets))

	for _, name := range targets {
		cd, err := t.deepRecoverInstance(ctx, data, name)
		if err != nil {
			common.WarnLog("deep-recover: could not read %s: %v — it stays UNREAD (authority remains undeterminable, by design)", name, err)
			continue
		}
		for i := range data.controlData {
			if data.controlData[i].Pod == name {
				data.controlData[i] = cd
				break
			}
		}
		output.Printf("  %s: recovered timeline=%s checkpoint=%s (read-only, fenced)\n",
			name, cd.Timeline, cd.CheckpointLocation)
	}
}

// cnpgIsAnythingAlive reports whether any instance is serving — a Ready postgres
// container, or ready instances per the cluster status. Pure over the collected
// state so the belly-up gate is unit-testable (the property that must never regress:
// a live cluster is never fenced).
func cnpgIsAnythingAlive(data *cnpgTriageData, readyInstances int64) bool {
	if readyInstances > 0 {
		return true
	}
	for _, pod := range data.runningPods {
		if k8s.ContainerReadyByName(pod, "postgres") {
			return true
		}
	}
	return false
}

// unreadBoundInstances returns the instances that hold data (a Bound PVC) but whose
// position could not be read — the only nodes a fenced read-only inspection can help.
// A non-Bound PVC (Pending/Lost/UNKNOWN) cannot be mounted even fenced, so it stays
// unread and continues to block authority (correctly).
func unreadBoundInstances(data *cnpgTriageData) []string {
	var out []string
	for _, cd := range data.controlData {
		if strings.TrimSpace(cd.Timeline) == "unknown" && data.pvcStates[cd.Pod] == "Bound" {
			out = append(out, cd.Pod)
		}
	}
	return out
}

// cnpgAnyServing freshly probes every running pod with pg_isready. Belt-and-
// suspenders over the collect-time readiness snapshot: only a POSITIVE "accepting
// connections" counts as serving. An exec error on a not-ready pod is consistent
// with a wedged/crash-looping instance and must not, on its own, block recovery —
// otherwise a crash-looping cluster could never be inspected.
func (t *cnpgTriage) cnpgAnyServing(ctx context.Context, data *cnpgTriageData) bool {
	ns := t.p.Config().Namespace
	for _, pod := range data.runningPods {
		res, err := k8s.ExecCommand(ctx, pod.Name, ns, "postgres",
			[]string{"pg_isready", "-U", "postgres", "-h", "localhost"})
		if err == nil && strings.Contains(res.Stdout, "accepting connections") {
			return true
		}
	}
	return false
}

// deepRecoverInstance fences one belly-up instance and reads its pg_controldata +
// timeline history READ-ONLY via cnpgjob.OfflinePVCJob (fence → disable reconcile →
// acquire PVC → OnPVCAcquired exec → re-enable → unfence, with the restore-on-every-
// exit-path safety contract already built into cnpgjob). The helper is a dumb
// `sleep infinity` exec target mounting the PVC read-only; all reads run in Go.
func (t *cnpgTriage) deepRecoverInstance(ctx context.Context, data *cnpgTriageData, instanceName string) (controlData, error) {
	cfg := t.p.Config()
	ns := cfg.Namespace
	// CNPG convention: the instance's PVC name equals the pod name.
	targetPVC := instanceName
	helperName := instanceName + "-triage-deeprecover"
	imageName := k8s.GetNestedString(t.p.Cluster(), "spec", "imageName")
	sa := k8s.ServiceAccountFromPods(data.runningPods)
	uid := int64(26) // CNPG postgres uid, same as the read-only probe

	// Shared builder → Istio exemption applied (a sidecar would strand the helper
	// Running so the PVC-acquire wait never settles). Read-only: inspection must never
	// write the datadir.
	helperPod := k8s.BuildHelperPod(k8s.HelperPodOpts{
		Name: helperName, Namespace: ns, Image: imageName, ServiceAccount: sa,
		PVCName: targetPVC, MountPath: "/var/lib/postgresql/data", ReadOnly: true,
		RunAsUID: &uid, RunAsGID: &uid, FSGroup: &uid,
		Labels:  map[string]string{"cnpg-triage": "deep-recover"},
		Command: []string{"sh", "-c", "sleep infinity"},
	})

	var recovered controlData
	err := cnpgjob.Run(ctx, cnpgjob.OfflinePVCJob{
		Namespace:        ns,
		ClusterName:      cfg.ClusterName,
		TargetPod:        instanceName,
		TargetPVC:        targetPVC,
		HelperPod:        helperPod,
		HelperPodName:    helperName,
		Label:            "deep-recover",
		DeleteTimeoutSec: cfg.DeleteTimeout,
		// Go-driven: the read runs while the helper holds the PVC.
		OnPVCAcquired: func(ctx context.Context) error {
			cd, rerr := readControlDataViaHelper(ctx, helperName, ns, instanceName)
			if rerr != nil {
				return rerr
			}
			recovered = cd
			return nil
		},
	})
	if err != nil {
		return controlData{}, err
	}
	return recovered, nil
}

// readControlDataViaHelper execs pg_controldata + the timeline-history files + a
// data-directory presence check into the deep-recover helper (which holds the fenced
// PVC), returning the parsed controlData. Read-only; mirrors the reads the rung-0/1
// paths already perform, so the recovered entry is indistinguishable from a normal
// Read to the authority determination.
func readControlDataViaHelper(ctx context.Context, helperPod, ns, instanceName string) (controlData, error) {
	res, err := k8s.ExecCommand(ctx, helperPod, ns, "helper",
		[]string{"pg_controldata", "/var/lib/postgresql/data/pgdata"})
	if err != nil {
		return controlData{}, fmt.Errorf("pg_controldata via helper: %w", err)
	}
	cd := parseControlData(instanceName, "deep_recover", res.Stdout)
	cd.Reachable = true
	if hr, herr := k8s.ExecCommand(ctx, helperPod, ns, "helper",
		[]string{"sh", "-c", cnpgHistoryCmd}); herr == nil {
		cd.HistoryRaw = strings.TrimSpace(hr.Stdout)
	}
	if pg, perr := k8s.ExecCommand(ctx, helperPod, ns, "helper",
		[]string{"sh", "-c", cnpgPGDataPresentScript}); perr == nil {
		cd.PGDataPresent = parsePGDataPresent(pg.Stdout)
	}
	return cd, nil
}
