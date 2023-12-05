/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/network"

	"knative.dev/eventing-kafka-broker/control-plane/pkg/apis/messaging/v1beta1"
)

const (
	portName   = "http"
	portNumber = 80

	tlsPortName   = "https"
	tlsPortNumber = 443

	MessagingRoleLabel = "messaging.knative.dev/role"
	MessagingRole      = "kafka-channel"

	ChannelSuffix = "-kn-channel"
	// MaxResourceNameLength is the maximum number of characters for a Kubernetes resource name.
	// See vendor/k8s.io/apiserver/pkg/storage/names/generate.go
	MaxResourceNameLength = 63
)

// ServiceOption can be used to optionally modify the K8s service in MakeK8sService.
type ServiceOption func(*corev1.Service) error

func MakeChannelServiceName(name string) string {
	if len(name)+len(ChannelSuffix) > MaxResourceNameLength {
		return name
	}
	return fmt.Sprintf("%s%s", name, ChannelSuffix)
}

// ExternalService is a functional option for MakeK8sService to create a K8s service of type ExternalName
// pointing to the specified service in a namespace.
func ExternalService(namespace, service string) ServiceOption {
	return func(svc *corev1.Service) error {
		svc.Spec.Type = corev1.ServiceTypeExternalName
		svc.Spec.ExternalName = network.GetServiceHostname(service, namespace)
		return nil
	}
}

// MakeK8sService creates a new K8s Service for a Channel resource. It also sets the appropriate
// OwnerReferences on the resource so handleObject can discover the Channel resource that 'owns' it.
// As well as being garbage collected when the Channel is deleted.
func MakeK8sService(kc *v1beta1.KafkaChannel, opts ...ServiceOption) (*corev1.Service, error) {
	// Add annotations
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MakeChannelServiceName(kc.ObjectMeta.Name),
			Namespace: kc.Namespace,
			Labels: map[string]string{
				MessagingRoleLabel: MessagingRole,
			},
			OwnerReferences: []metav1.OwnerReference{
				*kmeta.NewControllerRef(kc),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     portName,
					Protocol: corev1.ProtocolTCP,
					Port:     portNumber,
				},
				{
					Name:     tlsPortName,
					Protocol: corev1.ProtocolTCP,
					Port:     tlsPortNumber,
				},
			},
		},
	}
	for _, opt := range opts {
		if err := opt(svc); err != nil {
			return nil, err
		}
	}
	return svc, nil
}
