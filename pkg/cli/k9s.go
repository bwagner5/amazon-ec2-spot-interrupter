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
	"os"
	"strings"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
)

type K9sInterruptNodeOptions struct {
	NodeHint string
	Delay    time.Duration
	Clean    bool
}

func ResolveNodeHint(opts K9sInterruptNodeOptions) (string, error) {
	if strings.TrimSpace(opts.NodeHint) != "" {
		return strings.TrimSpace(opts.NodeHint), nil
	}
	// k9s plugin will set this env var from selected node name.
	if v := strings.TrimSpace(os.Getenv("SPOT_INTERRUPTER_NODE")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("node hint not provided (use --node or SPOT_INTERRUPTER_NODE)")
}

func InterruptNodeFromK9s(ctx context.Context, interrupter *itn.ITN, opts K9sInterruptNodeOptions) error {
	nodeHint, err := ResolveNodeHint(opts)
	if err != nil {
		return err
	}

	instanceID, err := interrupter.ResolveInstanceIDFromNodeHint(ctx, nodeHint)
	if err != nil {
		return err
	}

	experiment, events, err := interrupter.Interrupt(ctx, []string{instanceID}, opts.Delay, opts.Clean)
	if err != nil {
		return err
	}
	PrintMonitor(experiment, events)
	return nil
}
