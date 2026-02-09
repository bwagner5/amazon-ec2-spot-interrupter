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
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const refreshInterval = 10 * time.Second

var (
	frameStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
)

type listKeyMap struct {
	Up          key.Binding
	Down        key.Binding
	Select      key.Binding
	SelectAll   key.Binding
	Clear       key.Binding
	Refresh     key.Binding
	Search      key.Binding
	Monitor     key.Binding
	Global      key.Binding
	RegionModal key.Binding
	TagFilter   key.Binding
	Chaos       key.Binding
	Open        key.Binding
	Quit        key.Binding
}

func (k listKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Select, k.SelectAll, k.Open, k.Search, k.TagFilter, k.RegionModal, k.Chaos, k.Quit}
}

func (k listKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.Select, k.SelectAll}, {k.Clear, k.Refresh, k.Search, k.TagFilter, k.Chaos, k.Monitor, k.Global, k.RegionModal, k.Open, k.Quit}}
}

func defaultListKeys() listKeyMap {
	return listKeyMap{
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Select:      key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		SelectAll:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "select all visible")),
		Clear:       key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "clear")),
		Refresh:     key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "refresh")),
		Search:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Monitor:     key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "open experiment")),
		Global:      key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "query global")),
		RegionModal: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "region filter")),
		TagFilter:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tag filter")),
		Chaos:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "chaos mode")),
		Open:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "interrupt")),
		Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

type model struct {
	instances       []ec2types.Instance
	selected        map[string]struct{}
	ctx             context.Context
	itn             *itn.ITN
	initialized     bool
	spinner         spinner.Model
	help            help.Model
	keys            listKeyMap
	table           table.Model
	status          string
	lastRefresh     time.Time
	loading         bool
	listingError    error
	width           int
	height          int
	hub             *experimentHub
	searchInput     textinput.Model
	searching       bool
	filter          string
	filteredIdxs    []int
	eventWidth      int
	nameWidth       int
	globalMode      bool
	activeRegions   []string
	regionChoices   []string
	regionCursor    int
	showRegionModal bool
	regionModalBusy bool
	queryCompleted  int
	queryTotal      int
	queryingRegions bool
	showTagModal    bool
	tagKeyInput     textinput.Model
	tagValueInput   textinput.Model
	tagFocus        int
	tagCursor       int
	tagSuggestions  []string
	tagFilterKey    string
	tagFilterValue  string
	regionValues    []string
	chaos           *chaosController
	showChaosModal  bool
	chaosMaxInput   textinput.Model
	chaosWaitInput  textinput.Model
	chaosFocus      int
	chaosConfirming bool
}

type spotInstancesMsg struct {
	instances []ec2types.Instance
	err       error
}

type retrySpotInstances time.Time

type regionProgressMsg struct {
	p  itn.RegionQueryProgress
	ch <-chan itn.RegionQueryProgress
}

type regionProgressDone struct {
	ch <-chan itn.RegionQueryProgress
}

type regionChoicesMsg struct {
	labels []string
	values []string
	err    error
}

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
			{Title: "EXP", Width: 9},
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

	search := textinput.New()
	search.CharLimit = 80
	search.Prompt = "/ "
	search.Width = 40

	tagKey := textinput.New()
	tagKey.Prompt = ""
	tagKey.CharLimit = 128
	tagKey.Width = 24

	tagValue := textinput.New()
	tagValue.Prompt = ""
	tagValue.CharLimit = 128
	tagValue.Width = 24

	chaosMax := textinput.New()
	chaosMax.CharLimit = 10
	chaosMax.Width = 8
	chaosWait := textinput.New()
	chaosWait.CharLimit = 20
	chaosWait.Width = 12

	global := strings.EqualFold(itnClient.Region(), "global")

	return model{
		selected:       map[string]struct{}{},
		ctx:            ctx,
		itn:            itnClient,
		spinner:        sp,
		help:           h,
		keys:           defaultListKeys(),
		table:          tbl,
		status:         "Loading Spot instances...",
		loading:        true,
		hub:            hub,
		searchInput:    search,
		nameWidth:      24,
		eventWidth:     34,
		globalMode:     global,
		tagKeyInput:    tagKey,
		tagValueInput:  tagValue,
		chaos:          newChaosController(),
		chaosMaxInput:  chaosMax,
		chaosWaitInput: chaosWait,
	}
}

func loadSpotInstances(ctx context.Context, itnClient *itn.ITN, global bool, regions []string, progress chan<- itn.RegionQueryProgress) tea.Cmd {
	return func() tea.Msg {
		defer func() {
			if progress != nil {
				close(progress)
			}
		}()

		if global {
			instances, err := itnClient.SpotInstancesGlobal(ctx, progress)
			return spotInstancesMsg{instances: instances, err: err}
		}
		if len(regions) > 0 {
			instances, err := itnClient.SpotInstancesInRegions(ctx, regions, progress)
			return spotInstancesMsg{instances: instances, err: err}
		}
		instances, err := itnClient.SpotInstances(ctx)
		return spotInstancesMsg{instances: instances, err: err}
	}
}

