package main

import "testing"

func TestAWSLoadOptions(t *testing.T) {
	tests := []struct {
		name    string
		region  string
		profile string
		wantLen int
	}{
		{
			name:    "empty region and profile",
			region:  "",
			profile: "",
			wantLen: 0,
		},
		{
			name:    "region only",
			region:  "us-west-2",
			profile: "",
			wantLen: 1,
		},
		{
			name:    "profile only",
			region:  "",
			profile: "dev",
			wantLen: 1,
		},
		{
			name:    "region and profile",
			region:  "us-east-1",
			profile: "prod",
			wantLen: 2,
		},
		{
			name:    "trim whitespace",
			region:  "  us-east-2  ",
			profile: "  qa  ",
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := awsLoadOptions(tt.region, tt.profile)
			if len(got) != tt.wantLen {
				t.Fatalf("expected %d options, got %d", tt.wantLen, len(got))
			}
		})
	}
}
