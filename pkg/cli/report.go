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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

type OutputFormat string

const (
	OutputNone     OutputFormat = "none"
	OutputJSON     OutputFormat = "json"
	OutputYAML     OutputFormat = "yaml"
	OutputTable    OutputFormat = "table"
	OutputMarkdown OutputFormat = "markdown"
)

type ReportEvent struct {
	Timestamp string `json:"timestamp" yaml:"timestamp"`
	Message   string `json:"message" yaml:"message"`
}

type InstanceReport struct {
	InstanceID string        `json:"instance_id" yaml:"instance_id"`
	Name       string        `json:"name" yaml:"name"`
	Region     string        `json:"region" yaml:"region"`
	AZ         string        `json:"az" yaml:"az"`
	Events     []ReportEvent `json:"events" yaml:"events"`
}

func ParseOutputFormat(raw string) (OutputFormat, error) {
	v := OutputFormat(strings.ToLower(strings.TrimSpace(raw)))
	switch v {
	case OutputNone, OutputJSON, OutputYAML, OutputTable, OutputMarkdown:
		return v, nil
	default:
		return "", fmt.Errorf("invalid output format %q (valid: none,json,yaml,table,markdown)", raw)
	}
}

func CollectEvents(events <-chan itn.Event) []itn.Event {
	out := []itn.Event{}
	for e := range events {
		out = append(out, e)
	}
	return out
}

func BuildInterruptionReport(instances []*ec2types.Instance, events []itn.Event) []InstanceReport {
	reports := make([]InstanceReport, 0, len(instances))
	byID := map[string]int{}
	for _, ptr := range instances {
		if ptr == nil || ptr.InstanceId == nil {
			continue
		}
		id := *ptr.InstanceId
		if _, ok := byID[id]; ok {
			continue
		}
		report := InstanceReport{
			InstanceID: id,
			Name:       instanceName(*ptr),
			Region:     regionFromInstance(*ptr),
			AZ:         instanceAZ(*ptr),
			Events:     []ReportEvent{},
		}
		byID[id] = len(reports)
		reports = append(reports, report)
	}

	for _, e := range events {
		entry := ReportEvent{
			Timestamp: e.Timestamp.Format(time.RFC3339),
			Message:   strings.TrimSpace(e.Message),
		}
		if len(e.InstanceStates) == 0 {
			for i := range reports {
				reports[i].Events = append(reports[i].Events, entry)
			}
			continue
		}
		for id := range e.InstanceStates {
			idx, ok := byID[id]
			if !ok {
				continue
			}
			reports[idx].Events = append(reports[idx].Events, entry)
		}
	}

	sort.Slice(reports, func(i, j int) bool { return reports[i].InstanceID < reports[j].InstanceID })
	return reports
}

func PrintReport(format OutputFormat, reports []InstanceReport) error {
	switch format {
	case OutputNone:
		return nil
	case OutputJSON:
		b, err := json.MarshalIndent(reports, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	case OutputYAML:
		b, err := yaml.Marshal(reports)
		if err != nil {
			return err
		}
		fmt.Print(string(b))
		return nil
	case OutputTable:
		var buf bytes.Buffer
		w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "INSTANCE ID\tNAME\tREGION\tAZ\tEVENT TIME\tEVENT")
		for _, r := range reports {
			if len(r.Events) == 0 {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t-\t-\n", r.InstanceID, r.Name, r.Region, r.AZ)
				continue
			}
			for _, e := range r.Events {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.InstanceID, r.Name, r.Region, r.AZ, e.Timestamp, e.Message)
			}
		}
		_ = w.Flush()
		fmt.Print(buf.String())
		return nil
	case OutputMarkdown:
		fmt.Println("# Interruption Report")
		for _, r := range reports {
			fmt.Printf("## %s\n\n", r.InstanceID)
			fmt.Printf("- Name: %s\n", r.Name)
			fmt.Printf("- Region: %s\n", r.Region)
			fmt.Printf("- AZ: %s\n\n", r.AZ)
			fmt.Println("| Timestamp | Event |")
			fmt.Println("|---|---|")
			if len(r.Events) == 0 {
				fmt.Println("| - | - |")
			} else {
				for _, e := range r.Events {
					msg := strings.ReplaceAll(e.Message, "|", "\\|")
					fmt.Printf("| %s | %s |\n", e.Timestamp, msg)
				}
			}
			fmt.Println()
		}
		return nil
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

func instanceName(i ec2types.Instance) string {
	for _, tag := range i.Tags {
		if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
			return *tag.Value
		}
	}
	return "-"
}

func instanceAZ(i ec2types.Instance) string {
	if i.Placement.AvailabilityZone == nil {
		return "-"
	}
	return *i.Placement.AvailabilityZone
}

func regionFromInstance(i ec2types.Instance) string {
	az := instanceAZ(i)
	if len(az) < 2 || az == "-" {
		return "-"
	}
	return az[:len(az)-1]
}

func printReportError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ failed to render report: %v\n", err)
	}
}
