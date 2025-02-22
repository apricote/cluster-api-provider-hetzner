/*
Copyright 2022 The Kubernetes Authors.

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
	"time"

	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"github.com/syself/cluster-api-provider-hetzner/pkg/scope"
	secretutil "github.com/syself/cluster-api-provider-hetzner/pkg/secrets"
	hcloudclient "github.com/syself/cluster-api-provider-hetzner/pkg/services/hcloud/client"
	"github.com/syself/cluster-api-provider-hetzner/pkg/services/hcloud/machinetemplate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HCloudMachineTemplateReconciler reconciles a HCloudMachineTemplate object.
type HCloudMachineTemplateReconciler struct {
	client.Client
	RateLimitWaitTime   time.Duration
	APIReader           client.Reader
	HCloudClientFactory hcloudclient.Factory
	WatchFilterValue    string
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hcloudmachinetemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hcloudmachinetemplates/status,verbs=get;update;patch

// Reconcile manages the lifecycle of an HCloudMachineTemplate object.
func (r *HCloudMachineTemplateReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	machineTemplate := &infrav1.HCloudMachineTemplate{}
	if err := r.Get(ctx, req.NamespacedName, machineTemplate); err != nil {
		log.Error(err, "unable to fetch HCloudMachineTemplate")
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	log = log.WithValues("HCloudMachineTemplate", klog.KObj(machineTemplate))

	patchHelper, err := patch.NewHelper(machineTemplate, r.Client)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get patch helper: %w", err)
	}

	defer func() {
		if err := patchHelper.Patch(ctx, machineTemplate); err != nil {
			log.Error(err, "failed to patch HCloudMachineTemplate")
		}
	}()

	// Fetch the Cluster.
	var cluster *clusterv1.Cluster
	clusterClass, err := getOwnerClusterClass(ctx, r.Client, machineTemplate.ObjectMeta)
	// nothing to do if a ClusterClass is the owner
	if err == nil && clusterClass != nil {
		machineTemplate.Status.OwnerType = clusterClass.Kind
		return reconcile.Result{}, nil
	}
	cluster, err = util.GetOwnerCluster(ctx, r.Client, machineTemplate.ObjectMeta)
	if err != nil || cluster == nil {
		log.Info(fmt.Sprintf("%s is missing ownerRef to cluster or cluster does not exist %s/%s",
			machineTemplate.Kind, machineTemplate.Namespace, machineTemplate.Name))
		return reconcile.Result{Requeue: true}, nil
	}
	machineTemplate.Status.OwnerType = cluster.Kind

	log = log.WithValues("Cluster", klog.KObj(cluster))

	// Requeue if cluster has no infrastructure yet.
	if cluster.Spec.InfrastructureRef == nil {
		return reconcile.Result{Requeue: true}, nil
	}

	hetznerCluster := &infrav1.HetznerCluster{}

	hetznerClusterName := client.ObjectKey{
		Namespace: machineTemplate.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, hetznerClusterName, hetznerCluster); err != nil {
		log.Info("HetznerCluster is not available yet")
		return reconcile.Result{}, nil
	}

	log = log.WithValues("HetznerCluster", klog.KObj(hetznerCluster))
	ctx = ctrl.LoggerInto(ctx, log)

	// Create the scope.
	secretManager := secretutil.NewSecretManager(log, r.Client, r.APIReader)
	hcloudToken, _, err := getAndValidateHCloudToken(ctx, req.Namespace, hetznerCluster, secretManager)
	if err != nil {
		return hcloudTokenErrorResult(ctx, err, machineTemplate, infrav1.HCloudTokenAvailableCondition, r.Client)
	}

	hcc := r.HCloudClientFactory.NewClient(hcloudToken)

	machineTemplateScope, err := scope.NewHCloudMachineTemplateScope(scope.HCloudMachineTemplateScopeParams{
		Client:                r.Client,
		Logger:                &log,
		HCloudMachineTemplate: machineTemplate,
		HCloudClient:          hcc,
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create scope: %w", err)
	}

	// Always close the scope when exiting this function so we can persist any HCloudMachine changes.
	defer func() {
		if reterr != nil && errors.Is(reterr, hcloudclient.ErrUnauthorized) {
			conditions.MarkFalse(machineTemplate, infrav1.HCloudTokenAvailableCondition, infrav1.HCloudCredentialsInvalidReason, clusterv1.ConditionSeverityError, "wrong hcloud token")
		} else {
			conditions.MarkTrue(machineTemplate, infrav1.HCloudTokenAvailableCondition)
		}
	}()

	// check whether rate limit has been reached and if so, then wait.
	if wait := reconcileRateLimit(machineTemplate, r.RateLimitWaitTime); wait {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if !machineTemplate.DeletionTimestamp.IsZero() {
		r.reconcileDelete(machineTemplateScope)
		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, r.reconcile(ctx, machineTemplateScope)
}

func (r *HCloudMachineTemplateReconciler) reconcile(ctx context.Context, machineTemplateScope *scope.HCloudMachineTemplateScope) error {
	hcloudMachineTemplate := machineTemplateScope.HCloudMachineTemplate

	// If the HCloudMachineTemplate doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(machineTemplateScope.HCloudMachineTemplate, infrav1.MachineFinalizer)

	// Register the finalizer immediately to avoid orphaning HCloud resources on delete
	if err := machineTemplateScope.PatchObject(ctx); err != nil {
		return err
	}

	// reconcile machine template
	if err := machinetemplate.NewService(machineTemplateScope).Reconcile(ctx); err != nil {
		return fmt.Errorf("failed to reconcile machine template for HCloudMachineTemplate %s/%s: %w",
			hcloudMachineTemplate.Namespace, hcloudMachineTemplate.Name, err)
	}

	return nil
}

func (r *HCloudMachineTemplateReconciler) reconcileDelete(machineTemplateScope *scope.HCloudMachineTemplateScope) {
	controllerutil.RemoveFinalizer(machineTemplateScope.HCloudMachineTemplate, infrav1.MachineFinalizer)
}

func (r *HCloudMachineTemplateReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1.HCloudMachineTemplate{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)
}

// getOwnerClusterClass returns the ClusterClass object owning the current resource.
func getOwnerClusterClass(ctx context.Context, c client.Client, obj metav1.ObjectMeta) (*clusterv1.ClusterClass, error) {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind != "ClusterClass" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to parse GroupVersion: %w", err)
		}
		if gv.Group == clusterv1.GroupVersion.Group {
			return getClusterClassByName(ctx, c, obj.Namespace, ref.Name)
		}
	}
	return nil, nil
}

// getClusterClassByName finds and return a Cluster object using the specified params.
func getClusterClassByName(ctx context.Context, c client.Client, namespace, name string) (*clusterv1.ClusterClass, error) {
	clusterClass := &clusterv1.ClusterClass{}
	key := client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}

	if err := c.Get(ctx, key, clusterClass); err != nil {
		return nil, fmt.Errorf("failed to get ClusterClass/%s: %w", name, err)
	}

	return clusterClass, nil
}
