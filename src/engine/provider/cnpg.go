package provider

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func init() {
	RegisterProvider("cnpg", func() EngineProvider { return &CNPGProvider{} })
}

// CNPGProvider holds validated state for a CNPG (CloudNativePG PostgreSQL) engine.
type CNPGProvider struct {
	cfg     *common.Config
	cluster *unstructured.Unstructured

	// Parsed from Cluster CR
	clusterSpec        map[string]interface{}
	clusterStatus      map[string]interface{}
	clusterAnnotations map[string]interface{}
	instances          int64
	fencedInstances    []string
}

func (p *CNPGProvider) Name() string           { return "cnpg" }
func (p *CNPGProvider) Config() *common.Config { return p.cfg }

// PodSelector is the label selector matching every pod of this cluster.
func (p *CNPGProvider) PodSelector() string { return "cnpg.io/cluster=" + p.cfg.ClusterName }

// DataPVCName returns the datadir PVC name for a pod (CNPG: the PVC == pod name).
func (p *CNPGProvider) DataPVCName(pod string) string { return pod }

// NewCNPGProviderForTest builds a provider with injected state, bypassing Validate
// (no live cluster needed). TEST-ONLY — never call from production code.
func NewCNPGProviderForTest(cfg *common.Config, instances int64, cluster *unstructured.Unstructured) *CNPGProvider {
	p := &CNPGProvider{cfg: cfg, instances: instances}
	if cluster != nil {
		p.SetCluster(cluster)
	}
	return p
}

func (p *CNPGProvider) Cluster() *unstructured.Unstructured { return p.cluster }
func (p *CNPGProvider) Spec() map[string]interface{}        { return p.clusterSpec }
func (p *CNPGProvider) Status() map[string]interface{}      { return p.clusterStatus }
func (p *CNPGProvider) Annotations() map[string]interface{} { return p.clusterAnnotations }
func (p *CNPGProvider) Instances() int64                    { return p.instances }
func (p *CNPGProvider) FencedInstances() []string           { return p.fencedInstances }

// SetCluster replaces the cached CR state (used after re-fetch during repair).
func (p *CNPGProvider) SetCluster(obj *unstructured.Unstructured) {
	p.cluster = obj
	p.clusterSpec = k8s.GetNestedMap(obj, "spec")
	p.clusterStatus = k8s.GetNestedMap(obj, "status")
	p.clusterAnnotations = k8s.GetNestedMap(obj, "metadata", "annotations")
	p.fencedInstances = ParseFencedInstances(p.clusterAnnotations)
}

func (p *CNPGProvider) Validate(ctx context.Context, cfg *common.Config) error {
	p.cfg = cfg

	c := k8s.GetClients()
	if c == nil {
		return fmt.Errorf("kubernetes clients not initialized")
	}

	obj, err := c.Dynamic.Resource(k8s.CNPGClusterGVR).Namespace(cfg.Namespace).Get(
		ctx, cfg.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("CNPG Cluster %s/%s not found: %w", cfg.Namespace, cfg.ClusterName, err)
	}

	p.SetCluster(obj)
	p.instances = k8s.GetNestedInt64(obj, "spec", "instances")

	return nil
}

// ParseFencedInstances extracts the fenced instances list from cluster annotations.
func ParseFencedInstances(annotations map[string]interface{}) []string {
	raw, ok := annotations["cnpg.io/fencedInstances"]
	if !ok {
		return nil
	}
	s, ok := raw.(string)
	if !ok || s == "" || s == "[]" {
		return nil
	}
	return parseJSONStringArray(s)
}

// parseJSONStringArray is a lightweight parser for JSON string arrays
// to avoid pulling in encoding/json for this one use.
func parseJSONStringArray(s string) []string {
	var result []string
	inQuote := false
	var current []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inQuote:
			inQuote = true
			current = current[:0]
		case c == '"' && inQuote:
			inQuote = false
			result = append(result, string(current))
		case inQuote:
			current = append(current, c)
		}
	}
	return result
}
