// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type AWSFilter struct {
	Name   string
	Values []string
}

func ParseAWSFilters(raw []string) ([]AWSFilter, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]AWSFilter, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "Name=") {
			return nil, fmt.Errorf("invalid filter %q: must start with Name=", entry)
		}
		nameAndValues := strings.TrimPrefix(trimmed, "Name=")
		name := nameAndValues
		values := []string{}
		if idx := strings.Index(nameAndValues, ",Values="); idx >= 0 {
			name = strings.TrimSpace(nameAndValues[:idx])
			valuesPart := strings.TrimSpace(nameAndValues[idx+len(",Values="):])
			if valuesPart != "" {
				for _, v := range strings.Split(valuesPart, ",") {
					v = strings.TrimSpace(v)
					if v != "" {
						values = append(values, v)
					}
				}
			}
		}
		if name == "" {
			return nil, fmt.Errorf("invalid filter %q: empty Name", entry)
		}
		if err := validateFilterName(name); err != nil {
			return nil, err
		}
		out = append(out, AWSFilter{Name: name, Values: values})
	}
	return out, nil
}

func validateFilterName(name string) error {
	switch {
	case strings.HasPrefix(name, "tag:"):
		return nil
	case name == "tag-key", name == "tag-value", name == "instance-id":
		return nil
	default:
		return fmt.Errorf("unsupported filter name %q (supported: tag:<key>, tag-key, tag-value, instance-id)", name)
	}
}

func ResolveSpotTargets(ctx context.Context, interrupter *itn.ITN, instanceIDs []string, rawFilters []string, requireMatch bool) ([]ec2types.Instance, error) {
	filters, err := ParseAWSFilters(rawFilters)
	if err != nil {
		return nil, err
	}
	instances, err := interrupter.SpotInstances(ctx)
	if err != nil {
		return nil, err
	}
	idSet := map[string]struct{}{}
	for _, id := range instanceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		idSet[id] = struct{}{}
	}
	targets := make([]ec2types.Instance, 0, len(instances))
	for _, inst := range instances {
		id := instanceID(inst)
		if len(idSet) > 0 {
			if _, ok := idSet[id]; !ok {
				continue
			}
		}
		if !matchesAllFilters(inst, filters) {
			continue
		}
		targets = append(targets, inst)
	}
	sort.Slice(targets, func(i, j int) bool {
		return instanceID(targets[i]) < instanceID(targets[j])
	})
	if requireMatch && len(targets) == 0 {
		return nil, fmt.Errorf("no Spot instances matched the provided selectors")
	}
	return targets, nil
}

func matchesAllFilters(inst ec2types.Instance, filters []AWSFilter) bool {
	for _, f := range filters {
		if !matchesFilter(inst, f) {
			return false
		}
	}
	return true
}

func matchesFilter(inst ec2types.Instance, f AWSFilter) bool {
	switch {
	case strings.HasPrefix(f.Name, "tag:"):
		key := strings.TrimPrefix(f.Name, "tag:")
		for _, t := range inst.Tags {
			if t.Key == nil || *t.Key != key {
				continue
			}
			if len(f.Values) == 0 {
				return true
			}
			if t.Value == nil {
				continue
			}
			for _, v := range f.Values {
				if *t.Value == v {
					return true
				}
			}
		}
		return false
	case f.Name == "tag-key":
		for _, t := range inst.Tags {
			if t.Key == nil {
				continue
			}
			if len(f.Values) == 0 {
				return true
			}
			for _, v := range f.Values {
				if *t.Key == v {
					return true
				}
			}
		}
		return false
	case f.Name == "tag-value":
		for _, t := range inst.Tags {
			if t.Value == nil {
				continue
			}
			if len(f.Values) == 0 {
				return true
			}
			for _, v := range f.Values {
				if *t.Value == v {
					return true
				}
			}
		}
		return false
	case f.Name == "instance-id":
		if len(f.Values) == 0 {
			return false
		}
		id := instanceID(inst)
		for _, v := range f.Values {
			if id == v {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func instanceID(inst ec2types.Instance) string {
	if inst.InstanceId == nil {
		return ""
	}
	return *inst.InstanceId
}
