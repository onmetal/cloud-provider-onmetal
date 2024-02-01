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
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	commonv1alpha1 "github.com/onmetal/onmetal-api/api/common/v1alpha1"
	computev1alpha1 "github.com/onmetal/onmetal-api/api/compute/v1alpha1"
	"github.com/onmetal/onmetal-api/api/ipam/v1alpha1"
	networkingv1alpha1 "github.com/onmetal/onmetal-api/api/networking/v1alpha1"
)

const (
	waitLoadbalancerInitDelay   = 1 * time.Second
	waitLoadbalancerFactor      = 1.2
	waitLoadbalancerActiveSteps = 19
)

var (
	loadBalancerFieldOwner = client.FieldOwner("cloud-provider.onmetal.de/loadbalancer")
)

type onmetalLoadBalancer struct {
	targetClient     client.Client
	onmetalClient    client.Client
	onmetalNamespace string
	cloudConfig      CloudConfig
}

func newOnmetalLoadBalancer(targetClient client.Client, onmetalClient client.Client, namespace string, cloudConfig CloudConfig) cloudprovider.LoadBalancer {
	return &onmetalLoadBalancer{
		targetClient:     targetClient,
		onmetalClient:    onmetalClient,
		onmetalNamespace: namespace,
		cloudConfig:      cloudConfig,
	}
}

func (o *onmetalLoadBalancer) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	klog.V(2).InfoS("GetLoadBalancer for Service", "Cluster", clusterName, "Service", client.ObjectKeyFromObject(service))

	loadBalancer := &networkingv1alpha1.LoadBalancer{}
	loadBalancerName := o.GetLoadBalancerName(ctx, clusterName, service)
	if err = o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: loadBalancerName}, loadBalancer); err != nil {
		return nil, false, fmt.Errorf("failed to get LoadBalancer %s for Service %s: %w", loadBalancerName, client.ObjectKeyFromObject(service), err)
	}

	lbAllocatedIps := loadBalancer.Status.IPs
	status = &v1.LoadBalancerStatus{}
	for _, ip := range lbAllocatedIps {
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: ip.String()})
	}
	return status, true, nil
}

func (o *onmetalLoadBalancer) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	cloudprovider.DefaultLoadBalancerName(service)
	return getLoadBalancerNameForService(clusterName, service)
}

