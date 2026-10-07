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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/cloud-provider/api"
)

// Tests for the tracking of assign and remove jobs that are still running after the async timeout.

type pendingJobTestMocks struct {
	proxyTestMocks
	async *cloudstack.MockAsyncjobServiceIface
}

func newPendingJobTestMocks(t *testing.T) pendingJobTestMocks {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	return pendingJobTestMocks{
		proxyTestMocks: proxyTestMocks{
			lb:       cloudstack.NewMockLoadBalancerServiceIface(ctrl),
			vm:       cloudstack.NewMockVirtualMachineServiceIface(ctrl),
			network:  cloudstack.NewMockNetworkServiceIface(ctrl),
			firewall: cloudstack.NewMockFirewallServiceIface(ctrl),
		},
		async: cloudstack.NewMockAsyncjobServiceIface(ctrl),
	}
}

// newTrackingCSCloud returns a CSCloud with the mocks and job tracking.
func (m pendingJobTestMocks) newTrackingCSCloud(service *corev1.Service) *CSCloud {
	cs := newTestCSCloud(m.lb, nil, m.vm, m.network, m.firewall, service)
	cs.client.Asyncjob = m.async
	cs.pendingJobs = newPendingJobs()

	return cs
}

// expectInstances sets up one listLoadBalancerRuleInstances call for the rule that returns the given VM IDs.
func (m pendingJobTestMocks) expectInstances(ruleID string, vmIDs ...string) {
	vms := make([]*cloudstack.VirtualMachine, 0, len(vmIDs))
	for _, id := range vmIDs {
		vms = append(vms, &cloudstack.VirtualMachine{Id: id})
	}
	m.lb.EXPECT().NewListLoadBalancerRuleInstancesParams(ruleID).Return(&cloudstack.ListLoadBalancerRuleInstancesParams{})
	m.lb.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
		Count: len(vms), LoadBalancerRuleInstances: vms,
	}, nil)
}

// expectAssign sets up one assignToLoadBalancerRule call for the rule that returns the given job ID and error.
func (m pendingJobTestMocks) expectAssign(ruleID, jobID string, err error) {
	m.lb.EXPECT().NewAssignToLoadBalancerRuleParams(ruleID).Return(&cloudstack.AssignToLoadBalancerRuleParams{})
	m.lb.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{JobID: jobID}, err)
}

// testJobID is the ID of the pending job in the tests.
const testJobID = "job-1"

// expectJobStatus sets up one queryAsyncJobResult call for testJobID.
func (m pendingJobTestMocks) expectJobStatus(status int, err error) {
	m.async.EXPECT().NewQueryAsyncJobResultParams(testJobID).Return(&cloudstack.QueryAsyncJobResultParams{})
	if err != nil {
		m.async.EXPECT().QueryAsyncJobResult(gomock.Any()).Return(nil, err)

		return
	}
	m.async.EXPECT().QueryAsyncJobResult(gomock.Any()).Return(&cloudstack.QueryAsyncJobResultResponse{
		Jobstatus: status, Jobresult: json.RawMessage(`{"errortext":"boom"}`),
	}, nil)
}

func isRetryError(err error) bool {
	var re *api.RetryError

	return errors.As(err, &re)
}

var tcp443 = corev1.ServicePort{Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP}

