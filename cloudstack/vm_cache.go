/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package cloudstack

import (
	"sync"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
)

// defaultVMCacheTTL is the default time that the list of virtual machines is cached for load balancer host lookups.
const defaultVMCacheTTL = 30 * time.Second

// vmCache caches the list of all virtual machines, so that the load balancers of many services can share one
// listVirtualMachines call. Only verifyHosts uses it. A nil *vmCache is valid and disables the cache.
type vmCache struct {
	ttl time.Duration
	now func() time.Time

	// mu is held during a fetch, so concurrent callers wait for the fetch in progress and share its result.
	mu   sync.Mutex
	list vmList
}

// vmList is a list of virtual machines from one fetch.
type vmList struct {
	vms       []*cloudstack.VirtualMachine
	fetchedAt time.Time
	// generation is incremented on each fetch. 0 means that the list was not fetched by the cache.
	generation uint64
	// fresh is true if the list was fetched by the call to get that returned it.
	fresh bool
}

// newVMCache returns a cache with the given TTL, or nil (disabled) if the TTL is 0.
func newVMCache(ttl time.Duration) *vmCache {
	if ttl <= 0 {
		return nil
	}

	return &vmCache{ttl: ttl, now: time.Now}
}

// get returns the cached list of virtual machines if it did not expire. Else it calls fetch and caches the result.
// If stale is not nil, the cached list is used only if it is newer than stale, for example because another caller
// fetched it after stale. A nil cache always calls fetch.
func (c *vmCache) get(fetch func() ([]*cloudstack.VirtualMachine, error), stale *vmList) (vmList, error) {
	if c == nil {
		vms, err := fetch()

		return vmList{vms: vms, fresh: true}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	valid := c.list.generation != 0 && c.now().Sub(c.list.fetchedAt) < c.ttl
	if valid && (stale == nil || c.list.generation != stale.generation) {
		return c.list, nil
	}

	next := c.list.generation + 1
	fetchedAt := c.now()
	vms, err := fetch()
	if err != nil {
		c.list = vmList{generation: c.list.generation}

		return vmList{}, err
	}
	c.list = vmList{vms: vms, fetchedAt: fetchedAt, generation: next}

	list := c.list
	list.fresh = true

	return list, nil
}

// invalidate drops the cached list, so the next call to get fetches a new one.
func (c *vmCache) invalidate() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.list = vmList{generation: c.list.generation}
}
