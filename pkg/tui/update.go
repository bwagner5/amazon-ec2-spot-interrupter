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
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
	case spotInstancesMsg:
		m.loading = false
		m.initialized = true
		m.queryingRegions = false
		m.instances = msg.instances
		m.updateRegionStats(msg.instances)
		if m.queryTotal == 0 {
			m.queryTotal = 1
		}
		m.queryCompleted = m.queryTotal
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
		m.updateRegionChoices()
		m.pruneSelection()
		m.syncRows()
		if msg.err != nil {
			m.listingError = msg.err
			if len(m.instances) == 0 {
				m.status = fmt.Sprintf("Failed to list Spot instances: %v", msg.err)
			} else {
				m.status = fmt.Sprintf("Loaded %d Spot instances with region errors: %v", len(m.instances), msg.err)
			}
		} else {
			m.listingError = nil
			if len(m.instances) == 0 {
				m.status = "No Spot instances found in selected scope"
			} else {
				m.status = fmt.Sprintf("Loaded %d Spot instances", len(m.instances))
			}
		}
		m.lastRefresh = time.Now()
		return m, scheduleRefresh()
	case regionProgressMsg:
		m.queryCompleted = msg.p.Completed
		m.queryTotal = msg.p.Total
		m.status = fmt.Sprintf("Querying Spot instances across regions... %d/%d (%s)", m.queryCompleted, m.queryTotal, msg.p.Region)
		return m, listenRegionProgress(msg.ch)
	case regionProgressDone:
		m.queryingRegions = false
		return m, nil
	case regionChoicesMsg:
		m.regionModalBusy = false
		if msg.err != nil {
			m.status = fmt.Sprintf("Failed to list regions: %v", msg.err)
			return m, nil
		}
		m.regionChoices = msg.labels
		m.regionValues = msg.values
		if m.regionCursor >= len(m.regionChoices) {
			m.regionCursor = 0
		}
		return m, nil
	case retrySpotInstances:
		m.loading = true
		m.status = "Refreshing Spot instance list..."
		return m, m.startLoadCmd()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.syncRows()
		return m, cmd
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.showChaosModal {
			updated, cmd := m.handleChaosModalKey(msg)
			return updated, cmd
		}
		if m.showTagModal {
			updated, cmd := m.handleTagModalKey(msg)
			return updated, cmd
		}
		if m.showRegionModal {
			updated, cmd := m.handleRegionModalKey(msg)
			return updated, cmd
		}
		if m.searching {
			updated, cmd := m.handleSearchKey(msg)
			return updated, cmd
		}
		updated, cmd := m.handleListKey(msg)
		return updated, cmd
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) handleChaosModalKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.showChaosModal = false
		m.chaosConfirming = false
		m.chaosMaxInput.Blur()
		m.chaosWaitInput.Blur()
		return m, nil
	}

	snapshot := m.chaos.Snapshot()
	if snapshot.Running {
		if msg.String() == "s" {
			m.chaos.Stop()
			return m, nil
		}
		return m, nil
	}

	if m.chaosConfirming {
		switch strings.ToLower(msg.String()) {
		case "y", "enter":
			maxN, minWait, err := m.parseChaosInputs()
			if err != nil {
				m.status = fmt.Sprintf("Invalid chaos settings: %v", err)
				m.chaosConfirming = false
				return m, nil
			}
			discover := m.chaosDiscoverFunc()
			filterFn := m.chaosFilterFunc()
			if err := m.chaos.Start(m.ctx, m.itn, m.hub, discover, filterFn, maxN, minWait, true); err != nil {
				m.status = fmt.Sprintf("Chaos failed to start: %v", err)
			} else {
				m.status = "Chaos mode started"
				m.showChaosModal = false
			}
			m.chaosConfirming = false
			return m, nil
		case "n", "esc":
			m.chaosConfirming = false
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "tab", "right":
		m.chaosFocus = 1
		m.chaosMaxInput.Blur()
		m.chaosWaitInput.Focus()
		return m, nil
	case "left", "shift+tab":
		m.chaosFocus = 0
		m.chaosWaitInput.Blur()
		m.chaosMaxInput.Focus()
		return m, nil
	case "enter":
		if _, _, err := m.parseChaosInputs(); err != nil {
			m.status = fmt.Sprintf("Invalid chaos settings: %v", err)
			return m, nil
		}
		m.chaosConfirming = true
		return m, nil
	}

	var cmd tea.Cmd
	if m.chaosFocus == 0 {
		m.chaosMaxInput, cmd = m.chaosMaxInput.Update(msg)
	} else {
		m.chaosWaitInput, cmd = m.chaosWaitInput.Update(msg)
	}
	return m, cmd
}

