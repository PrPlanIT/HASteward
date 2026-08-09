package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/PrPlanIT/HASteward/src/k8s"
)

// Reading a DOWN CNPG instance's business content — the increment-3 mechanism,
// transcribed from a live validation against a stuck (incomplete-basebackup) replica.
// Universal: no fencing, no volume snapshots. It runs INSIDE the instance's own
// still-Running pod (CNPG's instance manager keeps the container up even while
// postgres crash-loops), copies pgdata to scratch on the SAME PVC (read-only to the
// real data), opens the COPY standalone read-only, and queries the witness.
//
// The copy must be coaxed open. A crash-looping replica is typically stuck because it
// still holds a backup_label from an unfinished pg_basebackup, pointing recovery at
// WAL it can no longer fetch (its primary/archive is gone) — so it waits forever.
// Removing standby.signal + backup_label makes it crash-recover from its OWN
// pg_control checkpoint and open. Streaming/archive/ssl are neutralized; and
// logging_collector is forced OFF — left on (CNPG's default) it hijacks the log to
// /controller/log AND makes `pg_ctl -w` falsely report "did not start". Everything
// runs on the disposable copy; the real datadir is never modified. Fail-closed:
// any error → ok=false → the pair stays contentUnknown and authority refuses.

const (
	dcPGData  = "/var/lib/postgresql/data/pgdata"
	dcScratch = "/var/lib/postgresql/data/hw-content"
	dcSockDir = "/var/lib/postgresql/data/hw-content-sock"
	dcPort    = "5433"
)

type cutResult struct {
	count int64
	hash  string
}

// deepContentBase reads a down instance's witness fingerprint from a standalone
// read-only copy of its pgdata. peerMaxes are the LIVE instances' max positions, for
// which containment cuts are precomputed inside this one-shot window — a down instance
// cannot be re-queried on demand once the copy is torn down.
func deepContentBase(ctx context.Context, ns, pod string, spec witnessSpec, peerMaxes []string) witnessBase {
	// Always tear the copy down, even on early return / failure.
	defer func() {
		_, _ = k8s.ExecCommand(ctx, pod, ns, "postgres", []string{"sh", "-c", dcCleanupScript()})
	}()

	if _, err := k8s.ExecCommand(ctx, pod, ns, "postgres", []string{"sh", "-c", dcPrepStartScript(spec)}); err != nil {
		// pg_ctl can exit non-zero on readiness timing even when the server came up; the
		// base query below is the real liveness test. Only a hard exec error is fatal.
		return witnessBase{}
	}

	cnt, maxPos, hash, ok := parseBase(psqlCopy(ctx, ns, pod, spec.DB, spec.baseQuery()))
	if !ok {
		return witnessBase{}
	}
	cuts := make(map[string]cutResult, len(peerMaxes))
	for _, pm := range peerMaxes {
		if c, h, cok := parseCut(psqlCopy(ctx, ns, pod, spec.DB, spec.cutQuery(pm))); cok {
			cuts[pm] = cutResult{c, h}
		}
	}
	return witnessBase{
		ok: true, count: cnt, maxPos: maxPos, hash: hash,
		cut: func(peerMax string) (int64, string, bool) {
			r, ok := cuts[peerMax]
			return r.count, r.hash, ok
		},
	}
}

// dcPrepStartScript copies pgdata to scratch, strips the recovery blockers, appends
// standalone read-only overrides, and starts postgres on the copy.
func dcPrepStartScript(_ witnessSpec) string {
	return fmt.Sprintf(`set -u
rm -rf %[1]s %[2]s
mkdir -p %[2]s && chmod 700 %[2]s
cp -a %[3]s %[1]s
rm -f %[1]s/standby.signal %[1]s/backup_label %[1]s/postmaster.pid %[1]s/recovery.signal
{
  echo "port = %[4]s"
  echo "listen_addresses = ''"
  echo "unix_socket_directories = '%[2]s'"
  echo "primary_conninfo = ''"
  echo "restore_command = ''"
  echo "archive_mode = off"
  echo "hot_standby = on"
  echo "ssl = off"
  echo "logging_collector = off"
} >> %[1]s/postgresql.conf
pg_ctl -D %[1]s -l %[2]s/pg.log -w -t 120 start
`, dcScratch, dcSockDir, dcPGData, dcPort)
}

func dcCleanupScript() string {
	return fmt.Sprintf("pg_ctl -D %[1]s -m immediate stop >/dev/null 2>&1; rm -rf %[1]s %[2]s", dcScratch, dcSockDir)
}

// psqlCopy queries the standalone copy over its local socket on the scratch port.
func psqlCopy(ctx context.Context, ns, pod, db, query string) (string, error) {
	res, err := k8s.ExecCommand(ctx, pod, ns, "postgres",
		[]string{"psql", "-h", dcSockDir, "-p", dcPort, "-U", "postgres", "-d", db, "-tAqc", query})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
