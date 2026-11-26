package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/telemetry"
)

// Egress IP rotation.
//
// The free tier meters a daily allowance per egress IP, and the allowance is
// small enough that a few dozen agent turns exhaust it. The cure is a new IP:
// the bucket the upstream charges is the source address, so a redial gives the
// exit a fresh allowance without touching anything else.
//
// This file is the controller half. Each configured exit may name an
// egress-agent (see AGENTS.md), a small daemon sitting next to the uplink that
// owns two duties: it is the CONNECT proxy this gateway dials through, and it
// exposes /healthz (the IP it currently leaves from) and /rotate (reboot the
// ONT to get a new one). The agent is what talks to the router, so no router
// credential ever reaches this process — that is why the split exists.
//
// Three rules shape everything below, and each came from a live incident
// recorded in the sibling project:
//
//  1. A 429 is not proof of exhaustion. Four independent gates answer 429 with
//     an identical body and only one of them (the real per-IP allowance) is
//     cured by a new IP. Rotating on every 429 rebooted three buckets 32/20/13
//     times in one day while every request kept failing, and each reboot tore
//     down the very tunnel being measured. So rotation is driven by a token
//     budget and by the hard-429 shape, never by a bare 429.
//  2. A transport failure never rotates. A flaky uplink looks nothing like a
//     burned allowance, and demotion (pool.go) already handles it.
//  3. A rotation that comes back with the same IP is a failure, not a
//     success. The ISP sometimes re-issues the address it just dropped, and
//     recording that as success leaves the bucket cordoned for the rest of the
//     day with nobody looking at it.

// rotateTimeoutDefault bounds one rotation when the configuration is silent:
// a router reboot is ~80s and the ISP's redial another ~40s, so anything
// shorter declares failures on rotations that were about to succeed.
const rotateTimeoutDefault = 180 * time.Second

// rotateMinIntervalDefault is the floor between two rotations of the same
// exit. A real daily allowance only needs one reset, so half an hour is
// generous; the incident that set it rotated three exits 32/20/13 times in a
// day while every request kept failing.
const rotateMinIntervalDefault = 30 * time.Minute

// rotationTarget is one exit that can be told to renew its IP.
type rotationTarget struct {
	// name is the exit's name, taken from the proxy URL's userinfo when it
	// carries one and otherwise from the agent URL's host. It only has to be
	// stable within one process.
	name string
	// proxy is the transport this target renews. Rotation marks the matching
	// transport, so the pool and the controller always agree on which exit
	// changed.
	proxy *proxyTransport
	// agentURL is the egress-agent's base URL (no trailing slash).
	agentURL string
	// agentAuth is the pre-encoded Basic credential for the agent's control
	// endpoints, or empty when the agent has no shared secret.
	agentAuth string
	// budget is the token budget that triggers a rotation for this exit. Zero
	// disables the token trigger and leaves only the hard-429 one.
	budget int64
}

// rotationState is the per-exit bookkeeping that must survive across requests.
type rotationState struct {
	// windowStart is the UTC-day window these counters belong to, as a Unix
	// timestamp at the window's start.
	windowStart int64
	tokens      int64
	requests    int64
	// triggered is set when a rotation was requested in this window, so the
	// budget cannot fire repeatedly while one is in flight or after it failed
	// in a way that should wait for the next window.
	triggered bool
	// rotating is true while a rotation is running, so two requests cannot
	// reboot the same router.
	rotating bool
	// lastResult and lastAt record the outcome for the admin view.
	lastResult string
	lastAt     time.Time
	lastIP     string
	rotations  int
}

