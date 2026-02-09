package mockaws

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	sim *Simulator
	mux *http.ServeMux
}

func NewServer(sim *Simulator) *Server {
	s := &Server{
		sim: sim,
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleEC2QueryAPI)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/ec2/regions", s.handleRegions)
	s.mux.HandleFunc("/api/ec2/instances", s.handleInstances)
	s.mux.HandleFunc("/api/iam/role", s.handleRole)
	s.mux.HandleFunc("/api/fis/experiments", s.handleExperiments)
	s.mux.HandleFunc("/api/fis/experiments/", s.handleExperimentByID)
}

func (s *Server) handleEC2QueryAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	action := strings.TrimSpace(r.URL.Query().Get("Action"))
	if action == "" {
		_ = r.ParseForm()
		action = strings.TrimSpace(r.Form.Get("Action"))
	}
	switch action {
	case "DescribeRegions":
		s.handleDescribeRegionsXML(w)
		return
	default:
		http.NotFound(w, r)
		return
	}
}

type describeRegionsResponse struct {
	XMLName   xml.Name          `xml:"DescribeRegionsResponse"`
	Xmlns     string            `xml:"xmlns,attr"`
	RequestID string            `xml:"requestId"`
	RegionSet describeRegionSet `xml:"regionInfo"`
}

type describeRegionSet struct {
	Items []describeRegionItem `xml:"item"`
}

type describeRegionItem struct {
	RegionName string `xml:"regionName"`
	Endpoint   string `xml:"regionEndpoint"`
}

func (s *Server) handleDescribeRegionsXML(w http.ResponseWriter) {
	regions := s.sim.ListRegions()
	items := make([]describeRegionItem, 0, len(regions))
	for _, region := range regions {
		items = append(items, describeRegionItem{
			RegionName: region,
			Endpoint:   fmt.Sprintf("ec2.%s.amazonaws.com", region),
		})
	}
	payload := describeRegionsResponse{
		Xmlns:     "http://ec2.amazonaws.com/doc/2016-11-15/",
		RequestID: fmt.Sprintf("mock-%d", time.Now().UnixNano()),
		RegionSet: describeRegionSet{Items: items},
	}
	w.Header().Set("Content-Type", "text/xml; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(payload)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRegions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"regions": s.sim.ListRegions()})
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	region := strings.TrimSpace(r.URL.Query().Get("region"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	instances := s.sim.ListInstances(region, state)
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances, "count": len(instances)})
}

func (s *Server) handleRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		name = "aws-fis-itn"
	}
	writeJSON(w, http.StatusOK, map[string]any{"role_name": name, "role_arn": s.sim.EnsureRole(name)})
}

type startExperimentRequest struct {
	InstanceIDs []string `json:"instance_ids"`
	Delay       string   `json:"delay"`
}

func (s *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req startExperimentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	delay := 15 * time.Second
	if strings.TrimSpace(req.Delay) != "" {
		parsed, err := time.ParseDuration(req.Delay)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid delay duration"})
			return
		}
		delay = parsed
	}
	exp := s.sim.StartExperiment(req.InstanceIDs, delay)
	writeJSON(w, http.StatusCreated, exp)
}

func (s *Server) handleExperimentByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/fis/experiments/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "experiment not found"})
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "stop" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		exp, ok := s.sim.StopExperiment(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "experiment not found"})
			return
		}
		writeJSON(w, http.StatusOK, exp)
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"experiment_id": id, "events": s.sim.Events(id)})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	exp, ok := s.sim.GetExperiment(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "experiment not found"})
		return
	}
	writeJSON(w, http.StatusOK, exp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
