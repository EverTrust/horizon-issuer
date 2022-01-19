/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	horizonv1alpha1 "gitlab.com/evertrust/horizon-cm/api/v1alpha1"
	issuerutil "gitlab.com/evertrust/horizon-cm/controllers/util"
)

const (
	issuerReadyConditionReason = "sample-issuer.IssuerController.Reconcile"
	defaultHealthCheckInterval = time.Minute
)

var (
	errGetAuthSecret        = errors.New("failed to get Secret containing Issuer credentials")
	errHealthCheckerBuilder = errors.New("failed to build the healthchecker")
	errHealthCheckerCheck   = errors.New("healthcheck failed")
)

// IssuerReconciler reconciles a Issuer object
type IssuerReconciler struct {
	client.Client
	Kind   string
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=horizon.k8s.evertrust.io,resources=issuers;clusterissuers,verbs=get;list;watch
// +kubebuilder:rbac:groups=horizon.k8s.evertrust.io,resources=issuers/status;clusterissuers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *IssuerReconciler) newIssuer() (client.Object, error) {
	issuerGVK := horizonv1alpha1.GroupVersion.WithKind(r.Kind)
	ro, err := r.Scheme.New(issuerGVK)
	if err != nil {
		return nil, err
	}
	return ro.(client.Object), nil
}

func (r *IssuerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := ctrl.LoggerFrom(ctx)

	issuer, err := r.newIssuer()
	if err != nil {
		log.Error(err, "Unrecognised issuer type")
		return ctrl.Result{}, nil
	}
	if err := r.Get(ctx, req.NamespacedName, issuer); err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			return ctrl.Result{}, fmt.Errorf("unexpected get error: %v", err)
		}
		log.Info("Not found. Ignoring.")
		return ctrl.Result{}, nil
	}

	issuerSpec, issuerStatus, err := issuerutil.GetSpecAndStatus(issuer)
	if err != nil {
		log.Error(err, "Unexpected error while getting issuer spec and status. Not retrying.")
		return ctrl.Result{}, nil
	}

	// Always attempt to update the Ready condition
	defer func() {
		if err != nil {
			issuerutil.SetReadyCondition(issuerStatus, horizonv1alpha1.ConditionFalse, issuerReadyConditionReason, err.Error())
		}
		if updateErr := r.Status().Update(ctx, issuer); updateErr != nil {
			err = utilerrors.NewAggregate([]error{err, updateErr})
			result = ctrl.Result{}
		}
	}()

	if ready := issuerutil.GetReadyCondition(issuerStatus); ready == nil {
		issuerutil.SetReadyCondition(issuerStatus, horizonv1alpha1.ConditionUnknown, issuerReadyConditionReason, "First seen")
		return ctrl.Result{}, nil
	}

	secretName := types.NamespacedName{
		Name: issuerSpec.AuthSecretName,
	}

	switch issuer.(type) {
	case *horizonv1alpha1.Issuer:
		secretName.Namespace = req.Namespace
	//case *horizonv1alpha1.ClusterIssuer:
	//	secretName.Namespace = r.ClusterResourceNamespace
	default:
		log.Error(fmt.Errorf("unexpected issuer type: %t", issuer), "Not retrying.")
		return ctrl.Result{}, nil
	}

	var secret corev1.Secret
	if err := r.Get(ctx, secretName, &secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("%w, secret name: %s, reason: %v", errGetAuthSecret, secretName, err)
	}

	//checker, err := r.HealthCheckerBuilder(issuerSpec, secret.Data)
	//if err != nil {
	//	return ctrl.Result{}, fmt.Errorf("%w: %v", errHealthCheckerBuilder, err)
	//}
	//
	//if err := checker.Check(); err != nil {
	//	return ctrl.Result{}, fmt.Errorf("%w: %v", errHealthCheckerCheck, err)
	//}

	issuerutil.SetReadyCondition(issuerStatus, horizonv1alpha1.ConditionTrue, issuerReadyConditionReason, "Success")
	return ctrl.Result{RequeueAfter: defaultHealthCheckInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IssuerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&horizonv1alpha1.Issuer{}).
		Complete(r)
}