// rotator renews egress IPs. It is safe for concurrent use; each exit rotates
// at most once at a time.
type rotator struct {
	targets map[string]*rotationTarget
	// mu guards state and the snapshot write.
	mu    sync.Mutex
	state map[string]*rotationState
	// http is the control-plane client. It never goes through the proxy pool:
	// the agent lives on the tailnet, and routing its own control call through
	// a bucket that is mid-reboot is how a rotation gets stuck.
	http *http.Client
	// now is swappable so the window logic can be exercised deterministically.
	now func() time.Time
	// snapshotPath, when set, persists the counters so a restart does not
	// forget that today's budget is already spent.
	snapshotPath string
	logf         func(event string, args ...any)
}

// newRotator builds the rotator for the targets the configuration named.
// Returns nil when nothing is rotatable, which callers treat as "rotation is
// off" rather than as an error.
func newRotator(targets []*rotationTarget, snapshotPath string, logf func(string, ...any)) *rotator {
	if len(targets) == 0 {
		return nil
	}
	r := &rotator{
		targets:      make(map[string]*rotationTarget, len(targets)),
		state:        make(map[string]*rotationState, len(targets)),
		http:         &http.Client{Timeout: 30 * time.Second},
		now:          time.Now,
		snapshotPath: snapshotPath,
		logf:         logf,
	}
	for _, target := range targets {
		if target == nil || target.agentURL == "" {
			continue
		}
		r.targets[target.name] = target
		r.state[target.name] = &rotationState{}
	}
	if len(r.targets) == 0 {
		return nil
	}
	r.load()
	return r
}

