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
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const refreshInterval = 10 * time.Second

var (
	frameStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
)

type listKeyMap struct {
	Up        key.Binding
	Down      key.Binding
	Select    key.Binding
	SelectAll key.Binding
	Clear     key.Binding
	Refresh   key.Binding
	Monitor   key.Binding
	Open      key.Binding
	Quit      key.Binding
}

func (k listKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Select, k.Open, k.Monitor, k.Quit}
}

func (k listKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.Select, k.SelectAll}, {k.Clear, k.Refresh, k.Monitor, k.Open, k.Quit}}
}

func defaultListKeys() listKeyMap {
	return listKeyMap{
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Select:    key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		SelectAll: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "select all running")),
		Clear:     key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "clear")),
		Refresh:   key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Monitor:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "open experiment")),
		Open:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "interrupt")),
		Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

type model struct {
	instances    []ec2types.Instance
	selected     map[string]struct{}
	ctx          context.Context
	itn          *itn.ITN
	initialized  bool
	spinner      spinner.Model
	help         help.Model
	keys         listKeyMap
	table        table.Model
	status       string
	lastRefresh  time.Time
	loading      bool
	listingError error
	width        int
	height       int
	hub          *experimentHub
}

type spotInstancesMsg struct {
	instances []ec2types.Instance
	err       error
}

type retrySpotInstances time.Time

func NewModel(ctx context.Context, itnClient *itn.ITN) model {
	return newModelWithHub(ctx, itnClient, newExperimentHub())
}

func newModelWithHub(ctx context.Context, itnClient *itn.ITN, hub *experimentHub) model {
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("206"))
	sp.Spinner = spinner.Points

	tbl := table.New(
		table.WithColumns([]table.Column{
			{Title: "SEL", Width: 5},
			{Title: "INSTANCE", Width: 20},
			{Title: "NAME", Width: 24},
			{Title: "STATE", Width: 12},
			{Title: "AZ", Width: 11},
			{Title: "TYPE", Width: 13},
			{Title: "EXP(A/T)", Width: 9},
			{Title: "PROGRESS", Width: 14},
			{Title: "EVENT", Width: 34},
		}),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Foreground(lipgloss.Color("86")).Bold(true)
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")).Bold(true)
	tbl.SetStyles(styles)

	h := help.New()
	h.ShowAll = false

	return model{
		selected: map[string]struct{}{},
		ctx:      ctx,
		itn:      itnClient,
		spinner:  sp,
		help:     h,
		keys:     defaultListKeys(),
		table:    tbl,
		status:   "Loading Spot instances...",
		loading:  true,
		hub:      hub,
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
	return tea.Batch(spinner.Tick, loadSpotInstances(m.ctx, m.itn), scheduleRefresh(), tea.WindowSize())
}

func (m *model) syncRows() {
	rows := make([]table.Row, 0, len(m.instances))
	for _, inst := range m.instances {
		id := instanceID(inst)
		sel := "[ ]"
		if _, ok := m.selected[id]; ok {
			sel = "[x]"
		}
		expStatus, progress, event := m.hub.RowStatus(id)
		rows = append(rows, table.Row{
			sel,
			id,
			truncate(instanceName(inst), 24),
			string(inst.State.Name),
			instanceAZ(inst),
			string(inst.InstanceType),
			expStatus,
			progress,
			truncate(event, 34),
		})
	}
	m.table.SetRows(rows)
	if len(rows) == 0 {
		m.table.SetCursor(0)
		return
	}
	if m.table.Cursor() >= len(rows) {
		m.table.SetCursor(len(rows) - 1)
	}
}

func (m *model) resize() {
	if m.width == 0 {
		m.width = 120
	}
	if m.height == 0 {
		m.height = 40
	}
	helpHeight := lipgloss.Height(m.help.View(m.keys))
	tableHeight := m.height - helpHeight - 7
	if tableHeight < 5 {
		tableHeight = 5
	}
	m.table.SetHeight(tableHeight)
	m.table.SetWidth(m.width - 4)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
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
		m.syncRows()
		if len(m.instances) == 0 {
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
		m.syncRows()
		return m, cmd
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Up):
			if m.table.Cursor() > 0 {
				m.table.SetCursor(m.table.Cursor() - 1)
			}
			return m, nil
		case key.Matches(msg, m.keys.Down):
			if m.table.Cursor() < len(m.table.Rows())-1 {
				m.table.SetCursor(m.table.Cursor() + 1)
			}
			return m, nil
		case key.Matches(msg, m.keys.Select):
			m.toggleSelectionAtCursor()
			m.syncRows()
			return m, nil
		case key.Matches(msg, m.keys.SelectAll):
			m.selectAllRunnable()
			m.syncRows()
			return m, nil
		case key.Matches(msg, m.keys.Clear):
			m.selected = map[string]struct{}{}
			m.status = "Selection cleared"
			m.syncRows()
			return m, nil
		case key.Matches(msg, m.keys.Refresh):
			m.loading = true
			m.status = "Refreshing Spot instance list..."
			return m, loadSpotInstances(m.ctx, m.itn)
		case key.Matches(msg, m.keys.Open):
			selectedInstances := m.selectedInstances()
			if len(selectedInstances) == 0 {
				m.status = "Select at least one running Spot instance"
				return m, nil
			}
			opts := NewOptions(m.ctx, m.itn, m.hub, selectedInstances)
			return opts, opts.Init()
		case key.Matches(msg, m.keys.Monitor):
			id := m.hub.LatestID(true)
			if id == "" {
				id = m.hub.LatestID(false)
			}
			if id == "" {
				m.status = "No experiments yet. Start one with enter."
				return m, nil
			}
			monitor := NewMonitor(m.ctx, m.itn, m.hub, id)
			return monitor, monitor.Init()
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
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
	rowIdx := m.table.Cursor()
	if rowIdx < 0 || rowIdx >= len(m.instances) {
		return
	}
	inst := m.instances[rowIdx]
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

func truncate(v string, width int) string {
	if len(v) <= width {
		return v
	}
	if width <= 3 {
		return v[:width]
	}
	return v[:width-3] + "..."
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		m.width, m.height = 120, 40
		m.resize()
	}
	if !m.initialized {
		loading := fmt.Sprintf("Loading Spot instances %s", m.spinner.View())
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, loading)
	}

	selectedCount := len(m.selectedInstances())
	runningCount := 0
	for _, inst := range m.instances {
		if isRunnable(inst) {
			runningCount++
		}
	}
	status := m.status
	if m.loading {
		status = status + " " + m.spinner.View()
	}
	if !m.lastRefresh.IsZero() {
		status += fmt.Sprintf("  | last refresh %s", m.lastRefresh.Format("15:04:05"))
	}
	if m.listingError != nil {
		status += fmt.Sprintf("  | error: %v", m.listingError)
	}

	header := titleStyle.Render("EC2 Spot Interrupter") + "\n" +
		fmt.Sprintf("instances=%d running=%d selected=%d active-experiments=%d\n", len(m.instances), runningCount, selectedCount, m.hub.RunningCount()) +
		status

	content := frameStyle.Width(m.width - 2).Render(m.table.View())
	helpView := m.help.View(m.keys)

	ui := strings.Join([]string{header, "", content, helpView}, "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, ui)
}
