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
	"maps"
	"strings"
	"testing"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

// Tests for an IP that is still allocated after the rules of a deleted load balancer are gone, for example because
// deleting the rules timed out on an earlier attempt.

type orphanedIPMocks struct {
	lb      *cloudstack.MockLoadBalancerServiceIface
	address *cloudstack.MockAddressServiceIface
}

func newOrphanedIPMocks(t *testing.T) orphanedIPMocks {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	return orphanedIPMocks{
		lb:      cloudstack.NewMockLoadBalancerServiceIface(ctrl),
		address: cloudstack.NewMockAddressServiceIface(ctrl),
	}
}

// expectRuleLists sets up the listLoadBalancerRules calls. Each call returns the next count in counts: the ID-based
// and the name-based lookups of the load balancer, and the check for other rules on the IP.
func (m orphanedIPMocks) expectRuleLists(counts ...int) {
	call := 0
	m.lb.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{}).AnyTimes()
	m.lb.EXPECT().ListLoadBalancerRules(gomock.Any()).DoAndReturn(func(*cloudstack.ListLoadBalancerRulesParams) (*cloudstack.ListLoadBalancerRulesResponse, error) {
		n := counts[call]
		call++
		rules := make([]*cloudstack.LoadBalancerRule, 0, n)
		for range n {
			rules = append(rules, &cloudstack.LoadBalancerRule{Id: "other-rule", Name: "K8s_svc_cluster_default_other-tcp-80"})
		}

		return &cloudstack.ListLoadBalancerRulesResponse{Count: n, LoadBalancerRules: rules}, nil
	}).Times(len(counts))
}

// expectIPLookup sets up one listPublicIpAddresses call that returns the IP with the ID, or no IP if id is "".
func (m orphanedIPMocks) expectIPLookup(id string, err error) {
	m.address.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
	if err != nil {
		m.address.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(nil, err)

		return
	}
	resp := &cloudstack.ListPublicIpAddressesResponse{}
	if id != "" {
		resp.Count = 1
		resp.PublicIpAddresses = []*cloudstack.PublicIpAddress{{Id: id, Ipaddress: "10.0.0.1"}}
	}
	m.address.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil)
}

func (m orphanedIPMocks) newCSCloud(service *corev1.Service) *CSCloud {
	return &CSCloud{
		client:        &cloudstack.CloudStackClient{LoadBalancer: m.lb, Address: m.address},
		kclient:       fake.NewSimpleClientset(service),
		eventRecorder: record.NewFakeRecorder(10),
	}
}

// deletedLBService returns a service of type LoadBalancer that is being deleted, with the annotations of a load
// balancer that had the IP 10.0.0.1 with ID ip-1.
func deletedLBService(extra map[string]string) *corev1.Service {
	now := metav1.Now()
	annotations := map[string]string{
		ServiceAnnotationLoadBalancerAddress:   "10.0.0.1",
		ServiceAnnotationLoadBalancerID:        "ip-1",
		ServiceAnnotationLoadBalancerNetworkID: "net-1",
	}
	maps.Copy(annotations, extra)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo", Namespace: "default", Annotations: annotations,
			DeletionTimestamp: &now, Finalizers: []string{"service.kubernetes.io/load-balancer-cleanup"},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
}

