package metrics

import (
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "hasteward"

// --- Backup metrics ---

var (
	BackupLastSuccessTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "backup_last_success_timestamp",
		Help:      "Unix timestamp of the last successful backup.",
	}, []string{"engine", "cluster", "namespace", "repository"})

	BackupLastDurationSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "backup_last_duration_seconds",
		Help:      "Duration of the last backup in seconds.",
	}, []string{"engine", "cluster", "namespace", "repository"})

	BackupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "backup_total",
		Help:      "Total number of backup operations.",
	}, []string{"engine", "cluster", "namespace", "repository", "status"})

	BackupSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "backup_size_bytes",
		Help:      "Size of the last backup in bytes.",
	}, []string{"engine", "cluster", "namespace", "repository"})
)

// --- Triage metrics ---

var (
	TriageLastRunTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "triage_last_run_timestamp",
		Help:      "Unix timestamp of the last triage run.",
	}, []string{"engine", "cluster", "namespace"})

	TriageHealthyInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "triage_healthy_instances",
		Help:      "Number of healthy instances after last triage.",
	}, []string{"engine", "cluster", "namespace"})

	TriageTotalInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "triage_total_instances",
		Help:      "Total number of instances after last triage.",
	}, []string{"engine", "cluster", "namespace"})

	TriageResult = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "triage_result",
		Help:      "Last triage result (1=active). Labels: result=healthy|unhealthy|split_brain.",
	}, []string{"engine", "cluster", "namespace", "result"})
)

// --- Repair metrics ---

var (
	RepairTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "repair_total",
		Help:      "Total number of repair operations.",
	}, []string{"engine", "cluster", "namespace", "status"})

	RepairLastTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "repair_last_timestamp",
		Help:      "Unix timestamp of the last repair.",
	}, []string{"engine", "cluster", "namespace"})
)

// --- Repository metrics ---

var (
	RepositorySnapshotCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "repository_snapshot_count",
		Help:      "Number of snapshots in a backup repository.",
	}, []string{"repository"})

	RepositoryTotalSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "repository_total_size_bytes",
		Help:      "Total size of a backup repository in bytes.",
	}, []string{"repository"})

	RepositoryDeduplicatedSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "repository_deduplicated_size_bytes",
		Help:      "Deduplicated size of a backup repository in bytes.",
	}, []string{"repository"})
)

// --- Operator health metrics ---

var (
	ManagedDatabases = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "managed_databases",
		Help:      "Number of databases managed by hasteward.",
	}, []string{"engine"})

	ControllerReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "controller_reconcile_total",
		Help:      "Total number of controller reconciliation loops.",
	}, []string{"engine", "status"})
)

func init() {
	// Register all metrics with the controller-runtime metrics registry.
	// This registry is automatically served at :8080/metrics by the manager.
	metrics.Registry.MustRegister(
		// Backup
		BackupLastSuccessTimestamp,
		BackupLastDurationSeconds,
		BackupTotal,
		BackupSizeBytes,
		// Triage
		TriageLastRunTimestamp,
		TriageHealthyInstances,
		TriageTotalInstances,
		TriageResult,
		// Repair
		RepairTotal,
		RepairLastTimestamp,
		// Repository
		RepositorySnapshotCount,
		RepositoryTotalSizeBytes,
		RepositoryDeduplicatedSizeBytes,
		// Operator
		ManagedDatabases,
		ControllerReconcileTotal,
	)
}

// --- Helper functions for recording metrics from engine results ---

// RecordBackupSuccess records metrics for a successful backup.
func RecordBackupSuccess(engine, cluster, ns, repo string, result *model.BackupResult) {
	labels := prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns, "repository": repo,
	}
	BackupLastSuccessTimestamp.With(labels).Set(float64(time.Now().Unix()))
	BackupLastDurationSeconds.With(labels).Set(result.Duration.Seconds())
	BackupTotal.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns, "repository": repo, "status": "success",
	}).Inc()
	if result.Size > 0 {
		BackupSizeBytes.With(labels).Set(float64(result.Size))
	}
}

// RecordBackupFailure records metrics for a failed backup.
func RecordBackupFailure(engine, cluster, ns, repo string) {
	BackupTotal.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns, "repository": repo, "status": "failure",
	}).Inc()
}

// RecordTriageResult records metrics for a triage operation.
func RecordTriageResult(engine, cluster, ns string, result *model.TriageResult, triageStatus string) {
	labels := prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns,
	}
	TriageLastRunTimestamp.With(labels).Set(float64(time.Now().Unix()))
	TriageHealthyInstances.With(labels).Set(float64(result.ReadyCount))
	TriageTotalInstances.With(labels).Set(float64(result.TotalCount))

	// Set the active result label (reset others to 0)
	for _, r := range []string{"healthy", "unhealthy", "split_brain"} {
		val := float64(0)
		if r == triageStatus {
			val = 1
		}
		TriageResult.With(prometheus.Labels{
			"engine": engine, "cluster": cluster, "namespace": ns, "result": r,
		}).Set(val)
	}
}

// RecordRepairSuccess records metrics for a successful repair.
func RecordRepairSuccess(engine, cluster, ns string) {
	RepairTotal.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns, "status": "success",
	}).Inc()
	RepairLastTimestamp.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns,
	}).Set(float64(time.Now().Unix()))
}

// RecordRepairFailure records metrics for a failed repair.
func RecordRepairFailure(engine, cluster, ns string) {
	RepairTotal.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns, "status": "failure",
	}).Inc()
	RepairLastTimestamp.With(prometheus.Labels{
		"engine": engine, "cluster": cluster, "namespace": ns,
	}).Set(float64(time.Now().Unix()))
}

// RecordManagedDatabases updates the managed database count for an engine.
func RecordManagedDatabases(engine string, count int) {
	ManagedDatabases.With(prometheus.Labels{"engine": engine}).Set(float64(count))
}

// RecordReconcile increments the reconcile counter.
func RecordReconcile(engine, status string) {
	ControllerReconcileTotal.With(prometheus.Labels{"engine": engine, "status": status}).Inc()
}
