package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1alpha1 "gitlab.prplanit.com/precisionplanit/hasteward/api/v1alpha1"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/metrics"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedDB holds the resolved state for a database managed by hasteward.
type ManagedDB struct {
	Namespace   string
	ClusterName string
	Engine      string // "cnpg" or "galera"
	Config      *v1alpha1.EffectiveConfig
}

// scheduledDB tracks the cron entries for a managed database.
type scheduledDB struct {
	db        *ManagedDB
	backupIDs []cron.EntryID
	triageID  cron.EntryID
}

// Scheduler manages cron-based backup and triage operations for all managed databases.
type Scheduler struct {
	cron     *cron.Cron
	rtClient client.Client
	managed  map[string]*scheduledDB // key: "engine/namespace/name"
	mu       sync.RWMutex
}

// NewScheduler creates a scheduler with a controller-runtime client for reading CRDs.
func NewScheduler(rtClient client.Client) *Scheduler {
	return &Scheduler{
		cron:     cron.New(cron.WithSeconds()),
		rtClient: rtClient,
		managed:  make(map[string]*scheduledDB),
	}
}

// Start begins the cron scheduler.
func (s *Scheduler) Start() {
	s.cron.Start()
	common.InfoLog("Cron scheduler started")
}

// Stop halts the cron scheduler.
func (s *Scheduler) Stop() {
	s.cron.Stop()
	common.InfoLog("Cron scheduler stopped")
}

// Register adds or updates a managed database in the scheduler.
// If the effective config changed, cron entries are re-created.
func (s *Scheduler) Register(key string, db *ManagedDB) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if already registered with same schedules
	if existing, ok := s.managed[key]; ok {
		if existing.db.Config.BackupSchedule == db.Config.BackupSchedule &&
			existing.db.Config.TriageSchedule == db.Config.TriageSchedule &&
			existing.db.Config.Mode == db.Config.Mode &&
			reposEqual(existing.db.Config.Repositories, db.Config.Repositories) {
			// Update config in place (retention, timeouts may have changed)
			existing.db = db
			return
		}
		// Schedules changed, remove old entries
		s.deregisterLocked(key)
	}

	entry := &scheduledDB{db: db}

	// Schedule backups (one per repository)
	if db.Config.BackupSchedule != "" && db.Config.Mode != "disabled" {
		for _, repoName := range db.Config.Repositories {
			repo := repoName // capture
			id, err := s.cron.AddFunc(db.Config.BackupSchedule, func() {
				s.runBackup(db, repo)
			})
			if err != nil {
				common.ErrorLog("Failed to schedule backup for %s repo %s: %v", key, repo, err)
				continue
			}
			entry.backupIDs = append(entry.backupIDs, id)
			common.InfoLog("Scheduled backup for %s → repo %s (%s)", key, repo, db.Config.BackupSchedule)
		}
	}

	// Schedule triage
	if db.Config.TriageSchedule != "" && db.Config.Mode != "disabled" {
		id, err := s.cron.AddFunc(db.Config.TriageSchedule, func() {
			s.runTriage(db)
		})
		if err != nil {
			common.ErrorLog("Failed to schedule triage for %s: %v", key, err)
		} else {
			entry.triageID = id
			common.InfoLog("Scheduled triage for %s (%s, mode=%s)", key, db.Config.TriageSchedule, db.Config.Mode)
		}
	}

	s.managed[key] = entry
	s.updateManagedGauge()
}

// Deregister removes a managed database from the scheduler.
func (s *Scheduler) Deregister(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregisterLocked(key)
}

func (s *Scheduler) deregisterLocked(key string) {
	entry, ok := s.managed[key]
	if !ok {
		return
	}
	for _, id := range entry.backupIDs {
		s.cron.Remove(id)
	}
	if entry.triageID != 0 {
		s.cron.Remove(entry.triageID)
	}
	delete(s.managed, key)
	common.InfoLog("Deregistered %s from scheduler", key)
	s.updateManagedGauge()
}

