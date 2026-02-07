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

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/cli"
	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type monitor struct {
	events         <-chan itn.Event
	spinner        spinner.Model
	experiment     *types.Experiment
	summary        string
	eventLog       []itn.Event
	progress       map[string]*instanceProgress
	orderedIDs     []string
	currentState   map[string]string
	experimentDone bool
}

type instanceProgress struct {
	rebalanceSent bool
	warningSent   bool
	terminating   bool
	terminated    bool
}

func NewMonitor(experiment *types.Experiment, events <-chan itn.Event) monitor {
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("206"))
	sp.Spinner = spinner.Points
	orderedIDs := targetInstanceIDs(experiment)
	progress := map[string]*instanceProgress{}
	for _, id := range orderedIDs {
		progress[id] = &instanceProgress{}
	}
	return monitor{
		experiment:   experiment,
		summary:      cli.Summary(experiment),
		events:       events,
		spinner:      sp,
		progress:     progress,
		orderedIDs:   orderedIDs,
		currentState: map[string]string{},
	}
}

type eventMsg itn.Event
type doneMsg bool

func eventListener(events <-chan itn.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return doneMsg(true)
		}
		return eventMsg(event)
	}
}

func (m monitor) Init() tea.Cmd {
	return tea.Batch(spinner.Tick, eventListener(m.events))
}

func (m monitor) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.experimentDone {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case eventMsg:
		e := itn.Event(msg)
		m.eventLog = append(m.eventLog, e)
		m.applyEvent(e)
		return m, eventListener(m.events)
	case doneMsg:
		m.experimentDone = true
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "enter":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *monitor) applyEvent(e itn.Event) {
	switch e.Stage {
	case itn.EventStageRebalanceSent:
		for _, id := range m.orderedIDs {
			m.progress[id].rebalanceSent = true
		}
	case itn.EventStageWarningSent:
		for _, id := range m.orderedIDs {
			m.progress[id].warningSent = true
		}
	case itn.EventStageInstanceTerminating:
		for id, state := range e.InstanceStates {
			m.ensureProgress(id)
			m.progress[id].terminating = true
			m.currentState[id] = state
		}
	case itn.EventStageInstanceTerminated:
		for id, state := range e.InstanceStates {
			m.ensureProgress(id)
			m.progress[id].terminated = true
			m.progress[id].terminating = true
			m.warningSentSafe(id)
			m.currentState[id] = state
		}
	default:
		if len(e.InstanceStates) > 0 {
			for id, state := range e.InstanceStates {
				m.currentState[id] = state
			}
		}
	}
}

func (m *monitor) warningSentSafe(id string) {
	if progress, ok := m.progress[id]; ok {
		progress.warningSent = true
	}
}

func (m *monitor) ensureProgress(id string) {
	if _, ok := m.progress[id]; !ok {
		m.progress[id] = &instanceProgress{}
		m.orderedIDs = append(m.orderedIDs, id)
		sort.Strings(m.orderedIDs)
	}
}

func marker(done bool, active bool) string {
	if done {
		return "[x]"
	}
	if active {
		return "[~]"
	}
	return "[ ]"
}

func (m monitor) View() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s\n", m.summary))
	b.WriteString("Progress\n")
	b.WriteString("INSTANCE ID          REBALANCE  ITN(2M)    TERMINATING  TERMINATED   STATE\n")
	b.WriteString("------------------  ---------  ---------  -----------  -----------  ----------\n")
	for _, id := range m.orderedIDs {
		p := m.progress[id]
		state := m.currentState[id]
		if state == "" {
			state = "-"
		}
		b.WriteString(fmt.Sprintf(
			"%-18s  %-9s  %-9s  %-11s  %-11s  %-10s\n",
			id,
			marker(p.rebalanceSent, false),
			marker(p.warningSent, false),
			marker(p.terminating, p.warningSent && !p.terminating),
			marker(p.terminated, p.terminating && !p.terminated),
			state,
		))
	}

	b.WriteString("\nEvents\n")
	start := 0
	if len(m.eventLog) > 10 {
		start = len(m.eventLog) - 10
	}
	for _, event := range m.eventLog[start:] {
		b.WriteString(fmt.Sprintf("%s  %s\n", event.Timestamp.Format("15:04:05"), event.Message))
	}

	if m.experimentDone {
		b.WriteString("\nExperiment stream completed. Press enter or q to exit.\n")
	} else {
		b.WriteString(fmt.Sprintf("\nMonitoring %s\n", m.spinner.View()))
	}
	b.WriteString(help())
	return b.String()
}

func targetInstanceIDs(experiment *types.Experiment) []string {
	ids := map[string]struct{}{}
	for _, target := range experiment.Targets {
		for _, arn := range target.ResourceArns {
			ids[itn.ARNToInstanceID(arn)] = struct{}{}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