func TestPendingAssignJobs(t *testing.T) {
	t.Run("timed-out assign is not sent again while it is running, and is done after it finished", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)

		// 1st reconcile: the assign times out.
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "job-1", cloudstack.AsyncTimeoutErr)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if !isRetryError(err) || !strings.Contains(err.Error(), "job-1") {
			t.Fatalf("err = %v, want a RetryError for job-1", err)
		}
		if job, ok := cs.pendingJobs.get("id-tcp-80"); !ok || job.jobID != "job-1" || job.op != "assign" {
			t.Fatalf("pending job = %+v, %v, want the assign job-1", job, ok)
		}

		// 2nd reconcile: the job is still running, so the rule is not listed and nothing is sent.
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(0, nil)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); !isRetryError(err) {
			t.Fatalf("err = %v, want a RetryError", err)
		}

		// 3rd reconcile: the job finished, so the rule is reconciled as usual.
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(1, nil)
		m.expectInstances("id-tcp-80", "vm-1")
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); ok {
			t.Error("the finished job is still tracked")
		}
	})

	t.Run("failed job is sent again", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: "job-1", op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(2, nil)
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "job-2", nil)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unknown job is forgotten", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: "job-1", op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(0, errors.New("CloudStack API error 431 (CSExceptionErrorCode: 9999): Invalid parameter jobid"))
		m.expectInstances("id-tcp-80", "vm-1")
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); ok {
			t.Error("the unknown job is still tracked")
		}
	})

	t.Run("other 431 error of the job query keeps the job", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: testJobID, op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(0, errors.New("CloudStack API error 431 (CSExceptionErrorCode: 9999): Invalid parameter projectid"))

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); err == nil || isRetryError(err) {
			t.Fatalf("err = %v, want the job query error", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); !ok {
			t.Error("the job is not tracked anymore")
		}
	})

	t.Run("rule claimed by another reconcile is skipped", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		if !cs.pendingJobs.claim("id-tcp-80") {
			t.Fatal("claim failed")
		}

		// No job query, no list and no assign for the claimed rule.
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if !isRetryError(err) || !strings.Contains(err.Error(), "being changed by another reconcile") {
			t.Fatalf("err = %v, want a RetryError for the claimed rule", err)
		}
		if cs.pendingJobs.claim("id-tcp-80") {
			t.Error("the claim of the other reconcile was released")
		}
	})

	t.Run("claim is released after a reconcile", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "job-2", nil)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); ok {
			t.Error("the rule is still claimed")
		}
	})

	t.Run("other error of the job query is returned", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: "job-1", op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectJobStatus(0, errors.New("connection refused"))

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if err == nil || isRetryError(err) || !strings.Contains(err.Error(), "error checking assign job job-1") {
			t.Fatalf("err = %v, want the job query error", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); !ok {
			t.Error("the job is not tracked anymore")
		}
	})

	t.Run("a pending rule does not stop the other rules", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80, tcp443)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: "job-1", op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"), testLBRule("tcp-443", "30443", "443"))
		m.expectJobStatus(0, nil)
		m.expectInstances("id-tcp-443")
		m.expectAssign("id-tcp-443", "job-2", nil)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80, fwTCP443)

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if !isRetryError(err) || !strings.Contains(err.Error(), "job-1") {
			t.Fatalf("err = %v, want a RetryError for job-1", err)
		}
	})

	t.Run("timed-out assign of a new rule is tracked", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80, tcp443)
		cs := m.newTrackingCSCloud(service)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80", "vm-1")
		m.lb.EXPECT().NewCreateLoadBalancerRuleParams(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleParams{})
		m.lb.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleResponse{
			Id: "rule-new", Name: testRulePrefix + "tcp-443", Publicip: "10.0.0.1", Publicipid: "ip-1",
		}, nil)
		m.expectAssign("rule-new", "job-3", cloudstack.AsyncTimeoutErr)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80, fwTCP443)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); !isRetryError(err) {
			t.Fatalf("err = %v, want a RetryError", err)
		}
		if _, ok := cs.pendingJobs.get("rule-new"); !ok {
			t.Error("the assign job of the new rule is not tracked")
		}
	})

	t.Run("without tracking a timeout is returned as before", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := newTestCSCloud(m.lb, nil, m.vm, m.network, m.firewall, service)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "job-1", cloudstack.AsyncTimeoutErr)

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		want := "error assigning new hosts to rule " + testRulePrefix + "tcp-80 (old hosts preserved): error assigning hosts to load balancer rule " +
			testRulePrefix + "tcp-80: Timeout while waiting for async job to finish"
		if err == nil || isRetryError(err) || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("the VM cache is kept while a job is pending", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)
		cs.vmCache = newVMCache(time.Minute)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "job-1", cloudstack.AsyncTimeoutErr)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes); !isRetryError(err) {
			t.Fatalf("err = %v, want a RetryError", err)
		}
		if cs.vmCache.list.generation == 0 || cs.vmCache.list.fetchedAt.IsZero() {
			t.Error("the VM cache was dropped")
		}
	})
}

