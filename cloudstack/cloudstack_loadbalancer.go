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
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	corev1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultAllowedCIDR is the network range that is allowed on the firewall
	// by default when no explicit CIDR list is given on a LoadBalancer.
	defaultAllowedCIDR = "0.0.0.0/0"

	// ServiceAnnotationLoadBalancerProxyProtocol is the annotation used on the
	// service to enable the proxy protocol on a CloudStack load balancer.
	// Note that this protocol only applies to TCP service ports and
	// CloudStack >= 4.6 is required for it to work.
	ServiceAnnotationLoadBalancerProxyProtocol = "service.beta.kubernetes.io/cloudstack-load-balancer-proxy-protocol"

	// ServiceAnnotationLoadBalancerLoadbalancerHostname can be used in conjunction
	// with PROXY protocol to allow the service to be accessible from inside the
	// cluster. This is a workaround for https://github.com/kubernetes/kubernetes/issues/66607
	ServiceAnnotationLoadBalancerLoadbalancerHostname = "service.beta.kubernetes.io/cloudstack-load-balancer-hostname"

	// ServiceAnnotationLoadBalancerAddress is a read-only annotation indicating the IP address assigned to the load balancer.
	ServiceAnnotationLoadBalancerAddress = "service.beta.kubernetes.io/cloudstack-load-balancer-address"

	// Used to construct the load balancer name.
	servicePrefix = "K8s_svc_"
	lbNameFormat  = "%s%s_%s_%s"
)

