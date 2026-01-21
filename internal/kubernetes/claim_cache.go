/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kubernetes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ResourceClaimCache watches NVIDIA GPU ResourceClaims and maintains a map of pod UIDs
// that are using GPU resources
type ResourceClaimCache struct {
	mu      sync.RWMutex
	podUIDs map[types.UID]struct{}

	informerFactory informers.SharedInformerFactory
	stopCh          chan struct{}
	synced          bool
	log             *logrus.Logger
}

func NewResourceClaimCache(clientset *kubernetes.Clientset, log *logrus.Logger) *ResourceClaimCache {
	rcc := &ResourceClaimCache{
		podUIDs: make(map[types.UID]struct{}),
		stopCh:  make(chan struct{}), 
		log:     log,
	}

	// resync every 30 minutes
	rcc.informerFactory = informers.NewSharedInformerFactory(clientset, 30*time.Minute)
	claimInformer := rcc.informerFactory.Resource().V1().ResourceClaims().Informer()

	_, err := claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    rcc.handleClaim(true),
		UpdateFunc: rcc.onClaimUpdate,
		DeleteFunc: rcc.handleClaim(false),
	})

	if err != nil {
		log.Errorf("failed to add event handler to ResourceClaim informer: %v", err)
		return nil
	}

	return rcc
}

// Start begins watching ResourceClaims. Call this after creating the cache.
func (rcc *ResourceClaimCache) Start(ctx context.Context) error {
	rcc.informerFactory.Start(rcc.stopCh)

	// Wait for cache sync
	syncCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	synced := rcc.informerFactory.WaitForCacheSync(syncCtx.Done())
	for informerType, ok := range synced {
		if !ok {
			return fmt.Errorf("failed to sync informer cache for %v", informerType)
		}
	}

	// Another Go routine may call IsSynced() concurrently
	rcc.mu.Lock()
	rcc.synced = true
	rcc.mu.Unlock()

	rcc.log.Info("ResourceClaim cache synced successfully")

	go func() {
		<-ctx.Done()
		close(rcc.stopCh)
	}()

	return nil
}

// IsSynced returns true if the informer cache has completed initial sync.
func (rcc *ResourceClaimCache) IsSynced() bool {
	rcc.mu.RLock()
	defer rcc.mu.RUnlock()
	return rcc.synced
}

// PodUsesNvidiaGPU returns true if the pod with the given UID has reserved an NVIDIA GPU claim.
func (rcc *ResourceClaimCache) PodUsesNvidiaGPU(podUID types.UID) bool {
	rcc.mu.RLock()
	defer rcc.mu.RUnlock()
	_, exists := rcc.podUIDs[podUID]
	return exists
}

func (rcc *ResourceClaimCache) handleClaim(add bool) func(obj interface{}) {
	return func(obj interface{}) {
		claim, ok := obj.(*resourcev1.ResourceClaim)
		if !ok {
			return
		}
		rcc.updatePodUIDs(claim, add)
	}
}

func (rcc *ResourceClaimCache) onClaimUpdate(oldObj, newObj interface{}) {
	oldClaim, ok := oldObj.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}
	newClaim, ok := newObj.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}

	// Remove old pod UIDs and add new ones
	rcc.updatePodUIDs(oldClaim, false)
	rcc.updatePodUIDs(newClaim, true)
}

// updatePodUIDs adds or removes pod UIDs from the cache based on the claim's reservedFor field.
func (rcc *ResourceClaimCache) updatePodUIDs(claim *resourcev1.ResourceClaim, add bool) {
	if !rcc.isNvidiaGPUClaim(claim) {
		return
	}

	rcc.mu.Lock()
	defer rcc.mu.Unlock()

	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		if add {
			rcc.podUIDs[ref.UID] = struct{}{}
		} else {
			delete(rcc.podUIDs, ref.UID)
		}
	}
}

// isNvidiaGPUClaim checks if a ResourceClaim is allocated by the NVIDIA GPU DRA driver.
func (rcc *ResourceClaimCache) isNvidiaGPUClaim(claim *resourcev1.ResourceClaim) bool {
	if claim.Status.Allocation == nil {
		return false
	}

	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == nvidiaDRADriverName {
			return true
		}
	}
	return false
}
