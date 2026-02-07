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
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render

const refreshInterval = 10 * time.Second
const tableWindowSize = 18

type model struct {
	instances    []ec2types.Instance
	cursor       int
	selected     map[string]struct{}
	ctx          context.Context
	itn          *itn.ITN
	initialized  bool
	spinner      spinner.Model
	status       string
	lastRefresh  time.Time
	loading      bool
	listingError error
}

type spotInstancesMsg struct {
	instances []ec2types.Instance
	err       error
}
type retrySpotInstances time.Time

func NewModel(ctx context.Context, itnClient *itn.ITN) model {
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("206"))
	sp.Spinner = spinner.Points
	return model{
		selected: map[string]struct{}{},
		ctx:      ctx,
		itn:      itnClient,
		spinner:  sp,
		status:   "Loading Spot instances...",
		loading:  true,
	}
}

func loadSpotInstances(ctx context.Context, itnClient *itn.ITN) tea.Cmd {
	return func() tea.Msg {
		instances, err := itnClient.SpotInstances(ctx)
		return spotInstancesMsg{instances: instances, err: err}
	}
}

func scheduleRefresh() tea.Cmd {
	return tea.Every(refreshInterval, func(t time.Time) tea.Msg {
		return retrySpotInstances(t)
	})
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		spinner.Tick,
		loadSpotInstances(m.ctx, m.itn),
		scheduleRefresh(),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spotInstancesMsg:
		m.loading = false
		m.initialized = true
		if msg.err != nil {
			m.listingError = msg.err
			m.status = fmt.Sprintf("Failed to list Spot instances: %v", msg.err)
			return m, scheduleRefresh()
		}
		m.listingError = nil
		m.instances = msg.instances
		sort.Slice(m.instances, func(i, j int) bool {
			leftID := instanceID(m.instances[i])
			rightID := instanceID(m.instances[j])
			leftState := string(m.instances[i].State.Name)
			rightState := string(m.instances[j].State.Name)
			if leftState != rightState {
				return leftState < rightState
			}
			return leftID < rightID
		})
		m.pruneSelection()
		if m.cursor >= len(m.instances) && len(m.instances) > 0 {
			m.cursor = len(m.instances) - 1
		}
		if len(m.instances) == 0 {
			m.cursor = 0
			m.status = "No Spot instances found in this account/region"
		} else {
			m.status = fmt.Sprintf("Loaded %d Spot instances", len(m.instances))
		}
		m.lastRefresh = time.Now()
		return m, scheduleRefresh()
	case retrySpotInstances:
		m.loading = true
		return m, loadSpotInstances(m.ctx, m.itn)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.instances)-1 {
				m.cursor++
			}
		case "g":
			m.cursor = 0
		case "G":
			if len(m.instances) > 0 {
				m.cursor = len(m.instances) - 1
			}
		case " ":
			m.toggleSelectionAtCursor()
		case "a":
			m.selectAllRunnable()
		case "x":
			m.selected = map[string]struct{}{}
			m.status = "Selection cleared"
		case "r":
			m.loading = true
			m.status = "Refreshing Spot instance list..."
			return m, loadSpotInstances(m.ctx, m.itn)
		case "enter":
			selectedInstances := m.selectedInstances()
			if len(selectedInstances) == 0 {
				m.status = "Select at least one running Spot instance"
				return m, nil
			}
			opts := NewOptions(m.ctx, m.itn, selectedInstances)
			return opts, opts.Init()
		}
	}
	return m, nil
}

func (m *model) pruneSelection() {
	available := map[string]struct{}{}
	for _, inst := range m.instances {
		available[instanceID(inst)] = struct{}{}
	}
	for id := range m.selected {
		if _, ok := available[id]; !ok {
			delete(m.selected, id)
		}
	}
}

func (m *model) toggleSelectionAtCursor() {
	if len(m.instances) == 0 || m.cursor >= len(m.instances) {
		return
	}
	inst := m.instances[m.cursor]
	id := instanceID(inst)
	if !isRunnable(inst) {
		m.status = fmt.Sprintf("%s is %s (only running instances can be interrupted)", id, string(inst.State.Name))
		return
	}
	if _, ok := m.selected[id]; ok {
		delete(m.selected, id)
		m.status = fmt.Sprintf("Unselected %s", id)
		return
	}
	m.selected[id] = struct{}{}
	m.status = fmt.Sprintf("Selected %s", id)
}

