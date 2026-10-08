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
	"slices"
	"strings"
	"testing"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

// Tests for a change of the protocol of a port between tcp and tcp-proxy, which changes the name of its
// load balancer rule.

// proxyTestMocks has the mocks of a proxy-protocol test.
type proxyTestMocks struct {
	lb       *cloudstack.MockLoadBalancerServiceIface
	vm       *cloudstack.MockVirtualMachineServiceIface
	network  *cloudstack.MockNetworkServiceIface
	firewall *cloudstack.MockFirewallServiceIface
}

func newProxyTestMocks(t *testing.T) proxyTestMocks {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	return proxyTestMocks{
		lb:       cloudstack.NewMockLoadBalancerServiceIface(ctrl),
		vm:       cloudstack.NewMockVirtualMachineServiceIface(ctrl),
		network:  cloudstack.NewMockNetworkServiceIface(ctrl),
		firewall: cloudstack.NewMockFirewallServiceIface(ctrl),
	}
}

// expectExistingRules sets up the name-based lookup of the load balancer, which returns the given rules.
func (m proxyTestMocks) expectExistingRules(rules ...*cloudstack.LoadBalancerRule) {
	m.lb.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
	m.lb.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
		Count: len(rules), LoadBalancerRules: rules,
	}, nil)
	setupVerifyHosts(m.vm)
}

// expectFirewallLists sets up n listFirewallRules calls that return the given rules.
func (m proxyTestMocks) expectFirewallLists(n int, rules ...*cloudstack.FirewallRule) {
	m.firewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{}).Times(n)
	m.firewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{
		Count: len(rules), FirewallRules: rules,
	}, nil).Times(n)
}

// ruleUpdate is an updateLoadBalancerRule call.
type ruleUpdate struct {
	id, name, protocol, algorithm string
}

// expectRuleUpdate records the updateLoadBalancerRule call, and returns err from it.
func (m proxyTestMocks) expectRuleUpdate(err error) *ruleUpdate {
	var got ruleUpdate
	m.lb.EXPECT().NewUpdateLoadBalancerRuleParams(gomock.Any()).DoAndReturn(func(id string) *cloudstack.UpdateLoadBalancerRuleParams {
		got.id = id

		return &cloudstack.UpdateLoadBalancerRuleParams{}
	})
	m.lb.EXPECT().UpdateLoadBalancerRule(gomock.Any()).DoAndReturn(func(p *cloudstack.UpdateLoadBalancerRuleParams) (*cloudstack.UpdateLoadBalancerRuleResponse, error) {
		got.name, _ = p.GetName()
		got.protocol, _ = p.GetProtocol()
		got.algorithm, _ = p.GetAlgorithm()

		return &cloudstack.UpdateLoadBalancerRuleResponse{}, err
	})

	return &got
}

func (m proxyTestMocks) expectRuleDelete(id string) {
	m.lb.EXPECT().NewDeleteLoadBalancerRuleParams(id).Return(&cloudstack.DeleteLoadBalancerRuleParams{})
	m.lb.EXPECT().DeleteLoadBalancerRule(gomock.Any()).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil)
}

func ruleWithProtocol(rule *cloudstack.LoadBalancerRule, protocol string) *cloudstack.LoadBalancerRule {
	rule.Protocol = protocol

	return rule
}

func proxyService(proxy bool, ports ...corev1.ServicePort) *corev1.Service {
	service := testService(ports...)
	if proxy {
		service.Annotations = map[string]string{ServiceAnnotationLoadBalancerProxyProtocol: "true"}
	}

	return service
}

var (
	tcp80       = corev1.ServicePort{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}
	fwTCP80     = &cloudstack.FirewallRule{Id: "fw-tcp-80", Protocol: "tcp", Startport: 80, Endport: 80, Cidrlist: "0.0.0.0/0"}
	fwTCP443    = &cloudstack.FirewallRule{Id: "fw-tcp-443", Protocol: "tcp", Startport: 443, Endport: 443, Cidrlist: "0.0.0.0/0"}
	proxyNodes  = []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	proxyPrefix = strings.TrimSuffix(testRulePrefix, "-")
)

func ensureProxyTest(t *testing.T, m proxyTestMocks, service *corev1.Service) (*CSCloud, *corev1.LoadBalancerStatus, error) {
	t.Helper()
	cs := newTestCSCloud(m.lb, nil, m.vm, m.network, m.firewall, service)
	status, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, proxyNodes)

	return cs, status, err
}

// events returns the reasons of the events that were recorded.
func events(cs *CSCloud) []string {
	var reasons []string
	recorder, ok := cs.eventRecorder.(*record.FakeRecorder)
	if !ok {
		return nil
	}
	for {
		select {
		case e := <-recorder.Events:
			fields := strings.Fields(e)
			if len(fields) > 1 {
				reasons = append(reasons, fields[1])
			}
		default:
			return reasons
		}
	}
}