type loadBalancer struct {
	*cloudstack.CloudStackClient

	name      string
	algorithm string
	hostIDs   []string
	ipAddr    string
	ipAddrID  string
	networkID string
	projectID string
	rules     map[string]*cloudstack.LoadBalancerRule
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service) (*corev1.LoadBalancerStatus, bool, error) {
	klog.V(4).InfoS("GetLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancerByName(name, legacyName)
	if err != nil {
		return nil, false, err
	}

	// If we don't have any rules, the load balancer does not exist.
	if len(lb.rules) == 0 {
		return nil, false, nil
	}

	klog.V(4).Infof("Found a load balancer associated with IP %v", lb.ipAddr)

	status := &corev1.LoadBalancerStatus{}
	status.Ingress = append(status.Ingress, corev1.LoadBalancerIngress{IP: lb.ipAddr})

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) (status *corev1.LoadBalancerStatus, err error) { //nolint:gocognit,gocyclo,nestif
	klog.V(4).InfoS("EnsureLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))
	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	if len(service.Spec.Ports) == 0 {
		return nil, errors.New("requested load balancer with no ports")
	}

	// Patch the service with new/updated annotations if needed after EnsureLoadBalancer finishes.
	patcher := newServicePatcher(cs.kclient, service)
	defer func() { err = patcher.Patch(ctx, err) }()

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancerByName(name, legacyName)
	if err != nil {
		return nil, err
	}

	// Set the load balancer algorithm.
	switch service.Spec.SessionAffinity {
	case corev1.ServiceAffinityNone:
		lb.algorithm = "roundrobin"
	case corev1.ServiceAffinityClientIP:
		lb.algorithm = "source"
	default:
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", service.Spec.SessionAffinity)
	}

	// Verify that all the hosts belong to the same network, and retrieve their ID's.
	lb.hostIDs, lb.networkID, err = cs.verifyHosts(nodes)
	if err != nil {
		return nil, err
	}

	if !lb.hasLoadBalancerIP() { //nolint:nestif
		// Create or retrieve the load balancer IP.
		if err := lb.getLoadBalancerIP(service.Spec.LoadBalancerIP); err != nil {
			return nil, err
		}

		msg := fmt.Sprintf("Created new load balancer for service %s with algorithm '%s' and IP address %s", serviceName, lb.algorithm, lb.ipAddr)
		cs.eventRecorder.Event(service, corev1.EventTypeNormal, "CreatedLoadBalancer", msg)
		klog.Info(msg)

		if lb.ipAddr != "" && lb.ipAddr != service.Spec.LoadBalancerIP {
			defer func(lb *loadBalancer) {
				if err != nil {
					if err := lb.releaseLoadBalancerIP(); err != nil {
						klog.Errorf("Attempt to release load balancer IP failed: %s", err.Error())
					}
				}
			}(lb)
		}
	}

	klog.V(4).Infof("Load balancer %v is associated with IP %v", lb.name, lb.ipAddr)

	// Set the load balancer IP address annotation on the Service
	setServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress, lb.ipAddr)

	for _, port := range service.Spec.Ports {
		// Construct the protocol name first, we need it a few times
		protocol := ProtocolFromServicePort(port, service)
		if protocol == LoadBalancerProtocolInvalid {
			return nil, fmt.Errorf("unsupported load balancer protocol: %v", port.Protocol)
		}

		// All ports have their own load balancer rule, so add the port to lbName to keep the names unique.
		lbRuleName := fmt.Sprintf("%s-%s-%d", lb.name, protocol, port.Port)

		// If the load balancer rule exists and is up-to-date, we move on to the next rule.
		lbRule, needsUpdate, err := lb.checkLoadBalancerRule(lbRuleName, port, protocol)
		if err != nil {
			return nil, err
		}

		if lbRule != nil { //nolint:nestif
			if needsUpdate {
				klog.V(4).Infof("Updating load balancer rule: %v", lbRuleName)
				if err := lb.updateLoadBalancerRule(lbRuleName, protocol); err != nil {
					return nil, err
				}
				// Delete the rule from the map, to prevent it being deleted.
				delete(lb.rules, lbRuleName)
			} else {
				klog.V(4).Infof("Load balancer rule %v is up-to-date", lbRuleName)
				// Delete the rule from the map, to prevent it being deleted.
				delete(lb.rules, lbRuleName)
			}
		} else {
			klog.V(4).Infof("Creating load balancer rule: %v", lbRuleName)
			lbRule, err = lb.createLoadBalancerRule(lbRuleName, port, protocol)
			if err != nil {
				return nil, err
			}

			klog.V(4).Infof("Assigning hosts (%v) to load balancer rule: %v", lb.hostIDs, lbRuleName)
			if err = lb.assignHostsToRule(lbRule, lb.hostIDs); err != nil {
				return nil, err
			}
		}

		network, count, err := lb.Network.GetNetworkByID(lb.networkID, cloudstack.WithProject(lb.projectID))
		if err != nil {
			if count == 0 {
				return nil, err
			}

			return nil, err
		}

		lbSourceRanges, err := getLoadBalancerSourceRanges(service)
		if err != nil {
			return nil, err
		}

		if lbRule != nil && isFirewallSupported(network.Service) {
			klog.V(4).Infof("Creating firewall rules for load balancer rule: %v (%v:%v:%v)", lbRuleName, protocol, lbRule.Publicip, port.Port)
			if _, err := lb.updateFirewallRule(lbRule.Publicipid, int(port.Port), protocol, lbSourceRanges.StringSlice()); err != nil {
				return nil, err
			}
		} else {
			msg := fmt.Sprintf("LoadBalancerSourceRanges are ignored for Service %s because this CloudStack network does not support it", serviceName)
			cs.eventRecorder.Event(service, corev1.EventTypeWarning, "LoadBalancerSourceRangesIgnored", msg)
			klog.Warning(msg)
		}
	}

	// Cleanup any rules that are now still in the rules map, as they are no longer needed.
	for _, lbRule := range lb.rules {
		protocol := ProtocolFromLoadBalancer(lbRule.Protocol)
		if protocol == LoadBalancerProtocolInvalid {
			return nil, fmt.Errorf("error parsing protocol %v: %w", lbRule.Protocol, err)
		}
		port, err := strconv.ParseInt(lbRule.Publicport, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("error parsing port %s: %w", lbRule.Publicport, err)
		}

		klog.V(4).Infof("Deleting firewall rules associated with load balancer rule: %v (%v:%v:%v)", lbRule.Name, protocol, lbRule.Publicip, port)
		if _, err := lb.deleteFirewallRule(lbRule.Publicipid, int(port), protocol); err != nil {
			return nil, err
		}

		klog.V(4).Infof("Deleting obsolete load balancer rule: %v", lbRule.Name)
		if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
			return nil, err
		}
	}

	status = &corev1.LoadBalancerStatus{}
	// If hostname is explicitly set using service annotation
	// Workaround for https://github.com/kubernetes/kubernetes/issues/66607
	if hostname := getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerLoadbalancerHostname, ""); hostname != "" {
		status.Ingress = []corev1.LoadBalancerIngress{{Hostname: hostname}}

		return status, nil
	}
	// Default to IP
	status.Ingress = []corev1.LoadBalancerIngress{{IP: lb.ipAddr}}

	return status, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) error {
	klog.V(4).InfoS("UpdateLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancerByName(name, legacyName)
	if err != nil {
		return err
	}

	// Verify that all the hosts belong to the same network, and retrieve their ID's.
	lb.hostIDs, _, err = cs.verifyHosts(nodes)
	if err != nil {
		return err
	}

	for _, lbRule := range lb.rules {
		p := lb.LoadBalancer.NewListLoadBalancerRuleInstancesParams(lbRule.Id)

		// Retrieve all VMs currently associated to this load balancer rule.
		l, err := lb.LoadBalancer.ListLoadBalancerRuleInstances(p)
		if err != nil {
			return fmt.Errorf("error retrieving associated instances: %w", err)
		}

		assign, remove := symmetricDifference(lb.hostIDs, l.LoadBalancerRuleInstances)

		if len(assign) > 0 {
			klog.V(4).Infof("Assigning new hosts (%v) to load balancer rule: %v", assign, lbRule.Name)
			if err := lb.assignHostsToRule(lbRule, assign); err != nil {
				return err
			}
		}

		if len(remove) > 0 {
			klog.V(4).Infof("Removing old hosts (%v) from load balancer rule: %v", remove, lbRule.Name)
			if err := lb.removeHostsFromRule(lbRule, remove); err != nil {
				return err
			}
		}
	}

	return nil
}

// isFirewallSupported checks whether a CloudStack network supports the Firewall service.
func isFirewallSupported(services []cloudstack.NetworkServiceInternal) bool {
	for _, svc := range services {
		if svc.Name == "Firewall" {
			return true
		}
	}

	return false
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it exists, returning
// nil if the load balancer specified either didn't exist or was successfully deleted.
func (cs *CSCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *corev1.Service) error {
	klog.V(4).InfoS("EnsureLoadBalancerDeleted", "cluster", clusterName, "service", klog.KObj(service))

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancerByName(name, legacyName)
	if err != nil {
		return err
	}

	for _, lbRule := range lb.rules {
		klog.V(4).Infof("Deleting firewall rules for load balancer: %v", lbRule.Name)
		protocol := ProtocolFromLoadBalancer(lbRule.Protocol)
		if protocol == LoadBalancerProtocolInvalid { //nolint:nestif
			klog.Errorf("Error parsing protocol: %v", lbRule.Protocol)
		} else {
			port, err := strconv.ParseInt(lbRule.Publicport, 10, 32)
			if err != nil {
				klog.Errorf("Error parsing port: %v", err)
			} else {
				if _, err := lb.deleteFirewallRule(lbRule.Publicipid, int(port), protocol); err != nil {
					return err
				}
			}

			klog.V(4).Infof("Deleting load balancer rule: %v", lbRule.Name)
			if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
				return err
			}
		}
	}

	if lb.ipAddr != "" {
		klog.V(4).Infof("Releasing load balancer IP: %v", lb.ipAddr)
		if err := lb.releaseLoadBalancerIP(); err != nil {
			return err
		}
	}

	return nil
}

// GetLoadBalancerName returns the name of the LoadBalancer.
func (cs *CSCloud) GetLoadBalancerName(_ context.Context, clusterName string, service *corev1.Service) string {
	return Sprintf255(lbNameFormat, servicePrefix, clusterName, service.Namespace, service.Name)
}

// getLoadBalancerLegacyName returns the legacy load balancer name for backward compatibility.
func (cs *CSCloud) getLoadBalancerLegacyName(_ context.Context, _ string, service *corev1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

// getLoadBalancerByName retrieves the IP address and ID and all the existing rules it can find.
func (cs *CSCloud) getLoadBalancerByName(name, legacyName string) (*loadBalancer, error) {
	lb := &loadBalancer{
		CloudStackClient: cs.client,
		name:             name,
		projectID:        cs.projectID,
		rules:            make(map[string]*cloudstack.LoadBalancerRule),
	}

	p := cs.client.LoadBalancer.NewListLoadBalancerRulesParams()
	p.SetKeyword(lb.name)
	p.SetListall(true)

	if cs.projectID != "" {
		p.SetProjectid(cs.projectID)
	}

	l, err := cs.client.LoadBalancer.ListLoadBalancerRules(p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving load balancer rules: %w", err)
	}

	// If no rules were found, check the legacy name.
	if len(l.LoadBalancerRules) == 0 { //nolint:nestif
		if len(legacyName) > 0 {
			p.SetKeyword(legacyName)
			l, err = cs.client.LoadBalancer.ListLoadBalancerRules(p)
			if err != nil {
				return nil, fmt.Errorf("error retrieving load balancer rules: %w", err)
			}
			if len(l.LoadBalancerRules) > 0 {
				lb.name = legacyName
			}
		} else {
			return lb, nil
		}
	}

	for _, lbRule := range l.LoadBalancerRules {
		lb.rules[lbRule.Name] = lbRule

		if lb.ipAddr != "" && lb.ipAddr != lbRule.Publicip {
			klog.Warningf("Load balancer %v has rules associated with different IP's: %v, %v", lb.name, lb.ipAddr, lbRule.Publicip)
		}

		lb.ipAddr = lbRule.Publicip
		lb.ipAddrID = lbRule.Publicipid
	}

	klog.V(4).Infof("Load balancer %v contains %d rule(s)", lb.name, len(lb.rules))

	return lb, nil
}

// verifyHosts verifies if all hosts belong to the same network, and returns the host ID's and network ID.
func (cs *CSCloud) verifyHosts(nodes []*corev1.Node) ([]string, string, error) {
	hostNames := map[string]bool{}
	for _, node := range nodes {
		// node.Name can be an FQDN as well, and CloudStack VM names aren't
		// To match, we need to Split the domain part off here, if present
		hostNames[strings.Split(strings.ToLower(node.Name), ".")[0]] = true
	}

	p := cs.client.VirtualMachine.NewListVirtualMachinesParams()
	p.SetListall(true)
	p.SetDetails([]string{"min", "nics"})

	if cs.projectID != "" {
		p.SetProjectid(cs.projectID)
	}

	l, err := cs.client.VirtualMachine.ListVirtualMachines(p)
	if err != nil {
		return nil, "", fmt.Errorf("error retrieving list of hosts: %w", err)
	}

	var hostIDs []string
	var networkID string

	// Check if the virtual machine is in the hosts slice, then add the corresponding ID.
	for _, vm := range l.VirtualMachines {
		if hostNames[strings.ToLower(vm.Name)] {
			if len(vm.Nic) == 0 {
				// Skip VM's without any active network interfaces. This happens during rollout f.e.
				continue
			}
			if networkID != "" && networkID != vm.Nic[0].Networkid {
				return nil, "", errors.New("found hosts that belong to different networks")
			}

			networkID = vm.Nic[0].Networkid
			hostIDs = append(hostIDs, vm.Id)
		}
	}

	if len(hostIDs) == 0 || len(networkID) == 0 {
		return nil, "", errors.New("none of the hosts matched the list of VMs retrieved from CS API")
	}

	return hostIDs, networkID, nil
}

// hasLoadBalancerIP returns true if we have a load balancer address and ID.
func (lb *loadBalancer) hasLoadBalancerIP() bool {
	return lb.ipAddr != "" && lb.ipAddrID != ""
}

// getLoadBalancerIP retrieves an existing IP or associates a new IP.
func (lb *loadBalancer) getLoadBalancerIP(loadBalancerIP string) error {
	if loadBalancerIP != "" {
		return lb.getPublicIPAddress(loadBalancerIP)
	}

	return lb.associatePublicIPAddress()
}

// getPublicIPAddressID retrieves the ID of the given IP, and sets the address and its ID.
func (lb *loadBalancer) getPublicIPAddress(loadBalancerIP string) error {
	klog.V(4).Infof("Retrieve load balancer IP details: %v", loadBalancerIP)

	p := lb.Address.NewListPublicIpAddressesParams()
	p.SetIpaddress(loadBalancerIP)
	p.SetListall(true)

	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	l, err := lb.Address.ListPublicIpAddresses(p)
	if err != nil {
		return fmt.Errorf("error retrieving IP address: %w", err)
	}

	if l.Count != 1 {
		return fmt.Errorf("could not find IP address %v", loadBalancerIP)
	}

	lb.ipAddr = l.PublicIpAddresses[0].Ipaddress
	lb.ipAddrID = l.PublicIpAddresses[0].Id

	return nil
}

// associatePublicIPAddress associates a new IP and sets the address and its ID.
func (lb *loadBalancer) associatePublicIPAddress() error {
	klog.V(4).Infof("Allocate new IP for load balancer: %v", lb.name)
	// If a network belongs to a VPC, the IP address needs to be associated with
	// the VPC instead of with the network.
	network, count, err := lb.Network.GetNetworkByID(lb.networkID, cloudstack.WithProject(lb.projectID))
	if err != nil {
		if count == 0 {
			return fmt.Errorf("could not find network %v", lb.networkID)
		}

		return fmt.Errorf("error retrieving network: %w", err)
	}

	p := lb.Address.NewAssociateIpAddressParams()

	if network.Vpcid != "" {
		p.SetVpcid(network.Vpcid)
	} else {
		p.SetNetworkid(lb.networkID)
	}

	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	// Associate a new IP address
	r, err := lb.Address.AssociateIpAddress(p)
	if err != nil {
		return fmt.Errorf("error associating new IP address: %w", err)
	}

	lb.ipAddr = r.Ipaddress
	lb.ipAddrID = r.Id

	return nil
}

// releasePublicIPAddress releases an associated IP.
func (lb *loadBalancer) releaseLoadBalancerIP() error {
	p := lb.Address.NewDisassociateIpAddressParams(lb.ipAddrID)

	if _, err := lb.Address.DisassociateIpAddress(p); err != nil {
		return fmt.Errorf("error releasing load balancer IP %v: %w", lb.ipAddr, err)
	}

	return nil
}

// checkLoadBalancerRule checks if the rule already exists and if it does, if it can be updated. If
// it does exist but cannot be updated, it will delete the existing rule so it can be created again.
func (lb *loadBalancer) checkLoadBalancerRule(lbRuleName string, port corev1.ServicePort, protocol LoadBalancerProtocol) (*cloudstack.LoadBalancerRule, bool, error) {
	lbRule, ok := lb.rules[lbRuleName]
	if !ok {
		return nil, false, nil
	}

	// Check if any of the values we cannot update (those that require a new load balancer rule) are changed.
	if lbRule.Publicip == lb.ipAddr && lbRule.Privateport == strconv.Itoa(int(port.NodePort)) && lbRule.Publicport == strconv.Itoa(int(port.Port)) {
		updateAlgo := lbRule.Algorithm != lb.algorithm
		updateProto := lbRule.Protocol != protocol.CSProtocol()

		return lbRule, updateAlgo || updateProto, nil
	}

	// Delete the load balancer rule so we can create a new one using the new values.
	if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
		return nil, false, err
	}

	return nil, false, nil
}

// updateLoadBalancerRule updates a load balancer rule.
func (lb *loadBalancer) updateLoadBalancerRule(lbRuleName string, protocol LoadBalancerProtocol) error {
	lbRule := lb.rules[lbRuleName]

	p := lb.LoadBalancer.NewUpdateLoadBalancerRuleParams(lbRule.Id)
	p.SetAlgorithm(lb.algorithm)
	p.SetProtocol(protocol.CSProtocol())

	_, err := lb.LoadBalancer.UpdateLoadBalancerRule(p)

	return err
}

// createLoadBalancerRule creates a new load balancer rule and returns its ID.
func (lb *loadBalancer) createLoadBalancerRule(lbRuleName string, port corev1.ServicePort, protocol LoadBalancerProtocol) (*cloudstack.LoadBalancerRule, error) {
	p := lb.LoadBalancer.NewCreateLoadBalancerRuleParams(
		lb.algorithm,
		lbRuleName,
		int(port.NodePort),
		int(port.Port),
	)

	p.SetNetworkid(lb.networkID)
	p.SetPublicipid(lb.ipAddrID)

	p.SetProtocol(protocol.CSProtocol())

	// Do not open the firewall implicitly, we always create explicit firewall rules
	p.SetOpenfirewall(false)

	// Create a new load balancer rule.
	r, err := lb.LoadBalancer.CreateLoadBalancerRule(p)
	if err != nil {
		return nil, fmt.Errorf("error creating load balancer rule %v: %w", lbRuleName, err)
	}

	lbRule := &cloudstack.LoadBalancerRule{
		Id:          r.Id,
		Algorithm:   r.Algorithm,
		Cidrlist:    r.Cidrlist,
		Name:        r.Name,
		Networkid:   r.Networkid,
		Privateport: r.Privateport,
		Publicport:  r.Publicport,
		Publicip:    r.Publicip,
		Publicipid:  r.Publicipid,
		Protocol:    r.Protocol,
	}

	return lbRule, nil
}

// deleteLoadBalancerRule deletes a load balancer rule.
func (lb *loadBalancer) deleteLoadBalancerRule(lbRule *cloudstack.LoadBalancerRule) error {
	p := lb.LoadBalancer.NewDeleteLoadBalancerRuleParams(lbRule.Id)

	if _, err := lb.LoadBalancer.DeleteLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error deleting load balancer rule %v: %w", lbRule.Name, err)
	}

	// Delete the rule from the map as it no longer exists
	delete(lb.rules, lbRule.Name)

	return nil
}

