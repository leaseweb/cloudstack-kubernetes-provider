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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tests for the reduction of CloudStack API calls: per-reconcile reuse of lookups in EnsureLoadBalancer,
// and the VM cache used by verifyHosts.

const testRulePrefix = "K8s_svc_cluster_default_foo-"

func testLBRule(name, privatePort, publicPort string) *cloudstack.LoadBalancerRule {
	return &cloudstack.LoadBalancerRule{
		Id: "id-" + name, Name: testRulePrefix + name, Algorithm: "roundrobin", Protocol: "tcp",
		Networkid: "net-1", Privateport: privatePort, Publicport: publicPort,
		Publicip: "10.0.0.1", Publicipid: "ip-1",
	}
}

func testService(ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports:           ports,
			SessionAffinity: corev1.ServiceAffinityNone,
		},
	}
}

// firewallCall is a firewall rule that EnsureLoadBalancer created.
type firewallCall struct {
	protocol string
	port     int
	cidrs    string
}

// expectFirewallCreates records the firewall rules that are created.
func expectFirewallCreates(mockFirewall *cloudstack.MockFirewallServiceIface, n int) *[]firewallCall {
	var created []firewallCall
	mockFirewall.EXPECT().NewCreateFirewallRuleParams("ip-1", gomock.Any()).DoAndReturn(func(ipID, proto string) *cloudstack.CreateFirewallRuleParams {
		p := &cloudstack.CreateFirewallRuleParams{}
		p.SetIpaddressid(ipID)
		p.SetProtocol(proto)

		return p
	}).Times(n)
	mockFirewall.EXPECT().CreateFirewallRule(gomock.Any()).DoAndReturn(func(p *cloudstack.CreateFirewallRuleParams) (*cloudstack.CreateFirewallRuleResponse, error) {
		proto, _ := p.GetProtocol()
		port, _ := p.GetStartport()
		cidrs, _ := p.GetCidrlist()
		created = append(created, firewallCall{protocol: proto, port: port, cidrs: fmt.Sprint(cidrs)})

		return &cloudstack.CreateFirewallRuleResponse{Id: "fw-new"}, nil
	}).Times(n)

	return &created
}

func expectCreateLBRules(mockLB *cloudstack.MockLoadBalancerServiceIface, n int) {
	mockLB.EXPECT().NewCreateLoadBalancerRuleParams(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleParams{}).Times(n)
	mockLB.EXPECT().CreateLoadBalancerRule(gomock.Any()).Return(&cloudstack.CreateLoadBalancerRuleResponse{
		Id: "rule-new", Algorithm: "roundrobin", Networkid: "net-1", Publicip: "10.0.0.1", Publicipid: "ip-1",
	}, nil).Times(n)
	mockLB.EXPECT().NewAssignToLoadBalancerRuleParams(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleParams{}).Times(n)
	mockLB.EXPECT().AssignToLoadBalancerRule(gomock.Any()).Return(&cloudstack.AssignToLoadBalancerRuleResponse{}, nil).Times(n)
}

func expectRuleInstances(mockLB *cloudstack.MockLoadBalancerServiceIface, n int) {
	mockLB.EXPECT().NewListLoadBalancerRuleInstancesParams(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesParams{}).Times(n)
	mockLB.EXPECT().ListLoadBalancerRuleInstances(gomock.Any()).Return(&cloudstack.ListLoadBalancerRuleInstancesResponse{
		LoadBalancerRuleInstances: []*cloudstack.VirtualMachine{{Id: "vm-1"}},
	}, nil).Times(n)
}

func expectNetworkWithFirewall(mockNetwork *cloudstack.MockNetworkServiceIface) {
	mockNetwork.EXPECT().GetNetworkByID("net-1", gomock.Any()).Return(&cloudstack.Network{
		Id: "net-1", Service: []cloudstack.NetworkServiceInternal{{Name: "Firewall"}},
	}, 1, nil).Times(1)
}

