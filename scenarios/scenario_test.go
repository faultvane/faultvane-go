package scenarios

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pods        = 10
	sharedRate  = 500
	sharedBurst = 50
	localRate   = 20
	localBurst  = 2
	usedPart    = 0.7
)

var (
	namespaceCounter atomic.Uint64
	client           = &http.Client{Timeout: 5 * time.Minute}
)

type result struct {
	OK         int            `json:"ok"`
	Failed     int            `json:"failed"`
	Timeouts   int            `json:"timeouts"`
	Rejected   map[string]int `json:"rejected"`
	DurationMs float64        `json:"durationMs"`
	P50Us      float64        `json:"p50Us"`
	P99Us      float64        `json:"p99Us"`
	P999Us     float64        `json:"p999Us"`
}

type status struct {
	Pod     string `json:"pod"`
	State   string `json:"state"`
	Mode    string `json:"mode"`
	Breaker struct {
		Failures         int64
		LocalModeEntries int64
		Opened           int64
		HalfOpened       int64
		Closed           int64
	} `json:"breaker"`
	StoreOperations int64  `json:"storeOperations"`
	Limit           int    `json:"limit"`
	RateLimitMode   string `json:"rateLimitMode"`
	RateLimit       struct {
		StoreErrors int64
		Allowance   int
	} `json:"rateLimit"`
}

type dependencyStats struct {
	Total         int            `json:"total"`
	ByPod         map[string]int `json:"byPod"`
	MaxConcurrent int            `json:"maxConcurrent"`
	MaxByPod      map[string]int `json:"maxByPod"`
}

type cluster struct {
	t          *testing.T
	pods       []string
	dependency string
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func compose(t *testing.T, args ...string) string {
	t.Helper()
	return run(t, "docker", append([]string{"compose", "-f", "docker-compose.yml"}, args...)...)
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func request(method, url string, body, out any) error {
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, url, &payload)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		var text bytes.Buffer
		_, _ = text.ReadFrom(response.Body)
		return fmt.Errorf("%s %s: status %d: %s", method, url, response.StatusCode, text.String())
	}
	if out != nil {
		return json.NewDecoder(response.Body).Decode(out)
	}
	return nil
}

func (c *cluster) must(err error) {
	c.t.Helper()
	if err != nil {
		c.t.Fatal(err)
	}
}

func address(t *testing.T, service string, index int) string {
	t.Helper()
	mapped := compose(t, "port", "--index", fmt.Sprint(index), service, "8080")
	return "http://127.0.0.1" + mapped[strings.LastIndex(mapped, ":"):]
}

func start(t *testing.T) *cluster {
	t.Helper()
	for _, binary := range []string{"pod", "dependency"} {
		build := exec.Command("go", "build", "-o", "bin/"+binary, "./cmd/"+binary)
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", binary, err, output)
		}
	}
	compose(t, "up", "-d", "--build", "--scale", fmt.Sprintf("pod=%d", pods))
	t.Cleanup(func() {
		if os.Getenv("FAULTVANE_SCENARIOS_KEEP") == "" {
			compose(t, "down", "-v", "--remove-orphans")
		}
	})
	c := &cluster{t: t, dependency: address(t, "dependency", 1)}
	for i := 1; i <= pods; i++ {
		c.pods = append(c.pods, address(t, "pod", i))
	}
	for _, url := range append([]string{c.dependency}, c.pods...) {
		eventually(t, url+" is ready", func() bool { return request(http.MethodGet, url+"/ready", nil, nil) == nil })
	}
	return c
}

func (c *cluster) mode(statusCode, delayMs int) {
	c.t.Helper()
	c.must(request(http.MethodPost, c.dependency+"/mode", map[string]int{"status": statusCode, "delayMs": delayMs}, nil))
	c.must(request(http.MethodPost, c.dependency+"/reset", nil, nil))
}

func (c *cluster) dependencyStats() dependencyStats {
	c.t.Helper()
	var stats dependencyStats
	c.must(request(http.MethodGet, c.dependency+"/stats", nil, &stats))
	return stats
}

