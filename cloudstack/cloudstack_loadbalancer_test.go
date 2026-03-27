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
	"sort"
	"strings"
	"testing"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func TestCompareStringSlice(t *testing.T) {
	tests := []struct {
		name string
		x    []string
		y    []string
		want bool
	}{
		{
			name: "equal slices same order",
			x:    []string{"a", "b", "c"},
			y:    []string{"a", "b", "c"},
			want: true,
		},
		{
			name: "equal slices different order",
			x:    []string{"a", "b", "c"},
			y:    []string{"c", "a", "b"},
			want: true,
		},
		{
			name: "different lengths",
			x:    []string{"a", "b"},
			y:    []string{"a", "b", "c"},
			want: false,
		},
		{
			name: "same length different elements",
			x:    []string{"a", "b", "c"},
			y:    []string{"a", "b", "d"},
			want: false,
		},
		{
			name: "both empty",
			x:    []string{},
			y:    []string{},
			want: true,
		},
		{
			name: "both nil",
			x:    nil,
			y:    nil,
			want: true,
		},
		{
			name: "one nil one empty",
			x:    nil,
			y:    []string{},
			want: true,
		},
		{
			name: "one empty one non-empty",
			x:    []string{},
			y:    []string{"a"},
			want: false,
		},
		{
			name: "duplicate elements equal",
			x:    []string{"a", "a", "b"},
			y:    []string{"a", "b", "a"},
			want: true,
		},
		{
			name: "duplicate elements not equal - different counts",
			x:    []string{"a", "a", "b"},
			y:    []string{"a", "b", "b"},
			want: false,
		},
		{
			name: "single element equal",
			x:    []string{"a"},
			y:    []string{"a"},
			want: true,
		},
		{
			name: "single element not equal",
			x:    []string{"a"},
			y:    []string{"b"},
			want: false,
		},
		{
			name: "CIDR list comparison - typical use case",
			x:    []string{"10.0.0.0/8", "192.168.0.0/16"},
			y:    []string{"192.168.0.0/16", "10.0.0.0/8"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareStringSlice(tt.x, tt.y); got != tt.want {
				t.Errorf("compareStringSlice(%v, %v) = %v, want %v", tt.x, tt.y, got, tt.want)
			}
		})
	}
}

func TestSymmetricDifference(t *testing.T) {
	tests := []struct {
		name        string
		hostIDs     []string
		lbInstances []*cloudstack.VirtualMachine
		wantAssign  []string
		wantRemove  []string
	}{
		{
			name:        "no hosts no instances",
			hostIDs:     []string{},
			lbInstances: []*cloudstack.VirtualMachine{},
			wantAssign:  nil,
			wantRemove:  nil,
		},
		{
			name:        "all new hosts",
			hostIDs:     []string{"host1", "host2", "host3"},
			lbInstances: []*cloudstack.VirtualMachine{},
			wantAssign:  []string{"host1", "host2", "host3"},
			wantRemove:  nil,
		},
		{
			name:    "all hosts to remove",
			hostIDs: []string{},
			lbInstances: []*cloudstack.VirtualMachine{
				{Id: "host1"},
				{Id: "host2"},
			},
			wantAssign: nil,
			wantRemove: []string{"host1", "host2"},
		},
		{
			name:    "exact match - nothing to do",
			hostIDs: []string{"host1", "host2"},
			lbInstances: []*cloudstack.VirtualMachine{
				{Id: "host1"},
				{Id: "host2"},
			},
			wantAssign: nil,
			wantRemove: nil,
		},
		{
			name:    "partial overlap - some to add some to remove",
			hostIDs: []string{"host1", "host3"},
			lbInstances: []*cloudstack.VirtualMachine{
				{Id: "host1"},
				{Id: "host2"},
			},
			wantAssign: []string{"host3"},
			wantRemove: []string{"host2"},
		},
		{
			name:    "add one host",
			hostIDs: []string{"host1", "host2", "host3"},
			lbInstances: []*cloudstack.VirtualMachine{
				{Id: "host1"},
				{Id: "host2"},
			},
			wantAssign: []string{"host3"},
			wantRemove: nil,
		},
		{
			name:    "remove one host",
			hostIDs: []string{"host1"},
			lbInstances: []*cloudstack.VirtualMachine{
				{Id: "host1"},
				{Id: "host2"},
			},
			wantAssign: nil,
			wantRemove: []string{"host2"},
		},
		{
			name:        "nil instances",
			hostIDs:     []string{"host1"},
			lbInstances: nil,
			wantAssign:  []string{"host1"},
			wantRemove:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAssign, gotRemove := symmetricDifference(tt.hostIDs, tt.lbInstances)

			// Sort slices for comparison since map iteration order is not guaranteed
			sort.Strings(gotAssign)
			sort.Strings(tt.wantAssign)
			sort.Strings(gotRemove)
			sort.Strings(tt.wantRemove)

			if !compareStringSlice(gotAssign, tt.wantAssign) {
				t.Errorf("symmetricDifference() assign = %v, want %v", gotAssign, tt.wantAssign)
			}
			if !compareStringSlice(gotRemove, tt.wantRemove) {
				t.Errorf("symmetricDifference() remove = %v, want %v", gotRemove, tt.wantRemove)
			}
		})
	}
}

