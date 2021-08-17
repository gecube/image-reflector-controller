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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helpers "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"

	imagev1 "github.com/fluxcd/image-reflector-controller/api/v1beta1"
)

// These are intended to match the keys used in e.g.,
// https://github.com/fluxcd/flux2/blob/main/cmd/flux/create_secret_helm.go,
// for consistency (and perhaps this will have its own flux create
// secret subcommand at some point).
const (
	ClientCert = "certFile"
	ClientKey  = "keyFile"
	CACert     = "caFile"
)

const (
	EventReasonScanFailed    = "ScanFailed"
	EventReasonScanSucceeded = "ScanSucceeded"
)

// ImageRepositoryReconciler reconciles a ImageRepository object
type ImageRepositoryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Database interface {
		DatabaseWriter
		DatabaseReader
	}

	helpers.Events
	helpers.Metrics
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

func (r *ImageRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	reconcileStart := time.Now()

	// NB: In general, if an error is returned then controller-runtime
	// will requeue the request with back-off. In the following this
	// is usually made explicit by _also_ returning
	// `ctrl.Result{Requeue: true}`.

	var imageRepo imagev1.ImageRepository
	if err := r.Get(ctx, req.NamespacedName, &imageRepo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.RecordSuspend(ctx, &imageRepo, imageRepo.Spec.Suspend)
	if imageRepo.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	patcher, err := patch.NewHelper(&imageRepo, r.Client)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	defer func() {
		// if the reconcile request annotation was set, consider it
		// handled (NB it doesn't matter here if it was changed since last
		// time)
		if token, ok := meta.ReconcileAnnotationValue(imageRepo.GetAnnotations()); ok {
			imageRepo.Status.SetLastHandledReconcileRequest(token)
		}

		if err := patcher.Patch(ctx, &imageRepo, patch.WithOwnedConditions{
			Conditions: []string{meta.ReadyCondition},
		}, patch.WithStatusObservedGeneration{}); err != nil {
			retErr = kerrors.NewAggregate([]error{retErr, err})
		}

		// Always record readiness and duration metrics
		r.RecordReadiness(ctx, &imageRepo)
		r.RecordDuration(ctx, &imageRepo, reconcileStart)
	}()

	log := logr.FromContext(ctx)

	ref, err := name.ParseReference(imageRepo.Spec.Image)
	if err != nil {
		conditions.MarkFalse(
			&imageRepo,
			meta.ReadyCondition,
			imagev1.ImageURLInvalidReason,
			err.Error(),
		)
		log.Error(err, "Unable to parse image name", "imageName", imageRepo.Spec.Image)
		return ctrl.Result{Requeue: true}, err
	}

	// Set CanonicalImageName based on the parsed reference
	if c := ref.Context().String(); imageRepo.Status.CanonicalImageName != c {
		imageRepo.Status.CanonicalImageName = c
	}

	// Throttle scans based on spec Interval
	ok, when, err := r.shouldScan(imageRepo, reconcileStart)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	if ok {
		reconcileErr := r.scan(ctx, &imageRepo, ref)
		if reconcileErr != nil {
			r.Event(ctx, &imageRepo, events.EventSeverityError, EventReasonScanFailed, reconcileErr.Error())
			return ctrl.Result{Requeue: true}, reconcileErr
		}
		// emit successful scan event
		if rc := apimeta.FindStatusCondition(imageRepo.Status.Conditions, meta.SucceededReason); rc != nil {
			r.Event(ctx, &imageRepo, events.EventSeverityInfo, EventReasonScanSucceeded, rc.Message)
		}
	}

	log.Info(fmt.Sprintf("reconciliation finished in %s, next run in %s",
		time.Now().Sub(reconcileStart).String(),
		when.String(),
	))

	return ctrl.Result{RequeueAfter: when}, nil
}

func (r *ImageRepositoryReconciler) scan(ctx context.Context, imageRepo *imagev1.ImageRepository, ref name.Reference) error {
	timeout := imageRepo.GetTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var options []remote.Option
	var authSecret corev1.Secret
	if imageRepo.Spec.SecretRef != nil {
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: imageRepo.GetNamespace(),
			Name:      imageRepo.Spec.SecretRef.Name,
		}, &authSecret); err != nil {
			conditions.MarkFalse(
				imageRepo,
				meta.ReadyCondition,
				meta.FailedReason,
				err.Error(),
			)
			return err
		}
		auth, err := authFromSecret(authSecret, ref)
		if err != nil {
			conditions.MarkFalse(
				imageRepo,
				meta.ReadyCondition,
				meta.FailedReason,
				err.Error(),
			)
			return err
		}
		options = append(options, remote.WithAuth(auth))
	}

	if imageRepo.Spec.CertSecretRef != nil {
		var certSecret corev1.Secret
		if imageRepo.Spec.SecretRef != nil && imageRepo.Spec.SecretRef.Name == imageRepo.Spec.CertSecretRef.Name {
			certSecret = authSecret
		} else {
			if err := r.Get(ctx, types.NamespacedName{
				Namespace: imageRepo.GetNamespace(),
				Name:      imageRepo.Spec.CertSecretRef.Name,
			}, &certSecret); err != nil {
				conditions.MarkFalse(
					imageRepo,
					meta.ReadyCondition,
					meta.FailedReason,
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

	tags, err := remote.ListWithContext(ctx, ref.Context(), options...)
	if err != nil {
		conditions.MarkFalse(
			imageRepo,
			meta.ReadyCondition,
			meta.FailedReason,
			err.Error(),
		)
		return err
	}

	canonicalName := ref.Context().String()
	if err := r.Database.SetTags(canonicalName, tags); err != nil {
		return fmt.Errorf("failed to set tags for %q: %w", canonicalName, err)
	}

	scanTime := metav1.Now()
	imageRepo.Status.LastScanResult = &imagev1.ScanResult{
		TagCount: len(tags),
		ScanTime: scanTime,
	}

	conditions.MarkTrue(
		imageRepo,
		meta.ReadyCondition,
		meta.SucceededReason,
		fmt.Sprintf("successful scan, found %v tags", len(tags)),
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
