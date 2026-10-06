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
	"time"

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

	// ServiceAnnotationLoadBalancerAddress is the annotation for the IP address assigned to the load balancer.
	// Users can set this annotation to request a specific IP address, replacing the deprecated spec.LoadBalancerIP field.
	// This annotation takes precedence; spec.LoadBalancerIP is only used as a fallback.
	ServiceAnnotationLoadBalancerAddress = "service.beta.kubernetes.io/cloudstack-load-balancer-address"

	// ServiceAnnotationLoadBalancerKeepIP is a boolean annotation that, when set to "true",
	// prevents the public IP from being released when the service is deleted.
	ServiceAnnotationLoadBalancerKeepIP = "service.beta.kubernetes.io/cloudstack-load-balancer-keep-ip"

	// ServiceAnnotationLoadBalancerID stores the CloudStack public IP UUID associated with the load balancer.
	// Used for efficient ID-based lookups instead of keyword-based searches.
	ServiceAnnotationLoadBalancerID = "service.beta.kubernetes.io/cloudstack-load-balancer-id"

	// ServiceAnnotationLoadBalancerNetworkID stores the CloudStack network UUID associated with the load balancer.
	// Used together with ServiceAnnotationLoadBalancerID for scoped ID-based lookups.
	ServiceAnnotationLoadBalancerNetworkID = "service.beta.kubernetes.io/cloudstack-load-balancer-network-id"

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

	// firewallRules has the firewall rules of each public IP ID that updateFirewallRule fetched, so the ports
	// of a service share one listFirewallRules call. Each port only uses the rules for its own protocol and port,
	// and the protocol/port pairs of a service are unique, so the changes for one port do not change the rules
	// that another port uses. A nil map disables this.
	firewallRules map[string][]*cloudstack.FirewallRule

	// pendingJobs has the assign and remove jobs that are still running. nil disables the tracking.
	pendingJobs *pendingJobs
}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
func (cs *CSCloud) GetLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service) (*corev1.LoadBalancerStatus, bool, error) {
	klog.V(4).InfoS("GetLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancer(service, name, legacyName)
	if err != nil {
		return nil, false, err
	}

	// If we don't have any rules, the load balancer does not exist.
	if len(lb.rules) == 0 {
		return nil, false, nil
	}

	klog.V(4).Infof("Found a load balancer associated with IP %v", lb.ipAddr)

	status := &corev1.LoadBalancerStatus{}
	status.Ingress = []corev1.LoadBalancerIngress{{IP: lb.ipAddr}}

	return status, true, nil
}

// EnsureLoadBalancer creates a new load balancer, or updates the existing one. Returns the status of the balancer.
func (cs *CSCloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) (status *corev1.LoadBalancerStatus, err error) { //nolint:gocognit,gocyclo,nestif,maintidx
	klog.V(4).InfoS("EnsureLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))
	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	if len(service.Spec.Ports) == 0 {
		return nil, errors.New("requested load balancer with no ports")
	}

	// Drop the cached VM list on failure, so the retry uses a new list (for example when a VM was deleted).
	// Keep it when CloudStack throttled the request or a job is still running, so the retries do not each
	// fetch the list again.
	defer func() {
		if err != nil && !keepVMCache(err) {
			cs.vmCache.invalidate()
		}
	}()

	// Patch the service with new/updated annotations if needed after EnsureLoadBalancer finishes.
	patcher := newServicePatcher(cs.kclient, service)
	defer func() { err = patcher.Patch(ctx, err) }()

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancer(service, name, legacyName)
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

	// Resolve the desired IP: annotation takes precedence, spec.LoadBalancerIP is fallback.
	desiredIP := getLoadBalancerAddress(service)

	if !lb.hasLoadBalancerIP() { //nolint:nestif
		// Before allocating a new IP, check the service annotation for a previously assigned IP.
		// This handles recovery from partial failures where the IP was allocated and annotated
		// but subsequent operations (rule creation) failed.
		annotatedIP := getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress, "")
		if annotatedIP != "" {
			found, lookupErr := lb.lookupPublicIPAddress(annotatedIP)
			if lookupErr != nil {
				klog.Warningf("Error looking up annotated IP %v for recovery: %v", annotatedIP, lookupErr)
			} else if found {
				klog.V(4).Infof("Recovered previously allocated IP %v from annotation", annotatedIP)
			}
		}

		if !lb.hasLoadBalancerIP() {
			// Create or retrieve the load balancer IP.
			if err := lb.getLoadBalancerIP(desiredIP); err != nil {
				return nil, err
			}
		}

		msg := fmt.Sprintf("Created new load balancer for service %s with algorithm '%s' and IP address %s", serviceName, lb.algorithm, lb.ipAddr)
		cs.eventRecorder.Event(service, corev1.EventTypeNormal, "CreatedLoadBalancer", msg)
		klog.Info(msg)
	} else if desiredIP != "" && desiredIP != lb.ipAddr {
		// IP reassignment on an active load balancer is not supported.
		// Users must delete and recreate the service to change the IP.
		msg := fmt.Sprintf("Load balancer IP change from %s to %s is not supported; delete and recreate the service to use a different IP", lb.ipAddr, desiredIP)
		cs.eventRecorder.Event(service, corev1.EventTypeWarning, "IPChangeNotSupported", msg)
		klog.Warning(msg)
	}

	klog.V(4).Infof("Load balancer %v is associated with IP %v", lb.name, lb.ipAddr)

	// Set the load balancer annotations on the Service
	setServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress, lb.ipAddr)
	setServiceAnnotation(service, ServiceAnnotationLoadBalancerID, lb.ipAddrID)
	setServiceAnnotation(service, ServiceAnnotationLoadBalancerNetworkID, lb.networkID)

	// These lookups give the same result for each port, so they are done at the first port and reused for the
	// other ports. This reduces the number of API calls but keeps the order of the calls for the first port.
	var network *cloudstack.Network
	var lbSourceRanges utilnet.IPNetSet
	lb.firewallRules = map[string][]*cloudstack.FirewallRule{}

	desiredRuleNames, desiredFirewallPorts := desiredRules(lb.name, service)

	// Rules with a CloudStack job that is still running. The other rules are still reconciled, and the service
	// is retried later.
	var pendingRules []string

	for _, port := range service.Spec.Ports {
		// Construct the protocol name first, we need it a few times
		protocol := ProtocolFromServicePort(port, service)
		if protocol == LoadBalancerProtocolInvalid {
			return nil, fmt.Errorf("unsupported load balancer protocol: %v", port.Protocol)
		}

		// All ports have their own load balancer rule, so add the port to lbName to keep the names unique.
		lbRuleName := loadBalancerRuleName(lb.name, protocol, port.Port)

		// If the protocol of the port changed between tcp and tcp-proxy, the rule name changed too. CloudStack does
		// not allow a second rule on the same public port, so switch the existing rule instead of creating one.
		if err := cs.switchLoadBalancerRule(lb, service, lbRuleName, port, protocol, desiredRuleNames); err != nil {
			return nil, err
		}

		// If the load balancer rule exists and is up-to-date, we move on to the next rule.
		_, ruleExisted := lb.rules[lbRuleName]
		lbRule, needsUpdate, err := lb.checkLoadBalancerRule(lbRuleName, port, protocol)
		if err != nil {
			return nil, err
		}

		if ruleExisted && lbRule == nil {
			// checkLoadBalancerRule deleted the load balancer rule. Fetch the firewall rules again in
			// case CloudStack also changed them.
			clear(lb.firewallRules)
		}

		if lbRule != nil { //nolint:nestif
			if needsUpdate {
				klog.V(4).Infof("Updating load balancer rule: %v", lbRuleName)
				if err := lb.updateLoadBalancerRule(lbRuleName, protocol); err != nil {
					return nil, err
				}
			} else {
				klog.V(4).Infof("Load balancer rule %v is up-to-date", lbRuleName)
			}

			if err := lb.reconcileHostsForRule(lbRule, lb.hostIDs); err != nil {
				if !errors.Is(err, errJobPending) {
					return nil, err
				}
				pendingRules = append(pendingRules, err.Error())
			}

			// Delete the rule from the map, to prevent it being deleted.
			delete(lb.rules, lbRuleName)
		} else {
			klog.V(4).Infof("Creating load balancer rule: %v", lbRuleName)
			lbRule, err = lb.createLoadBalancerRule(lbRuleName, port, protocol)
			if err != nil {
				return nil, err
			}

			klog.V(4).Infof("Assigning hosts (%v) to load balancer rule: %v", lb.hostIDs, lbRuleName)
			if err = lb.assignHostsToRule(lbRule, lb.hostIDs); err != nil {
				if !errors.Is(err, errJobPending) {
					return nil, err
				}
				pendingRules = append(pendingRules, err.Error())
			}
		}

		if network == nil {
			var count int
			network, count, err = lb.Network.GetNetworkByID(lb.networkID, cloudstack.WithProject(lb.projectID))
			if err != nil {
				if count == 0 {
					return nil, fmt.Errorf("could not find network with ID %s: %w", lb.networkID, err)
				}

				return nil, fmt.Errorf("failed to get network with ID %s: %w", lb.networkID, err)
			}

			lbSourceRanges, err = getLoadBalancerSourceRanges(service)
			if err != nil {
				return nil, err
			}
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

		// Keep the firewall rules if a port of the service still uses the same protocol and port, for example when
		// both a tcp and a tcp-proxy rule exist for the same port.
		if desiredFirewallPorts[firewallPortKey(protocol, int(port))] {
			klog.V(4).Infof("Keeping firewall rules of load balancer rule %v, because %v/%v is still in use", lbRule.Name, protocol.IPProtocol(), port)
		} else {
			klog.V(4).Infof("Deleting firewall rules associated with load balancer rule: %v (%v:%v:%v)", lbRule.Name, protocol, lbRule.Publicip, port)
			if _, err := lb.deleteFirewallRule(lbRule.Publicipid, int(port), protocol); err != nil {
				return nil, err
			}
		}

		klog.V(4).Infof("Deleting obsolete load balancer rule: %v", lbRule.Name)
		if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
			return nil, err
		}
	}

	if err := pendingJobsError(pendingRules); err != nil {
		return nil, err
	}

	return lb.generateLoadBalancerStatus(service), nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (cs *CSCloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *corev1.Service, nodes []*corev1.Node) (err error) {
	klog.V(4).InfoS("UpdateLoadBalancer", "cluster", clusterName, "service", klog.KObj(service))

	// Drop the cached VM list on failure, so the retry uses a new list (for example when a VM was deleted).
	// Keep it when CloudStack throttled the request or a job is still running, so the retries do not each
	// fetch the list again.
	defer func() {
		if err != nil && !keepVMCache(err) {
			cs.vmCache.invalidate()
		}
	}()

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancer(service, name, legacyName)
	if err != nil {
		return err
	}

	// Verify that all the hosts belong to the same network, and retrieve their ID's.
	lb.hostIDs, _, err = cs.verifyHosts(nodes)
	if err != nil {
		return err
	}

	// Rules with a CloudStack job that is still running. The other rules are still reconciled, and the service
	// is retried later.
	var pendingRules []string
	for _, lbRule := range lb.rules {
		if err := lb.reconcileHostsForRule(lbRule, lb.hostIDs); err != nil {
			if !errors.Is(err, errJobPending) {
				return err
			}
			pendingRules = append(pendingRules, err.Error())
		}
	}

	return pendingJobsError(pendingRules)
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
func (cs *CSCloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *corev1.Service) (err error) {
	klog.V(4).InfoS("EnsureLoadBalancerDeleted", "cluster", clusterName, "service", klog.KObj(service))

	// Patch the service to remove annotations after EnsureLoadBalancerDeleted finishes.
	patcher := newServicePatcher(cs.kclient, service)
	defer func() { err = patcher.Patch(ctx, err) }()

	// Get the load balancer details and existing rules.
	name := cs.GetLoadBalancerName(ctx, clusterName, service)
	legacyName := cs.getLoadBalancerLegacyName(ctx, clusterName, service)
	lb, err := cs.getLoadBalancer(service, name, legacyName)
	if err != nil {
		return err
	}

	// If no rules exist, the load balancer doesn't exist. However, an IP may have been
	// orphaned from a previous partial failure. Check the service annotation for cleanup.
	if len(lb.rules) == 0 {
		klog.V(4).Infof("No load balancer rules found for service, checking annotation for orphaned IP")

		if err := cs.releaseOrphanedIPIfNeeded(lb, service); err != nil {
			return err
		}

		// If the service is not marked for deletion (f.e. when switching from type
		// LoadBalancer to ClusterIP), remove our annotations.
		if service.DeletionTimestamp.IsZero() {
			deleteLoadBalancerAnnotations(service)
		}

		return nil
	}

	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	var deletionErrors []error

	// Delete all firewall rules and load balancer rules
	for _, lbRule := range lb.rules {
		klog.V(4).Infof("Processing deletion of load balancer rule: %v", lbRule.Name)

		// Parse protocol
		protocol := ProtocolFromLoadBalancer(lbRule.Protocol)
		if protocol == LoadBalancerProtocolInvalid {
			err := fmt.Errorf("error parsing protocol %v for rule %v", lbRule.Protocol, lbRule.Name)
			klog.Errorf("%v", err)
			deletionErrors = append(deletionErrors, err)
			// Continue to delete other rules even if this one fails
			continue
		}

		// Parse port
		port, err := strconv.ParseInt(lbRule.Publicport, 10, 32)
		if err != nil {
			err := fmt.Errorf("error parsing port %s for rule %v: %w", lbRule.Publicport, lbRule.Name, err)
			klog.Errorf("%v", err)
			deletionErrors = append(deletionErrors, err)
			// Continue to delete other rules even if this one fails
			continue
		}

		// Delete firewall rules first
		klog.V(4).Infof("Deleting firewall rules for load balancer rule: %v (IP:%v, Port:%d, Protocol:%v)",
			lbRule.Name, lbRule.Publicip, port, protocol)
		if _, err := lb.deleteFirewallRule(lbRule.Publicipid, int(port), protocol); err != nil {
			err := fmt.Errorf("error deleting firewall rules for rule %v: %w", lbRule.Name, err)
			klog.Errorf("%v", err)
			deletionErrors = append(deletionErrors, err)
			// Continue to delete the load balancer rule even if firewall deletion fails
		}

		// Delete load balancer rule
		klog.V(4).Infof("Deleting load balancer rule: %v", lbRule.Name)
		if err := lb.deleteLoadBalancerRule(lbRule); err != nil {
			err := fmt.Errorf("error deleting load balancer rule %v: %w", lbRule.Name, err)
			klog.Errorf("%v", err)
			deletionErrors = append(deletionErrors, err)
			// Continue to attempt IP cleanup even if this rule deletion fails
		}
	}

	// Delete the public IP address if appropriate
	if lb.ipAddr != "" { //nolint:nestif
		klog.V(4).Infof("Processing public IP deletion for load balancer: IP=%v, ID=%v", lb.ipAddr, lb.ipAddrID)

		// Check if we should release the IP
		shouldReleaseIP, err := cs.shouldReleaseLoadBalancerIP(lb, service)
		switch {
		case err != nil:
			err := fmt.Errorf("error determining if IP should be released: %w", err)
			klog.Errorf("%v", err)
			deletionErrors = append(deletionErrors, err)
		case shouldReleaseIP:
			klog.V(4).Infof("Releasing load balancer IP: %v", lb.ipAddr)
			if err := lb.releaseLoadBalancerIP(); err != nil {
				err := fmt.Errorf("error releasing load balancer IP %v: %w", lb.ipAddr, err)
				klog.Errorf("%v", err)
				deletionErrors = append(deletionErrors, err)
			} else {
				msg := fmt.Sprintf("Released load balancer IP %s for service %s", lb.ipAddr, serviceName)
				cs.eventRecorder.Event(service, corev1.EventTypeNormal, "ReleasedLoadBalancerIP", msg)
				klog.Info(msg)
			}
		default:
			klog.V(4).Infof("Load balancer IP %v is in use by other services, keeping it allocated", lb.ipAddr)
		}
	}

	// Return aggregated errors if any occurred
	if len(deletionErrors) > 0 {
		msg := fmt.Sprintf("Encountered %d error(s) while deleting load balancer for service %s", len(deletionErrors), serviceName)
		klog.Warningf("%s: %v", msg, deletionErrors)
		cs.eventRecorder.Event(service, corev1.EventTypeWarning, "DeletingLoadBalancerFailed", msg)

		// Return the first error or a combined error message
		return fmt.Errorf("load balancer deletion completed with errors: %w", deletionErrors[0])
	}

	// If the service is not marked for deletion (f.e. when switching from type
	// LoadBalancer to ClusterIP), remove our annotations.
	if service.DeletionTimestamp.IsZero() {
		deleteLoadBalancerAnnotations(service)
	}

	msg := "Successfully deleted load balancer for service " + serviceName
	cs.eventRecorder.Event(service, corev1.EventTypeNormal, "DeletedLoadBalancer", msg)
	klog.Info(msg)

	return nil
}

// shouldReleaseLoadBalancerIP determines whether the public IP should be released.
func (cs *CSCloud) shouldReleaseLoadBalancerIP(lb *loadBalancer, service *corev1.Service) (bool, error) {
	// If the keep-ip annotation is set to true, don't release the IP.
	// The user is responsible for managing the lifecycle of kept IPs.
	if getBoolFromServiceAnnotation(service, ServiceAnnotationLoadBalancerKeepIP, false) {
		klog.V(4).Infof("IP %v has keep-ip annotation set, not releasing", lb.ipAddr)

		return false, nil
	}

	// Check if this IP is used by other load balancer rules (other services)
	p := lb.LoadBalancer.NewListLoadBalancerRulesParams()
	p.SetPublicipid(lb.ipAddrID)
	p.SetListall(true)
	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	otherRules, err := lb.LoadBalancer.ListLoadBalancerRules(p)
	if err != nil {
		return false, fmt.Errorf("error checking for other load balancer rules using IP %v: %w", lb.ipAddr, err)
	}

	// If other rules exist, this IP is in use by other services
	if otherRules.Count > 0 {
		klog.V(4).Infof("IP %v has %d other load balancer rule(s) in use, not releasing", lb.ipAddr, otherRules.Count)

		return false, nil
	}

	// IP is safe to release - it's either controller-allocated or no longer in use
	klog.V(4).Infof("IP %v is no longer in use and safe to release", lb.ipAddr)

	return true, nil
}

// releaseOrphanedIPIfNeeded checks the service annotation for an orphaned IP and releases it if appropriate.
// This handles the case where all LB rules were successfully deleted but IP release failed on a prior attempt.
func (cs *CSCloud) releaseOrphanedIPIfNeeded(lb *loadBalancer, service *corev1.Service) error {
	annotatedIP := getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress, "")
	if annotatedIP == "" {
		return nil
	}

	found, lookupErr := lb.lookupPublicIPAddress(annotatedIP)
	if lookupErr != nil {
		klog.Warningf("Error looking up annotated IP %v during delete: %v", annotatedIP, lookupErr)

		return nil
	}

	if !found {
		return nil
	}

	shouldRelease, shouldErr := cs.shouldReleaseLoadBalancerIP(lb, service)
	if shouldErr != nil {
		klog.Warningf("Error checking if annotated IP %v should be released: %v", annotatedIP, shouldErr)

		return nil
	}

	if !shouldRelease {
		klog.V(4).Infof("Annotated IP %v should not be released (keep-ip set or has other rules)", annotatedIP)

		return nil
	}

	if releaseErr := lb.releaseLoadBalancerIP(); releaseErr != nil {
		return fmt.Errorf("error releasing orphaned load balancer IP %v: %w", annotatedIP, releaseErr)
	}

	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	msg := fmt.Sprintf("Released orphaned load balancer IP %s for service %s", annotatedIP, serviceName)
	cs.eventRecorder.Event(service, corev1.EventTypeNormal, "ReleasedOrphanedIP", msg)
	klog.Info(msg)

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

// filterRulesByPrefix returns only the rules whose Name starts with the given prefix.
// This is needed because CloudStack's SetKeyword uses LIKE %keyword% matching,
// which can return rules belonging to other services with overlapping name substrings.
func filterRulesByPrefix(rules []*cloudstack.LoadBalancerRule, prefix string) []*cloudstack.LoadBalancerRule {
	var filtered []*cloudstack.LoadBalancerRule
	for _, rule := range rules {
		if strings.HasPrefix(rule.Name, prefix) {
			filtered = append(filtered, rule)
		}
	}

	return filtered
}

// getLoadBalancer tries to find the load balancer using ID-based lookup first (if annotations
// are present), then falls back to the keyword-based name lookup.
func (cs *CSCloud) getLoadBalancer(service *corev1.Service, name, legacyName string) (*loadBalancer, error) {
	if ipAddrID := getLoadBalancerID(service); ipAddrID != "" {
		networkID := getLoadBalancerNetworkID(service)
		klog.V(4).Infof("Attempting ID-based load balancer lookup: ipAddrID=%v, networkID=%v", ipAddrID, networkID)

		lb, err := cs.getLoadBalancerByID(name, ipAddrID, networkID)
		if err != nil {
			return nil, err
		}

		if len(lb.rules) > 0 {
			return lb, nil
		}

		klog.V(4).Infof("ID-based lookup returned no rules, falling back to name-based lookup")
	}

	return cs.getLoadBalancerByName(name, legacyName)
}

// getLoadBalancerByName retrieves the IP address and ID and all the existing rules it can find.
func (cs *CSCloud) getLoadBalancerByName(name, legacyName string) (*loadBalancer, error) {
	lb := &loadBalancer{
		CloudStackClient: cs.client,
		pendingJobs:      cs.pendingJobs,
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

	// Filter keyword results to exact prefix matches. CloudStack's SetKeyword uses
	// LIKE %keyword% matching, so searching for "foo" can also return "foobar" rules.
	filtered := filterRulesByPrefix(l.LoadBalancerRules, lb.name+"-")

	// If no rules were found, check the legacy name.
	if len(filtered) == 0 { //nolint:nestif
		if len(legacyName) > 0 {
			p.SetKeyword(legacyName)
			l, err = cs.client.LoadBalancer.ListLoadBalancerRules(p)
			if err != nil {
				return nil, fmt.Errorf("error retrieving load balancer rules: %w", err)
			}
			legacyFiltered := filterRulesByPrefix(l.LoadBalancerRules, legacyName+"-")
			if len(legacyFiltered) > 0 {
				lb.name = legacyName
				filtered = legacyFiltered
			}
		} else {
			return lb, nil
		}
	}

	for _, lbRule := range filtered {
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

// getLoadBalancerByID retrieves load balancer rules by public IP ID and network ID.
// This is more reliable than keyword-based search as it uses exact ID matching.
func (cs *CSCloud) getLoadBalancerByID(name, ipAddrID, networkID string) (*loadBalancer, error) {
	lb := &loadBalancer{
		CloudStackClient: cs.client,
		pendingJobs:      cs.pendingJobs,
		name:             name,
		projectID:        cs.projectID,
		rules:            make(map[string]*cloudstack.LoadBalancerRule),
	}

	p := cs.client.LoadBalancer.NewListLoadBalancerRulesParams()
	p.SetPublicipid(ipAddrID)
	p.SetListall(true)

	if networkID != "" {
		p.SetNetworkid(networkID)
	}

	if cs.projectID != "" {
		p.SetProjectid(cs.projectID)
	}

	l, err := cs.client.LoadBalancer.ListLoadBalancerRules(p)
	if err != nil {
		return nil, fmt.Errorf("error retrieving load balancer rules by IP ID %v: %w", ipAddrID, err)
	}

	filtered := filterRulesByPrefix(l.LoadBalancerRules, lb.name+"-")
	for _, lbRule := range filtered {
		lb.rules[lbRule.Name] = lbRule

		if lb.ipAddr != "" && lb.ipAddr != lbRule.Publicip {
			klog.Warningf("Load balancer %v has rules associated with different IP's: %v, %v", lb.name, lb.ipAddr, lbRule.Publicip)
		}

		lb.ipAddr = lbRule.Publicip
		lb.ipAddrID = lbRule.Publicipid
		lb.networkID = lbRule.Networkid
	}

	klog.V(4).Infof("Load balancer %v (by ID %v) contains %d rule(s)", lb.name, ipAddrID, len(lb.rules))

	return lb, nil
}

// verifyHosts verifies if all hosts belong to the same network, and returns the host ID's and network ID.
// During rolling upgrades some nodes may not yet have a corresponding VM in CloudStack, so we tolerate
// partial matches: as long as at least one node can be resolved we return the matched set and log
// warnings for the nodes we could not find.
//
// The list of VMs can come from the VM cache. If the cached list does not give a complete match, or a node
// is newer than the cached list, a new list is fetched and the match is done again. So the result is the
// same as with a new list, unless VMs that match a node were changed or deleted within the cache TTL.
func (cs *CSCloud) verifyHosts(nodes []*corev1.Node) ([]string, string, error) {
	// Fetch all VMs using pagination to avoid missing VMs when the project has many instances.
	list, err := cs.vmCache.get(cs.listAllVirtualMachines, nil)
	if err != nil {
		return nil, "", fmt.Errorf("error retrieving list of hosts: %w", err)
	}

	m := matchHosts(nodes, list.vms)
	if !list.fresh && m.needsFreshList(nodes, list.fetchedAt) {
		klog.V(4).Infof("Cached list of VMs does not match all %d node(s), fetching a new list", len(nodes))
		list, err = cs.vmCache.get(cs.listAllVirtualMachines, &list)
		if err != nil {
			return nil, "", fmt.Errorf("error retrieving list of hosts: %w", err)
		}
		m = matchHosts(nodes, list.vms)
	}

	if !list.fresh {
		klog.V(4).Infof("Using cached list of %d VM(s), fetched %v ago", len(list.vms), time.Since(list.fetchedAt).Round(time.Millisecond))
	}

	for i, name := range m.skippedNoNIC {
		klog.Warningf("Skipping VM %v (id: %v) as it contains no active network interfaces (may still be provisioning)", name, m.skippedNoNICIDs[i])
	}

	if m.err != nil {
		return nil, "", m.err
	}

	// Log warnings for nodes that could not be matched — this is expected during rolling upgrades.
	if len(m.unmatchedNodes) > 0 {
		klog.Warningf("Could not match %d node(s) to CloudStack VMs (may be provisioning or terminating): %v", len(m.unmatchedNodes), m.unmatchedNodes)
	}
	if len(m.skippedNoNIC) > 0 {
		klog.Warningf("Skipped %d VM(s) with no NICs (still provisioning): %v", len(m.skippedNoNIC), m.skippedNoNIC)
	}

	if len(m.hostIDs) == 0 || len(m.networkID) == 0 {
		return nil, "", fmt.Errorf("could not match any of the %d node(s) to VMs in CloudStack (unmatched: %v, skipped-no-nic: %v)",
			len(nodes), m.unmatchedNodes, m.skippedNoNIC)
	}

	klog.V(4).Infof("Matched %d of %d nodes to CloudStack VMs", len(m.hostIDs), len(nodes))

	return m.hostIDs, m.networkID, nil
}

// hostMatch is the result of matching nodes to VMs.
type hostMatch struct {
	hostIDs        []string
	networkID      string
	unmatchedNodes []string
	skippedNoNIC   []string
	// skippedNoNICIDs has the IDs of the VMs in skippedNoNIC, in the same order.
	skippedNoNICIDs []string
	// missingProviderID is true if the VM ID in the ProviderID of a node is not in the list of VMs.
	missingProviderID bool
	err               error
}

// matchHosts matches the nodes to the VMs by name and by the VM ID in the ProviderID of the node.
func matchHosts(nodes []*corev1.Node, allVMs []*cloudstack.VirtualMachine) hostMatch {
	var m hostMatch

	hostNames := map[string]bool{}
	// providerVMIDs maps CloudStack VM IDs extracted from node.Spec.ProviderID
	// so we can match by ID in addition to name.
	providerVMIDs := map[string]bool{}
	for _, node := range nodes {
		// node.Name can be an FQDN as well, and CloudStack VM names aren't
		// To match, we need to Split the domain part off here, if present
		hostNames[strings.Split(strings.ToLower(node.Name), ".")[0]] = true

		// Also extract the VM ID from the ProviderID for a more reliable match.
		if node.Spec.ProviderID != "" {
			if id, _, err := instanceIDFromProviderID(node.Spec.ProviderID); err == nil {
				providerVMIDs[id] = true
			}
		}
	}

	matchedNames := map[string]bool{}
	matchedVMIDs := map[string]bool{}
	foundVMIDs := map[string]bool{}

	// Check if the virtual machine is in the hosts slice, then add the corresponding ID.
	for _, vm := range allVMs {
		nameMatch := hostNames[strings.ToLower(vm.Name)]
		idMatch := providerVMIDs[vm.Id]
		if idMatch {
			foundVMIDs[vm.Id] = true
		}
		if nameMatch || idMatch {
			if len(vm.Nic) == 0 {
				m.skippedNoNIC = append(m.skippedNoNIC, vm.Name)
				m.skippedNoNICIDs = append(m.skippedNoNICIDs, vm.Id)
				// Skip VM's without any active network interfaces. This happens during rollout f.e.
				continue
			}
			if m.networkID != "" && m.networkID != vm.Nic[0].Networkid {
				m.err = errors.New("found hosts that belong to different networks")

				return m
			}

			m.networkID = vm.Nic[0].Networkid
			m.hostIDs = append(m.hostIDs, vm.Id)
			matchedNames[strings.ToLower(vm.Name)] = true
			matchedVMIDs[vm.Id] = true
		}
	}

	m.missingProviderID = len(foundVMIDs) < len(providerVMIDs)

	// A node is matched by its name, or by the VM ID in its ProviderID when the node name differs from the VM name.
	for _, node := range nodes {
		shortName, _, _ := strings.Cut(strings.ToLower(node.Name), ".")
		if matchedNames[shortName] {
			continue
		}
		if id, _, err := instanceIDFromProviderID(node.Spec.ProviderID); err == nil && matchedVMIDs[id] {
			continue
		}
		m.unmatchedNodes = append(m.unmatchedNodes, node.Name)
	}

	return m
}

// needsFreshList returns true if a match on a cached list of VMs (fetched at fetchedAt) may differ from a match
// on a new list.
func (m hostMatch) needsFreshList(nodes []*corev1.Node, fetchedAt time.Time) bool {
	if m.err != nil || m.missingProviderID || len(m.unmatchedNodes) > 0 || len(m.skippedNoNIC) > 0 {
		return true
	}

	// The API server stores the creation time of a node in whole seconds. A node that was created in the same
	// second as the fetch may be newer than the list, so it also needs a new list.
	fetchedAtSecond := fetchedAt.Truncate(time.Second)
	for _, node := range nodes {
		if !node.CreationTimestamp.Time.Before(fetchedAtSecond) {
			return true
		}
	}

	return false
}

// listAllVirtualMachines retrieves all VMs using pagination to handle large projects.
func (cs *CSCloud) listAllVirtualMachines() ([]*cloudstack.VirtualMachine, error) {
	var allVMs []*cloudstack.VirtualMachine

	page := 1
	pageSize := 500

	for {
		p := cs.client.VirtualMachine.NewListVirtualMachinesParams()
		p.SetListall(true)
		p.SetDetails([]string{"min", "nics"})
		p.SetPage(page)
		p.SetPagesize(pageSize)

		if cs.projectID != "" {
			p.SetProjectid(cs.projectID)
		}

		l, err := cs.client.VirtualMachine.ListVirtualMachines(p)
		if err != nil {
			return nil, fmt.Errorf("failed to list virtual machines: %w", err)
		}

		allVMs = append(allVMs, l.VirtualMachines...)

		// If we got fewer results than the page size, we've reached the last page.
		if len(l.VirtualMachines) < pageSize {
			break
		}
		page++
	}

	return allVMs, nil
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

// lookupPublicIPAddress checks whether the given IP address is already allocated in CloudStack.
// If found and allocated, it sets lb.ipAddr and lb.ipAddrID and returns (true, nil).
// If not found or not allocated, it returns (false, nil) without modifying lb state.
// Unlike getPublicIPAddress, this method does NOT call associatePublicIPAddress for unallocated IPs.
func (lb *loadBalancer) lookupPublicIPAddress(ip string) (bool, error) {
	p := lb.Address.NewListPublicIpAddressesParams()
	p.SetIpaddress(ip)
	p.SetAllocatedonly(true)
	p.SetListall(true)

	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	l, err := lb.Address.ListPublicIpAddresses(p)
	if err != nil {
		return false, fmt.Errorf("error looking up IP address %v: %w", ip, err)
	}

	if l.Count != 1 {
		return false, nil
	}

	lb.ipAddr = l.PublicIpAddresses[0].Ipaddress
	lb.ipAddrID = l.PublicIpAddresses[0].Id

	return true, nil
}

// getPublicIPAddressID retrieves the ID of the given IP, and sets the address and its ID.
func (lb *loadBalancer) getPublicIPAddress(loadBalancerIP string) error {
	klog.V(4).Infof("Retrieve load balancer IP details: %v", loadBalancerIP)

	p := lb.Address.NewListPublicIpAddressesParams()
	p.SetIpaddress(loadBalancerIP)
	p.SetAllocatedonly(false)
	p.SetListall(true)

	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}

	l, err := lb.Address.ListPublicIpAddresses(p)
	if err != nil {
		return fmt.Errorf("error retrieving IP address: %w", err)
	}

	if l.Count != 1 {
		return fmt.Errorf("could not find IP address %v. Found %d addresses", loadBalancerIP, l.Count)
	}

	lb.ipAddr = l.PublicIpAddresses[0].Ipaddress
	lb.ipAddrID = l.PublicIpAddresses[0].Id

	// If the IP Address is not allocated then associate it
	if l.PublicIpAddresses[0].Allocated == "" {
		return lb.associatePublicIPAddress()
	}

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

	if lb.ipAddr != "" {
		p.SetIpaddress(lb.ipAddr)
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

// loadBalancerRuleName returns the name of the load balancer rule for a protocol and port.
func loadBalancerRuleName(lbName string, protocol LoadBalancerProtocol, port int32) string {
	return fmt.Sprintf("%s-%s-%d", lbName, protocol, port)
}

// firewallPortKey returns the key of the firewall rules for a protocol and port. The tcp and tcp-proxy
// protocols have the same key, because they use the same firewall rules.
func firewallPortKey(protocol LoadBalancerProtocol, port int) string {
	return fmt.Sprintf("%s/%d", protocol.IPProtocol(), port)
}

// desiredRules returns the names of the load balancer rules and the firewall keys (see firewallPortKey) for the
// ports of the service. Ports with an invalid protocol are skipped; EnsureLoadBalancer returns an error for them.
func desiredRules(lbName string, service *corev1.Service) (map[string]bool, map[string]bool) {
	ruleNames := map[string]bool{}
	firewallPorts := map[string]bool{}
	for _, port := range service.Spec.Ports {
		protocol := ProtocolFromServicePort(port, service)
		if protocol == LoadBalancerProtocolInvalid {
			continue
		}
		ruleNames[loadBalancerRuleName(lbName, protocol, port.Port)] = true
		firewallPorts[firewallPortKey(protocol, int(port.Port))] = true
	}

	return ruleNames, firewallPorts
}

// findRuleForPort returns an existing load balancer rule for the same public IP, public port and IP protocol as
// the port, but with another name. This is the rule of the port before its protocol changed between tcp and
// tcp-proxy. Rules that another port of the service uses (desired) are not returned.
func (lb *loadBalancer) findRuleForPort(lbRuleName string, port corev1.ServicePort, protocol LoadBalancerProtocol, desired map[string]bool) *cloudstack.LoadBalancerRule {
	for name, rule := range lb.rules {
		if name == lbRuleName || desired[name] {
			continue
		}
		if rule.Publicip == lb.ipAddr && rule.Publicport == strconv.Itoa(int(port.Port)) &&
			ProtocolFromLoadBalancer(rule.Protocol).IPProtocol() == protocol.IPProtocol() {
			return rule
		}
	}

	return nil
}

// switchLoadBalancerRule switches the existing rule of a port to the new rule name and protocol, if the protocol
// of the port changed between tcp and tcp-proxy. If the node port is the same, the rule is updated in place, so its
// hosts and firewall rules stay. Else the old rule is deleted, so the new rule can be created. The firewall rules
// of the port are kept in both cases.
func (cs *CSCloud) switchLoadBalancerRule(lb *loadBalancer, service *corev1.Service, lbRuleName string, port corev1.ServicePort, protocol LoadBalancerProtocol, desired map[string]bool) error {
	if _, ok := lb.rules[lbRuleName]; ok {
		return nil
	}

	old := lb.findRuleForPort(lbRuleName, port, protocol, desired)
	if old == nil {
		return nil
	}

	// The firewall rules are not changed, but fetch them again in case CloudStack changed them.
	defer clear(lb.firewallRules)

	if old.Privateport != strconv.Itoa(int(port.NodePort)) {
		klog.Infof("Deleting load balancer rule %v, because it is replaced by %v with another node port", old.Name, lbRuleName)

		return lb.deleteLoadBalancerRule(old)
	}

	if err := lb.renameLoadBalancerRule(old, lbRuleName, protocol); err != nil {
		return err
	}

	msg := fmt.Sprintf("Updated load balancer rule %s to %s with protocol %s", old.Name, lbRuleName, protocol.CSProtocol())
	cs.eventRecorder.Event(service, corev1.EventTypeNormal, "UpdatedLoadBalancerRule", msg)
	klog.Info(msg)

	return nil
}

// renameLoadBalancerRule updates the name, protocol and algorithm of an existing load balancer rule.
func (lb *loadBalancer) renameLoadBalancerRule(old *cloudstack.LoadBalancerRule, lbRuleName string, protocol LoadBalancerProtocol) error {
	p := lb.LoadBalancer.NewUpdateLoadBalancerRuleParams(old.Id)
	p.SetName(lbRuleName)
	p.SetAlgorithm(lb.algorithm)
	p.SetProtocol(protocol.CSProtocol())

	if _, err := lb.LoadBalancer.UpdateLoadBalancerRule(p); err != nil {
		return fmt.Errorf("failed to update load balancer rule %v to %v: %w", old.Name, lbRuleName, err)
	}

	renamed := *old
	renamed.Name = lbRuleName
	renamed.Algorithm = lb.algorithm
	renamed.Protocol = protocol.CSProtocol()
	delete(lb.rules, old.Name)
	lb.rules[lbRuleName] = &renamed

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
	if err != nil {
		return fmt.Errorf("failed to update loadbalancer rule with ID %s: %w", lbRule.Id, err)
	}

	return nil
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
	lb.pendingJobs.delete(lbRule.Id)

	return nil
}

// reconcileHostsForRule ensures the load balancer rule has exactly the expected set of hosts.
// It lists the current members, computes the difference, and assigns new hosts before removing
// old ones so the rule always has backends during rolling upgrades.
//
// If an earlier assign or remove job for the rule is still running, it returns an error that wraps errJobPending
// and changes nothing, so the same job is not sent again.
func (lb *loadBalancer) reconcileHostsForRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
	if err := lb.checkPendingJob(lbRule); err != nil {
		return err
	}

	// The node sync and the service sync can reconcile the same service at the same time. Only one of them
	// changes a rule, so the other one does not send the same job.
	if !lb.pendingJobs.claim(lbRule.Id) {
		return fmt.Errorf("load balancer rule %v is being changed by another reconcile: %w", lbRule.Name, errJobPending)
	}
	defer lb.pendingJobs.release(lbRule.Id)

	p := lb.LoadBalancer.NewListLoadBalancerRuleInstancesParams(lbRule.Id)

	l, err := lb.LoadBalancer.ListLoadBalancerRuleInstances(p)
	if err != nil {
		return fmt.Errorf("error retrieving associated instances: %w", err)
	}

	assign, remove := symmetricDifference(hostIDs, l.LoadBalancerRuleInstances)

	klog.V(4).Infof("Reconcile hosts for rule %v: %d host(s) to assign, %d host(s) to remove (wanted: %v, current: %d instances)",
		lbRule.Name, len(assign), len(remove), hostIDs, len(l.LoadBalancerRuleInstances))

	if len(assign) > 0 {
		klog.V(4).Infof("Assigning new hosts (%v) to load balancer rule: %v", assign, lbRule.Name)
		if err := lb.assignHostsToRule(lbRule, assign); err != nil {
			return fmt.Errorf("error assigning new hosts to rule %v (old hosts preserved): %w", lbRule.Name, err)
		}
	}

	if len(remove) > 0 {
		klog.V(4).Infof("Removing old hosts (%v) from load balancer rule: %v", remove, lbRule.Name)
		if err := lb.removeHostsFromRule(lbRule, remove); err != nil {
			return err
		}
	}

	return nil
}

// assignHostsToRule assigns hosts to a load balancer rule.
func (lb *loadBalancer) assignHostsToRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
	p := lb.LoadBalancer.NewAssignToLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if r, err := lb.LoadBalancer.AssignToLoadBalancerRule(p); err != nil {
		var jobID string
		if r != nil {
			jobID = r.JobID
		}
		if perr := lb.trackTimedOutJob(lbRule, "assign", jobID, hostIDs, err); perr != nil {
			return perr
		}

		return fmt.Errorf("error assigning hosts to load balancer rule %v: %w", lbRule.Name, err)
	}

	return nil
}

// removeHostsFromRule removes hosts from a load balancer rule.
func (lb *loadBalancer) removeHostsFromRule(lbRule *cloudstack.LoadBalancerRule, hostIDs []string) error {
	p := lb.LoadBalancer.NewRemoveFromLoadBalancerRuleParams(lbRule.Id)
	p.SetVirtualmachineids(hostIDs)

	if r, err := lb.LoadBalancer.RemoveFromLoadBalancerRule(p); err != nil {
		var jobID string
		if r != nil {
			jobID = r.JobID
		}
		if perr := lb.trackTimedOutJob(lbRule, "remove", jobID, hostIDs, err); perr != nil {
			return perr
		}

		return fmt.Errorf("error removing hosts from load balancer rule %v: %w", lbRule.Name, err)
	}

	return nil
}

// generateLoadBalancerStatus returns the LoadBalancerStatus based on various service annotations.
func (lb *loadBalancer) generateLoadBalancerStatus(service *corev1.Service) *corev1.LoadBalancerStatus {
	status := &corev1.LoadBalancerStatus{}
	// If hostname is explicitly set using service annotation
	// Workaround for https://github.com/kubernetes/kubernetes/issues/66607
	if hostname := getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerLoadbalancerHostname, ""); hostname != "" {
		status.Ingress = []corev1.LoadBalancerIngress{{Hostname: hostname}}

		return status
	}

	ipMode := corev1.LoadBalancerIPModeVIP
	if getBoolFromServiceAnnotation(service, ServiceAnnotationLoadBalancerProxyProtocol, false) {
		// Set the LoadBalancerIPMode to Proxy to prevent kube-proxy from injecting an iptables bypass.
		// https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1860-kube-proxy-IP-node-binding
		ipMode = corev1.LoadBalancerIPModeProxy
	}
	// Default to IP
	status.Ingress = []corev1.LoadBalancerIngress{{
		IP:     lb.ipAddr,
		IPMode: &ipMode,
	}}

	return status
}

// symmetricDifference returns the symmetric difference between the old (existing) and new (wanted) host ID's.
func symmetricDifference(hostIDs []string, lbInstances []*cloudstack.VirtualMachine) ([]string, []string) {
	newIDs := make(map[string]bool)
	for _, hostID := range hostIDs {
		newIDs[hostID] = true
	}

	var remove []string //nolint:prealloc
	for _, instance := range lbInstances {
		if instance == nil {
			continue
		}

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
	rules, ok := lb.firewallRules[publicIPID]
	if !ok {
		var err error
		if rules, err = lb.listFirewallRules(publicIPID); err != nil {
			return false, err
		}
		if lb.firewallRules != nil {
			lb.firewallRules[publicIPID] = rules
		}
	}

	return lb.reconcileFirewallRule(rules, publicIPID, publicPort, protocol, allowedCIDRs)
}

// listFirewallRules returns the firewall rules of a public IP.
func (lb *loadBalancer) listFirewallRules(publicIPID string) ([]*cloudstack.FirewallRule, error) {
	p := lb.Firewall.NewListFirewallRulesParams()
	p.SetIpaddressid(publicIPID)
	p.SetListall(true)
	if lb.projectID != "" {
		p.SetProjectid(lb.projectID)
	}
	r, err := lb.Firewall.ListFirewallRules(p)
	if err != nil {
		return nil, fmt.Errorf("error fetching firewall rules for public IP %v: %w", publicIPID, err)
	}
	klog.V(4).Infof("Existing firewall rules for %v: %v", lb.ipAddr, rulesToString(r.FirewallRules))

	return r.FirewallRules, nil
}

// reconcileFirewallRule creates or updates the firewall rule for a protocol and port, using the given existing
// firewall rules of the public IP.
//
// Returns true if the firewall rule was created or updated.
func (lb *loadBalancer) reconcileFirewallRule(existing []*cloudstack.FirewallRule, publicIPID string, publicPort int, protocol LoadBalancerProtocol, allowedCIDRs []string) (bool, error) {
	var err error

	// Default to allow-all if no allowed CIDRs are defined.
	if len(allowedCIDRs) == 0 {
		allowedCIDRs = []string{defaultAllowedCIDR}
	}

	// find all rules that have a matching proto+port
	// a map may or may not be faster, but is a bit easier to understand
	filtered := make(map[*cloudstack.FirewallRule]bool)
	for _, rule := range existing {
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
	var deleteErr error
	for rule := range filtered {
		p := lb.Firewall.NewDeleteFirewallRuleParams(rule.Id)
		if _, err = lb.Firewall.DeleteFirewallRule(p); err != nil {
			// report the error, but keep on deleting the other rules
			klog.Errorf("Error deleting old firewall rule %v: %v", rule.Id, err)
			deleteErr = err
		}
	}

	// create new rule if necessary
	if match == nil {
		// no rule found, create a new one
		p := lb.Firewall.NewCreateFirewallRuleParams(publicIPID, protocol.IPProtocol())
		p.SetCidrlist(allowedCIDRs)
		p.SetStartport(publicPort)
		p.SetEndport(publicPort)
		if _, err = lb.Firewall.CreateFirewallRule(p); err != nil {
			// return immediately if we can't create the new rule
			return false, fmt.Errorf("error creating new firewall rule for public IP %v, proto %v, port %v, allowed %v: %w", publicIPID, protocol, publicPort, allowedCIDRs, err)
		}
	}

	changed := match == nil || len(filtered) > 0

	return changed, deleteErr
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
	var errs error
	deleted := false
	for _, rule := range filtered {
		p := lb.Firewall.NewDeleteFirewallRuleParams(rule.Id)
		_, err = lb.Firewall.DeleteFirewallRule(p)
		if err != nil {
			klog.Errorf("Error deleting old firewall rule %v: %v", rule.Id, err)
			errs = errors.Join(errs, fmt.Errorf("error deleting old firewall rule %v: %w", rule.Id, err))
		} else {
			deleted = true
		}
	}

	return deleted, errs
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

// getLoadBalancerAddress returns the desired load balancer IP address.
// It checks the ServiceAnnotationLoadBalancerAddress annotation first (preferred),
// then falls back to the deprecated spec.LoadBalancerIP field.
func getLoadBalancerAddress(service *corev1.Service) string {
	if service == nil {
		return ""
	}
	if addr := getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress, ""); addr != "" {
		return addr
	}

	return service.Spec.LoadBalancerIP //nolint:staticcheck // deprecated but kept as fallback
}

// getLoadBalancerID returns the stored load balancer public IP UUID from the service annotation.
func getLoadBalancerID(service *corev1.Service) string {
	return getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerID, "")
}

// getLoadBalancerNetworkID returns the stored load balancer network UUID from the service annotation.
func getLoadBalancerNetworkID(service *corev1.Service) string {
	return getStringFromServiceAnnotation(service, ServiceAnnotationLoadBalancerNetworkID, "")
}

// setServiceAnnotation is used to create/set or update an annotation on the Service object.
func setServiceAnnotation(service *corev1.Service, key, value string) {
	if service.Annotations == nil {
		service.Annotations = map[string]string{}
	}
	service.Annotations[key] = value
}

// deleteServiceAnnotation removes an annotation from the Service object.
func deleteServiceAnnotation(service *corev1.Service, key string) {
	if service.Annotations == nil {
		return
	}
	delete(service.Annotations, key)
}

// deleteLoadBalancerAnnotations removes all CloudStack load balancer annotations from the service.
func deleteLoadBalancerAnnotations(service *corev1.Service) {
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerProxyProtocol)
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerLoadbalancerHostname)
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerAddress)
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerKeepIP)
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerID)
	deleteServiceAnnotation(service, ServiceAnnotationLoadBalancerNetworkID)
}
