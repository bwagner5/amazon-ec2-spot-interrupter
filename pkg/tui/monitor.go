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
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type monitorKeyMap struct {
	Back key.Binding
	Next key.Binding
	Prev key.Binding
	Stop key.Binding
	Quit key.Binding
}

func (k monitorKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Back, k.Next, k.Prev, k.Stop, k.Quit}
}

func (k monitorKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Back, k.Next, k.Prev, k.Stop, k.Quit}}
}

func defaultMonitorKeys() monitorKeyMap {
	return monitorKeyMap{
		Back: key.NewBinding(key.WithKeys("b", "esc"), key.WithHelp("b/esc", "back to instances")),
		Next: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next experiment")),
		Prev: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "prev experiment")),
		Stop: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop experiment")),
		Quit: key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

type monitor struct {
	ctx          context.Context
	itn          *itn.ITN
	hub          *experimentHub
	back         tea.Model
	experimentID string
	spinner      spinner.Model
	width        int
	height       int
	help         help.Model
	keys         monitorKeyMap
	table        table.Model
	logs         viewport.Model
	snapshot     trackedExperiment
	haveData     bool
	status       string
}

type refreshMonitorMsg time.Time

func refreshMonitor() tea.Cmd {
	return tea.Every(time.Second, func(t time.Time) tea.Msg {
		return refreshMonitorMsg(t)
	})
}

func NewMonitor(ctx context.Context, itnClient *itn.ITN, hub *experimentHub, back tea.Model, experimentID string) monitor {
	sp := spinner.New()
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("206"))
	sp.Spinner = spinner.Points

	tbl := table.New(
		table.WithColumns([]table.Column{
			{Title: "SEL", Width: 5},
			{Title: "INSTANCE", Width: 20},
			{Title: "NAME", Width: 28},
			{Title: "STATE", Width: 12},
			{Title: "AZ", Width: 12},
			{Title: "TYPE", Width: 14},
			{Title: "PROGRESS", Width: 18},
		}),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Foreground(lipgloss.Color("86")).Bold(true)
	tbl.SetStyles(styles)

	vp := viewport.New(10, 10)
	vp.SetContent("Waiting for events...")

	h := help.New()
	h.ShowAll = false

	m := monitor{
		ctx:          ctx,
		itn:          itnClient,
		hub:          hub,
		back:         back,
		experimentID: experimentID,
		spinner:      sp,
		help:         h,
		keys:         defaultMonitorKeys(),
		table:        tbl,
		logs:         vp,
	}
	m.pullSnapshot()
	m.syncRows()
	m.syncLogs()
	return m
}

func (m monitor) Init() tea.Cmd {
	return tea.Batch(spinner.Tick, tea.WindowSize(), refreshMonitor())
}

func (m *monitor) pullSnapshot() {
	snapshot, ok := m.hub.Snapshot(m.experimentID)
	m.haveData = ok
	if ok {
		m.snapshot = snapshot
	}
}

func (m *monitor) resize() {
	if m.width == 0 {
		m.width = 120
	}
	if m.height == 0 {
		m.height = 40
	}
	helpHeight := lipgloss.Height(m.help.View(m.keys))
	tableHeight := (m.height / 2) - 4
	if tableHeight < 6 {
		tableHeight = 6
	}
	logHeight := m.height - tableHeight - helpHeight - 8
	if logHeight < 4 {
		logHeight = 4
	}
	m.table.SetHeight(tableHeight)
	m.table.SetWidth(m.width - 4)
	m.logs.Width = m.width - 4
	m.logs.Height = logHeight
}

