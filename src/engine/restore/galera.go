package restore

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/restic"
)

// DumpFilenameGalera is the virtual filename used in restic snapshots for mysqldump output.
const DumpFilenameGalera = "mysqldump.sql"

func init() {
	Register("galera", func(ep provider.EngineProvider) (Restorer, error) {
		p, ok := ep.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("restore/galera: expected *provider.GaleraProvider, got %T", ep)
		}
		return &galeraRestore{p: p}, nil
	})
}

type galeraRestore struct {
	p *provider.GaleraProvider
}

func (r *galeraRestore) Name() string { return r.p.Name() }

func (r *galeraRestore) Restore(ctx context.Context) (*model.RestoreResult, error) {
	return r.restoreDump(ctx)
}

func (r *galeraRestore) restoreDump(ctx context.Context) (*model.RestoreResult, error) {
	start := time.Now()
	cfg := r.p.Config()
	ns := cfg.Namespace

	// Find a healthy target pod
	target, err := r.findHealthyPod(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot find healthy pod for restore: %w", err)
	}

	snapshotID := cfg.Snapshot
	if snapshotID == "" {
		snapshotID = "latest"
	}

	rc := restic.NewClient(cfg.BackupsPath, cfg.ResticPassword)
	// Diverged snapshots use ordinal-prefixed filename
	dumpFile := DumpFilenameGalera
	if cfg.InstanceNumber != nil {
		dumpFile = strconv.Itoa(*cfg.InstanceNumber) + "-" + dumpFile
	}
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, cfg.ClusterName, dumpFile)
	filterTags := map[string]string{
		"engine":    "galera",
		"cluster":   cfg.ClusterName,
		"namespace": ns,
	}

	output.Section("Dump Restore")
	output.Field("Snapshot", snapshotID)
	output.Field("Target", target)
	output.Field("Repository", cfg.BackupsPath)

	// Set up pipe: restic dump -> pipe -> mysql stdin
	pr, pw := io.Pipe()

	var resticErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		resticErr = rc.Dump(ctx, snapshotID, stdinFilename, pw, filterTags)
		if resticErr != nil {
			pw.CloseWithError(resticErr)
		}
	}()

	// Stream restore using MYSQL_PWD env var
	cmd := []string{"sh", "-c",
		"export MYSQL_PWD='" + k8s.ShellEscape(r.p.RootPassword()) + "'; " +
			"mysql -u root"}

	common.InfoLog("Streaming restic dump → mysql")
	err = k8s.ExecStream(ctx, target, ns, "mariadb", cmd, pr, output.Writer(), os.Stderr)
	<-done

	if err != nil {
		return nil, fmt.Errorf("restore stream failed: %w", err)
	}
	if resticErr != nil {
		return nil, fmt.Errorf("restic dump failed: %w", resticErr)
	}

	output.Section("Restore Complete")
	common.InfoLog("Galera replication will propagate the restored data to other nodes")
	output.Success("Restore complete")
	return &model.RestoreResult{
		Engine:     r.p.Name(),
		Cluster:    model.ObjectRef{Namespace: ns, Name: cfg.ClusterName},
		SnapshotID: snapshotID,
		Duration:   time.Since(start),
	}, nil
}

// findHealthyPod returns the name of a healthy running MariaDB pod.
func (r *galeraRestore) findHealthyPod(ctx context.Context) (string, error) {
	cfg := r.p.Config()
	return k8s.FindReadyPod(ctx, cfg.Namespace, r.p.PodSelector(), "mariadb")
}
