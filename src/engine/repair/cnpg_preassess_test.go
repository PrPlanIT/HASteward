package repair

import (
	"context"
	"strings"
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakePreassessTriager returns a canned TriageResult so PreAssess's gate logic can be
// tested without a live cluster (triage.Run only calls Collect then Analyze).
type fakePreassessTriager struct{ result *model.TriageResult }

func (f *fakePreassessTriager) Name() string                    { return "fake" }
func (f *fakePreassessTriager) Collect(ctx context.Context) error { return nil }
func (f *fakePreassessTriager) Analyze(ctx context.Context) (*model.TriageResult, error) {
	return f.result, nil
}

func preassessRepair(t *testing.T, unwedge bool, result *model.TriageResult) *cnpgRepair {
	t.Helper()
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]interface{}{"name": "pg", "namespace": "ns"},
		"spec":     map[string]interface{}{"instances": int64(3)},
	}}
	p := provider.NewCNPGProviderForTest(&common.Config{ClusterName: "pg", Namespace: "ns", Unwedge: unwedge}, 3, cluster)
	return &cnpgRepair{p: p, triager: &fakePreassessTriager{result: result}}
}

func blockedDeadlock(authorityStatus string) *model.TriageResult {
	return &model.TriageResult{
		AuthorityStatus: authorityStatus,
		Recovery: &model.Recovery{
			Blocked:    true,
			Reason:     "disk_full_disposable_replica",
			Authority:  "pg-1",
			Disposable: []string{"pg-2"},
		},
	}
}

// Without --unwedge the breaker is inert: it returns (nil, nil) and never triages,
// so the normal repair path is unchanged.
func TestPreAssess_InertWithoutUnwedge(t *testing.T) {
	r := preassessRepair(t, false, nil)
	got, err := r.PreAssess(context.Background())
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil) when not unwedging; got (%v, %v)", got, err)
	}
}

// --unwedge but no breakable deadlock → defers to normal repair (result, nil).
func TestPreAssess_NoDeadlockDefers(t *testing.T) {
	res := &model.TriageResult{AuthorityStatus: "unambiguous"} // Recovery nil
	r := preassessRepair(t, true, res)
	got, err := r.PreAssess(context.Background())
	if err != nil {
		t.Fatalf("no deadlock should defer, not error: %v", err)
	}
	if got != res {
		t.Fatalf("should return the triage result unchanged")
	}
}

// THE breaker safety gate: a disk-full deadlock is present, but authority is
// ambiguous → REFUSE. The breaker must never rm -rf a datadir while the authority is
// unproven.
func TestPreAssess_RefusesWhenAuthorityAmbiguous(t *testing.T) {
	r := preassessRepair(t, true, blockedDeadlock("ambiguous"))
	_, err := r.PreAssess(context.Background())
	if err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("a deadlock with ambiguous authority must be REFUSED; got %v", err)
	}
}