func TestIsFirewallSupported(t *testing.T) {
	tests := []struct {
		name     string
		services []cloudstack.NetworkServiceInternal
		want     bool
	}{
		{
			name:     "empty services",
			services: []cloudstack.NetworkServiceInternal{},
			want:     false,
		},
		{
			name:     "nil services",
			services: nil,
			want:     false,
		},
		{
			name: "firewall present",
			services: []cloudstack.NetworkServiceInternal{
				{Name: "Dhcp"},
				{Name: "Firewall"},
				{Name: "Dns"},
			},
			want: true,
		},
		{
			name: "firewall not present",
			services: []cloudstack.NetworkServiceInternal{
				{Name: "Dhcp"},
				{Name: "Dns"},
				{Name: "Lb"},
			},
			want: false,
		},
		{
			name: "only firewall",
			services: []cloudstack.NetworkServiceInternal{
				{Name: "Firewall"},
			},
			want: true,
		},
		{
			name: "case sensitive - lowercase firewall",
			services: []cloudstack.NetworkServiceInternal{
				{Name: "firewall"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFirewallSupported(tt.services); got != tt.want {
				t.Errorf("isFirewallSupported() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetStringFromServiceAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		annotations    map[string]string
		annotationKey  string
		defaultSetting string
		want           string
	}{
		{
			name:           "annotation present",
			annotations:    map[string]string{"key1": "value1"},
			annotationKey:  "key1",
			defaultSetting: "default",
			want:           "value1",
		},
		{
			name:           "annotation not present - use default",
			annotations:    map[string]string{"other": "value"},
			annotationKey:  "key1",
			defaultSetting: "default",
			want:           "default",
		},
		{
			name:           "annotation present but empty - return empty",
			annotations:    map[string]string{"key1": ""},
			annotationKey:  "key1",
			defaultSetting: "default",
			want:           "",
		},
		{
			name:           "nil annotations - use default",
			annotations:    nil,
			annotationKey:  "key1",
			defaultSetting: "default",
			want:           "default",
		},
		{
			name:           "empty default when not found",
			annotations:    map[string]string{},
			annotationKey:  "key1",
			defaultSetting: "",
			want:           "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-service",
					Namespace:   "default",
					Annotations: tt.annotations,
				},
			}
			if got := getStringFromServiceAnnotation(service, tt.annotationKey, tt.defaultSetting); got != tt.want {
				t.Errorf("getStringFromServiceAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetBoolFromServiceAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		annotations    map[string]string
		annotationKey  string
		defaultSetting bool
		want           bool
	}{
		{
			name:           "annotation true",
			annotations:    map[string]string{"key1": "true"},
			annotationKey:  "key1",
			defaultSetting: false,
			want:           true,
		},
		{
			name:           "annotation false",
			annotations:    map[string]string{"key1": "false"},
			annotationKey:  "key1",
			defaultSetting: true,
			want:           false,
		},
		{
			name:           "annotation not present - use default true",
			annotations:    map[string]string{},
			annotationKey:  "key1",
			defaultSetting: true,
			want:           true,
		},
		{
			name:           "annotation not present - use default false",
			annotations:    map[string]string{},
			annotationKey:  "key1",
			defaultSetting: false,
			want:           false,
		},
		{
			name:           "invalid value - use default true",
			annotations:    map[string]string{"key1": "invalid"},
			annotationKey:  "key1",
			defaultSetting: true,
			want:           true,
		},
		{
			name:           "invalid value - use default false",
			annotations:    map[string]string{"key1": "yes"},
			annotationKey:  "key1",
			defaultSetting: false,
			want:           false,
		},
		{
			name:           "empty value - use default",
			annotations:    map[string]string{"key1": ""},
			annotationKey:  "key1",
			defaultSetting: true,
			want:           true,
		},
		{
			name:           "nil annotations - use default",
			annotations:    nil,
			annotationKey:  "key1",
			defaultSetting: true,
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-service",
					Namespace:   "default",
					Annotations: tt.annotations,
				},
			}
			if got := getBoolFromServiceAnnotation(service, tt.annotationKey, tt.defaultSetting); got != tt.want {
				t.Errorf("getBoolFromServiceAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckLoadBalancerRule(t *testing.T) {
	t.Run("rule not present returns nil", func(t *testing.T) {
		lb := &loadBalancer{
			rules: map[string]*cloudstack.LoadBalancerRule{},
		}
		port := corev1.ServicePort{Port: 80, NodePort: 30000, Protocol: corev1.ProtocolTCP}

		rule, needsUpdate, err := lb.checkLoadBalancerRule("missing", port, LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule != nil {
			t.Fatalf("expected nil rule, got %v", rule)
		}
		if needsUpdate {
			t.Fatalf("expected needsUpdate to be false")
		}
	})

	t.Run("basic property mismatch deletes rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		deleteParams := &cloudstack.DeleteLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("rule-id").Return(deleteParams),
			mockLB.EXPECT().DeleteLoadBalancerRule(deleteParams).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			ipAddr: "1.1.1.1",
			rules: map[string]*cloudstack.LoadBalancerRule{
				"rule": {
					Id:          "rule-id",
					Name:        "rule",
					Publicip:    "2.2.2.2",
					Privateport: "30000",
					Publicport:  "80",
					Cidrlist:    defaultAllowedCIDR,
					Algorithm:   "roundrobin",
					Protocol:    LoadBalancerProtocolTCP.CSProtocol(),
				},
			},
		}
		port := corev1.ServicePort{Port: 80, NodePort: 30000, Protocol: corev1.ProtocolTCP}

		rule, needsUpdate, err := lb.checkLoadBalancerRule("rule", port, LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule != nil {
			t.Fatalf("expected nil rule after deletion, got %v", rule)
		}
		if needsUpdate {
			t.Fatalf("expected needsUpdate to be false")
		}
		if _, exists := lb.rules["rule"]; exists {
			t.Fatalf("expected rule entry to be removed from map")
		}
	})
}

func TestRuleToString(t *testing.T) {
	tests := []struct {
		name string
		rule *cloudstack.FirewallRule
		want string
	}{
		{
			name: "TCP rule",
			rule: &cloudstack.FirewallRule{
				Protocol:  "tcp",
				Cidrlist:  "10.0.0.0/8",
				Ipaddress: "203.0.113.1",
				Startport: 80,
				Endport:   80,
			},
			want: "{[10.0.0.0/8] -> 203.0.113.1:[80-80] (tcp)}",
		},
		{
			name: "UDP rule",
			rule: &cloudstack.FirewallRule{
				Protocol:  "udp",
				Cidrlist:  "192.168.0.0/16",
				Ipaddress: "203.0.113.2",
				Startport: 53,
				Endport:   53,
			},
			want: "{[192.168.0.0/16] -> 203.0.113.2:[53-53] (udp)}",
		},
		{
			name: "TCP rule with port range",
			rule: &cloudstack.FirewallRule{
				Protocol:  "tcp",
				Cidrlist:  "0.0.0.0/0",
				Ipaddress: "203.0.113.3",
				Startport: 8000,
				Endport:   8999,
			},
			want: "{[0.0.0.0/0] -> 203.0.113.3:[8000-8999] (tcp)}",
		},
		{
			name: "ICMP rule",
			rule: &cloudstack.FirewallRule{
				Protocol:  "icmp",
				Cidrlist:  "10.0.0.0/8",
				Ipaddress: "203.0.113.4",
				Icmptype:  8,
				Icmpcode:  0,
			},
			want: "{[10.0.0.0/8] -> 203.0.113.4 [8,0] (icmp)}",
		},
		{
			name: "unknown protocol",
			rule: &cloudstack.FirewallRule{
				Protocol:  "gre",
				Cidrlist:  "10.0.0.0/8",
				Ipaddress: "203.0.113.6",
			},
			want: "{[10.0.0.0/8] -> 203.0.113.6 (gre)}",
		},
		{
			name: "nil rule",
			rule: nil,
			want: "nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ruleToString(tt.rule)
			if got != tt.want {
				t.Errorf("ruleToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRulesToString(t *testing.T) {
	tests := []struct {
		name  string
		rules []*cloudstack.FirewallRule
		want  string
	}{
		{
			name:  "empty list",
			rules: []*cloudstack.FirewallRule{},
			want:  "none",
		},
		{
			name: "single rule",
			rules: []*cloudstack.FirewallRule{
				{
					Protocol:  "tcp",
					Cidrlist:  "10.0.0.0/8",
					Ipaddress: "203.0.113.1",
					Startport: 80,
					Endport:   80,
				},
			},
			want: "{[10.0.0.0/8] -> 203.0.113.1:[80-80] (tcp)}",
		},
		{
			name: "multiple rules",
			rules: []*cloudstack.FirewallRule{
				{
					Protocol:  "tcp",
					Cidrlist:  "10.0.0.0/8",
					Ipaddress: "203.0.113.1",
					Startport: 80,
					Endport:   80,
				},
				{
					Protocol:  "udp",
					Cidrlist:  "192.168.0.0/16",
					Ipaddress: "203.0.113.2",
					Startport: 53,
					Endport:   53,
				},
				{
					Protocol:  "icmp",
					Cidrlist:  "0.0.0.0/0",
					Ipaddress: "203.0.113.3",
					Icmptype:  8,
					Icmpcode:  0,
				},
			},
			want: "{[10.0.0.0/8] -> 203.0.113.1:[80-80] (tcp)}, {[192.168.0.0/16] -> 203.0.113.2:[53-53] (udp)}, {[0.0.0.0/0] -> 203.0.113.3 [8,0] (icmp)}",
		},
		{
			name: "rules with nil rule",
			rules: []*cloudstack.FirewallRule{
				{
					Protocol:  "tcp",
					Cidrlist:  "10.0.0.0/8",
					Ipaddress: "203.0.113.1",
					Startport: 80,
					Endport:   80,
				},
				nil,
				{
					Protocol:  "udp",
					Cidrlist:  "192.168.0.0/16",
					Ipaddress: "203.0.113.2",
					Startport: 53,
					Endport:   53,
				},
			},
			want: "{[10.0.0.0/8] -> 203.0.113.1:[80-80] (tcp)}, nil, {[192.168.0.0/16] -> 203.0.113.2:[53-53] (udp)}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rulesToString(tt.rules)
			if got != tt.want {
				t.Errorf("rulesToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRulesMapToString(t *testing.T) {
	const unorderedSentinel = "__unordered__"

	tests := []struct {
		name  string
		rules map[*cloudstack.FirewallRule]bool
		want  string
	}{
		{
			name:  "empty map",
			rules: map[*cloudstack.FirewallRule]bool{},
			want:  "none",
		},
		{
			name: "single rule",
			rules: map[*cloudstack.FirewallRule]bool{
				{
					Protocol:  "tcp",
					Cidrlist:  "10.0.0.0/8",
					Ipaddress: "203.0.113.1",
					Startport: 80,
					Endport:   80,
				}: true,
			},
			want: "{[10.0.0.0/8] -> 203.0.113.1:[80-80] (tcp)}",
		},
		{
			name: "multiple rules",
			rules: map[*cloudstack.FirewallRule]bool{
				{
					Protocol:  "tcp",
					Cidrlist:  "10.0.0.0/8",
					Ipaddress: "203.0.113.1",
					Startport: 80,
					Endport:   80,
				}: true,
				{
					Protocol:  "udp",
					Cidrlist:  "192.168.0.0/16",
					Ipaddress: "203.0.113.2",
					Startport: 53,
					Endport:   53,
				}: false,
				{
					Protocol:  "icmp",
					Cidrlist:  "0.0.0.0/0",
					Ipaddress: "203.0.113.3",
					Icmptype:  8,
					Icmpcode:  0,
				}: true,
			},
			want: unorderedSentinel, // order-independent
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rulesMapToString(tt.rules)

			if tt.want == unorderedSentinel {
				// For maps, order is non-deterministic, so check that all rules are present.
				expectedRules := make([]string, 0, len(tt.rules))
				for rule := range tt.rules {
					expectedRules = append(expectedRules, ruleToString(rule))
				}

				parts := strings.Split(got, ", ")
				if len(parts) != len(expectedRules) {
					t.Errorf("rulesMapToString() returned %d rules, want %d", len(parts), len(expectedRules))

					return
				}
				for _, expectedRule := range expectedRules {
					found := false
					for _, part := range parts { //nolint:modernize
						if part == expectedRule {
							found = true

							break
						}
					}
					if !found {
						t.Errorf("rulesMapToString() missing rule %q in output %q", expectedRule, got)
					}
				}

				return
			}

			if got != tt.want {
				t.Errorf("rulesMapToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetPublicIPAddress(t *testing.T) {
	t.Run("IP found and allocated", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
					Allocated: "2023-01-01T00:00:00+0000",
				},
			},
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("IP found but not allocated - associates", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
					Allocated: "",
				},
			},
		}

		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
			ipAddr:    "203.0.113.1",
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("IP not found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count:             0,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{},
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err == nil {
			t.Fatalf("expected error for IP not found")
		}
		if !strings.Contains(err.Error(), "could not find IP address") {
			t.Errorf("error message = %q, want to contain 'could not find IP address'", err.Error())
		}
	})

	t.Run("multiple IPs found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 2,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-1", Ipaddress: "203.0.113.1"},
				{Id: "ip-2", Ipaddress: "203.0.113.1"},
			},
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err == nil {
			t.Fatalf("expected error for multiple IPs found")
		}
		if !strings.Contains(err.Error(), "Found 2 addresses") {
			t.Errorf("error message = %q, want to contain 'Found 2 addresses'", err.Error())
		}
	})

	t.Run("error retrieving IP", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		apiErr := errors.New("API error")

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error retrieving IP address") {
			t.Errorf("error message = %q, want to contain 'error retrieving IP address'", err.Error())
		}
	})

	t.Run("project ID handling", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
					Allocated: "2023-01-01T00:00:00+0000",
				},
			},
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
			projectID: "proj-123",
		}

		err := lb.getPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})
}

func TestAssociatePublicIPAddress(t *testing.T) {
	t.Run("associate IP for regular network", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.associatePublicIPAddress()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("associate IP for VPC network", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "vpc-456",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.associatePublicIPAddress()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("error retrieving network", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		apiErr := errors.New("network API error")

		mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(nil, 1, apiErr)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.associatePublicIPAddress()
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error retrieving network") {
			t.Errorf("error message = %q, want to contain 'error retrieving network'", err.Error())
		}
	})

	t.Run("network not found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)

		mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(nil, 0, errors.New("not found"))

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.associatePublicIPAddress()
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "could not find network") {
			t.Errorf("error message = %q, want to contain 'could not find network'", err.Error())
		}
	})

	t.Run("error associating IP", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		apiErr := errors.New("associate API error")

		gomock.InOrder(
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.associatePublicIPAddress()
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error associating new IP address") {
			t.Errorf("error message = %q, want to contain 'error associating new IP address'", err.Error())
		}
	})

	t.Run("project ID handling", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
			projectID: "proj-123",
		}

		err := lb.associatePublicIPAddress()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestReleaseLoadBalancerIP(t *testing.T) {
	t.Run("successful release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		disassociateParams := &cloudstack.DisassociateIpAddressParams{}

		gomock.InOrder(
			mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-123").Return(disassociateParams),
			mockAddress.EXPECT().DisassociateIpAddress(disassociateParams).Return(&cloudstack.DisassociateIpAddressResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
			ipAddrID: "ip-123",
			ipAddr:   "203.0.113.1",
		}

		err := lb.releaseLoadBalancerIP()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("error releasing IP", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		disassociateParams := &cloudstack.DisassociateIpAddressParams{}
		apiErr := errors.New("disassociate API error")

		gomock.InOrder(
			mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-123").Return(disassociateParams),
			mockAddress.EXPECT().DisassociateIpAddress(disassociateParams).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
			ipAddrID: "ip-123",
			ipAddr:   "203.0.113.1",
		}

		err := lb.releaseLoadBalancerIP()
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error releasing load balancer IP") {
			t.Errorf("error message = %q, want to contain 'error releasing load balancer IP'", err.Error())
		}
	})
}

func TestGetLoadBalancerIP(t *testing.T) {
	t.Run("IP specified - retrieve existing", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
					Allocated: "2023-01-01T00:00:00+0000",
				},
			},
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		err := lb.getLoadBalancerIP("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
	})

	t.Run("IP specified - associate unallocated", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
					Allocated: "",
				},
			},
		}

		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams),
			mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil),
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
			ipAddr:    "203.0.113.1",
		}

		err := lb.getLoadBalancerIP("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("no IP specified - associate new", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		networkResp := &cloudstack.Network{
			Id:      "net-123",
			Vpcid:   "",
			Service: []cloudstack.NetworkServiceInternal{},
		}

		associateParams := &cloudstack.AssociateIpAddressParams{}
		associateResp := &cloudstack.AssociateIpAddressResponse{
			Id:        "ip-123",
			Ipaddress: "203.0.113.1",
		}

		gomock.InOrder(
			mockNetwork.EXPECT().GetNetworkByID("net-123", gomock.Any()).Return(networkResp, 1, nil),
			mockAddress.EXPECT().NewAssociateIpAddressParams().Return(associateParams),
			mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(associateResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
				Network: mockNetwork,
			},
			networkID: "net-123",
		}

		err := lb.getLoadBalancerIP("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})
}

