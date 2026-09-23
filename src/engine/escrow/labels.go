package escrow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// The discovery vocabulary stamped on every escrow object, wherever it is taken
// from. It is exported because retention has to find escrows without knowing
// which code path captured them, and because an operator doing a manual restore
// months later needs one set of labels to search, not one per call site.
const (
	LabelPrefix = "hasteward.prplanit.com/"

	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelEscrow    = LabelPrefix + "escrow"   // "true" on every escrow object
	LabelCluster   = LabelPrefix + "cluster"  // owning database cluster
	LabelInstance  = LabelPrefix + "instance" // the PVC/instance escrowed
	LabelRunID     = LabelPrefix + "run-id"   // the operation run that captured it
	LabelKind      = LabelPrefix + "kind"     // which operation captured it

	// RFC3339 contains ':', which is not legal in a label value, so capture time
	// is an annotation. Retention falls back to creationTimestamp when it is absent.
	AnnCapturedAt = LabelPrefix + "captured-at"

	ManagedByValue = "hasteward"
)

// Escrow kinds. The kind is recorded rather than inferred from the object name so
// retention can report what it is about to release in the operator's own terms.
const (
	KindSplitBrain = "split-brain" // escrow taken before a split-brain repair
	KindDeadlock   = "deadlock"    // escrow taken before an in-place WAL replay
)

// Labels returns the discovery labels for one escrowed PVC.
func Labels(cluster, instance, runID, kind string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelEscrow:    "true",
		LabelCluster:   cluster,
		LabelInstance:  instance,
		LabelRunID:     runID,
		LabelKind:      kind,
	}
}

// LabelsAsInterface is Labels shaped for an unstructured object's metadata.
func LabelsAsInterface(cluster, instance, runID, kind string) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range Labels(cluster, instance, runID, kind) {
		out[k] = v
	}
	return out
}

// CapturedAtAnnotation stamps the capture time.
func CapturedAtAnnotation(t time.Time) map[string]interface{} {
	return map[string]interface{}{AnnCapturedAt: t.UTC().Format(time.RFC3339)}
}

// ClusterSelector matches every escrow belonging to one cluster. Retention uses it
// so it can never see, let alone release, a snapshot some other tool created.
func ClusterSelector(cluster string) string {
	return fmt.Sprintf("%s=true,%s=%s", LabelEscrow, LabelCluster, cluster)
}

// NewRunID mints the identifier that groups every PVC escrowed by one run.
func NewRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
