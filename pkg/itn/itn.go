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

package itn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/ptr"
	"go.uber.org/multierr"
)

const (
	trustPolicy = `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {
					"Service": [
					  "fis.amazonaws.com"
					]
				},
				"Action": "sts:AssumeRole"
			}
		]
	}`
	rolePolicy = `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "AllowFISExperimentRoleSpotInstanceActions",
				"Effect": "Allow",
				"Action": [
					"ec2:SendSpotInstanceInterruptions"
				],
				"Resource": "arn:aws:ec2:*:*:instance/*"
			}
		]
	}`
	SpotITNAction  = "aws:ec2:send-spot-instance-interruptions"
	fisRoleName    = "aws-fis-itn"
	fisTargetLimit = 5
)

type ITN struct {
	cfg       aws.Config
	stsClient stsAPI
	fisClient fisAPI
	iamClient iamAPI
	ec2Client ec2API
}

func New(cfg aws.Config) *ITN {
	return &ITN{
		cfg:       cfg,
		stsClient: sts.NewFromConfig(cfg),
		fisClient: fis.NewFromConfig(cfg),
		iamClient: iam.NewFromConfig(cfg),
		ec2Client: ec2.NewFromConfig(cfg),
	}
}

// Interrupt will start an FIS experiment to send Spot ITNs to the instance IDs specified and then monitor
// the experiment for the progress.
func (i ITN) Interrupt(ctx context.Context, instanceIDs []string, delay time.Duration, clean bool) (*types.Experiment, <-chan Event, error) {
	if err := i.validate(ctx, instanceIDs); err != nil {
		return nil, nil, err
	}
	experiment, err := i.createInterruptions(ctx, instanceIDs, delay)
	if err != nil {
		return nil, nil, err
	}
	events := make(chan Event, 10)
	go func() {
		defer close(events)
		if clean {
			defer func() {
				if err := i.Clean(ctx, *experiment); err != nil {
					events <- Event{
						Timestamp: time.Now(),
						Message:   fmt.Sprintf("❌ Error cleaning up FIS Experiment: %v", err),
					}
				}
			}()
		}
		if err := i.monitor(ctx, events, experiment, delay); err != nil {
			events <- Event{
				Timestamp: time.Now(),
				Message:   fmt.Sprintf("❌ Error executing: %v", err),
			}
		}
	}()
	return experiment, events, nil
}

func (i ITN) validate(ctx context.Context, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return errors.New("no instances specified")
	}
	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{InstanceIds: instanceIDs})
	var instances []ec2types.Instance
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, r := range out.Reservations {
			instances = append(instances, r.Instances...)
		}
	}
	var err error
	for _, instance := range instances {
		if instance.InstanceLifecycle != ec2types.InstanceLifecycleTypeSpot {
			err = multierr.Append(err, fmt.Errorf("%s is not a Spot instance", *instance.InstanceId))
		}
		if instance.State.Name != ec2types.InstanceStateNameRunning {
			err = multierr.Append(err, fmt.Errorf("%s is not running", *instance.InstanceId))
		}
	}
	return err
}

func (i ITN) SpotInstances(ctx context.Context) ([]ec2types.Instance, error) {
	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-lifecycle"),
				Values: []string{string(ec2types.InstanceLifecycleSpot)},
			},
		},
	})
	var instances []ec2types.Instance
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return instances, err
		}
		for _, r := range out.Reservations {
			instances = append(instances, r.Instances...)
		}
	}
	return instances, nil
}

// Clean deletes the generated experiment template from FIS
func (i ITN) Clean(ctx context.Context, experiment types.Experiment) error {
	_, err := i.fisClient.DeleteExperimentTemplate(ctx, &fis.DeleteExperimentTemplateInput{Id: experiment.ExperimentTemplateId})
	return err
}

type Event struct {
	Stage          EventStage
	Message        string
	NextEvent      time.Duration
	Timestamp      time.Time
	InstanceStates map[string]string
}

type EventStage string

const (
	EventStageUnknown             EventStage = "unknown"
	EventStageRebalanceSent       EventStage = "rebalance-sent"
	EventStageWarningScheduled    EventStage = "warning-scheduled"
	EventStageExperimentUpdate    EventStage = "experiment-update"
	EventStageWarningSent         EventStage = "warning-sent"
	EventStageInstanceTerminating EventStage = "instance-terminating"
	EventStageInstanceTerminated  EventStage = "instance-terminated"
)

