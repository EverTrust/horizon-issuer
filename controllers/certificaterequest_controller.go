/*
Copyright 2020 The cert-manager Authors

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
	cmutil "github.com/jetstack/cert-manager/pkg/api/util"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	"gitlab.com/evertrust/horizon-go/requests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"net/url"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"

	issuerapi "gitlab.com/evertrust/horizon-cm/api/v1alpha1"
	issuerutil "gitlab.com/evertrust/horizon-cm/controllers/util"
	"gitlab.com/evertrust/horizon-go"
)

var (
	errIssuerRef      = errors.New("error interpreting issuerRef")
	errGetIssuer      = errors.New("error getting issuer")
	errIssuerNotReady = errors.New("issuer is not ready")
	errSignerBuilder  = errors.New("failed to build the signer")
	errSignerSign     = errors.New("failed to sign")
	errInvalidBaseUrl = errors.New("invalid base url")
	errUnknownHorizon = errors.New("horizon returned an error")
)

var requestIdAnnotation = "horizon.evertrust.io/request-id"

// CertificateRequestReconciler reconciles a CertificateRequest object
type CertificateRequestReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	ClusterResourceNamespace string
	Clock                    clock.Clock
	HorizonClient            horizon.Horizon
}

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *CertificateRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := ctrl.LoggerFrom(ctx)

	// Get the CertificateRequest
	var certificateRequest cmapi.CertificateRequest
	if err := r.Get(ctx, req.NamespacedName, &certificateRequest); err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			return ctrl.Result{}, fmt.Errorf("unexpected get error: %v", err)
		}
		log.Info("Not found. Ignoring.")
		return ctrl.Result{}, nil
	}

	// Ignore CertificateRequest if issuerRef doesn't match our group
	if certificateRequest.Spec.IssuerRef.Group != issuerapi.GroupVersion.Group {
		log.Info("Foreign group. Ignoring.", "group", certificateRequest.Spec.IssuerRef.Group)
		return ctrl.Result{}, nil
	}

	// Ignore CertificateRequest if it is already Ready
	if cmutil.CertificateRequestHasCondition(&certificateRequest, cmapi.CertificateRequestCondition{
		Type:   cmapi.CertificateRequestConditionReady,
		Status: cmmeta.ConditionTrue,
	}) {
		log.Info("CertificateRequest is Ready. Ignoring.")
		return ctrl.Result{}, nil
	}
	// Ignore CertificateRequest if it is already Failed
	if cmutil.CertificateRequestHasCondition(&certificateRequest, cmapi.CertificateRequestCondition{
		Type:   cmapi.CertificateRequestConditionReady,
		Status: cmmeta.ConditionFalse,
		Reason: cmapi.CertificateRequestReasonFailed,
	}) {
		log.Info("CertificateRequest is Failed. Ignoring.")
		return ctrl.Result{}, nil
	}
	// Ignore CertificateRequest if it already has a Denied Ready Reason
	if cmutil.CertificateRequestHasCondition(&certificateRequest, cmapi.CertificateRequestCondition{
		Type:   cmapi.CertificateRequestConditionReady,
		Status: cmmeta.ConditionFalse,
		Reason: cmapi.CertificateRequestReasonDenied,
	}) {
		log.Info("CertificateRequest already has a Ready condition with Denied Reason. Ignoring.")
		return ctrl.Result{}, nil
	}

	// We now have a CertificateRequest that belongs to us so we are responsible
	// for updating its Ready condition.
	setReadyCondition := func(status cmmeta.ConditionStatus, reason, message string) {
		cmutil.SetCertificateRequestCondition(
			&certificateRequest,
			cmapi.CertificateRequestConditionReady,
			status,
			reason,
			message,
		)
	}

	// Always attempt to update the Ready condition
	defer func() {
		if err != nil {
			setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, err.Error())
		}
		if updateErr := r.Status().Update(ctx, &certificateRequest); updateErr != nil {
			err = utilerrors.NewAggregate([]error{err, updateErr})
			result = ctrl.Result{}
		}
	}()

	// If CertificateRequest has been denied, mark the CertificateRequest as
	// Ready=Denied and set FailureTime if not already.
	if cmutil.CertificateRequestIsDenied(&certificateRequest) {
		log.Info("CertificateRequest has been denied yet. Marking as failed.")

		if certificateRequest.Status.FailureTime == nil {
			nowTime := metav1.NewTime(r.Clock.Now())
			certificateRequest.Status.FailureTime = &nowTime
		}

		message := "The CertificateRequest was denied by an approval controller"
		setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonDenied, message)
		return ctrl.Result{}, nil
	}

	// Ignore but log an error if the issuerRef.Kind is unrecognised
	issuerGVK := issuerapi.GroupVersion.WithKind(certificateRequest.Spec.IssuerRef.Kind)
	issuerRO, err := r.Scheme.New(issuerGVK)
	if err != nil {
		err = fmt.Errorf("%w: %v", errIssuerRef, err)
		log.Error(err, "Unrecognised kind. Ignoring.")
		setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, err.Error())
		return ctrl.Result{}, nil
	}
	issuer := issuerRO.(client.Object)
	// Create a Namespaced name for Issuer and a non-Namespaced name for ClusterIssuer
	issuerName := types.NamespacedName{
		Name: certificateRequest.Spec.IssuerRef.Name,
	}
	var secretNamespace string
	switch t := issuer.(type) {
	case *issuerapi.Issuer:
		issuerName.Namespace = certificateRequest.Namespace
		secretNamespace = certificateRequest.Namespace
		log = log.WithValues("issuer", issuerName)
	//case *issuerapi.ClusterIssuer:
	//	secretNamespace = r.ClusterResourceNamespace
	//	log = log.WithValues("clusterissuer", issuerName)
	default:
		err := fmt.Errorf("unexpected issuer type: %v", t)
		log.Error(err, "The issuerRef referred to a registered Kind which is not yet handled. Ignoring.")
		setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, err.Error())
		return ctrl.Result{}, nil
	}

	// Get the Issuer or ClusterIssuer
	if err := r.Get(ctx, issuerName, issuer); err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %v", errGetIssuer, err)
	}

	issuerSpec, issuerStatus, err := issuerutil.GetSpecAndStatus(issuer)
	if err != nil {
		log.Error(err, "Unable to get the IssuerStatus. Ignoring.")
		setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, err.Error())
		return ctrl.Result{}, nil
	}

	if !issuerutil.IsReady(issuerStatus) {
		return ctrl.Result{}, errIssuerNotReady
	}

	secretName := types.NamespacedName{
		Name:      issuerSpec.AuthSecretName,
		Namespace: secretNamespace,
	}

	var secret corev1.Secret

	err = r.Get(ctx, secretName, &secret)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w, secret name: %s, reason: %v", errGetAuthSecret, secretName, err)
	}

	// From here, we're ready to instantiate a Horizon client
	baseUrl, err := url.Parse(issuerSpec.URL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %v", errInvalidBaseUrl, err)
	}

	r.HorizonClient.Init(*baseUrl, string(secret.Data["username"]), string(secret.Data["password"]))

	// If CertificateRequest has not been approved, we should submit the request.
	if !cmutil.CertificateRequestIsApproved(&certificateRequest) {
		// If the request has been submitted to Horizon, pull info from Horizon
		if requestId, ok := certificateRequest.Annotations[requestIdAnnotation]; ok {
			log.Info("Pulling request " + requestId)
			request, err := r.HorizonClient.Requests.Get(requestId)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("%w: %v", errUnknownHorizon, err)
			}
			if request.Status == "completed" {
				cmutil.SetCertificateRequestCondition(
					&certificateRequest,
					cmapi.CertificateRequestConditionApproved,
					cmmeta.ConditionTrue,
					"horizon.evertrust.io",
					"Request approved on Horizon",
				)
				certificateRequest.Status.Certificate = []byte(request.Certificate.Certificate)
				setReadyCondition(cmmeta.ConditionTrue, cmapi.CertificateRequestReasonIssued, "Signed")
				return ctrl.Result{}, nil
			}

			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Minute,
			}, nil
		} else {
			// Else, submit the request
			request, err := r.HorizonClient.Requests.DecentralizedEnroll(
				issuerSpec.Profile,
				certificateRequest.Spec.Request,
				[]requests.LabelElement{},
			)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("%w: %v", errUnknownHorizon, err)
			}

			// Update the request with the Horizon request ID
			certificateRequest.Annotations[requestIdAnnotation] = request.Id
			if err := r.Update(ctx, &certificateRequest); err != nil {
				return ctrl.Result{}, fmt.Errorf("%w, secret name: %s, reason: %v", errGetAuthSecret, secretName, err)
			}
			setReadyCondition(cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "Submitted request to Horizon")

			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Minute,
			}, nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *CertificateRequestReconciler) updateCertificateRequest(certificateRequest *cmapi.CertificateRequest) (result ctrl.Result, err error) {
	request, err := r.HorizonClient.Requests.Get(certificateRequest.Annotations[requestIdAnnotation])
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %v", errUnknownHorizon, err)
	}

	// todo: finish this
	if request.Status == "completed" {
		cmutil.SetCertificateRequestCondition(
			certificateRequest,
			cmapi.CertificateRequestConditionApproved,
			cmmeta.ConditionTrue,
			"horizon.evertrust.io",
			"Request approved on Horizon",
		)
		certificateRequest.Status.Certificate = []byte(request.Certificate.Certificate)

		cmutil.SetCertificateRequestCondition(
			certificateRequest,
			cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionTrue,
			cmapi.CertificateRequestReasonIssued,
			"Signed",
		)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: time.Minute,
	}, nil
}

func (r *CertificateRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cmapi.CertificateRequest{}).
		Complete(r)
}