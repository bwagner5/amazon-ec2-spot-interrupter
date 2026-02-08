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
	"math/rand"
	"sync"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type chaosSnapshot struct {
	Running   bool
	Last      string
	Max       int
	MinWait   time.Duration
	StartedAt time.Time
}

type chaosController struct {
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}
	last    string
	max     int
	minWait time.Duration
	started time.Time
}

func newChaosController() *chaosController {
	return &chaosController{}
}

func (c *chaosController) Snapshot() chaosSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return chaosSnapshot{
		Running:   c.running,
		Last:      c.last,
		Max:       c.max,
		MinWait:   c.minWait,
		StartedAt: c.started,
	}
}

func (c *chaosController) setLast(msg string) {
	c.mu.Lock()
	c.last = msg
	c.mu.Unlock()
}

func (c *chaosController) Start(
	ctx context.Context,
	interrupter *itn.ITN,
	hub *experimentHub,
	discover func(context.Context) ([]ec2types.Instance, error),
	filter func(ec2types.Instance) bool,
	max int,
	minWait time.Duration,
	clean bool,
) error {
	if max < 1 {
		return fmt.Errorf("max instances must be >= 1")
	}
	if minWait < 0 {
		return fmt.Errorf("min wait must be >= 0")
	}

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("chaos is already running")
	}
	c.running = true
	c.stopCh = make(chan struct{})
	c.max = max
	c.minWait = minWait
	c.started = time.Now()
	c.last = "chaos started"
	stop := c.stopCh
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			c.running = false
			c.stopCh = nil
			if c.last == "" {
				c.last = "chaos stopped"
			}
			c.mu.Unlock()
		}()

		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			select {
			case <-ctx.Done():
				c.setLast("chaos stopped (context cancelled)")
				return
			case <-stop:
				c.setLast("chaos stopped")
				return
			default:
			}

			instances, err := interrupter.SpotInstances(ctx)
			if discover != nil {
				instances, err = discover(ctx)
			}
			if err != nil {
				c.setLast(fmt.Sprintf("chaos waiting: spot instance query failed (%v)", err))
				if !sleepInterruptible(stop, minWait) {
					return
				}
				continue
			}

			runnable := make([]ec2types.Instance, 0, len(instances))
			for _, inst := range instances {
				if !isRunnable(inst) {
					continue
				}
				if filter != nil && !filter(inst) {
					continue
				}
				// Warm-up guard: don't interrupt instances immediately after launch.
				if inst.LaunchTime != nil && minWait > 0 && time.Since(*inst.LaunchTime) < minWait {
					continue
				}
				runnable = append(runnable, inst)
			}
			if len(runnable) == 0 {
				c.setLast("chaos waiting: no eligible running instances yet")
				if !sleepInterruptible(stop, minWait) {
					return
				}
				continue
			}

			high := max
			if high > len(runnable) {
				high = len(runnable)
			}
			if high < 1 {
				high = 1
			}
			low := 1
			if high > 1 {
				low = (high + 1) / 2 // keep within roughly lower half-to-max as requested
			}
			count := low
			if high > low {
				count = low + rng.Intn(high-low+1)
			}
			if count > len(runnable) {
				count = len(runnable)
			}

			picked := rng.Perm(len(runnable))[:count]
			subset := make([]*ec2types.Instance, 0, count)
			for _, idx := range picked {
				inst := runnable[idx]
				subset = append(subset, &inst)
			}

			experiments, events, err := interrupter.InterruptInstances(ctx, subset, 15*time.Second, clean)
			if err != nil {
				c.setLast(fmt.Sprintf("chaos error: %v", err))
			} else if len(experiments) > 0 {
				expID := hub.Track(experiments[0], subset, events)
				c.setLast(fmt.Sprintf("started chaos experiment %s (%d instances)", expID, count))
			} else {
				c.setLast(fmt.Sprintf("chaos started (%d instances)", count))
			}

			wait := minWait
			if minWait > 0 {
				wait += time.Duration(rng.Int63n(int64(minWait) + 1)) // between min and 2*min
			}
			if !sleepInterruptible(stop, wait) {
				return
			}
		}
	}()

	return nil
}

func (c *chaosController) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	c.running = false
	c.last = "chaos stopping"
}

func sleepInterruptible(stop <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-stop:
		return false
	}
}