func TestCreateLoadBalancerRule(t *testing.T) {
	t.Run("create rule with default CIDR", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		createParams := &cloudstack.CreateLoadBalancerRuleParams{}
		createResp := &cloudstack.CreateLoadBalancerRuleResponse{
			Id:          "rule-123",
			Algorithm:   "roundrobin",
			Cidrlist:    defaultAllowedCIDR,
			Name:        "test-rule-tcp-80",
			Networkid:   "net-123",
			Privateport: "30000",
			Publicport:  "80",
			Publicip:    "203.0.113.1",
			Publicipid:  "ip-123",
			Protocol:    "tcp",
		}

		gomock.InOrder(
			mockLB.EXPECT().NewCreateLoadBalancerRuleParams("roundrobin", "test-rule-tcp-80", 30000, 80).Return(createParams),
			mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			algorithm: "roundrobin",
			networkID: "net-123",
			ipAddrID:  "ip-123",
			ipAddr:    "203.0.113.1",
		}

		port := corev1.ServicePort{
			Port:     80,
			NodePort: 30000,
			Protocol: corev1.ProtocolTCP,
		}

		rule, err := lb.createLoadBalancerRule("test-rule-tcp-80", port, LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.Id != "rule-123" {
			t.Errorf("rule.Id = %q, want %q", rule.Id, "rule-123")
		}
		if rule.Name != "test-rule-tcp-80" {
			t.Errorf("rule.Name = %q, want %q", rule.Name, "test-rule-tcp-80")
		}
	})

	t.Run("create rule with proxy protocol", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		createParams := &cloudstack.CreateLoadBalancerRuleParams{}
		createResp := &cloudstack.CreateLoadBalancerRuleResponse{
			Id:          "rule-123",
			Algorithm:   "roundrobin",
			Cidrlist:    defaultAllowedCIDR,
			Name:        "test-rule-tcp-proxy-80",
			Networkid:   "net-123",
			Privateport: "30000",
			Publicport:  "80",
			Publicip:    "203.0.113.1",
			Publicipid:  "ip-123",
			Protocol:    "tcp-proxy",
		}

		gomock.InOrder(
			mockLB.EXPECT().NewCreateLoadBalancerRuleParams("roundrobin", "test-rule-tcp-proxy-80", 30000, 80).Return(createParams),
			mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			algorithm: "roundrobin",
			networkID: "net-123",
			ipAddrID:  "ip-123",
			ipAddr:    "203.0.113.1",
		}

		port := corev1.ServicePort{
			Port:     80,
			NodePort: 30000,
			Protocol: corev1.ProtocolTCP,
		}

		rule, err := lb.createLoadBalancerRule("test-rule-tcp-proxy-80", port, LoadBalancerProtocolTCPProxy)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.Protocol != "tcp-proxy" {
			t.Errorf("rule.Protocol = %q, want %q", rule.Protocol, "tcp-proxy")
		}
	})

	t.Run("error creating rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		createParams := &cloudstack.CreateLoadBalancerRuleParams{}
		apiErr := errors.New("create rule API error")

		gomock.InOrder(
			mockLB.EXPECT().NewCreateLoadBalancerRuleParams("roundrobin", "test-rule-tcp-80", 30000, 80).Return(createParams),
			mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			algorithm: "roundrobin",
			networkID: "net-123",
			ipAddrID:  "ip-123",
			ipAddr:    "203.0.113.1",
		}

		port := corev1.ServicePort{
			Port:     80,
			NodePort: 30000,
			Protocol: corev1.ProtocolTCP,
		}
		_, err := lb.createLoadBalancerRule("test-rule-tcp-80", port, LoadBalancerProtocolTCP)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error creating load balancer rule") {
			t.Errorf("error message = %q, want to contain 'error creating load balancer rule'", err.Error())
		}
	})
}

