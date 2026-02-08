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
	"strings"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/charmbracelet/lipgloss"
)

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		m.width, m.height = 120, 40
		m.resize()
	}
	if !m.initialized {
		loading := fmt.Sprintf("Loading Spot instances %s", m.spinner.View())
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, loading)
	}

	header := m.headerView()
	content := m.contentView()
	footer := m.footerView()

	parts := []string{header, ""}
	if search := m.searchView(); search != "" {
		parts = append(parts, search, "")
	}
	if m.showTagModal {
		parts = append(parts, "", m.tagFilterSectionView(), "")
	}
	parts = append(parts, content)

	ui := strings.Join(parts, "\n")
	ui = pinFooterToBottom(m.height, ui, footer)
	base := lipgloss.Place(m.width, m.height, lipgloss.Left, lipgloss.Top, ui)

	if m.showRegionModal {
		return m.overlayCentered(base, m.regionModalView())
	}
	if m.showChaosModal {
		return m.overlayCentered(base, m.chaosModalView())
	}
	return base
}

func (m model) headerView() string {
	selectedCount := len(m.selectedInstances())
	runningCount := 0
	for _, inst := range m.instances {
		if isRunnable(inst) {
			runningCount++
		}
	}

	header := titleStyle.Render("EC2 Spot Interrupter") + "\n" +
		fmt.Sprintf("scope=%s instances=%d running=%d selected=%d active-experiments=%d\n", m.scopeLabel(), len(m.instances), runningCount, selectedCount, m.hub.RunningCount()) +
		m.statusLine()
	if m.tagFilterKey != "" {
		if m.tagFilterValue != "" {
			header += fmt.Sprintf("\nTag filter: %s=%s", m.tagFilterKey, m.tagFilterValue)
		} else {
			header += fmt.Sprintf("\nTag filter: %s", m.tagFilterKey)
		}
	}
	return header
}

func (m model) scopeLabel() string {
	if m.globalMode {
		return "global"
	}
	if len(m.activeRegions) > 0 {
		return strings.Join(m.activeRegions, ",")
	}
	if r := m.itn.Region(); r != "" {
		return r
	}
	return "default"
}

func (m model) statusLine() string {
	status := m.status
	if m.loading {
		status += " " + m.spinner.View()
	}
	if !m.lastRefresh.IsZero() {
		status += fmt.Sprintf("  | last refresh %s", m.lastRefresh.Format("15:04:05"))
	}
	if m.listingError != nil {
		status += fmt.Sprintf("  | error: %v", m.listingError)
	}
	if chaos := m.chaos.Snapshot(); chaos.Running {
		status += "  | chaos running"
	}
	return status
}

func (m model) contentView() string {
	if m.showTagModal {
		return m.tagSuggestionsView()
	}
	return frameStyle.Width(m.width - 2).Render(m.table.View())
}

func (m model) searchView() string {
	if m.searching {
		return "Search: " + m.searchInput.View()
	}
	if m.filter != "" {
		return fmt.Sprintf("Filter: %q (press / to edit)", m.filter)
	}
	return ""
}

func (m model) footerView() string {
	switch {
	case m.showTagModal:
		return "Tag filter: enter apply | c clear | tab/right next | shift+tab/left previous | esc/backspace close"
	case m.showRegionModal:
		return "Region scope: enter apply | up/down move | esc/backspace close"
	case m.showChaosModal:
		snapshot := m.chaos.Snapshot()
		if snapshot.Running {
			return "Chaos mode: s stop | esc/backspace close"
		}
		if m.chaosConfirming {
			return "Chaos confirm: y start | n cancel"
		}
		return "Chaos config: enter continue | tab/shift+tab switch | esc/backspace close"
	default:
		return m.help.View(m.keys)
	}
}

