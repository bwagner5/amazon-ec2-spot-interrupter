package itn

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	fistypes "github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const mockEndpointAccountID = "123456789012"

var (
	mockEndpointProbeCache sync.Map
	ptSecondsRegex         = regexp.MustCompile(`^PT(\d+)S$`)
)

func isMockEndpoint(endpoint string) bool {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return false
	}
	if cached, ok := mockEndpointProbeCache.Load(e); ok {
		return cached.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(e, "/healthz", nil), nil)
	if err != nil {
		mockEndpointProbeCache.Store(e, false)
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		mockEndpointProbeCache.Store(e, false)
		return false
	}
	defer resp.Body.Close()
	ok := resp.StatusCode == http.StatusOK
	mockEndpointProbeCache.Store(e, ok)
	return ok
}

func newMockEndpointITN(cfg aws.Config, endpoint string) *ITN {
	return &ITN{
		cfg:       cfg,
		endpoint:  endpoint,
		stsClient: &mockSTSClient{accountID: mockEndpointAccountID},
		iamClient: &mockIAMClient{endpoint: endpoint, accountID: mockEndpointAccountID},
		fisClient: newMockFISClient(endpoint, cfg.Region, mockEndpointAccountID),
		ec2Client: &mockEC2Client{endpoint: endpoint, region: cfg.Region},
	}
}

type mockSTSClient struct {
	accountID string
}

func (m *mockSTSClient) GetCallerIdentity(ctx context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	account := m.accountID
	arn := fmt.Sprintf("arn:aws:sts::%s:assumed-role/mock-user/session", account)
	return &sts.GetCallerIdentityOutput{
		Account: &account,
		Arn:     &arn,
	}, nil
}

type mockIAMClient struct {
	endpoint  string
	accountID string
}

func (m *mockIAMClient) CreateRole(ctx context.Context, params *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	roleName := "aws-fis-itn"
	if params != nil && params.RoleName != nil && strings.TrimSpace(*params.RoleName) != "" {
		roleName = *params.RoleName
	}
	var roleResp struct {
		RoleARN string `json:"role_arn"`
	}
	_ = mockRequestJSON(ctx, m.endpoint, http.MethodPost, "/api/iam/role", url.Values{"name": []string{roleName}}, nil, &roleResp)
	if strings.TrimSpace(roleResp.RoleARN) == "" {
		roleResp.RoleARN = fmt.Sprintf("arn:aws:iam::%s:role/%s", m.accountID, roleName)
	}
	return &iam.CreateRoleOutput{
		Role: &iamtypes.Role{
			Arn:      aws.String(roleResp.RoleARN),
			RoleName: aws.String(roleName),
		},
	}, nil
}

func (m *mockIAMClient) PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}

type mockEC2Client struct {
	endpoint string
	region   string
}

type mockInstance struct {
	InstanceID string            `json:"instance_id"`
	Name       string            `json:"name"`
	Region     string            `json:"region"`
	AZ         string            `json:"az"`
	PrivateDNS string            `json:"private_dns"`
	PublicDNS  string            `json:"public_dns"`
	Type       string            `json:"type"`
	Lifecycle  string            `json:"lifecycle"`
	State      string            `json:"state"`
	LaunchTime time.Time         `json:"launch_time"`
	Tags       map[string]string `json:"tags"`
}

func (m *mockEC2Client) DescribeRegions(ctx context.Context, _ *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	var resp struct {
		Regions []string `json:"regions"`
	}
	if err := mockRequestJSON(ctx, m.endpoint, http.MethodGet, "/api/ec2/regions", nil, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]ec2types.Region, 0, len(resp.Regions))
	for _, r := range resp.Regions {
		rr := strings.TrimSpace(r)
		if rr == "" {
			continue
		}
		out = append(out, ec2types.Region{RegionName: aws.String(rr)})
	}
	return &ec2.DescribeRegionsOutput{Regions: out}, nil
}

