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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
)

type experimentHub struct {
	mu          sync.RWMutex
	order       []string
	experiments map[string]*trackedExperiment
}

type trackedExperiment struct {
	ID         string
	Selected   map[string]ec2types.Instance
	OrderedIDs []string
	Progress   map[string]*instanceProgress
	States     map[string]string
	EventLog   []itn.Event
	Done       bool
	UpdatedAt  time.Time
	StartedAt  time.Time
	Experiment *types.Experiment
}

type instanceProgress struct {
	rebalanceSent bool
	warningSent   bool
	terminating   bool
	terminated    bool
}

func newExperimentHub() *experimentHub {
	return &experimentHub{
		experiments: map[string]*trackedExperiment{},
	}
}

func safeString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *experimentHub) Track(experiment *types.Experiment, selected []*ec2types.Instance, events <-chan itn.Event) string {
	id := safeString(experiment.Id)
	if id == "" || id == "-" {
		id = fmt.Sprintf("unknown-%d", time.Now().UnixNano())
	}

	selectedMap := map[string]ec2types.Instance{}
	ordered := make([]string, 0, len(selected))
	progress := map[string]*instanceProgress{}
	states := map[string]string{}
	for _, ptr := range selected {
		if ptr == nil || ptr.InstanceId == nil {
			continue
		}
		inst := *ptr
		instanceID := *inst.InstanceId
		selectedMap[instanceID] = inst
		ordered = append(ordered, instanceID)
		progress[instanceID] = &instanceProgress{}
		states[instanceID] = string(inst.State.Name)
	}
	sort.Strings(ordered)

	tracked := &trackedExperiment{
		ID:         id,
		Selected:   selectedMap,
		OrderedIDs: ordered,
		Progress:   progress,
		States:     states,
		EventLog:   []itn.Event{},
		StartedAt:  time.Now(),
		UpdatedAt:  time.Now(),
		Experiment: experiment,
	}

	h.mu.Lock()
	h.experiments[id] = tracked
	h.order = append(h.order, id)
	h.mu.Unlock()

	go func() {
		for event := range events {
			h.mu.Lock()
			h.applyEventLocked(tracked, event)
			h.mu.Unlock()
		}
		h.mu.Lock()
		tracked.Done = true
		tracked.UpdatedAt = time.Now()
		h.mu.Unlock()
	}()

	return id
}

func (h *experimentHub) applyEventLocked(exp *trackedExperiment, e itn.Event) {
	exp.EventLog = append(exp.EventLog, e)
	exp.UpdatedAt = time.Now()

	switch e.Stage {
	case itn.EventStageRebalanceSent:
		for _, id := range exp.OrderedIDs {
			exp.Progress[id].rebalanceSent = true
		}
	case itn.EventStageWarningSent:
		for _, id := range exp.OrderedIDs {
			exp.Progress[id].warningSent = true
		}
	case itn.EventStageInstanceTerminating:
		for id, state := range e.InstanceStates {
			h.ensureInstanceLocked(exp, id)
			exp.Progress[id].terminating = true
			exp.States[id] = state
		}
	case itn.EventStageInstanceTerminated:
		for id, state := range e.InstanceStates {
			h.ensureInstanceLocked(exp, id)
			exp.Progress[id].terminated = true
			exp.Progress[id].terminating = true
			exp.Progress[id].warningSent = true
			exp.States[id] = state
		}
	default:
		for id, state := range e.InstanceStates {
			h.ensureInstanceLocked(exp, id)
			exp.States[id] = state
		}
	}
}

func (h *experimentHub) ensureInstanceLocked(exp *trackedExperiment, id string) {
	if _, ok := exp.Progress[id]; !ok {
		exp.Progress[id] = &instanceProgress{}
	}
	if _, ok := exp.Selected[id]; !ok {
		exp.Selected[id] = ec2types.Instance{InstanceId: &id}
		exp.OrderedIDs = append(exp.OrderedIDs, id)
		sort.Strings(exp.OrderedIDs)
	}
}