func (o *onmetalLoadBalancer) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	klog.V(2).InfoS("EnsureLoadBalancer for Service", "Cluster", clusterName, "Service", client.ObjectKeyFromObject(service))

	// decide load balancer type based on service annotation for internal load balancer
	var desiredLoadBalancerType networkingv1alpha1.LoadBalancerType
	if value, ok := service.Annotations[InternalLoadBalancerAnnotation]; ok && value == "true" {
		desiredLoadBalancerType = networkingv1alpha1.LoadBalancerTypeInternal
	} else {
		desiredLoadBalancerType = networkingv1alpha1.LoadBalancerTypePublic
	}

	loadBalancerName := getLoadBalancerNameForService(clusterName, service)

	// get existing load balancer type
	existingLoadBalancer := &networkingv1alpha1.LoadBalancer{}
	var existingLoadBalancerType networkingv1alpha1.LoadBalancerType
	if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: loadBalancerName}, existingLoadBalancer); err == nil {
		existingLoadBalancerType = existingLoadBalancer.Spec.Type
		if existingLoadBalancerType != desiredLoadBalancerType {
			if err = o.EnsureLoadBalancerDeleted(ctx, clusterName, service); err != nil {
				return nil, fmt.Errorf("failed deleting existing loadbalancer %s: %w", loadBalancerName, err)
			}
		}
	}

	klog.V(2).InfoS("Getting LoadBalancer ports from Service", "Service", client.ObjectKeyFromObject(service))
	var lbPorts []networkingv1alpha1.LoadBalancerPort
	for _, svcPort := range service.Spec.Ports {
		protocol := svcPort.Protocol
		lbPorts = append(lbPorts, networkingv1alpha1.LoadBalancerPort{
			Protocol: &protocol,
			Port:     svcPort.Port,
		})
	}

	loadBalancer := &networkingv1alpha1.LoadBalancer{
		TypeMeta: metav1.TypeMeta{
			Kind:       "LoadBalancer",
			APIVersion: networkingv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      loadBalancerName,
			Namespace: o.onmetalNamespace,
			Annotations: map[string]string{
				AnnotationKeyClusterName:      clusterName,
				AnnotationKeyServiceName:      service.Name,
				AnnotationKeyServiceNamespace: service.Namespace,
				AnnotationKeyServiceUID:       string(service.UID),
			},
		},
		Spec: networkingv1alpha1.LoadBalancerSpec{
			Type:       desiredLoadBalancerType,
			IPFamilies: service.Spec.IPFamilies,
			NetworkRef: v1.LocalObjectReference{
				Name: o.cloudConfig.NetworkName,
			},
			Ports: lbPorts,
		},
	}

	// if load balancer type is Internal then update IPSource with valid prefix template
	if desiredLoadBalancerType == networkingv1alpha1.LoadBalancerTypeInternal {
		if o.cloudConfig.PrefixName == "" {
			return nil, fmt.Errorf("prefixName is not defined in config")
		}
		loadBalancer.Spec.IPs = []networkingv1alpha1.IPSource{
			{
				Ephemeral: &networkingv1alpha1.EphemeralPrefixSource{
					PrefixTemplate: &v1alpha1.PrefixTemplateSpec{
						Spec: v1alpha1.PrefixSpec{
							// TODO: for now we only support IPv4 until Gardener has support for IPv6 based Shoots
							IPFamily: v1.IPv4Protocol,
							ParentRef: &v1.LocalObjectReference{
								Name: o.cloudConfig.PrefixName,
							},
						},
					},
				},
			},
		}
	}

	klog.V(2).InfoS("Applying LoadBalancer for Service", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer), "Service", client.ObjectKeyFromObject(service))
	if err := o.onmetalClient.Patch(ctx, loadBalancer, client.Apply, loadBalancerFieldOwner, client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("failed to apply LoadBalancer %s for Service %s: %w", client.ObjectKeyFromObject(loadBalancer), client.ObjectKeyFromObject(service), err)
	}
	klog.V(2).InfoS("Applied LoadBalancer for Service", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer), "Service", client.ObjectKeyFromObject(service))

	klog.V(2).InfoS("Applying LoadBalancerRouting for LoadBalancer", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	if err := o.applyLoadBalancerRoutingForLoadBalancer(ctx, loadBalancer, nodes); err != nil {
		return nil, err
	}
	klog.V(2).InfoS("Applied LoadBalancerRouting for LoadBalancer", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))

	lbStatus, err := waitLoadBalancerActive(ctx, o.onmetalClient, existingLoadBalancerType, service, loadBalancer)
	if err != nil {
		return nil, err
	}
	return &lbStatus, nil
}

func getLoadBalancerNameForService(clusterName string, service *v1.Service) string {
	nameSuffix := strings.Split(string(service.UID), "-")[0]
	return fmt.Sprintf("%s-%s-%s", clusterName, service.Name, nameSuffix)
}

func waitLoadBalancerActive(ctx context.Context, onmetalClient client.Client, existingLoadBalancerType networkingv1alpha1.LoadBalancerType,
	service *v1.Service, loadBalancer *networkingv1alpha1.LoadBalancer) (v1.LoadBalancerStatus, error) {
	klog.V(2).InfoS("Waiting for LoadBalancer instance to become ready", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	backoff := wait.Backoff{
		Duration: waitLoadbalancerInitDelay,
		Factor:   waitLoadbalancerFactor,
		Steps:    waitLoadbalancerActiveSteps,
	}

	loadBalancerStatus := v1.LoadBalancerStatus{}
	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if err := onmetalClient.Get(ctx, client.ObjectKey{Namespace: loadBalancer.Namespace, Name: loadBalancer.Name}, loadBalancer); err != nil {
			return false, err
		}
		if len(loadBalancer.Status.IPs) == 0 {
			return false, nil
		}
		lbIngress := []v1.LoadBalancerIngress{}
		for _, ipAddr := range loadBalancer.Status.IPs {
			lbIngress = append(lbIngress, v1.LoadBalancerIngress{IP: ipAddr.String()})
		}
		loadBalancerStatus.Ingress = lbIngress

		if loadBalancer.Spec.Type != existingLoadBalancerType && servicehelper.LoadBalancerStatusEqual(&service.Status.LoadBalancer, &loadBalancerStatus) {
			return false, nil
		}
		return true, nil
	}); wait.Interrupted(err) {
		return loadBalancerStatus, fmt.Errorf("timeout waiting for the LoadBalancer %s to become ready", client.ObjectKeyFromObject(loadBalancer))
	}

	// workaround for refresh issues on the machinepoollet
	lbr := &networkingv1alpha1.LoadBalancerRouting{}
	if err := onmetalClient.Get(ctx, client.ObjectKey{Namespace: loadBalancer.Namespace, Name: loadBalancer.Name}, lbr); err != nil {
		return loadBalancerStatus, err
	}

	if len(lbr.Labels) == 0 {
		lbr.Labels = make(map[string]string)
	}
	formattedTime := strings.ReplaceAll(time.Now().Format(time.RFC3339), ":", "-")
	formattedTime = strings.TrimSuffix(formattedTime, "Z") // If you want to remove 'Z' at the end
	lbr.Labels["updated"] = formattedTime
	if err := onmetalClient.Update(ctx, lbr); err != nil {
		return loadBalancerStatus, err
	}

	klog.V(2).InfoS("LoadBalancer became ready", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	return loadBalancerStatus, nil
}

