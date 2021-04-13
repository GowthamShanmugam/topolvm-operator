/*
Copyright 2021 The Topolvm-Operator Authors. All rights reserved.

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
	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"os/signal"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"syscall"
	topolvmv1 "topolvm-operator/api/v1"
	"topolvm-operator/pkg/cluster"
	"topolvm-operator/pkg/operator/controller"
	"topolvm-operator/pkg/operator/k8sutil"
	"topolvm-operator/pkg/operator/volumectr"
	"topolvm-operator/pkg/operator/volumegroup"
)

var (
	clusterLogger = capnslog.NewPackageLogger("topolvm/operator", "topolvm-cluster-reconciler")
)

// TopolvmClusterReconciler reconciles a TopolvmCluster object
type TopolvmClusterReconciler struct {
	scheme              *runtime.Scheme
	context             *cluster.Context
	clusterController   *ClusterController
	configMapController *ConfigMapController
}

func NewTopolvmClusterReconciler(scheme *runtime.Scheme, context *cluster.Context, operatorImage string) *TopolvmClusterReconciler {
	return &TopolvmClusterReconciler{
		scheme:            scheme,
		context:           context,
		clusterController: NewClusterContoller(context, operatorImage),
	}
}

// +kubebuilder:rbac:groups=topolvm.cybozu.com,resources=topolvmclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=topolvm.cybozu.com,resources=topolvmclusters/status,verbs=get;update;patch

func (r *TopolvmClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// your logic here
	clusterLogger.Debugf("start reconcile")
	return r.reconcile(req)
}

func (r *TopolvmClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&topolvmv1.TopolvmCluster{}).
		Complete(r)
}

func (r *TopolvmClusterReconciler) reconcile(request reconcile.Request) (reconcile.Result, error) {

	// Pass object name and namespace
	if request.Namespace != cluster.NameSpace {
		clusterLogger.Errorf("namespace %s of topovlm cluster:%s is not equal to operator namespace:%s", request.Namespace, request.NamespacedName.Name, cluster.NameSpace)
		return reconcile.Result{}, fmt.Errorf("namespace %s of topovlm cluster:%s is not equal to operator namespace:%s", request.Namespace, request.NamespacedName.Name, cluster.NameSpace)
	}

	r.clusterController.namespacedName = request.NamespacedName
	cluster.ClusterName = request.NamespacedName.Name
	// Fetch the topolvmCluster instance
	topolvmCluster := &topolvmv1.TopolvmCluster{}
	err := r.context.Client.Get(context.TODO(), request.NamespacedName, topolvmCluster)
	if err != nil {
		if kerrors.IsNotFound(err) {
			clusterLogger.Debug("topolvm cluster resource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, errors.Wrap(err, "failed to get topolvm cluster")
	}

	cluster.TopolvmImage = topolvmCluster.Spec.TopolvmVersion

	// Set a finalizer so we can do cleanup before the object goes away
	err = controller.AddFinalizerIfNotPresent(r.context.Client, topolvmCluster)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to add finalizer")
	}

	// DELETE: the CR was deleted
	if !topolvmCluster.GetDeletionTimestamp().IsZero() {
		clusterLogger.Infof("deleting topolvm cluster %q", topolvmCluster.Name)
		r.clusterController.lastCluster = nil
		// Remove finalizer
		err = removeFinalizer(r.context.Client, request.NamespacedName)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to remove finalize")
		}

		// Return and do not requeue. Successful deletion.
		return reconcile.Result{}, nil
	}

	// Create the controller owner ref
	ref, err := controller.GetControllerObjectOwnerReference(topolvmCluster, r.scheme)
	if err != nil || ref == nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to get controller %q owner reference", topolvmCluster.Name)
	}

	//start configmap controller
	if r.configMapController == nil {

		r.configMapController = NewConfigMapController(cluster.NewContext(), cluster.NameSpace, ref, r.clusterController)
		go func() {
			stopChan := make(chan struct{})
			sigc := make(chan os.Signal, 1)
			signal.Notify(sigc, syscall.SIGTERM)
			r.configMapController.StartWatch(stopChan)
			<-sigc
			logger.Infof("shutdown signal received, exiting...")
			close(stopChan)
		}()

	}
	r.configMapController.UpdateRef(ref)

	// Do reconcile here!
	if err := r.clusterController.onAdd(topolvmCluster, ref); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile cluster %q", topolvmCluster.Name)
	}

	// Return and do not requeue
	return reconcile.Result{}, nil
}

// ClusterController controls an instance of a topolvm cluster
type ClusterController struct {
	context        *cluster.Context
	namespacedName types.NamespacedName
	lastCluster    *topolvmv1.TopolvmCluster
	operatorImage  string
}

func NewClusterContoller(ctx *cluster.Context, operatorImage string) *ClusterController {

	return &ClusterController{
		context:       ctx,
		operatorImage: operatorImage,
	}

}

func (c *ClusterController) onAdd(topolvmCluster *topolvmv1.TopolvmCluster, ref *metav1.OwnerReference) error {

	// Start the main topolvm cluster orchestration

	if err := c.startPrepareVolumeGroupJob(topolvmCluster, ref); err != nil {
		return errors.Wrap(err, "start prepare volume group failed")
	}

	if err := c.startTopolvmControllerDeployment(topolvmCluster, ref); err != nil {
		return errors.Wrap(err, "start create or update topolvm controller deployment  failed")
	}

	if err := c.startReplaceNodeDeployment(topolvmCluster, ref); err != nil {
		return errors.Wrap(err, "start replace node deployment  failed")
	}

	c.lastCluster = topolvmCluster

	return nil

}

func (c *ClusterController) startReplaceNodeDeployment(topolvmCluster *topolvmv1.TopolvmCluster, ref *metav1.OwnerReference) error {

	ctx := context.TODO()
	deploys, err := c.context.Clientset.AppsV1().Deployments(cluster.NameSpace).List(ctx, metav1.ListOptions{})
	if err != nil {
		clusterLogger.Errorf("list deployment failed err:%v", err)
		return err
	}

	for i := 0; i < len(deploys.Items); i++ {

		if strings.HasPrefix(deploys.Items[i].ObjectMeta.Name, cluster.TopolvmNodeDeploymentNamePrefix) {

			if deploys.Items[i].Spec.Template.Spec.Containers[0].Image == topolvmCluster.Spec.TopolvmVersion {
				clusterLogger.Info("node deployment no change need not reconcile")
				return nil
			}
			containers := deploys.Items[i].Spec.Template.Spec.Containers
			for j := 0; j < len(containers); j++ {
				containers[j].Image = topolvmCluster.Spec.TopolvmVersion
			}
			_, err := c.context.Clientset.AppsV1().Deployments(cluster.NameSpace).Update(ctx, &deploys.Items[i], metav1.UpdateOptions{})
			if err != nil {
				logger.Errorf("update deployment:%s image failed err:%v", deploys.Items[i].ObjectMeta.Name, err)

			}
		}
	}

	return nil
}

func (c *ClusterController) startPrepareVolumeGroupJob(topolvmCluster *topolvmv1.TopolvmCluster, ref *metav1.OwnerReference) error {

	list := topolvmCluster.Spec.DeviceClasses

	// if device class not change then check if has fail class that should be recreate
	if c.lastCluster != nil && reflect.DeepEqual(c.lastCluster.Spec.DeviceClasses, list) {

		go func() {
			for _, ele := range topolvmCluster.Status.NodeStorageStatus {

				if len(ele.FailClasses) > 0 || len(ele.SuccessClasses) == 0 {
					logger.Infof("node%s has fail classes recreate job again", ele.Node)
					if err := volumegroup.MakeAndRunJob(c.context.Clientset, ele.Node, c.operatorImage, ref); err != nil {
						clusterLogger.Errorf("create job for node failed %s", ele.Node)
					}
				} else {
					logger.Infof("class info nothing change no need to start prepare volumegroup job")
				}
			}
		}()

		return nil
	}

	// first should create job anyway
	logger.Info("start make prepare volume group job")
	go func() {
		for _, ele := range list {
			if err := volumegroup.MakeAndRunJob(c.context.Clientset, ele.NodeName, c.operatorImage, ref); err != nil {
				clusterLogger.Errorf("create job for node failed %s", ele.NodeName)
			}
		}

	}()

	return nil
}

func (c *ClusterController) startTopolvmControllerDeployment(topolvmCluster *topolvmv1.TopolvmCluster, ref *metav1.OwnerReference) error {

	ctx := context.TODO()
	deployment, err := c.context.Clientset.AppsV1().Deployments(cluster.NameSpace).Get(ctx, cluster.TopolvmControllerDeploymentName, metav1.GetOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		logger.Errorf("failed to detect deployment:%s. err:%v", cluster.TopolvmControllerDeploymentName, err)
		return errors.Wrap(err, "failed to detect deployment")
	} else if err == nil {
		if deployment.Spec.Template.Spec.Containers[0].Image == topolvmCluster.Spec.TopolvmVersion {
			clusterLogger.Info("controller deployment no change need not reconcile")
			return nil

		}
		length := len(deployment.Spec.Template.Spec.Containers)
		for i := 0; i < length; i++ {
			deployment.Spec.Template.Spec.Containers[i].Image = topolvmCluster.Spec.TopolvmVersion
		}
		_, err := c.context.Clientset.AppsV1().Deployments(deployment.Namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			logger.Errorf("update topolvm controller image failed err %v", err)
			return errors.Wrap(err, "update topolvm controller image failed")
		} else {
			logger.Infof("update topolvm contorller image to %s", topolvmCluster.Spec.TopolvmVersion)
			return nil
		}

	}

	logger.Info("start create controller deployment")
	if err := volumectr.CreateReplaceTopolvmControllerDeployment(c.context.Clientset, ref); err != nil {
		clusterLogger.Errorf("create and replace controller deployment failed err: %v", err)
		return errors.Wrap(err, "create and replace controller deployment failed")
	}

	return nil
}

func (c *ClusterController) UpdateStatus(state *topolvmv1.NodeStorageState) error {

	topolvmCluster := &topolvmv1.TopolvmCluster{}

	err := c.context.Client.Get(context.TODO(), c.namespacedName, topolvmCluster)
	if err != nil {
		if kerrors.IsNotFound(err) {
			clusterLogger.Debug("TopolvmCluster resource not found. Ignoring since object must be deleted.")
			return nil
		}
		clusterLogger.Errorf("failed to retrieve topolvm cluster %q to update topolvm cluster status. %v", c.namespacedName.Name, err)
		return errors.Wrapf(err, "failed to retrieve topolvm cluster %q to update topolvm cluster status ", c.namespacedName.Name)
	}

	length := len(topolvmCluster.Status.NodeStorageStatus)
	nodeFound := false
	for i := 0; i < length; i++ {
		if topolvmCluster.Status.NodeStorageStatus[i].Node == state.Node {
			nodeFound = true
			topolvmCluster.Status.NodeStorageStatus[i].FailClasses = state.FailClasses
			topolvmCluster.Status.NodeStorageStatus[i].SuccessClasses = state.SuccessClasses
			break
		}
	}

	if !nodeFound {
		topolvmCluster.Status.NodeStorageStatus = append(topolvmCluster.Status.NodeStorageStatus, *state)
	}

	if err := k8sutil.UpdateStatus(c.context.Client, topolvmCluster); err != nil {
		clusterLogger.Errorf("failed to update cluster %q status. %v", c.namespacedName.Name, err)
		return errors.Wrapf(err, "failed to update cluster %q status", c.namespacedName.Name)
	}
	return nil

}

// removeFinalizer removes a finalizer
func removeFinalizer(client client.Client, name types.NamespacedName) error {
	topolvmCluster := &topolvmv1.TopolvmCluster{}
	err := client.Get(context.TODO(), name, topolvmCluster)
	if err != nil {
		if kerrors.IsNotFound(err) {
			clusterLogger.Debug("TopolvmCluster resource not found. Ignoring since object must be deleted.")
			return nil
		}
		return errors.Wrapf(err, "failed to retrieve topolvm cluster %q to remove finalizer", name.Name)
	}
	err = controller.RemoveFinalizer(client, topolvmCluster)
	if err != nil {
		return errors.Wrap(err, "failed to remove finalizer")
	}

	return nil
}