func TestUpdateLoadBalancerRule(t *testing.T) {
	t.Run("update algorithm", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		updateParams := &cloudstack.UpdateLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewUpdateLoadBalancerRuleParams("rule-123").Return(updateParams),
			mockLB.EXPECT().UpdateLoadBalancerRule(gomock.Any()).Return(&cloudstack.UpdateLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			algorithm: "source",
			rules: map[string]*cloudstack.LoadBalancerRule{
				"test-rule-tcp-80": {
					Id:        "rule-123",
					Algorithm: "roundrobin",
					Protocol:  "tcp",
				},
			},
		}

		err := lb.updateLoadBalancerRule("test-rule-tcp-80", LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("update protocol", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		updateParams := &cloudstack.UpdateLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewUpdateLoadBalancerRuleParams("rule-123").Return(updateParams),
			mockLB.EXPECT().UpdateLoadBalancerRule(gomock.Any()).Return(&cloudstack.UpdateLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			algorithm: "roundrobin",
			rules: map[string]*cloudstack.LoadBalancerRule{
				"test-rule-tcp-80": {
					Id:        "rule-123",
					Algorithm: "roundrobin",
					Protocol:  "tcp",
				},
			},
		}

		err := lb.updateLoadBalancerRule("test-rule-tcp-80", LoadBalancerProtocolTCPProxy)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestDeleteLoadBalancerRule(t *testing.T) {
	t.Run("successful deletion", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		deleteParams := &cloudstack.DeleteLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("rule-123").Return(deleteParams),
			mockLB.EXPECT().DeleteLoadBalancerRule(deleteParams).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			rules: map[string]*cloudstack.LoadBalancerRule{
				"test-rule": {
					Id:   "rule-123",
					Name: "test-rule",
				},
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.deleteLoadBalancerRule(rule)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, exists := lb.rules["test-rule"]; exists {
			t.Errorf("expected rule to be removed from map")
		}
	})

	t.Run("error deleting rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		deleteParams := &cloudstack.DeleteLoadBalancerRuleParams{}
		apiErr := errors.New("delete rule API error")

		gomock.InOrder(
			mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("rule-123").Return(deleteParams),
			mockLB.EXPECT().DeleteLoadBalancerRule(deleteParams).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			rules: map[string]*cloudstack.LoadBalancerRule{
				"test-rule": {
					Id:   "rule-123",
					Name: "test-rule",
				},
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.deleteLoadBalancerRule(rule)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error deleting load balancer rule") {
			t.Errorf("error message = %q, want to contain 'error deleting load balancer rule'", err.Error())
		}
	})
}

func TestAssignHostsToRule(t *testing.T) {
	t.Run("successful assignment", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-123").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.assignHostsToRule(rule, []string{"vm-1", "vm-2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("error assigning hosts", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}
		apiErr := errors.New("assign API error")

		gomock.InOrder(
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-123").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.assignHostsToRule(rule, []string{"vm-1"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error assigning hosts") {
			t.Errorf("error message = %q, want to contain 'error assigning hosts'", err.Error())
		}
	})

	t.Run("empty host list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-123").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.assignHostsToRule(rule, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestRemoveHostsFromRule(t *testing.T) {
	t.Run("successful removal", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		removeParams := &cloudstack.RemoveFromLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewRemoveFromLoadBalancerRuleParams("rule-123").Return(removeParams),
			mockLB.EXPECT().RemoveFromLoadBalancerRule(gomock.Any()).Return(&cloudstack.RemoveFromLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.removeHostsFromRule(rule, []string{"vm-1", "vm-2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("error removing hosts", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		removeParams := &cloudstack.RemoveFromLoadBalancerRuleParams{}
		apiErr := errors.New("remove API error")

		gomock.InOrder(
			mockLB.EXPECT().NewRemoveFromLoadBalancerRuleParams("rule-123").Return(removeParams),
			mockLB.EXPECT().RemoveFromLoadBalancerRule(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.removeHostsFromRule(rule, []string{"vm-1"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error removing hosts") {
			t.Errorf("error message = %q, want to contain 'error removing hosts'", err.Error())
		}
	})

	t.Run("empty host list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		removeParams := &cloudstack.RemoveFromLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewRemoveFromLoadBalancerRuleParams("rule-123").Return(removeParams),
			mockLB.EXPECT().RemoveFromLoadBalancerRule(gomock.Any()).Return(&cloudstack.RemoveFromLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{
			Id:   "rule-123",
			Name: "test-rule",
		}

		err := lb.removeHostsFromRule(rule, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestUpdateFirewallRule(t *testing.T) {
	t.Run("create new firewall rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count:         0,
			FirewallRules: []*cloudstack.FirewallRule{},
		}

		createParams := &cloudstack.CreateFirewallRuleParams{}
		createResp := &cloudstack.CreateFirewallRuleResponse{
			Id: "fw-123",
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-123", "tcp").Return(createParams),
			mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		updated, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !updated {
			t.Errorf("updated = false, want true")
		}
	})

	t.Run("rule already exists - no change", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count: 1,
			FirewallRules: []*cloudstack.FirewallRule{
				{
					Id:          "fw-123",
					Protocol:    "tcp",
					Startport:   80,
					Endport:     80,
					Cidrlist:    "10.0.0.0/8",
					Ipaddress:   "203.0.113.1",
					Ipaddressid: "ip-123",
				},
			},
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		updated, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated {
			t.Errorf("updated = true, want false (nothing changed)")
		}
	})

	t.Run("update existing rule - CIDR change", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count: 1,
			FirewallRules: []*cloudstack.FirewallRule{
				{
					Id:          "fw-123",
					Protocol:    "tcp",
					Startport:   80,
					Endport:     80,
					Cidrlist:    "192.168.0.0/16",
					Ipaddress:   "203.0.113.1",
					Ipaddressid: "ip-123",
				},
			},
		}

		deleteParams := &cloudstack.DeleteFirewallRuleParams{}
		createParams := &cloudstack.CreateFirewallRuleParams{}
		createResp := &cloudstack.CreateFirewallRuleResponse{
			Id: "fw-124",
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewDeleteFirewallRuleParams("fw-123").Return(deleteParams),
			mockFirewall.EXPECT().DeleteFirewallRule(deleteParams).Return(&cloudstack.DeleteFirewallRuleResponse{}, nil),
			mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-123", "tcp").Return(createParams),
			mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		updated, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !updated {
			t.Errorf("updated = false, want true")
		}
	})

	t.Run("default CIDR when empty list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count:         0,
			FirewallRules: []*cloudstack.FirewallRule{},
		}

		createParams := &cloudstack.CreateFirewallRuleParams{}
		createResp := &cloudstack.CreateFirewallRuleResponse{
			Id: "fw-123",
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-123", "tcp").Return(createParams),
			mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		updated, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !updated {
			t.Errorf("updated = false, want true")
		}
	})

	t.Run("error listing rules", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		apiErr := errors.New("list API error")

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		_, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error fetching firewall rules") {
			t.Errorf("error message = %q, want to contain 'error fetching firewall rules'", err.Error())
		}
	})

	t.Run("error creating rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count:         0,
			FirewallRules: []*cloudstack.FirewallRule{},
		}

		createParams := &cloudstack.CreateFirewallRuleParams{}
		apiErr := errors.New("create API error")

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-123", "tcp").Return(createParams),
			mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		_, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error creating new firewall rule") {
			t.Errorf("error message = %q, want to contain 'error creating new firewall rule'", err.Error())
		}
	})

	t.Run("error deleting rule - continues", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count: 1,
			FirewallRules: []*cloudstack.FirewallRule{
				{
					Id:          "fw-123",
					Protocol:    "tcp",
					Startport:   80,
					Endport:     80,
					Cidrlist:    "192.168.0.0/16",
					Ipaddress:   "203.0.113.1",
					Ipaddressid: "ip-123",
				},
			},
		}

		deleteParams := &cloudstack.DeleteFirewallRuleParams{}
		deleteErr := errors.New("delete API error")
		createParams := &cloudstack.CreateFirewallRuleParams{}
		createResp := &cloudstack.CreateFirewallRuleResponse{
			Id: "fw-124",
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewDeleteFirewallRuleParams("fw-123").Return(deleteParams),
			mockFirewall.EXPECT().DeleteFirewallRule(deleteParams).Return(nil, deleteErr),
			mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-123", "tcp").Return(createParams),
			mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(createResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
			ipAddr: "203.0.113.1",
		}

		updated, err := lb.updateFirewallRule("ip-123", 80, LoadBalancerProtocolTCP, []string{"10.0.0.0/8"})
		// Should still return true even if delete failed, but the deletion error should be surfaced
		if err == nil {
			t.Fatalf("expected deletion error to be returned, got nil")
		}
		if !strings.Contains(err.Error(), "delete API error") {
			t.Fatalf("expected deletion error, got: %v", err)
		}
		if !updated {
			t.Errorf("updated = false, want true")
		}
	})
}

func TestDeleteFirewallRule(t *testing.T) {
	t.Run("delete matching rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count: 1,
			FirewallRules: []*cloudstack.FirewallRule{
				{
					Id:          "fw-123",
					Protocol:    "tcp",
					Startport:   80,
					Endport:     80,
					Ipaddressid: "ip-123",
				},
			},
		}

		deleteParams := &cloudstack.DeleteFirewallRuleParams{}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewDeleteFirewallRuleParams("fw-123").Return(deleteParams),
			mockFirewall.EXPECT().DeleteFirewallRule(deleteParams).Return(&cloudstack.DeleteFirewallRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
		}

		deleted, err := lb.deleteFirewallRule("ip-123", 80, LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !deleted {
			t.Errorf("deleted = false, want true")
		}
	})

	t.Run("no matching rules", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count:         0,
			FirewallRules: []*cloudstack.FirewallRule{},
		}

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
		}

		deleted, err := lb.deleteFirewallRule("ip-123", 80, LoadBalancerProtocolTCP)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deleted {
			t.Errorf("deleted = true, want false")
		}
	})

	t.Run("error listing rules", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		apiErr := errors.New("list API error")

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(nil, apiErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
		}

		_, err := lb.deleteFirewallRule("ip-123", 80, LoadBalancerProtocolTCP)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error fetching firewall rules") {
			t.Errorf("error message = %q, want to contain 'error fetching firewall rules'", err.Error())
		}
	})

	t.Run("error deleting rule", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)
		listParams := &cloudstack.ListFirewallRulesParams{}
		listResp := &cloudstack.ListFirewallRulesResponse{
			Count: 1,
			FirewallRules: []*cloudstack.FirewallRule{
				{
					Id:          "fw-123",
					Protocol:    "tcp",
					Startport:   80,
					Endport:     80,
					Ipaddressid: "ip-123",
				},
			},
		}

		deleteParams := &cloudstack.DeleteFirewallRuleParams{}
		deleteErr := errors.New("delete API error")

		gomock.InOrder(
			mockFirewall.EXPECT().NewListFirewallRulesParams().Return(listParams),
			mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(listResp, nil),
			mockFirewall.EXPECT().NewDeleteFirewallRuleParams("fw-123").Return(deleteParams),
			mockFirewall.EXPECT().DeleteFirewallRule(deleteParams).Return(nil, deleteErr),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Firewall: mockFirewall,
			},
		}

		deleted, err := lb.deleteFirewallRule("ip-123", 80, LoadBalancerProtocolTCP)
		// Should return false if deletion failed
		if deleted {
			t.Errorf("deleted = true, want false")
		}
		if !errors.Is(err, deleteErr) {
			t.Errorf("error = %v, want %v", err, deleteErr)
		}
	})
}