func TestEnsureLoadBalancerReusesLookupsAcrossPorts(t *testing.T) {
	t.Run("one network lookup and one firewall list for all ports, same changes", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		// Existing rules for tcp/80 and tcp/443; udp/80 is new.
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{testLBRule("tcp-80", "30080", "80"), testLBRule("tcp-443", "30443", "443")},
		}, nil)
		setupVerifyHosts(mockVM)
		expectRuleInstances(mockLB, 2)
		expectCreateLBRules(mockLB, 1)
		expectNetworkWithFirewall(mockNetwork)

		mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{}).Times(1)
		mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{
			FirewallRules: []*cloudstack.FirewallRule{
				{Id: "fw-tcp-80", Protocol: "tcp", Startport: 80, Endport: 80, Cidrlist: "0.0.0.0/0"},
				{Id: "fw-tcp-443", Protocol: "tcp", Startport: 443, Endport: 443, Cidrlist: "10.0.0.0/8"},
				{Id: "fw-udp-53", Protocol: "udp", Startport: 53, Endport: 53, Cidrlist: "0.0.0.0/0"},
			},
		}, nil).Times(1)

		// Only the tcp/443 rule has a different CIDR list, so only it is replaced.
		mockFirewall.EXPECT().NewDeleteFirewallRuleParams("fw-tcp-443").Return(&cloudstack.DeleteFirewallRuleParams{}).Times(1)
		mockFirewall.EXPECT().DeleteFirewallRule(gomock.Any()).Return(&cloudstack.DeleteFirewallRuleResponse{}, nil).Times(1)
		created := expectFirewallCreates(mockFirewall, 2)

		service := testService(
			corev1.ServicePort{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Port: 80, NodePort: 30081, Protocol: corev1.ProtocolUDP},
			corev1.ServicePort{Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP},
		)
		cs := newTestCSCloud(mockLB, nil, mockVM, mockNetwork, mockFirewall, service)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := []firewallCall{{"udp", 80, "[0.0.0.0/0]"}, {"tcp", 443, "[0.0.0.0/0]"}}
		if !slices.Equal(*created, want) {
			t.Errorf("created firewall rules = %v, want %v", *created, want)
		}
	})

	t.Run("firewall rules are listed again after a load balancer rule was deleted", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)
		mockFirewall := cloudstack.NewMockFirewallServiceIface(ctrl)

		// The tcp/443 rule has an old node port, so checkLoadBalancerRule deletes and recreates it.
		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{testLBRule("tcp-443", "39999", "443")},
		}, nil)
		setupVerifyHosts(mockVM)
		mockLB.EXPECT().NewDeleteLoadBalancerRuleParams("id-tcp-443").Return(&cloudstack.DeleteLoadBalancerRuleParams{})
		mockLB.EXPECT().DeleteLoadBalancerRule(gomock.Any()).Return(&cloudstack.DeleteLoadBalancerRuleResponse{}, nil)
		expectCreateLBRules(mockLB, 2)
		expectNetworkWithFirewall(mockNetwork)

		// Listed for tcp/80, and again for tcp/443 after the load balancer rule was deleted.
		mockFirewall.EXPECT().NewListFirewallRulesParams().Return(&cloudstack.ListFirewallRulesParams{}).Times(2)
		mockFirewall.EXPECT().ListFirewallRules(gomock.Any()).Return(&cloudstack.ListFirewallRulesResponse{}, nil).Times(2)
		created := expectFirewallCreates(mockFirewall, 2)

		service := testService(
			corev1.ServicePort{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP},
		)
		cs := newTestCSCloud(mockLB, nil, mockVM, mockNetwork, mockFirewall, service)

		if _, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := []firewallCall{{"tcp", 80, "[0.0.0.0/0]"}, {"tcp", 443, "[0.0.0.0/0]"}}
		if !slices.Equal(*created, want) {
			t.Errorf("created firewall rules = %v, want %v", *created, want)
		}
	})

	t.Run("network lookup error on the first port is returned as before", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)

		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockNetwork := cloudstack.NewMockNetworkServiceIface(ctrl)

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(&cloudstack.ListLoadBalancerRulesResponse{
			LoadBalancerRules: []*cloudstack.LoadBalancerRule{testLBRule("tcp-80", "30080", "80")},
		}, nil)
		setupVerifyHosts(mockVM)
		expectRuleInstances(mockLB, 1)
		mockNetwork.EXPECT().GetNetworkByID("net-1", gomock.Any()).Return(nil, 0, errors.New("boom"))

		service := testService(
			corev1.ServicePort{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP},
		)
		cs := newTestCSCloud(mockLB, nil, mockVM, mockNetwork, nil, service)

		_, err := cs.EnsureLoadBalancer(t.Context(), "cluster", service, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}})
		if err == nil || err.Error() != "could not find network with ID net-1: boom" {
			t.Fatalf("err = %v, want %q", err, "could not find network with ID net-1: boom")
		}
	})
}

