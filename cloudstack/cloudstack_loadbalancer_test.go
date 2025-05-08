package cloudstack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_generateLoadBalancerStatus(t *testing.T) {
	ipmodeProxy := corev1.LoadBalancerIPModeProxy
	ipmodeVIP := corev1.LoadBalancerIPModeVIP
	type args struct {
		service *corev1.Service
		addr    string
	}
	type result struct {
		HostName  string
		IPAddress string
		IPMode    *corev1.LoadBalancerIPMode
	}
	tests := []struct {
		name string
		args args
		want result
	}{
		{
			name: "It should return hostname from service annotation",
			args: args{
				service: &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{"service.beta.kubernetes.io/cloudstack-load-balancer-hostname": "testor"},
					},
				},
				addr: "172.17.0.2",
			},
			want: result{
				HostName: "testor",
			},
		},
		{
			name: "it should default to ip address if no hostname can be found from svc or proxyProtocol",
			args: args{
				service: &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{},
				},
				addr: "172.17.0.2",
			},
			want: result{
				IPAddress: "172.17.0.2",
				IPMode:    &ipmodeVIP,
			},
		},
		{
			name: "it should return ipMode proxy if using proxyProtocol and not EnableIngressHostname",
			args: args{
				service: &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{"service.beta.kubernetes.io/cloudstack-load-balancer-proxy-protocol": "true"},
					},
				},
				addr: "172.17.0.2",
			},
			want: result{
				IPAddress: "172.17.0.2",
				IPMode:    &ipmodeProxy,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lb := &loadBalancer{
				ipAddr: tt.args.addr,
			}

			result := lb.generateLoadBalancerStatus(tt.args.service)
			assert.Equal(t, tt.want.HostName, result.Ingress[0].Hostname)
			assert.Equal(t, tt.want.IPAddress, result.Ingress[0].IP)
			assert.Equal(t, tt.want.IPMode, result.Ingress[0].IPMode)
		})
	}
}