func TestVerifyHosts(t *testing.T) {
	t.Run("all hosts in same network", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 2,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
				{
					Id:   "vm-2",
					Name: "node-2",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
		}

		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hostIDs) != 2 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 2)
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("hosts in different networks", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 2,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
				{
					Id:   "vm-2",
					Name: "node-2",
					Nic: []cloudstack.Nic{
						{Networkid: "net-456"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
		}

		_, _, err := cs.verifyHosts(nodes)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "different networks") {
			t.Errorf("error message = %q, want to contain 'different networks'", err.Error())
		}
	})

	t.Run("no matching hosts", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count:           0,
			VirtualMachines: []*cloudstack.VirtualMachine{},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		_, _, err := cs.verifyHosts(nodes)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "could not match any") {
			t.Errorf("error message = %q, want to contain 'could not match any'", err.Error())
		}
	})

	t.Run("FQDN node names", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 1,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1.example.com"}},
		}

		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hostIDs) != 1 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 1)
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("case-insensitive matching", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 1,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "NODE-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hostIDs) != 1 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 1)
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("partial match during rolling upgrade - some nodes not yet in CloudStack", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		// Only node-1 exists in CloudStack; node-2 is still being provisioned.
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 1,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}, // Not yet in CloudStack
		}

		// Should succeed with partial match - only node-1 matched
		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error (should tolerate partial match): %v", err)
		}
		if len(hostIDs) != 1 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 1)
		}
		if hostIDs[0] != "vm-1" {
			t.Errorf("hostIDs[0] = %q, want %q", hostIDs[0], "vm-1")
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("partial match - VM exists but has no NICs yet", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 2,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
				{
					Id:   "vm-2",
					Name: "node-2",
					Nic:  []cloudstack.Nic{}, // No NICs yet - still provisioning
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
		}

		// Should succeed with partial match - node-2 skipped due to no NICs
		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error (should tolerate VM with no NICs): %v", err)
		}
		if len(hostIDs) != 1 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 1)
		}
		if hostIDs[0] != "vm-1" {
			t.Errorf("hostIDs[0] = %q, want %q", hostIDs[0], "vm-1")
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("match by ProviderID when name does not match", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 1,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-abc-123",
					Name: "different-name", // Name does not match node name
					Nic: []cloudstack.Nic{
						{Networkid: "net-123"},
					},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
				Spec: corev1.NodeSpec{
					ProviderID: "cloudstack:///vm-abc-123", // Matches by VM ID
				},
			},
		}

		hostIDs, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hostIDs) != 1 {
			t.Errorf("hostIDs count = %d, want %d", len(hostIDs), 1)
		}
		if hostIDs[0] != "vm-abc-123" {
			t.Errorf("hostIDs[0] = %q, want %q", hostIDs[0], "vm-abc-123")
		}
		if networkID != "net-123" {
			t.Errorf("networkID = %q, want %q", networkID, "net-123")
		}
	})

	t.Run("all VMs have no NICs - should error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		listParams := &cloudstack.ListVirtualMachinesParams{}
		listResp := &cloudstack.ListVirtualMachinesResponse{
			Count: 2,
			VirtualMachines: []*cloudstack.VirtualMachine{
				{
					Id:   "vm-1",
					Name: "node-1",
					Nic:  []cloudstack.Nic{},
				},
				{
					Id:   "vm-2",
					Name: "node-2",
					Nic:  []cloudstack.Nic{},
				},
			},
		}

		mockVM.EXPECT().NewListVirtualMachinesParams().Return(listParams)
		mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				VirtualMachine: mockVM,
			},
		}

		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
		}

		// Should error - all VMs have no NICs, zero backends
		_, _, err := cs.verifyHosts(nodes)
		if err == nil {
			t.Fatalf("expected error when all VMs have no NICs")
		}
		if !strings.Contains(err.Error(), "could not match any") {
			t.Errorf("error message = %q, want to contain 'could not match any'", err.Error())
		}
	})
}

func TestReconcileHostsForRule(t *testing.T) {
	t.Run("hosts already correct - no-op", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRuleInstancesParams{}

		mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams("rule-1").Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
			Count: 2,
			LoadBalancerRuleInstances: []*cloudstack.VirtualMachine{
				{Id: "vm-1"},
				{Id: "vm-2"},
			},
		}, nil)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{Id: "rule-1", Name: "test-rule"}
		err := lb.reconcileHostsForRule(rule, []string{"vm-1", "vm-2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing hosts get assigned", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRuleInstancesParams{}
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams("rule-1").Return(listParams),
			mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
				Count:                     0,
				LoadBalancerRuleInstances: []*cloudstack.VirtualMachine{},
			}, nil),
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-1").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{Id: "rule-1", Name: "test-rule"}
		err := lb.reconcileHostsForRule(rule, []string{"vm-1", "vm-2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("stale hosts removed and new hosts assigned", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRuleInstancesParams{}
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}
		removeParams := &cloudstack.RemoveFromLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams("rule-1").Return(listParams),
			mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
				Count: 2,
				LoadBalancerRuleInstances: []*cloudstack.VirtualMachine{
					{Id: "vm-old-1"},
					{Id: "vm-old-2"},
				},
			}, nil),
			// Assign new hosts BEFORE removing old ones
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-1").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil),
			mockLB.EXPECT().NewRemoveFromLoadBalancerRuleParams("rule-1").Return(removeParams),
			mockLB.EXPECT().RemoveFromLoadBalancerRule(gomock.Any()).Return(&cloudstack.RemoveFromLoadBalancerRuleResponse{}, nil),
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{Id: "rule-1", Name: "test-rule"}
		err := lb.reconcileHostsForRule(rule, []string{"vm-new-1", "vm-new-2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("assign failure preserves old hosts", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRuleInstancesParams{}
		assignParams := &cloudstack.AssignToLoadBalancerRuleParams{}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams("rule-1").Return(listParams),
			mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
				Count: 1,
				LoadBalancerRuleInstances: []*cloudstack.VirtualMachine{
					{Id: "vm-old"},
				},
			}, nil),
			mockLB.EXPECT().NewAssignToLoadBalancerRuleParams("rule-1").Return(assignParams),
			mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(nil, errors.New("assign API error")),
			// removeHostsFromRule should NOT be called because assign failed
		)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{Id: "rule-1", Name: "test-rule"}
		err := lb.reconcileHostsForRule(rule, []string{"vm-new"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "old hosts preserved") {
			t.Errorf("error message = %q, want to contain 'old hosts preserved'", err.Error())
		}
	})

	t.Run("list instances error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRuleInstancesParams{}

		mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams("rule-1").Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(nil, errors.New("API error"))

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		rule := &cloudstack.LoadBalancerRule{Id: "rule-1", Name: "test-rule"}
		err := lb.reconcileHostsForRule(rule, []string{"vm-1"})
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error retrieving associated instances") {
			t.Errorf("error message = %q, want to contain 'error retrieving associated instances'", err.Error())
		}
	})
}

// --- Fix A tests ---

func TestFilterRulesByPrefix(t *testing.T) {
	tests := []struct {
		name   string
		rules  []*cloudstack.LoadBalancerRule
		prefix string
		want   []string // expected rule names
	}{
		{
			name: "exact prefix match",
			rules: []*cloudstack.LoadBalancerRule{
				{Name: "K8s_svc_c_ns_foo-tcp-80"},
				{Name: "K8s_svc_c_ns_foo-tcp-443"},
			},
			prefix: "K8s_svc_c_ns_foo-",
			want:   []string{"K8s_svc_c_ns_foo-tcp-80", "K8s_svc_c_ns_foo-tcp-443"},
		},
		{
			name: "filters out superset names",
			rules: []*cloudstack.LoadBalancerRule{
				{Name: "K8s_svc_c_ns_foo-tcp-80"},
				{Name: "K8s_svc_c_ns_foobar-tcp-80"},
			},
			prefix: "K8s_svc_c_ns_foo-",
			want:   []string{"K8s_svc_c_ns_foo-tcp-80"},
		},
		{
			name:   "empty input",
			rules:  []*cloudstack.LoadBalancerRule{},
			prefix: "K8s_svc_c_ns_foo-",
			want:   nil,
		},
		{
			name: "no matches",
			rules: []*cloudstack.LoadBalancerRule{
				{Name: "K8s_svc_c_ns_bar-tcp-80"},
				{Name: "K8s_svc_c_ns_baz-tcp-80"},
			},
			prefix: "K8s_svc_c_ns_foo-",
			want:   nil,
		},
		{
			name:   "nil input",
			rules:  nil,
			prefix: "K8s_svc_c_ns_foo-",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterRulesByPrefix(tt.rules, tt.prefix)
			var gotNames []string
			for _, r := range got {
				gotNames = append(gotNames, r.Name)
			}
			if len(gotNames) != len(tt.want) {
				t.Fatalf("filterRulesByPrefix() returned %d rules, want %d: got %v", len(gotNames), len(tt.want), gotNames)
			}
			for i, name := range gotNames {
				if name != tt.want[i] {
					t.Errorf("filterRulesByPrefix()[%d].Name = %q, want %q", i, name, tt.want[i])
				}
			}
		})
	}
}