func (h *experimentHub) RunningCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, id := range h.order {
		exp := h.experiments[id]
		if exp != nil && !exp.Done {
			count++
		}
	}
	return count
}

func (h *experimentHub) LatestID(runningOnly bool) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := len(h.order) - 1; i >= 0; i-- {
		id := h.order[i]
		exp := h.experiments[id]
		if exp == nil {
			continue
		}
		if runningOnly && exp.Done {
			continue
		}
		return id
	}
	return ""
}

func (h *experimentHub) NextID(current string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.order) == 0 {
		return ""
	}
	index := -1
	for i, id := range h.order {
		if id == current {
			index = i
			break
		}
	}
	if index == -1 {
		return h.order[len(h.order)-1]
	}
	return h.order[(index+1)%len(h.order)]
}

func (h *experimentHub) PrevID(current string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.order) == 0 {
		return ""
	}
	index := -1
	for i, id := range h.order {
		if id == current {
			index = i
			break
		}
	}
	if index == -1 {
		return h.order[len(h.order)-1]
	}
	next := index - 1
	if next < 0 {
		next = len(h.order) - 1
	}
	return h.order[next]
}

func (h *experimentHub) Snapshot(id string) (trackedExperiment, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	exp, ok := h.experiments[id]
	if !ok || exp == nil {
		return trackedExperiment{}, false
	}
	out := trackedExperiment{
		ID:         exp.ID,
		Selected:   map[string]ec2types.Instance{},
		OrderedIDs: append([]string{}, exp.OrderedIDs...),
		Progress:   map[string]*instanceProgress{},
		States:     map[string]string{},
		EventLog:   append([]itn.Event{}, exp.EventLog...),
		Done:       exp.Done,
		UpdatedAt:  exp.UpdatedAt,
		StartedAt:  exp.StartedAt,
		Experiment: exp.Experiment,
	}
	for id, inst := range exp.Selected {
		out.Selected[id] = inst
	}
	for id, p := range exp.Progress {
		cp := *p
		out.Progress[id] = &cp
	}
	for id, state := range exp.States {
		out.States[id] = state
	}
	return out, true
}

func (h *experimentHub) RowStatus(instanceID string) (string, string, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	ref := "-"
	progress := "-"
	lastEvent := "-"

	for i := len(h.order) - 1; i >= 0; i-- {
		id := h.order[i]
		exp := h.experiments[id]
		if exp == nil {
			continue
		}
		if _, ok := exp.Selected[instanceID]; !ok {
			continue
		}

		if ref == "-" {
			ref = shortExperimentID(exp.ID)
		}
		if progress == "-" {
			progress = progressLabel(exp.Progress[instanceID])
		}
		if lastEvent == "-" {
			lastEvent = latestInstanceEvent(instanceID, exp.EventLog)
		}
	}

	if lastEvent == "" {
		lastEvent = "-"
	}
	return ref, progress, lastEvent
}

func latestInstanceEvent(instanceID string, events []itn.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if len(e.InstanceStates) == 0 {
			// global events (rebalance/warning) still apply to selected instances
			return sanitizeEvent(e.Message)
		}
		if _, ok := e.InstanceStates[instanceID]; ok {
			return sanitizeEvent(e.Message)
		}
	}
	return "-"
}

func sanitizeEvent(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return "-"
	}
	return strings.ReplaceAll(strings.TrimSpace(msg), "\n", " ")
}

func shortExperimentID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func progressLabel(p *instanceProgress) string {
	switch {
	case p == nil:
		return "queued"
	case p.terminated:
		return "terminated"
	case p.terminating:
		return "terminating"
	case p.warningSent:
		return "itn sent"
	case p.rebalanceSent:
		return "rebalance sent"
	default:
		return "queued"
	}
}