func (m model) handleTagModalKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.showTagModal = false
		m.tagKeyInput.Blur()
		m.tagValueInput.Blur()
		return m, nil
	case "tab":
		if m.tagFocus == 0 && len(m.tagSuggestions) > 0 && m.tagCursor >= 0 && m.tagCursor < len(m.tagSuggestions) {
			m.tagKeyInput.SetValue(m.tagSuggestions[m.tagCursor])
		}
		m.tagFocus = 1
		m.tagKeyInput.Blur()
		m.tagValueInput.Focus()
		m.updateTagSuggestions()
		return m, nil
	case "shift+tab":
		m.tagFocus = 0
		m.tagValueInput.Blur()
		m.tagKeyInput.Focus()
		m.updateTagSuggestions()
		return m, nil
	case "up":
		if m.tagCursor > 0 {
			m.tagCursor--
		}
		return m, nil
	case "down":
		if m.tagCursor < len(m.tagSuggestions)-1 {
			m.tagCursor++
		}
		return m, nil
	case "enter":
		if len(m.tagSuggestions) > 0 && m.tagCursor >= 0 && m.tagCursor < len(m.tagSuggestions) {
			if m.tagFocus == 0 && strings.TrimSpace(m.tagKeyInput.Value()) != m.tagSuggestions[m.tagCursor] {
				m.tagKeyInput.SetValue(m.tagSuggestions[m.tagCursor])
			} else if m.tagFocus == 1 && strings.TrimSpace(m.tagValueInput.Value()) != m.tagSuggestions[m.tagCursor] {
				m.tagValueInput.SetValue(m.tagSuggestions[m.tagCursor])
			}
		}
		enteredKey := strings.TrimSpace(m.tagKeyInput.Value())
		enteredValue := strings.TrimSpace(m.tagValueInput.Value())
		if enteredKey == "" {
			enteredKey = "*"
		}
		if enteredValue == "" {
			enteredValue = "*"
		}
		if enteredKey == "*" {
			enteredValue = "*"
		}
		if enteredKey == "*" && enteredValue == "*" {
			m.tagFilterKey = ""
			m.tagFilterValue = ""
		} else {
			m.tagFilterKey = enteredKey
			if enteredValue == "*" {
				m.tagFilterValue = ""
			} else {
				m.tagFilterValue = enteredValue
			}
		}
		m.showTagModal = false
		m.tagKeyInput.Blur()
		m.tagValueInput.Blur()
		m.syncRows()
		if m.tagFilterKey == "" {
			m.status = "Tag filter cleared"
		} else if m.tagFilterValue == "" {
			m.status = fmt.Sprintf("Tag filter applied: key=%s", m.tagFilterKey)
		} else {
			m.status = fmt.Sprintf("Tag filter applied: %s=%s", m.tagFilterKey, m.tagFilterValue)
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.tagFocus == 0 {
		m.tagKeyInput, cmd = m.tagKeyInput.Update(msg)
	} else {
		m.tagValueInput, cmd = m.tagValueInput.Update(msg)
	}
	m.updateTagSuggestions()
	return m, cmd
}

