// Copyright 2023 OnMetal authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package onmetal

const (
	// InternalLoadBalancerAnnotation is internal load balancer annotation of service
	InternalLoadBalancerAnnotation = "service.beta.kubernetes.io/onmetal-load-balancer-internal"
	// AnnotationKeyClusterName is the cluster name annotation key name
	AnnotationKeyClusterName = "cluster-name"
	// AnnotationKeyServiceName is the service name annotation key name
	AnnotationKeyServiceName = "service-name"
	// AnnotationKeyServiceNamespace is the service namespace annotation key name
	AnnotationKeyServiceNamespace = "service-namespace"
	// AnnotationKeyServiceUID is the service UID annotation key name
	AnnotationKeyServiceUID = "service-uid"
	// LabeKeylClusterName is the cluster name label key name
	LabeKeylClusterName = "kubernetes.io/cluster"
)