// assignHostsToRule assigns hosts to a load balancer rule.
func (lb *loadBalancer) assignHostsToRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
	p := lb.LoadBalancer.NewAssignToLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := lb.LoadBalancer.AssignToLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error assigning hosts to load balancer rule %v: %w", lbRule.Name, err)
	}

	return nil
}

// removeHostsFromRule removes hosts from a load balancer rule.
func (lb *loadBalancer) removeHostsFromRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
	p := lb.LoadBalancer.NewRemoveFromLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if _, err := lb.LoadBalancer.RemoveFromLoadBalancerRule(p); err != nil {
		return fmt.Errorf("error removing hosts from load balancer rule %v: %w", lbRule.Name, err)
	}

	return nil
}

// symmetricDifference returns the symmetric difference between the old (existing) and new (wanted) host ID's.
func symmetricDifference(hostIDs []string, lbInstances []*cloudstack.VirtualMachine) ([]string, []string) {
	newIDs := make(map[string]bool)
	for _, hostID := range hostIDs {
		newIDs[hostID] = true
	}

	var remove []string //nolint:prealloc
	for _, instance := range lbInstances {
		if newIDs[instance.Id] {
			delete(newIDs, instance.Id)

			continue
		}

		remove = append(remove, instance.Id)
	}

	var assign []string //nolint:prealloc
	for hostID := range newIDs {
		assign = append(assign, hostID)
	}

	return assign, remove
}

