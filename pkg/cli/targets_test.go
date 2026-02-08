package cli

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"
)

func TestParseAWSFilters(t *testing.T) {
	filters, err := ParseAWSFilters([]string{
		"Name=tag:Name,Values=node-a,node-b",
		"Name=tag-key,Values=Name",
		"Name=tag-value,Values=worker",
		"Name=instance-id,Values=i-123",
	})
	require.NoError(t, err)
	require.Len(t, filters, 4)
	require.Equal(t, "tag:Name", filters[0].Name)
	require.Equal(t, []string{"node-a", "node-b"}, filters[0].Values)
}

func TestMatchesFilter_TagAndID(t *testing.T) {
	nameKey := "Name"
	nameVal := "node-a"
	envKey := "Environment"
	envVal := "prod"
	id := "i-1234567890"
	inst := ec2types.Instance{
		InstanceId: &id,
		Tags: []ec2types.Tag{
			{Key: &nameKey, Value: &nameVal},
			{Key: &envKey, Value: &envVal},
		},
	}

	require.True(t, matchesFilter(inst, AWSFilter{Name: "tag:Name", Values: []string{"node-a"}}))
	require.True(t, matchesFilter(inst, AWSFilter{Name: "tag-key", Values: []string{"Environment"}}))
	require.True(t, matchesFilter(inst, AWSFilter{Name: "tag-value", Values: []string{"prod"}}))
	require.True(t, matchesFilter(inst, AWSFilter{Name: "instance-id", Values: []string{"i-1234567890"}}))
	require.False(t, matchesFilter(inst, AWSFilter{Name: "tag:Name", Values: []string{"node-b"}}))
}
