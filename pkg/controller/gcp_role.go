/*
Copyright The KubeVault Authors.

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

package controller

import (
	"context"
	"fmt"
	"time"

	api "kubevault.dev/operator/apis/engine/v1alpha1"
	patchutil "kubevault.dev/operator/client/clientset/versioned/typed/engine/v1alpha1/util"
	"kubevault.dev/operator/pkg/vault/role/gcp"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	kmapi "kmodules.xyz/client-go/api/v1"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/tools/queue"
)

const (
	GCPRolePhaseSuccess api.GCPRolePhase = "Success"
	GCPRoleFinalizer    string           = "gcprole.engine.kubevault.com"
)

func (c *VaultController) initGCPRoleWatcher() {
	c.gcpRoleInformer = c.extInformerFactory.Engine().V1alpha1().GCPRoles().Informer()
	c.gcpRoleQueue = queue.New(api.ResourceKindGCPRole, c.MaxNumRequeues, c.NumThreads, c.runGCPRoleInjector)
	c.gcpRoleInformer.AddEventHandler(queue.NewReconcilableHandler(c.gcpRoleQueue.GetQueue()))
	c.gcpRoleLister = c.extInformerFactory.Engine().V1alpha1().GCPRoles().Lister()
}

func (c *VaultController) runGCPRoleInjector(key string) error {
	obj, exist, err := c.gcpRoleInformer.GetIndexer().GetByKey(key)
	if err != nil {
		glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exist {
		glog.Warningf("GCPRole %s does not exist anymore", key)

	} else {
		role := obj.(*api.GCPRole).DeepCopy()

		glog.Infof("Sync/Add/Update for GCPRole %s/%s", role.Namespace, role.Name)

		if role.DeletionTimestamp != nil {
			if core_util.HasFinalizer(role.ObjectMeta, GCPRoleFinalizer) {
				go c.runGCPRoleFinalizer(role, finalizerTimeout, finalizerInterval)
			}
		} else {
			if !core_util.HasFinalizer(role.ObjectMeta, GCPRoleFinalizer) {
				// Add finalizer
				_, _, err := patchutil.PatchGCPRole(context.TODO(), c.extClient.EngineV1alpha1(), role, func(role *api.GCPRole) *api.GCPRole {
					role.ObjectMeta = core_util.AddFinalizer(role.ObjectMeta, GCPRoleFinalizer)
					return role
				}, metav1.PatchOptions{})
				if err != nil {
					return errors.Wrapf(err, "failed to set GCPRole finalizer for %s/%s", role.Namespace, role.Name)
				}
			}

			gcpRClient, err := gcp.NewGCPRole(c.kubeClient, c.appCatalogClient, role)
			if err != nil {
				return err
			}

			err = c.reconcileGCPRole(gcpRClient, role)
			if err != nil {
				return errors.Wrapf(err, "for GCPRole %s/%s:", role.Namespace, role.Name)
			}
		}
	}
	return nil
}

// Will do:
//	For vault:
// 	  - configure a GCP role
//    - sync role
func (c *VaultController) reconcileGCPRole(gcpRClient gcp.GCPRoleInterface, role *api.GCPRole) error {
	// create role
	err := gcpRClient.CreateRole()
	if err != nil {
		_, err2 := patchutil.UpdateGCPRoleStatus(
			context.TODO(),
			c.extClient.EngineV1alpha1(),
			role.ObjectMeta, func(status *api.GCPRoleStatus) *api.GCPRoleStatus {
				status.Conditions = []kmapi.Condition{
					{
						Type:    kmapi.ConditionFailure,
						Status:  kmapi.ConditionTrue,
						Reason:  "FailedToCreateRole",
						Message: err.Error(),
					},
				}
				return status
			},
			metav1.UpdateOptions{},
		)
		return utilerrors.NewAggregate([]error{err2, errors.Wrap(err, "failed to create role")})
	}

	_, err = patchutil.UpdateGCPRoleStatus(
		context.TODO(),
		c.extClient.EngineV1alpha1(),
		role.ObjectMeta, func(status *api.GCPRoleStatus) *api.GCPRoleStatus {
			status.Conditions = []kmapi.Condition{}
			status.Phase = GCPRolePhaseSuccess
			status.ObservedGeneration = role.Generation
			return status
		},
		metav1.UpdateOptions{},
	)
	return err
}

func (c *VaultController) runGCPRoleFinalizer(role *api.GCPRole, timeout time.Duration, interval time.Duration) {
	if role == nil {
		glog.Infoln("GCPRole is nil")
		return
	}

	id := getGCPRoleId(role)
	if c.finalizerInfo.IsAlreadyProcessing(id) {
		// already processing
		return
	}

	glog.Infof("Processing finalizer for GCPRole %s/%s", role.Namespace, role.Name)
	// Add key to finalizerInfo, it will prevent other go routine to processing for this GCPRole
	c.finalizerInfo.Add(id)

	stopCh := time.After(timeout)
	finalizationDone := false
	timeOutOccured := false
	attempt := 0

	for {
		glog.Infof("GCPRole %s/%s finalizer: attempt %d\n", role.Namespace, role.Name, attempt)

		select {
		case <-stopCh:
			timeOutOccured = true
		default:
		}

		if timeOutOccured {
			break
		}

		if !finalizationDone {
			d, err := gcp.NewGCPRole(c.kubeClient, c.appCatalogClient, role)
			if err != nil {
				glog.Errorf("GCPRole %s/%s finalizer: %v", role.Namespace, role.Name, err)
			} else {
				err = c.finalizeGCPRole(d, role)
				if err != nil {
					glog.Errorf("GCPRole %s/%s finalizer: %v", role.Namespace, role.Name, err)
				} else {
					finalizationDone = true
				}
			}
		}

		if finalizationDone {
			err := c.removeGCPRoleFinalizer(role)
			if err != nil {
				glog.Errorf("GCPRole %s/%s finalizer: removing finalizer %v", role.Namespace, role.Name, err)
			} else {
				break
			}
		}

		select {
		case <-stopCh:
			timeOutOccured = true
		case <-time.After(interval):
		}
		attempt++
	}

	err := c.removeGCPRoleFinalizer(role)
	if err != nil {
		glog.Errorf("GCPRole %s/%s finalizer: removing finalizer %v", role.Namespace, role.Name, err)
	} else {
		glog.Infof("Removed finalizer for GCPRole %s/%s", role.Namespace, role.Name)
	}

	// Delete key from finalizer info as processing is done
	c.finalizerInfo.Delete(id)
}

// Do:
//	- delete role in vault
func (c *VaultController) finalizeGCPRole(gcpRClient gcp.GCPRoleInterface, role *api.GCPRole) error {
	err := gcpRClient.DeleteRole(role.RoleName())
	if err != nil {
		return errors.Wrap(err, "failed to delete gcp role")
	}
	return nil
}

func (c *VaultController) removeGCPRoleFinalizer(role *api.GCPRole) error {
	m, err := c.extClient.EngineV1alpha1().GCPRoles(role.Namespace).Get(context.TODO(), role.Name, metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	// remove finalizer
	_, _, err = patchutil.PatchGCPRole(context.TODO(), c.extClient.EngineV1alpha1(), m, func(role *api.GCPRole) *api.GCPRole {
		role.ObjectMeta = core_util.RemoveFinalizer(role.ObjectMeta, GCPRoleFinalizer)
		return role
	}, metav1.PatchOptions{})
	return err
}

func getGCPRoleId(role *api.GCPRole) string {
	return fmt.Sprintf("%s/%s/%s", api.ResourceGCPRole, role.Namespace, role.Name)
}
