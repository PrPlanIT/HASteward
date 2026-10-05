package escrow

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reserveBytes is headroom that must remain free in the escrow store beyond the capture
// estimate, so an escrow can never fill the repository to the brim.
const reserveBytes int64 = 1 << 30 // 1 GiB

// maxOutstandingEscrows caps how many unreleased escrow snapshots one cluster may hold.
//
// It exists because the byte guard cannot bound a copy-on-write provider: creation costs
// nothing and the growth arrives later, so AvailableBytes has no honest number to report
// and the space check is structurally inert there. Without a cap, nothing stops escrows
// accumulating in the same pool as the volumes they protect. Twelve is deliberately
// generous — a handful of operations' worth — because tripping it should mean retention
// has stopped running, not that an operator was busy.
const maxOutstandingEscrows = 12

// escrowKind maps an operation to the kind label stamped on its escrow objects, so
// retention can report what took an escrow in the operator's own terms. The two
// long-standing values are preserved for the paths that already used them; anything
// else is labelled with the operation's own name.
func escrowKind(operation string) string {
	switch operation {
	case "escrow":
		return KindSplitBrain
	case "deadlock-recover":
		return KindDeadlock
	default:
		return operation
	}
}

// CountOutstanding is countOutstanding behind a package variable, so the cap can be
// exercised without a live cluster — the same seam SelectProvider uses. Never reassigned
// outside tests.
var CountOutstanding = countOutstanding

// countOutstanding reports how many escrow snapshots this cluster already holds. A
// listing failure is returned, never treated as zero: the cap is a safety bound, and
// assuming the best about a number you could not read is how bounds get bypassed.
func countOutstanding(ctx context.Context, cfg *common.Config) (int, error) {
	list, err := k8s.GetClients().Dynamic.Resource(k8s.VolumeSnapshotGVR).Namespace(cfg.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: ClusterSelector(cfg.ClusterName)})
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// SelectProvider is Select behind a package variable so the fail-closed contract — a
// refusal or an unproven capture must abort the caller — is testable without standing up
// a storage backend. Never reassigned outside tests.
var SelectProvider = Select

// RecoverySet is a selected provider plus the proven space headroom for capturing a set
// of PVCs. Preparing and capturing are separate so a caller can report a plan and stop
// (--dry-run) without having written anything.
type RecoverySet struct {
	provider  EscrowProvider
	set       []string
	operation string // refusal prefix, e.g. "restore", "bootstrap"
	estimate  int64
	available int64
}

// Prepare selects the provider FAIL-CLOSED and proves there is room for the capture
// BEFORE anything is written — "requires X, only Y available", rather than discovering a
// full repository halfway through an escrow.
func Prepare(ctx context.Context, cfg *common.Config, operation string, set []string,
	usedBytes map[string]int64) (*RecoverySet, error) {

	if len(set) == 0 {
		return nil, fmt.Errorf("%s REFUSED: empty recovery set — nothing to make reversible", operation)
	}
	prov, err := SelectProvider(ctx, cfg, escrowKind(operation), set)
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: %w", operation, err)
	}
	// The count bound, which is what actually guards a copy-on-write provider.
	n, cerr := CountOutstanding(ctx, cfg)
	if cerr != nil {
		return nil, fmt.Errorf("%s REFUSED: cannot count the escrows %s already holds: %w", operation, cfg.ClusterName, cerr)
	}
	if n >= maxOutstandingEscrows {
		return nil, fmt.Errorf("%s REFUSED: %s already holds %d unreleased escrows (cap %d) — release them with "+
			"`hasteward backup prune -t escrow` before taking another; an escrow store that only grows protects nothing",
			operation, cfg.ClusterName, n, maxOutstandingEscrows)
	}
	est := prov.EstimateCaptureBytes(set, usedBytes)
	avail, err := prov.AvailableBytes()
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: cannot determine escrow free space: %w", operation, err)
	}
	if est+reserveBytes > avail {
		return nil, fmt.Errorf("%s REFUSED: escrow (%s) requires %s + %s reserve, only %s available in the escrow store",
			operation, prov.Name(), output.FormatBytes(est), output.FormatBytes(reserveBytes), output.FormatBytes(avail))
	}
	return &RecoverySet{provider: prov, set: set, operation: operation, estimate: est, available: avail}, nil
}

// Describe renders the chosen provider and the space it needs, for both the dry-run
// preview and the live run — so what a preview promises and what a run does are the same
// line of text.
func (r *RecoverySet) Describe() string {
	return fmt.Sprintf("%s — ~%s needed, %s available",
		r.provider.Name(), output.FormatBytes(r.estimate), output.FormatBytes(r.available))
}