func TestGetLoadBalancerByNameFiltering(t *testing.T) {
	t.Run("keyword results filtered to exact prefix", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		// CloudStack returns both "foo" and "foobar" rules due to LIKE matching
		listResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 2,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "K8s_svc_c_ns_foo-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1"},
				{Name: "K8s_svc_c_ns_foobar-tcp-80", Publicip: "5.6.7.8", Publicipid: "ip-2"},
			},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		lb, err := cs.getLoadBalancerByName("K8s_svc_c_ns_foo", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if _, ok := lb.rules["K8s_svc_c_ns_foo-tcp-80"]; !ok {
			t.Errorf("expected rule K8s_svc_c_ns_foo-tcp-80 to be present")
		}
		if lb.ipAddr != "1.2.3.4" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "1.2.3.4")
		}
	})

	t.Run("legacy fallback with filtering", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		// First call (modern name) returns foobar rules but NOT foo rules → filtered to 0
		modernResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "K8s_svc_c_ns_foobar-tcp-80", Publicip: "5.6.7.8", Publicipid: "ip-2"},
			},
		}

		// Second call (legacy name) returns matching rule
		legacyResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "a1b2c3d4-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1"},
			},
		}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(modernResp, nil),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(legacyResp, nil),
		)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		lb, err := cs.getLoadBalancerByName("K8s_svc_c_ns_foo", "a1b2c3d4")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.name != "a1b2c3d4" {
			t.Errorf("lb.name = %q, want %q", lb.name, "a1b2c3d4")
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if lb.ipAddr != "1.2.3.4" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "1.2.3.4")
		}
	})

	t.Run("legacy results also filtered", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		// First call: no matching rules after filtering
		modernResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count:             0,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}

		// Second call (legacy): returns both exact match and superset
		legacyResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 2,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "a1b2-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1"},
				{Name: "a1b2c3d4-tcp-80", Publicip: "5.6.7.8", Publicipid: "ip-2"},
			},
		}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(modernResp, nil),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(legacyResp, nil),
		)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		lb, err := cs.getLoadBalancerByName("K8s_svc_c_ns_foo", "a1b2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lb.name != "a1b2" {
			t.Errorf("lb.name = %q, want %q", lb.name, "a1b2")
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if _, ok := lb.rules["a1b2-tcp-80"]; !ok {
			t.Errorf("expected rule a1b2-tcp-80 to be present")
		}
	})
}

// --- Fix B tests ---

func TestLookupPublicIPAddress(t *testing.T) {
	t.Run("found and allocated", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
				},
			},
		}

		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams)
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		found, err := lb.lookupPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatalf("expected found = true")
		}
		if lb.ipAddr != "203.0.113.1" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "203.0.113.1")
		}
		if lb.ipAddrID != "ip-123" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-123")
		}
	})

	t.Run("not found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count:             0,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{},
		}

		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams)
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		found, err := lb.lookupPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if found {
			t.Fatalf("expected found = false")
		}
		if lb.ipAddr != "" {
			t.Errorf("ipAddr should be empty, got %q", lb.ipAddr)
		}
	})

	t.Run("API error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}

		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams)
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(nil, errors.New("API error"))

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
		}

		found, err := lb.lookupPublicIPAddress("203.0.113.1")
		if err == nil {
			t.Fatalf("expected error")
		}
		if found {
			t.Fatalf("expected found = false on error")
		}
		if !strings.Contains(err.Error(), "error looking up IP address") {
			t.Errorf("error = %q, want to contain 'error looking up IP address'", err.Error())
		}
	})

	t.Run("project ID propagation", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		listParams := &cloudstack.ListPublicIpAddressesParams{}
		resp := &cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{
					Id:        "ip-123",
					Ipaddress: "203.0.113.1",
				},
			},
		}

		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(listParams)
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(resp, nil)

		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				Address: mockAddress,
			},
			projectID: "proj-456",
		}

		found, err := lb.lookupPublicIPAddress("203.0.113.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found {
			t.Fatalf("expected found = true")
		}
	})
}

// --- Fix C tests ---

// setupGetLoadBalancerByNameEmpty sets up mock expectations for getLoadBalancerByName
// when it should return an empty result (no matching LB rules). This requires two
// ListLoadBalancerRules calls: one for the modern name and one for the legacy name.
func setupGetLoadBalancerByNameEmpty(mockLB *cloudstack.MockLoadBalancerServiceIface) {
	emptyResp := &cloudstack.ListLoadBalancerRulesResponse{Count: 0, LoadBalancerRules: []*cloudstack.LoadBalancerRule{}}
	// Modern name call
	mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
	mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(emptyResp, nil)
	// Legacy name fallback call
	mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(emptyResp, nil)
}

func TestEnsureLoadBalancerDeletedOrphanedIP(t *testing.T) {
	t.Run("orphaned IP released via annotation", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)

		// getLoadBalancerByName returns no rules (2 ListLoadBalancerRules calls)
		setupGetLoadBalancerByNameEmpty(mockLB)

		// lookupPublicIPAddress finds the orphaned IP
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-orphan", Ipaddress: "10.0.0.1"},
			},
		}, nil)

		// shouldReleaseLoadBalancerIP: no other rules on this IP
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0, LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}, nil)

		// releaseLoadBalancerIP
		mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-orphan").Return(&cloudstack.DisassociateIpAddressParams{})
		mockAddress.EXPECT().DisassociateIpAddress(gomock.Any()).Return(&cloudstack.DisassociateIpAddressResponse{}, nil)

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
				},
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no annotation returns nil", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		setupGetLoadBalancerByNameEmpty(mockLB)

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("IP already gone returns nil", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)

		// lookupPublicIPAddress: not found
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 0, PublicIpAddresses: []*cloudstack.PublicIpAddress{},
		}, nil)

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
				},
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("release fails returns error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)

		// lookupPublicIPAddress
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-orphan", Ipaddress: "10.0.0.1"},
			},
		}, nil)

		// shouldRelease: yes
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0, LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}, nil)

		// releaseLoadBalancerIP fails
		mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-orphan").Return(&cloudstack.DisassociateIpAddressParams{})
		mockAddress.EXPECT().DisassociateIpAddress(gomock.Any()).Return(nil, errors.New("release failed"))

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
				},
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "error releasing orphaned load balancer IP") {
			t.Errorf("error = %q, want to contain 'error releasing orphaned load balancer IP'", err.Error())
		}
	})

	t.Run("keep-ip annotation prevents release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)

		// lookupPublicIPAddress
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-user", Ipaddress: "10.0.0.1"},
			},
		}, nil)
		// shouldReleaseLoadBalancerIP returns false because keep-ip annotation is set

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
					ServiceAnnotationLoadBalancerKeepIP:  "true",
				},
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestEnsureLoadBalancerDeletedAnnotationCleanup(t *testing.T) {
	// allLBAnnotations returns a map with all 6 CloudStack LB annotations set.
	allLBAnnotations := func() map[string]string {
		return map[string]string{
			ServiceAnnotationLoadBalancerProxyProtocol:        "true",
			ServiceAnnotationLoadBalancerLoadbalancerHostname: "lb.example.com",
			ServiceAnnotationLoadBalancerAddress:              "10.0.0.1",
			ServiceAnnotationLoadBalancerKeepIP:               "false",
			ServiceAnnotationLoadBalancerID:                   "ip-1",
			ServiceAnnotationLoadBalancerNetworkID:            "net-1",
		}
	}

	lbAnnotationKeys := []string{
		ServiceAnnotationLoadBalancerProxyProtocol,
		ServiceAnnotationLoadBalancerLoadbalancerHostname,
		ServiceAnnotationLoadBalancerAddress,
		ServiceAnnotationLoadBalancerKeepIP,
		ServiceAnnotationLoadBalancerID,
		ServiceAnnotationLoadBalancerNetworkID,
	}

	t.Run("annotations removed after successful deletion", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		// getLoadBalancerByName returns one rule
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{
					Id: "rule-1", Name: "K8s_svc_cluster_default_foo-tcp-80",
					Publicip: "10.0.0.1", Publicipid: "ip-1", Publicport: "80",
					Protocol: "tcp", Networkid: "net-1",
				},
			},
		}, nil)

		// deleteFirewallRule
		mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{})
		mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{
			Count: 0, FirewallRules: []*cloudstack.FirewallRule{},
		}, nil)

		// deleteLoadBalancerRule
		mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("rule-1").Return(&cloudstack.DeleteLoadBalancerRuleParams{})
		mockLB.EXPECT().DeleteLoadBalancerRule(gomock.Any()).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil)

		// shouldReleaseLoadBalancerIP: no keep-ip, no other rules
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0, LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}, nil)

		// releaseLoadBalancerIP
		mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-1").Return(&cloudstack.DisassociateIpAddressParams{})
		mockAddress.EXPECT().DisassociateIpAddress(gomock.Any()).Return(&cloudstack.DisassociateIpAddressResponse{}, nil)

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "foo",
				Namespace:   "default",
				Annotations: allLBAnnotations(),
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
				Firewall:     mockFirewall,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for _, key := range lbAnnotationKeys {
			if _, ok := service.Annotations[key]; ok {
				t.Errorf("annotation %q should have been removed", key)
			}
		}
	})

	t.Run("annotations preserved when deletion fails", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		// getLoadBalancerByName returns one rule
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{}).Times(2)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{
					Id: "rule-1", Name: "K8s_svc_cluster_default_foo-tcp-80",
					Publicip: "10.0.0.1", Publicipid: "ip-1", Publicport: "80",
					Protocol: "tcp", Networkid: "net-1",
				},
			},
		}, nil).Times(1)

		// deleteFirewallRule fails
		mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{})
		mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(nil, errors.New("firewall error"))

		// deleteLoadBalancerRule fails
		mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("rule-1").Return(&cloudstack.DeleteLoadBalancerRuleParams{})
		mockLB.EXPECT().DeleteLoadBalancerRule(gomock.Any()).Return(nil, errors.New("delete rule error"))

		// shouldReleaseLoadBalancerIP is still called (IP cleanup attempted even on rule errors)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0, LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}, nil)

		// releaseLoadBalancerIP also fails
		mockAddress.EXPECT().NewDisassociateIpAddressParams("ip-1").Return(&cloudstack.DisassociateIpAddressParams{})
		mockAddress.EXPECT().DisassociateIpAddress(gomock.Any()).Return(nil, errors.New("release failed"))

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "foo",
				Namespace:   "default",
				Annotations: allLBAnnotations(),
			},
		}

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
				Address:      mockAddress,
				Firewall:     mockFirewall,
			},
			kclient:       fake.NewSimpleClientset(service),
			eventRecorder: record.NewFakeRecorder(10),
		}

		err := cs.EnsureLoadBalancerDeleted(t.Context(), "cluster", service)
		if err == nil {
			t.Fatalf("expected error")
		}

		for _, key := range lbAnnotationKeys {
			if _, ok := service.Annotations[key]; !ok {
				t.Errorf("annotation %q should have been preserved on error", key)
			}
		}
	})
}