func TestGetLoadBalancerOrphanedIP(t *testing.T) {
	t.Run("deleted service with an allocated IP and no rules exists", func(t *testing.T) {
		m := newOrphanedIPMocks(t)
		// ID lookup, name lookup, legacy name lookup: no rules; no other rules on the IP.
		m.expectRuleLists(0, 0, 0, 0)
		m.expectIPLookup("ip-1", nil)

		service := deletedLBService(nil)
		status, exists, err := m.newCSCloud(service).GetLoadBalancer(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exists || status == nil || status.Ingress[0].IP != "10.0.0.1" {
			t.Fatalf("exists = %v, status = %+v, want the load balancer with IP 10.0.0.1", exists, status)
		}
	})

	t.Run("service changed to ClusterIP with an allocated IP exists", func(t *testing.T) {
		m := newOrphanedIPMocks(t)
		m.expectRuleLists(0, 0, 0, 0)
		m.expectIPLookup("ip-1", nil)

		service := deletedLBService(nil)
		service.DeletionTimestamp = nil
		service.Spec.Type = corev1.ServiceTypeClusterIP
		if _, exists, err := m.newCSCloud(service).GetLoadBalancer(t.Context(), "cluster", service); err != nil || !exists {
			t.Fatalf("exists = %v, err = %v, want true, nil", exists, err)
		}
	})

	for _, tc := range []struct {
		name    string
		service func() *corev1.Service
		setup   func(m orphanedIPMocks)
	}{
		{
			name:    "IP already released",
			service: func() *corev1.Service { return deletedLBService(nil) },
			setup: func(m orphanedIPMocks) {
				m.expectRuleLists(0, 0, 0)
				m.expectIPLookup("", nil)
			},
		},
		{
			name: "keep-ip is set",
			service: func() *corev1.Service {
				return deletedLBService(map[string]string{ServiceAnnotationLoadBalancerKeepIP: "true"})
			},
			setup: func(m orphanedIPMocks) {
				m.expectRuleLists(0, 0, 0)
				m.expectIPLookup("ip-1", nil)
			},
		},
		{
			name:    "IP is used by another service",
			service: func() *corev1.Service { return deletedLBService(nil) },
			setup: func(m orphanedIPMocks) {
				m.expectRuleLists(0, 0, 0, 1)
				m.expectIPLookup("ip-1", nil)
			},
		},
		{
			name:    "allocated IP has another ID than the annotation",
			service: func() *corev1.Service { return deletedLBService(nil) },
			setup: func(m orphanedIPMocks) {
				m.expectRuleLists(0, 0, 0)
				m.expectIPLookup("ip-other", nil)
			},
		},
		{
			name: "service is not cleaned up",
			service: func() *corev1.Service {
				service := deletedLBService(nil)
				service.DeletionTimestamp = nil

				return service
			},
			setup: func(m orphanedIPMocks) {
				// No IP lookup for a service that keeps its load balancer.
				m.expectRuleLists(0, 0, 0)
			},
		},
	} {
		t.Run(tc.name+" does not exist", func(t *testing.T) {
			m := newOrphanedIPMocks(t)
			tc.setup(m)

			service := tc.service()
			status, exists, err := m.newCSCloud(service).GetLoadBalancer(t.Context(), "cluster", service)
			if err != nil || exists || status != nil {
				t.Fatalf("status = %+v, exists = %v, err = %v, want nil, false, nil", status, exists, err)
			}
		})
	}

	t.Run("IP lookup error is returned", func(t *testing.T) {
		m := newOrphanedIPMocks(t)
		m.expectRuleLists(0, 0, 0)
		m.expectIPLookup("", errors.New("boom"))

		service := deletedLBService(nil)
		_, exists, err := m.newCSCloud(service).GetLoadBalancer(t.Context(), "cluster", service)
		if err == nil || exists || !strings.Contains(err.Error(), "error looking up annotated IP 10.0.0.1") {
			t.Fatalf("exists = %v, err = %v, want the lookup error", exists, err)
		}
	})
}

// TestOrphanedIPReleasedOnRetry follows the service controller after an earlier delete left the IP allocated:
// GetLoadBalancer reports the load balancer, EnsureLoadBalancerDeleted releases the IP, and then GetLoadBalancer
// reports it as gone, so the finalizer can be removed.
func TestOrphanedIPReleasedOnRetry(t *testing.T) {
	m := newOrphanedIPMocks(t)
	service := deletedLBService(nil)
	cs := m.newCSCloud(service)

	// GetLoadBalancer: no rules, IP still allocated.
	m.expectRuleLists(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	m.expectIPLookup("ip-1", nil)
	if _, exists, err := cs.GetLoadBalancer(t.Context(), "cluster", service); err != nil || !exists {
		t.Fatalf("GetLoadBalancer: exists = %v, err = %v, want true, nil", exists, err)
	}

	// EnsureLoadBalancerDeleted: releases the IP.
	m.expectIPLookup("ip-1", nil)
	m.address.EXPECT().NewDisassociateIpAddressParams("ip-1").Return(&cloudstack.DisassociateIpAddressParams{})
	m.address.EXPECT().DisassociateIpAddress(gomock.Any()).Return(&cloudstack.DisassociateIpAddressResponse{}, nil)
	if err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service); err != nil {
		t.Fatalf("EnsureLoadBalancerDeleted: unexpected error: %v", err)
	}

	// GetLoadBalancer: the IP is gone now.
	m.expectIPLookup("", nil)
	if _, exists, err := cs.GetLoadBalancer(t.Context(), "cluster", service); err != nil || exists {
		t.Fatalf("GetLoadBalancer after release: exists = %v, err = %v, want false, nil", exists, err)
	}
}

func TestReleaseOrphanedIPReturnsLookupErrors(t *testing.T) {
	m := newOrphanedIPMocks(t)
	m.expectRuleLists(0, 0, 0)
	m.expectIPLookup("", errors.New("boom"))

	service := deletedLBService(nil)
	if err := m.newCSCloud(service).EnsureLoadBalancerDeleted(t.Context(), "cluster", service); err == nil {
		t.Fatal("expected an error, so the finalizer is not removed while the IP may still be allocated")
	}
}
