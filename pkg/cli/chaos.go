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
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type ChaosOptions struct {
	InstanceIDs []string
	Filters     []string
	Output      OutputFormat
	MaxAtOnce   int
	MinWait     time.Duration
	Delay       time.Duration
	Clean       bool
	Force       bool
}

func RunChaos(ctx context.Context, interrupter *itn.ITN, opts ChaosOptions) error {
	if opts.MaxAtOnce < 0 {
		return fmt.Errorf("max-at-once must be >= 0")
	}
	if opts.MinWait < 0 {
		return fmt.Errorf("min-wait must be >= 0")
	}
	if _, err := ParseAWSFilters(opts.Filters); err != nil {
		return err
	}
	if _, err := ParseOutputFormat(string(opts.Output)); err != nil {
		return err
	}

	if !opts.Force {
		if !confirmChaosStart(opts, interrupter.Region()) {
			return fmt.Errorf("chaos cancelled")
		}
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	fmt.Printf("Starting chaos loop in scope %q. Press Ctrl+C to stop.\n", interrupter.Region())
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		targets, err := ResolveSpotTargets(ctx, interrupter, opts.InstanceIDs, opts.Filters, false)
		if err != nil {
			fmt.Printf("chaos waiting: failed to resolve targets: %v\n", err)
			if !sleepUntilNext(ctx, opts.MinWait) {
				return nil
			}
			continue
		}

		eligible := make([]ec2types.Instance, 0, len(targets))
		for _, inst := range targets {
			if inst.State.Name != ec2types.InstanceStateNameRunning {
				continue
			}
			if inst.LaunchTime != nil && opts.MinWait > 0 && time.Since(*inst.LaunchTime) < opts.MinWait {
				continue
			}
			eligible = append(eligible, inst)
		}
		if len(eligible) == 0 {
			fmt.Println("chaos waiting: no eligible running Spot instances yet")
			if !sleepUntilNext(ctx, opts.MinWait) {
				return nil
			}
			continue
		}

		max := opts.MaxAtOnce
		if max == 0 {
			max = len(eligible) / 3
			if max < 1 {
				max = 1
			}
		}
		if max > len(eligible) {
			max = len(eligible)
		}
		if max < 1 {
			max = 1
		}

		low := 1
		if max > 1 {
			low = (max + 1) / 2
		}
		count := low
		if max > low {
			count = low + rng.Intn(max-low+1)
		}
		subset := randomSubset(eligible, count, rng)
		ptrs := make([]*ec2types.Instance, 0, len(subset))
		for idx := range subset {
			ptrs = append(ptrs, &subset[idx])
		}

		experiments, events, err := interrupter.InterruptInstances(ctx, ptrs, opts.Delay, opts.Clean)
		if err != nil {
			fmt.Printf("chaos error: interrupt failed: %v\n", err)
		} else {
			fmt.Printf("chaos: started interruption for %d instances\n", len(ptrs))
			if opts.Output == OutputNone {
				for _, exp := range experiments {
					fmt.Print(Summary(exp))
				}
				go PrintEvents(events)
			} else {
				go func(instances []*ec2types.Instance, ev <-chan itn.Event) {
					collected := CollectEvents(ev)
					report := BuildInterruptionReport(instances, collected)
					printReportError(PrintReport(opts.Output, report))
				}(ptrs, events)
			}
		}

		wait := opts.MinWait
		if opts.MinWait > 0 {
			wait += time.Duration(rng.Int63n(int64(opts.MinWait) + 1))
		}
		if !sleepUntilNext(ctx, wait) {
			return nil
		}
	}
}

func randomSubset(instances []ec2types.Instance, n int, rng *rand.Rand) []ec2types.Instance {
	if n >= len(instances) {
		return append([]ec2types.Instance{}, instances...)
	}
	order := rng.Perm(len(instances))
	out := make([]ec2types.Instance, 0, n)
	for _, idx := range order[:n] {
		out = append(out, instances[idx])
	}
	return out
}

func sleepUntilNext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = 15 * time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func confirmChaosStart(opts ChaosOptions, region string) bool {
	target := "all Spot instances in scope"
	if len(opts.InstanceIDs) > 0 {
		target = fmt.Sprintf("instance IDs: %s", strings.Join(opts.InstanceIDs, ","))
	} else if len(opts.Filters) > 0 {
		target = fmt.Sprintf("filters: %s", strings.Join(opts.Filters, " ; "))
	}
	fmt.Printf("Chaos will run continuously in scope %q against %s\n", region, target)
	fmt.Printf("max-at-once=%d (0 means dynamic 1/3), min-wait=%s, delay=%s\n", opts.MaxAtOnce, opts.MinWait, opts.Delay)
	fmt.Print("Continue? [y/N]: ")
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}