// --- Fix D tests ---

// newTestCSCloud creates a minimal CSCloud with mocks for EnsureLoadBalancer tests.
// The provided service is pre-created in the fake clientset so the service patcher can find it.
func newTestCSCloud(mockLB *cloudstack.MockLoadBalancerServiceIface, mockAddress *cloudstack.MockAddressServiceIface, mockVM *cloudstack.MockVirtualMachineServiceIface, mockNetwork *cloudstack.MockNetworkServiceIface, mockFirewall *cloudstack.MockFirewallServiceIface, service *corev1.Service) *CSCloud {
	return &CSCloud{
		client: &cloudstack.CloudStackClient{
			LoadBalancer:   mockLB,
			Address:        mockAddress,
			VirtualMachine: mockVM,
			Network:        mockNetwork,
			Firewall:       mockFirewall,
		},
		kclient:       fake.NewSimpleClientset(service),
		eventRecorder: record.NewFakeRecorder(10),
	}
}

// setupVerifyHosts sets up mock expectations for verifyHosts returning one node.
func setupVerifyHosts(mockVM *cloudstack.MockVirtualMachineServiceIface) {
	mockVM.EXPECT().NewListVirtualMachinesParams().Return(&cloudstack.ListVirtualMachinesParams{})
	mockVM.EXPECT().ListVirtualMachines(gomock.Any()).Return(&cloudstack.ListVirtualMachinesResponse{
		Count: 1,
		VirtualMachines: []*cloudstack.VirtualMachine{
			{Id: "vm-1", Name: "node-1", Nic: []cloudstack.Nic{{Networkid: "net-1"}}},
		},
	}, nil)
}

// setupCreateRuleAndFirewall sets up mock expectations for creating one LB rule
// with firewall, which is the common tail of EnsureLoadBalancer tests.
func setupCreateRuleAndFirewall(mockLB *cloudstack.MockLoadBalancerServiceIface, mockNetwork *cloudstack.MockNetworkServiceIface, mockFirewall *cloudstack.MockFirewallServiceIface, ip, ipID string) {
	mockLB.EXPECT().NewCreateLoadBalancerRuleParams(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleParams{})
	mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleResponse{
		Id: "rule-1", Algorithm: "roundrobin", Name: "K8s_svc_cluster_default_foo-tcp-80",
		Networkid: "net-1", Privateport: "30080", Publicport: "80",
		Publicip: ip, Publicipid: ipID, Protocol: "tcp",
	}, nil)
	mockLB.EXPECT().NewAssignToLoadBalancerRuleParams(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleParams{})
	mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil)

	mockNetwork.EXPECT().GetNetworkByID("net-1", gomock.Any()).Return(&cloudstack.Network{
		Id: "net-1", Service: []cloudstack.NetworkServiceInternal{{Name: "Firewall"}},
	}, 1, nil)
	mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{})
	mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{Count: 0, FirewallRules: []*cloudstack.FirewallRule{}}, nil)
	mockFirewall.EXPECT().NewCreateFirewallRuleParams(gomock.Any(), gomock.Any()).Return(&cloudstack.CreateFirewallRuleParams{})
	mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(&cloudstack.CreateFirewallRuleResponse{Id: "fw-1"}, nil)
}

func TestEnsureLoadBalancerAnnotationRecovery(t *testing.T) {
	t.Run("recovers annotated IP on retry", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		// getLoadBalancerByName: no rules (2 LB list calls: modern + legacy)
		setupGetLoadBalancerByNameEmpty(mockLB)
		setupVerifyHosts(mockVM)

		// lookupPublicIPAddress: finds the annotated IP
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-recovered", Ipaddress: "10.0.0.1"},
			},
		}, nil)

		setupCreateRuleAndFirewall(mockLB, mockNetwork, mockFirewall, "10.0.0.1", "ip-recovered")

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
				},
				SessionAffinity: corev1.ServiceAffinityNone,
			},
		}
		cs := newTestCSCloud(mockLB, mockAddress, mockVM, mockNetwork, mockFirewall, service)
		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		status, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status == nil || len(status.Ingress) == 0 {
			t.Fatalf("expected non-empty status")
		}
		if status.Ingress[0].IP != "10.0.0.1" {
			t.Errorf("status IP = %q, want %q", status.Ingress[0].IP, "10.0.0.1")
		}
	})

	t.Run("auto-allocates when no IP specified", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)
		setupVerifyHosts(mockVM)

		// No annotation, no spec.LoadBalancerIP → auto-allocate via associatePublicIPAddress
		// GetNetworkByID called twice: once for associatePublicIPAddress, once for firewall check
		mockNetwork.EXPECT().GetNetworkByID("net-1", gomock.Any()).Return(&cloudstack.Network{
			Id: "net-1", Service: []cloudstack.NetworkServiceInternal{{Name: "Firewall"}},
		}, 1, nil).Times(2)

		mockAddress.EXPECT().NewAssociateIpAddressParams().Return(&cloudstack.AssociateIpAddressParams{})
		mockAddress.EXPECT().AssociateIpAddress(gomock.Any()).Return(&cloudstack.AssociateIpAddressResponse{
			Id: "ip-new", Ipaddress: "10.0.0.2",
		}, nil)

		// createLoadBalancerRule + firewall (but GetNetworkByID already set up above)
		mockLB.EXPECT().NewCreateLoadBalancerRuleParams(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleParams{})
		mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleResponse{
			Id: "rule-1", Algorithm: "roundrobin", Name: "K8s_svc_cluster_default_foo-tcp-80",
			Networkid: "net-1", Privateport: "30080", Publicport: "80",
			Publicip: "10.0.0.2", Publicipid: "ip-new", Protocol: "tcp",
		}, nil)
		mockLB.EXPECT().NewAssignToLoadBalancerRuleParams(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleParams{})
		mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil)

		mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{})
		mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{Count: 0, FirewallRules: []*cloudstack.FirewallRule{}}, nil)
		mockFirewall.EXPECT().NewCreateFirewallRuleParams(gomock.Any(), gomock.Any()).Return(&cloudstack.CreateFirewallRuleParams{})
		mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).Return(&cloudstack.CreateFirewallRuleResponse{Id: "fw-1"}, nil)

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
				},
				SessionAffinity: corev1.ServiceAffinityNone,
			},
		}
		cs := newTestCSCloud(mockLB, mockAddress, mockVM, mockNetwork, mockFirewall, service)
		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		status, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status == nil || len(status.Ingress) == 0 {
			t.Fatalf("expected non-empty status")
		}
		if status.Ingress[0].IP != "10.0.0.2" {
			t.Errorf("status IP = %q, want %q (new allocation)", status.Ingress[0].IP, "10.0.0.2")
		}
	})

	t.Run("annotation-specified IP is allocated on fresh LB", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)
		setupVerifyHosts(mockVM)

		// lookupPublicIPAddress for annotated IP: found (already allocated)
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-new", Ipaddress: "10.0.0.2", Allocated: "2023-01-01"},
			},
		}, nil)

		setupCreateRuleAndFirewall(mockLB, mockNetwork, mockFirewall, "10.0.0.2", "ip-new")

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.2", // user-specified desired IP
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
				},
				SessionAffinity: corev1.ServiceAffinityNone,
			},
		}
		cs := newTestCSCloud(mockLB, mockAddress, mockVM, mockNetwork, mockFirewall, service)
		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		status, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status == nil || len(status.Ingress) == 0 {
			t.Fatalf("expected non-empty status")
		}
		if status.Ingress[0].IP != "10.0.0.2" {
			t.Errorf("status IP = %q, want %q", status.Ingress[0].IP, "10.0.0.2")
		}
	})

	t.Run("spec.LoadBalancerIP fallback used when no annotation", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockAddress := cloudstack.NewMockAddressServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		setupGetLoadBalancerByNameEmpty(mockLB)
		setupVerifyHosts(mockVM)

		// getLoadBalancerIP for spec IP (10.0.0.2) → getPublicIPAddress
		mockAddress.EXPECT().NewListPublicIpAddressesParams().Return(&cloudstack.ListPublicIpAddressesParams{})
		mockAddress.EXPECT().ListPublicIpAddresses(gomock.Any()).Return(&cloudstack.ListPublicIpAddressesResponse{
			Count: 1,
			PublicIpAddresses: []*cloudstack.PublicIpAddress{
				{Id: "ip-new", Ipaddress: "10.0.0.2", Allocated: "2023-01-01"},
			},
		}, nil)

		setupCreateRuleAndFirewall(mockLB, mockNetwork, mockFirewall, "10.0.0.2", "ip-new")

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				// No annotation — spec.LoadBalancerIP used as fallback
			},
			Spec: corev1.ServiceSpec{
				LoadBalancerIP: "10.0.0.2",
				Ports: []corev1.ServicePort{
					{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
				},
				SessionAffinity: corev1.ServiceAffinityNone,
			},
		}
		cs := newTestCSCloud(mockLB, mockAddress, mockVM, mockNetwork, mockFirewall, service)
		nodes := []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		}

		status, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status == nil || len(status.Ingress) == 0 {
			t.Fatalf("expected non-empty status")
		}
		if status.Ingress[0].IP != "10.0.0.2" {
			t.Errorf("status IP = %q, want %q", status.Ingress[0].IP, "10.0.0.2")
		}
	})
}