func (o *onmetalLoadBalancer) applyLoadBalancerRoutingForLoadBalancer(ctx context.Context, loadBalancer *networkingv1alpha1.LoadBalancer, nodes []*v1.Node) error {
	networkInterfaces, err := o.getNetworkInterfacesForNodes(ctx, nodes, loadBalancer.Spec.NetworkRef.Name)
	if err != nil {
		return fmt.Errorf("failed to get NetworkInterfaces for Nodes: %w", err)
	}

	sort.Slice(networkInterfaces, func(i, j int) bool {
		return networkInterfaces[i].UID < networkInterfaces[j].UID
	})

	network := &networkingv1alpha1.Network{}
	networkKey := client.ObjectKey{Namespace: o.onmetalNamespace, Name: loadBalancer.Spec.NetworkRef.Name}
	if err := o.onmetalClient.Get(ctx, networkKey, network); err != nil {
		return fmt.Errorf("failed to get Network %s: %w", o.cloudConfig.NetworkName, err)
	}

	loadBalancerRouting := &networkingv1alpha1.LoadBalancerRouting{
		TypeMeta: metav1.TypeMeta{
			Kind:       "LoadBalancerRouting",
			APIVersion: networkingv1alpha1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      loadBalancer.Name,
			Namespace: o.onmetalNamespace,
		},
		NetworkRef: commonv1alpha1.LocalUIDReference{
			Name: network.Name,
			UID:  network.UID,
		},
		Destinations: networkInterfaces,
	}

	if err := controllerutil.SetOwnerReference(loadBalancer, loadBalancerRouting, o.onmetalClient.Scheme()); err != nil {
		return fmt.Errorf("failed to set owner reference for load balancer routing %s: %w", client.ObjectKeyFromObject(loadBalancerRouting), err)
	}

	if err := o.onmetalClient.Patch(ctx, loadBalancerRouting, client.Apply, loadBalancerFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply LoadBalancerRouting %s for LoadBalancer %s: %w", client.ObjectKeyFromObject(loadBalancerRouting), client.ObjectKeyFromObject(loadBalancer), err)
	}
	return nil
}

func (o *onmetalLoadBalancer) getNetworkInterfacesForNodes(ctx context.Context, nodes []*v1.Node, networkName string) ([]commonv1alpha1.LocalUIDReference, error) {
	var networkInterfaces []commonv1alpha1.LocalUIDReference
	for _, node := range nodes {
		machineName := extractMachineNameFromProviderID(node.Spec.ProviderID)
		machine := &computev1alpha1.Machine{}
		if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: machineName}, machine); client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("failed to get machine object for node %s: %w", node.Name, err)
		}

		for _, machineNIC := range machine.Spec.NetworkInterfaces {
			networkInterface := &networkingv1alpha1.NetworkInterface{}

			networkInterfaceName := fmt.Sprintf("%s-%s", machine.Name, machineNIC.Name)

			if machineNIC.NetworkInterfaceRef != nil {
				networkInterfaceName = machineNIC.NetworkInterfaceRef.Name
			}

			if err := o.onmetalClient.Get(ctx, client.ObjectKey{Namespace: o.onmetalNamespace, Name: networkInterfaceName}, networkInterface); err != nil {
				return nil, fmt.Errorf("failed to get network interface %s for machine %s: %w", client.ObjectKeyFromObject(networkInterface), client.ObjectKeyFromObject(machine), err)
			}

			if networkInterface.Spec.NetworkRef.Name == networkName {
				networkInterfaces = append(networkInterfaces, commonv1alpha1.LocalUIDReference{
					Name: networkInterface.Name,
					UID:  networkInterface.UID,
				})
			}
		}
	}
	return networkInterfaces, nil
}