func (m *mockEC2Client) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	query := url.Values{}
	if rr := strings.TrimSpace(m.region); rr != "" && !strings.EqualFold(rr, "global") {
		query.Set("region", rr)
	}
	var resp struct {
		Instances []mockInstance `json:"instances"`
	}
	if err := mockRequestJSON(ctx, m.endpoint, http.MethodGet, "/api/ec2/instances", query, nil, &resp); err != nil {
		return nil, err
	}
	if in != nil && len(in.InstanceIds) > 0 {
		found := map[string]struct{}{}
		for _, inst := range resp.Instances {
			found[inst.InstanceID] = struct{}{}
		}
		for _, requested := range in.InstanceIds {
			if _, ok := found[requested]; !ok {
				return nil, fmt.Errorf("InvalidInstanceID.NotFound: The instance ID '%s' does not exist", requested)
			}
		}
	}
	filtered := make([]ec2types.Instance, 0, len(resp.Instances))
	for _, inst := range resp.Instances {
		if !matchesDescribeInstancesInput(inst, in) {
			continue
		}
		filtered = append(filtered, toEC2Instance(inst))
	}
	sort.Slice(filtered, func(i, j int) bool {
		return aws.ToString(filtered[i].InstanceId) < aws.ToString(filtered[j].InstanceId)
	})
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{
			{
				ReservationId: aws.String("r-" + randHex(8)),
				Instances:     filtered,
			},
		},
	}, nil
}

