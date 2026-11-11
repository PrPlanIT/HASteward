package provider

import (
	"testing"

	"github.com/PrPlanIT/HASteward/src/common"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Helper pods take the workload's ServiceAccount from the CR, so it must resolve with
// every pod down; "default" is refused in ambient-mesh namespaces.
func TestServiceAccount(t *testing.T) {
	cfg := &common.Config{ClusterName: "kimai-mariadb", Namespace: "ns"}

	withSA := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"serviceAccountName": "custom-sa"},
	}}
	noSA := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}

	cases := []struct {
		name string
		p    EngineProvider
		want string
	}{
		{"galera spec.serviceAccountName", NewGaleraProviderForTest(cfg, 3, withSA), "custom-sa"},
		{"galera defaults to the CR name", NewGaleraProviderForTest(cfg, 3, noSA), "kimai-mariadb"},
		{"galera with no CR loaded", NewGaleraProviderForTest(cfg, 3, nil), "kimai-mariadb"},
		{"cnpg is the cluster name", NewCNPGProviderForTest(cfg, 3, nil), "kimai-mariadb"},
	}
	for _, tc := range cases {
		if got := tc.p.ServiceAccount(); got != tc.want {
			t.Errorf("%s: ServiceAccount() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