func TestGetLoadBalancerAddress(t *testing.T) {
	t.Run("nil service", func(t *testing.T) {
		if got := getLoadBalancerAddress(nil); got != "" {
			t.Errorf("getLoadBalancerAddress(nil) = %q, want empty", got)
		}
	})

	t.Run("annotation takes precedence over spec", func(t *testing.T) {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerAddress: "10.0.0.1",
				},
			},
			Spec: corev1.ServiceSpec{
				LoadBalancerIP: "10.0.0.2",
			},
		}
		if got := getLoadBalancerAddress(service); got != "10.0.0.1" {
			t.Errorf("getLoadBalancerAddress() = %q, want %q", got, "10.0.0.1")
		}
	})

	t.Run("falls back to spec.LoadBalancerIP", func(t *testing.T) {
		service := &corev1.Service{
			Spec: corev1.ServiceSpec{
				LoadBalancerIP: "10.0.0.2",
			},
		}
		if got := getLoadBalancerAddress(service); got != "10.0.0.2" {
			t.Errorf("getLoadBalancerAddress() = %q, want %q", got, "10.0.0.2")
		}
	})

	t.Run("both empty returns empty", func(t *testing.T) {
		service := &corev1.Service{}
		if got := getLoadBalancerAddress(service); got != "" {
			t.Errorf("getLoadBalancerAddress() = %q, want empty", got)
		}
	})
}

func TestShouldReleaseLoadBalancerIPKeepIP(t *testing.T) {
	t.Run("keep-ip true prevents release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		cs := &CSCloud{}
		lb := &loadBalancer{
			ipAddr:   "10.0.0.1",
			ipAddrID: "ip-1",
		}
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerKeepIP: "true",
				},
			},
		}

		release, err := cs.shouldReleaseLoadBalancerIP(lb, service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if release {
			t.Error("expected shouldReleaseLoadBalancerIP to return false when keep-ip is true")
		}
	})

	t.Run("keep-ip false allows release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0,
		}, nil)

		cs := &CSCloud{}
		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			ipAddr:   "10.0.0.1",
			ipAddrID: "ip-1",
		}
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerKeepIP: "false",
				},
			},
		}

		release, err := cs.shouldReleaseLoadBalancerIP(lb, service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !release {
			t.Error("expected shouldReleaseLoadBalancerIP to return true when keep-ip is false")
		}
	})

	t.Run("keep-ip absent allows release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0,
		}, nil)

		cs := &CSCloud{}
		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			ipAddr:   "10.0.0.1",
			ipAddrID: "ip-1",
		}
		service := &corev1.Service{}

		release, err := cs.shouldReleaseLoadBalancerIP(lb, service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !release {
			t.Error("expected shouldReleaseLoadBalancerIP to return true when keep-ip is absent")
		}
	})

	t.Run("spec.LoadBalancerIP no longer prevents release", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			Count: 0,
		}, nil)

		cs := &CSCloud{}
		lb := &loadBalancer{
			CloudStackClient: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
			ipAddr:   "10.0.0.1",
			ipAddrID: "ip-1",
		}
		service := &corev1.Service{
			Spec: corev1.ServiceSpec{
				LoadBalancerIP: "10.0.0.1", // previously this would have prevented release
			},
		}

		release, err := cs.shouldReleaseLoadBalancerIP(lb, service)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !release {
			t.Error("expected shouldReleaseLoadBalancerIP to return true; spec.LoadBalancerIP should no longer prevent release")
		}
	})
}

func TestGetLoadBalancerID(t *testing.T) {
	t.Run("annotation present", func(t *testing.T) {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerID: "ip-uuid-123",
				},
			},
		}
		if got := getLoadBalancerID(service); got != "ip-uuid-123" {
			t.Errorf("getLoadBalancerID() = %q, want %q", got, "ip-uuid-123")
		}
	})

	t.Run("annotation absent", func(t *testing.T) {
		service := &corev1.Service{}
		if got := getLoadBalancerID(service); got != "" {
			t.Errorf("getLoadBalancerID() = %q, want empty string", got)
		}
	})
}

func TestGetLoadBalancerNetworkID(t *testing.T) {
	t.Run("annotation present", func(t *testing.T) {
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerNetworkID: "net-uuid-456",
				},
			},
		}
		if got := getLoadBalancerNetworkID(service); got != "net-uuid-456" {
			t.Errorf("getLoadBalancerNetworkID() = %q, want %q", got, "net-uuid-456")
		}
	})

	t.Run("annotation absent", func(t *testing.T) {
		service := &corev1.Service{}
		if got := getLoadBalancerNetworkID(service); got != "" {
			t.Errorf("getLoadBalancerNetworkID() = %q, want empty string", got)
		}
	})
}

func TestGetLoadBalancerByID(t *testing.T) {
	t.Run("rules found by IP ID", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		listResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 2,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "lb-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1", Networkid: "net-1"},
				{Name: "lb-tcp-443", Publicip: "1.2.3.4", Publicipid: "ip-1", Networkid: "net-1"},
			},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		lb, err := cs.getLoadBalancerByID("my-lb", "ip-1", "net-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 2 {
			t.Fatalf("expected 2 rules, got %d", len(lb.rules))
		}
		if lb.ipAddr != "1.2.3.4" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "1.2.3.4")
		}
		if lb.ipAddrID != "ip-1" {
			t.Errorf("ipAddrID = %q, want %q", lb.ipAddrID, "ip-1")
		}
		if lb.networkID != "net-1" {
			t.Errorf("networkID = %q, want %q", lb.networkID, "net-1")
		}
	})

	t.Run("no rules found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		listResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count:             0,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(listResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		lb, err := cs.getLoadBalancerByID("my-lb", "ip-1", "net-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 0 {
			t.Fatalf("expected 0 rules, got %d", len(lb.rules))
		}
	})

	t.Run("API error propagated", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(nil, errors.New("API failure"))

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		_, err := cs.getLoadBalancerByID("my-lb", "ip-1", "net-1")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "API failure") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "API failure")
		}
	})

	t.Run("with project ID", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		listResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count:             0,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).DoAndReturn(func(p *cloudstack.ListLoadBalancerRulesParams) (*cloudstack.ListLoadBalancerRulesResponse, error) {
			projectID, ok := p.GetProjectid()
			if !ok || projectID != "proj-1" {
				t.Errorf("expected projectid = %q, got %q (ok=%v)", "proj-1", projectID, ok)
			}

			return listResp, nil
		})

		cs := &CSCloud{
			projectID: "proj-1",
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		_, err := cs.getLoadBalancerByID("my-lb", "ip-1", "net-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty network ID omits SetNetworkid", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		listResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count:             0,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).DoAndReturn(func(p *cloudstack.ListLoadBalancerRulesParams) (*cloudstack.ListLoadBalancerRulesResponse, error) {
			_, ok := p.GetNetworkid()
			if ok {
				t.Error("expected networkid to not be set when empty string passed")
			}

			return listResp, nil
		})

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		_, err := cs.getLoadBalancerByID("my-lb", "ip-1", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGetLoadBalancerOrchestrator(t *testing.T) {
	t.Run("ID annotation present and rules found - returns ID-based result", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		// ID-based lookup returns rules
		idResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "lb-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1", Networkid: "net-1"},
			},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(idResp, nil)
		// Name-based lookup should NOT be called

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerID:        "ip-1",
					ServiceAnnotationLoadBalancerNetworkID: "net-1",
				},
			},
		}

		lb, err := cs.getLoadBalancer(service, "my-lb", "legacy-lb")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if lb.ipAddr != "1.2.3.4" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "1.2.3.4")
		}
	})

	t.Run("ID annotation present but no rules - falls back to name-based", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)

		// First call: ID-based lookup → no rules
		idParams := &cloudstack.ListLoadBalancerRulesParams{}
		idResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count:             0,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{},
		}

		// Second call: name-based lookup → rules found
		nameParams := &cloudstack.ListLoadBalancerRulesParams{}
		nameResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "my-lb-tcp-80", Publicip: "5.6.7.8", Publicipid: "ip-2"},
			},
		}

		gomock.InOrder(
			mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(idParams),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(idResp, nil),
			mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(nameParams),
			mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(nameResp, nil),
		)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerID:        "ip-stale",
					ServiceAnnotationLoadBalancerNetworkID: "net-stale",
				},
			},
		}

		lb, err := cs.getLoadBalancer(service, "my-lb", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if lb.ipAddr != "5.6.7.8" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "5.6.7.8")
		}
	})

	t.Run("no ID annotation - goes directly to name-based", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		nameResp := &cloudstack.ListLoadBalancerRulesResponse{
			Count: 1,
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{
				{Name: "my-lb-tcp-80", Publicip: "1.2.3.4", Publicipid: "ip-1"},
			},
		}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(nameResp, nil)

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		service := &corev1.Service{} // no annotations

		lb, err := cs.getLoadBalancer(service, "my-lb", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lb.rules) != 1 {
			t.Fatalf("expected 1 rule, got %d", len(lb.rules))
		}
		if lb.ipAddr != "1.2.3.4" {
			t.Errorf("ipAddr = %q, want %q", lb.ipAddr, "1.2.3.4")
		}
	})

	t.Run("ID-based API error propagated without fallback", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		listParams := &cloudstack.ListLoadBalancerRulesParams{}

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(listParams)
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(nil, errors.New("infra error"))

		cs := &CSCloud{
			client: &cloudstack.CloudStackClient{
				LoadBalancer: mockLB,
			},
		}

		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					ServiceAnnotationLoadBalancerID: "ip-1",
				},
			},
		}

		_, err := cs.getLoadBalancer(service, "my-lb", "")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "infra error") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "infra error")
		}
	})
}