// ManagedCount returns the number of managed databases.
func (s *Scheduler) ManagedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.managed)
}

// getRepoCredentials fetches restic connection details from a BackupRepository CR.
func (s *Scheduler) getRepoCredentials(ctx context.Context, repoName string) (repository, password string, env map[string]string, err error) {
	repo := &v1alpha1.BackupRepository{}
	if err := s.rtClient.Get(ctx, types.NamespacedName{Name: repoName}, repo); err != nil {
		return "", "", nil, fmt.Errorf("BackupRepository %q not found: %w", repoName, err)
	}

	// Get password from secret
	ref := repo.Spec.Restic.PasswordSecretRef
	secret := &corev1.Secret{}
	if err := s.rtClient.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, secret); err != nil {
		return "", "", nil, fmt.Errorf("password Secret %s/%s not found: %w", ref.Namespace, ref.Name, err)
	}
	pw, ok := secret.Data[ref.Key]
	if !ok {
		return "", "", nil, fmt.Errorf("key %q not found in Secret %s/%s", ref.Key, ref.Namespace, ref.Name)
	}

	// Get env vars from optional secret
	envMap := make(map[string]string)
	if repo.Spec.Restic.EnvSecretRef != nil {
		envRef := repo.Spec.Restic.EnvSecretRef
		envSecret := &corev1.Secret{}
		if err := s.rtClient.Get(ctx, types.NamespacedName{Name: envRef.Name, Namespace: envRef.Namespace}, envSecret); err != nil {
			return "", "", nil, fmt.Errorf("env Secret %s/%s not found: %w", envRef.Namespace, envRef.Name, err)
		}
		for k, v := range envSecret.Data {
			envMap[k] = string(v)
		}
	}

	return repo.Spec.Restic.Repository, string(pw), envMap, nil
}

// updateStatusAnnotation patches a status annotation on a database CR.
func (s *Scheduler) updateStatusAnnotation(ctx context.Context, db *ManagedDB, key, value string) {
	var gvr schema.GroupVersionResource
	switch db.Engine {
	case "cnpg":
		gvr = k8s.CNPGClusterGVR
	case "galera":
		gvr = k8s.MariaDBGVR
	}

	c := k8s.GetClients()
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
	_, err := c.Dynamic.Resource(gvr).Namespace(db.Namespace).Patch(
		ctx, db.ClusterName, types.MergePatchType, patch, k8s.PatchOptions)
	if err != nil {
		common.WarnLog("Failed to update annotation %s on %s/%s: %v", key, db.Namespace, db.ClusterName, err)
	}
}

// updateMultipleAnnotations patches multiple status annotations at once.
func (s *Scheduler) updateMultipleAnnotations(ctx context.Context, db *ManagedDB, kv map[string]string) {
	var gvr schema.GroupVersionResource
	switch db.Engine {
	case "cnpg":
		gvr = k8s.CNPGClusterGVR
	case "galera":
		gvr = k8s.MariaDBGVR
	}

	annotations := ""
	first := true
	for k, v := range kv {
		if !first {
			annotations += ","
		}
		annotations += fmt.Sprintf("%q:%q", k, v)
		first = false
	}

	c := k8s.GetClients()
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%s}}}`, annotations))
	_, err := c.Dynamic.Resource(gvr).Namespace(db.Namespace).Patch(
		ctx, db.ClusterName, types.MergePatchType, patch, k8s.PatchOptions)
	if err != nil {
		common.WarnLog("Failed to update annotations on %s/%s: %v", db.Namespace, db.ClusterName, err)
	}
}

func reposEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// updateManagedGauge recalculates the managed databases gauge per engine.
// Must be called with s.mu held.
func (s *Scheduler) updateManagedGauge() {
	counts := map[string]int{}
	for _, entry := range s.managed {
		counts[entry.db.Engine]++
	}
	for _, eng := range []string{"cnpg", "galera"} {
		metrics.RecordManagedDatabases(eng, counts[eng])
	}
}

// nowRFC3339 returns the current time in RFC3339 format.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
