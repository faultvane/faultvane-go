package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

type mode struct {
	Status  int `json:"status"`
	DelayMs int `json:"delayMs"`
}

type stats struct {
	Total         int            `json:"total"`
	ByPod         map[string]int `json:"byPod"`
	InFlight      int            `json:"inFlight"`
	MaxConcurrent int            `json:"maxConcurrent"`
	MaxByPod      map[string]int `json:"maxByPod"`
}

type dependency struct {
	mu        sync.Mutex
	mode      mode
	total     int
	byPod     map[string]int
	inFlight  int
	maxTotal  int
	podFlight map[string]int
	maxByPod  map[string]int
}

func newDependency() *dependency {
	d := &dependency{mode: mode{Status: http.StatusOK}}
	d.reset()
	return d
}

func (d *dependency) reset() {
	d.total, d.maxTotal = 0, d.inFlight
	d.byPod, d.maxByPod = map[string]int{}, map[string]int{}
	if d.podFlight == nil {
		d.podFlight = map[string]int{}
	}
}

func (d *dependency) work(w http.ResponseWriter, r *http.Request) {
	pod := r.URL.Query().Get("pod")
	d.mu.Lock()
	current := d.mode
	d.total++
	d.byPod[pod]++
	d.inFlight++
	d.podFlight[pod]++
	d.maxTotal = max(d.maxTotal, d.inFlight)
	d.maxByPod[pod] = max(d.maxByPod[pod], d.podFlight[pod])
	d.mu.Unlock()

	select {
	case <-time.After(time.Duration(current.DelayMs) * time.Millisecond):
	case <-r.Context().Done():
	}

	d.mu.Lock()
	d.inFlight--
	d.podFlight[pod]--
	d.mu.Unlock()
	w.WriteHeader(current.Status)
}

func (d *dependency) setMode(w http.ResponseWriter, r *http.Request) {
	var next mode
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil || next.Status == 0 {
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.mode = next
	d.mu.Unlock()
}

func (d *dependency) stats(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	result := stats{Total: d.total, ByPod: map[string]int{}, InFlight: d.inFlight, MaxConcurrent: d.maxTotal, MaxByPod: map[string]int{}}
	for pod, count := range d.byPod {
		result.ByPod[pod] = count
	}
	for pod, count := range d.maxByPod {
		result.MaxByPod[pod] = count
	}
	d.mu.Unlock()
	_ = json.NewEncoder(w).Encode(result)
}

func main() {
	d := newDependency()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /work", d.work)
	mux.HandleFunc("POST /mode", d.setMode)
	mux.HandleFunc("GET /stats", d.stats)
	mux.HandleFunc("POST /reset", func(http.ResponseWriter, *http.Request) {
		d.mu.Lock()
		d.reset()
		d.mu.Unlock()
	})
	mux.HandleFunc("GET /ready", func(http.ResponseWriter, *http.Request) {})
	log.Fatal(http.ListenAndServe(":8080", mux))
}
