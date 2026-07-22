package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const (
	indexApplicationAuthFlowHandle         = "spec.authFlowHandle"
	indexApplicationRegistrationFlowHandle = "spec.registrationFlowHandle"
	indexApplicationRecoveryFlowHandle     = "spec.recoveryFlowHandle"
	indexApplicationThemeHandle            = "spec.themeHandle"
	indexApplicationOUHandle               = "spec.ouHandle"
	indexRoleOUHandle                      = "spec.ouHandle"
	indexUserTypeOUHandle                  = "spec.ouHandle"
	indexUserOUHandle                      = "spec.ouHandle"
	indexResourceServerOUHandle            = "spec.ouHandle"
	indexGroupOUHandle                     = "spec.ouHandle"
)

func refValue(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// SetupReferenceIndexes registers a field index for every handle field in the API.
// Must be called once, before the manager starts.
func SetupReferenceIndexes(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()

	if err := indexer.IndexField(ctx, &appsv1alpha1.Application{}, indexApplicationAuthFlowHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Application).Spec.AuthFlowHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Application{}, indexApplicationRegistrationFlowHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Application).Spec.RegistrationFlowHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Application{}, indexApplicationRecoveryFlowHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Application).Spec.RecoveryFlowHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Application{}, indexApplicationThemeHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Application).Spec.ThemeHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Application{}, indexApplicationOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Application).Spec.OUHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Role{}, indexRoleOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Role).Spec.OUHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.UserType{}, indexUserTypeOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.UserType).Spec.OUHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.User{}, indexUserOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.User).Spec.OUHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.ResourceServer{}, indexResourceServerOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.ResourceServer).Spec.OUHandle)
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &appsv1alpha1.Group{}, indexGroupOUHandle, func(o client.Object) []string {
		return refValue(o.(*appsv1alpha1.Group).Spec.OUHandle)
	}); err != nil {
		return err
	}

	return nil
}

// enqueueReferencing returns a handler.MapFunc that, given a changed object,
// extracts a lookup value via getValue (e.g. the object's handle), then finds
// every object of the type produced by newList whose indexKey field equals that
// value, and returns a reconcile.Request for each.
func enqueueReferencing(c client.Client, indexKey string, newList func() client.ObjectList, getValue func(client.Object) string) func(ctx context.Context, obj client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		val := getValue(obj)
		if val == "" {
			return nil
		}
		list := newList()
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{indexKey: val}); err != nil {
			return nil
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(items))
		for _, it := range items {
			co, ok := it.(client.Object)
			if !ok {
				continue
			}
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: co.GetName(), Namespace: co.GetNamespace()},
			})
		}
		return reqs
	}
}

// deterministicUUIDv7 derives a stable UUIDv7 from a CR's creation timestamp and
// Kubernetes UID. ThunderID's resource loader requires UUIDv7 format — Kubernetes
// assigns UUIDv4 UIDs, which ThunderID silently rejects. Using creation timestamp
// as the 48-bit time field and a SHA-256 hash of the UID for the random fields
// gives a result that is both valid UUIDv7 and stable across reconciles.
func deterministicUUIDv7(meta metav1.ObjectMeta) string {
	tsMs := meta.CreationTimestamp.UnixMilli()
	h := sha256.Sum256([]byte(string(meta.UID)))

	var b [16]byte
	b[0] = byte(tsMs >> 40)
	b[1] = byte(tsMs >> 32)
	b[2] = byte(tsMs >> 24)
	b[3] = byte(tsMs >> 16)
	b[4] = byte(tsMs >> 8)
	b[5] = byte(tsMs)
	b[6] = 0x70 | (h[0] & 0x0F) // version nibble = 7
	b[7] = h[1]
	b[8] = 0x80 | (h[2] & 0x3F) // variant = 10xx
	copy(b[9:], h[3:10])

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
