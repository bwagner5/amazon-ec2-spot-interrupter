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

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/cli"
	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	"github.com/aws/amazon-ec2-spot-interrupter/pkg/tui"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// TODOs(bwagner5):
//   1. Option to pass tags instead of instance IDs
//   2. Option to pass an OD instance and have this tool create a matching instance that is spot to test an interruption
//   3. Automated chaos - give this tool a tag or vpc and allow it to randomly interrupt spot instances at will

var version string

type Options struct {
	instanceIDs []string
	filters     []string
	output      string
	endpoint    string
	delay       time.Duration
	clean       bool
	version     bool
	region      string
	profile     string
	interactive bool
}

func main() {
	options := Options{}
	rootCmd := &cobra.Command{
		Use:   "ec2-spot-interrupter",
		Short: "ec2-spot-interrupter is a simple CLI tool that triggers Amazon EC2 Spot Instance Interruption Notifications and Rebalance Recommendations.",
		Run: func(cmd *cobra.Command, _ []string) {
			if options.version {
				fmt.Println(version)
				os.Exit(0)
			}
			if len(options.instanceIDs) == 0 && len(options.filters) == 0 && !options.interactive {
				options.interactive = true
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			output, err := cli.ParseOutputFormat(options.output)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			endpoint := strings.TrimSpace(options.endpoint)
			if endpoint == "" {
				endpoint = strings.TrimSpace(os.Getenv("ENDPOINT"))
			}
			cfg, err := config.LoadDefaultConfig(ctx, awsLoadOptions(options.region, options.profile)...)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			cfg = withMockCredentials(cfg, endpoint)
			interrupter := itn.NewWithEndpoint(cfg, endpoint)
			if options.interactive {
				p := tea.NewProgram(tui.NewModel(ctx, interrupter), tea.WithAltScreen())
				if err := p.Start(); err != nil {
					fmt.Printf("❌ Error initializing TUI: %v", err)
					os.Exit(1)
				}
				os.Exit(0)
			}
			targets, err := cli.ResolveSpotTargets(ctx, interrupter, options.instanceIDs, options.filters, true)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			ptrs := make([]*ec2types.Instance, 0, len(targets))
			for idx := range targets {
				ptrs = append(ptrs, &targets[idx])
			}
			experiments, events, err := interrupter.InterruptInstances(ctx, ptrs, options.delay, options.clean)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			if output == cli.OutputNone {
				for _, exp := range experiments {
					fmt.Print(cli.Summary(exp))
				}
				cli.PrintEvents(events)
				return
			}
			collected := cli.CollectEvents(events)
			report := cli.BuildInterruptionReport(ptrs, collected)
			if err := cli.PrintReport(output, report); err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
		},
	}

	chaosOpts := cli.ChaosOptions{}
	chaosCmd := &cobra.Command{
		Use:   "chaos",
		Short: "Run randomized Spot interruption chaos non-interactively",
		Run: func(cmd *cobra.Command, _ []string) {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			output, err := cli.ParseOutputFormat(options.output)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			endpoint := strings.TrimSpace(options.endpoint)
			if endpoint == "" {
				endpoint = strings.TrimSpace(os.Getenv("ENDPOINT"))
			}
			cfg, err := config.LoadDefaultConfig(ctx, awsLoadOptions(options.region, options.profile)...)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			cfg = withMockCredentials(cfg, endpoint)
			interrupter := itn.NewWithEndpoint(cfg, endpoint)
			chaosOpts.InstanceIDs = options.instanceIDs
			chaosOpts.Filters = options.filters
			chaosOpts.Output = output
			chaosOpts.Delay = options.delay
			chaosOpts.Clean = options.clean
			if err := cli.RunChaos(ctx, interrupter, chaosOpts); err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
		},
	}
	chaosCmd.Flags().IntVar(&chaosOpts.MaxAtOnce, "max-at-once", 0, "maximum instances to interrupt per cycle (default: dynamic 1/3 of eligible instances)")
	chaosCmd.Flags().DurationVar(&chaosOpts.MinWait, "min-wait", 5*time.Minute, "minimum wait between chaos cycles and minimum warm-up time since launch")
	chaosCmd.Flags().BoolVar(&chaosOpts.Force, "force", false, "skip confirmation prompt")

	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install helper integrations",
	}
	var k9sDir string
	k9sPluginCmd := &cobra.Command{
		Use:   "k9s-plugin",
		Short: "Install the k9s plugin configuration for ec2-spot-interrupter",
		Run: func(cmd *cobra.Command, _ []string) {
			result, err := cli.InstallK9sPlugin(k9sDir)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			if result.BackupFile != "" {
				fmt.Printf("🗂️  Backed up existing plugin config to %s\n", result.BackupFile)
			}
			if result.Installed {
				fmt.Printf("✅ Installed k9s plugin in %s\n", result.PluginFile)
			} else {
				fmt.Printf("ℹ️  k9s plugin already configured in %s\n", result.PluginFile)
			}
			fmt.Println("Restart k9s (or reload plugins) and press Shift-I to launch.")
		},
	}
	k9sPluginCmd.Flags().StringVar(&k9sDir, "k9s-dir", "", "path to k9s config directory (default: ~/.k9s)")
	installCmd.AddCommand(k9sPluginCmd)

	k9sCmd := &cobra.Command{
		Use:   "k9s",
		Short: "k9s integration commands",
	}
	var k9sNode string
	interruptNodeCmd := &cobra.Command{
		Use:   "interrupt-node",
		Short: "Interrupt the EC2 Spot instance matching a node hint",
		Run: func(cmd *cobra.Command, _ []string) {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			endpoint := strings.TrimSpace(options.endpoint)
			if endpoint == "" {
				endpoint = strings.TrimSpace(os.Getenv("ENDPOINT"))
			}
			cfg, err := config.LoadDefaultConfig(ctx, awsLoadOptions(options.region, options.profile)...)
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
			cfg = withMockCredentials(cfg, endpoint)
			interrupter := itn.NewWithEndpoint(cfg, endpoint)
			err = cli.InterruptNodeFromK9s(ctx, interrupter, cli.K9sInterruptNodeOptions{
				NodeHint: k9sNode,
				Delay:    options.delay,
				Clean:    options.clean,
			})
			if err != nil {
				fmt.Printf("❌ %s\n", err)
				os.Exit(1)
			}
		},
	}
	interruptNodeCmd.Flags().StringVar(&k9sNode, "node", "", "node hint (node name/FQDN/instance-id)")
	k9sCmd.AddCommand(interruptNodeCmd)

	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(k9sCmd)
	rootCmd.AddCommand(chaosCmd)

	rootCmd.PersistentFlags().StringSliceVarP(&options.instanceIDs, "instance-ids", "i", []string{}, "instance IDs to interrupt")
	rootCmd.PersistentFlags().StringArrayVar(&options.filters, "filter", []string{}, "AWS-style filter selector, e.g. Name=tag:Name,Values=worker-a")
	rootCmd.PersistentFlags().StringVarP(&options.output, "output", "o", "none", "report output format: none,json,yaml,table,markdown")
	rootCmd.PersistentFlags().StringVar(&options.endpoint, "endpoint", "", "override AWS API endpoint (also supports ENDPOINT env var)")
	rootCmd.PersistentFlags().BoolVarP(&options.clean, "clean", "c", true, "clean up the underlying simulations")
	rootCmd.PersistentFlags().DurationVarP(&options.delay, "delay", "d", time.Second*15, "duration until the interruption notification is sent")
	rootCmd.PersistentFlags().BoolVarP(&options.version, "version", "v", false, "the version")
	rootCmd.PersistentFlags().BoolVar(&options.interactive, "interactive", false, "interactive TUI")
	rootCmd.PersistentFlags().StringVarP(&options.region, "region", "r", "", "the AWS Region (or 'global')")
	rootCmd.PersistentFlags().StringVarP(&options.profile, "profile", "p", "", "the AWS Profile")
	rootCmd.Execute()
}

func withMockCredentials(cfg aws.Config, endpoint string) aws.Config {
	if strings.TrimSpace(endpoint) == "" {
		return cfg
	}
	cfg.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("mock-access-key", "mock-secret-key", "mock-session-token"))
	return cfg
}

func awsLoadOptions(region, profile string) []func(*config.LoadOptions) error {
	var opts []func(*config.LoadOptions) error
	if r := strings.TrimSpace(region); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	if p := strings.TrimSpace(profile); p != "" {
		opts = append(opts, config.WithSharedConfigProfile(p))
	}
	return opts
}
