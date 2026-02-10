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
	"runtime"
	"strings"
)

const (
	k9sPluginName = "ec2-spot-interrupter"
	k9sPluginYAML = `ec2-spot-interrupter:
  shortCut: Shift-I
  confirm: true
  description: Interrupt Spot
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
		var err error
		k9sDir, err = defaultK9sConfigDir()
		if err != nil {
			return nil, err
		}
	}

	pluginDir := filepath.Join(k9sDir, "plugins", k9sPluginName)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create plugin dir: %w", err)
	}
	pluginFile := filepath.Join(pluginDir, k9sPluginName+".yaml")
	result := &InstallK9sResult{
		PluginFile: pluginFile,
	}

	existing, err := os.ReadFile(pluginFile)
	if err == nil {
		if strings.TrimSpace(string(existing)) == strings.TrimSpace(k9sPluginYAML) {
			result.Installed = false
			return result, nil
		}
		// Never overwrite an existing plugin file.
		result.Installed = false
		return result, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read plugin file: %w", err)
	}

	if err := os.WriteFile(pluginFile, []byte(strings.TrimRight(k9sPluginYAML, "\n")+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write plugin file: %w", err)
	}
	result.Installed = true
	return result, nil
}

func defaultK9sConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home dir: %w", err)
	}
	if runtime.GOOS == "darwin" {
		modern := filepath.Join(home, "Library", "Application Support", "k9s")
		legacy := filepath.Join(home, ".k9s")
		if pathExists(modern) || !pathExists(legacy) {
			return modern, nil
		}
		return legacy, nil
	}
	return filepath.Join(home, ".k9s"), nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
