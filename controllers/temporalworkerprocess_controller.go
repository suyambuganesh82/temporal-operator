// Licensed to Alexandre VILAIN under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Alexandre VILAIN licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/alexandrevilain/temporal-operator/api/v1beta1"
	"github.com/alexandrevilain/temporal-operator/pkg/resource"
	"github.com/alexandrevilain/temporal-operator/pkg/resource/workerbuilder"
	"github.com/alexandrevilain/temporal-operator/pkg/status"
	"github.com/alexandrevilain/temporal-operator/pkg/workerprocess"
)

// TemporalWorkerProcessReconciler reconciles a TemporalWorkerProcess object
type TemporalWorkerProcessReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=temporal.io,resources=temporalworkerprocesses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=temporal.io,resources=temporalworkerprocesses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=temporal.io,resources=temporalworkerprocesses/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *TemporalWorkerProcessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Starting reconciliation")

	worker := &v1beta1.TemporalWorkerProcess{}
	err := r.Get(ctx, req.NamespacedName, worker)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Check if the resource has been marked for deletion
	if !worker.ObjectMeta.DeletionTimestamp.IsZero() {
		logger.Info("Deleting worker process", "name", worker.Name)
		return reconcile.Result{}, nil
	}

	// Set defaults on unfiled fields.
	updated := r.reconcileDefaults(ctx, worker)
	if updated {
		err := r.Update(ctx, worker)
		if err != nil {
			logger.Error(err, "Can't set worker defaults")
			return r.handleError(ctx, worker, "", err)
		}
		// As we updated the instance, another reconcile will be triggered.
		return reconcile.Result{}, nil
	}

	if worker.Spec.Builder.BuilderEnabled() {
		// First of all, ensure the configmap containing scripts is up-to-date
		err = r.reconcileWorkerScriptsConfigmap(ctx, worker)
		if err != nil {
			return r.handleErrorWithRequeue(ctx, worker, "can't reconcile schema script configmap", err, 2*time.Second)
		}

		// Optionally build worker process using job
		jobs := resource.GetWorkerProcessJobs()
		for _, job := range jobs {
			if job.Skip(worker) {
				continue
			}

			logger.Info("Checking for worker process builder job", "name", job.Name)
			expectedJobBuilder := workerbuilder.NewWorkerProcessJobBuilder(worker, r.Scheme, job.Name, job.Command)

			expectedJob, err := expectedJobBuilder.Build()
			if err != nil {
				return r.handleSuccessWithRequeue(ctx, worker, 2*time.Second)
			}

			matchingJob := &batchv1.Job{}
			err = r.Client.Get(ctx, types.NamespacedName{Name: expectedJob.GetName(), Namespace: expectedJob.GetNamespace()}, matchingJob)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// The job is not found, create it
					_, err := controllerutil.CreateOrUpdate(ctx, r.Client, expectedJob, func() error {
						return expectedJobBuilder.Update(expectedJob)
					})
					if err != nil {
						return r.handleSuccessWithRequeue(ctx, worker, 2*time.Second)
					}
				} else {
					return r.handleErrorWithRequeue(ctx, worker, "can't get job", err, 2*time.Second)
				}
			}

			if matchingJob.Status.Succeeded != 1 {
				logger.Info("Waiting for worker process build job to complete", "name", job.Name)

				// Requeue after 10 seconds
				return r.handleSuccessWithRequeue(ctx, worker, 2*time.Second)
			}

			logger.Info("Worker process build job is finished", "name", job.Name)

			err = job.ReportSuccess(worker)
			if err != nil {
				return r.handleErrorWithRequeue(ctx, worker, "can't report job success", err, 2*time.Second)
			}
		}
	}

	// Set namespace based on ClusterRef
	namespace := worker.Spec.ClusterRef.Namespace
	if namespace == "" {
		namespace = req.Namespace
	}

	namespacedName := types.NamespacedName{Namespace: namespace, Name: worker.Spec.ClusterRef.Name}
	cluster := &v1beta1.TemporalCluster{}
	err = r.Get(ctx, namespacedName, cluster)
	if err != nil {
		return r.handleError(ctx, worker, v1beta1.ReconcileErrorReason, err)
	}

	if err := r.reconcileResources(ctx, worker, cluster); err != nil {
		logger.Error(err, "Can't reconcile resources")
		return r.handleErrorWithRequeue(ctx, worker, v1beta1.ResourcesReconciliationFailedReason, err, 2*time.Second)
	}

	return r.handleSuccess(ctx, worker)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TemporalWorkerProcessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.TemporalWorkerProcess{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{})

	return controller.Complete(r)
}

// Reconcile worker process builder config maps
func (r *TemporalWorkerProcessReconciler) reconcileWorkerScriptsConfigmap(ctx context.Context, worker *v1beta1.TemporalWorkerProcess) error {
	workerScriptConfigMapBuilder := workerbuilder.NewBuilderScriptsConfigmapBuilder(worker, r.Scheme)
	schemaScriptConfigMap, err := workerScriptConfigMapBuilder.Build()
	if err != nil {
		return err
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, schemaScriptConfigMap, func() error {
		return workerScriptConfigMapBuilder.Update(schemaScriptConfigMap)
	})
	return err
}