func (m model) handleRegionModalKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		if m.requireRegionSelection {
			return m, nil
		}
		m.showRegionModal = false
		return m, nil
	case "up", "k":
		if m.regionCursor > 0 {
			m.regionCursor--
		}
		return m, nil
	case "down", "j":
		if m.regionCursor < len(m.regionChoices)-1 {
			m.regionCursor++
		}
		return m, nil
	case " ":
		if len(m.regionChoices) == 0 || m.regionModalBusy {
			return m, nil
		}
		if m.regionSelected == nil {
			m.regionSelected = map[int]struct{}{}
		}
		choice := m.regionValues[m.regionCursor]
		if choice == "GLOBAL" {
			// Global toggles all non-GLOBAL entries
			allSelected := true
			for i := 1; i < len(m.regionValues); i++ {
				if _, ok := m.regionSelected[i]; !ok {
					allSelected = false
					break
				}
			}
			if allSelected {
				m.regionSelected = map[int]struct{}{}
			} else {
				for i := 1; i < len(m.regionValues); i++ {
					m.regionSelected[i] = struct{}{}
				}
			}
		} else {
			if _, ok := m.regionSelected[m.regionCursor]; ok {
				delete(m.regionSelected, m.regionCursor)
			} else {
				m.regionSelected[m.regionCursor] = struct{}{}
			}
		}
		return m, nil
	case "x":
		// Clear all selections
		m.regionSelected = map[int]struct{}{}
		return m, nil
	case "enter":
		if len(m.regionChoices) == 0 || m.regionModalBusy || len(m.regionSelected) == 0 {
			return m, nil
		}
		m.showRegionModal = false
		m.requireRegionSelection = false
		// Check if all non-GLOBAL regions are selected
		allSelected := true
		for i := 1; i < len(m.regionValues); i++ {
			if _, ok := m.regionSelected[i]; !ok {
				allSelected = false
				break
			}
		}
		if allSelected {
			m.globalMode = true
			m.activeRegions = nil
		} else {
			m.globalMode = false
			m.activeRegions = nil
			for i := 1; i < len(m.regionValues); i++ {
				if _, ok := m.regionSelected[i]; ok {
					m.activeRegions = append(m.activeRegions, m.regionValues[i])
				}
			}
		}
		m.loading = true
		return m, m.startLoadCmd()
	}
	return m, nil
}

func (m model) handleSearchKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.searching = false
		m.searchInput.Blur()
		return m, nil
	case "enter":
		m.searching = false
		m.searchInput.Blur()
		m.filter = m.searchInput.Value()
		m.syncRows()
		return m, nil
	}

	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.filter = m.searchInput.Value()
	m.syncRows()
	return m, cmd
}

func (m model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		m.toggleSelectAllVisible()
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
		return m, m.startLoadCmd()
	case key.Matches(msg, m.keys.RegionModal):
		m.showRegionModal = true
		m.regionCursor = 0
		m.regionSelected = map[int]struct{}{}
		m.regionModalBusy = true
		m.regionChoices = nil
		m.regionValues = nil
		counts, queried := m.regionStatsSnapshot()
		return m, loadRegionChoices(m.ctx, m.itn, counts, queried, m.queriedGlobal)
	case key.Matches(msg, m.keys.Chaos):
		m.showChaosModal = true
		m.chaosConfirming = false
		m.initChaosDefaults()
		m.chaosFocus = 0
		m.chaosMaxInput.Focus()
		m.chaosWaitInput.Blur()
		return m, nil
	case key.Matches(msg, m.keys.TagFilter):
		m.showTagModal = true
		m.tagFocus = 0
		m.tagCursor = 0
		keyValue := m.tagFilterKey
		valueValue := m.tagFilterValue
		if strings.TrimSpace(keyValue) == "" {
			keyValue = "*"
		}
		if strings.TrimSpace(valueValue) == "" {
			valueValue = "*"
		}
		m.tagKeyInput.SetValue(keyValue)
		m.tagValueInput.SetValue(valueValue)
		m.tagKeyInput.Focus()
		m.tagValueInput.Blur()
		m.updateTagSuggestions()
		return m, nil
	case key.Matches(msg, m.keys.Search):
		m.searching = true
		m.searchInput.Focus()
		m.searchInput.SetValue(m.filter)
		return m, nil
	case key.Matches(msg, m.keys.Open):
		selectedInstances := m.selectedInstances()
		if len(selectedInstances) == 0 {
			m.status = "Select at least one running Spot instance"
			return m, nil
		}
		opts := NewOptions(m.ctx, m.itn, m.hub, m, selectedInstances)
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
		monitor := NewMonitor(m.ctx, m.itn, m.hub, m, id)
		return monitor, monitor.Init()
	}

	return m, nil
}
