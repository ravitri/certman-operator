/*
Copyright 2019 Red Hat, Inc.

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

package clusterdeployment

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	certmanv1alpha1 "github.com/openshift/certman-operator/api/v1alpha1"
	"github.com/openshift/certman-operator/controllers/utils"
	"github.com/openshift/certman-operator/pkg/localmetrics"
)

var log = logf.Log.WithName("controller_clusterdeployment")

const (
	ClusterDeploymentManagedLabel        = "api.openshift.com/managed"
	hiveRelocationAnnotation             = "hive.openshift.io/relocate"
	hiveRelocationOutgoingValue          = "outgoing"
	fakeClusterDeploymentAnnotation      = "managed.openshift.com/fake"
	ClusterDeploymentLimitedSupportLabel = "api.openshift.com/limited-support"
)

var _ reconcile.Reconciler = &ClusterDeploymentReconciler{}

// ClusterDeploymentReconciler reconciles a ClusterDeployment object
type ClusterDeploymentReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a ClusterDeployment object and sets up
// any needed CertificateRequest objects.
func (r *ClusterDeploymentReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("reconciling ClusterDeployment")

	timer := prometheus.NewTimer(localmetrics.MetricClusterDeploymentReconcileDuration)
	defer func() {
		reconcileDuration := timer.ObserveDuration()
		reqLogger.WithValues("Duration", reconcileDuration).Info("Reconcile complete.")
	}()

	// Fetch the ClusterDeployment instance
	cd := &hivev1.ClusterDeployment{}
	err := r.Client.Get(context.TODO(), request.NamespacedName, cd)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "error looking up clusterDeployment")
		return reconcile.Result{}, err
	}
	// Report LimitedSupport status clusters
	val, ok := cd.Labels[ClusterDeploymentLimitedSupportLabel]
	if val == "true" {
		reqLogger.Info("Cluster is in Limited Support")
		localmetrics.ClusterInLimitedSupport(cd.Name, cd.Namespace)
	}
	if val != "true" || !ok {
		reqLogger.Info("Cluster is in Full Support")
		localmetrics.ClusterInFullSupport(cd.Name, cd.Namespace)
	}

	// Do not make certificate request if the cluster is not a Red Hat managed cluster.
	val, ok = cd.Labels[ClusterDeploymentManagedLabel]
	if !ok || val != "true" {
		reqLogger.Info("not a managed cluster")
		return reconcile.Result{}, nil
	}

	// Do not make certificate request if fake cluster
	val, ok = cd.Annotations[fakeClusterDeploymentAnnotation]
	if ok && val == "true" {
		reqLogger.Info("fake cluster identified, skipping reconcile")
		return reconcile.Result{}, nil
	}

	//Do not reconcile if cluster is not installed
	if !cd.Spec.Installed {
		reqLogger.Info(fmt.Sprintf("cluster %v is not yet in installed state", cd.Name))
		return reconcile.Result{}, nil
	}

	// Do not reconcile if the cluster is being relocated
	for a, v := range cd.Annotations {
		if a == hiveRelocationAnnotation && strings.Split(v, "/")[1] == hiveRelocationOutgoingValue {
			reqLogger.Info(fmt.Sprintf("Not reconciling: ClusterDeployment %s is relocating", cd.Name))
			return reconcile.Result{}, nil
		}
	}

	// Check if CertificateResource is being deleted, if it's deleted remove the finalizer if it exists.
	if !cd.DeletionTimestamp.IsZero() {
		// The object is being deleted
		if utils.ContainsString(cd.ObjectMeta.Finalizers, certmanv1alpha1.CertmanOperatorFinalizerLabel) {
			reqLogger.Info("deleting the CertificateRequest for the ClusterDeployment")
			if err := r.handleDelete(cd, reqLogger); err != nil {
				reqLogger.Error(err, "error deleting CertificateRequests")
				return reconcile.Result{}, err
			}

			reqLogger.Info("removing CertmanOperator finalizer from the ClusterDeployment")
			baseToPatch := client.MergeFrom(cd.DeepCopy())
			cd.ObjectMeta.Finalizers = utils.RemoveString(cd.ObjectMeta.Finalizers, certmanv1alpha1.CertmanOperatorFinalizerLabel)
			if err := r.Client.Patch(context.TODO(), cd, baseToPatch); err != nil {
				reqLogger.Error(err, "error removing finalizer from ClusterDeployment")
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}
	// add finalizer
	if !utils.ContainsString(cd.ObjectMeta.Finalizers, certmanv1alpha1.CertmanOperatorFinalizerLabel) {
		reqLogger.Info("adding CertmanOperator finalizer to the ClusterDeployment")
		baseToPatch := client.MergeFrom(cd.DeepCopy())
		cd.ObjectMeta.Finalizers = append(cd.ObjectMeta.Finalizers, certmanv1alpha1.CertmanOperatorFinalizerLabel)
		if err := r.Client.Patch(context.TODO(), cd, baseToPatch); err != nil {
			reqLogger.Error(err, "error adding finalizer to ClusterDeployment")
			return reconcile.Result{}, err
		}
	}

	if err := r.syncCertificateRequests(cd, reqLogger); err != nil {
		reqLogger.Error(err, "error syncing CertificateRequests")
		return reconcile.Result{}, err
	}

	reqLogger.Info("done syncing")
	return reconcile.Result{}, nil
}

// syncCertificateRequests generates/updates a CertificateRequest for each CertificateBundle
// with CertificateBundle.Generate == true. Returns an error if anything fails in this process.
// Cleanup is performed by deleting old CertificateRequests.
func (r *ClusterDeploymentReconciler) syncCertificateRequests(cd *hivev1.ClusterDeployment, logger logr.Logger) error {
	desiredCRs := []certmanv1alpha1.CertificateRequest{}

	// get a list of current CertificateRequests
	currentCRs, err := r.getCurrentCertificateRequests(cd, logger)
	if err != nil {
		logger.Error(err, err.Error())
		return err
	}

	// for each certbundle with generate==true make a CertificateRequest
	for _, cb := range cd.Spec.CertificateBundles {

		logger.Info(fmt.Sprintf("processing certificate bundle %v", cb.Name),
			"CertificateBundleName", cb.Name,
			"GenerateCertificate", cb.Generate,
		)

		if cb.Generate {
			domains := getDomainsForCertBundle(cb, cd, logger)

			emailAddress, err := utils.GetDefaultNotificationEmailAddress(r.Client)
			if err != nil {
				logger.Error(err, err.Error())
				return err
			}

			if len(domains) > 0 {
				certReq := createCertificateRequest(cb.Name, cb.CertificateSecretRef.Name, domains, cd, emailAddress)
				desiredCRs = append(desiredCRs, certReq)
			} else {
				err := fmt.Errorf("no domains provided for certificate bundle %v in the cluster deployment %v", cb.Name, cd.Name)
				logger.Error(err, err.Error())
			}
		}
	}

	deleteCRs := []certmanv1alpha1.CertificateRequest{}

	// find any extra certificateRequests and mark them for deletion
	for i, currentCR := range currentCRs {
		found := false
		for _, desiredCR := range desiredCRs {
			if desiredCR.Name == currentCR.Name {
				found = true
				break
			}
		}
		if !found {
			deleteCRs = append(deleteCRs, currentCRs[i])
		}
	}

	certBundleStatusList := []hivev1.CertificateBundleStatus{}
	errs := []error{}
	// create/update the desired certificaterequests
	for _, desiredCR := range desiredCRs {
		desiredCR := desiredCR
		currentCR := &certmanv1alpha1.CertificateRequest{}
		searchKey := types.NamespacedName{Name: desiredCR.Name, Namespace: desiredCR.Namespace}
		certBundleStatus := hivev1.CertificateBundleStatus{}
		certBundleStatus.Name = strings.TrimPrefix(desiredCR.Name, cd.Name+"-")
		if err := r.Client.Get(context.TODO(), searchKey, currentCR); err != nil {
			certBundleStatus.Generated = false
			if errors.IsNotFound(err) {
				// create
				if err := controllerutil.SetControllerReference(cd, &desiredCR, r.Scheme); err != nil {
					logger.Error(err, "error setting owner reference", "certrequest", desiredCR.Name)
					errs = append(errs, err)
					continue
				}

				logger.Info(fmt.Sprintf("creating CertificateRequest resource config %v", desiredCR.Name))
				if err := r.Client.Create(context.TODO(), &desiredCR); err != nil {
					logger.Error(err, "error creating certificaterequest")
					errs = append(errs, err)
					continue
				}

			} else {
				logger.Error(err, "error checking for existing certificaterequest")
				errs = append(errs, err)
			}
		} else {
			// update or no update needed
			if !reflect.DeepEqual(currentCR.Spec, desiredCR.Spec) {
				certBundleStatus.Generated = false
				currentCR.Spec = desiredCR.Spec
				if err := r.Client.Update(context.TODO(), currentCR); err != nil {
					logger.Error(err, "error updating certificaterequest", "certrequest", currentCR.Name)
					errs = append(errs, err)
					continue
				}
			} else {
				if currentCR.Status.Issued {
					certBundleStatus.Generated = true
				} else {
					certBundleStatus.Generated = false
				}

				logger.Info("no update needed for certificaterequest", "certrequest", desiredCR.Name)
			}
		}
		certBundleStatusList = append(certBundleStatusList, certBundleStatus)
	}
	cd.Status.CertificateBundles = certBundleStatusList
	if len(errs) > 0 {
		return fmt.Errorf("met multiple errors when sync certificaterequests")
	}

	// delete the  certificaterequests
	for _, deleteCR := range deleteCRs {
		deleteCR := deleteCR
		logger.Info(fmt.Sprintf("deleting CertificateRequest resource config  %v", deleteCR.Name))
		if err := r.Client.Delete(context.TODO(), &deleteCR); err != nil {
			logger.Error(err, "error deleting CertificateRequest that is no longer needed", "certrequest", deleteCR.Name)
			return err
		}
	}

	cdCopy := cd.DeepCopy()
	// update the clusterDeployment certificateBundleStatus
	if !reflect.DeepEqual(cd.Status, cdCopy.Status) {
		baseToPatch := client.MergeFrom(cdCopy.DeepCopy())
		cdCopy.Status.CertificateBundles = certBundleStatusList
		if err := r.Client.Patch(context.TODO(), cdCopy, baseToPatch); err != nil {
			logger.Error(err, "error when update clusterDeploymentStatus")
		}
	}

	return nil
}

// getCurrentCertificateRequests returns an array of CertificateRequests owned by the cluster, within the clusters namespace.
func (r *ClusterDeploymentReconciler) getCurrentCertificateRequests(cd *hivev1.ClusterDeployment, logger logr.Logger) ([]certmanv1alpha1.CertificateRequest, error) {
	certReqsForCluster := []certmanv1alpha1.CertificateRequest{}

	// get all CRs in the cluster's namespace
	currentCRs := &certmanv1alpha1.CertificateRequestList{}
	if err := r.Client.List(context.TODO(), currentCRs, client.InNamespace(cd.Namespace)); err != nil {
		logger.Error(err, "error listing current CertificateRequests")
		return certReqsForCluster, err
	}

	certReqsForCluster = append(certReqsForCluster, currentCRs.Items...)

	return certReqsForCluster, nil
}

// getDomainsForCertBundle returns a slice of domains after validating if CertificateBundleSpec.Name
// matches the default control plane name and appending any other matching domain names from the rest
// of the control plane and ingress list to the domain slice.
func getDomainsForCertBundle(cb hivev1.CertificateBundleSpec, cd *hivev1.ClusterDeployment, logger logr.Logger) []string {
	// declare a slice to hold domains
	domains := []string{}
	dLogger := logger.WithValues("CertificateBundle", cb.Name)

	// First check if CertificateBundleSpec.Name matches the default control plane name
	if cd.Spec.ControlPlaneConfig.ServingCertificates.Default == cb.Name {

		// Add default record for the cluster api
		controlPlaneCertDomain := fmt.Sprintf("api.%s.%s", cd.Spec.ClusterName, cd.Spec.BaseDomain)
		dLogger.Info("control plane config DNS name: " + controlPlaneCertDomain)
		domains = append(domains, controlPlaneCertDomain)

		// Check for extra record option and add to SAN if it's present
		userDomain := os.Getenv("EXTRA_RECORD")
		if userDomain != "" {
			extraDomain := fmt.Sprintf("%s.%s.%s", userDomain, cd.Spec.ClusterName, cd.Spec.BaseDomain)
			dLogger.Info("RH private control plane config DNS name: " + extraDomain)
			domains = append(domains, extraDomain)
		}
	}

	// now check the rest of the control plane
	for _, additionalCert := range cd.Spec.ControlPlaneConfig.ServingCertificates.Additional {
		if additionalCert.Name == cb.Name {
			dLogger.Info("additional domain added to certificate request: " + additionalCert.Domain)
			domains = append(domains, additionalCert.Domain)
		}
	}

	// and lastly the ingress list
	for _, ingress := range cd.Spec.Ingress {
		if ingress.ServingCertificate == cb.Name {
			ingressDomain := ingress.Domain

			// always request wildcard certificates for the ingress domain
			if !strings.HasPrefix(ingressDomain, "*.") {
				ingressDomain = fmt.Sprintf("*.%s", ingress.Domain)
			}

			dLogger.Info("ingress domain added to certificate request: " + ingressDomain)
			domains = append(domains, ingressDomain)
		}
	}

	return domains
}

// createCertificateRequest constructs a CertificateRequest constructed by the
// certmanv1alpha1.CertificateRequest schema.
func createCertificateRequest(certBundleName string, secretName string, domains []string, cd *hivev1.ClusterDeployment, emailAddress string) certmanv1alpha1.CertificateRequest {
	name := fmt.Sprintf("%s-%s", cd.Name, certBundleName)
	name = strings.ToLower(name)

	cr := certmanv1alpha1.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cd.Namespace,
		},
		Spec: certmanv1alpha1.CertificateRequestSpec{
			ACMEDNSDomain: cd.Spec.BaseDomain,
			CertificateSecret: corev1.ObjectReference{
				Kind:      "secret",
				Namespace: cd.Namespace,
				Name:      secretName,
			},
			DnsNames:      domains,
			Email:         emailAddress,
			APIURL:        cd.Status.APIURL,
			WebConsoleURL: cd.Status.WebConsoleURL,
		},
	}

	// GCP platform
	if cd.Spec.Platform.GCP != nil {
		cr.Spec.Platform = certmanv1alpha1.Platform{
			GCP: &certmanv1alpha1.GCPPlatformSecrets{
				Credentials: corev1.LocalObjectReference{
					Name: cd.Spec.Platform.GCP.CredentialsSecretRef.Name,
				},
			},
		}
	}
	// AWS platform
	if cd.Spec.Platform.AWS != nil {
		cr.Spec.Platform = certmanv1alpha1.Platform{
			AWS: &certmanv1alpha1.AWSPlatformSecrets{
				Credentials: corev1.LocalObjectReference{
					Name: cd.Spec.Platform.AWS.CredentialsSecretRef.Name,
				},
				Region: cd.Spec.Platform.AWS.Region,
			},
		}
	}

	// Azure platform
	if cd.Spec.Platform.Azure != nil {
		cr.Spec.Platform = certmanv1alpha1.Platform{
			Azure: &certmanv1alpha1.AzurePlatformSecrets{
				Credentials: corev1.LocalObjectReference{
					Name: cd.Spec.Platform.Azure.CredentialsSecretRef.Name,
				},
				ResourceGroupName: cd.Spec.Platform.Azure.BaseDomainResourceGroupName,
			},
		}
	}

	return cr
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hivev1.ClusterDeployment{}).
		Owns(&certmanv1alpha1.CertificateRequest{}).
		Complete(r)
}