// Capture captures the recovery set and PROVES it restorable. Fail-closed at both
// stages: an unproven escrow is indistinguishable from none, so either failure aborts.
func (r *RecoverySet) Capture(ctx context.Context) ([]EscrowRef, error) {
	refs, err := r.provider.Capture(ctx, r.set)
	if err != nil {
		return nil, fmt.Errorf("%s REFUSED: escrow capture failed: %w", r.operation, err)
	}
	if err := r.provider.Verify(ctx, refs); err != nil {
		return nil, fmt.Errorf("%s REFUSED: escrow verification failed (rollback unproven): %w", r.operation, err)
	}
	return refs, nil
}

// Gate is the enforced pre-mutation escrow that every destructive operation must pass.
//
// It is the mechanism behind the tool's central promise: a HASteward operation that can
// lose data takes a proven rollback first, or it does not run. There are exactly two
// outcomes — a verified escrow, or a refusal — and one explicit way out, --no-escrow,
// which is the operator stating they accept the loss. "Could not escrow" must never
// quietly become "proceeded anyway", because the operations this guards are the ones
// whose damage cannot be undone.
//
// Call it IMMEDIATELY before the first mutation, and never on a dry run: it writes.
func Gate(ctx context.Context, cfg *common.Config, operation string, set []string,
	usedBytes map[string]int64) ([]EscrowRef, error) {

	if cfg.NoEscrow {
		common.WarnLog("%s: --no-escrow — proceeding with NO rollback for %v. Data lost here cannot be recovered.", operation, set)
		return nil, nil
	}
	rs, err := Prepare(ctx, cfg, operation, set, usedBytes)
	if err != nil {
		return nil, err
	}
	output.Field("Escrow", rs.Describe())
	refs, err := rs.Capture(ctx)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		common.InfoLog("%s escrow: %s %s (pvc %s)", operation, ref.Provider, ref.ID, ref.PVC)
	}
	return refs, nil
}

// UsedBytesByPVC pulls each recovery-set PVC's used bytes from triage's DiskStats, for
// the space estimate. Missing/unknown disk → absent (the estimate is a floor, never a
// fabricated number).
func UsedBytesByPVC(t *model.TriageResult, set []string) map[string]int64 {
	out := make(map[string]int64, len(set))
	if t == nil {
		return out
	}
	want := make(map[string]bool, len(set))
	for _, p := range set {
		want[p] = true
	}
	for _, a := range t.Assessments {
		if want[a.Pod] && a.Disk != nil {
			out[a.Pod] = a.Disk.UsedBytes
		}
	}
	return out
}

// PVCNamer is the provider's pod→datadir-PVC mapping, taken structurally so this
// package needs no dependency on provider. Both engines already implement it:
// CNPG names an instance's PVC after the pod, Galera uses storage-<pod>.
type PVCNamer interface {
	DataPVCName(pod string) string
}

// PVCsFor maps instance pods to the PVCs an escrow must capture, via the provider that
// already knows the convention. Deriving names here instead is what previously left
// Galera with no escrow path at all, on the false premise that its PVCs were not
// derivable — the mapping was on the EngineProvider interface the whole time.
func PVCsFor(n PVCNamer, pods []string) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, n.DataPVCName(p))
	}
	return out
}

// ReadPod is the pod read behind DiscoverPVCs, a package variable so the union contract
// is testable without a cluster — the same seam SelectProvider and CountOutstanding use.
// Never reassigned outside tests.
var ReadPod = readPod

func readPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	return k8s.GetClients().Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
}

// DiscoverPVCs maps instance pods to the PVCs an escrow must capture: the UNION of the
// provider's naming rule and every PersistentVolumeClaim in each pod's own volume list.
// The naming rule alone under-captures a multi-PVC instance — CNPG with walStorage adds
// <pod>-wal, a Galera node carries a storage-/galera- pair — and an escrow that omits
// one of a lineage's volumes is not a rollback, it is a trap a later --force springs.
// The union's other half covers the opposite gap: a pod object that no longer exists has
// no volume list to read, and the naming rule is the only recovery set it has left.
//
// Fail-closed on any pod read error other than NotFound: assuming the best about a
// volume list you could not read is how an escrow under-captures.
func DiscoverPVCs(ctx context.Context, ns string, n PVCNamer, pods []string) ([]string, error) {
	seen := make(map[string]bool, len(pods))
	out := make([]string, 0, len(pods))
	add := func(pvc string) {
		if pvc != "" && !seen[pvc] {
			seen[pvc] = true
			out = append(out, pvc)
		}
	}
	for _, pod := range pods {
		add(n.DataPVCName(pod))
		p, err := ReadPod(ctx, ns, pod)
		if apierrors.IsNotFound(err) {
			continue // pod object gone: the naming rule above is all that is left
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read pod %s/%s to discover its PVCs: %w", ns, pod, err)
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				add(v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	return out, nil
}