func (i ITN) monitor(ctx context.Context, events chan Event, experiment *types.Experiment, delay time.Duration) error {
	instanceIDs := i.experimentInstanceIDs(experiment)
	events <- Event{
		Stage:     EventStageRebalanceSent,
		Timestamp: time.Now(),
		Message:   "✅ Rebalance Recommendation sent",
	}
	if experiment.StartTime != nil && time.Until(*experiment.StartTime) < delay {
		timeUntilStart := delay - time.Until(*experiment.StartTime)
		events <- Event{
			Stage:     EventStageWarningScheduled,
			Message:   fmt.Sprintf("⏳ Interruption will be sent in %d seconds", int(timeUntilStart.Seconds())),
			NextEvent: timeUntilStart,
			Timestamp: time.Now(),
		}
		time.Sleep(timeUntilStart)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastStatus types.ExperimentStatus
	var statusInitialized bool
	for {
		select {
		case <-ticker.C:
			experimentUpdate, err := i.fisClient.GetExperiment(ctx, &fis.GetExperimentInput{Id: experiment.Id})
			if err != nil {
				return err
			}
			status := experimentUpdate.Experiment.State.Status
			changed := !statusInitialized || status != lastStatus
			if changed {
				statusInitialized = true
				lastStatus = status
			}
			switch status {
			case types.ExperimentStatusPending:
				if changed {
					events <- Event{
						Stage:     EventStageExperimentUpdate,
						Timestamp: time.Now(),
						Message:   "⏰ Interruption Experiment is pending",
					}
				}
			case types.ExperimentStatusInitiating:
				if changed {
					events <- Event{
						Stage:     EventStageExperimentUpdate,
						Timestamp: time.Now(),
						Message:   "🔧 Interruption Experiment is initializing",
					}
				}
			case types.ExperimentStatusFailed, types.ExperimentStatusStopped:
				reason := "experiment failed"
				if experimentUpdate.Experiment.State.Reason != nil {
					reason = *experimentUpdate.Experiment.State.Reason
				}
				return errors.New(reason)
			case types.ExperimentStatusCompleted:
				events <- Event{
					Stage:     EventStageWarningSent,
					Timestamp: time.Now(),
					Message:   "✅ Spot 2-minute Interruption Notification sent",
					NextEvent: time.Minute * 2,
				}
				return i.monitorTermination(ctx, events, instanceIDs)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (i ITN) monitorTermination(ctx context.Context, events chan Event, instanceIDs []string) error {
	terminatingSeen := map[string]struct{}{}
	terminatedSeen := map[string]struct{}{}

	emitStateChanges := func(states map[string]string) {
		for _, id := range instanceIDs {
			state, ok := states[id]
			if !ok {
				continue
			}
			switch state {
			case string(ec2types.InstanceStateNameShuttingDown), string(ec2types.InstanceStateNameStopping):
				if _, seen := terminatingSeen[id]; !seen {
					terminatingSeen[id] = struct{}{}
					events <- Event{
						Stage:     EventStageInstanceTerminating,
						Timestamp: time.Now(),
						Message:   fmt.Sprintf("🔻 Instance %s is terminating (%s)", id, state),
						InstanceStates: map[string]string{
							id: state,
						},
					}
				}
			case string(ec2types.InstanceStateNameTerminated):
				if _, seen := terminatedSeen[id]; !seen {
					terminatedSeen[id] = struct{}{}
					events <- Event{
						Stage:     EventStageInstanceTerminated,
						Timestamp: time.Now(),
						Message:   fmt.Sprintf("✅ Instance %s terminated", id),
						InstanceStates: map[string]string{
							id: state,
						},
					}
				}
			}
		}
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		states, err := i.describeInstanceStates(ctx, instanceIDs)
		if err != nil {
			return err
		}
		emitStateChanges(states)

		allTerminated := true
		for _, id := range instanceIDs {
			if states[id] != string(ec2types.InstanceStateNameTerminated) {
				if _, seen := terminatedSeen[id]; seen {
					continue
				}
				allTerminated = false
				break
			}
		}
		if allTerminated {
			events <- Event{
				Stage:          EventStageInstanceTerminated,
				Timestamp:      time.Now(),
				Message:        "✅ Spot Instance Shutdown sent",
				InstanceStates: states,
			}
			return nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (i ITN) describeInstanceStates(ctx context.Context, instanceIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(instanceIDs) == 0 {
		return out, nil
	}
	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				if instance.InstanceId == nil {
					continue
				}
				out[*instance.InstanceId] = string(instance.State.Name)
			}
		}
	}
	return out, nil
}

func (i ITN) createInterruptions(ctx context.Context, instanceIDs []string, delay time.Duration) (*types.Experiment, error) {
	accountID, err := i.getAccountID(ctx)
	if err != nil {
		return nil, err
	}
	roleARN, err := i.getOrCreateFISRole(ctx, accountID)
	if err != nil {
		return nil, err
	}
	template := &fis.CreateExperimentTemplateInput{
		Actions:        map[string]types.CreateExperimentTemplateActionInput{},
		Targets:        map[string]types.CreateExperimentTemplateTargetInput{},
		StopConditions: []types.CreateExperimentTemplateStopConditionInput{{Source: aws.String("none")}},
		RoleArn:        roleARN,
		Description:    aws.String(fmt.Sprintf("trigger spot ITN for instances %v", instanceIDs)),
	}
	for j, batch := range i.batchInstances(instanceIDs, fisTargetLimit) {
		key := fmt.Sprintf("itn%d", j)
		template.Actions[key] = types.CreateExperimentTemplateActionInput{
			ActionId: ptr.String(SpotITNAction),
			Parameters: map[string]string{
				// durationBeforeInterruption is the time before the instance is terminated, so we add 2 minutes
				// so that a user can configure the notificatin delay rather than the termination delay.
				"durationBeforeInterruption": fmt.Sprintf("PT%dS", int((time.Minute*2 + delay).Seconds())),
			},
			Targets: map[string]string{"SpotInstances": key},
		}
		template.Targets[key] = types.CreateExperimentTemplateTargetInput{
			ResourceType:  ptr.String("aws:ec2:spot-instance"),
			SelectionMode: ptr.String("ALL"),
			ResourceArns:  i.instanceIDsToARNs(batch, i.cfg.Region, accountID),
		}
	}
	experimentTemplate, err := i.fisClient.CreateExperimentTemplate(ctx, template)
	if err != nil {
		return nil, err
	}
	experiment, err := i.fisClient.StartExperiment(ctx, &fis.StartExperimentInput{ExperimentTemplateId: experimentTemplate.ExperimentTemplate.Id})
	if err != nil {
		return nil, err
	}
	return experiment.Experiment, nil
}

func (i ITN) batchInstances(instanceIDs []string, size int) [][]string {
	instanceIDBatches := [][]string{}
	currentBatch := []string{}
	for i, instanceID := range instanceIDs {
		if i%size == 0 && len(currentBatch) > 0 {
			instanceIDBatches = append(instanceIDBatches, currentBatch)
			currentBatch = []string{}
		}
		currentBatch = append(currentBatch, instanceID)
	}
	if len(currentBatch) > 0 {
		instanceIDBatches = append(instanceIDBatches, currentBatch)
	}
	return instanceIDBatches
}

func (i ITN) getOrCreateFISRole(ctx context.Context, accountID string) (*string, error) {
	roleName := fisRoleName
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, fisRoleName)
	out, err := i.iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 ptr.String(roleName),
		AssumeRolePolicyDocument: ptr.String(trustPolicy),
	})
	var alreadyExists *iamtypes.EntityAlreadyExistsException
	if errors.As(err, &alreadyExists) {
		// continue so we always enforce/refresh policy attachment
	} else if err != nil {
		return nil, err
	} else if out.Role != nil && out.Role.Arn != nil {
		roleARN = *out.Role.Arn
	}
	_, err = i.iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		PolicyName:     ptr.String(fmt.Sprintf("%s-policy", fisRoleName)),
		PolicyDocument: ptr.String(rolePolicy),
		RoleName:       ptr.String(roleName),
	})
	if err != nil {
		return nil, err
	}
	return ptr.String(roleARN), nil
}

func (i ITN) getAccountID(ctx context.Context) (string, error) {
	identity, err := i.stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return *identity.Account, nil
}

func (i ITN) instanceIDsToARNs(instanceIDs []string, region string, accountID string) []string {
	var arns []string
	for _, instanceID := range instanceIDs {
		arns = append(arns, fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", region, accountID, instanceID))
	}
	return arns
}

func ARNToInstanceID(arn string) string {
	return strings.Split(strings.Split(arn, ":")[5], "/")[1]
}

func (i ITN) experimentInstanceIDs(experiment *types.Experiment) []string {
	ids := map[string]struct{}{}
	for _, target := range experiment.Targets {
		for _, arn := range target.ResourceArns {
			ids[ARNToInstanceID(arn)] = struct{}{}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
