/*
Copyright 2020 The Kubernetes Authors.

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
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	ekscontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/eks/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/feature"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/awsnode"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/eks"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/gc"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/iamauth"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/instancestate"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/kubeproxy"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/network"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/securitygroup"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	capiannotations "sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
)

const (
	// deleteRequeueAfter is how long to wait before checking again to see if the control plane still
	// has dependencies during deletion.
	deleteRequeueAfter = 20 * time.Second

	awsManagedControlPlaneKind = "AWSManagedControlPlane"
)

var defaultEKSSecurityGroupRoles = []infrav1.SecurityGroupRole{
	infrav1.SecurityGroupEKSNodeAdditional,
}

// securityGroupRolesForControlPlane returns the security group roles determined by the control plane configuration.
func securityGroupRolesForControlPlane(scope *scope.ManagedControlPlaneScope) []infrav1.SecurityGroupRole {
	// Copy to ensure we do not modify the package-level variable.
	roles := make([]infrav1.SecurityGroupRole, len(defaultEKSSecurityGroupRoles))
	copy(roles, defaultEKSSecurityGroupRoles)

	if scope.Bastion().Enabled {
		roles = append(roles, infrav1.SecurityGroupBastion)
	}
	return roles
}

// AWSManagedControlPlaneReconciler reconciles a AWSManagedControlPlane object.
type AWSManagedControlPlaneReconciler struct {
	client.Client
	Recorder  record.EventRecorder
	Endpoints []scope.ServiceEndpoint

	EnableIAM             bool
	AllowAdditionalRoles  bool
	WatchFilterValue      string
	ExternalResourceGC    bool
	AlternativeGCStrategy bool
	WaitInfraPeriod       time.Duration
}

// SetupWithManager is used to setup the controller.
func (r *AWSManagedControlPlaneReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := logger.FromContext(ctx)

	awsManagedControlPlane := &ekscontrolplanev1.AWSManagedControlPlane{}
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(awsManagedControlPlane).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(log.GetLogger(), r.WatchFilterValue)).
		Build(r)

	if err != nil {
		return fmt.Errorf("failed setting up the AWSManagedControlPlane controller manager: %w", err)
	}

	if err = c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, awsManagedControlPlane.GroupVersionKind(), mgr.GetClient(), &ekscontrolplanev1.AWSManagedControlPlane{})),
		predicates.ClusterUnpausedAndInfrastructureReady(log.GetLogger()),
	); err != nil {
		return fmt.Errorf("failed adding a watch for ready clusters: %w", err)
	}

	if err = c.Watch(
		&source.Kind{Type: &infrav1.AWSManagedCluster{}},
		handler.EnqueueRequestsFromMapFunc(r.managedClusterToManagedControlPlane(ctx, log)),
	); err != nil {
		return fmt.Errorf("failed adding a watch for AWSManagedCluster")
	}

	return nil
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachines;awsmachines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmanagedmachinepools;awsmanagedmachinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools;awsmachinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=awsmanagedcontrolplanes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=awsmanagedcontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusterroleidentities;awsclusterstaticidentities;awsclustercontrolleridentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmanagedclusters;awsmanagedclusters/status,verbs=get;list;watch

// Reconcile will reconcile AWSManagedControlPlane Resources.
func (r *AWSManagedControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := logger.FromContext(ctx)

	// Get the control plane instance
	awsControlPlane := &ekscontrolplanev1.AWSManagedControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, awsControlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Get the cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, awsControlPlane.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	if capiannotations.IsPaused(cluster, awsControlPlane) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	managedScope, err := scope.NewManagedControlPlaneScope(scope.ManagedControlPlaneScopeParams{
		Client:               r.Client,
		Cluster:              cluster,
		ControlPlane:         awsControlPlane,
		ControllerName:       strings.ToLower(awsManagedControlPlaneKind),
		EnableIAM:            r.EnableIAM,
		AllowAdditionalRoles: r.AllowAdditionalRoles,
		Endpoints:            r.Endpoints,
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create scope: %w", err)
	}

	// Always close the scope
	defer func() {
		applicableConditions := []clusterv1.ConditionType{
			ekscontrolplanev1.EKSControlPlaneReadyCondition,
			ekscontrolplanev1.IAMControlPlaneRolesReadyCondition,
			ekscontrolplanev1.IAMAuthenticatorConfiguredCondition,
			ekscontrolplanev1.EKSAddonsConfiguredCondition,
			infrav1.VpcReadyCondition,
			infrav1.SubnetsReadyCondition,
			infrav1.ClusterSecurityGroupsReadyCondition,
		}

		if managedScope.VPC().IsManaged(managedScope.Name()) {
			applicableConditions = append(applicableConditions,
				infrav1.InternetGatewayReadyCondition,
				infrav1.NatGatewaysReadyCondition,
				infrav1.RouteTablesReadyCondition,
			)
			if managedScope.Bastion().Enabled {
				applicableConditions = append(applicableConditions, infrav1.BastionHostReadyCondition)
			}
			if managedScope.VPC().IsIPv6Enabled() {
				applicableConditions = append(applicableConditions, infrav1.EgressOnlyInternetGatewayReadyCondition)
			}
		}

		conditions.SetSummary(managedScope.ControlPlane, conditions.WithConditions(applicableConditions...), conditions.WithStepCounter())

		if err := managedScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	if !awsControlPlane.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.reconcileDelete(ctx, managedScope)
	}

	// Handle normal reconciliation loop.
	return r.reconcileNormal(ctx, managedScope)
}

func (r *AWSManagedControlPlaneReconciler) reconcileNormal(ctx context.Context, managedScope *scope.ManagedControlPlaneScope) (res ctrl.Result, reterr error) {
	managedScope.Info("Reconciling AWSManagedControlPlane")

	// TODO (richardcase): we can remove the if check here in the future when we have
	// allowed enough time for users to move away from using the single kind for
	// infrastructureRef and controlplaneRef.
	if managedScope.Cluster.Spec.InfrastructureRef.Kind != awsManagedControlPlaneKind {
		// Wait for the cluster infrastructure to be ready before creating machines
		if !managedScope.Cluster.Status.InfrastructureReady {
			managedScope.Info("Cluster infrastructure is not ready yet")
			return ctrl.Result{RequeueAfter: r.WaitInfraPeriod}, nil
		}
	}

	awsManagedControlPlane := managedScope.ControlPlane

	if controllerutil.AddFinalizer(managedScope.ControlPlane, ekscontrolplanev1.ManagedControlPlaneFinalizer) {
		if err := managedScope.PatchObject(); err != nil {
			return ctrl.Result{}, err
		}
	}

	ec2Service := ec2.NewService(managedScope)
	networkSvc := network.NewService(managedScope)
	ekssvc := eks.NewService(managedScope)
	sgService := securitygroup.NewService(managedScope, securityGroupRolesForControlPlane(managedScope))
	authService := iamauth.NewService(managedScope, iamauth.BackendTypeConfigMap, managedScope.Client)
	awsnodeService := awsnode.NewService(managedScope)
	kubeproxyService := kubeproxy.NewService(managedScope)

	if err := networkSvc.ReconcileNetwork(); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile network for AWSManagedControlPlane %s/%s: %w", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name, err)
	}

	if err := sgService.ReconcileSecurityGroups(); err != nil {
		conditions.MarkFalse(awsManagedControlPlane, infrav1.ClusterSecurityGroupsReadyCondition, infrav1.ClusterSecurityGroupReconciliationFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile general security groups for AWSManagedControlPlane %s/%s", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name)
	}

	if err := ec2Service.ReconcileBastion(); err != nil {
		conditions.MarkFalse(awsManagedControlPlane, infrav1.BastionHostReadyCondition, infrav1.BastionHostFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, fmt.Errorf("failed to reconcile bastion host for AWSManagedControlPlane %s/%s: %w", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name, err)
	}

	if err := ekssvc.ReconcileControlPlane(ctx); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile control plane for AWSManagedControlPlane %s/%s: %w", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name, err)
	}

	if err := awsnodeService.ReconcileCNI(ctx); err != nil {
		conditions.MarkFalse(managedScope.InfraCluster(), infrav1.SecondaryCidrsReadyCondition, infrav1.SecondaryCidrReconciliationFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, fmt.Errorf("failed to reconcile control plane for AWSManagedControlPlane %s/%s: %w", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name, err)
	}

	if err := kubeproxyService.ReconcileKubeProxy(ctx); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile control plane for AWSManagedControlPlane %s/%s: %w", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name, err)
	}

	if feature.Gates.Enabled(feature.EventBridgeInstanceState) {
		instancestateSvc := instancestate.NewService(managedScope)
		if err := instancestateSvc.ReconcileEC2Events(); err != nil {
			// non fatal error, so we continue
			managedScope.Error(err, "non-fatal: failed to set up EventBridge")
		}
	}
	if err := authService.ReconcileIAMAuthenticator(ctx); err != nil {
		conditions.MarkFalse(awsManagedControlPlane, ekscontrolplanev1.IAMAuthenticatorConfiguredCondition, ekscontrolplanev1.IAMAuthenticatorConfigurationFailedReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile aws-iam-authenticator config for AWSManagedControlPlane %s/%s", awsManagedControlPlane.Namespace, awsManagedControlPlane.Name)
	}
	conditions.MarkTrue(awsManagedControlPlane, ekscontrolplanev1.IAMAuthenticatorConfiguredCondition)

	for _, subnet := range managedScope.Subnets().FilterPrivate() {
		managedScope.SetFailureDomain(subnet.AvailabilityZone, clusterv1.FailureDomainSpec{
			ControlPlane: true,
		})
	}

	return reconcile.Result{}, nil
}

func (r *AWSManagedControlPlaneReconciler) reconcileDelete(ctx context.Context, managedScope *scope.ManagedControlPlaneScope) (_ ctrl.Result, reterr error) {
	log := logger.FromContext(ctx)

	managedScope.Info("Reconciling AWSManagedControlPlane delete")

	controlPlane := managedScope.ControlPlane

	numDependencies, err := r.dependencyCount(ctx, managedScope)
	if err != nil {
		log.Error(err, "error getting controlplane dependencies", "namespace", controlPlane.Namespace, "name", controlPlane.Name)
		return reconcile.Result{}, err
	}
	if numDependencies > 0 {
		log.Info("EKS cluster still has dependencies - requeue needed", "dependencyCount", numDependencies)
		return reconcile.Result{RequeueAfter: deleteRequeueAfter}, nil
	}
	log.Info("EKS cluster has no dependencies")

	ekssvc := eks.NewService(managedScope)
	ec2svc := ec2.NewService(managedScope)
	networkSvc := network.NewService(managedScope)
	sgService := securitygroup.NewService(managedScope, securityGroupRolesForControlPlane(managedScope))

	if err := ekssvc.DeleteControlPlane(); err != nil {
		log.Error(err, "error deleting EKS cluster for EKS control plane", "namespace", controlPlane.Namespace, "name", controlPlane.Name)
		return reconcile.Result{}, err
	}

	if err := ec2svc.DeleteBastion(); err != nil {
		log.Error(err, "error deleting bastion for AWSManagedControlPlane", "namespace", controlPlane.Namespace, "name", controlPlane.Name)
		return reconcile.Result{}, err
	}

	if err := sgService.DeleteSecurityGroups(); err != nil {
		log.Error(err, "error deleting general security groups for AWSManagedControlPlane", "namespace", controlPlane.Namespace, "name", controlPlane.Name)
		return reconcile.Result{}, err
	}

	if r.ExternalResourceGC {
		gcSvc := gc.NewService(managedScope, gc.WithGCStrategy(r.AlternativeGCStrategy))
		if gcErr := gcSvc.ReconcileDelete(ctx); gcErr != nil {
			return reconcile.Result{}, fmt.Errorf("failed delete reconcile for gc service: %w", gcErr)
		}
	}

	if err := networkSvc.DeleteNetwork(); err != nil {
		log.Error(err, "error deleting network for AWSManagedControlPlane", "namespace", controlPlane.Namespace, "name", controlPlane.Name)
		return reconcile.Result{}, err
	}

	controllerutil.RemoveFinalizer(controlPlane, ekscontrolplanev1.ManagedControlPlaneFinalizer)

	return reconcile.Result{}, nil
}

// ClusterToAWSManagedControlPlane is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for AWSManagedControlPlane based on updates to a Cluster.
func (r *AWSManagedControlPlaneReconciler) ClusterToAWSManagedControlPlane(o client.Object) []ctrl.Request {
	c, ok := o.(*clusterv1.Cluster)
	if !ok {
		panic(fmt.Sprintf("Expected a Cluster but got a %T", o))
	}

	if !c.ObjectMeta.DeletionTimestamp.IsZero() {
		return nil
	}

	controlPlaneRef := c.Spec.ControlPlaneRef
	if controlPlaneRef != nil && controlPlaneRef.Kind == awsManagedControlPlaneKind {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}}}
	}

	return nil
}

func (r *AWSManagedControlPlaneReconciler) dependencyCount(ctx context.Context, managedScope *scope.ManagedControlPlaneScope) (int, error) {
	log := logger.FromContext(ctx)

	clusterName := managedScope.Name()
	namespace := managedScope.Namespace()
	log.Info("looking for EKS cluster dependencies", "cluster", klog.KRef(namespace, clusterName))

	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels(map[string]string{clusterv1.ClusterLabelName: clusterName}),
	}

	dependencies := 0

	machines := &infrav1.AWSMachineList{}
	if err := r.Client.List(ctx, machines, listOptions...); err != nil {
		return dependencies, fmt.Errorf("failed to list machines for cluster %s/%s: %w", namespace, clusterName, err)
	}
	log.Debug("tested for AWSMachine dependencies", "count", len(machines.Items))
	dependencies += len(machines.Items)

	if feature.Gates.Enabled(feature.MachinePool) {
		managedMachinePools := &expinfrav1.AWSManagedMachinePoolList{}
		if err := r.Client.List(ctx, managedMachinePools, listOptions...); err != nil {
			return dependencies, fmt.Errorf("failed to list managed machine pools for cluster %s/%s: %w", namespace, clusterName, err)
		}
		log.Debug("tested for AWSManagedMachinePool dependencies", "count", len(managedMachinePools.Items))
		dependencies += len(managedMachinePools.Items)

		machinePools := &expinfrav1.AWSMachinePoolList{}
		if err := r.Client.List(ctx, machinePools, listOptions...); err != nil {
			return dependencies, fmt.Errorf("failed to list machine pools for cluster %s/%s: %w", namespace, clusterName, err)
		}
		log.Debug("tested for AWSMachinePool dependencies", "count", len(machinePools.Items))
		dependencies += len(machinePools.Items)
	}

	return dependencies, nil
}

func (r *AWSManagedControlPlaneReconciler) managedClusterToManagedControlPlane(ctx context.Context, log *logger.Logger) handler.MapFunc {
	return func(o client.Object) []ctrl.Request {
		awsManagedCluster, ok := o.(*infrav1.AWSManagedCluster)
		if !ok {
			log.Error(fmt.Errorf("expected a AWSManagedCluster but got a %T", o), "Expected AWSManagedCluster")
			return nil
		}

		if !awsManagedCluster.ObjectMeta.DeletionTimestamp.IsZero() {
			log.Debug("AWSManagedCluster has a deletion timestamp, skipping mapping")
			return nil
		}

		cluster, err := util.GetOwnerCluster(ctx, r.Client, awsManagedCluster.ObjectMeta)
		if err != nil {
			log.Error(err, "failed to get owning cluster")
			return nil
		}
		if cluster == nil {
			log.Debug("Owning cluster not set on AWSManagedCluster, skipping mapping")
			return nil
		}

		controlPlaneRef := cluster.Spec.ControlPlaneRef
		if controlPlaneRef == nil || controlPlaneRef.Kind != awsManagedControlPlaneKind {
			log.Debug("ControlPlaneRef is nil or not AWSManagedControlPlane, skipping mapping")
			return nil
		}

		return []ctrl.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      controlPlaneRef.Name,
					Namespace: controlPlaneRef.Namespace,
				},
			},
		}
	}
}