// compareStringSlice compares two unsorted slices of strings without sorting them first.
//
// The slices are equal if and only if both contain the same number of every unique element.
//
// Thanks to: https://stackoverflow.com/a/36000696
func compareStringSlice(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	// create a map of string -> int
	diff := make(map[string]int, len(x))
	for _, _x := range x {
		// 0 value for int is 0, so just increment a counter for the string
		diff[_x]++
	}
	for _, _y := range y {
		// If the string _y is not in diff bail out early
		if _, ok := diff[_y]; !ok {
			return false
		}
		diff[_y]--
		if diff[_y] == 0 {
			delete(diff, _y)
		}
	}

	return len(diff) == 0
}

func ruleToString(rule *cloudstack.FirewallRule) string {
	ls := &strings.Builder{}
	if rule == nil {
		ls.WriteString("nil")
	} else {
		switch rule.Protocol {
		case ProtoTCP:
			fallthrough
		case ProtoUDP:
			fmt.Fprintf(ls, "{[%s] -> %s:[%d-%d] (%s)}", rule.Cidrlist, rule.Ipaddress, rule.Startport, rule.Endport, rule.Protocol)
		case ProtoICMP:
			fmt.Fprintf(ls, "{[%s] -> %s [%d,%d] (%s)}", rule.Cidrlist, rule.Ipaddress, rule.Icmptype, rule.Icmpcode, rule.Protocol)
		default:
			fmt.Fprintf(ls, "{[%s] -> %s (%s)}", rule.Cidrlist, rule.Ipaddress, rule.Protocol)
		}
	}

	return ls.String()
}

