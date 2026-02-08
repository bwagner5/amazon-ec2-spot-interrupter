package cli

import (
	"testing"
	"time"

	"github.com/aws/amazon-ec2-spot-interrupter/pkg/itn"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"
)

func TestParseOutputFormat(t *testing.T) {
	_, err := ParseOutputFormat("json")
	require.NoError(t, err)
	_, err = ParseOutputFormat("yaml")
	require.NoError(t, err)
	_, err = ParseOutputFormat("table")
	require.NoError(t, err)
	_, err = ParseOutputFormat("markdown")
	require.NoError(t, err)
	_, err = ParseOutputFormat("none")
	require.NoError(t, err)
	_, err = ParseOutputFormat("xml")
	require.Error(t, err)
}

func TestBuildInterruptionReport(t *testing.T) {
	nameKey := "Name"
	nameVal := "node-a"
	az := "us-west-2a"
	id := "i-123"
	inst := ec2types.Instance{
		InstanceId: &id,
		Placement:  &ec2types.Placement{AvailabilityZone: &az},
		Tags:       []ec2types.Tag{{Key: &nameKey, Value: &nameVal}},
	}
	now := time.Now().UTC()
	events := []itn.Event{
		{Timestamp: now, Message: "rebalance"},
		{Timestamp: now.Add(time.Second), Message: "terminating", InstanceStates: map[string]string{id: "shutting-down"}},
	}
	report := BuildInterruptionReport([]*ec2types.Instance{&inst}, events)
	require.Len(t, report, 1)
	require.Equal(t, "i-123", report[0].InstanceID)
	require.Equal(t, "node-a", report[0].Name)
	require.Equal(t, "us-west-2", report[0].Region)
	require.Equal(t, "us-west-2a", report[0].AZ)
	require.Len(t, report[0].Events, 2)
}