func TestEnsureLoadBalancerProxyProtocolToggle(t *testing.T) {
	t.Run("enabling proxy protocol updates the rule in place and keeps the firewall rule", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		update := m.expectRuleUpdate(nil)
		expectRuleInstances(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		cs, status, err := ensureProxyTest(t, m, proxyService(true, tcp80))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := ruleUpdate{id: "id-tcp-80", name: proxyPrefix + "-tcp-proxy-80", protocol: "tcp-proxy", algorithm: "roundrobin"}
		if *update != want {
			t.Errorf("update = %+v, want %+v", *update, want)
		}
		if status.Ingress[0].IP != "10.0.0.1" {
			t.Errorf("status IP = %q, want 10.0.0.1", status.Ingress[0].IP)
		}
		if reasons := events(cs); !slices.Contains(reasons, "UpdatedLoadBalancerRule") {
			t.Errorf("events = %v, want UpdatedLoadBalancerRule", reasons)
		}
	})

	t.Run("disabling proxy protocol updates the rule in place", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(ruleWithProtocol(testLBRule("tcp-proxy-80", "30080", "80"), "tcp-proxy"))
		update := m.expectRuleUpdate(nil)
		expectRuleInstances(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, _, err := ensureProxyTest(t, m, proxyService(false, tcp80)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := ruleUpdate{id: "id-tcp-proxy-80", name: proxyPrefix + "-tcp-80", protocol: "tcp", algorithm: "roundrobin"}
		if *update != want {
			t.Errorf("update = %+v, want %+v", *update, want)
		}
	})

	t.Run("toggle with a new node port replaces the rule and keeps the firewall rule", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(testLBRule("tcp-80", "39999", "80"))
		m.expectRuleDelete("id-tcp-80")
		expectCreateLBRules(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)

		if _, _, err := ensureProxyTest(t, m, proxyService(true, tcp80)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("failed update returns the error and creates nothing", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"))
		m.expectRuleUpdate(errors.New("boom"))

		_, _, err := ensureProxyTest(t, m, proxyService(true, tcp80))
		if err == nil || !strings.Contains(err.Error(), "failed to update load balancer rule") {
			t.Fatalf("err = %v, want a failed update error", err)
		}
	})

	t.Run("obsolete rule on a port that is still in use keeps the firewall rule", func(t *testing.T) {
		m := newProxyTestMocks(t)
		// Leftovers of an earlier toggle: both rules exist for port 80.
		m.expectExistingRules(
			testLBRule("tcp-80", "30080", "80"),
			ruleWithProtocol(testLBRule("tcp-proxy-80", "30080", "80"), "tcp-proxy"),
		)
		expectRuleInstances(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		m.expectFirewallLists(1, fwTCP80)
		m.expectRuleDelete("id-tcp-80")

		if _, _, err := ensureProxyTest(t, m, proxyService(true, tcp80)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("removed port still deletes its firewall rule and load balancer rule", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(testLBRule("tcp-80", "30080", "80"), testLBRule("tcp-443", "30443", "443"))
		expectRuleInstances(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		// Listed by the port loop for port 80, and by the cleanup for port 443.
		m.expectFirewallLists(2, fwTCP80, fwTCP443)
		m.firewall.EXPECT().NewDeleteFirewallRuleParams("fw-tcp-443").Return(&cloudstack.DeleteFirewallRuleParams{})
		m.firewall.EXPECT().DeleteFirewallRule(gomock.Any()).Return(&cloudstack.DeleteFirewallRuleResponse{}, nil)
		m.expectRuleDelete("id-tcp-443")

		if _, _, err := ensureProxyTest(t, m, proxyService(false, tcp80)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("udp rule on the same port is not switched to tcp", func(t *testing.T) {
		m := newProxyTestMocks(t)
		m.expectExistingRules(ruleWithProtocol(testLBRule("udp-80", "30080", "80"), "udp"))
		expectCreateLBRules(m.lb, 1)
		expectNetworkWithFirewall(m.network)
		// Listed by the port loop for tcp/80, and by the cleanup for udp/80.
		m.expectFirewallLists(2)
		created := expectFirewallCreates(m.firewall, 1)
		m.expectRuleDelete("id-udp-80")

		if _, _, err := ensureProxyTest(t, m, proxyService(false, tcp80)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := []firewallCall{{"tcp", 80, "[0.0.0.0/0]"}}; !slices.Equal(*created, want) {
			t.Errorf("created firewall rules = %v, want %v", *created, want)
		}
	})
}
