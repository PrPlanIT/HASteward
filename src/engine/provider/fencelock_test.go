package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PrPlanIT/HASteward/src/k8s"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func TestAcquireFenceLock(t *testing.T) {
	ctx := context.Background()

	t.Run("free cluster -> acquired, annotation stamped", func(t *testing.T) {
		dyn := fakeDynamic(mariadbCR(false))
		defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
		ok, err := testProvider().AcquireFenceLock(ctx, "triage-recover")
		if err != nil || !ok {
			t.Fatalf("want (true,nil), got (%v,%v)", ok, err)
		}
		got, _ := dyn.Resource(k8s.MariaDBGVR).Namespace("ns").Get(ctx, "c", metav1.GetOptions{})
		if v := got.GetAnnotations()[FenceLockAnnotation]; !strings.HasPrefix(v, "triage-recover@") {
			t.Fatalf("fence-lock annotation not stamped: %q", v)
		}
	})

	t.Run("suspended cluster -> not acquired", func(t *testing.T) {
		dyn := fakeDynamic(mariadbCR(true))
		defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
		if ok, err := testProvider().AcquireFenceLock(ctx, "x"); ok || err != nil {
			t.Fatalf("suspended -> want (false,nil), got (%v,%v)", ok, err)
		}
	})

	t.Run("fresh lock held -> not acquired", func(t *testing.T) {
		cr := mariadbCR(false)
		cr.SetAnnotations(map[string]string{FenceLockAnnotation: "bootstrap@" + time.Now().UTC().Format(time.RFC3339)})
		dyn := fakeDynamic(cr)
		defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
		if ok, _ := testProvider().AcquireFenceLock(ctx, "x"); ok {
			t.Fatal("a fresh lock must block acquisition")
		}
	})

	t.Run("stale lock (>1h) -> acquired", func(t *testing.T) {
		cr := mariadbCR(false)
		cr.SetAnnotations(map[string]string{FenceLockAnnotation: "bootstrap@" + time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339)})
		dyn := fakeDynamic(cr)
		defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
		if ok, err := testProvider().AcquireFenceLock(ctx, "x"); !ok || err != nil {
			t.Fatalf("stale lock -> want (true,nil), got (%v,%v)", ok, err)
		}
	})

	t.Run("CAS conflict -> not acquired, no error (lost the race)", func(t *testing.T) {
		dyn := fakeDynamic(mariadbCR(false))
		dyn.PrependReactor("update", "mariadbs", func(a clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "k8s.mariadb.com", Resource: "mariadbs"}, "c", fmt.Errorf("resourceVersion changed"))
		})
		defer k8s.SetClientsForTest(&k8s.Clients{Dynamic: dyn})()
		if ok, err := testProvider().AcquireFenceLock(ctx, "x"); ok || err != nil {
			t.Fatalf("conflict -> want (false,nil), got (%v,%v)", ok, err)
		}
	})
}