// windowStartOf returns the start of the UTC day containing t. The upstream
// resets its allowance at UTC midnight, so this is the only window that
// matters; using local midnight would split a quota day in half.
func windowStartOf(t time.Time) int64 {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// roll advances the state to the window containing now, clearing the counters
// when the window moved. Callers must hold mu.
func (r *rotator) roll(name string, now time.Time) *rotationState {
	st := r.state[name]
	if st == nil {
		st = &rotationState{}
		r.state[name] = st
	}
	if start := windowStartOf(now); st.windowStart != start {
		st.windowStart = start
		st.tokens = 0
		st.requests = 0
		st.triggered = false
		// lastResult, lastAt and rotations deliberately survive the roll: they
		// are history, not budget.
	}
	return st
}

// noteOutcome folds one completed request into the exit's counters and reports
// whether that completion crossed the token budget.
//
// The token count is the meter that matters. Measured on the sibling project:
// 166 requests burned a whole day because the upstream charges prompt and
// completion tokens (~62k per agent turn), so a request-count threshold fires
// long after the allowance is gone.
func (r *rotator) noteOutcome(name string, tokens int64) (trigger bool, budget int64, spent int64) {
	if r == nil {
		return false, 0, 0
	}
	target := r.targets[name]
	if target == nil || target.budget <= 0 {
		return false, 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	st := r.roll(name, now)
	st.requests++
	if tokens > 0 {
		st.tokens += tokens
	}
	if st.triggered || st.rotating {
		return false, target.budget, st.tokens
	}
	if st.tokens < target.budget {
		return false, target.budget, st.tokens
	}
	st.triggered = true
	return true, target.budget, st.tokens
}

// noteHardQuota reports whether a 429 should rotate this exit right now.
//
// The discriminator is the Retry-After shape. A missing header or one pointing
// more than an hour out is the per-IP daily verdict; anything shorter is a
// burst throttle that clears on its own, and rebooting for it only makes the
// outage longer. That classification is the whole reason this function exists
// instead of rotating on any 429.
func (r *rotator) noteHardQuota(name string, retryAfter time.Duration, hasRetryAfter bool) (trigger bool, reason string) {
	if r == nil {
		return false, ""
	}
	if r.targets[name] == nil {
		return false, ""
	}
	hard := !hasRetryAfter || retryAfter > time.Hour
	if !hard {
		return false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.roll(name, r.now())
	if st.triggered || st.rotating {
		return false, ""
	}
	st.triggered = true
	if hasRetryAfter {
		return true, fmt.Sprintf("hard 429 (Retry-After %s)", retryAfter.Round(time.Second))
	}
	return true, "hard 429 (no Retry-After)"
}

// begin marks a rotation as running. Returns false when one already is.
func (r *rotator) begin(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.roll(name, r.now())
	if st.rotating {
		return false
	}
	st.rotating = true
	return true
}

// finish records a rotation's outcome and clears the running flag.
func (r *rotator) finish(name, detail, newIP string, ok bool) {
	r.mu.Lock()
	st := r.roll(name, r.now())
	st.rotating = false
	st.lastResult = detail
	st.lastAt = r.now()
	st.lastIP = newIP
	if ok {
		st.rotations++
		// A successful rotation means the exit now holds a fresh allowance, so
		// the budget restarts rather than staying spent for the rest of the day.
		st.tokens = 0
		st.requests = 0
		st.triggered = false
	}
	r.mu.Unlock()
	r.save()
}

// rotate reboots one exit's uplink and waits for the IP to actually change.
//
// It blocks for up to the configured timeout: the ONT takes ~80s to come back
// and the ISP another ~40s to redial. Callers run it in a goroutine so no
// client request waits on it.
func (r *rotator) rotate(name, trigger string, timeout time.Duration) {
	target := r.targets[name]
	if target == nil {
		return
	}
	if !r.begin(name) {
		return
	}
	started := time.Now()
	from, _ := r.probeIP(target)
	r.logf("exit_rotation_started", "exit", name, "trigger", trigger, "ip_from", from)

	ok, detail := r.requestRotate(target)
	if !ok && detail != "unreachable" {
		// A definite refusal is final. An unreachable agent is not: the reset
		// may already have gone out (the agent is often the first thing a
		// router reboot takes down), so fall through to polling either way.
		r.finish(name, "agent refused: "+detail, from, false)
		r.logf("exit_rotation_failed", "exit", name, "trigger", trigger, "detail", detail, "elapsed_s", time.Since(started).Seconds())
		return
	}

	to, changed := r.waitForIPChange(target, from, timeout)
	elapsed := time.Since(started)
	switch {
	case changed:
		r.finish(name, fmt.Sprintf("exit ip %s -> %s", orUnknown(from), to), to, true)
		r.liftCordon(target.proxy)
		r.logf("exit_rotation_succeeded", "exit", name, "trigger", trigger, "ip_from", from, "ip_to", to, "elapsed_s", elapsed.Seconds())
	case to != "":
		// The uplink came back on the same address. That is a failed rotation:
		// the allowance did not reset, so the exit must stay cordoned.
		r.finish(name, fmt.Sprintf("exit ip unchanged (%s)", to), to, false)
		r.logf("exit_rotation_unchanged", "exit", name, "trigger", trigger, "ip", to, "elapsed_s", elapsed.Seconds())
	default:
		r.finish(name, fmt.Sprintf("exit did not come back within %s", timeout), from, false)
		r.logf("exit_rotation_timeout", "exit", name, "trigger", trigger, "ip_from", from, "elapsed_s", elapsed.Seconds())
	}
}

// liftCordon clears the free-tier IP verdict on a transport whose address has
// just changed. The verdict was about the old address, so it cannot still hold
// — that is the one thing besides expiry that may lift a cordon.
func (r *rotator) liftCordon(proxy *proxyTransport) {
	if proxy == nil {
		return
	}
	proxy.blockedUntil.Store(0)
	proxy.clearDemotion()
}

// requestRotate asks the agent to reboot. detail is "unreachable" for a
// transport-level failure, which the caller treats as "keep polling" rather
// than as a refusal.
func (r *rotator) requestRotate(target *rotationTarget) (bool, string) {
	body := strings.NewReader(`{"wait":false}`)
	req, err := http.NewRequest(http.MethodPost, target.agentURL+"/rotate", body)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if target.agentAuth != "" {
		req.Header.Set("Authorization", target.agentAuth)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return false, "unreachable"
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload struct {
		OK      bool   `json:"ok"`
		Detail  string `json:"detail"`
		Results []struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"results"`
	}
	_ = json.Unmarshal(raw, &payload)
	detail := payload.Detail
	if detail == "" && len(payload.Results) > 0 {
		detail = payload.Results[0].Detail
	}
	if detail == "" {
		detail = fmt.Sprintf("status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || !payload.OK {
		return false, detail
	}
	return true, detail
}

// waitForIPChange polls the agent until it reports an address different from
// from, or the deadline passes. It returns the last address seen and whether
// it differs.
func (r *rotator) waitForIPChange(target *rotationTarget, from string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		ip, ready := r.probeIP(target)
		if !ready || ip == "" {
			// Mid-reboot the agent has no egress IP yet. Keep waiting.
			continue
		}
		last = ip
		if ip != from {
			return ip, true
		}
	}
	return last, false
}

// probeIP reads the agent's current egress IP. ready is false when the agent
// reports it has no working uplink right now.
func (r *rotator) probeIP(target *rotationTarget) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, target.agentURL+"/healthz", nil)
	if err != nil {
		return "", false
	}
	if target.agentAuth != "" {
		req.Header.Set("Authorization", target.agentAuth)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var payload struct {
		Ready    bool   `json:"ready"`
		EgressIP string `json:"egress_ip"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&payload); err != nil {
		return "", false
	}
	return payload.EgressIP, payload.Ready
}

// snapshot is the admin/telemetry view of every rotatable exit.
func (r *rotator) snapshot() []map[string]any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	names := make([]string, 0, len(r.targets))
	for name := range r.targets {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		st := r.roll(name, now)
		target := r.targets[name]
		out = append(out, map[string]any{
			"exit":         name,
			"agent":        configRedactURL(target.agentURL),
			"token_budget": target.budget,
			"tokens_used":  st.tokens,
			"requests":     st.requests,
			"triggered":    st.triggered,
			"rotating":     st.rotating,
			"rotations":    st.rotations,
			"last_result":  st.lastResult,
			"last_ip":      st.lastIP,
			"last_at":      formatMaybeTime(st.lastAt),
			"window_start": time.Unix(st.windowStart, 0).UTC().Format(time.RFC3339),
		})
	}
	return out
}

// rotationSnapshot is the persisted shape. It carries counters and outcomes,
// never message content or credentials.
type rotationSnapshot struct {
	Version int                        `json:"version"`
	SavedAt string                     `json:"saved_at"`
	Buckets map[string]rotationPersist `json:"buckets"`
}

type rotationPersist struct {
	WindowStart int64  `json:"window_start"`
	Tokens      int64  `json:"tokens"`
	Requests    int64  `json:"requests"`
	Triggered   bool   `json:"triggered"`
	Rotations   int    `json:"rotations"`
	LastResult  string `json:"last_result"`
	LastIP      string `json:"last_ip"`
	LastAt      string `json:"last_at"`
}

// save persists the counters so a restart does not re-spend a budget that is
// already gone. Failures are logged and ignored: losing the snapshot costs at
// most one extra rotation, while refusing to serve because a state file is
// unwritable would cost the whole service.
func (r *rotator) save() {
	if r == nil || r.snapshotPath == "" {
		return
	}
	r.mu.Lock()
	snapshot := rotationSnapshot{
		Version: 1,
		SavedAt: r.now().UTC().Format(time.RFC3339),
		Buckets: make(map[string]rotationPersist, len(r.state)),
	}
	for name, st := range r.state {
		snapshot.Buckets[name] = rotationPersist{
			WindowStart: st.windowStart,
			Tokens:      st.tokens,
			Requests:    st.requests,
			Triggered:   st.triggered,
			Rotations:   st.rotations,
			LastResult:  st.lastResult,
			LastIP:      st.lastIP,
			LastAt:      formatMaybeTime(st.lastAt),
		}
	}
	r.mu.Unlock()
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	writeFileAtomic(r.snapshotPath, encoded)
}

// load restores a snapshot. A stale window is ignored wholesale, because its
// budget belonged to a day that has already reset upstream.
func (r *rotator) load() {
	if r.snapshotPath == "" {
		return
	}
	raw, err := readFileIfExists(r.snapshotPath)
	if err != nil || len(raw) == 0 {
		return
	}
	var snapshot rotationSnapshot
	if err := json.Unmarshal(bytes.TrimSpace(raw), &snapshot); err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	start := windowStartOf(now)
	for name, persisted := range snapshot.Buckets {
		st := r.state[name]
		if st == nil {
			continue
		}
		*st = rotationState{
			windowStart: persisted.WindowStart,
			rotations:   persisted.Rotations,
			lastResult:  persisted.LastResult,
			lastIP:      persisted.LastIP,
		}
		if at, err := time.Parse(time.RFC3339, persisted.LastAt); err == nil {
			st.lastAt = at
		}
		if persisted.WindowStart != start {
			// Yesterday's budget: keep only the history.
			st.tokens = 0
			st.requests = 0
			st.triggered = false
			st.windowStart = start
			continue
		}
		st.tokens = persisted.Tokens
		st.requests = persisted.Requests
		st.triggered = persisted.Triggered
	}
}

// telemetryMetaFromContext is telemetry.MetaFromContext under a shorter name,
// so the rotation policy reads without a package qualifier on every line.
func telemetryMetaFromContext(ctx context.Context) *telemetry.RequestMeta {
	return telemetry.MetaFromContext(ctx)
}

// nameForTransport returns the rotation name of the exit that uses proxy, or
// "" when that exit is not rotatable.
func (r *rotator) nameForTransport(proxy *proxyTransport) string {
	if r == nil || proxy == nil {
		return ""
	}
	for name, target := range r.targets {
		if target.proxy == proxy {
			return name
		}
	}
	return ""
}

// reserve claims the right to rotate name now, enforcing the floor between
// two rotations of the same exit.
//
// The floor is not politeness. A 429 arrives on many concurrent requests at
// once, and without it each one reboots the router: three exits rotated
// 32/20/13 times in a single day in the sibling project while every request
// kept failing, and each reboot tore down the tunnel being measured. A real
// daily allowance needs one reset, so a floor of half an hour costs nothing
// and prevents the storm.
func (r *rotator) reserve(name string, minInterval time.Duration) bool {
	if r == nil || r.targets[name] == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.roll(name, r.now())
	if st.rotating {
		return false
	}
	if minInterval > 0 && !st.lastAt.IsZero() && r.now().Sub(st.lastAt) < minInterval {
		return false
	}
	return true
}

// noteEgressSpend folds a completed anonymous request into its exit's token
// budget and rotates when the budget is crossed.
//
// The spend is the estimated prompt tokens the request carried, which is the
// same estimate the gateway reports to streaming clients (see
// estimatePromptTokens). It is deliberately an estimate: the free tier does
// not return usage on streams, and waiting for a real number would mean never
// rotating on the lane that needs it most.
func (g *Gateway) noteEgressSpend(ctx context.Context, node *anonymousNode, resp *http.Response) {
	if g == nil || g.rotator == nil || node == nil || node.proxy == nil {
		return
	}
	name := g.rotator.nameForTransport(node.proxy)
	if name == "" {
		return
	}
	meta := telemetryMetaFromContext(ctx)
	tokens := int64(0)
	if meta != nil {
		tokens = int64(meta.EstimatedPromptTokens)
	}
	trigger, budget, spent := g.rotator.noteOutcome(name, tokens)
	if !trigger {
		return
	}
	g.logger.Info("egress token budget crossed; rotating this exit",
		"component", "rotation", "event", "exit_rotation_budget",
		"exit", name, "tokens_used", spent, "token_budget", budget)
	g.startRotation(name, "token budget")
}

// noteEgressQuota rotates an exit when a 429 carries the per-IP daily verdict.
//
// The classification is the point: a 429 with a short Retry-After is a burst
// throttle that clears on its own, and rebooting the router for it only makes
// the outage longer. Only a missing header or one pointing more than an hour
// out is the daily verdict a new IP actually cures.
func (g *Gateway) noteEgressQuota(node *anonymousNode, resp *http.Response, err error) {
	if g == nil || g.rotator == nil || node == nil || node.proxy == nil {
		return
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return
	}
	name := g.rotator.nameForTransport(node.proxy)
	if name == "" {
		return
	}
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	hasRetryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")) != ""
	trigger, reason := g.rotator.noteHardQuota(name, retryAfter, hasRetryAfter)
	if !trigger {
		return
	}
	g.logger.Warn("egress hit the per-IP daily verdict; rotating this exit",
		"component", "rotation", "event", "exit_rotation_quota",
		"exit", name, "reason", reason)
	g.startRotation(name, reason)
}

// startRotation runs one rotation in the background. A client request must
// never wait on a router reboot: the reboot takes minutes, and the request
// that discovered the burned allowance can be answered by another exit.
func (g *Gateway) startRotation(name, trigger string) {
	if g == nil || g.rotator == nil || name == "" {
		return
	}
	// The floor between rotations is checked here rather than inside the
	// goroutine so a burst of 429s from concurrent requests starts one reboot.
	if !g.rotator.reserve(name, g.rotateMinInterval) {
		return
	}
	timeout := g.rotateTimeout
	if timeout <= 0 {
		timeout = rotateTimeoutDefault
	}
	go g.rotator.rotate(name, trigger, timeout)
}

// rotationTargetsFromTransports pairs each transport with the agent that owns
// it. The mapping lives in the proxy URL itself: an exit that can be rotated
// is written as
//
//	http://user:pass@100.109.109.108:10002
//
// and the controller finds the agent by looking the exit's address up in
// cfg.EgressAgents. Keeping the mapping in the proxy entry rather than in a
// parallel list means a typo in either half shows up as "this exit has no
// agent" instead of as a rotation that reboots the wrong router.
func rotationTargetsFromTransports(transports *proxyTransports, agents map[string]egressAgentSpec) []*rotationTarget {
	if transports == nil || len(agents) == 0 {
		return nil
	}
	targets := make([]*rotationTarget, 0, len(transports.items))
	for _, transport := range transports.items {
		clean, _ := configParseProxyEntry(transport.name)
		if clean == "direct" || clean == "" {
			continue
		}
		parsed, err := url.Parse(clean)
		if err != nil || parsed.Host == "" {
			continue
		}
		spec, ok := agents[parsed.Host]
		if !ok {
			// Also allow matching on the whole URL, which is what a person
			// reading config.json is most likely to write.
			spec, ok = agents[clean]
			if !ok {
				continue
			}
		}
		name := spec.Name
		if name == "" {
			name = parsed.Host
		}
		targets = append(targets, &rotationTarget{
			name:      name,
			proxy:     transport,
			agentURL:  strings.TrimRight(spec.AgentURL, "/"),
			agentAuth: basicAuthHeader(spec.Secret),
			budget:    spec.TokenBudget,
		})
	}
	return targets
}

// egressAgentSpec is one agent's address and shared secret, as configured.
type egressAgentSpec struct {
	Name        string
	AgentURL    string
	Secret      string
	TokenBudget int64
}

// basicAuthHeader encodes a shared secret the way the agent's control
// endpoints expect it: Basic with the secret as both user and password.
func basicAuthHeader(secret string) string {
	if secret == "" {
		return ""
	}
	return "Basic " + base64Encode(secret+":"+secret)
}

func orUnknown(ip string) string {
	if ip == "" {
		return "unknown"
	}
	return ip
}

func formatMaybeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
