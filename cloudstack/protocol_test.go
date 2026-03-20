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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLoadBalancerProtocol_String(t *testing.T) {
	tests := []struct {
		name     string
		protocol LoadBalancerProtocol
		want     string
	}{
		{"TCP", LoadBalancerProtocolTCP, "tcp"},
		{"UDP", LoadBalancerProtocolUDP, "udp"},
		{"TCPProxy", LoadBalancerProtocolTCPProxy, "tcp-proxy"},
		{"Invalid", LoadBalancerProtocolInvalid, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.protocol.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadBalancerProtocol_CSProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol LoadBalancerProtocol
		want     string
	}{
		{"TCP", LoadBalancerProtocolTCP, "tcp"},
		{"UDP", LoadBalancerProtocolUDP, "udp"},
		{"TCPProxy", LoadBalancerProtocolTCPProxy, "tcp-proxy"},
		{"Invalid", LoadBalancerProtocolInvalid, ""},
		{"Unknown value", LoadBalancerProtocol(99), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.protocol.CSProtocol(); got != tt.want {
				t.Errorf("CSProtocol() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadBalancerProtocol_IPProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol LoadBalancerProtocol
		want     string
	}{
		{"TCP", LoadBalancerProtocolTCP, "tcp"},
		{"UDP", LoadBalancerProtocolUDP, "udp"},
		{"TCPProxy maps to tcp", LoadBalancerProtocolTCPProxy, "tcp"},
		{"Invalid", LoadBalancerProtocolInvalid, ""},
		{"Unknown value", LoadBalancerProtocol(99), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.protocol.IPProtocol(); got != tt.want {
				t.Errorf("IPProtocol() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProtocolFromServicePort(t *testing.T) {
	tests := []struct {
		name        string
		port        corev1.ServicePort
		annotations map[string]string
		want        LoadBalancerProtocol
	}{
		{
			name: "TCP without proxy",
			port: corev1.ServicePort{Protocol: corev1.ProtocolTCP},
			want: LoadBalancerProtocolTCP,
		},
		{
			name: "TCP with proxy annotation",
			port: corev1.ServicePort{Protocol: corev1.ProtocolTCP},
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerProxyProtocol: "true",
			},
			want: LoadBalancerProtocolTCPProxy,
		},
		{
			name: "TCP with proxy annotation false",
			port: corev1.ServicePort{Protocol: corev1.ProtocolTCP},
			annotations: map[string]string{
				ServiceAnnotationLoadBalancerProxyProtocol: "false",
			},
			want: LoadBalancerProtocolTCP,
		},
		{
			name: "UDP",
			port: corev1.ServicePort{Protocol: corev1.ProtocolUDP},
			want: LoadBalancerProtocolUDP,
		},
		{
			name: "SCTP is invalid",
			port: corev1.ServicePort{Protocol: corev1.ProtocolSCTP},
			want: LoadBalancerProtocolInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-svc",
					Annotations: tt.annotations,
				},
			}
			if got := ProtocolFromServicePort(tt.port, svc); got != tt.want {
				t.Errorf("ProtocolFromServicePort() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProtocolFromLoadBalancer(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		want     LoadBalancerProtocol
	}{
		{"empty string defaults to TCP", "", LoadBalancerProtocolTCP},
		{"tcp", "tcp", LoadBalancerProtocolTCP},
		{"udp", "udp", LoadBalancerProtocolUDP},
		{"tcp-proxy", "tcp-proxy", LoadBalancerProtocolTCPProxy},
		{"unknown protocol", "sctp", LoadBalancerProtocolInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProtocolFromLoadBalancer(tt.protocol); got != tt.want {
				t.Errorf("ProtocolFromLoadBalancer(%q) = %v, want %v", tt.protocol, got, tt.want)
			}
		})
	}
}
