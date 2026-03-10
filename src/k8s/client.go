package k8s

import (
	"fmt"
	"sync"

	"gitlab.prplanit.com/precisionplanit/hasteward/src/common"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// PatchOptions is a reusable empty PatchOptions for dynamic client patches.
var PatchOptions = metav1.PatchOptions{}

// Well-known GVRs for CRDs we interact with.
var (
	CNPGClusterGVR = schema.GroupVersionResource{
		Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters",
	}
	CNPGBackupGVR = schema.GroupVersionResource{
		Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups",
	}
	MariaDBGVR = schema.GroupVersionResource{
		Group: "k8s.mariadb.com", Version: "v1alpha1", Resource: "mariadbs",
	}
)

// SecretGVK is the GVK for core/v1 Secrets (used by unstructured lookups).
var SecretGVK = schema.GroupVersionKind{
	Group: "", Version: "v1", Kind: "Secret",
}

// ListOptions returns a default metav1.ListOptions.
func ListOptions() metav1.ListOptions {
	return metav1.ListOptions{}
}

// Clients holds the initialized Kubernetes client set.
type Clients struct {
	Clientset  kubernetes.Interface
	Dynamic    dynamic.Interface
	RestConfig *rest.Config
}

var (
	clients     *Clients
	clientsOnce sync.Once
	clientsErr  error
)

// Init initializes the Kubernetes clients. Safe to call multiple times;
// only the first call performs initialization. Pass kubeconfig="" to use
// standard resolution (in-cluster → KUBECONFIG → ~/.kube/config).
func Init(kubeconfig string) (*Clients, error) {
	clientsOnce.Do(func() {
		var cfg *rest.Config

		if kubeconfig == "" {
			kubeconfig = common.EnvRaw("KUBECONFIG", "")
		}

		if kubeconfig != "" {
			cfg, clientsErr = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			// Try in-cluster first, fall back to default kubeconfig
			cfg, clientsErr = rest.InClusterConfig()
			if clientsErr != nil {
				loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
				configOverrides := &clientcmd.ConfigOverrides{}
				cfg, clientsErr = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
					loadingRules, configOverrides).ClientConfig()
			}
		}
		if clientsErr != nil {
			clientsErr = fmt.Errorf("failed to build kubeconfig: %w", clientsErr)
			return
		}

		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			clientsErr = fmt.Errorf("failed to create clientset: %w", err)
			return
		}

		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			clientsErr = fmt.Errorf("failed to create dynamic client: %w", err)
			return
		}

		clients = &Clients{
			Clientset:  cs,
			Dynamic:    dyn,
			RestConfig: cfg,
		}
	})
	return clients, clientsErr
}

// GetClients returns the cached clients. Must call Init first.
func GetClients() *Clients {
	return clients
}

// ServiceAccountFromPods returns the ServiceAccountName from the first pod
// with one set, falling back to "default". This lets spawned probe/heal pods
// inherit the workload's own SA rather than assuming a specific name exists
// in the target namespace.
func ServiceAccountFromPods(pods []corev1.Pod) string {
	for _, p := range pods {
		if p.Spec.ServiceAccountName != "" {
			return p.Spec.ServiceAccountName
		}
	}
	return "default"
}

// --- Unstructured field helpers ---

// GetNestedString extracts a string from an unstructured object at the given path.
func GetNestedString(obj *unstructured.Unstructured, fields ...string) string {
	val, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return val
}

// GetNestedInt64 extracts an int64 from an unstructured object at the given path.
func GetNestedInt64(obj *unstructured.Unstructured, fields ...string) int64 {
	val, found, err := unstructured.NestedInt64(obj.Object, fields...)
	if err != nil || !found {
		return 0
	}
	return val
}

// GetNestedBool extracts a bool from an unstructured object at the given path.
func GetNestedBool(obj *unstructured.Unstructured, fields ...string) bool {
	val, found, err := unstructured.NestedBool(obj.Object, fields...)
	if err != nil || !found {
		return false
	}
	return val
}

// GetNestedSlice extracts a slice from an unstructured object at the given path.
func GetNestedSlice(obj *unstructured.Unstructured, fields ...string) []interface{} {
	val, found, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil || !found {
		return nil
	}
	return val
}

// GetNestedMap extracts a map from an unstructured object at the given path.
func GetNestedMap(obj *unstructured.Unstructured, fields ...string) map[string]interface{} {
	val, found, err := unstructured.NestedMap(obj.Object, fields...)
	if err != nil || !found {
		return nil
	}
	return val
}

// GetNestedFieldCopy extracts a value from an unstructured object at the given path.
func GetNestedFieldCopy(obj *unstructured.Unstructured, fields ...string) (interface{}, bool) {
	val, found, err := unstructured.NestedFieldCopy(obj.Object, fields...)
	if err != nil || !found {
		return nil, false
	}
	return val, true
}