func (m model) regionModalView() string {
	width := m.width - 12
	if width > 80 {
		width = 80
	}
	if width < 40 {
		width = 40
	}
	if m.regionModalBusy {
		return frameStyle.Width(width).Render("Loading regions...")
	}
	if len(m.regionChoices) == 0 {
		return frameStyle.Width(width).Render("No regions available")
	}
	lines := []string{"Region Scope", "enter apply | esc/backspace close", ""}
	for i, r := range m.regionChoices {
		prefix := "  "
		if i == m.regionCursor {
			prefix = "> "
		}
		lines = append(lines, prefix+r)
	}
	return frameStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func (m *model) initChaosDefaults() {
	running := 0
	for _, inst := range m.instances {
		if isRunnable(inst) {
			running++
		}
	}
	defMax := running / 3
	if defMax < 1 {
		defMax = 1
	}
	m.chaosMaxInput.SetValue(fmt.Sprintf("%d", defMax))
	m.chaosWaitInput.SetValue("5m")
}

func (m model) parseChaosInputs() (int, time.Duration, error) {
	maxStr := strings.TrimSpace(m.chaosMaxInput.Value())
	if maxStr == "" {
		return 0, 0, fmt.Errorf("max instances is required")
	}
	maxN := 0
	if _, err := fmt.Sscanf(maxStr, "%d", &maxN); err != nil || maxN < 1 {
		return 0, 0, fmt.Errorf("max instances must be a positive integer")
	}
	waitStr := strings.TrimSpace(m.chaosWaitInput.Value())
	minWait, err := time.ParseDuration(waitStr)
	if err != nil || minWait < 0 {
		return 0, 0, fmt.Errorf("min wait must be a valid duration (example: 5m)")
	}
	return maxN, minWait, nil
}

func (m model) chaosDiscoverFunc() func(context.Context) ([]ec2types.Instance, error) {
	global := m.globalMode
	regions := append([]string{}, m.activeRegions...)
	return func(ctx context.Context) ([]ec2types.Instance, error) {
		if global {
			return m.itn.SpotInstancesGlobal(ctx, nil)
		}
		if len(regions) > 0 {
			return m.itn.SpotInstancesInRegions(ctx, regions, nil)
		}
		return m.itn.SpotInstances(ctx)
	}
}

func (m model) chaosFilterFunc() func(ec2types.Instance) bool {
	tagKey := m.tagFilterKey
	tagVal := m.tagFilterValue
	searchQ := strings.ToLower(strings.TrimSpace(m.filter))
	return func(inst ec2types.Instance) bool {
		if !matchesTagFilter(inst, tagKey, tagVal) {
			return false
		}
		if searchQ == "" {
			return true
		}
		haystack := strings.ToLower(strings.Join([]string{
			instanceID(inst),
			instanceName(inst),
			string(inst.State.Name),
			instanceAZ(inst),
			string(inst.InstanceType),
		}, " "))
		return strings.Contains(haystack, searchQ)
	}
}

func (m model) chaosModalView() string {
	width := m.width - 12
	if width > 84 {
		width = 84
	}
	if width < 44 {
		width = 44
	}
	snapshot := m.chaos.Snapshot()
	lines := []string{"Random Chaos Mode"}
	if snapshot.Running {
		lines = append(lines, fmt.Sprintf("Running | max=%d min-wait=%s", snapshot.Max, snapshot.MinWait))
		if snapshot.Last != "" {
			lines = append(lines, snapshot.Last)
		}
		lines = append(lines, "", "Press 's' to stop chaos | esc/backspace close")
		return frameStyle.Width(width).Render(strings.Join(lines, "\n"))
	}

	lines = append(lines, fmt.Sprintf("[Max instances] %s    [Min wait] %s", m.chaosMaxInput.View(), m.chaosWaitInput.View()))
	lines = append(lines, "Behavior: random batch size between half-max and max, random wait between min and 2x min.")
	if m.chaosConfirming {
		lines = append(lines, "", "Confirm start randomized chaos? (y/n)")
	} else {
		lines = append(lines, "", "enter continue | tab/shift+tab switch field | esc/backspace close")
	}
	return frameStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func (m *model) tagFilterSectionView() string {
	lines := []string{"Tag Filter"}
	lines = append(lines, fmt.Sprintf("[Key] %s    [Value] %s", m.tagKeyInput.View(), m.tagValueInput.View()))
	return frameStyle.Width(m.width - 2).Render(strings.Join(lines, "\n"))
}

func (m *model) tagSuggestionsView() string {
	lines := []string{"Tag Suggestions"}
	if len(m.tagSuggestions) == 0 {
		lines = append(lines, "No matching tag suggestions")
	} else {
		maxRows := 20
		if m.height > 0 {
			maxRows = m.height / 2
			if maxRows < 8 {
				maxRows = 8
			}
		}
		for i, s := range m.tagSuggestions {
			prefix := "  "
			if i == m.tagCursor {
				prefix = "> "
			}
			lines = append(lines, prefix+s)
			if i >= maxRows {
				break
			}
		}
	}
	return frameStyle.Width(m.width - 2).Render(strings.Join(lines, "\n"))
}

func (m model) overlayCentered(base string, overlay string) string {
	baseLines := strings.Split(base, "\n")
	ovLines := strings.Split(overlay, "\n")
	startY := (len(baseLines) - len(ovLines)) / 2
	if startY < 0 {
		startY = 0
	}
	for i, ov := range ovLines {
		y := startY + i
		if y >= len(baseLines) {
			break
		}
		ovLen := lipgloss.Width(ov)
		baseWidth := lipgloss.Width(baseLines[y])
		startX := (baseWidth - ovLen) / 2
		if startX < 0 {
			startX = 0
		}
		line := baseLines[y]
		lineW := lipgloss.Width(line)
		if lineW < startX {
			line = line + strings.Repeat(" ", startX-lineW)
		}
		prefix := truncateVisible(line, startX)
		suffix := ""
		if startX+ovLen < lipgloss.Width(line) {
			suffix = sliceVisible(line, startX+ovLen)
		}
		baseLines[y] = prefix + ov + suffix
	}
	return strings.Join(baseLines, "\n")
}

func truncateVisible(s string, width int) string {
	if width <= 0 {
		return ""
	}
	out := ""
	for _, r := range s {
		if lipgloss.Width(out+string(r)) > width {
			break
		}
		out += string(r)
	}
	return out
}

func sliceVisible(s string, start int) string {
	if start <= 0 {
		return s
	}
	cur := 0
	out := ""
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if cur+w <= start {
			cur += w
			continue
		}
		out += string(r)
		cur += w
	}
	return out
}