// fakeClock is a settable clock for the VM cache.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// newCachedTestCSCloud returns a CSCloud with a VM cache that uses the fake clock.
func newCachedTestCSCloud(mockVM *cloudstack.MockVirtualMachineServiceIface, clock *fakeClock) *CSCloud {
	cache := newVMCache(30 * time.Second)
	cache.now = clock.Now

	return &CSCloud{
		client:  &cloudstack.CloudStackClient{VirtualMachine: mockVM},
		vmCache: cache,
	}
}

// expectVMLists sets up n listVirtualMachines calls. Each call returns the next list of VMs in lists,
// and the last list for the calls after that.
func expectVMLists(mockVM *cloudstack.MockVirtualMachineServiceIface, n int, lists ...[]*cloudstack.VirtualMachine) {
	call := 0
	mockVM.EXPECT().NewListVirtualMachinesParams().Return(&cloudstack.ListVirtualMachinesParams{}).Times(n)
	mockVM.EXPECT().ListVirtualMachines(gomock.Any()).DoAndReturn(func(*cloudstack.ListVirtualMachinesParams) (*cloudstack.ListVirtualMachinesResponse, error) {
		vms := lists[min(call, len(lists)-1)]
		call++

		return &cloudstack.ListVirtualMachinesResponse{Count: len(vms), VirtualMachines: vms}, nil
	}).Times(n)
}

func testVM(id, name string) *cloudstack.VirtualMachine {
	return &cloudstack.VirtualMachine{Id: id, Name: name, Nic: []cloudstack.Nic{{Networkid: "net-1"}}}
}

