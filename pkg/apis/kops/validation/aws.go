/*
Copyright 2017 The Kubernetes Authors.

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

package validation

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
)

func awsValidateCluster(c *kops.Cluster) field.ErrorList {
	allErrs := field.ErrorList{}

	if c.Spec.API != nil {
		if c.Spec.API.LoadBalancer != nil {
			allErrs = append(allErrs, awsValidateAdditionalSecurityGroups(field.NewPath("spec", "api", "loadBalancer", "additionalSecurityGroups"), c.Spec.API.LoadBalancer.AdditionalSecurityGroups)...)
			allErrs = append(allErrs, awsValidateSSLPolicy(field.NewPath("spec", "api", "loadBalancer", "sslPolicy"), c.Spec.API.LoadBalancer)...)
			allErrs = append(allErrs, awsValidateLoadBalancerSubnets(field.NewPath("spec", "api", "loadBalancer", "subnets"), c.Spec)...)
		}
	}

	allErrs = append(allErrs, awsValidateExternalCloudControllerManager(c.Spec)...)

	return allErrs
}

func awsValidateExternalCloudControllerManager(c kops.ClusterSpec) (allErrs field.ErrorList) {

	if c.ExternalCloudControllerManager != nil {
		if c.KubeControllerManager == nil || c.KubeControllerManager.ExternalCloudVolumePlugin != "aws" {
			if c.CloudConfig == nil || c.CloudConfig.AWSEBSCSIDriver == nil || !fi.BoolValue(c.CloudConfig.AWSEBSCSIDriver.Enabled) {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "externalCloudControllerManager"),
					"AWS external CCM cannot be used without enabling spec.cloudConfig.AWSEBSCSIDriver or setting spec.kubeControllerManaager.externalCloudVolumePlugin set to `aws`"))
			}
		}
	}
	return allErrs

}

func awsValidateInstanceGroup(ig *kops.InstanceGroup, cloud awsup.AWSCloud) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, awsValidateAdditionalSecurityGroups(field.NewPath("spec", "additionalSecurityGroups"), ig.Spec.AdditionalSecurityGroups)...)

	allErrs = append(allErrs, awsValidateInstanceType(field.NewPath(ig.GetName(), "spec", "machineType"), ig.Spec.MachineType, cloud)...)

	allErrs = append(allErrs, awsValidateSpotDurationInMinute(field.NewPath(ig.GetName(), "spec", "spotDurationInMinutes"), ig)...)

	allErrs = append(allErrs, awsValidateInstanceInterruptionBehavior(field.NewPath(ig.GetName(), "spec", "instanceInterruptionBehavior"), ig)...)

	if ig.Spec.MixedInstancesPolicy != nil {
		allErrs = append(allErrs, awsValidateMixedInstancesPolicy(field.NewPath("spec", "mixedInstancesPolicy"), ig.Spec.MixedInstancesPolicy, ig, cloud)...)
	}

	if ig.Spec.InstanceMetadata != nil {
		allErrs = append(allErrs, awsValidateInstanceMetadata(field.NewPath("spec", "instanceMetadata"), ig.Spec.InstanceMetadata)...)
	}

	return allErrs
}

func awsValidateInstanceMetadata(fieldPath *field.Path, instanceMetadata *kops.InstanceMetadataOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if instanceMetadata.HTTPTokens != nil {
		allErrs = append(allErrs, IsValidValue(fieldPath.Child("httpTokens"), instanceMetadata.HTTPTokens, []string{"optional", "required"})...)
	}

	if instanceMetadata.HTTPPutResponseHopLimit != nil {
		httpPutResponseHopLimit := fi.Int64Value(instanceMetadata.HTTPPutResponseHopLimit)
		if httpPutResponseHopLimit < 1 || httpPutResponseHopLimit > 64 {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("httpPutResponseHopLimit"), instanceMetadata.HTTPPutResponseHopLimit,
				"HTTPPutResponseLimit must be a value between 1 and 64"))
		}
	}

	return allErrs
}

func awsValidateAdditionalSecurityGroups(fieldPath *field.Path, groups []string) field.ErrorList {
	allErrs := field.ErrorList{}

	names := sets.NewString()
	for i, s := range groups {
		if names.Has(s) {
			allErrs = append(allErrs, field.Duplicate(fieldPath.Index(i), s))
		}
		names.Insert(s)
		if strings.TrimSpace(s) == "" {
			allErrs = append(allErrs, field.Invalid(fieldPath.Index(i), s, "security group cannot be empty, if specified"))
			continue
		}
		if !strings.HasPrefix(s, "sg-") {
			allErrs = append(allErrs, field.Invalid(fieldPath.Index(i), s, "security group does not match the expected AWS format"))
		}
	}

	return allErrs
}

func awsValidateInstanceType(fieldPath *field.Path, instanceType string, cloud awsup.AWSCloud) field.ErrorList {
	allErrs := field.ErrorList{}
	if instanceType != "" && cloud != nil {
		for _, typ := range strings.Split(instanceType, ",") {
			if _, err := cloud.DescribeInstanceType(typ); err != nil {
				allErrs = append(allErrs, field.Invalid(fieldPath, typ, "machine type specified is invalid"))
			}
		}
	}

	return allErrs
}

func awsValidateSpotDurationInMinute(fieldPath *field.Path, ig *kops.InstanceGroup) field.ErrorList {
	allErrs := field.ErrorList{}
	if ig.Spec.SpotDurationInMinutes != nil {
		validSpotDurations := []string{"60", "120", "180", "240", "300", "360"}
		spotDurationStr := strconv.FormatInt(*ig.Spec.SpotDurationInMinutes, 10)
		allErrs = append(allErrs, IsValidValue(fieldPath, &spotDurationStr, validSpotDurations)...)
	}
	return allErrs
}

func awsValidateInstanceInterruptionBehavior(fieldPath *field.Path, ig *kops.InstanceGroup) field.ErrorList {
	allErrs := field.ErrorList{}
	if ig.Spec.InstanceInterruptionBehavior != nil {
		instanceInterruptionBehavior := *ig.Spec.InstanceInterruptionBehavior
		allErrs = append(allErrs, IsValidValue(fieldPath, &instanceInterruptionBehavior, ec2.InstanceInterruptionBehavior_Values())...)
	}
	return allErrs
}

// awsValidateMixedInstancesPolicy is responsible for validating the user input of a mixed instance policy
func awsValidateMixedInstancesPolicy(path *field.Path, spec *kops.MixedInstancesPolicySpec, ig *kops.InstanceGroup, cloud awsup.AWSCloud) field.ErrorList {
	var errs field.ErrorList

	// @step: check the instance types are valid
	for i, x := range spec.Instances {
		errs = append(errs, awsValidateInstanceType(path.Child("instances").Index(i), x, cloud)...)
	}

	if spec.OnDemandBase != nil {
		if fi.Int64Value(spec.OnDemandBase) < 0 {
			errs = append(errs, field.Invalid(path.Child("onDemandBase"), spec.OnDemandBase, "cannot be less than zero"))
		}
		if fi.Int64Value(spec.OnDemandBase) > int64(fi.Int32Value(ig.Spec.MaxSize)) {
			errs = append(errs, field.Invalid(path.Child("onDemandBase"), spec.OnDemandBase, "cannot be greater than max size"))
		}
	}

	if spec.OnDemandAboveBase != nil {
		if fi.Int64Value(spec.OnDemandAboveBase) < 0 {
			errs = append(errs, field.Invalid(path.Child("onDemandAboveBase"), spec.OnDemandAboveBase, "cannot be less than 0"))
		}
		if fi.Int64Value(spec.OnDemandAboveBase) > 100 {
			errs = append(errs, field.Invalid(path.Child("onDemandAboveBase"), spec.OnDemandAboveBase, "cannot be greater than 100"))
		}
	}

	errs = append(errs, IsValidValue(path.Child("spotAllocationStrategy"), spec.SpotAllocationStrategy, kops.SpotAllocationStrategies)...)

	return errs
}

func awsValidateSSLPolicy(fieldPath *field.Path, spec *kops.LoadBalancerAccessSpec) field.ErrorList {
	allErrs := field.ErrorList{}

	if spec.SSLPolicy != nil {
		if spec.Class != kops.LoadBalancerClassNetwork {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "sslPolicy should be specified with Network Load Balancer"))
		}
		if spec.SSLCertificate == "" {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "sslPolicy should not be specified without SSLCertificate"))
		}
	}

	return allErrs
}

func awsValidateLoadBalancerSubnets(fieldPath *field.Path, spec kops.ClusterSpec) field.ErrorList {
	allErrs := field.ErrorList{}

	lbSpec := spec.API.LoadBalancer

	for i, subnet := range lbSpec.Subnets {
		var clusterSubnet *kops.ClusterSubnetSpec
		if subnet.Name == "" {
			allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("name"), "subnet name can't be empty"))
		} else {
			for _, cs := range spec.Subnets {
				if subnet.Name == cs.Name {
					clusterSubnet = &cs
					break
				}
			}
			if clusterSubnet == nil {
				allErrs = append(allErrs, field.NotFound(fieldPath.Index(i).Child("name"), fmt.Sprintf("subnet %q not found in cluster subnets", subnet.Name)))
			}
		}

		if subnet.PrivateIPv4Address != nil {
			if *subnet.PrivateIPv4Address == "" {
				allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("privateIPv4Address"), "privateIPv4Address can't be empty"))
			}
			ip := net.ParseIP(*subnet.PrivateIPv4Address)
			if ip == nil || ip.To4() == nil {
				allErrs = append(allErrs, field.Invalid(fieldPath.Index(i).Child("privateIPv4Address"), subnet, "privateIPv4Address is not a valid IPv4 address"))
			} else if clusterSubnet != nil {
				_, ipNet, err := net.ParseCIDR(clusterSubnet.CIDR)
				if err == nil { // we assume that the cidr is actually valid
					if !ipNet.Contains(ip) {
						allErrs = append(allErrs, field.Invalid(fieldPath.Index(i).Child("privateIPv4Address"), subnet, "privateIPv4Address is not part of the subnet CIDR"))
					}
				}

			}
			if lbSpec.Class != kops.LoadBalancerClassNetwork || lbSpec.Type != kops.LoadBalancerTypeInternal {
				allErrs = append(allErrs, field.Forbidden(fieldPath.Index(i).Child("privateIPv4Address"), "privateIPv4Address only allowed for internal NLBs"))
			}
		}

		if subnet.AllocationID != nil {
			if *subnet.AllocationID == "" {
				allErrs = append(allErrs, field.Required(fieldPath.Index(i).Child("allocationID"), "allocationID can't be empty"))
			}

			if lbSpec.Class != kops.LoadBalancerClassNetwork || lbSpec.Type == kops.LoadBalancerTypeInternal {
				allErrs = append(allErrs, field.Forbidden(fieldPath.Index(i).Child("allocationID"), "allocationID only allowed for Public NLBs"))
			}
		}
	}

	return allErrs
}
