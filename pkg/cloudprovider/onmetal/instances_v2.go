// Copyright 2022 OnMetal authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package onmetal

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/onmetal/onmetal-api/api/compute/v1alpha1"
	networkingv1alpha1 "github.com/onmetal/onmetal-api/api/networking/v1alpha1"
)

type onmetalInstancesV2 struct {
	targetClient     client.Client
	onmetalClient    client.Client
	onmetalNamespace string
	clusterName      string
}

func newOnmetalInstancesV2(targetClient client.Client, onmetalClient client.Client, namespace, clusterName string) cloudprovider.InstancesV2 {
	return &onmetalInstancesV2{
		targetClient:     targetClient,
		onmetalClient:    onmetalClient,
		onmetalNamespace: namespace,
		clusterName:      clusterName,
	}
}

func (o *onmetalInstancesV2) InstanceExists(ctx context.Context, node *corev1.Node) (bool, error) {
	if node == nil {
		return false, nil
	}
	klog.V(4).InfoS("Checking if node exists", "Node", node.Name)

	machine := &computev1alpha1.Machine{}
	if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: node.Name}, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, cloudprovider.InstanceNotFound
		}
		return false, fmt.Errorf("failed to get machine object for node %s: %w", node.Name, err)
	}

	klog.V(4).InfoS("Instance for node exists", "Node", node.Name, "Machine", client.ObjectKeyFromObject(machine))
	return true, nil
}

func (o *onmetalInstancesV2) InstanceShutdown(ctx context.Context, node *corev1.Node) (bool, error) {
	if node == nil {
		return false, nil
	}
	klog.V(4).InfoS("Checking if instance is shut down", "Node", node.Name)

	machine := &computev1alpha1.Machine{}
	if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: node.Name}, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, cloudprovider.InstanceNotFound
		}
		return false, fmt.Errorf("failed to get machine object for node %s: %w", node.Name, err)
	}

	nodeShutDownStatus := machine.Status.State == computev1alpha1.MachineStateShutdown
	klog.V(4).InfoS("Instance shut down status", "NodeShutdown", nodeShutDownStatus)
	return nodeShutDownStatus, nil
}

func (o *onmetalInstancesV2) InstanceMetadata(ctx context.Context, node *corev1.Node) (*cloudprovider.InstanceMetadata, error) {
	if node == nil {
		return nil, nil
	}
	machine := &computev1alpha1.Machine{}
	if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: node.Name}, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloudprovider.InstanceNotFound
		}
		return nil, fmt.Errorf("failed to get machine object for node %s: %w", node.Name, err)
	}

	//add label for clusterName to machine object
	machineBase := machine.DeepCopy()
	if machine.Labels == nil {
		machine.Labels = make(map[string]string)
	}
	machine.Labels[LabeKeylClusterName] = o.clusterName
	klog.V(2).InfoS("Adding cluster name label to Machine object", "Machine", client.ObjectKeyFromObject(machine), "Node", node.Name)
	if err := o.onmetalClient.Patch(ctx, machine, client.MergeFrom(machineBase)); err != nil {
		return nil, fmt.Errorf("failed to patch Machine %s for Node %s: %w", client.ObjectKeyFromObject(machine), node.Name, err)
	}

	for _, networkInterface := range machine.Spec.NetworkInterfaces {
		nic := &networkingv1alpha1.NetworkInterface{}
		nicName := fmt.Sprintf("%s-%s", machine.Name, networkInterface.Name)
		if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: nicName}, nic); err != nil {
			return nil, fmt.Errorf("failed to get network interface %s for machine %s: %w", client.ObjectKeyFromObject(nic), machine.Name, err)
		}

		// add label for clusterName to network interface of machine object
		nicBase := nic.DeepCopy()
		if nic.Labels == nil {
			nic.Labels = make(map[string]string)
		}
		nic.Labels[LabeKeylClusterName] = o.clusterName
		klog.V(2).InfoS("Adding cluster name label to NetworkInterface", "NetworkInterface", client.ObjectKeyFromObject(nic), "Node", node.Name, "Label", nic.Labels[LabeKeylClusterName])
		if err := o.onmetalClient.Patch(ctx, nic, client.MergeFrom(nicBase)); err != nil {
			return nil, fmt.Errorf("failed to patch NetworkInterface %s for Node %s: %w", client.ObjectKeyFromObject(nic), node.Name, err)
		}
	}

	addresses := make([]corev1.NodeAddress, 0)
	for _, iface := range machine.Status.NetworkInterfaces {
		if iface.VirtualIP != nil {
			addresses = append(addresses, corev1.NodeAddress{
				Type:    corev1.NodeExternalIP,
				Address: iface.VirtualIP.String(),
			})
		}
		for _, ip := range iface.IPs {
			addresses = append(addresses, corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: ip.String(),
			})
		}
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		providerID = fmt.Sprintf("%s://%s/%s", ProviderName, o.onmetalNamespace, machine.Name)
	}

	zone := ""
	if machine.Spec.MachinePoolRef != nil {
		zone = machine.Spec.MachinePoolRef.Name
	}

	// TODO: handle region
	return &cloudprovider.InstanceMetadata{
		ProviderID:    providerID,
		InstanceType:  machine.Spec.MachineClassRef.Name,
		NodeAddresses: addresses,
		Zone:          zone,
		Region:        "",
	}, nil
}
