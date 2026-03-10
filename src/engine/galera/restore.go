package galera

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/k8s"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output"
	"gitlab.prplanit.com/precisionplanit/hasteward/src/output/model"
)

func (e *Engine) Restore(ctx context.Context) (*model.RestoreResult, error) {
	return e.restoreDump(ctx)
}

func (e *Engine) restoreDump(ctx context.Context) (*model.RestoreResult, error) {
	start := time.Now()
	ns := e.cfg.Namespace

	// Find a healthy target pod
	target, err := e.findHealthyPod(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot find healthy pod for restore: %w", err)
	}

	snapshotID := e.cfg.Snapshot
	if snapshotID == "" {
		snapshotID = "latest"
	}

	rc := e.newResticClient()
	// Diverged snapshots use ordinal-prefixed filename
	dumpFile := DumpFilename
	if e.cfg.InstanceNumber != nil {
		dumpFile = strconv.Itoa(*e.cfg.InstanceNumber) + "-" + dumpFile
	}
	stdinFilename := fmt.Sprintf("%s/%s/%s", ns, e.cfg.ClusterName, dumpFile)
	filterTags := map[string]string{
		"engine":    "galera",
		"cluster":   e.cfg.ClusterName,
		"namespace": ns,
	}

	output.Section("Dump Restore")
	output.Field("Snapshot", snapshotID)
	output.Field("Target", target)
	output.Field("Repository", e.cfg.BackupsPath)

	// Set up pipe: restic dump → pipe → mysql stdin
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
		"export MYSQL_PWD='" + k8s.ShellEscape(e.rootPassword) + "'; " +
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
		Engine:     e.Name(),
		Cluster:    model.ObjectRef{Namespace: ns, Name: e.cfg.ClusterName},
		SnapshotID: snapshotID,
		Duration:   time.Since(start),
	}, nil
}
