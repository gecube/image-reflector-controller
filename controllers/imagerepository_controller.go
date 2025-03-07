/*
Copyright 2020 The Flux authors

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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/predicates"

	imagev1 "github.com/fluxcd/image-reflector-controller/api/v1beta1"
	"github.com/fluxcd/image-reflector-controller/internal/registry/login"
)

// These are intended to match the keys used in e.g.,
// https://github.com/fluxcd/flux2/blob/main/cmd/flux/create_secret_helm.go,
// for consistency (and perhaps this will have its own flux create
// secret subcommand at some point).
const (
	ClientCert        = "certFile"
	ClientKey         = "keyFile"
	CACert            = "caFile"
	CosignObjectRegex = "^.*\\.sig$"
)

// ImageRepositoryReconciler reconciles a ImageRepository object
type ImageRepositoryReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	EventRecorder         kuberecorder.EventRecorder
	ExternalEventRecorder *events.Recorder
	MetricsRecorder       *metrics.Recorder
	Database              interface {
		DatabaseWriter
		DatabaseReader
	}
	login.ProviderOptions
}

type ImageRepositoryReconcilerOptions struct {
	MaxConcurrentReconciles int
}

type dockerConfig struct {
	Auths map[string]authn.AuthConfig
}

// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagerepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=image.toolkit.fluxcd.io,resources=imagerepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
func (r *ImageRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileStart := time.Now()

	// NB: In general, if an error is returned then controller-runtime
	// will requeue the request with back-off. In the following this
	// is usually made explicit by _also_ returning
	// `ctrl.Result{Requeue: true}`.

	var imageRepo imagev1.ImageRepository
	if err := r.Get(ctx, req.NamespacedName, &imageRepo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	defer r.recordSuspension(ctx, imageRepo)

	log := ctrl.LoggerFrom(ctx)

	// Add our finalizer if it does not exist.
	if !controllerutil.ContainsFinalizer(&imageRepo, imagev1.ImageRepositoryFinalizer) {
		patch := client.MergeFrom(imageRepo.DeepCopy())
		controllerutil.AddFinalizer(&imageRepo, imagev1.ImageRepositoryFinalizer)
		if err := r.Patch(ctx, &imageRepo, patch); err != nil {
			log.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}

	// If the object is under deletion, record the readiness, and remove our finalizer.
	if !imageRepo.ObjectMeta.DeletionTimestamp.IsZero() {
		r.recordReadinessMetric(ctx, &imageRepo)
		controllerutil.RemoveFinalizer(&imageRepo, imagev1.ImageRepositoryFinalizer)
		if err := r.Update(ctx, &imageRepo); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if imageRepo.Spec.Suspend {
		msg := "ImageRepository is suspended, skipping reconciliation"
		imagev1.SetImageRepositoryReadiness(
			&imageRepo,
			metav1.ConditionFalse,
			meta.SuspendedReason,
			msg,
		)
		if err := r.patchStatus(ctx, req, imageRepo.Status); err != nil {
			log.Error(err, "unable to update status")
			return ctrl.Result{Requeue: true}, err
		}
		log.Info(msg)
		return ctrl.Result{}, nil
	}

	// Record readiness metric
	defer r.recordReadinessMetric(ctx, &imageRepo)
	// Record reconciliation duration
	if r.MetricsRecorder != nil {
		objRef, err := reference.GetReference(r.Scheme, &imageRepo)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer r.MetricsRecorder.RecordDuration(*objRef, reconcileStart)
	}

	ref, err := parseImageReference(imageRepo.Spec.Image)
	if err != nil {
		imagev1.SetImageRepositoryReadiness(
			&imageRepo,
			metav1.ConditionFalse,
			imagev1.ImageURLInvalidReason,
			err.Error(),
		)
		if err := r.patchStatus(ctx, req, imageRepo.Status); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
		err := fmt.Errorf("Unable to parse image name: %s: %w", imageRepo.Spec.Image, err)
		r.event(ctx, imageRepo, events.EventSeverityError, err.Error())
		return ctrl.Result{Requeue: true}, err
	}

	// Set CanonicalImageName based on the parsed reference
	if c := ref.Context().String(); imageRepo.Status.CanonicalImageName != c {
		imageRepo.Status.CanonicalImageName = c
		if err = r.patchStatus(ctx, req, imageRepo.Status); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}

	// Throttle scans based on spec Interval
	ok, when, err := r.shouldScan(imageRepo, reconcileStart)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	if ok {
		reconcileErr := r.scan(ctx, &imageRepo, ref)
		if err := r.patchStatus(ctx, req, imageRepo.Status); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
		if reconcileErr != nil {
			r.event(ctx, imageRepo, events.EventSeverityError, reconcileErr.Error())
			return ctrl.Result{Requeue: true}, reconcileErr
		}
		// emit successful scan event
		if rc := apimeta.FindStatusCondition(imageRepo.Status.Conditions, imagev1.ReconciliationSucceededReason); rc != nil {
			r.event(ctx, imageRepo, events.EventSeverityInfo, rc.Message)
		}
	}

	log.Info(fmt.Sprintf("reconciliation finished in %s, next run in %s",
		time.Now().Sub(reconcileStart).String(),
		when.String(),
	))

	return ctrl.Result{RequeueAfter: when}, nil
}

func parseImageReference(url string) (name.Reference, error) {
	if s := strings.Split(url, "://"); len(s) > 1 {
		return nil, fmt.Errorf(".spec.image value should not start with URL scheme; remove '%s://'", s[0])
	}

	ref, err := name.ParseReference(url)
	if err != nil {
		return nil, err
	}

	imageName := strings.TrimPrefix(url, ref.Context().RegistryStr())
	if s := strings.Split(imageName, ":"); len(s) > 1 {
		return nil, fmt.Errorf(".spec.image value should not contain a tag; remove ':%s'", s[1])
	}

	return ref, nil
}

func (r *ImageRepositoryReconciler) scan(ctx context.Context, imageRepo *imagev1.ImageRepository, ref name.Reference) error {
	timeout := imageRepo.GetTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Configure authentication strategy to access the registry.
	var options []remote.Option
	var authSecret corev1.Secret
	var auth authn.Authenticator
	var authErr error
	if imageRepo.Spec.SecretRef != nil {
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: imageRepo.GetNamespace(),
			Name:      imageRepo.Spec.SecretRef.Name,
		}, &authSecret); err != nil {
			imagev1.SetImageRepositoryReadiness(
				imageRepo,
				metav1.ConditionFalse,
				imagev1.ReconciliationFailedReason,
				err.Error(),
			)
			return err
		}
		auth, authErr = authFromSecret(authSecret, ref)
	} else {
		// Use the registry provider options to attempt registry login.
		auth, authErr = login.NewManager().Login(ctx, imageRepo.Spec.Image, ref, r.ProviderOptions)
	}
	if authErr != nil {
		imagev1.SetImageRepositoryReadiness(
			imageRepo,
			metav1.ConditionFalse,
			imagev1.ReconciliationFailedReason,
			authErr.Error(),
		)
		return authErr
	}
	if auth != nil {
		options = append(options, remote.WithAuth(auth))
	}

	// Load any provided certificate.
	if imageRepo.Spec.CertSecretRef != nil {
		var certSecret corev1.Secret
		if imageRepo.Spec.SecretRef != nil && imageRepo.Spec.SecretRef.Name == imageRepo.Spec.CertSecretRef.Name {
			certSecret = authSecret
		} else {
			if err := r.Get(ctx, types.NamespacedName{
				Namespace: imageRepo.GetNamespace(),
				Name:      imageRepo.Spec.CertSecretRef.Name,
			}, &certSecret); err != nil {
				imagev1.SetImageRepositoryReadiness(
					imageRepo,
					metav1.ConditionFalse,
					imagev1.ReconciliationFailedReason,
					err.Error(),
				)
				return err
			}
		}

		tr, err := transportFromSecret(&certSecret)
		if err != nil {
			return err
		}
		options = append(options, remote.WithTransport(tr))
	}

	if imageRepo.Spec.ServiceAccountName != "" {

		serviceAccount := corev1.ServiceAccount{}
		// lookup service account
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: imageRepo.GetNamespace(),
			Name:      imageRepo.Spec.ServiceAccountName,
		}, &serviceAccount); err != nil {
			imagev1.SetImageRepositoryReadiness(
				imageRepo,
				metav1.ConditionFalse,
				imagev1.ReconciliationFailedReason,
				err.Error(),
			)
			return err
		}

		if len(serviceAccount.ImagePullSecrets) > 0 {
			imagePullSecrets := make([]corev1.Secret, len(serviceAccount.ImagePullSecrets))

			for i, ips := range serviceAccount.ImagePullSecrets {
				var saAuthSecret corev1.Secret

				if err := r.Get(ctx, types.NamespacedName{
					Namespace: imageRepo.GetNamespace(),
					Name:      ips.Name,
				}, &saAuthSecret); err != nil {
					imagev1.SetImageRepositoryReadiness(
						imageRepo,
						metav1.ConditionFalse,
						imagev1.ReconciliationFailedReason,
						err.Error(),
					)
					return err
				}

				imagePullSecrets[i] = saAuthSecret
			}

			keychain, err := k8schain.NewFromPullSecrets(ctx, imagePullSecrets)
			if err != nil {
				return err
			}

			options = append(options, remote.WithAuthFromKeychain(keychain))
		}
	}

	options = append(options, remote.WithContext(ctx))

	tags, err := remote.List(ref.Context(), options...)
	if err != nil {
		imagev1.SetImageRepositoryReadiness(
			imageRepo,
			metav1.ConditionFalse,
			imagev1.ReconciliationFailedReason,
			err.Error(),
		)
		return err
	}

	// If no exclusion list has been defined, we make sure to always skip tags ending with
	// ".sig", since that tag does not point to a valid image.
	if len(imageRepo.Spec.ExclusionList) == 0 {
		imageRepo.Spec.ExclusionList = append(imageRepo.Spec.ExclusionList, CosignObjectRegex)
	}

	filteredTags := []string{}
	for _, regex := range imageRepo.Spec.ExclusionList {
		r, err := regexp.Compile(regex)
		if err != nil {
			return fmt.Errorf("failed to compile regex %s: %w", regex, err)
		}
		for _, tag := range tags {
			if !r.MatchString(tag) {
				filteredTags = append(filteredTags, tag)
			}
		}
	}

	canonicalName := ref.Context().String()
	if err := r.Database.SetTags(canonicalName, filteredTags); err != nil {
		return fmt.Errorf("failed to set tags for %q: %w", canonicalName, err)
	}

	scanTime := metav1.Now()
	imageRepo.Status.LastScanResult = &imagev1.ScanResult{
		TagCount: len(filteredTags),
		ScanTime: scanTime,
	}

	// if the reconcile request annotation was set, consider it
	// handled (NB it doesn't matter here if it was changed since last
	// time)
	if token, ok := meta.ReconcileAnnotationValue(imageRepo.GetAnnotations()); ok {
		imageRepo.Status.SetLastHandledReconcileRequest(token)
	}

	imagev1.SetImageRepositoryReadiness(
		imageRepo,
		metav1.ConditionTrue,
		imagev1.ReconciliationSucceededReason,
		fmt.Sprintf("successful scan, found %v tags", len(filteredTags)),
	)

	return nil
}

func transportFromSecret(certSecret *corev1.Secret) (*http.Transport, error) {
	// It's possible the secret doesn't contain any certs after
	// all and the default transport could be used; but it's
	// simpler here to assume a fresh transport is needed.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{},
	}
	tlsConfig := transport.TLSClientConfig

	if clientCert, ok := certSecret.Data[ClientCert]; ok {
		// parse and set client cert and secret
		if clientKey, ok := certSecret.Data[ClientKey]; ok {
			cert, err := tls.X509KeyPair(clientCert, clientKey)
			if err != nil {
				return nil, err
			}
			tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
		} else {
			return nil, fmt.Errorf("client certificate found, but no key")
		}
	}
	if caCert, ok := certSecret.Data[CACert]; ok {
		syscerts, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		syscerts.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = syscerts
	}

	return transport, nil
}

// shouldScan takes an image repo and the time now, and says whether
// the repository should be scanned now, and how long to wait for the
// next scan.
func (r *ImageRepositoryReconciler) shouldScan(repo imagev1.ImageRepository, now time.Time) (bool, time.Duration, error) {
	scanInterval := repo.Spec.Interval.Duration

	// never scanned; do it now
	lastScanResult := repo.Status.LastScanResult
	if lastScanResult == nil {
		return true, scanInterval, nil
	}
	lastScanTime := lastScanResult.ScanTime

	// Is the controller seeing this because the reconcileAt
	// annotation was tweaked? Despite the name of the annotation, all
	// that matters is that it's different.
	if syncAt, ok := meta.ReconcileAnnotationValue(repo.GetAnnotations()); ok {
		if syncAt != repo.Status.GetLastHandledReconcileRequest() {
			return true, scanInterval, nil
		}
	}

	// when recovering, it's possible that the resource has a last
	// scan time, but there's no records because the database has been
	// dropped and created again.

	// FIXME If the repo exists, has been
	// scanned, and doesn't have any tags, this will mean a scan every
	// time the resource comes up for reconciliation.
	tags, err := r.Database.Tags(repo.Status.CanonicalImageName)
	if err != nil {
		return false, scanInterval, err
	}
	if len(tags) == 0 {
		return true, scanInterval, nil
	}

	when := scanInterval - now.Sub(lastScanTime.Time)
	if when < time.Second {
		return true, scanInterval, nil
	}
	return false, when, nil
}

func (r *ImageRepositoryReconciler) SetupWithManager(mgr ctrl.Manager, opts ImageRepositoryReconcilerOptions) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&imagev1.ImageRepository{}).
		WithEventFilter(predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
		}).
		Complete(r)
}

// authFromSecret creates an Authenticator that can be given to the
// `remote` funcs, from a Kubernetes secret. If the secret doesn't
// have the right format or data, it returns an error.
func authFromSecret(secret corev1.Secret, ref name.Reference) (authn.Authenticator, error) {
	switch secret.Type {
	case "kubernetes.io/dockerconfigjson":
		var dockerconfig dockerConfig
		configData := secret.Data[".dockerconfigjson"]
		if err := json.NewDecoder(bytes.NewBuffer(configData)).Decode(&dockerconfig); err != nil {
			return nil, err
		}

		authMap, err := parseAuthMap(dockerconfig)
		if err != nil {
			return nil, err
		}
		registry := ref.Context().RegistryStr()
		auth, ok := authMap[registry]
		if !ok {
			return nil, fmt.Errorf("auth for %q not found in secret %v", registry, types.NamespacedName{Name: secret.GetName(), Namespace: secret.GetNamespace()})
		}
		return authn.FromConfig(auth), nil
	default:
		return nil, fmt.Errorf("unknown secret type %q", secret.Type)
	}
}

// event emits a Kubernetes event and forwards the event to notification controller if configured
func (r *ImageRepositoryReconciler) event(ctx context.Context, repo imagev1.ImageRepository, severity, msg string) {
	eventtype := "Normal"
	if severity == events.EventSeverityError {
		eventtype = "Warning"
	}
	r.EventRecorder.Eventf(&repo, eventtype, severity, msg)
}

func (r *ImageRepositoryReconciler) recordReadinessMetric(ctx context.Context, repo *imagev1.ImageRepository) {
	if r.MetricsRecorder == nil {
		return
	}

	objRef, err := reference.GetReference(r.Scheme, repo)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to record readiness metric")
		return
	}
	if rc := apimeta.FindStatusCondition(repo.Status.Conditions, meta.ReadyCondition); rc != nil {
		r.MetricsRecorder.RecordCondition(*objRef, *rc, !repo.DeletionTimestamp.IsZero())
	} else {
		r.MetricsRecorder.RecordCondition(*objRef, metav1.Condition{
			Type:   meta.ReadyCondition,
			Status: metav1.ConditionUnknown,
		}, !repo.DeletionTimestamp.IsZero())
	}
}

func (r *ImageRepositoryReconciler) recordSuspension(ctx context.Context, imageRepo imagev1.ImageRepository) {
	if r.MetricsRecorder == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)

	objRef, err := reference.GetReference(r.Scheme, &imageRepo)
	if err != nil {
		log.Error(err, "unable to record suspended metric")
		return
	}

	if !imageRepo.DeletionTimestamp.IsZero() {
		r.MetricsRecorder.RecordSuspend(*objRef, false)
	} else {
		r.MetricsRecorder.RecordSuspend(*objRef, imageRepo.Spec.Suspend)
	}
}

func (r *ImageRepositoryReconciler) patchStatus(ctx context.Context, req ctrl.Request,
	newStatus imagev1.ImageRepositoryStatus) error {
	var res imagev1.ImageRepository
	if err := r.Get(ctx, req.NamespacedName, &res); err != nil {
		return err
	}

	patch := client.MergeFrom(res.DeepCopy())
	res.Status = newStatus

	return r.Status().Patch(ctx, &res, patch)
}

func parseAuthMap(config dockerConfig) (map[string]authn.AuthConfig, error) {
	auth := map[string]authn.AuthConfig{}
	for url, entry := range config.Auths {
		host, err := getURLHost(url)
		if err != nil {
			return nil, err
		}

		auth[host] = entry
	}

	return auth, nil
}

func getURLHost(urlStr string) (string, error) {
	if urlStr == "http://" || urlStr == "https://" {
		return "", errors.New("Empty url")
	}

	// ensure url has https:// or http:// prefix
	// url.Parse won't parse the ip:port format very well without the prefix.
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = fmt.Sprintf("https://%s/", urlStr)
	}

	// Some users were passing in credentials in the form of
	// http://docker.io and http://docker.io/v1/, etc.
	// So strip everything down to the host.
	// Also, the registry might be local and on a different port.
	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}

	if u.Host == "" {
		return "", errors.New(fmt.Sprintf(
			"Invalid registry auth key: %s. Expected an HTTPS URL (e.g. 'https://index.docker.io/v2/' or 'https://index.docker.io'), or the same without the 'https://' (e.g., 'index.docker.io/v2/' or 'index.docker.io')",
			urlStr))
	}

	return u.Host, nil
}