func (m *model) selectAllRunnable() {
	count := 0
	for _, inst := range m.instances {
		if isRunnable(inst) {
			m.selected[instanceID(inst)] = struct{}{}
			count++
		}
	}
	m.status = fmt.Sprintf("Selected %d running Spot instances", count)
}

func (m model) selectedInstances() []*ec2types.Instance {
	instances := []*ec2types.Instance{}
	for i := range m.instances {
		id := instanceID(m.instances[i])
		if _, ok := m.selected[id]; ok && isRunnable(m.instances[i]) {
			instances = append(instances, &m.instances[i])
		}
	}
	return instances
}

func instanceName(i ec2types.Instance) string {
	for _, tag := range i.Tags {
		if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
			return *tag.Value
		}
	}
	return "-"
}

func instanceID(i ec2types.Instance) string {
	if i.InstanceId == nil {
		return "-"
	}
	return *i.InstanceId
}

func instanceAZ(i ec2types.Instance) string {
	if i.Placement.AvailabilityZone == nil {
		return "-"
	}
	return *i.Placement.AvailabilityZone
}

func isRunnable(i ec2types.Instance) bool {
	return i.State.Name == ec2types.InstanceStateNameRunning
}

func checked(isSelected bool) string {
	if isSelected {
		return "[x]"
	}
	return "[ ]"
}

func truncate(v string, width int) string {
	if len(v) <= width {
		return v
	}
	if width <= 3 {
		return v[:width]
	}
	return v[:width-3] + "..."
}

func (m model) tableRange() (int, int) {
	if len(m.instances) <= tableWindowSize {
		return 0, len(m.instances)
	}
	start := m.cursor - (tableWindowSize / 2)
	if start < 0 {
		start = 0
	}
	end := start + tableWindowSize
	if end > len(m.instances) {
		end = len(m.instances)
		start = end - tableWindowSize
	}
	return start, end
}

func (m model) View() string {
	if !m.initialized {
		return fmt.Sprintf("Loading Spot instances %s\n%s", m.spinner.View(), help())
	}

	var b strings.Builder
	selectedCount := len(m.selectedInstances())
	runnableCount := 0
	for _, inst := range m.instances {
		if isRunnable(inst) {
			runnableCount++
		}
	}

	statusSuffix := ""
	if m.loading {
		statusSuffix = " " + m.spinner.View()
	}
	if !m.lastRefresh.IsZero() {
		statusSuffix += fmt.Sprintf("  last refresh: %s", m.lastRefresh.Format("15:04:05"))
	}

	b.WriteString(fmt.Sprintf("Spot Interrupter  instances=%d running=%d selected=%d%s\n", len(m.instances), runnableCount, selectedCount, statusSuffix))
	b.WriteString(fmt.Sprintf("status: %s\n\n", m.status))

	if m.listingError != nil {
		b.WriteString(fmt.Sprintf("error: %v\n\n", m.listingError))
	}

	b.WriteString("    SEL  INSTANCE ID         NAME                       STATE         AZ         TYPE\n")
	b.WriteString("    ---  ------------------  -------------------------  ------------  ---------  -------------\n")

	start, end := m.tableRange()
	for i := start; i < end; i++ {
		inst := m.instances[i]
		cursor := " "
		if m.cursor == i {
			cursor = ">"
		}
		id := instanceID(inst)
		_, isSelected := m.selected[id]
		b.WriteString(fmt.Sprintf(
			"%s   %s  %-18s  %-25s  %-12s  %-9s  %-13s\n",
			cursor,
			checked(isSelected),
			id,
			truncate(instanceName(inst), 25),
			string(inst.State.Name),
			instanceAZ(inst),
			string(inst.InstanceType),
		))
	}

	if len(m.instances) > tableWindowSize {
		b.WriteString(fmt.Sprintf("\nshowing %d-%d of %d\n", start+1, end, len(m.instances)))
	}
	b.WriteString(help())
	return b.String()
}

func help() string {
	return helpStyle("\nKeys: j/k or arrows move | space select | a select running | x clear | r refresh | enter interrupt | q quit\n")
}