func testNode(name, providerID string, created time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestVerifyHostsVMCache(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := start.Add(-time.Hour)

	verify := func(t *testing.T, cs *CSCloud, nodes ...*corev1.Node) []string {
		t.Helper()
		ids, networkID, err := cs.verifyHosts(nodes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if networkID != "net-1" {
			t.Errorf("networkID = %q, want net-1", networkID)
		}
		slices.Sort(ids)

		return ids
	}

	t.Run("second call within the TTL uses the cache", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 1, []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		clock.Advance(29 * time.Second)
		if ids := verify(t, cs, testNode("node-1", "", old)); !slices.Equal(ids, []string{"vm-1"}) {
			t.Errorf("ids = %v, want [vm-1]", ids)
		}
	})

	t.Run("call after the TTL fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2, []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		clock.Advance(30 * time.Second)
		verify(t, cs, testNode("node-1", "", old))
	})

	t.Run("unmatched node fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2,
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1")},
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1"), testVM("vm-2", "node-2")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		clock.Advance(time.Second)
		if ids := verify(t, cs, testNode("node-1", "", old), testNode("node-2", "", old)); !slices.Equal(ids, []string{"vm-1", "vm-2"}) {
			t.Errorf("ids = %v, want [vm-1 vm-2]", ids)
		}
	})

	t.Run("ProviderID that is not in the cache fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		// The VM was recreated with the same name and a new ID.
		expectVMLists(mockVM, 2,
			[]*cloudstack.VirtualMachine{testVM("vm-old", "node-1")},
			[]*cloudstack.VirtualMachine{testVM("vm-new", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		clock.Advance(time.Second)
		if ids := verify(t, cs, testNode("node-1", "cloudstack:///vm-new", old)); !slices.Equal(ids, []string{"vm-new"}) {
			t.Errorf("ids = %v, want [vm-new]", ids)
		}
	})

	t.Run("matched VM without NIC fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2,
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1"), {Id: "vm-2", Name: "node-2"}},
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1"), testVM("vm-2", "node-2")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old), testNode("node-2", "", old))
		clock.Advance(time.Second)
		if ids := verify(t, cs, testNode("node-1", "", old), testNode("node-2", "", old)); !slices.Equal(ids, []string{"vm-1", "vm-2"}) {
			t.Errorf("ids = %v, want [vm-1 vm-2]", ids)
		}
	})

	t.Run("node newer than the cache fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2,
			[]*cloudstack.VirtualMachine{testVM("vm-old", "node-1")},
			[]*cloudstack.VirtualMachine{testVM("vm-new", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		clock.Advance(5 * time.Second)
		if ids := verify(t, cs, testNode("node-1", "", start.Add(2*time.Second))); !slices.Equal(ids, []string{"vm-new"}) {
			t.Errorf("ids = %v, want [vm-new]", ids)
		}
	})

	t.Run("still unmatched after a new list gives the same error as without cache", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2, []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		_, _, err := cs.verifyHosts([]*corev1.Node{testNode("node-9", "", old)})
		want := "could not match any of the 1 node(s) to VMs in CloudStack (unmatched: [node-9], skipped-no-nic: [])"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	})

	t.Run("hosts in different networks in the cache fetches a new list", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		other := testVM("vm-2", "node-2")
		other.Nic[0].Networkid = "net-2"
		expectVMLists(mockVM, 2,
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1"), other},
			[]*cloudstack.VirtualMachine{testVM("vm-1", "node-1"), testVM("vm-2", "node-2")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)

		verify(t, cs, testNode("node-1", "", old))
		if ids := verify(t, cs, testNode("node-1", "", old), testNode("node-2", "", old)); !slices.Equal(ids, []string{"vm-1", "vm-2"}) {
			t.Errorf("ids = %v, want [vm-1 vm-2]", ids)
		}
	})

	t.Run("failed UpdateLoadBalancer drops the cache", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		mockLB := cloudstack.NewMockLoadBalancerServiceIface(ctrl)
		expectVMLists(mockVM, 2, []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")})
		clock := &fakeClock{now: start}
		cs := newCachedTestCSCloud(mockVM, clock)
		cs.client.LoadBalancer = mockLB

		verify(t, cs, testNode("node-1", "", old))

		mockLB.EXPECT().NewListLoadBalancerRulesParams().Return(&cloudstack.ListLoadBalancerRulesParams{})
		mockLB.EXPECT().ListLoadBalancerRules(gomock.Any()).Return(nil, errors.New("boom"))
		if err := cs.UpdateLoadBalancer(t.Context(), "cluster", testService(), []*corev1.Node{testNode("node-1", "", old)}); err == nil {
			t.Fatal("expected an error")
		}

		verify(t, cs, testNode("node-1", "", old))
	})

	t.Run("without cache every call lists the VMs", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		mockVM := cloudstack.NewMockVirtualMachineServiceIface(ctrl)
		expectVMLists(mockVM, 2, []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")})
		cs := &CSCloud{client: &cloudstack.CloudStackClient{VirtualMachine: mockVM}, vmCache: newVMCache(0)}

		verify(t, cs, testNode("node-1", "", old))
		verify(t, cs, testNode("node-1", "", old))
	})
}