func TestPendingJobsUpdateLoadBalancer(t *testing.T) {
	t.Run("pending rule is skipped, the other rule is reconciled", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80, tcp443)
		cs := m.newTrackingCSCloud(service)
		cs.pendingJobs.set("id-tcp-80", pendingJob{jobID: "job-1", op: "assign", submitted: time.Now()})

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"), testLBRule("tcp-443", "30443", "443"))
		m.expectJobStatus(0, nil)
		m.expectInstances("id-tcp-443", "vm-1")

		if err := cs.UpdateLoadBalancer(t.Context(), "cluster", service, proxyNodes); !isRetryError(err) {
			t.Fatalf("err = %v, want a RetryError", err)
		}
	})

	t.Run("timed-out remove is tracked", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80", "vm-1", "vm-old")
		m.lb.EXPECT().NewRemoveFromLoadBalancerRuleParams("id-tcp-80").Return(&cloudstack.RemoveFromLoadBalancerRuleParams{})
		m.lb.EXPECT().RemoveFromLoadBalancerRule(gomock.Any()).Return(&cloudstack.RemoveFromLoadBalancerRuleResponse{JobID: "job-9"}, cloudstack.AsyncTimeoutErr)

		err := cs.UpdateLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if !isRetryError(err) || !strings.Contains(err.Error(), "remove job job-9") {
			t.Fatalf("err = %v, want a RetryError for the remove job", err)
		}
		if job, ok := cs.pendingJobs.get("id-tcp-80"); !ok || job.op != "remove" || len(job.hostIDs) != 1 || job.hostIDs[0] != "vm-old" {
			t.Errorf("pending job = %+v, %v, want the remove of vm-old", job, ok)
		}
	})

	t.Run("other errors are returned as before", func(t *testing.T) {
		m := newPendingJobTestMocks(t)
		service := proxyService(false, tcp80)
		cs := m.newTrackingCSCloud(service)

		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectInstances("id-tcp-80")
		m.expectAssign("id-tcp-80", "", errors.New("boom"))

		err := cs.UpdateLoadBalancer(t.Context(), "cluster", service, proxyNodes)
		if err == nil || isRetryError(err) || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the assign error", err)
		}
		if _, ok := cs.pendingJobs.get("id-tcp-80"); ok {
			t.Error("a failed assign is tracked")
		}
	})
}

func TestPendingJobsDeletedRule(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
	mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("id-tcp-80").Return(&cloudstack.DeleteLoadBalancerRuleParams{})
	mockLB.EXPECT().DeleteLoadBalancerRule(gomock.Any()).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil)

	rule := testLBRule("tcp-80", "30080", "80")
	lb := &loadBalancer{
		CloudStackClient: &cloudstack.CloudStackClient{LoadBalancer: mockLB},
		rules:            map[string]*cloudstack.LoadBalancerRule{rule.Name: rule},
		pendingJobs:      newPendingJobs(),
	}
	lb.pendingJobs.set(rule.Id, pendingJob{jobID: testJobID, op: "assign", submitted: time.Now()})

	if err := lb.deleteLoadBalancerRule(rule); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := lb.pendingJobs.get(rule.Id); ok {
		t.Error("the job of the deleted rule is still tracked")
	}
}

func TestNewCSCloudAsyncJobTimeout(t *testing.T) {
	cfg := func(timeout *int) *CSConfig {
		c := &CSConfig{}
		c.Global.APIURL = "https://cloudstack.example/client/api"
		c.Global.APIKey = "key"
		c.Global.SecretKey = "secret"
		c.Global.AsyncJobTimeout = timeout

		return c
	}
	ptr := func(v int) *int { return &v }

	for _, tc := range []struct {
		name    string
		timeout *int
		wantErr bool
	}{
		{"unset", nil, false},
		{"set", ptr(120), false},
		{"maximum", ptr(86400), false},
		{"above the maximum", ptr(86401), true},
		{"overflow", ptr(10000000000), true},
		{"zero", ptr(0), true},
		{"negative", ptr(-1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, err := newCSCloud(cfg(tc.timeout))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cs.pendingJobs == nil {
				t.Error("job tracking is not enabled")
			}
		})
	}
}
