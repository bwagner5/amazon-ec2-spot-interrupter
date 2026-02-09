package mockaws

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

type ScaleProfile string

const (
	ScaleSmall  ScaleProfile = "small"
	ScaleMedium ScaleProfile = "medium"
	ScaleLarge  ScaleProfile = "large"
)

type Config struct {
	Scale            ScaleProfile
	Seed             int64
	InstanceCount    int
	Regions          []string
	TickInterval     time.Duration
	MinRunTime       time.Duration
	MaxRunTime       time.Duration
	ChurnProbability float64
	FISWarningWindow time.Duration
}

type Instance struct {
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

	terminateAt time.Time
	gcAt        time.Time
}

type Experiment struct {
	ID         string    `json:"id"`
	TemplateID string    `json:"template_id"`
	State      string    `json:"state"`
	Region     string    `json:"region"`
	InstanceID []string  `json:"instance_ids"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Delay      time.Duration

	warningAt     time.Time
	terminateAt   time.Time
	shutdownStart time.Time
	rebalanceSent bool
	warningSent   bool
}

type Event struct {
	ExperimentID string    `json:"experiment_id"`
	Timestamp    time.Time `json:"timestamp"`
	Message      string    `json:"message"`
}

type Simulator struct {
	mu          sync.RWMutex
	cfg         Config
	rng         *rand.Rand
	instances   map[string]*Instance
	experiments map[string]*Experiment
	events      map[string][]Event
	roleARN     string
	stopped     chan struct{}
}

var defaultRegionCatalog = []string{
	"af-south-1", "ap-east-1", "ap-east-2", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-3", "ap-southeast-4",
	"ap-southeast-5", "ap-southeast-7", "ca-central-1", "ca-west-1", "eu-central-1", "eu-central-2",
	"eu-north-1", "eu-south-1", "eu-south-2", "eu-west-1", "eu-west-2", "eu-west-3",
	"il-central-1", "me-central-1", "me-south-1", "mx-central-1", "sa-east-1",
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
}

func DefaultConfig(scale ScaleProfile) Config {
	cfg := Config{
		Scale:            scale,
		Seed:             time.Now().UnixNano(),
		Regions:          []string{"us-east-1", "us-west-2", "eu-west-1"},
		TickInterval:     2 * time.Second,
		MinRunTime:       20 * time.Minute,
		MaxRunTime:       2 * time.Hour,
		ChurnProbability: 0.01,
		FISWarningWindow: 2 * time.Minute,
	}
	switch scale {
	case ScaleMedium:
		cfg.InstanceCount = 250
	case ScaleLarge:
		cfg.InstanceCount = 2000
	default:
		cfg.InstanceCount = 40
	}
	return cfg
}

func New(cfg Config) *Simulator {
	if cfg.InstanceCount <= 0 {
		cfg.InstanceCount = 20
	}
	if len(cfg.Regions) == 0 {
		cfg.Regions = []string{"us-east-1"}
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 2 * time.Second
	}
	if cfg.MinRunTime <= 0 {
		cfg.MinRunTime = 10 * time.Minute
	}
	if cfg.MaxRunTime <= cfg.MinRunTime {
		cfg.MaxRunTime = cfg.MinRunTime + 30*time.Minute
	}
	if cfg.FISWarningWindow <= 0 {
		cfg.FISWarningWindow = 2 * time.Minute
	}
	return &Simulator{
		cfg:         cfg,
		rng:         rand.New(rand.NewSource(cfg.Seed)),
		instances:   map[string]*Instance{},
		experiments: map[string]*Experiment{},
		events:      map[string][]Event{},
		roleARN:     "arn:aws:iam::123456789012:role/aws-fis-itn",
		stopped:     make(chan struct{}),
	}
}

func (s *Simulator) Start() {
	s.mu.Lock()
	for len(s.instances) < s.cfg.InstanceCount {
		s.spawnLocked()
	}
	s.mu.Unlock()
	go s.loop()
}

func (s *Simulator) Stop() {
	close(s.stopped)
}

func (s *Simulator) loop() {
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopped:
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

func (s *Simulator) tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Natural churn and replacement.
	for id, inst := range s.instances {
		if inst.State == "terminated" {
			if !inst.gcAt.IsZero() && now.After(inst.gcAt) {
				delete(s.instances, id)
			}
			continue
		}
		if inst.State != "running" {
			continue
		}
		if now.After(inst.terminateAt) || s.rng.Float64() < s.cfg.ChurnProbability {
			inst.State = "terminated"
			inst.gcAt = now.Add(10 * time.Minute)
		}
	}

	runningCount := 0
	for _, inst := range s.instances {
		if inst.State == "running" {
			runningCount++
		}
	}
	for runningCount < s.cfg.InstanceCount {
		s.spawnLocked()
		runningCount++
	}

	// Experiment progression.
	for _, exp := range s.experiments {
		if exp.State == "stopped" || exp.State == "completed" || exp.State == "failed" {
			continue
		}
		if !exp.rebalanceSent {
			exp.rebalanceSent = true
			exp.State = "running"
			s.addEventLocked(exp.ID, now, "✅ Rebalance Recommendation sent")
		}
		if !exp.warningSent && !now.Before(exp.warningAt) {
			exp.warningSent = true
			s.addEventLocked(exp.ID, now, "✅ Spot 2-minute Interruption Notification sent")
		}
		if exp.shutdownStart.IsZero() && !now.Before(exp.terminateAt) {
			exp.shutdownStart = now
			for _, id := range exp.InstanceID {
				if inst, ok := s.instances[id]; ok {
					inst.State = "shutting-down"
				}
			}
			s.addEventLocked(exp.ID, now, "🔻 Instances transitioning to shutting-down")
		}
		if !exp.shutdownStart.IsZero() && now.Sub(exp.shutdownStart) > 12*time.Second {
			for _, id := range exp.InstanceID {
				if inst, ok := s.instances[id]; ok {
					inst.State = "terminated"
					inst.gcAt = now.Add(10 * time.Minute)
				}
			}
			exp.State = "completed"
			exp.UpdatedAt = now
			s.addEventLocked(exp.ID, now, "✅ Spot Instance Shutdown sent")
		}
	}
}

func (s *Simulator) spawnLocked() {
	region := s.cfg.Regions[s.rng.Intn(len(s.cfg.Regions))]
	az := region + string(rune('a'+s.rng.Intn(3)))
	id := "i-" + randHex(s.rng, 8)
	pools := []string{"payments", "api", "worker", "batch", "analytics"}
	pool := pools[s.rng.Intn(len(pools))]
	name := fmt.Sprintf("%s-%s", pool, randHex(s.rng, 3))
	types := []string{"m6i.large", "m6i.xlarge", "c7g.large", "c7g.xlarge", "r6i.large"}
	launch := time.Now().UTC()
	lifetime := s.cfg.MinRunTime + time.Duration(s.rng.Int63n(int64(s.cfg.MaxRunTime-s.cfg.MinRunTime)))
	a := s.rng.Intn(255)
	b := s.rng.Intn(255)
	c := s.rng.Intn(255)
	d := s.rng.Intn(255)
	s.instances[id] = &Instance{
		InstanceID: id,
		Name:       name,
		Region:     region,
		AZ:         az,
		PrivateDNS: fmt.Sprintf("ip-10-0-%d-%d.%s.compute.internal", a, b, region),
		PublicDNS:  fmt.Sprintf("ec2-%d-%d-%d-%d.%s.compute.amazonaws.com", a, b, c, d, region),
		Type:       types[s.rng.Intn(len(types))],
		Lifecycle:  "spot",
		State:      "running",
		LaunchTime: launch,
		Tags: map[string]string{
			"Name":        name,
			"Environment": []string{"dev", "staging", "prod"}[s.rng.Intn(3)],
			"Service":     pool,
			"Team":        []string{"core", "platform", "search"}[s.rng.Intn(3)],
		},
		terminateAt: launch.Add(lifetime),
	}
}

func randHex(rng *rand.Rand, bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rng.Read(b)
	return strings.ToLower(hex.EncodeToString(b))
}

func (s *Simulator) ListRegions() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]struct{}{}
	out := make([]string, 0, len(defaultRegionCatalog)+len(s.cfg.Regions))
	for _, r := range defaultRegionCatalog {
		rr := strings.TrimSpace(r)
		if rr == "" {
			continue
		}
		if _, ok := seen[rr]; ok {
			continue
		}
		seen[rr] = struct{}{}
		out = append(out, rr)
	}
	for _, r := range s.cfg.Regions {
		rr := strings.TrimSpace(r)
		if rr == "" {
			continue
		}
		if _, ok := seen[rr]; ok {
			continue
		}
		seen[rr] = struct{}{}
		out = append(out, rr)
	}
	sort.Strings(out)
	return out
}

func (s *Simulator) ListInstances(region string, state string) []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Instance{}
	for _, inst := range s.instances {
		if region != "" && inst.Region != region {
			continue
		}
		if state != "" && inst.State != state {
			continue
		}
		out = append(out, *inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

func (s *Simulator) EnsureRole(_ string) string {
	return s.roleARN
}

func (s *Simulator) StartExperiment(instanceIDs []string, delay time.Duration) Experiment {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	exp := &Experiment{
		ID:          "EXP" + strings.ToUpper(randHex(s.rng, 5)),
		TemplateID:  "TPL" + strings.ToUpper(randHex(s.rng, 5)),
		State:       "initiating",
		Region:      s.firstRegionLocked(instanceIDs),
		InstanceID:  append([]string{}, instanceIDs...),
		CreatedAt:   now,
		UpdatedAt:   now,
		Delay:       delay,
		warningAt:   now.Add(delay),
		terminateAt: now.Add(delay + s.cfg.FISWarningWindow),
	}
	s.experiments[exp.ID] = exp
	s.events[exp.ID] = []Event{
		{ExperimentID: exp.ID, Timestamp: now, Message: "📖 Experiment created"},
	}
	return *exp
}

func (s *Simulator) firstRegionLocked(instanceIDs []string) string {
	for _, id := range instanceIDs {
		if inst, ok := s.instances[id]; ok {
			return inst.Region
		}
	}
	return s.cfg.Regions[0]
}

func (s *Simulator) StopExperiment(id string) (Experiment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.experiments[id]
	if !ok {
		return Experiment{}, false
	}
	exp.State = "stopped"
	exp.UpdatedAt = time.Now().UTC()
	s.addEventLocked(id, exp.UpdatedAt, "🛑 Experiment stopped by caller")
	return *exp, true
}

func (s *Simulator) GetExperiment(id string) (Experiment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	exp, ok := s.experiments[id]
	if !ok {
		return Experiment{}, false
	}
	return *exp, true
}

func (s *Simulator) Events(id string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Event{}, s.events[id]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

func (s *Simulator) addEventLocked(id string, ts time.Time, msg string) {
	s.events[id] = append(s.events[id], Event{
		ExperimentID: id,
		Timestamp:    ts,
		Message:      msg,
	})
}
