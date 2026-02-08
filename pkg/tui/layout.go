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
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func pinFooterToBottom(height int, body string, footer string) string {
	if strings.TrimSpace(footer) == "" {
		return body
	}
	bodyH := lipgloss.Height(body)
	footerH := lipgloss.Height(footer)
	padding := height - bodyH - footerH
	if padding < 1 {
		padding = 1
	}
	return body + strings.Repeat("\n", padding) + footer
}
