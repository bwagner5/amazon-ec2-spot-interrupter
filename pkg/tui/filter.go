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

package tui

import (
	"sort"
	"strings"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func (m *model) updateTagSuggestions() {
	if m.tagFocus == 0 {
		typed := strings.ToLower(strings.TrimSpace(m.tagKeyInput.Value()))
		keys := m.tagKeysFromRunning()
		m.tagSuggestions = filterContains(keys, typed)
	} else {
		keyName := strings.TrimSpace(m.tagKeyInput.Value())
		typed := strings.ToLower(strings.TrimSpace(m.tagValueInput.Value()))
		values := m.tagValuesForKeyFromRunning(keyName)
		m.tagSuggestions = filterContains(values, typed)
	}
	if m.tagCursor >= len(m.tagSuggestions) {
		m.tagCursor = len(m.tagSuggestions) - 1
	}
	if m.tagCursor < 0 {
		m.tagCursor = 0
	}
}

func (m model) tagKeysFromRunning() []string {
	set := map[string]struct{}{}
	for _, inst := range m.instances {
		if !isRunnable(inst) {
			continue
		}
		for _, t := range inst.Tags {
			if t.Key == nil || *t.Key == "" {
				continue
			}
			set[*t.Key] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m model) tagValuesForKeyFromRunning(key string) []string {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	set := map[string]struct{}{}
	for _, inst := range m.instances {
		if !isRunnable(inst) {
			continue
		}
		for _, t := range inst.Tags {
			if t.Key == nil || *t.Key != key || t.Value == nil {
				continue
			}
			set[*t.Value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func filterContains(items []string, q string) []string {
	if q == "" {
		return items
	}
	out := []string{}
	for _, it := range items {
		if strings.Contains(strings.ToLower(it), q) {
			out = append(out, it)
		}
	}
	return out
}

func matchesTagFilter(inst ec2types.Instance, key string, value string) bool {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return true
	}
	for _, t := range inst.Tags {
		if t.Key == nil || *t.Key != key {
			continue
		}
		if value == "" {
			return true
		}
		if t.Value != nil && *t.Value == value {
			return true
		}
	}
	return false
}

func (m model) filteredIndices() []int {
	if strings.TrimSpace(m.filter) == "" {
		idxs := make([]int, 0, len(m.instances))
		for i := range m.instances {
			if !matchesTagFilter(m.instances[i], m.tagFilterKey, m.tagFilterValue) {
				continue
			}
			idxs = append(idxs, i)
		}
		return idxs
	}

	q := strings.ToLower(strings.TrimSpace(m.filter))
	idxs := []int{}
	for i, inst := range m.instances {
		if !matchesTagFilter(inst, m.tagFilterKey, m.tagFilterValue) {
			continue
		}
		id := instanceID(inst)
		expRef, progress, event := m.hub.RowStatus(id)
		haystack := strings.ToLower(strings.Join([]string{
			id,
			instanceName(inst),
			string(inst.State.Name),
			instanceAZ(inst),
			string(inst.InstanceType),
			expRef,
			progress,
			event,
		}, " "))
		if strings.Contains(haystack, q) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}