func (m monitor) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		m.syncLogs()
		return m, nil
	case spinner.TickMsg:
		if m.haveData && !m.snapshot.Done {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case refreshMonitorMsg:
		m.pullSnapshot()
		m.syncRows()
		m.syncLogs()
		return m, refreshMonitor()
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Back):
			if m.back != nil {
				return resumeInstancesView(m.back)
			}
			return m, nil
		case key.Matches(msg, m.keys.Next):
			done := false
			if m.haveData {
				done = m.snapshot.Done
			}
			next := m.hub.NextIDInCohort(m.experimentID, done)
			if next == "" {
				next = m.hub.NextID(m.experimentID)
			}
			if next != "" {
				m.experimentID = next
				m.pullSnapshot()
				m.syncRows()
				m.syncLogs()
			}
			return m, nil
		case key.Matches(msg, m.keys.Prev):
			done := false
			if m.haveData {
				done = m.snapshot.Done
			}
			prev := m.hub.PrevIDInCohort(m.experimentID, done)
			if prev == "" {
				prev = m.hub.PrevID(m.experimentID)
			}
			if prev != "" {
				m.experimentID = prev
				m.pullSnapshot()
				m.syncRows()
				m.syncLogs()
			}
			return m, nil
		case key.Matches(msg, m.keys.Stop):
			if !m.haveData {
				m.status = "Experiment data not available"
				return m, nil
			}
			if m.snapshot.Done {
				m.status = "Experiment already completed"
				return m, nil
			}
			if err := m.itn.StopExperiment(m.ctx, m.experimentID); err != nil {
				m.status = fmt.Sprintf("Failed to stop experiment: %v", err)
			} else {
				m.status = fmt.Sprintf("Stopping experiment %s...", m.experimentID)
			}
			m.pullSnapshot()
			m.syncRows()
			m.syncLogs()
			return m, nil
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *monitor) syncRows() {
	rows := []table.Row{}
	if !m.haveData {
		m.table.SetRows(rows)
		return
	}
	for _, id := range m.snapshot.OrderedIDs {
		inst := m.snapshot.Selected[id]
		state := m.snapshot.States[id]
		if state == "" {
			state = string(inst.State.Name)
		}
		if state == "" {
			state = "-"
		}
		rows = append(rows, table.Row{
			"[x]",
			id,
			truncate(instanceName(inst), 28),
			state,
			instanceAZ(inst),
			string(inst.InstanceType),
			progressLabel(m.snapshot.Progress[id]),
		})
	}
	m.table.SetRows(rows)
}

func (m *monitor) syncLogs() {
	lines := []string{}
	if !m.haveData {
		lines = append(lines, "Experiment data not available.")
		m.logs.SetContent(strings.Join(lines, "\n"))
		return
	}
	seen := map[string]struct{}{}
	for _, e := range m.snapshot.EventLog {
		instanceRef, instanceKey := m.eventInstanceRef(e)
		dedupeKey := fmt.Sprintf("%s|%s|%s", e.Stage, instanceKey, strings.TrimSpace(e.Message))
		if _, ok := seen[dedupeKey]; ok {
			continue
		}
		seen[dedupeKey] = struct{}{}
		lines = append(lines, fmt.Sprintf("%s  [%s] %s", e.Timestamp.Format("15:04:05"), instanceRef, e.Message))
	}
	if len(lines) == 0 {
		lines = append(lines, "Waiting for events...")
	}
	if m.snapshot.Done {
		lines = append(lines, "", "Experiment stream completed. Press b to go back.")
	}
	m.logs.SetContent(strings.Join(lines, "\n"))
	m.logs.GotoBottom()
}

func (m monitor) eventInstanceRef(e itn.Event) (string, string) {
	if len(e.InstanceStates) == 0 {
		return "all", "all"
	}
	ids := make([]string, 0, len(e.InstanceStates))
	for id := range e.InstanceStates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == len(m.snapshot.OrderedIDs) && len(ids) > 0 {
		all := true
		for idx := range ids {
			if ids[idx] != m.snapshot.OrderedIDs[idx] {
				all = false
				break
			}
		}
		if all {
			return "all", "all"
		}
	}
	return strings.Join(ids, ","), strings.Join(ids, ",")
}

func (m monitor) View() string {
	if m.width == 0 || m.height == 0 {
		m.width, m.height = 120, 40
		m.resize()
	}
	header := titleStyle.Render("EC2 Spot Interrupter") + "\n"
	if m.haveData {
		completedCount := m.hub.CompletedCount()
		pos, total := m.hub.PositionInCohort(m.experimentID, m.snapshot.Done)
		if m.snapshot.Done {
			header += fmt.Sprintf("completed experiments=%d  viewing=%d/%d", completedCount, pos, total)
		} else {
			header += fmt.Sprintf("active experiments=%d  viewing=%d/%d  completed experiments=%d", m.hub.RunningCount(), pos, total, completedCount)
		}
		header += fmt.Sprintf("\nwatching experiment=%s selected=%d", m.snapshot.ID, len(m.snapshot.OrderedIDs))
		if !m.snapshot.Done {
			header += "  " + m.spinner.View()
		}
	} else {
		header += fmt.Sprintf("watching experiment=%s", m.experimentID)
	}
	if strings.TrimSpace(m.status) != "" {
		header += "\n" + m.status
	}

	tablePanel := frameStyle.Width(m.width - 2).Render(m.table.View())
	logPanel := frameStyle.Width(m.width - 2).Render(m.logs.View())
	ui := strings.Join([]string{header, "", tablePanel, "Events", logPanel, m.help.View(m.keys)}, "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, ui)
}
