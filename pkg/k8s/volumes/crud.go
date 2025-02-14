// Copyright 2020 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package volumes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/okteto/okteto/pkg/config"
	"github.com/okteto/okteto/pkg/errors"
	"github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
)

//List returns the list of volumes
func List(ctx context.Context, namespace, labels string, c kubernetes.Interface) ([]apiv1.PersistentVolumeClaim, error) {
	vList, err := c.CoreV1().PersistentVolumeClaims(namespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: labels,
		},
	)
	if err != nil {
		return nil, err
	}
	return vList.Items, nil
}

//Create deploys the volume claim for a given development container
func Create(ctx context.Context, dev *model.Dev, c *kubernetes.Clientset) error {
	vClient := c.CoreV1().PersistentVolumeClaims(dev.Namespace)
	pvc := translate(dev)
	k8Volume, err := vClient.Get(ctx, pvc.Name, metav1.GetOptions{})
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("error getting kubernetes volume claim: %s", err)
	}
	if k8Volume.Name != "" {
		return checkPVCValues(k8Volume, dev)
	}
	log.Infof("creating volume claim '%s'", pvc.Name)
	_, err = vClient.Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating kubernetes volume claim: %s", err)
	}
	return nil
}

func checkPVCValues(pvc *apiv1.PersistentVolumeClaim, dev *model.Dev) error {
	currentSize, ok := pvc.Spec.Resources.Requests["storage"]
	if !ok {
		return fmt.Errorf("current okteto volume size is wrong. Run 'okteto down -v' and try again")
	}
	if currentSize.Cmp(resource.MustParse(dev.PersistentVolumeSize())) != 0 {
		if currentSize.Cmp(resource.MustParse("10Gi")) != 0 || dev.PersistentVolumeSize() != model.OktetoDefaultPVSize {
			return fmt.Errorf(
				"current okteto volume size is '%s' instead of '%s'. Run 'okteto down -v' and try again",
				currentSize.String(),
				dev.PersistentVolumeSize(),
			)
		}
	}
	if dev.PersistentVolumeStorageClass() != "" {
		if pvc.Spec.StorageClassName == nil {
			return fmt.Errorf(
				"current okteto volume storageclass is '' instead of '%s'. Run 'okteto down -v' and try again",
				dev.PersistentVolumeStorageClass(),
			)
		} else if dev.PersistentVolumeStorageClass() != *pvc.Spec.StorageClassName {
			return fmt.Errorf(
				"current okteto volume storageclass is '%s' instead of '%s'. Run 'okteto down -v' and try again",
				*pvc.Spec.StorageClassName,
				dev.PersistentVolumeStorageClass(),
			)
		}
	}
	return nil

}

//DestroyDev destroys the persistent volume claim for a given development container
func DestroyDev(ctx context.Context, dev *model.Dev, c *kubernetes.Clientset) error {
	return Destroy(ctx, dev.GetVolumeName(), dev.Namespace, c)
}

//Destroy destroys a persistent volume claim
func Destroy(ctx context.Context, name, namespace string, c *kubernetes.Clientset) error {
	vClient := c.CoreV1().PersistentVolumeClaims(namespace)
	log.Infof("destroying volume '%s'", name)

	ticker := time.NewTicker(1 * time.Second)
	to := 3 * config.GetTimeout() // 90 seconds
	timeout := time.Now().Add(to)

	for i := 0; ; i++ {
		err := vClient.Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				log.Infof("volume '%s' successfully destroyed", name)
				return nil
			}

			return fmt.Errorf("error deleting kubernetes volume: %s", err)
		}

		if time.Now().After(timeout) {
			if err := checkIfAttached(ctx, name, namespace, c); err != nil {
				return err
			}

			return fmt.Errorf("volume claim '%s' wasn't destroyed after %s", name, to.String())
		}

		if i%10 == 5 {
			log.Infof("waiting for volume '%s' to be destroyed", name)
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			log.Info("call to volumes.Destroy cancelled")
			return ctx.Err()
		}
	}

}

func checkIfAttached(ctx context.Context, name, namespace string, c *kubernetes.Clientset) error {
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Infof("failed to get available pods: %s", err)
		return nil
	}

	for i := range pods.Items {
		for j := range pods.Items[i].Spec.Volumes {
			if pods.Items[i].Spec.Volumes[j].PersistentVolumeClaim != nil {
				if pods.Items[i].Spec.Volumes[j].PersistentVolumeClaim.ClaimName == name {
					log.Infof("pvc/%s is still attached to pod/%s", name, pods.Items[i].Name)
					return fmt.Errorf("can't delete the volume '%s' since it's still attached to 'pod/%s'", name, pods.Items[i].Name)
				}
			}
		}
	}

	return nil
}
