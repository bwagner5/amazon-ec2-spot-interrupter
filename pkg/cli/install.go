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

	"gopkg.in/yaml.v3"
)

const (
	k9sPluginName = "ec2-spot-interrupter"
	k9sPluginSpec = `shortCut: Shift-I
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
		var err error
		k9sDir, err = defaultK9sConfigDir()
		if err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(k9sDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create k9s dir: %w", err)
	}

	pluginFile := filepath.Join(k9sDir, "plugins.yaml")
	result := &InstallK9sResult{
		PluginFile: pluginFile,
	}

	root, err := loadPluginsYAML(pluginFile)
	if err != nil {
		return nil, err
	}

	inserted, err := ensureK9sPlugin(root)
	if err != nil {
		return nil, err
	}
	if !inserted {
		result.Installed = false
		return result, nil
	}

	raw, err := marshalPluginsYAML(root)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(pluginFile, raw, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write plugin config: %w", err)
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

func loadPluginsYAML(pluginFile string) (*yaml.Node, error) {
	var root yaml.Node
	existing, err := os.ReadFile(pluginFile)
	if err != nil {
		if os.IsNotExist(err) {
			root.Kind = yaml.DocumentNode
			root.Content = []*yaml.Node{
				{Kind: yaml.MappingNode},
			}
			return &root, nil
		}
		return nil, fmt.Errorf("failed to read plugin config: %w", err)
	}
	if strings.TrimSpace(string(existing)) == "" {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{
			{Kind: yaml.MappingNode},
		}
		return &root, nil
	}
	if err := yaml.Unmarshal(existing, &root); err != nil {
		return nil, fmt.Errorf("failed to parse plugin config: %w", err)
	}
	if root.Kind != yaml.DocumentNode {
		return nil, fmt.Errorf("unexpected YAML root type in plugin config")
	}
	if len(root.Content) == 0 || root.Content[0] == nil {
		root.Content = []*yaml.Node{
			{Kind: yaml.MappingNode},
		}
	}
	if root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected top-level mapping in plugin config")
	}
	return &root, nil
}

func ensureK9sPlugin(root *yaml.Node) (bool, error) {
	doc := root.Content[0]
	pluginsNode := upsertMappingValue(doc, "plugins")
	if pluginsNode.Kind == 0 {
		pluginsNode.Kind = yaml.MappingNode
	}
	if pluginsNode.Kind != yaml.MappingNode {
		return false, fmt.Errorf("expected 'plugins' to be a mapping in plugin config")
	}

	if hasMappingKey(pluginsNode, k9sPluginName) {
		return false, nil
	}

	specNode, err := decodeMappingNode(k9sPluginSpec)
	if err != nil {
		return false, fmt.Errorf("failed to build k9s plugin spec: %w", err)
	}
	appendMappingEntry(pluginsNode, k9sPluginName, specNode)
	return true, nil
}

func marshalPluginsYAML(root *yaml.Node) ([]byte, error) {
	raw, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("failed to render plugin config: %w", err)
	}
	return raw, nil
}

func decodeMappingNode(src string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0] == nil {
		return nil, fmt.Errorf("invalid mapping YAML")
	}
	node := doc.Content[0]
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping YAML")
	}
	return node, nil
}

func upsertMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		v := mapping.Content[i+1]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return v
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode}
	mapping.Content = append(mapping.Content, keyNode, valNode)
	return valNode
}

func hasMappingKey(mapping *yaml.Node, key string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return true
		}
	}
	return false
}

func appendMappingEntry(mapping *yaml.Node, key string, value *yaml.Node) {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	mapping.Content = append(mapping.Content, keyNode, value)
}