func matchesDescribeInstancesInput(inst mockInstance, in *ec2.DescribeInstancesInput) bool {
	if in == nil {
		return true
	}
	if len(in.InstanceIds) > 0 {
		match := false
		for _, id := range in.InstanceIds {
			if id == inst.InstanceID {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	for _, f := range in.Filters {
		if !matchesFilter(inst, f) {
			return false
		}
	}
	return true
}

func matchesFilter(inst mockInstance, f ec2types.Filter) bool {
	name := strings.TrimSpace(aws.ToString(f.Name))
	values := f.Values
	switch {
	case name == "instance-lifecycle":
		return anyValueMatch(values, inst.Lifecycle)
	case name == "instance-state-name":
		return anyValueMatch(values, inst.State)
	case name == "instance-id":
		return anyValueMatch(values, inst.InstanceID)
	case name == "private-dns-name":
		return anyValueMatch(values, inst.PrivateDNS)
	case name == "dns-name":
		return anyValueMatch(values, inst.PublicDNS)
	case strings.HasPrefix(name, "tag:"):
		key := strings.TrimPrefix(name, "tag:")
		val, ok := inst.Tags[key]
		if !ok {
			return false
		}
		if len(values) == 0 {
			return true
		}
		return anyValueMatch(values, val)
	default:
		return false
	}
}

func anyValueMatch(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, p := range patterns {
		if matchPattern(p, value) {
			return true
		}
	}
	return false
}

func matchPattern(pattern string, value string) bool {
	if pattern == value {
		return true
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		return false
	}
	return ok
}

func toEC2Instance(inst mockInstance) ec2types.Instance {
	launch := inst.LaunchTime
	tags := make([]ec2types.Tag, 0, len(inst.Tags))
	keys := make([]string, 0, len(inst.Tags))
	for k := range inst.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := inst.Tags[k]
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return ec2types.Instance{
		InstanceId:        aws.String(inst.InstanceID),
		InstanceLifecycle: ec2types.InstanceLifecycleTypeSpot,
		InstanceType:      ec2types.InstanceType(inst.Type),
		State:             &ec2types.InstanceState{Name: ec2types.InstanceStateName(inst.State)},
		Placement:         &ec2types.Placement{AvailabilityZone: aws.String(inst.AZ)},
		Tags:              tags,
		PrivateDnsName:    aws.String(inst.PrivateDNS),
		PublicDnsName:     aws.String(inst.PublicDNS),
		LaunchTime:        &launch,
	}
}

type mockFISClient struct {
	mu        sync.Mutex
	endpoint  string
	region    string
	accountID string
	templates map[string]fisTemplate
	expMeta   map[string]fisExperimentMeta
}

type fisTemplate struct {
	RoleARN     string
	InstanceIDs []string
	Delay       time.Duration
}

type fisExperimentMeta struct {
	TemplateID  string
	RoleARN     string
	InstanceIDs []string
}

type mockExperimentResponse struct {
	ID         string        `json:"id"`
	TemplateID string        `json:"template_id"`
	State      string        `json:"state"`
	Region     string        `json:"region"`
	InstanceID []string      `json:"instance_ids"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Delay      time.Duration `json:"delay"`
}

func newMockFISClient(endpoint, region, accountID string) *mockFISClient {
	return &mockFISClient{
		endpoint:  endpoint,
		region:    region,
		accountID: accountID,
		templates: map[string]fisTemplate{},
		expMeta:   map[string]fisExperimentMeta{},
	}
}

func (m *mockFISClient) CreateExperimentTemplate(_ context.Context, params *fis.CreateExperimentTemplateInput, _ ...func(*fis.Options)) (*fis.CreateExperimentTemplateOutput, error) {
	id := "tpl-" + randHex(5)
	delay := parseDelayFromTemplate(params)
	ids := extractInstanceIDsFromTemplate(params)
	roleArn := aws.ToString(params.RoleArn)
	if strings.TrimSpace(roleArn) == "" {
		roleArn = fmt.Sprintf("arn:aws:iam::%s:role/aws-fis-itn", m.accountID)
	}
	m.mu.Lock()
	m.templates[id] = fisTemplate{
		RoleARN:     roleArn,
		InstanceIDs: ids,
		Delay:       delay,
	}
	m.mu.Unlock()
	return &fis.CreateExperimentTemplateOutput{
		ExperimentTemplate: &fistypes.ExperimentTemplate{
			Id:      aws.String(id),
			RoleArn: aws.String(roleArn),
		},
	}, nil
}

func (m *mockFISClient) DeleteExperimentTemplate(_ context.Context, params *fis.DeleteExperimentTemplateInput, _ ...func(*fis.Options)) (*fis.DeleteExperimentTemplateOutput, error) {
	if params != nil && params.Id != nil {
		m.mu.Lock()
		delete(m.templates, *params.Id)
		m.mu.Unlock()
	}
	return &fis.DeleteExperimentTemplateOutput{}, nil
}

func (m *mockFISClient) StartExperiment(ctx context.Context, params *fis.StartExperimentInput, _ ...func(*fis.Options)) (*fis.StartExperimentOutput, error) {
	if params == nil || params.ExperimentTemplateId == nil {
		return nil, fmt.Errorf("missing experiment template id")
	}
	templateID := *params.ExperimentTemplateId
	m.mu.Lock()
	tpl, ok := m.templates[templateID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("template not found: %s", templateID)
	}
	req := map[string]any{
		"instance_ids": tpl.InstanceIDs,
		"delay":        tpl.Delay.String(),
	}
	var resp mockExperimentResponse
	if err := mockRequestJSON(ctx, m.endpoint, http.MethodPost, "/api/fis/experiments", nil, req, &resp); err != nil {
		return nil, err
	}
	exp := m.toSDKExperiment(resp, fisExperimentMeta{
		TemplateID:  templateID,
		RoleARN:     tpl.RoleARN,
		InstanceIDs: tpl.InstanceIDs,
	})
	m.mu.Lock()
	m.expMeta[resp.ID] = fisExperimentMeta{
		TemplateID:  templateID,
		RoleARN:     tpl.RoleARN,
		InstanceIDs: tpl.InstanceIDs,
	}
	m.mu.Unlock()
	return &fis.StartExperimentOutput{Experiment: exp}, nil
}

func (m *mockFISClient) GetExperiment(ctx context.Context, params *fis.GetExperimentInput, _ ...func(*fis.Options)) (*fis.GetExperimentOutput, error) {
	if params == nil || params.Id == nil {
		return nil, fmt.Errorf("missing experiment id")
	}
	id := *params.Id
	var resp mockExperimentResponse
	if err := mockRequestJSON(ctx, m.endpoint, http.MethodGet, "/api/fis/experiments/"+id, nil, nil, &resp); err != nil {
		return nil, err
	}
	m.mu.Lock()
	meta := m.expMeta[id]
	m.mu.Unlock()
	return &fis.GetExperimentOutput{Experiment: m.toSDKExperiment(resp, meta)}, nil
}

func (m *mockFISClient) StopExperiment(ctx context.Context, params *fis.StopExperimentInput, _ ...func(*fis.Options)) (*fis.StopExperimentOutput, error) {
	if params == nil || params.Id == nil {
		return nil, fmt.Errorf("missing experiment id")
	}
	id := *params.Id
	var resp mockExperimentResponse
	if err := mockRequestJSON(ctx, m.endpoint, http.MethodPost, "/api/fis/experiments/"+id+"/stop", nil, nil, &resp); err != nil {
		return nil, err
	}
	m.mu.Lock()
	meta := m.expMeta[id]
	m.mu.Unlock()
	return &fis.StopExperimentOutput{Experiment: m.toSDKExperiment(resp, meta)}, nil
}

func (m *mockFISClient) toSDKExperiment(resp mockExperimentResponse, meta fisExperimentMeta) *fistypes.Experiment {
	ids := append([]string{}, meta.InstanceIDs...)
	if len(ids) == 0 {
		ids = append(ids, resp.InstanceID...)
	}
	region := strings.TrimSpace(resp.Region)
	if region == "" {
		region = strings.TrimSpace(m.region)
	}
	if region == "" || strings.EqualFold(region, "global") {
		region = "us-east-1"
	}
	roleArn := strings.TrimSpace(meta.RoleARN)
	if roleArn == "" {
		roleArn = fmt.Sprintf("arn:aws:iam::%s:role/aws-fis-itn", m.accountID)
	}
	templateID := strings.TrimSpace(meta.TemplateID)
	if templateID == "" {
		templateID = strings.TrimSpace(resp.TemplateID)
	}
	targetArns := make([]string, 0, len(ids))
	for _, id := range ids {
		targetArns = append(targetArns, fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", region, m.accountID, id))
	}
	created := resp.CreatedAt
	status := mapExperimentStatus(resp.State)
	return &fistypes.Experiment{
		Id:                   aws.String(resp.ID),
		ExperimentTemplateId: aws.String(templateID),
		RoleArn:              aws.String(roleArn),
		CreationTime:         &created,
		State: &fistypes.ExperimentState{
			Status: status,
		},
		Targets: map[string]fistypes.ExperimentTarget{
			"SpotInstances": {
				ResourceArns:  targetArns,
				ResourceType:  aws.String("aws:ec2:spot-instance"),
				SelectionMode: aws.String("ALL"),
			},
		},
		StopConditions: []fistypes.ExperimentStopCondition{
			{Source: aws.String("none")},
		},
	}
}

func mapExperimentStatus(state string) fistypes.ExperimentStatus {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "initiating":
		return fistypes.ExperimentStatusInitiating
	case "running":
		return fistypes.ExperimentStatusRunning
	case "completed":
		return fistypes.ExperimentStatusCompleted
	case "stopping":
		return fistypes.ExperimentStatusStopping
	case "stopped":
		return fistypes.ExperimentStatusStopped
	case "failed":
		return fistypes.ExperimentStatusFailed
	case "cancelled":
		return fistypes.ExperimentStatusCancelled
	default:
		return fistypes.ExperimentStatusPending
	}
}

func extractInstanceIDsFromTemplate(params *fis.CreateExperimentTemplateInput) []string {
	ids := map[string]struct{}{}
	if params == nil {
		return nil
	}
	for _, t := range params.Targets {
		for _, arn := range t.ResourceArns {
			id := ARNToInstanceID(arn)
			if strings.TrimSpace(id) != "" {
				ids[id] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func parseDelayFromTemplate(params *fis.CreateExperimentTemplateInput) time.Duration {
	if params == nil {
		return 0
	}
	for _, a := range params.Actions {
		raw := strings.TrimSpace(a.Parameters["durationBeforeInterruption"])
		if raw == "" {
			continue
		}
		matches := ptSecondsRegex.FindStringSubmatch(raw)
		if len(matches) != 2 {
			continue
		}
		seconds, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		delay := time.Duration(seconds) * time.Second
		delay -= 2 * time.Minute
		if delay < 0 {
			return 0
		}
		return delay
	}
	return 0
}

func mockRequestJSON(ctx context.Context, endpoint string, method string, requestPath string, query url.Values, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURL(endpoint, requestPath, query), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mock endpoint %s %s failed: status %d body=%s", method, requestPath, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func joinURL(endpoint string, requestPath string, query url.Values) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	p := "/" + strings.TrimLeft(requestPath, "/")
	u := base + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func randHex(size int) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, size)
	_, _ = r.Read(b)
	return strings.ToLower(hex.EncodeToString(b))
}
