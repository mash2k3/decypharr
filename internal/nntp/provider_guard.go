package nntp

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Provider guard (Ether's usenet breaker + learned limits, as in
// cli_debrid's built-in engine):
//
//   - Breaker: a provider whose login is refused (an expired account, a
//     wrong password) or that keeps failing to connect (host down, wrong
//     port, a blackholed address) is paused: no new dials, and it is left out
//     of provider selection while it has no idle connection, so backups step
//     in. Before, every article paid a dial, TLS and login (or the whole dial
//     timeout) on it, and a dead primary kept backups from ever being used.
//     After a backoff one dial is let through as a probe; it closes the
//     breaker or reopens it for twice as long. Never proof an article is
//     missing.
//   - Limits: "too many connections" caps the pool at what the server
//     carried for busyHold, then widens one connection every widenEvery
//     instead of jumping back into the same refusals. A refusal that states
//     the account's limit below MaxConnections keeps the pool under it
//     (minus ~10%) for the client's life. The cap works by parking spare
//     tokens in the pool's slot semaphore, so every acquire path honours it.

// errProviderPaused is a dial refused by an open breaker.
var errProviderPaused = errors.New("provider paused")

// Guard timings (vars for tests).
var (
	authMin      = 30 * time.Second
	authMax      = 10 * time.Minute
	connMin      = 2 * time.Second
	connMax      = time.Minute
	connFailures = 3
	busyHold     = 30 * time.Second
	widenEvery   = 5 * time.Second
	parkEvery    = time.Second
)

type providerGuard struct {
	mu sync.Mutex
	// breaker
	until   time.Time
	backoff time.Duration
	streak  int
	auth    bool
	err     error
	probing bool
	// limits
	capConns int
	capUntil time.Time
	learned  int
	parked   int
	parking  bool // the parking loop is running
}

// busyMessage: status texts providers use for a full account.
func busyMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"too many", "connection limit", "max connections",
		"maximum connections", "maximum number of connections", "connections exceeded",
		"connections reached"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// A number after "limit"/"max" wins over one before "connections".
var (
	limitAfterRE  = regexp.MustCompile(`(?i)\b(?:limit|max(?:imum)?)(?:\s+(?:number\s+)?(?:of\s+)?(?:connections?|conns?|sessions?))?(?:\s+(?:is|of|reached|exceeded|allowed))?\s*[:=(]?\s*(\d{1,4})\b`)
	limitBeforeRE = regexp.MustCompile(`(?i)\b(\d{1,4})\s*(?:concurrent\s+|simultaneous\s+)?connections`)
)

// statedLimit finds a connection limit in a refusal's text (0 = none).
func statedLimit(msg string) int {
	for _, re := range []*regexp.Regexp{limitAfterRE, limitBeforeRE} {
		if m := re.FindStringSubmatch(msg); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// dialKind sorts a failed createConnection.
type dialKind int

const (
	dialOther  dialKind = iota // connect/greeting/timeout: counts toward the breaker
	dialAuth                   // login refused
	dialBusy                   // the account is full
	dialGaveUp                 // the caller's context ended: says nothing
)

func classifyDialError(err error, ctxErr error) dialKind {
	if ctxErr != nil {
		return dialGaveUp
	}
	msg := err.Error()
	if busyMessage(msg) {
		return dialBusy
	}
	var ne *Error
	if errors.As(err, &ne) {
		switch ne.Type {
		case ErrorTypeAuthentication, ErrorTypePermissionDenied:
			return dialAuth
		case ErrorTypeServerBusy:
			return dialBusy
		}
	}
	if strings.Contains(msg, "unexpected greeting: 400") {
		return dialBusy // "400 service temporarily unavailable" on connect
	}
	if strings.Contains(msg, "unexpected greeting: 502") {
		return dialAuth // "502 access denied / account expired"
	}
	return dialOther
}

// admit decides whether a new dial may go ahead (probe = the single
// recovery attempt after a backoff).
func (pp *ProviderPool) admit() (probe bool, err error) {
	g := &pp.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() {
		return false, nil
	}
	now := time.Now()
	if now.Before(g.until) || g.probing {
		return false, g.pausedErrLocked(now)
	}
	g.probing = true
	return true, nil
}

func (g *providerGuard) pausedErrLocked(now time.Time) error {
	kind := "connection failing"
	if g.auth {
		kind = "login refused"
	}
	wait := max(g.until.Sub(now), 0).Round(time.Second)
	return NewConnectionError(fmt.Errorf("%w (%s, retry in %s): %v", errProviderPaused, kind, wait, g.err))
}

// paused reports whether the breaker is open and no idle connection is
// left, so selecting this provider now can only fail.
func (pp *ProviderPool) paused() bool {
	g := &pp.guard
	g.mu.Lock()
	open := !g.until.IsZero() && (time.Now().Before(g.until) || g.probing)
	g.mu.Unlock()
	if !open {
		return false
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return len(pp.conns) == 0
}

func doubled(d, lo, hi time.Duration, grow bool) time.Duration {
	switch {
	case d <= 0:
		return lo
	case !grow:
		return d
	}
	return min(max(2*d, lo), hi)
}

// dialResult records a dial's outcome. It returns the backoff when the
// breaker opened (0 otherwise) and whether a paused provider recovered.
func (pp *ProviderPool) dialResult(probe bool, err, ctxErr error) (opened time.Duration, recovered bool) {
	g := &pp.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if probe {
		g.probing = false
	}
	if err == nil {
		recovered = !g.until.IsZero()
		g.until, g.backoff, g.streak, g.auth, g.err = time.Time{}, 0, 0, false, nil
		return 0, recovered
	}
	now := time.Now()
	switch classifyDialError(err, ctxErr) {
	case dialGaveUp:
		return 0, false
	case dialBusy:
		// The server answered: it is up. Its own throttle handles this.
		if probe {
			g.until, g.backoff, g.streak, g.auth, g.err = time.Time{}, 0, 0, false, nil
			return 0, true
		}
		g.streak = 0
		return 0, false
	case dialAuth:
		g.auth = true
		g.backoff = doubled(g.backoff, authMin, authMax, probe || g.until.IsZero())
	default:
		g.streak++
		if !probe && g.until.IsZero() && g.streak < connFailures {
			return 0, false
		}
		g.backoff = doubled(g.backoff, connMin, connMax, probe || g.until.IsZero())
		g.auth = false
	}
	g.err = err
	g.until = now.Add(g.backoff)
	return g.backoff, false
}

// guardState is the breaker state for stats: "ok", "auth_failed", "down".
func (pp *ProviderPool) guardState() (state string, retryIn time.Duration, learned, capNow, parked int) {
	g := &pp.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	state = "ok"
	if !g.until.IsZero() && (now.Before(g.until) || g.probing) {
		state = "down"
		if g.auth {
			state = "auth_failed"
		}
		retryIn = max(g.until.Sub(now), 0)
	}
	return state, retryIn, g.learned, g.capLocked(now, pp.max), g.parked
}

// capLocked is the connection limit in force now (g.mu held).
func (g *providerGuard) capLocked(now time.Time, maxConns int) int {
	limit := maxConns
	if g.learned > 0 {
		limit = min(limit, g.learned)
	}
	if g.capConns > 0 {
		c := g.capConns
		if !now.Before(g.capUntil) {
			c += 1 + int(now.Sub(g.capUntil)/widenEvery)
		}
		if c < limit {
			return c
		}
		g.capConns = 0
	}
	return limit
}

// throttle handles a "too many connections" refusal of a dial whose slot
// the caller still holds.
func (pp *ProviderPool) throttle(err error) {
	g := &pp.guard
	g.mu.Lock()
	if n := statedLimit(err.Error()); n > 0 && n < pp.max {
		n = max(n-n/10, 1)
		if g.learned == 0 || n < g.learned {
			g.learned = n
		}
	}
	now := time.Now()
	if g.capConns == 0 || !now.Before(g.capUntil) {
		// What the server carried: slots in use minus parked ones minus
		// the refused dial's own.
		g.capConns = max(len(pp.slots)-g.parked-1, 1)
	}
	g.capUntil = now.Add(busyHold)
	start := !g.parking
	g.parking = true
	g.mu.Unlock()
	if start {
		go pp.parkLoop()
	}
}

// parkLoop keeps (MaxConnections - cap) tokens parked in the slot
// semaphore while a cap is in force, then gives them all back.
func (pp *ProviderPool) parkLoop() {
	t := time.NewTicker(parkEvery)
	defer t.Stop()
	for {
		if done := pp.adjustParking(); done {
			return
		}
		select {
		case <-t.C:
		case <-pp.closed:
			return
		}
	}
}

// adjustParking moves parked tokens toward what the cap needs; true once
// no cap is left and nothing is parked (the loop then ends).
func (pp *ProviderPool) adjustParking() bool {
	g := &pp.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	want := pp.max - g.capLocked(time.Now(), pp.max)
	for g.parked < want {
		select {
		case pp.slots <- struct{}{}:
			g.parked++
			continue
		default: // every slot busy: park the rest as they free up
		}
		break
	}
	for g.parked > want {
		select {
		case <-pp.slots:
			g.parked--
			continue
		default:
		}
		break
	}
	if want == 0 && g.parked == 0 {
		g.parking = false
		return true
	}
	return false
}