func TestVMCache(t *testing.T) {
	t.Run("concurrent callers share one fetch", func(t *testing.T) {
		cache := newVMCache(time.Minute)
		var fetches atomic.Int32
		release := make(chan struct{})
		fetch := func() ([]*cloudstack.VirtualMachine, error) { //nolint:unparam // Must match the fetch signature.
			fetches.Add(1)
			<-release

			return []*cloudstack.VirtualMachine{testVM("vm-1", "node-1")}, nil
		}

		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				if list, err := cache.get(fetch, nil); err != nil || len(list.vms) != 1 {
					t.Errorf("get() = %v, %v", list.vms, err)
				}
			})
		}
		time.Sleep(50 * time.Millisecond)
		close(release)
		wg.Wait()

		if n := fetches.Load(); n != 1 {
			t.Errorf("fetches = %d, want 1", n)
		}
	})

	t.Run("refresh of the current list fetches, refresh of an older list does not", func(t *testing.T) {
		cache := newVMCache(time.Minute)
		var fetches int
		fetch := func() ([]*cloudstack.VirtualMachine, error) {
			fetches++

			return nil, nil
		}

		first, _ := cache.get(fetch, nil)
		second, _ := cache.get(fetch, &first)
		if fetches != 2 || !second.fresh {
			t.Fatalf("fetches = %d, fresh = %v, want 2, true", fetches, second.fresh)
		}
		// Another caller still has the first list: the second list is newer, so it is used.
		if third, _ := cache.get(fetch, &first); fetches != 2 || third.fresh || third.generation != second.generation {
			t.Errorf("fetches = %d, fresh = %v, generation = %d, want 2, false, %d", fetches, third.fresh, third.generation, second.generation)
		}
	})

	t.Run("invalidate drops the list", func(t *testing.T) {
		cache := newVMCache(time.Minute)
		var fetches int
		fetch := func() ([]*cloudstack.VirtualMachine, error) {
			fetches++

			return nil, nil
		}

		_, _ = cache.get(fetch, nil)
		cache.invalidate()
		if list, _ := cache.get(fetch, nil); fetches != 2 || !list.fresh {
			t.Errorf("fetches = %d, fresh = %v, want 2, true", fetches, list.fresh)
		}
	})

	t.Run("fetch error is returned and not cached", func(t *testing.T) {
		cache := newVMCache(time.Minute)
		var fetches int
		fail := func() ([]*cloudstack.VirtualMachine, error) {
			fetches++

			return nil, errors.New("boom")
		}

		for range 2 {
			if _, err := cache.get(fail, nil); err == nil {
				t.Fatal("expected an error")
			}
		}
		if fetches != 2 {
			t.Errorf("fetches = %d, want 2", fetches)
		}
	})

	t.Run("nil cache always fetches", func(t *testing.T) {
		var cache *vmCache
		var fetches int
		fetch := func() ([]*cloudstack.VirtualMachine, error) {
			fetches++

			return nil, nil
		}

		for range 2 {
			if list, err := cache.get(fetch, nil); err != nil || !list.fresh {
				t.Fatalf("get() fresh = %v, err = %v", list.fresh, err)
			}
		}
		cache.invalidate()
		if fetches != 2 {
			t.Errorf("fetches = %d, want 2", fetches)
		}
	})
}

func TestNeedsFreshListTimestampPrecision(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 1, 12, 0, 0, 300*int(time.Millisecond), time.UTC)
	tests := []struct {
		created time.Time
		want    bool
	}{
		// The node was created at 12:00:00.800, after the fetch. The API server stores 12:00:00.
		{time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 1, 1, 12, 0, 1, 0, time.UTC), true},
		{time.Date(2026, 1, 1, 11, 59, 59, 0, time.UTC), false},
	}
	for _, tt := range tests {
		nodes := []*corev1.Node{testNode("node-1", "", tt.created)}
		if got := (hostMatch{}).needsFreshList(nodes, fetchedAt); got != tt.want {
			t.Errorf("needsFreshList(created %v) = %v, want %v", tt.created, got, tt.want)
		}
	}
}
