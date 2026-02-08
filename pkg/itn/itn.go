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
	"regexp"
	"sort"
	"strings"
	"sync"
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
					"ec2:SendSpotInstanceInterruptions",
					"ec2:DescribeInstances"
				],
				"Resource": "*"
			}
		]
	}`
	SpotITNAction  = "aws:ec2:send-spot-instance-interruptions"
	fisRoleName    = "aws-fis-itn"
	fisTargetLimit = 5
)

var instanceIDInStringRegex = regexp.MustCompile(`i-[a-z0-9]{8,}`)

type ITN struct {
	cfg       aws.Config
	stsClient stsAPI
	fisClient fisAPI
	iamClient iamAPI
	ec2Client ec2API
}

func (i ITN) Region() string {
	return i.cfg.Region
}

type RegionQueryProgress struct {
	Region    string
	Completed int
	Total     int
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
	if delay < 0 {
		return nil, nil, errors.New("delay cannot be negative")
	}
	if delay > 13*time.Minute {
		return nil, nil, errors.New("delay must be <= 13m (FIS requires interruption at <= 15m and includes a fixed 2m warning window)")
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
	if strings.EqualFold(i.cfg.Region, "global") {
		return i.SpotInstancesGlobal(ctx, nil)
	}
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

func (i ITN) SpotInstancesGlobal(ctx context.Context, progress chan<- RegionQueryProgress) ([]ec2types.Instance, error) {
	regions, err := i.ListRegions(ctx)
	if err != nil {
		return nil, err
	}
	return i.SpotInstancesInRegions(ctx, regions, progress)
}

func (i ITN) SpotInstancesInRegions(ctx context.Context, regions []string, progress chan<- RegionQueryProgress) ([]ec2types.Instance, error) {
	if len(regions) == 0 {
		return nil, nil
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		out       []ec2types.Instance
		errs      error
		completed int
	)
	total := len(regions)
	wg.Add(total)
	for _, region := range regions {
		region := region
		go func() {
			defer wg.Done()
			client := i.withRegion(region)
			instances, err := client.spotInstancesSingleRegion(ctx)
			mu.Lock()
			defer mu.Unlock()
			completed++
			if progress != nil {
				progress <- RegionQueryProgress{Region: region, Completed: completed, Total: total}
			}
			if err != nil {
				errs = multierr.Append(errs, fmt.Errorf("%s: %w", region, err))
				return
			}
			out = append(out, instances...)
		}()
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool {
		ra := regionFromInstance(out[a])
		rb := regionFromInstance(out[b])
		if ra != rb {
			return ra < rb
		}
		return instanceIDValue(out[a]) < instanceIDValue(out[b])
	})
	return out, errs
}

func (i ITN) ListRegions(ctx context.Context) ([]string, error) {
	cfg := i.cfg
	if cfg.Region == "" || strings.EqualFold(cfg.Region, "global") {
		cfg.Region = "us-east-1"
	}
	ec2Client := ec2.NewFromConfig(cfg)
	out, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(false)})
	if err != nil {
		return nil, err
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		if r.RegionName == nil {
			continue
		}
		regions = append(regions, *r.RegionName)
	}
	sort.Strings(regions)
	return regions, nil
}

func (i ITN) InterruptInstances(ctx context.Context, instances []*ec2types.Instance, delay time.Duration, clean bool) ([]*types.Experiment, <-chan Event, error) {
	byRegion := map[string][]string{}
	for _, inst := range instances {
		if inst == nil || inst.InstanceId == nil {
			continue
		}
		region := regionFromInstance(*inst)
		if region == "" {
			region = i.cfg.Region
		}
		byRegion[region] = append(byRegion[region], *inst.InstanceId)
	}
	return i.interruptByRegion(ctx, byRegion, delay, clean)
}

func (i ITN) InterruptInstanceIDs(ctx context.Context, instanceIDs []string, delay time.Duration, clean bool) ([]*types.Experiment, <-chan Event, error) {
	if !strings.EqualFold(i.cfg.Region, "global") {
		exp, events, err := i.Interrupt(ctx, instanceIDs, delay, clean)
		if err != nil {
			return nil, nil, err
		}
		return []*types.Experiment{exp}, events, nil
	}
	instances, err := i.resolveInstancesByIDGlobal(ctx, instanceIDs)
	if err != nil {
		return nil, nil, err
	}
	ptrs := make([]*ec2types.Instance, 0, len(instances))
	for idx := range instances {
		ptrs = append(ptrs, &instances[idx])
	}
	return i.InterruptInstances(ctx, ptrs, delay, clean)
}

func (i ITN) interruptByRegion(ctx context.Context, byRegion map[string][]string, delay time.Duration, clean bool) ([]*types.Experiment, <-chan Event, error) {
	if len(byRegion) == 0 {
		return nil, nil, errors.New("no instances specified")
	}
	merged := make(chan Event, 50)
	experiments := make([]*types.Experiment, 0, len(byRegion))
	chs := make([]<-chan Event, 0, len(byRegion))
	for region, ids := range byRegion {
		regional := i.withRegion(region)
		exp, ev, err := regional.Interrupt(ctx, ids, delay, clean)
		if err != nil {
			return nil, nil, err
		}
		experiments = append(experiments, exp)
		chs = append(chs, ev)
	}
	go func() {
		defer close(merged)
		var wg sync.WaitGroup
		wg.Add(len(chs))
		for idx, ch := range chs {
			region := regionFromExperiment(experiments[idx])
			go func(region string, c <-chan Event) {
				defer wg.Done()
				for e := range c {
					e.Message = fmt.Sprintf("[%s] %s", region, e.Message)
					merged <- e
				}
			}(region, ch)
		}
		wg.Wait()
	}()
	return experiments, merged, nil
}

func (i ITN) resolveInstancesByIDGlobal(ctx context.Context, instanceIDs []string) ([]ec2types.Instance, error) {
	regions, err := i.ListRegions(ctx)
	if err != nil {
		return nil, err
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		found    = map[string]ec2types.Instance{}
		allErr   error
		idLookup = map[string]struct{}{}
	)
	for _, id := range instanceIDs {
		idLookup[id] = struct{}{}
	}
	wg.Add(len(regions))
	for _, region := range regions {
		region := region
		go func() {
			defer wg.Done()
			client := i.withRegion(region)
			paginator := ec2.NewDescribeInstancesPaginator(client.ec2Client, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{
					{Name: aws.String("instance-id"), Values: instanceIDs},
				},
			})
			instances := []ec2types.Instance{}
			for paginator.HasMorePages() {
				out, err := paginator.NextPage(ctx)
				if err != nil {
					mu.Lock()
					allErr = multierr.Append(allErr, fmt.Errorf("%s: %w", region, err))
					mu.Unlock()
					return
				}
				for _, res := range out.Reservations {
					instances = append(instances, res.Instances...)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, inst := range instances {
				id := instanceIDValue(inst)
				if id == "" {
					continue
				}
				found[id] = inst
			}
		}()
	}
	wg.Wait()
	if allErr != nil {
		return nil, allErr
	}
	missing := []string{}
	out := []ec2types.Instance{}
	for _, id := range instanceIDs {
		inst, ok := found[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		out = append(out, inst)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("could not locate instances in global region scan: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (i ITN) withRegion(region string) *ITN {
	cfg := i.cfg
	cfg.Region = region
	return New(cfg)
}

func (i ITN) spotInstancesSingleRegion(ctx context.Context) ([]ec2types.Instance, error) {
	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-lifecycle"), Values: []string{string(ec2types.InstanceLifecycleSpot)}},
		},
	})
	out := []ec2types.Instance{}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	return out, nil
}

func regionFromInstance(inst ec2types.Instance) string {
	if inst.Placement.AvailabilityZone == nil {
		return ""
	}
	az := *inst.Placement.AvailabilityZone
	if len(az) < 2 {
		return az
	}
	return az[:len(az)-1]
}

func instanceIDValue(inst ec2types.Instance) string {
	if inst.InstanceId == nil {
		return ""
	}
	return *inst.InstanceId
}

func regionFromExperiment(experiment *types.Experiment) string {
	for _, target := range experiment.Targets {
		for _, arn := range target.ResourceArns {
			parts := strings.Split(arn, ":")
			if len(parts) > 3 && parts[3] != "" {
				return parts[3]
			}
		}
	}
	return "unknown-region"
}

func (i ITN) ResolveInstanceIDFromNodeHint(ctx context.Context, nodeHint string) (string, error) {
	hint := strings.TrimSpace(nodeHint)
	if hint == "" {
		return "", errors.New("empty node hint")
	}

	if extracted := instanceIDInStringRegex.FindString(hint); extracted != "" {
		if _, err := i.validateHintTarget(ctx, extracted); err == nil {
			return extracted, nil
		}
	}

	if _, err := i.validateHintTarget(ctx, hint); err == nil {
		return hint, nil
	}

	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-lifecycle"),
				Values: []string{string(ec2types.InstanceLifecycleSpot)},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{string(ec2types.InstanceStateNameRunning)},
			},
			{
				Name:   aws.String("tag:Name"),
				Values: []string{hint},
			},
		},
	})
	if id, err := i.extractSingleInstanceID(ctx, paginator); err == nil {
		return id, nil
	}

	paginator = ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-lifecycle"),
				Values: []string{string(ec2types.InstanceLifecycleSpot)},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{string(ec2types.InstanceStateNameRunning)},
			},
			{
				Name:   aws.String("private-dns-name"),
				Values: []string{hint},
			},
		},
	})
	if id, err := i.extractSingleInstanceID(ctx, paginator); err == nil {
		return id, nil
	}

	paginator = ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("instance-lifecycle"),
				Values: []string{string(ec2types.InstanceLifecycleSpot)},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{string(ec2types.InstanceStateNameRunning)},
			},
			{
				Name:   aws.String("dns-name"),
				Values: []string{hint},
			},
		},
	})
	if id, err := i.extractSingleInstanceID(ctx, paginator); err == nil {
		return id, nil
	}

	return "", fmt.Errorf("unable to resolve node hint %q to a single running Spot instance", hint)
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
	rebalanceSent := false
	warningSent := false
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var warningTimer *time.Timer
	terminatingSeen := map[string]struct{}{}
	terminatedSeen := map[string]struct{}{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-warningTimerChan(warningTimer):
			events <- Event{
				Stage:     EventStageWarningSent,
				Timestamp: time.Now(),
				Message:   "✅ Spot 2-minute Interruption Notification sent",
				NextEvent: time.Minute * 2,
			}
			warningSent = true
			warningTimer = nil
		case <-ticker.C:
			experimentUpdate, err := i.fisClient.GetExperiment(ctx, &fis.GetExperimentInput{Id: experiment.Id})
			if err != nil {
				return err
			}
			status := experimentUpdate.Experiment.State.Status
			if !rebalanceSent && i.isSpotActionInitiated(status) {
				events <- Event{
					Stage:     EventStageRebalanceSent,
					Timestamp: time.Now(),
					Message:   "✅ Rebalance Recommendation sent",
				}
				rebalanceSent = true
				if delay > 0 {
					events <- Event{
						Stage:     EventStageWarningScheduled,
						Message:   fmt.Sprintf("⏳ Interruption warning scheduled in %d seconds", int(delay.Seconds())),
						NextEvent: delay,
						Timestamp: time.Now(),
					}
					warningTimer = time.NewTimer(delay)
					defer warningTimer.Stop()
				} else {
					events <- Event{
						Stage:     EventStageWarningSent,
						Timestamp: time.Now(),
						Message:   "✅ Spot 2-minute Interruption Notification sent",
						NextEvent: time.Minute * 2,
					}
					warningSent = true
				}
			}
			switch status {
			case types.ExperimentStatusFailed, types.ExperimentStatusStopped:
				reason := "experiment failed"
				if experimentUpdate.Experiment.State.Reason != nil {
					reason = *experimentUpdate.Experiment.State.Reason
				}
				return errors.New(reason)
			}

			states, err := i.describeInstanceStates(ctx, instanceIDs)
			if err != nil {
				return err
			}
			i.emitStateChanges(events, instanceIDs, states, terminatingSeen, terminatedSeen)

			if warningSent && i.allInstancesFinal(instanceIDs, states, terminatedSeen) {
				events <- Event{
					Stage:          EventStageInstanceTerminated,
					Timestamp:      time.Now(),
					Message:        "✅ Spot Instance Shutdown sent",
					InstanceStates: states,
				}
				return nil
			}
		}
	}
}

func (i ITN) isSpotActionInitiated(status types.ExperimentStatus) bool {
	return status == types.ExperimentStatusInitiating || status == types.ExperimentStatusRunning || status == types.ExperimentStatusCompleted
}

func warningTimerChan(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func (i ITN) emitStateChanges(events chan Event, instanceIDs []string, states map[string]string, terminatingSeen map[string]struct{}, terminatedSeen map[string]struct{}) {
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
		case string(ec2types.InstanceStateNameTerminated), string(ec2types.InstanceStateNameStopped):
			if _, seen := terminatedSeen[id]; !seen {
				terminatedSeen[id] = struct{}{}
				events <- Event{
					Stage:     EventStageInstanceTerminated,
					Timestamp: time.Now(),
					Message:   fmt.Sprintf("✅ Instance %s reached final state (%s)", id, state),
					InstanceStates: map[string]string{
						id: state,
					},
				}
			}
		}
	}
}

func (i ITN) allInstancesFinal(instanceIDs []string, states map[string]string, finalSeen map[string]struct{}) bool {
	for _, id := range instanceIDs {
		state := states[id]
		if state == string(ec2types.InstanceStateNameTerminated) || state == string(ec2types.InstanceStateNameStopped) {
			continue
		}
		if _, seen := finalSeen[id]; seen {
			continue
		}
		return false
	}
	return true
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

func (i ITN) validateHintTarget(ctx context.Context, candidate string) (*ec2types.Instance, error) {
	paginator := ec2.NewDescribeInstancesPaginator(i.ec2Client, &ec2.DescribeInstancesInput{
		InstanceIds: []string{candidate},
	})
	instance, err := i.extractSingleInstance(ctx, paginator)
	if err != nil {
		return nil, err
	}
	if instance.InstanceLifecycle != ec2types.InstanceLifecycleTypeSpot {
		return nil, errors.New("target is not a Spot instance")
	}
	if instance.State.Name != ec2types.InstanceStateNameRunning {
		return nil, errors.New("target Spot instance is not running")
	}
	return &instance, nil
}

func (i ITN) extractSingleInstanceID(ctx context.Context, paginator *ec2.DescribeInstancesPaginator) (string, error) {
	instance, err := i.extractSingleInstance(ctx, paginator)
	if err != nil {
		return "", err
	}
	if instance.InstanceId == nil {
		return "", errors.New("instance id missing")
	}
	return *instance.InstanceId, nil
}

func (i ITN) extractSingleInstance(ctx context.Context, paginator *ec2.DescribeInstancesPaginator) (ec2types.Instance, error) {
	matches := []ec2types.Instance{}
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return ec2types.Instance{}, err
		}
		for _, reservation := range out.Reservations {
			matches = append(matches, reservation.Instances...)
			if len(matches) > 1 {
				return ec2types.Instance{}, errors.New("multiple matching instances found")
			}
		}
	}
	if len(matches) == 0 {
		return ec2types.Instance{}, errors.New("no matching instances found")
	}
	return matches[0], nil
}