func loadRegionChoices(ctx context.Context, itnClient *itn.ITN, instances []ec2types.Instance) tea.Cmd {
	return func() tea.Msg {
		regions, err := itnClient.ListRegions(ctx)
		if err != nil {
			return regionChoicesMsg{err: err}
		}
		counts := map[string]int{}
		for _, inst := range instances {
			r := regionFromAZ(instanceAZ(inst))
			if r == "" || r == "-" {
				continue
			}
			counts[r]++
		}
		sort.Slice(regions, func(i, j int) bool {
			left := counts[regions[i]]
			right := counts[regions[j]]
			if left != right {
				return left > right
			}
			return regions[i] < regions[j]
		})
		labels := []string{"GLOBAL (all regions)"}
		values := []string{"GLOBAL"}
		for _, r := range regions {
			labels = append(labels, fmt.Sprintf("%s (%d)", r, counts[r]))
			values = append(values, r)
		}
		return regionChoicesMsg{labels: labels, values: values}
	}
}

func listenRegionProgress(ch <-chan itn.RegionQueryProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return regionProgressDone{ch: ch}
		}
		return regionProgressMsg{p: p, ch: ch}
	}
}

func scheduleRefresh() tea.Cmd {
	return tea.Every(refreshInterval, func(t time.Time) tea.Msg {
		return retrySpotInstances(t)
	})
}

func (m model) startLoadCmd() tea.Cmd {
	progress := make(chan itn.RegionQueryProgress, 16)
	if m.globalMode || len(m.activeRegions) > 0 {
		m.queryTotal = 0
	} else {
		m.queryTotal = 1
	}
	m.queryCompleted = 0
	m.queryingRegions = m.globalMode || len(m.activeRegions) > 0
	return tea.Batch(
		loadSpotInstances(m.ctx, m.itn, m.globalMode, m.activeRegions, progress),
		listenRegionProgress(progress),
	)
}

func (m model) Init() tea.Cmd {
	return tea.Batch(spinner.Tick, m.startLoadCmd(), scheduleRefresh(), tea.WindowSize())
}

func (m *model) syncRows() {
	rows := make([]table.Row, 0, len(m.instances))
	m.filteredIdxs = m.filteredIndices()
	for _, idx := range m.filteredIdxs {
		inst := m.instances[idx]
		id := instanceID(inst)
		sel := "[ ]"
		if _, ok := m.selected[id]; ok {
			sel = "[x]"
		}
		expStatus, progress, event := m.hub.RowStatus(id)
		rows = append(rows, table.Row{
			sel,
			id,
			truncate(instanceName(inst), m.nameWidth),
			string(inst.State.Name),
			instanceAZ(inst),
			string(inst.InstanceType),
			expStatus,
			progress,
			truncate(event, m.eventWidth),
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
	m.updateSelectAllHelp()
}

func (m *model) updateSelectAllHelp() {
	helpLabel := "select all visible"
	if m.allVisibleSelected() {
		helpLabel = "deselect all visible"
	}
	m.keys.SelectAll = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", helpLabel))
}

func (m *model) resize() {
	if m.width == 0 {
		m.width = 120
	}
	if m.height == 0 {
		m.height = 40
	}
	helpHeight := lipgloss.Height(m.help.View(m.keys))
	searchHeight := 0
	if m.searching || m.filter != "" {
		searchHeight = 2
	}
	tableHeight := m.height - helpHeight - searchHeight - 7
	if tableHeight < 5 {
		tableHeight = 5
	}
	m.table.SetHeight(tableHeight)
	m.table.SetWidth(m.width - 6)
	m.applyColumnWidths()
}

func (m *model) applyColumnWidths() {
	total := m.width - 8
	if total < 80 {
		return
	}
	fixed := 5 + 20 + 12 + 11 + 13 + 9 + 14
	remaining := total - fixed
	if remaining < 24 {
		remaining = 24
	}
	name := remaining / 3
	if name < 16 {
		name = 16
	}
	event := remaining - name
	if event < 20 {
		event = 20
	}
	m.nameWidth = name
	m.eventWidth = event
	m.table.SetColumns([]table.Column{
		{Title: "SEL", Width: 5},
		{Title: "INSTANCE", Width: 20},
		{Title: "NAME", Width: m.nameWidth},
		{Title: "STATE", Width: 12},
		{Title: "AZ", Width: 11},
		{Title: "TYPE", Width: 13},
		{Title: "EXP", Width: 9},
		{Title: "PROGRESS", Width: 14},
		{Title: "EVENT", Width: m.eventWidth},
	})
}

func (m *model) updateRegionChoices() {
	// Region choices are loaded from AWS region list via the modal command.
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
	if rowIdx < 0 || rowIdx >= len(m.filteredIdxs) {
		return
	}
	inst := m.instances[m.filteredIdxs[rowIdx]]
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

func (m *model) selectAllVisible() {
	count := 0
	for _, idx := range m.filteredIdxs {
		inst := m.instances[idx]
		id := instanceID(inst)
		m.selected[id] = struct{}{}
		count++
	}
	m.status = fmt.Sprintf("Selected %d visible Spot instances", count)
}

func (m *model) deselectAllVisible() {
	count := 0
	for _, idx := range m.filteredIdxs {
		id := instanceID(m.instances[idx])
		if _, ok := m.selected[id]; ok {
			delete(m.selected, id)
			count++
		}
	}
	m.status = fmt.Sprintf("Deselected %d visible Spot instances", count)
}

func (m *model) allVisibleSelected() bool {
	if len(m.filteredIdxs) == 0 {
		return false
	}
	for _, idx := range m.filteredIdxs {
		if _, ok := m.selected[instanceID(m.instances[idx])]; !ok {
			return false
		}
	}
	return true
}

func (m *model) toggleSelectAllVisible() {
	if m.allVisibleSelected() {
		m.deselectAllVisible()
		return
	}
	m.selectAllVisible()
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

func regionFromAZ(az string) string {
	if len(az) < 2 || az == "-" {
		return az
	}
	return az[:len(az)-1]
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
