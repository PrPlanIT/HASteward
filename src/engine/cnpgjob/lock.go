package cnpgjob

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/PrPlanIT/HASteward/src/common"
	"github.com/PrPlanIT/HASteward/src/k8s"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Cluster-scoped operation lock tuning. The duration is intentionally short and the
// lease is renewed continuously in the background, so a crashed holder's lock frees
// itself within leaseDuration rather than blocking the cluster until a human intervenes.
const (
	leaseDuration = 60 * time.Second
	leaseRenew    = 20 * time.Second
)

// AcquireClusterLock takes an EXCLUSIVE HASteward operation lock on a CNPG cluster via a
// coordination.k8s.io/v1 Lease named "hasteward-<cluster>". Repair, unwedge and prune-WAL
// must serialize across invocations because they all toggle the same cluster-scoped
// cnpg.io/reconciliationLoop switch and perform read-modify-write updates to the shared
// cnpg.io/fencedInstances annotation — two concurrent operations corrupt each other's
// ownership window (operation A's reconcile-restore silently re-enables the operator
// while operation B is mid-handoff).
//
// It is a single, non-blocking attempt: on success it returns a release func and renews
// the lease in the background until release is called; if another, non-expired holder
// owns the lease it returns an error naming the holder so the operator can retry later.
// A crashed holder's lease expires after leaseDuration and is then taken over.
func AcquireClusterLock(ctx context.Context, ns, cluster, operation string) (func(), error) {
	leases := k8s.GetClients().Clientset.CoordinationV1().Leases(ns)
	name := "hasteward-" + cluster
	identity := leaseIdentity(operation)
	now := metav1.NewMicroTime(time.Now())
	dur := int32(leaseDuration.Seconds())

	existing, err := leases.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, cErr := leases.Create(ctx, newLease(name, identity, now, dur), metav1.CreateOptions{}); cErr != nil {
			if apierrors.IsAlreadyExists(cErr) {
				return nil, fmt.Errorf("another HASteward operation is acquiring the lock on cluster %s/%s; refusing — retry shortly", ns, cluster)
			}
			return nil, fmt.Errorf("failed to create cluster lock %s: %w", name, cErr)
		}
	case err != nil:
		return nil, fmt.Errorf("failed to read cluster lock %s: %w", name, err)
	default:
		if holder := leaseHeldByOther(existing, identity); holder != "" {
			return nil, fmt.Errorf("another HASteward operation holds cluster %s/%s (holder %s); refusing — retry when it completes", ns, cluster, holder)
		}
		// Free (expired or already ours): take it over. A racing taker loses on the
		// resourceVersion conflict and is refused.
		existing.Spec.HolderIdentity = &identity
		existing.Spec.AcquireTime = &now
		existing.Spec.RenewTime = &now
		existing.Spec.LeaseDurationSeconds = &dur
		if _, uErr := leases.Update(ctx, existing, metav1.UpdateOptions{}); uErr != nil {
			if apierrors.IsConflict(uErr) {
				return nil, fmt.Errorf("another HASteward operation is acquiring the lock on cluster %s/%s; refusing — retry shortly", ns, cluster)
			}
			return nil, fmt.Errorf("failed to take cluster lock %s: %w", name, uErr)
		}
	}

	common.InfoLog("Acquired cluster operation lock %s/%s (holder %s)", ns, cluster, identity)

	stop := make(chan struct{})
	go renewLease(ns, name, identity, stop)

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		close(stop)
		// Best-effort delete on a DETACHED context so the lock is freed even if the
		// operation's ctx was cancelled; if it fails the lease simply expires.
		dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if dErr := leases.Delete(dctx, name, metav1.DeleteOptions{}); dErr != nil && !apierrors.IsNotFound(dErr) {
			common.WarnLog("failed to release cluster lock %s: %v (it will expire in %s)", name, dErr, leaseDuration)
			return
		}
		common.InfoLog("Released cluster operation lock %s/%s", ns, cluster)
	}
	return release, nil
}

func leaseIdentity(operation string) string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s@%s:%d:%d", operation, host, os.Getpid(), time.Now().UnixNano())
}

func newLease(name, identity string, now metav1.MicroTime, dur int32) *coordv1.Lease {
	return &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: coordv1.LeaseSpec{
			HolderIdentity:       &identity,
			AcquireTime:          &now,
			RenewTime:            &now,
			LeaseDurationSeconds: &dur,
		},
	}
}

// leaseHeldByOther returns the holder identity if the lease is currently held by a
// DIFFERENT, non-expired holder; "" if it is free (expired or unheld) or already ours.
func leaseHeldByOther(l *coordv1.Lease, identity string) string {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || *l.Spec.HolderIdentity == identity {
		return ""
	}
	if l.Spec.RenewTime != nil && l.Spec.LeaseDurationSeconds != nil {
		expiry := l.Spec.RenewTime.Time.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second)
		if time.Now().After(expiry) {
			return "" // expired — free to take over
		}
	}
	return *l.Spec.HolderIdentity
}

// renewLease keeps our hold fresh until release closes stop. It runs on a detached
// context so it survives caller-ctx cancellation (the operation may legitimately outlive
// a cancelled request while it finishes cleaning up); only updates if we still hold it.
func renewLease(ns, name, identity string, stop <-chan struct{}) {
	leases := k8s.GetClients().Clientset.CoordinationV1().Leases(ns)
	ticker := time.NewTicker(leaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			l, err := leases.Get(dctx, name, metav1.GetOptions{})
			if err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == identity {
				now := metav1.NewMicroTime(time.Now())
				l.Spec.RenewTime = &now
				_, _ = leases.Update(dctx, l, metav1.UpdateOptions{})
			}
			cancel()
		}
	}
}