func extractMachineNameFromProviderID(providerID string) string {
	lastSlash := strings.LastIndex(providerID, "/")
	if lastSlash == -1 || lastSlash+1 >= len(providerID) {
		return ""
	}
	return providerID[lastSlash+1:]
}

func (o *onmetalLoadBalancer) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(2).InfoS("Updating LoadBalancer for Service", "Service", client.ObjectKeyFromObject(service))
	if len(nodes) == 0 {
		return fmt.Errorf("no Nodes available for LoadBalancer Service %s", client.ObjectKeyFromObject(service))
	}

	loadBalancerName := o.GetLoadBalancerName(ctx, clusterName, service)
	loadBalancer := &networkingv1alpha1.LoadBalancer{}
	loadBalancerKey := client.ObjectKey{Namespace: o.onmetalNamespace, Name: loadBalancerName}
	if err := o.onmetalClient.Get(ctx, loadBalancerKey, loadBalancer); err != nil {
		return fmt.Errorf("failed to get LoadBalancer %s: %w", client.ObjectKeyFromObject(loadBalancer), err)
	}

	loadBalancerRouting := &networkingv1alpha1.LoadBalancerRouting{}
	loadBalancerRoutingKey := client.ObjectKey{Namespace: o.onmetalNamespace, Name: loadBalancerName}
	if err := o.onmetalClient.Get(ctx, loadBalancerRoutingKey, loadBalancerRouting); err != nil {
		return fmt.Errorf("failed to get LoadBalancerRouting %s for LoadBalancer %s: %w", client.ObjectKeyFromObject(loadBalancer), client.ObjectKeyFromObject(loadBalancerRouting), err)
	}

	klog.V(2).InfoS("Updating LoadBalancerRouting destinations for LoadBalancer", "LoadBalancerRouting", client.ObjectKeyFromObject(loadBalancerRouting), "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	networkInterfaces, err := o.getNetworkInterfacesForNodes(ctx, nodes, loadBalancer.Spec.NetworkRef.Name)
	if err != nil {
		return fmt.Errorf("failed to get NetworkInterfaces for LoadBalancer %s: %w", client.ObjectKeyFromObject(loadBalancer), err)
	}
	loadBalancerRoutingBase := loadBalancerRouting.DeepCopy()
	loadBalancerRouting.Destinations = networkInterfaces

	if err := o.onmetalClient.Patch(ctx, loadBalancerRouting, client.MergeFrom(loadBalancerRoutingBase)); err != nil {
		return fmt.Errorf("failed to patch LoadBalancerRouting %s for LoadBalancer %s: %w", client.ObjectKeyFromObject(loadBalancerRouting), client.ObjectKeyFromObject(loadBalancer), err)
	}

	klog.V(2).InfoS("Updated LoadBalancer for Service", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer), "Service", client.ObjectKeyFromObject(service))
	return nil
}

func (o *onmetalLoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	loadBalancerName := o.GetLoadBalancerName(ctx, clusterName, service)
	loadBalancer := &networkingv1alpha1.LoadBalancer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: o.onmetalNamespace,
			Name:      loadBalancerName,
		},
	}
	klog.V(2).InfoS("Deleting LoadBalancer", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	if err := o.onmetalClient.Delete(ctx, loadBalancer); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(2).InfoS("LoadBalancer is already gone", client.ObjectKeyFromObject(loadBalancer))
			return nil
		}
		return fmt.Errorf("failed to delete loadbalancer %s: %w", client.ObjectKeyFromObject(loadBalancer), err)
	}
	if err := waitForDeletingLoadBalancer(ctx, service, o.onmetalClient, loadBalancer); err != nil {
		return err
	}
	return nil
}

func waitForDeletingLoadBalancer(ctx context.Context, service *v1.Service, onmetalClient client.Client, loadBalancer *networkingv1alpha1.LoadBalancer) error {
	klog.V(2).InfoS("Waiting for LoadBalancer instance to be deleted", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	backoff := wait.Backoff{
		Duration: waitLoadbalancerInitDelay,
		Factor:   waitLoadbalancerFactor,
		Steps:    waitLoadbalancerActiveSteps,
	}

	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if err := onmetalClient.Get(ctx, client.ObjectKey{Namespace: loadBalancer.Namespace, Name: loadBalancer.Name}, loadBalancer); !apierrors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	}); wait.Interrupted(err) {
		return fmt.Errorf("timeout waiting for the LoadBalancer %s to be deleted", client.ObjectKeyFromObject(loadBalancer))
	}

	klog.V(2).InfoS("Deleted LoadBalancer", "LoadBalancer", client.ObjectKeyFromObject(loadBalancer))
	return nil
}
