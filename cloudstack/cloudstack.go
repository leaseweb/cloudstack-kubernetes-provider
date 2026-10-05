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
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"gopkg.in/gcfg.v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

// CSConfig wraps the config for the CloudStack cloud provider.
type CSConfig struct {
	Global struct {
		APIURL      string `gcfg:"api-url"`
		APIKey      string `gcfg:"api-key"`
		SecretKey   string `gcfg:"secret-key"`
		SSLNoVerify bool   `gcfg:"ssl-no-verify"`
		ProjectID   string `gcfg:"project-id"`
		Zone        string `gcfg:"zone"`

		// APIRateLimitQPS is the maximum number of CloudStack API requests per second. 0 disables the rate limit.
		APIRateLimitQPS *float64 `gcfg:"api-rate-limit-qps"`
		// APIRateLimitBurst is the number of CloudStack API requests that may exceed the QPS for a short time.
		APIRateLimitBurst *int `gcfg:"api-rate-limit-burst"`
		// VMCacheTTL is the number of seconds that the list of virtual machines is cached for load balancer
		// host lookups. 0 disables the cache.
		VMCacheTTL *int `gcfg:"vm-cache-ttl"`
	}
}

var (
	_ cloudprovider.Interface    = (*CSCloud)(nil)
	_ cloudprovider.InstancesV2  = (*CSCloud)(nil)
	_ cloudprovider.LoadBalancer = (*CSCloud)(nil)
)

// CSCloud is an implementation of Interface for CloudStack.
type CSCloud struct {
	client        *cloudstack.CloudStackClient
	projectID     string // If non-"", all resources will be created within this project
	zone          string
	kclient       kubernetes.Interface
	eventRecorder record.EventRecorder
	vmCache       *vmCache // nil disables the cache
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		cfg, err := readConfig(config)
		if err != nil {
			return nil, err
		}

		return newCSCloud(cfg)
	})
}

func readConfig(config io.Reader) (*CSConfig, error) {
	cfg := &CSConfig{}

	if config == nil {
		return cfg, nil
	}

	if err := gcfg.ReadInto(cfg, config); err != nil {
		return nil, fmt.Errorf("could not parse cloud provider config: %w", err)
	}

	return cfg, nil
}

// newCSCloud creates a new instance of CSCloud.
func newCSCloud(cfg *CSConfig) (*CSCloud, error) {
	qps := defaultAPIRateLimitQPS
	if cfg.Global.APIRateLimitQPS != nil {
		qps = *cfg.Global.APIRateLimitQPS
		if qps < 0 || math.IsNaN(qps) {
			return nil, fmt.Errorf("invalid cloud provider configuration: api-rate-limit-qps must not be negative, got %v", qps)
		}
	}

	burst := defaultAPIRateLimitBurst
	if cfg.Global.APIRateLimitBurst != nil {
		burst = *cfg.Global.APIRateLimitBurst
	}
	if qps > 0 && burst < 1 {
		return nil, fmt.Errorf("invalid cloud provider configuration: api-rate-limit-burst must be at least 1, got %d", burst)
	}

	vmCacheTTL := defaultVMCacheTTL
	if cfg.Global.VMCacheTTL != nil {
		if *cfg.Global.VMCacheTTL < 0 {
			return nil, fmt.Errorf("invalid cloud provider configuration: vm-cache-ttl must not be negative, got %d", *cfg.Global.VMCacheTTL)
		}
		vmCacheTTL = time.Duration(*cfg.Global.VMCacheTTL) * time.Second
	}

	cs := &CSCloud{
		projectID: cfg.Global.ProjectID,
		zone:      cfg.Global.Zone,
		vmCache:   newVMCache(vmCacheTTL),
	}

	if cfg.Global.APIURL != "" && cfg.Global.APIKey != "" && cfg.Global.SecretKey != "" {
		cs.client = cloudstack.NewAsyncClient(cfg.Global.APIURL, cfg.Global.APIKey, cfg.Global.SecretKey, !cfg.Global.SSLNoVerify,
			cloudstack.WithHTTPClient(newHTTPClient(cfg.Global.SSLNoVerify, qps, burst)))
	}

	if qps > 0 {
		klog.Infof("CloudStack API rate limit: %v QPS, burst %d", qps, burst)
	} else {
		klog.Info("CloudStack API rate limit is disabled")
	}

	if cs.client == nil {
		return nil, errors.New("cloud provider configuration incomplete: api-url, api-key, and secret-key are all required")
	}

	return cs, nil
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider.
func (cs *CSCloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, _ <-chan struct{}) {
	clientset := clientBuilder.ClientOrDie("cloud-controller-manager")
	cs.kclient = clientset
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: cs.kclient.CoreV1().Events(""),
	})
	cs.eventRecorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "cloud-provider-cloudstack"})
}

// LoadBalancer returns an implementation of LoadBalancer for CloudStack.
func (cs *CSCloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if cs.client == nil {
		return nil, false
	}

	return cs, true
}

// Instances returns an implementation of Instances for CloudStack.
func (cs *CSCloud) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

// InstancesV2 is an implementation for instances and should only be implemented by external cloud providers.
// Implementing InstancesV2 is behaviorally identical to Instances but is optimized to significantly reduce
// API calls to the cloud provider when registering and syncing nodes. Implementation of this interface will
// disable calls to the Zones interface. Also returns true if the interface is supported, false otherwise.
func (cs *CSCloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	if cs.client == nil {
		return nil, false
	}

	return cs, true
}

// Zones returns an implementation of Zones for CloudStack.
func (cs *CSCloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

// Clusters returns an implementation of Clusters for CloudStack.
func (cs *CSCloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns an implementation of Routes for CloudStack.
func (cs *CSCloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (cs *CSCloud) ProviderName() string {
	return ProviderName
}

// HasClusterID returns true if the cluster has a clusterID.
func (cs *CSCloud) HasClusterID() bool {
	return true
}
