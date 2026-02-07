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

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	k9sPluginName = "ec2-spot-interrupter"
	k9sPluginYAML = `  ec2-spot-interrupter:
    shortCut: Shift-I
    confirm: true
    description: Interrupt selected Spot node via AWS
    scopes:
      - nodes
    command: ec2-spot-interrupter
    background: true
    env:
      - SPOT_INTERRUPTER_NODE=$NAME
    args:
      - k9s
      - interrupt-node
`
)

type InstallK9sResult struct {
	PluginFile string
	BackupFile string
	Installed  bool
}

func InstallK9sPlugin(k9sDir string) (*InstallK9sResult, error) {
	if k9sDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to resolve home dir: %w", err)
		}
		k9sDir = filepath.Join(home, ".k9s")
	}

	if err := os.MkdirAll(k9sDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create k9s dir: %w", err)
	}

	pluginFile := filepath.Join(k9sDir, "plugins.yaml")
	result := &InstallK9sResult{
		PluginFile: pluginFile,
	}

	content := ""
	existing, err := os.ReadFile(pluginFile)
	if err == nil {
		content = string(existing)
		backupFile := fmt.Sprintf("%s.bak.%s", pluginFile, time.Now().Format("20060102150405"))
		if err := copyFile(pluginFile, backupFile); err != nil {
			return nil, fmt.Errorf("failed to back up existing plugin config: %w", err)
		}
		result.BackupFile = backupFile
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read plugin config: %w", err)
	}
	content = removeExistingPluginBlock(content)

	if strings.TrimSpace(content) == "" {
		content = "plugins:\n" + k9sPluginYAML
	} else if strings.Contains(content, "\nplugins:") || strings.HasPrefix(content, "plugins:") {
		content = strings.TrimRight(content, "\n") + "\n\n" + k9sPluginYAML
	} else {
		content = strings.TrimRight(content, "\n") + "\n\nplugins:\n" + k9sPluginYAML
	}

	if err := os.WriteFile(pluginFile, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write plugin config: %w", err)
	}
	result.Installed = true
	return result, nil
}

func copyFile(src string, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

func removeExistingPluginBlock(content string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	pluginBlock := regexp.MustCompile(`(?ms)^[ \t]*` + regexp.QuoteMeta(k9sPluginName) + `:[ \t]*\n(?:^[ \t]{2,}.*\n?)*`)
	return strings.TrimSpace(pluginBlock.ReplaceAllString(content, "")) + "\n"
}