func rulesToString(rules []*cloudstack.FirewallRule) string {
	if len(rules) == 0 {
		return "none"
	}

	ls := &strings.Builder{}
	first := true
	for _, rule := range rules {
		if first {
			first = false
		} else {
			ls.WriteString(", ")
		}
		ls.WriteString(ruleToString(rule))
	}

	return ls.String()
}

func rulesMapToString(rules map[*cloudstack.FirewallRule]bool) string {
	if len(rules) == 0 {
		return "none"
	}

	ls := &strings.Builder{}
	first := true
	for rule := range rules {
		if first {
			first = false
		} else {
			ls.WriteString(", ")
		}
		ls.WriteString(ruleToString(rule))
	}

	return ls.String()
}

// updateFirewallRule creates a firewall rule for a load balancer rule
//
// Returns true if the firewall rule was created or updated.
func (lb *loadBalancer) updateFirewallRule(publicIPID string, publicPort int, protocol LoadBalancerProtocol, allowedCIDRs []string) (bool, error) {
	// Default to allow-all if no allowed CIDRs are defined.
	if len(allowedCIDRs) == 0 {
		allowedCIDRs = []string{defaultAllowedCIDR}
	}

	p := lb.Firewall.NewListFirewallRulesParams()
	p.SetIpaddressid(publicIPID)
	p.SetListall(true)
	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}
	r, err := lb.Firewall.ListFirewallRules(p)
	if err != nil {
		return false, fmt.Errorf("error fetching firewall rules for public IP %v: %w", publicIPID, err)
	}
	klog.V(4).Infof("Existing firewall rules for %v: %v", lb.ipAddr, rulesToString(r.FirewallRules))

	// find all rules that have a matching proto+port
	// a map may or may not be faster, but is a bit easier to understand
	filtered := make(map[*cloudstack.FirewallRule]bool)
	for _, rule := range r.FirewallRules {
		if rule.Protocol == protocol.IPProtocol() && rule.Startport == publicPort && rule.Endport == publicPort {
			filtered[rule] = true
		}
	}
	klog.V(4).Infof("Matching rules for %v: %v", lb.ipAddr, rulesMapToString(filtered))

	// determine if we already have a rule with matching cidrs
	var match *cloudstack.FirewallRule
	for rule := range filtered {
		cidrlist := strings.Split(rule.Cidrlist, ",")
		if compareStringSlice(cidrlist, allowedCIDRs) {
			klog.V(4).Infof("Found identical rule: %v", ruleToString(rule))
			match = rule

			break
		}
	}

	if match != nil {
		// no need to create a new rule - but prevent deletion of the matching rule
		delete(filtered, match)
	}

	// delete all other rules that didn't match the CIDR list
	// do this first to prevent CS rule conflict errors
	klog.V(4).Infof("Firewall rules to be deleted for %v: %v", lb.ipAddr, rulesMapToString(filtered))
	for rule := range filtered {
		p := lb.Firewall.NewDeleteFirewallRuleParams(rule.Id)
		_, err = lb.Firewall.DeleteFirewallRule(p)
		if err != nil {
			// report the error, but keep on deleting the other rules
			klog.Errorf("Error deleting old firewall rule %v: %v", rule.Id, err)
		}
	}

	// create new rule if necessary
	if match == nil {
		// no rule found, create a new one
		p := lb.Firewall.NewCreateFirewallRuleParams(publicIPID, protocol.IPProtocol())
		p.SetCidrlist(allowedCIDRs)
		p.SetStartport(publicPort)
		p.SetEndport(publicPort)
		_, err = lb.Firewall.CreateFirewallRule(p)
		if err != nil {
			// return immediately if we can't create the new rule
			return false, fmt.Errorf("error creating new firewall rule for public IP %v, proto %v, port %v, allowed %v: %w", publicIPID, protocol, publicPort, allowedCIDRs, err)
		}
	}

	// return true (because we changed something), but also the last error if deleting one old rule failed
	return true, err
}

