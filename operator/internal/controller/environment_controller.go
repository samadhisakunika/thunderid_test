package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// EnvironmentReconciler is a one-shot trigger, not an ongoing controller for a persistent
// object: it syncs spec.env into a Secret of the same name, then deletes the Environment that
// triggered it — see Environment's own doc comment for why. Updating values later means
// applying a new Environment with the same name, not editing an existing one (it won't still
// exist to edit).
type EnvironmentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.thunderid.io,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;patch

func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	obj := &appsv1alpha1.Environment{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.syncSecret(ctx, obj); err != nil {
		log.Error(err, "failed to sync Secret from Environment")
		r.Recorder.Event(obj, corev1.EventTypeWarning, "SyncFailed", err.Error())
		obj.Status.Phase = "Error"
		obj.Status.Synced = false
		obj.Status.SecretName = obj.Name
		_ = r.Status().Update(ctx, obj)
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(obj, corev1.EventTypeNormal, "SecretSynced",
		"Secret %q synced from this Environment's spec.env; deleting the Environment now that "+
			"the Secret holds its content — apply a new Environment with this same name to change "+
			"the values later.", obj.Name)

	// Deliberately deleted, not left Ready: the Secret this just wrote to must be the only
	// lasting copy of these values — see Environment's own doc comment. Only reached after
	// syncSecret succeeds; a failed sync above leaves this object in place with an Error status
	// instead, so it's inspectable.
	return ctrl.Result{}, r.Delete(ctx, obj)
}

// syncSecret creates or updates a Secret named after obj holding obj.Spec.Env verbatim as
// stringData. Deliberately no ctrl.SetControllerReference: unlike every other generated object
// in this operator, this Secret must outlive its trigger (Reconcile deletes obj right after this
// succeeds), so it cannot be an owned child — an owner reference here would make that deletion
// cascade-delete the Secret too.
func (r *EnvironmentReconciler) syncSecret(ctx context.Context, obj *appsv1alpha1.Environment) error {
	// Built as Data (not StringData) even here: StringData only ever merges into whatever
	// Data a Secret already has (see the update path's comment below) — using it on create too
	// would be an inconsistency waiting to bite if this function's paths ever get reordered.
	data := make(map[string][]byte, len(obj.Spec.Env))
	for k, v := range obj.Spec.Env {
		data[k] = []byte(v)
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, secret)
	if errors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      obj.Name,
				Namespace: obj.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		return r.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, latest); err != nil {
			return err
		}
		// Data, not StringData: StringData only ever merges its keys into whatever Data the
		// Secret already has — a key present in an earlier Environment but dropped from this
		// one would linger forever. Setting Data directly makes the Secret's content an exact
		// match for spec.env every time, the same way a declarative resource is supposed to
		// behave (confirmed live — a reapplied Environment missing a previously-set key left
		// that key in place until this fix).
		latest.Data = data
		latest.StringData = nil
		return r.Update(ctx, latest)
	})
}

func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Environment{}).
		Named("environment").
		Complete(r)
}