func workers() int {
	return max(1, 2*runtime.NumCPU()/pods)
}

func namespace() string {
	return fmt.Sprintf("scenario-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))
}

func (c *cluster) setup(configuration map[string]any, indexes ...int) {
	c.t.Helper()
	if len(indexes) == 0 {
		for i := range c.pods {
			indexes = append(indexes, i)
		}
	}
	for _, i := range indexes {
		eventually(c.t, fmt.Sprintf("setup of pod %d", i), func() bool {
			err := request(http.MethodPost, c.pods[i]+"/setup", configuration, nil)
			if err != nil {
				c.t.Logf("setup of pod %d: %v", i, err)
				time.Sleep(time.Second)
			}
			return err == nil
		})
	}
}

func (c *cluster) load(index int, load map[string]any) result {
	c.t.Helper()
	var r result
	c.must(request(http.MethodPost, c.pods[index]+"/load", load, &r))
	return r
}

func (c *cluster) loadAll(load map[string]any) []result {
	c.t.Helper()
	results := make([]result, len(c.pods))
	errs := make([]error, len(c.pods))
	var group sync.WaitGroup
	for i := range c.pods {
		group.Add(1)
		go func() {
			defer group.Done()
			errs[i] = request(http.MethodPost, c.pods[i]+"/load", load, &results[i])
		}()
	}
	group.Wait()
	for _, err := range errs {
		c.must(err)
	}
	return results
}

func (c *cluster) status(index int) status {
	c.t.Helper()
	var s status
	c.must(request(http.MethodGet, c.pods[index]+"/stats", nil, &s))
	return s
}

func (c *cluster) all(state, mode string) func() bool {
	return func() bool {
		for i := range c.pods {
			if s := c.status(i); s.State != state || s.Mode != mode {
				return false
			}
		}
		return true
	}
}

func breaker(store string, extra map[string]any) map[string]any {
	settings := map[string]any{"failureThreshold": 5, "syncMs": 100}
	for key, value := range extra {
		settings[key] = value
	}
	return map[string]any{"namespace": namespace(), "store": store, "breaker": settings}
}

func total(results []result) (ok, failed int, rejected map[string]int) {
	rejected = map[string]int{}
	for _, r := range results {
		ok += r.OK
		failed += r.Failed
		for kind, count := range r.Rejected {
			rejected[kind] += count
		}
	}
	return ok, failed, rejected
}

func (c *cluster) openAll(configuration map[string]any) {
	c.t.Helper()
	c.mode(http.StatusServiceUnavailable, 0)
	c.setup(configuration)
	c.load(0, map[string]any{"calls": 5})
	eventually(c.t, "all pods are open", c.all("open", "distributed"))
}

func (c *cluster) loseData(store string) {
	c.t.Helper()
	switch store {
	case "redis":
		compose(c.t, "exec", "-T", "redis", "redis-cli", "flushall")
	case "postgres":
		compose(c.t, "exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "faultvane", "-c", "DELETE FROM faultvane_circuit_breakers")
	case "mysql":
		compose(c.t, "exec", "-T", "mysql", "mysql", "-uroot", "-pfaultvane", "faultvane", "-e", "DELETE FROM faultvane_circuit_breakers")
	default:
		compose(c.t, "exec", "-T", "mongodb", "mongosh", "--quiet", "faultvane", "--eval", "db.circuit_breakers.deleteMany({})")
	}
}

func sharedRateLimit() map[string]any {
	return map[string]any{"namespace": namespace(), "rateLimit": map[string]any{"rate": sharedRate, "burst": sharedBurst, "localRate": localRate}}
}

func heavyLoad() map[string]any {
	return map[string]any{"durationMs": 3000, "concurrency": workers(), "pauseMs": 1}
}

func (c *cluster) storeTime() time.Time {
	c.t.Helper()
	var seconds, microseconds int64
	if _, err := fmt.Sscan(compose(c.t, "exec", "-T", "redis", "redis-cli", "time"), &seconds, &microseconds); err != nil {
		c.t.Fatal(err)
	}
	return time.Unix(seconds, microseconds*1000)
}

func (c *cluster) rateLimitMode(mode string) func() bool {
	return func() bool {
		for i := range c.pods {
			if c.status(i).RateLimitMode != mode {
				return false
			}
		}
		return true
	}
}

func (c *cluster) heldTokens() (held int, idle bool) {
	idle = true
	for i := range c.pods {
		s := c.status(i)
		held += s.RateLimit.Allowance
		idle = idle && s.RateLimit.Allowance <= 1
	}
	return held, idle && c.rateLimitMode("distributed")()
}

func (c *cluster) expectSharedRate(started time.Time, held int) {
	c.t.Helper()
	ok, failed, rejected := total(c.loadAll(heavyLoad()))
	seconds := c.storeTime().Sub(started).Seconds()
	got := c.dependencyStats().Total
	most, least := sharedBurst+int(sharedRate*seconds)+held, int(usedPart*sharedRate*seconds)
	c.t.Logf("the dependency got %d calls in %.3f s of store time, limits %d and %d, %d held tokens, %d rejections", got, seconds, least, most, held, rejected["rate"])
	if got != ok || failed != 0 || rejected["rate"] == 0 || len(rejected) != 1 || got > most || got < least {
		c.t.Fatalf("the dependency got %d calls in %.3f s of store time, want from %d to %d; ok %d, failed %d, rejected %v", got, seconds, least, most, ok, failed, rejected)
	}
	if !c.rateLimitMode("distributed")() {
		c.t.Fatal("a pod is not in distributed mode after the load")
	}
}

func TestScenarios(t *testing.T) {
	if os.Getenv("FAULTVANE_SCENARIOS") == "" {
		t.Skip("set FAULTVANE_SCENARIOS=1 to run the scenarios with 10 pods in Docker")
	}
	c := start(t)
	fmt.Println("| Scenario | Store | Calls each second, all pods | p50 | p99 | p99.9 | Store operations each second, each pod |")
	fmt.Println("|---|---|---|---|---|---|---|")

	for _, store := range []string{"redis", "postgres", "mysql", "mongodb"} {
		t.Run("AT-1 FD-6 ten pods with one failure each open the circuit with "+store, func(t *testing.T) {
			c.t = t
			c.mode(http.StatusServiceUnavailable, 0)
			c.setup(breaker(store, nil))
			for i := range c.pods {
				if r := c.load(i, map[string]any{"calls": 1}); r.Failed != 1 {
					t.Fatalf("pod %d: %+v, want 1 failed call", i, r)
				}
			}
			eventually(t, "all pods are open", c.all("open", "distributed"))
			for i := range c.pods {
				if s := c.status(i); s.Breaker.Failures != 1 || s.Breaker.Opened != 1 {
					t.Fatalf("pod %d: %d failures and %d open transitions, want 1 and 1", i, s.Breaker.Failures, s.Breaker.Opened)
				}
			}
			ok, failed, rejected := total(c.loadAll(map[string]any{"calls": 20, "concurrency": 4}))
			if ok != 0 || failed != 0 || rejected["open"] != 200 {
				t.Fatalf("after the transition: ok %d, failed %d, rejected %v, want 200 rejections", ok, failed, rejected)
			}
			if stats := c.dependencyStats(); stats.Total != pods {
				t.Fatalf("the dependency got %d calls, want %d", stats.Total, pods)
			}
		})

		t.Run("GEN-1 GEN-3 ten pods that report failures at the same time open the circuit one time with "+store, func(t *testing.T) {
			c.t = t
			c.mode(http.StatusServiceUnavailable, 20)
			c.setup(breaker(store, nil))
			ok, _, _ := total(c.loadAll(map[string]any{"calls": 300, "concurrency": 10}))
			eventually(t, "all pods are open", c.all("open", "distributed"))
			for i := range c.pods {
				if s := c.status(i); s.Breaker.Opened != 1 || s.Breaker.HalfOpened != 0 || s.Breaker.Closed != 0 {
					t.Fatalf("pod %d: transitions open %d, half-open %d, closed %d, want 1, 0, 0", i, s.Breaker.Opened, s.Breaker.HalfOpened, s.Breaker.Closed)
				}
			}
			if stats := c.dependencyStats(); ok != 0 || stats.Total < 5 || stats.Total >= 3000 {
				t.Fatalf("ok %d, the dependency got %d of 3000 calls, want 0 ok and a part of the calls", ok, stats.Total)
			}
		})

		t.Run("AT-2 HO-1 exactly one probe reaches the dependency from ten pods with "+store, func(t *testing.T) {
			c.t = t
			c.openAll(breaker(store, map[string]any{"openMs": 3000}))
			c.mode(http.StatusOK, 2000)
			time.Sleep(3200 * time.Millisecond)
			ok, failed, rejected := total(c.loadAll(map[string]any{"calls": 3, "concurrency": 3}))
			if stats := c.dependencyStats(); stats.Total != 1 || ok != 1 || failed != 0 || rejected["probe"] != 29 {
				t.Fatalf("the dependency got %d calls; ok %d, failed %d, rejected %v; want 1 call, 1 ok, 29 probe rejections", stats.Total, ok, failed, rejected)
			}
			c.mode(http.StatusOK, 0)
			eventually(t, "all pods are closed", c.all("closed", "distributed"))
		})

		t.Run("RF-5 the pods restore the state after a loss of data in "+store, func(t *testing.T) {
			c.t = t
			configuration := breaker(store, nil)
			c.openAll(configuration)
			c.loseData(store)
			time.Sleep(500 * time.Millisecond)
			c.setup(configuration, pods-1)
			if s := c.status(pods - 1); s.State != "open" || s.Mode != "distributed" {
				t.Fatalf("a pod that starts after the loss reads %s in %s mode, want open in distributed mode", s.State, s.Mode)
			}
		})

		t.Run("RC-6 a pod that starts again reads the open state from "+store, func(t *testing.T) {
			c.t = t
			configuration := breaker(store, nil)
			c.openAll(configuration)
			before := c.dependencyStats().Total
			container := strings.Fields(compose(t, "ps", "-q", "pod"))[0]
			run(t, "docker", "restart", container)
			for i := range c.pods {
				c.pods[i] = address(t, "pod", i+1)
			}
			for i, url := range c.pods {
				eventually(t, fmt.Sprintf("pod %d is ready", i), func() bool { return request(http.MethodGet, url+"/ready", nil, nil) == nil })
			}
			restarted := -1
			for i := range c.pods {
				if c.status(i).State == "" {
					restarted = i
				}
			}
			if restarted < 0 {
				t.Fatal("no pod lost its configuration after the restart")
			}
			c.setup(configuration, restarted)
			if s := c.status(restarted); s.State != "open" {
				t.Fatalf("the restarted pod reads %s, want open", s.State)
			}
			if r := c.load(restarted, map[string]any{"calls": 5}); r.Rejected["open"] != 5 || c.dependencyStats().Total != before {
				t.Fatalf("the restarted pod: %+v, dependency calls %d, want 5 rejections and %d calls", r, c.dependencyStats().Total, before)
			}
		})

		t.Run("AT-3 AT-4 AT-6 the pods work without "+store+" and converge when it is back", func(t *testing.T) {
			c.t = t
			c.mode(http.StatusOK, 0)
			c.setup(breaker(store, map[string]any{"localThreshold": 3}))
			compose(t, "stop", store)
			restarted := false
			defer func() {
				if !restarted {
					compose(t, "start", store)
				}
			}()
			eventually(t, "all pods are in local mode", c.all("closed", "local"))
			ok, failed, rejected := total(c.loadAll(map[string]any{"calls": 50, "concurrency": 5}))
			if ok != 500 || failed != 0 || len(rejected) != 0 {
				t.Fatalf("without the store: ok %d, failed %d, rejected %v, want 500 ok", ok, failed, rejected)
			}
			c.mode(http.StatusServiceUnavailable, 0)
			for i := range pods / 2 {
				c.load(i, map[string]any{"calls": 3})
			}
			if first, last := c.status(0), c.status(pods-1); first.State != "open" || last.State != "closed" {
				t.Fatalf("local states = %s, %s, want open, closed", first.State, last.State)
			}
			compose(t, "start", store)
			restarted = true
			eventually(t, "all pods are in distributed mode with one state", func() bool {
				first := c.status(0)
				for i := range c.pods {
					if s := c.status(i); s.Mode != "distributed" || s.State != first.State {
						return false
					}
				}
				return true
			})
		})

		t.Run("AT-7 PF-1 PF-3 ten pods under load with "+store, func(t *testing.T) {
			c.t = t
			c.mode(http.StatusOK, 0)
			c.setup(breaker(store, map[string]any{"syncMs": 500}))
			started := time.Now()
			results := c.loadAll(map[string]any{"calls": 2000000, "concurrency": workers(), "local": true})
			elapsed := time.Since(started).Seconds()
			calls, worst := 0, result{}
			for _, r := range results {
				calls += r.OK
				if r.P99Us > worst.P99Us {
					worst = r
				}
			}
			var operations int64
			for i := range c.pods {
				s := c.status(i)
				operations += s.StoreOperations
				if s.Mode != "distributed" || s.State != "closed" || s.Breaker.LocalModeEntries != 0 {
					t.Fatalf("pod %d: %s in %s mode after the load, %d changes to local mode", i, s.State, s.Mode, s.Breaker.LocalModeEntries)
				}
			}
			rate := float64(operations) / elapsed / pods
			fmt.Printf("| Circuit breaker, closed | %s | %.0f | %.2fµs | %.2fµs | %.2fµs | %.1f |\n", store, float64(calls)/elapsed, worst.P50Us, worst.P99Us, worst.P999Us, rate)
			if calls != pods*2000000 || worst.P99Us > 1000 {
				t.Fatalf("calls = %d, worst p99 = %.1fµs, want all calls with a p99 below 1 ms", calls, worst.P99Us)
			}
			if rate > 6 {
				t.Fatalf("%.1f store operations each second for each pod, want at most 6 with a sync interval of 500 ms", rate)
			}
		})
	}

	t.Run("BH-3 a bulkhead in each pod keeps the calls in progress at its limit", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 500)
		c.setup(map[string]any{"bulkhead": map[string]any{"max": 5}})
		for i, r := range c.loadAll(map[string]any{"calls": 50, "concurrency": 50}) {
			if r.OK != 5 || r.Rejected["bulkhead"] != 45 {
				t.Fatalf("pod %d: %+v, want 5 ok and 45 rejections", i, r)
			}
		}
		stats := c.dependencyStats()
		for pod, most := range stats.MaxByPod {
			if most > 5 {
				t.Fatalf("%s had %d calls in progress at the dependency, want at most 5", pod, most)
			}
		}
		if stats.Total != 5*pods {
			t.Fatalf("the dependency got %d calls, want %d", stats.Total, 5*pods)
		}
	})

	t.Run("RL-3 a rate limiter in each pod keeps the call rate at its limit", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 0)
		c.setup(map[string]any{"rateLimit": map[string]any{"rate": 200, "burst": 20}})
		for i, r := range c.loadAll(map[string]any{"calls": 3000, "concurrency": 8, "pauseMs": 1}) {
			limit := 20 + int(200*r.DurationMs/1000) + 2
			if r.OK < 20 || r.OK > limit || r.OK+r.Rejected["rate"] != 3000 {
				t.Fatalf("pod %d: %d ok in %.0f ms, want from 20 to %d; rejected %v", i, r.OK, r.DurationMs, limit, r.Rejected)
			}
		}
	})

	t.Run("RL-39 RL-26 ten pods under heavy load share one rate through Redis", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 0)
		started := c.storeTime()
		c.setup(sharedRateLimit())
		c.expectSharedRate(started, 0)
	})

	t.Run("RL-40 RL-27 RL-28 RL-29 the pods use their local rate without Redis and share the rate again when it is back", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 0)
		c.setup(sharedRateLimit())
		eventually(t, "all pods are in distributed mode", c.rateLimitMode("distributed"))
		compose(t, "stop", "redis")
		restarted := false
		defer func() {
			if !restarted {
				compose(t, "start", "redis")
			}
		}()
		eventually(t, "all pods are in local mode", c.rateLimitMode("local"))
		results := c.loadAll(heavyLoad())
		byPod := c.dependencyStats().ByPod
		for i, r := range results {
			s := c.status(i)
			seconds := r.DurationMs / 1000
			most, least := localBurst+int(localRate*seconds), int(usedPart*localRate*seconds)
			t.Logf("pod %d without Redis: %d calls in %.3f s, limits %d and %d", i, r.OK, seconds, least, most)
			if byPod[s.Pod] != r.OK || r.Failed != 0 || r.Timeouts != 0 || r.Rejected["rate"] == 0 || len(r.Rejected) != 1 || r.OK > most || r.OK < least {
				t.Fatalf("pod %d without Redis: the dependency got %d calls in %.3f s, want from %d to %d; %+v", i, byPod[s.Pod], seconds, least, most, r)
			}
			if s.RateLimitMode != "local" || s.RateLimit.StoreErrors < 3 {
				t.Fatalf("pod %d without Redis: %s mode after %d store errors, want local mode after 3 or more", i, s.RateLimitMode, s.RateLimit.StoreErrors)
			}
		}
		compose(t, "start", "redis")
		restarted = true
		eventually(t, "all pods are in distributed mode and hold at most 1 token", func() bool { _, idle := c.heldTokens(); return idle })
		c.mode(http.StatusOK, 0)
		started := c.storeTime()
		held, _ := c.heldTokens()
		c.expectSharedRate(started, held)
	})

	t.Run("TO-1 a timeout in each pod ends the calls to a slow dependency", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 3000)
		c.setup(map[string]any{"timeoutMs": 100})
		for i, r := range c.loadAll(map[string]any{"calls": 10, "concurrency": 10}) {
			if r.Timeouts != 10 || r.DurationMs > 1500 {
				t.Fatalf("pod %d: %d timeouts in %.0f ms, want 10 in less than 1500 ms", i, r.Timeouts, r.DurationMs)
			}
		}
	})

	t.Run("RT-12 PI-3 a retry budget in each pod limits the extra traffic during an outage", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusServiceUnavailable, 0)
		c.setup(map[string]any{"retry": map[string]any{"attempts": 3, "budget": 0.1}})
		results := c.loadAll(map[string]any{"calls": 500})
		stats := c.dependencyStats()
		for i, r := range results {
			if r.Failed != 500 {
				t.Fatalf("pod %d: %+v, want 500 failed calls", i, r)
			}
		}
		for pod, attempts := range stats.ByPod {
			if attempts < 550 || attempts > 561 {
				t.Fatalf("%s made %d attempts for 500 requests, want from 550 to 561", pod, attempts)
			}
		}
	})

	t.Run("CL-9 a concurrency limiter in each pod lowers its limit for a slow dependency", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 200)
		c.setup(map[string]any{"concurrencyLimit": map[string]any{"initial": 20, "min": 2, "max": 50, "latencyMs": 50}})
		for i, r := range c.loadAll(map[string]any{"calls": 300, "concurrency": 40, "pauseMs": 5}) {
			if r.Rejected["concurrency"] == 0 || r.OK == 0 {
				t.Fatalf("pod %d: %+v, want permitted calls and rejections", i, r)
			}
			if limit := c.status(i).Limit; limit > 3 {
				t.Fatalf("pod %d: limit %d after slow calls, want at most 3", i, limit)
			}
		}
		for pod, most := range c.dependencyStats().MaxByPod {
			if most > 20 {
				t.Fatalf("%s had %d calls in progress at the dependency, want at most 20", pod, most)
			}
		}
	})

	t.Run("LS-8 load shedding in each pod rejects low-priority work first", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 500)
		c.setup(map[string]any{"loadShed": map[string]any{"max": 10}})
		for i, r := range c.loadAll(map[string]any{"calls": 30, "concurrency": 30, "priority": "low"}) {
			if r.OK != 5 || r.Rejected["shed"] != 25 {
				t.Fatalf("pod %d, low priority: %+v, want 5 ok and 25 shed", i, r)
			}
		}
		for i, r := range c.loadAll(map[string]any{"calls": 30, "concurrency": 30, "priority": "critical"}) {
			if r.OK != 10 || r.Rejected["shed"] != 20 {
				t.Fatalf("pod %d, critical priority: %+v, want 10 ok and 20 shed", i, r)
			}
		}
	})

	t.Run("PI-1 PI-3 ten pods with all policies protect the dependency in an outage and recover", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusServiceUnavailable, 0)
		c.setup(map[string]any{
			"namespace": namespace(), "store": "redis",
			"breaker":          map[string]any{"failureThreshold": 5, "syncMs": 100, "openMs": 1500},
			"loadShed":         map[string]any{"max": 200},
			"rateLimit":        map[string]any{"rate": 5000, "burst": 500},
			"concurrencyLimit": map[string]any{"initial": 50, "min": 5, "max": 100, "latencyMs": 400},
			"bulkhead":         map[string]any{"max": 20},
			"retry":            map[string]any{"attempts": 3, "budget": 0.2, "gateState": true},
			"timeoutMs":        500,
		})
		ok, _, rejected := total(c.loadAll(map[string]any{"calls": 500, "concurrency": 10, "pauseMs": 1}))
		eventually(t, "all pods are open", c.all("open", "distributed"))
		if stats := c.dependencyStats(); ok != 0 || stats.Total >= 2500 || rejected["open"] == 0 {
			t.Fatalf("outage: ok %d, the dependency got %d attempts for 5000 requests, rejected %v", ok, stats.Total, rejected)
		}
		c.mode(http.StatusOK, 0)
		time.Sleep(1700 * time.Millisecond)
		eventually(t, "all pods are closed after the recovery", func() bool {
			c.loadAll(map[string]any{"calls": 5, "pauseMs": 10})
			return c.all("closed", "distributed")()
		})
		ok, failed, rejected := total(c.loadAll(map[string]any{"calls": 100, "concurrency": 5}))
		if ok != 1000 || failed != 0 || len(rejected) != 0 {
			t.Fatalf("after the recovery: ok %d, failed %d, rejected %v, want 1000 ok", ok, failed, rejected)
		}
	})

	t.Run("TK-50 TK-51 each policy alone under load in ten pods", func(t *testing.T) {
		c.t = t
		c.mode(http.StatusOK, 0)
		policies := []struct {
			name          string
			configuration map[string]any
		}{
			{"Circuit breaker without a store", map[string]any{"breaker": map[string]any{"failureThreshold": 5}}},
			{"Bulkhead", map[string]any{"bulkhead": map[string]any{"max": 100}}},
			{"Concurrency limiter", map[string]any{"concurrencyLimit": map[string]any{"initial": 100, "min": 1, "max": 200, "latencyMs": 1000}}},
			{"Rate limiter", map[string]any{"rateLimit": map[string]any{"rate": 1e9, "burst": 1000000}}},
			{"Load shedding", map[string]any{"loadShed": map[string]any{"max": 1000}}},
			{"Retry", map[string]any{"retry": map[string]any{"attempts": 3, "budget": 0.1}}},
			{"Timeout", map[string]any{"timeoutMs": 1000}},
		}
		for _, policy := range policies {
			c.setup(policy.configuration)
			started := time.Now()
			results := c.loadAll(map[string]any{"calls": 1000000, "concurrency": workers(), "local": true})
			elapsed := time.Since(started).Seconds()
			calls, worst := 0, result{}
			for _, r := range results {
				calls += r.OK
				if r.P99Us > worst.P99Us {
					worst = r
				}
			}
			fmt.Printf("| %s | none | %.0f | %.2fµs | %.2fµs | %.2fµs | 0 |\n", policy.name, float64(calls)/elapsed, worst.P50Us, worst.P99Us, worst.P999Us)
			if calls != pods*1000000 || worst.P99Us > 1000 {
				t.Errorf("%s: %d ok calls, worst p99 %.1fµs, want all calls with a p99 below 1 ms", policy.name, calls, worst.P99Us)
			}
		}
	})
}