// deleteFirewallRule deletes the firewall rule associated with the ip:port:protocol combo
//
// returns true when corresponding rules were deleted.
func (lb *loadBalancer) deleteFirewallRule(publicIPID string, publicPort int, protocol LoadBalancerProtocol) (bool, error) { //nolint:unparam
	p := lb.Firewall.NewListFirewallRulesParams()
	p.SetIpaddressid(publicIPID)
	p.SetListall(true)
	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}
	r, err := lb.Firewall.ListFirewallRules(p)
	if err != nil {
		return false, fmt.Errorf("error fetching firewall rules for public IP %v: %w", publicIPID, err)
	}

	// filter by proto:port
	filtered := make([]*cloudstack.FirewallRule, 0, 1)
	for _, rule := range r.FirewallRules {
		if rule.Protocol == protocol.IPProtocol() && rule.Startport == publicPort && rule.Endport == publicPort {
			filtered = append(filtered, rule)
		}
	}

	// delete all rules
	deleted := false
	for _, rule := range filtered {
		p := lb.Firewall.NewDeleteFirewallRuleParams(rule.Id)
		_, err = lb.Firewall.DeleteFirewallRule(p)
		if err != nil {
			klog.Errorf("Error deleting old firewall rule %v: %v", rule.Id, err)
		} else {
			deleted = true
		}
	}

	return deleted, err
}