func (r *TemporalWorkerProcessReconciler) handleErrorWithRequeue(ctx context.Context, worker *v1beta1.TemporalWorkerProcess, reason string, err error, requeueAfter time.Duration) (ctrl.Result, error) {
	if reason == "" {
		reason = v1beta1.ReconcileErrorReason
	}
	v1beta1.SetTemporalWorkerProcessReconcileError(worker, metav1.ConditionTrue, reason, err.Error())
	err = r.updateWorkerProcessStatus(ctx, worker)
	return reconcile.Result{RequeueAfter: requeueAfter}, err
}

func (r *TemporalWorkerProcessReconciler) handleError(ctx context.Context, worker *v1beta1.TemporalWorkerProcess, reason string, err error) (ctrl.Result, error) {
	return r.handleErrorWithRequeue(ctx, worker, reason, err, 0)
}

func (r *TemporalWorkerProcessReconciler) updateWorkerProcessStatus(ctx context.Context, worker *v1beta1.TemporalWorkerProcess) error {
	err := r.Status().Update(ctx, worker)
	if err != nil {
		return err
	}
	return nil
}

func (r *TemporalWorkerProcessReconciler) reconcileResources(ctx context.Context, temporalWorkerProcess *v1beta1.TemporalWorkerProcess, temporalCluster *v1beta1.TemporalCluster) error {
	logger := log.FromContext(ctx)

	workerProcessBuilder := workerprocess.Builder{
		Instance: temporalWorkerProcess,
		Cluster:  temporalCluster,
		Scheme:   r.Scheme,
	}

	builders, err := workerProcessBuilder.ResourceBuilders()
	if err != nil {
		return err
	}

	logger.Info("Retrieved builders", "count", len(builders))

	for _, builder := range builders {
		if comparer, ok := builder.(resource.Comparer); ok {
			err := equality.Semantic.AddFunc(comparer.Equal)
			if err != nil {
				return err
			}
		}

		res, err := builder.Build()
		if err != nil {
			return err
		}

		operationResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, res, func() error {
			return builder.Update(res)
		})
		r.logAndRecordOperationResult(ctx, temporalWorkerProcess, res, operationResult, err)
		if err != nil {
			return err
		}

		reporter, ok := builder.(resource.WorkerProcessDeploymentReporter)
		if !ok {
			continue
		}

		isWorkerDeploymentReady, err := reporter.ReportWorkerDeploymentStatus(ctx, r.Client)
		if err != nil {
			return err
		}

		logger.Info("Reporting worker process status")
		temporalWorkerProcess.Status.Ready = isWorkerDeploymentReady
	}

	if status.IsWorkerProcessReady(temporalWorkerProcess) {
		v1beta1.SetTemporalWorkerProcessReady(temporalWorkerProcess, metav1.ConditionTrue, v1beta1.ServicesReadyReason, "")
	} else {
		v1beta1.SetTemporalWorkerProcessReady(temporalWorkerProcess, metav1.ConditionFalse, v1beta1.ServicesNotReadyReason, "")
	}

	return r.updateWorkerProcessStatus(ctx, temporalWorkerProcess)
}

func (r *TemporalWorkerProcessReconciler) logAndRecordOperationResult(ctx context.Context, worker *v1beta1.TemporalWorkerProcess, resource runtime.Object, operationResult controllerutil.OperationResult, err error) {
	logger := log.FromContext(ctx)

	var (
		action string
		reason string
	)
	switch operationResult {
	case controllerutil.OperationResultCreated:
		action = "create"
		reason = "RessourceCreate"
	case controllerutil.OperationResultUpdated:
		action = "update"
		reason = "ResourceUpdate"
	case controllerutil.OperationResult("deleted"):
		action = "delete"
		reason = "ResourceDelete"
	default:
		return
	}

	if err == nil {
		msg := fmt.Sprintf("%sd resource %s of type %T", action, resource.(metav1.Object).GetName(), resource.(metav1.Object))
		reason := fmt.Sprintf("%sSucess", reason)
		logger.Info(msg)
		r.Recorder.Event(worker, corev1.EventTypeNormal, reason, msg)
	}

	if err != nil {
		msg := fmt.Sprintf("failed to %s resource %s of Type %T", action, resource.(metav1.Object).GetName(), resource.(metav1.Object))
		reason := fmt.Sprintf("%sError", reason)
		logger.Error(err, msg)
		r.Recorder.Event(worker, corev1.EventTypeWarning, reason, msg)
	}
}

func (r *TemporalWorkerProcessReconciler) handleSuccess(ctx context.Context, worker *v1beta1.TemporalWorkerProcess) (ctrl.Result, error) {
	return r.handleSuccessWithRequeue(ctx, worker, 0)
}

func (r *TemporalWorkerProcessReconciler) handleSuccessWithRequeue(ctx context.Context, worker *v1beta1.TemporalWorkerProcess, requeueAfter time.Duration) (ctrl.Result, error) {
	v1beta1.SetTemporalWorkerProcessReconcileSuccess(worker, metav1.ConditionTrue, v1beta1.ReconcileSuccessReason, "")
	err := r.updateWorkerProcessStatus(ctx, worker)
	return reconcile.Result{RequeueAfter: requeueAfter}, err
}
