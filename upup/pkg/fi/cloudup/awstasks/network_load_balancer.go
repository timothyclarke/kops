/*
Copyright 2019 The Kubernetes Authors.

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

package awstasks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/route53"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/cloudformation"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// NetworkLoadBalancer manages an NLB.  We find the existing NLB using the Name tag.
var _ DNSTarget = &NetworkLoadBalancer{}

//go:generate fitask -type=NetworkLoadBalancer
type NetworkLoadBalancer struct {
	// We use the Name tag to find the existing NLB, because we are (more or less) unrestricted when
	// it comes to tag values, but the LoadBalancerName is length limited
	Name      *string
	Lifecycle *fi.Lifecycle

	// LoadBalancerName is the name in NLB, possibly different from our name
	// (NLB is restricted as to names, so we have limited choices!)
	// We use the Name tag to find the existing NLB.
	LoadBalancerName *string

	DNSName      *string
	HostedZoneId *string

	SubnetMappings []*SubnetMapping

	Listeners []*NetworkLoadBalancerListener

	Scheme *string

	CrossZoneLoadBalancing *bool

	Tags         map[string]string
	ForAPIServer bool

	Type *string

	VPC          *VPC
	TargetGroups []*TargetGroup
}

var _ fi.CompareWithID = &NetworkLoadBalancer{}

func (e *NetworkLoadBalancer) CompareWithID() *string {
	return e.Name
}

type NetworkLoadBalancerListener struct {
	Port             int
	TargetGroupName  string
	SSLCertificateID string
	SSLPolicy        string
}

func (e *NetworkLoadBalancerListener) mapToAWS(targetGroups []*TargetGroup, loadBalancerArn string) (*elbv2.CreateListenerInput, error) {
	var tgARN string
	for _, tg := range targetGroups {
		if fi.StringValue(tg.Name) == e.TargetGroupName {
			tgARN = fi.StringValue(tg.ARN)
		}
	}
	if tgARN == "" {
		return nil, fmt.Errorf("target group not found for NLB listener %+v", e)
	}

	l := &elbv2.CreateListenerInput{
		DefaultActions: []*elbv2.Action{
			{
				TargetGroupArn: aws.String(tgARN),
				Type:           aws.String(elbv2.ActionTypeEnumForward),
			},
		},
		LoadBalancerArn: aws.String(loadBalancerArn),
		Port:            aws.Int64(int64(e.Port)),
	}

	if e.SSLCertificateID != "" {
		l.Certificates = []*elbv2.Certificate{}
		l.Certificates = append(l.Certificates, &elbv2.Certificate{
			CertificateArn: aws.String(e.SSLCertificateID),
		})
		l.Protocol = aws.String(elbv2.ProtocolEnumTls)
		if e.SSLPolicy != "" {
			l.SslPolicy = aws.String(e.SSLPolicy)
		}
	} else {
		l.Protocol = aws.String(elbv2.ProtocolEnumTcp)
	}

	return l, nil
}

var _ fi.HasDependencies = &NetworkLoadBalancerListener{}

func (e *NetworkLoadBalancerListener) GetDependencies(tasks map[string]fi.Task) []fi.Task {
	return nil
}

// OrderListenersByPort implements sort.Interface for []OrderListenersByPort, based on port number
type OrderListenersByPort []*NetworkLoadBalancerListener

func (a OrderListenersByPort) Len() int      { return len(a) }
func (a OrderListenersByPort) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a OrderListenersByPort) Less(i, j int) bool {
	return a[i].Port < a[j].Port
}

//The load balancer name 'api.renamenlbcluster.k8s.local' can only contain characters that are alphanumeric characters and hyphens(-)\n\tstatus code: 400,
func findNetworkLoadBalancerByLoadBalancerName(cloud awsup.AWSCloud, loadBalancerName string) (*elbv2.LoadBalancer, error) {
	request := &elbv2.DescribeLoadBalancersInput{
		Names: []*string{&loadBalancerName},
	}
	found, err := describeNetworkLoadBalancers(cloud, request, func(lb *elbv2.LoadBalancer) bool {
		// TODO: Filter by cluster?

		if aws.StringValue(lb.LoadBalancerName) == loadBalancerName {
			return true
		}

		klog.Warningf("Got NLB with unexpected name: %q", aws.StringValue(lb.LoadBalancerName))
		return false
	})

	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "LoadBalancerNotFound" {
				return nil, nil
			}
		}

		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple NLBs with name %q", loadBalancerName)
	}

	return found[0], nil
}

func findNetworkLoadBalancerByAlias(cloud awsup.AWSCloud, alias *route53.AliasTarget) (*elbv2.LoadBalancer, error) {
	// TODO: Any way to avoid listing all NLBs?
	request := &elbv2.DescribeLoadBalancersInput{}

	dnsName := aws.StringValue(alias.DNSName)
	matchDnsName := strings.TrimSuffix(dnsName, ".")
	if matchDnsName == "" {
		return nil, fmt.Errorf("DNSName not set on AliasTarget")
	}

	matchHostedZoneId := aws.StringValue(alias.HostedZoneId)

	found, err := describeNetworkLoadBalancers(cloud, request, func(lb *elbv2.LoadBalancer) bool {
		// TODO: Filter by cluster?

		if matchHostedZoneId != aws.StringValue(lb.CanonicalHostedZoneId) {
			return false
		}

		lbDnsName := aws.StringValue(lb.DNSName)
		lbDnsName = strings.TrimSuffix(lbDnsName, ".")
		return lbDnsName == matchDnsName || "dualstack."+lbDnsName == matchDnsName
	})

	if err != nil {
		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple NLBs with DNSName %q", dnsName)
	}

	return found[0], nil
}

func FindNetworkLoadBalancerByNameTag(cloud awsup.AWSCloud, findNameTag string) (*elbv2.LoadBalancer, error) {
	// TODO: Any way around this?
	klog.V(2).Infof("Listing all NLBs for findNetworkLoadBalancerByNameTag")

	request := &elbv2.DescribeLoadBalancersInput{}
	// ELB DescribeTags has a limit of 20 names, so we set the page size here to 20 also
	request.PageSize = aws.Int64(20)

	var found []*elbv2.LoadBalancer

	var innerError error
	err := cloud.ELBV2().DescribeLoadBalancersPages(request, func(p *elbv2.DescribeLoadBalancersOutput, lastPage bool) bool {
		if len(p.LoadBalancers) == 0 {
			return true
		}

		// TODO: Filter by cluster?

		var arns []string
		arnToELB := make(map[string]*elbv2.LoadBalancer)
		for _, elb := range p.LoadBalancers {
			arn := aws.StringValue(elb.LoadBalancerArn)
			arnToELB[arn] = elb
			arns = append(arns, arn)
		}

		tagMap, err := describeNetworkLoadBalancerTags(cloud, arns)
		if err != nil {
			innerError = err
			return false
		}

		for loadBalancerArn, tags := range tagMap {
			name, foundNameTag := awsup.FindELBV2Tag(tags, "Name")
			if !foundNameTag || name != findNameTag {
				continue
			}
			elb := arnToELB[loadBalancerArn]
			found = append(found, elb)
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("error describing LoadBalancers: %v", err)
	}
	if innerError != nil {
		return nil, fmt.Errorf("error describing LoadBalancers: %v", innerError)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple NLBs with Name %q", findNameTag)
	}

	return found[0], nil
}

func describeNetworkLoadBalancers(cloud awsup.AWSCloud, request *elbv2.DescribeLoadBalancersInput, filter func(*elbv2.LoadBalancer) bool) ([]*elbv2.LoadBalancer, error) {
	var found []*elbv2.LoadBalancer
	err := cloud.ELBV2().DescribeLoadBalancersPages(request, func(p *elbv2.DescribeLoadBalancersOutput, lastPage bool) (shouldContinue bool) {
		for _, lb := range p.LoadBalancers {
			if filter(lb) {
				found = append(found, lb)
			}
		}

		return true
	})

	if err != nil {
		return nil, fmt.Errorf("error listing NLBs: %v", err)
	}

	return found, nil
}

func describeNetworkLoadBalancerTags(cloud awsup.AWSCloud, loadBalancerArns []string) (map[string][]*elbv2.Tag, error) {
	// TODO: Filter by cluster?

	request := &elbv2.DescribeTagsInput{}
	request.ResourceArns = aws.StringSlice(loadBalancerArns)

	// TODO: Cache?
	klog.V(2).Infof("Querying ELBV2 api for tags for %s", loadBalancerArns)
	response, err := cloud.ELBV2().DescribeTags(request)
	if err != nil {
		return nil, err
	}

	tagMap := make(map[string][]*elbv2.Tag)
	for _, tagset := range response.TagDescriptions {
		tagMap[aws.StringValue(tagset.ResourceArn)] = tagset.Tags
	}
	return tagMap, nil
}

func (e *NetworkLoadBalancer) getDNSName() *string {
	return e.DNSName
}

func (e *NetworkLoadBalancer) getHostedZoneId() *string {
	return e.HostedZoneId
}

func (e *NetworkLoadBalancer) Find(c *fi.Context) (*NetworkLoadBalancer, error) {
	cloud := c.Cloud.(awsup.AWSCloud)

	lb, err := FindNetworkLoadBalancerByNameTag(cloud, e.Tags["Name"])
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	loadBalancerArn := lb.LoadBalancerArn

	actual := &NetworkLoadBalancer{}
	actual.Name = e.Name
	actual.LoadBalancerName = lb.LoadBalancerName
	actual.DNSName = lb.DNSName
	actual.HostedZoneId = lb.CanonicalHostedZoneId //CanonicalHostedZoneNameID
	actual.Scheme = lb.Scheme
	actual.VPC = &VPC{ID: lb.VpcId}
	actual.Type = lb.Type

	tagMap, err := describeNetworkLoadBalancerTags(cloud, []string{*loadBalancerArn})
	if err != nil {
		return nil, err
	}
	actual.Tags = make(map[string]string)
	for _, tag := range tagMap[*loadBalancerArn] {
		if strings.HasPrefix(aws.StringValue(tag.Key), "aws:cloudformation:") {
			continue
		}
		actual.Tags[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}

	for _, az := range lb.AvailabilityZones {
		sm := &SubnetMapping{
			Subnet: &Subnet{ID: az.SubnetId},
		}
		for _, a := range az.LoadBalancerAddresses {
			if a.PrivateIPv4Address != nil {
				if sm.PrivateIPv4Address != nil {
					return nil, fmt.Errorf("NLB has more then one PrivateIPv4Address, which is unexpected. This is a bug in kOps, please open a GitHub issue.")
				}
				sm.PrivateIPv4Address = a.PrivateIPv4Address
			}
			if a.AllocationId != nil {
				if sm.AllocationId != nil {
					return nil, fmt.Errorf("NLB has more then one AllocationId per subnet, which is unexpected. This is a bug in kOps, please open a GitHub issue.")
				}
				sm.AllocationId = a.AllocationId
			}
		}
		actual.SubnetMappings = append(actual.SubnetMappings, sm)
	}

	{
		request := &elbv2.DescribeListenersInput{
			LoadBalancerArn: loadBalancerArn,
		}
		response, err := cloud.ELBV2().DescribeListeners(request)
		if err != nil {
			return nil, fmt.Errorf("error querying for NLB listeners :%v", err)
		}

		actual.Listeners = []*NetworkLoadBalancerListener{}
		actual.TargetGroups = []*TargetGroup{}
		for _, l := range response.Listeners {
			actualListener := &NetworkLoadBalancerListener{}
			actualListener.Port = int(aws.Int64Value(l.Port))
			if len(l.Certificates) != 0 {
				actualListener.SSLCertificateID = aws.StringValue(l.Certificates[0].CertificateArn) // What if there is more then one certificate, can we just grab the default certificate? we don't set it as default, we only set the one.
				if l.SslPolicy != nil {
					actualListener.SSLPolicy = aws.StringValue(l.SslPolicy)
				}
			}

			// This will need to be rearranged when we recognized multiple listeners and target groups per NLB
			if len(l.DefaultActions) > 0 {
				targetGroupARN := l.DefaultActions[0].TargetGroupArn
				if targetGroupARN != nil {
					targetGroupName, err := awsup.GetTargetGroupNameFromARN(fi.StringValue(targetGroupARN))
					if err != nil {
						return nil, err
					}
					actual.TargetGroups = append(actual.TargetGroups, &TargetGroup{ARN: targetGroupARN, Name: fi.String(targetGroupName)})

					cloud := c.Cloud.(awsup.AWSCloud)
					descResp, err := cloud.ELBV2().DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
						TargetGroupArns: []*string{targetGroupARN},
					})
					if err != nil {
						return nil, fmt.Errorf("error querying for NLB listener target groups: %v", err)
					}
					if len(descResp.TargetGroups) != 1 {
						return nil, fmt.Errorf("unexpected DescribeTargetGroups response: %v", descResp)
					}

					actualListener.TargetGroupName = aws.StringValue(descResp.TargetGroups[0].TargetGroupName)
				}
			}
			actual.Listeners = append(actual.Listeners, actualListener)
		}
		sort.Stable(OrderTargetGroupsByName(actual.TargetGroups))

	}

	{
		lbAttributes, err := findNetworkLoadBalancerAttributes(cloud, aws.StringValue(loadBalancerArn))
		if err != nil {
			return nil, err
		}
		klog.V(4).Infof("NLB Load Balancer attributes: %+v", lbAttributes)

		for _, attribute := range lbAttributes {
			if attribute.Value == nil {
				continue
			}
			switch key, value := attribute.Key, attribute.Value; *key {
			case "load_balancing.cross_zone.enabled":
				b, err := strconv.ParseBool(*value)
				if err != nil {
					return nil, err
				}
				actual.CrossZoneLoadBalancing = fi.Bool(b)
			default:
				klog.V(2).Infof("unsupported key -- ignoring, %v.\n", key)
			}
		}
	}

	// Avoid spurious mismatches
	if subnetMappingSlicesEqualIgnoreOrder(actual.SubnetMappings, e.SubnetMappings) {
		actual.SubnetMappings = e.SubnetMappings
	}
	if e.DNSName == nil {
		e.DNSName = actual.DNSName
	}
	if e.HostedZoneId == nil {
		e.HostedZoneId = actual.HostedZoneId
	}
	if e.LoadBalancerName == nil {
		e.LoadBalancerName = actual.LoadBalancerName
	}

	// We allow for the LoadBalancerName to be wrong:
	// 1. We don't want to force a rename of the NLB, because that is a destructive operation
	if fi.StringValue(e.LoadBalancerName) != fi.StringValue(actual.LoadBalancerName) {
		klog.V(2).Infof("Reusing existing load balancer with name: %q", aws.StringValue(actual.LoadBalancerName))
		e.LoadBalancerName = actual.LoadBalancerName
	}

	// TODO: Make Normalize a standard method
	actual.Normalize()
	actual.ForAPIServer = e.ForAPIServer
	actual.Lifecycle = e.Lifecycle

	klog.V(4).Infof("Found NLB %+v", actual)

	return actual, nil
}

var _ fi.HasAddress = &NetworkLoadBalancer{}

func (e *NetworkLoadBalancer) IsForAPIServer() bool {
	return e.ForAPIServer
}

func (e *NetworkLoadBalancer) FindIPAddress(context *fi.Context) (*string, error) {
	cloud := context.Cloud.(awsup.AWSCloud)

	lb, err := FindNetworkLoadBalancerByNameTag(cloud, e.Tags["Name"])
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	lbDnsName := fi.StringValue(lb.DNSName)
	if lbDnsName == "" {
		return nil, nil
	}
	return &lbDnsName, nil
}

func (e *NetworkLoadBalancer) Run(c *fi.Context) error {
	// TODO: Make Normalize a standard method
	e.Normalize()

	return fi.DefaultDeltaRunMethod(e, c)
}

func (e *NetworkLoadBalancer) Normalize() {
	// We need to sort our arrays consistently, so we don't get spurious changes
	sort.Stable(OrderSubnetMappingsByID(e.SubnetMappings))
	sort.Stable(OrderListenersByPort(e.Listeners))
	sort.Stable(OrderTargetGroupsByName(e.TargetGroups))
}

func (s *NetworkLoadBalancer) CheckChanges(a, e, changes *NetworkLoadBalancer) error {
	if a == nil {
		if fi.StringValue(e.Name) == "" {
			return fi.RequiredField("Name")
		}
		if len(e.SubnetMappings) == 0 {
			return fi.RequiredField("SubnetMappings")
		}

		if e.CrossZoneLoadBalancing != nil {
			if e.CrossZoneLoadBalancing == nil {
				return fi.RequiredField("CrossZoneLoadBalancing")
			}
		}
	} else {
		if len(changes.SubnetMappings) > 0 {
			expectedSubnets := make(map[string]*string)
			for _, s := range e.SubnetMappings {
				expectedSubnets[*s.Subnet.ID] = s.PrivateIPv4Address
			}

			for _, s := range e.SubnetMappings {
				expectedSubnets[*s.Subnet.ID] = s.AllocationId
			}

			for _, s := range a.SubnetMappings {
				eIP, ok := expectedSubnets[*s.Subnet.ID]
				if !ok {
					return fmt.Errorf("network load balancers do not support detaching subnets")
				}
				if fi.StringValue(eIP) != fi.StringValue(s.PrivateIPv4Address) || fi.StringValue(eIP) != fi.StringValue(s.AllocationId) {
					return fmt.Errorf("network load balancers do not support modifying address settings")
				}
			}
		}
	}
	return nil
}

func (_ *NetworkLoadBalancer) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *NetworkLoadBalancer) error {
	var loadBalancerName string
	var loadBalancerArn string

	if len(e.Listeners) != len(e.TargetGroups) {
		return fmt.Errorf("nlb listeners and target groups do not match: %v listeners vs %v target groups", len(e.Listeners), len(e.TargetGroups))
	}

	if a == nil {
		if e.LoadBalancerName == nil {
			return fi.RequiredField("LoadBalancerName")
		}
		for _, tg := range e.TargetGroups {
			if tg.ARN == nil {
				return fmt.Errorf("missing required target group ARN for NLB creation %v", tg)
			}
		}

		loadBalancerName = *e.LoadBalancerName

		request := &elbv2.CreateLoadBalancerInput{}
		request.Name = e.LoadBalancerName
		request.Scheme = e.Scheme
		request.Type = e.Type

		for _, subnetMapping := range e.SubnetMappings {
      if subnetMapping.PrivateIPv4Address != nil {
        request.SubnetMappings = append(request.SubnetMappings, &elbv2.SubnetMapping{
          SubnetId:           subnetMapping.Subnet.ID,
          PrivateIPv4Address: subnetMapping.PrivateIPv4Address,
        })
      }
      if subnetMapping.AllocationId != nil {
        request.SubnetMappings = append(request.SubnetMappings, &elbv2.SubnetMapping{
          SubnetId:     subnetMapping.Subnet.ID,
          AllocationId: subnetMapping.AllocationId,
        })
      }
		}

		{
			klog.V(2).Infof("Creating NLB with Name:%q", loadBalancerName)

			response, err := t.Cloud.ELBV2().CreateLoadBalancer(request)
			if err != nil {
				return fmt.Errorf("error creating NLB: %v", err)
			}

			if len(response.LoadBalancers) != 1 {
				return fmt.Errorf("Either too many or too few NLBs were created, wanted to find %q", loadBalancerName)
			} else {
				lb := response.LoadBalancers[0]
				e.DNSName = lb.DNSName
				e.HostedZoneId = lb.CanonicalHostedZoneId
				e.VPC = &VPC{ID: lb.VpcId}
				loadBalancerArn = fi.StringValue(lb.LoadBalancerArn)
			}
		}

		{
			for _, listener := range e.Listeners {
				createListenerInput, err := listener.mapToAWS(e.TargetGroups, loadBalancerArn)
				if err != nil {
					return err
				}

				klog.V(2).Infof("Creating Listener for NLB with port %v", listener.Port)
				_, err = t.Cloud.ELBV2().CreateListener(createListenerInput)
				if err != nil {
					return fmt.Errorf("error creating listener for NLB: %v", err)
				}
			}
		}
	} else {
		loadBalancerName = fi.StringValue(a.LoadBalancerName)

		lb, err := findNetworkLoadBalancerByLoadBalancerName(t.Cloud, loadBalancerName)
		if err != nil {
			return fmt.Errorf("error getting load balancer by name: %v", err)
		}

		loadBalancerArn = fi.StringValue(lb.LoadBalancerArn)

		if changes.SubnetMappings != nil {
			actualSubnets := make(map[string]*string)
			for _, s := range a.SubnetMappings {
        if s.PrivateIPv4Address != nil {
				  actualSubnets[*s.Subnet.ID] = s.PrivateIPv4Address
        }
        if s.AllocationId != nil {
				  actualSubnets[*s.Subnet.ID] = s.AllocationId
        }
			}

			var awsSubnetMappings []*elbv2.SubnetMapping
			hasChanges := false
			for _, s := range e.SubnetMappings {
				aIP, ok := actualSubnets[*s.Subnet.ID]
				if !ok || fi.StringValue(s.PrivateIPv4Address) != fi.StringValue(aIP) {
					hasChanges = true
				}
				awsSubnetMappings = append(awsSubnetMappings, &elbv2.SubnetMapping{
					SubnetId:           s.Subnet.ID,
					PrivateIPv4Address: s.PrivateIPv4Address,
				})
			}

			if hasChanges {
				request := &elbv2.SetSubnetsInput{}
				request.SetLoadBalancerArn(loadBalancerArn)
				request.SetSubnetMappings(awsSubnetMappings)

				klog.V(2).Infof("Attaching Load Balancer to new subnets")
				if _, err := t.Cloud.ELBV2().SetSubnets(request); err != nil {
					return fmt.Errorf("error attaching load balancer to new subnets: %v", err)
				}
			}
		}

		if changes.Listeners != nil {

			if lb != nil {

				request := &elbv2.DescribeListenersInput{
					LoadBalancerArn: lb.LoadBalancerArn,
				}
				response, err := t.Cloud.ELBV2().DescribeListeners(request)
				if err != nil {
					return fmt.Errorf("error querying for NLB listeners :%v", err)
				}

				for _, l := range response.Listeners {
					// delete the listener before recreating it
					_, err := t.Cloud.ELBV2().DeleteListener(&elbv2.DeleteListenerInput{
						ListenerArn: l.ListenerArn,
					})
					if err != nil {
						return fmt.Errorf("error deleting load balancer listener with arn = : %v : %v", l.ListenerArn, err)
					}
				}
			}

			for _, listener := range changes.Listeners {

				awsListener, err := listener.mapToAWS(e.TargetGroups, loadBalancerArn)
				if err != nil {
					return err
				}

				klog.V(2).Infof("Creating Listener for NLB with port %v", listener.Port)
				_, err = t.Cloud.ELBV2().CreateListener(awsListener)
				if err != nil {
					return fmt.Errorf("error creating NLB listener: %v", err)
				}
			}
		}
	}

	if err := t.AddELBV2Tags(loadBalancerArn, e.Tags); err != nil {
		return err
	}

	if err := t.RemoveELBV2Tags(loadBalancerArn, e.Tags); err != nil {
		return err
	}

	if err := e.modifyLoadBalancerAttributes(t, a, e, changes, loadBalancerArn); err != nil {
		klog.Infof("error modifying NLB attributes: %v", err)
		return err
	}
	return nil
}

type terraformNetworkLoadBalancer struct {
	Name                   string                                      `json:"name" cty:"name"`
	Internal               bool                                        `json:"internal" cty:"internal"`
	Type                   string                                      `json:"load_balancer_type" cty:"load_balancer_type"`
	SubnetMappings         []terraformNetworkLoadBalancerSubnetMapping `json:"subnet_mapping" cty:"subnet_mapping"`
	CrossZoneLoadBalancing bool                                        `json:"enable_cross_zone_load_balancing" cty:"enable_cross_zone_load_balancing"`

	Tags map[string]string `json:"tags" cty:"tags"`
}

type terraformNetworkLoadBalancerSubnetMapping struct {
	Subnet             *terraform.Literal `json:"subnet_id" cty:"subnet_id"`
	AllocationID       *string            `json:"allocation_id,omitempty" cty:"allocation_id"`
	PrivateIPv4Address *string            `json:"private_ipv4_address,omitempty" cty:"private_ipv4_address"`
}

type terraformNetworkLoadBalancerListener struct {
	LoadBalancer   *terraform.Literal                           `json:"load_balancer_arn" cty:"load_balancer_arn"`
	Port           int64                                        `json:"port" cty:"port"`
	Protocol       string                                       `json:"protocol" cty:"protocol"`
	CertificateARN *string                                      `json:"certificate_arn,omitempty" cty:"certificate_arn"`
	SSLPolicy      *string                                      `json:"ssl_policy,omitempty" cty:"ssl_policy"`
	DefaultAction  []terraformNetworkLoadBalancerListenerAction `json:"default_action" cty:"default_action"`
}

type terraformNetworkLoadBalancerListenerAction struct {
	Type           string             `json:"type" cty:"type"`
	TargetGroupARN *terraform.Literal `json:"target_group_arn,omitempty" cty:"target_group_arn"`
}

func (_ *NetworkLoadBalancer) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *NetworkLoadBalancer) error {
	nlbTF := &terraformNetworkLoadBalancer{
		Name:                   *e.LoadBalancerName,
		Internal:               fi.StringValue(e.Scheme) == elbv2.LoadBalancerSchemeEnumInternal,
		Type:                   elbv2.LoadBalancerTypeEnumNetwork,
		Tags:                   e.Tags,
		CrossZoneLoadBalancing: fi.BoolValue(e.CrossZoneLoadBalancing),
	}

	for _, subnetMapping := range e.SubnetMappings {
		nlbTF.SubnetMappings = append(nlbTF.SubnetMappings, terraformNetworkLoadBalancerSubnetMapping{
			Subnet:             subnetMapping.Subnet.TerraformLink(),
			PrivateIPv4Address: subnetMapping.PrivateIPv4Address,
		})
	}

	err := t.RenderResource("aws_lb", *e.Name, nlbTF)
	if err != nil {
		return err
	}

	for _, listener := range e.Listeners {
		var listenerTG *TargetGroup
		for _, tg := range e.TargetGroups {
			if aws.StringValue(tg.Name) == listener.TargetGroupName {
				listenerTG = tg
				break
			}
		}
		if listenerTG == nil {
			return fmt.Errorf("target group not found for NLB listener %+v", e)
		}
		listenerTF := &terraformNetworkLoadBalancerListener{
			LoadBalancer: e.TerraformLink(),
			Port:         int64(listener.Port),
			DefaultAction: []terraformNetworkLoadBalancerListenerAction{
				{
					Type:           elbv2.ActionTypeEnumForward,
					TargetGroupARN: listenerTG.TerraformLink(),
				},
			},
		}
		if listener.SSLCertificateID != "" {
			listenerTF.CertificateARN = &listener.SSLCertificateID
			listenerTF.Protocol = elbv2.ProtocolEnumTls
			if listener.SSLPolicy != "" {
				listenerTF.SSLPolicy = &listener.SSLPolicy
			}
		} else {
			listenerTF.Protocol = elbv2.ProtocolEnumTcp
		}

		err = t.RenderResource("aws_lb_listener", fmt.Sprintf("%v-%v", *e.Name, listener.Port), listenerTF)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *NetworkLoadBalancer) TerraformLink(params ...string) *terraform.Literal {
	prop := "id"
	if len(params) > 0 {
		prop = params[0]
	}
	return terraform.LiteralProperty("aws_lb", *e.Name, prop)
}

type cloudformationNetworkLoadBalancer struct {
	Name           string                         `json:"Name"`
	Scheme         string                         `json:"Scheme"`
	SubnetMappings []*cloudformationSubnetMapping `json:"SubnetMappings"`
	Type           string                         `json:"Type"`
	Tags           []cloudformationTag            `json:"Tags"`
}

type cloudformationSubnetMapping struct {
	Subnet             *cloudformation.Literal `json:"SubnetId"`
	AllocationId       *string                 `json:"AllocationId,omitempty"`
	PrivateIPv4Address *string                 `json:"PrivateIPv4Address,omitempty"`
}

type cloudformationNetworkLoadBalancerListener struct {
	Certificates    []cloudformationNetworkLoadBalancerListenerCertificate `json:"Certificates,omitempty"`
	DefaultActions  []cloudformationNetworkLoadBalancerListenerAction      `json:"DefaultActions"`
	LoadBalancerARN *cloudformation.Literal                                `json:"LoadBalancerArn"`
	Port            int64                                                  `json:"Port"`
	Protocol        string                                                 `json:"Protocol"`
	SSLPolicy       *string                                                `json:"SslPolicy,omitempty"`
}

type cloudformationNetworkLoadBalancerListenerCertificate struct {
	CertificateArn string `json:"CertificateArn"`
}

type cloudformationNetworkLoadBalancerListenerAction struct {
	Type           string                  `json:"Type"`
	TargetGroupARN *cloudformation.Literal `json:"TargetGroupArn"`
}

func (_ *NetworkLoadBalancer) RenderCloudformation(t *cloudformation.CloudformationTarget, a, e, changes *NetworkLoadBalancer) error {
	nlbCF := &cloudformationNetworkLoadBalancer{
		Name: *e.LoadBalancerName,
		Type: elbv2.LoadBalancerTypeEnumNetwork,
		Tags: buildCloudformationTags(e.Tags),
	}
	for _, subnetMapping := range e.SubnetMappings {
		nlbCF.SubnetMappings = append(nlbCF.SubnetMappings, &cloudformationSubnetMapping{
			Subnet:             subnetMapping.Subnet.CloudformationLink(),
			PrivateIPv4Address: subnetMapping.PrivateIPv4Address,
		})
	}
	if e.Scheme != nil {
		nlbCF.Scheme = *e.Scheme
	} else {
		nlbCF.Scheme = elbv2.LoadBalancerSchemeEnumInternetFacing
	}
	err := t.RenderResource("AWS::ElasticLoadBalancingV2::LoadBalancer", *e.Name, nlbCF)
	if err != nil {
		return err
	}

	for _, listener := range e.Listeners {
		var listenerTG *TargetGroup
		for _, tg := range e.TargetGroups {
			if aws.StringValue(tg.Name) == listener.TargetGroupName {
				listenerTG = tg
				break
			}
		}
		if listenerTG == nil {
			return fmt.Errorf("target group not found for NLB listener %+v", e)
		}
		listenerCF := &cloudformationNetworkLoadBalancerListener{
			LoadBalancerARN: e.CloudformationLink(),
			Port:            int64(listener.Port),
			DefaultActions: []cloudformationNetworkLoadBalancerListenerAction{
				{
					Type:           elbv2.ActionTypeEnumForward,
					TargetGroupARN: listenerTG.CloudformationLink(),
				},
			},
		}
		if listener.SSLCertificateID != "" {
			listenerCF.Certificates = []cloudformationNetworkLoadBalancerListenerCertificate{
				{CertificateArn: listener.SSLCertificateID},
			}
			listenerCF.Protocol = elbv2.ProtocolEnumTls
			if listener.SSLPolicy != "" {
				listenerCF.SSLPolicy = &listener.SSLPolicy
			}
		} else {
			listenerCF.Protocol = elbv2.ProtocolEnumTcp
		}

		err = t.RenderResource("AWS::ElasticLoadBalancingV2::Listener", fmt.Sprintf("%v-%v", *e.Name, listener.Port), listenerCF)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *NetworkLoadBalancer) CloudformationLink() *cloudformation.Literal {
	return cloudformation.Ref("AWS::ElasticLoadBalancingV2::LoadBalancer", *e.Name)
}

func (e *NetworkLoadBalancer) CloudformationAttrCanonicalHostedZoneNameID() *cloudformation.Literal {
	return cloudformation.GetAtt("AWS::ElasticLoadBalancingV2::LoadBalancer", *e.Name, "CanonicalHostedZoneID")
}

func (e *NetworkLoadBalancer) CloudformationAttrDNSName() *cloudformation.Literal {
	return cloudformation.GetAtt("AWS::ElasticLoadBalancingV2::LoadBalancer", *e.Name, "DNSName")
}