// getLoadBalancerSourceRanges first tries to parse and verify loadBalancerSourceRanges field from a Service object.
// If the field is not specified in the Service, try to parse and verify the AnnotationLoadBalancerSourceRangesKey annotation from a service,
// extracting the source ranges to allow. If the annotation is not present either, return a default (allow-all) value.
func getLoadBalancerSourceRanges(service *corev1.Service) (utilnet.IPNetSet, error) {
	var ipnets utilnet.IPNetSet
	var err error
	// if SourceRange field is specified, ignore sourceRange annotation
	if len(service.Spec.LoadBalancerSourceRanges) > 0 {
		specs := service.Spec.LoadBalancerSourceRanges
		ipnets, err = utilnet.ParseIPNets(specs...)
		if err != nil {
			return nil, fmt.Errorf("service.Spec.LoadBalancerSourceRanges: %v is not valid. Expecting a list of IP ranges. For example, 10.0.0.0/24. Error msg: %w", specs, err)
		}
	} else {
		val := service.Annotations[corev1.AnnotationLoadBalancerSourceRangesKey]
		val = strings.TrimSpace(val)
		if val == "" {
			val = defaultAllowedCIDR
		}
		specs := strings.Split(val, ",")
		ipnets, err = utilnet.ParseIPNets(specs...)
		if err != nil {
			return nil, fmt.Errorf("%s: %s is not valid. Expecting a comma-separated list of source IP ranges. For example, 10.0.0.0/24,192.168.2.0/24", corev1.AnnotationLoadBalancerSourceRangesKey, val)
		}
	}

	return ipnets, nil
}

// getStringFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's string value or a specified defaultSetting.
func getStringFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting string) string {
	klog.V(4).InfoS("Attempting to get string value from service annotation", "service", klog.KObj(service), "annotationKey", annotationKey, "defaultSetting", defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		// If there is an annotation for this setting, set the "setting" var to it
		// annotationValue can be empty, it is working as designed
		// it makes possible for instance provisioning loadbalancer without floatingip
		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, annotationValue)

		return annotationValue
	}
	// If there is no annotation, set "settings" var to the value from cloud config
	if defaultSetting != "" {
		klog.V(4).InfoS("Could not find a Service Annotation; falling back on cloud-config setting", "service", klog.KObj(service), "annotationKey", annotationKey, "defaultSetting", defaultSetting)
	}

	return defaultSetting
}

// getBoolFromServiceAnnotation searches a given v1.Service for a specific annotationKey and either returns the annotation's boolean value or a specified defaultSetting.
func getBoolFromServiceAnnotation(service *corev1.Service, annotationKey string, defaultSetting bool) bool {
	klog.V(4).InfoS("Attempting to get bool value from service annotation", "service", klog.KObj(service), "annotationKey", annotationKey, "defaultSetting", defaultSetting)
	if annotationValue, ok := service.Annotations[annotationKey]; ok {
		var returnValue bool
		switch annotationValue {
		case "true":
			returnValue = true
		case "false":
			returnValue = false
		default:
			returnValue = defaultSetting
		}

		klog.V(4).Infof("Found a Service Annotation: %v = %v", annotationKey, returnValue)

		return returnValue
	}
	klog.V(4).InfoS("Could not find a Service Annotation; falling back to default setting", "service", klog.KObj(service), "annotationKey", annotationKey, "defaultSetting", defaultSetting)

	return defaultSetting
}

// setServiceAnnotation is used to create/set or update an annotation on the Service object.
func setServiceAnnotation(service *corev1.Service, key, value string) {
	if service.ObjectMeta.Annotations == nil {
		service.ObjectMeta.Annotations = map[string]string{}
	}
	service.ObjectMeta.Annotations[key] = value
}
