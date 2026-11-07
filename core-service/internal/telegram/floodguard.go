package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/bin"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/time/rate"
)

// Flood protection for one account, shared by its main connection and the
// per-DC download pools (which gotd does not pass through middlewares).
//
//   - Every RPC waits for the account's flood gate and a request-rate limit.
//   - FLOOD_WAIT closes the gate for the whole account for the wait Telegram
//     asked for, so other workers stop instead of digging the wait deeper.
//   - Waits up to floodSleepThreshold are slept through and retried (as
//     gramJS did with floodSleepThreshold = 60 s); longer waits are returned
//     to the caller, which reschedules the work after the gate reopens.
var (
	floodSleepThreshold = 60 * time.Second
	floodInlineRetries  = 3
	floodGateMargin     = time.Second
	accountRequestRate  = rate.Limit(60)
	accountRequestBurst = 60
)

type floodGuard struct {
	limiter *rate.Limiter
	mu      sync.Mutex
	until   time.Time
	// filesUntil is the gate for file downloads alone: Telegram limits
	// upload.getFile by itself, so a wait it asks for there holds the
	// downloads back but not the account's other requests (rescans, gap
	// checks), and the reverse.
	filesUntil time.Time
	// untilSpeedOnly and filesSpeedOnly: the gate closed now was closed
	// only by FLOOD_PREMIUM_WAITs, Telegram's speed limit for accounts
	// without Premium. It is about volume: fewer files at once do not help
	// against it. A plain FLOOD_WAIT while the gate is closed makes it a
	// real gate until it reopens.
	untilSpeedOnly, filesSpeedOnly bool
	// floods counts the FLOOD_WAITs Telegram has answered this account with.
	floods atomic.Uint64
	// sharedUntil: until then a FLOOD_PREMIUM_WAIT on a file part closes
	// the files' gate for every server, as before ix.12.4 (the fallback).
	// Otherwise only the request that got it waits (premiumPerRequest).
	sharedUntil time.Time
	// The last few minutes of file parts: answered, and speed-limit waits,
	// for the fallback rule (premiumRecent).
	partsAt, premiumAt []time.Time
	// fallbacks counts the times the fallback was taken, with why.
	fallbacks map[string]int
	// periods: the last fallbacks taken, for the report (ix.12.7).
	periods []FallbackPeriod
}

// FallbackPeriod is one stretch of the shared gate: when it began, when it
// ends (or ended), and why.
type FallbackPeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Why   string    `json:"why"`
}

const fallbackPeriodsKept = 20

// A FLOOD_PREMIUM_WAIT on a file part is waited out by that request alone
// (ix.12.4), as Telegram's own apps do (TDLib's NetQueryDelayer delays the
// one query; each server keeps its own download budget). Before, every file
// on every server stopped for each one, and no file could be started
// meanwhile: under the speed limit that was most of the time.
//
// The fallback to the shared gate, for premiumFallbackFor:
//   - a plain FLOOD_WAIT on a file part (asking too much, not the speed
//     limit);
//   - most file requests refused: at least premiumRecentMin speed-limit
//     waits in premiumRecent and more than premiumRecentRatio per part
//     answered meanwhile.
var (
	premiumFallbackFor = 2 * time.Hour
	premiumRecent      = 10 * time.Minute
	premiumRecentMin   = 40
	premiumRecentRatio = 2.0
)

// Why the fallback was taken (PremiumFallbacks).
const (
	FallbackPlainWait = "plain FLOOD_WAIT on a file part"
	FallbackRefused   = "most file requests refused"
)

// premiumPerRequest says whether a speed-limit wait on a file part holds only
// that request now (false: the shared gate, the fallback).
func (g *floodGuard) premiumPerRequest(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !now.Before(g.sharedUntil)
}

// fallBackLocked goes back to the shared gate for premiumFallbackFor.
func (g *floodGuard) fallBackLocked(now time.Time, why string) {
	if now.Before(g.sharedUntil) {
		return
	}
	g.sharedUntil = now.Add(premiumFallbackFor)
	g.periods = append(g.periods, FallbackPeriod{Start: now, End: g.sharedUntil, Why: why})
	if len(g.periods) > fallbackPeriodsKept {
		g.periods = append([]FallbackPeriod(nil), g.periods[len(g.periods)-fallbackPeriodsKept:]...)
	}
	if g.fallbacks == nil {
		g.fallbacks = map[string]int{}
	}
	g.fallbacks[why]++
}

// partAnswered and partPremium keep the recent record of file parts.
func (g *floodGuard) partAnswered(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.partsAt = append(trimTimes(g.partsAt, now.Add(-premiumRecent)), now)
}

func (g *floodGuard) partPremium(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.premiumAt = append(trimTimes(g.premiumAt, now.Add(-premiumRecent)), now)
	g.partsAt = trimTimes(g.partsAt, now.Add(-premiumRecent))
	if len(g.premiumAt) >= premiumRecentMin && float64(len(g.premiumAt)) > premiumRecentRatio*float64(max(1, len(g.partsAt))) {
		g.fallBackLocked(now, FallbackRefused)
	}
}

// FallbackPeriods are the last fallbacks to the shared gate, oldest first.
func (g *floodGuard) FallbackPeriods() []FallbackPeriod {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]FallbackPeriod(nil), g.periods...)
}

// PremiumMode reports how speed-limit waits are taken now: per request, or
// the shared gate until when; and the fallbacks taken since start, by why.
func (g *floodGuard) PremiumMode() (perRequest bool, sharedUntil time.Time, fallbacks map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	out := make(map[string]int, len(g.fallbacks))
	for k, v := range g.fallbacks {
		out[k] = v
	}
	if now.Before(g.sharedUntil) {
		return false, g.sharedUntil, out
	}
	return true, time.Time{}, out
}

// sleepRequest waits d for this request alone, outside the transfer's time
// budget, as the gate's waits are.
func sleepRequest(ctx context.Context, d time.Duration, hooks partHooks) error {
	resume := pauseDownloadWait(ctx)
	if hooks.waiting != nil {
		hooks.waiting(true)
	}
	started := time.Now()
	timer := time.NewTimer(d)
	var err error
	select {
	case <-ctx.Done():
		timer.Stop()
		err = ctx.Err()
	case <-timer.C:
	}
	resume()
	if hooks.waiting != nil {
		hooks.waiting(false)
	}
	if hooks.slept != nil {
		hooks.slept(time.Since(started))
	}
	if hooks.waited != nil {
		hooks.waited(time.Since(started))
	}
	return err
}

func newFloodGuard() *floodGuard {
	return &floodGuard{limiter: rate.NewLimiter(accountRequestRate, accountRequestBurst)}
}

// FloodWaitUntil reports when the account's flood gate reopens (zero when open).
func (g *floodGuard) FloodWaitUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().After(g.until) {
		return time.Time{}
	}
	return g.until
}

func (g *floodGuard) close(d time.Duration) time.Time {
	return g.closeGate(d, false, false)
}

// closeGate closes the account's gate (or the files' one) for d; premium:
// the wait was a FLOOD_PREMIUM_WAIT.
func (g *floodGuard) closeGate(d time.Duration, files, premium bool) time.Time {
	g.floods.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	gate, speedOnly := &g.until, &g.untilSpeedOnly
	if files {
		gate, speedOnly = &g.filesUntil, &g.filesSpeedOnly
	}
	now := time.Now()
	if now.After(*gate) {
		*speedOnly = premium // the gate was open: a new wait starts
	} else {
		*speedOnly = *speedOnly && premium
	}
	if until := now.Add(d + floodGateMargin); until.After(*gate) {
		*gate = until
	}
	return *gate
}

// SpeedLimitedOnly: a gate is closed now, and every gate closed was
// closed only by Telegram's speed limit for accounts without Premium
// (FLOOD_PREMIUM_WAIT), not by a plain FLOOD_WAIT.
func (g *floodGuard) SpeedLimitedOnly() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	account, files := !now.After(g.until), !now.After(g.filesUntil)
	if !account && !files {
		return false
	}
	return (!account || g.untilSpeedOnly) && (!files || g.filesSpeedOnly)
}

// FilesWaitUntil reports when file downloads may go on (zero when open).
func (g *floodGuard) FilesWaitUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().After(g.filesUntil) {
		return time.Time{}
	}
	return g.filesUntil
}

func (g *floodGuard) wait(ctx context.Context) error {
	return g.waitGate(ctx, false)
}

func (g *floodGuard) waitGate(ctx context.Context, files bool) error {
	return g.waitGateObserved(ctx, files, nil, nil, nil)
}

// limited (ix.12.12, optional) hears how long the account's request rate
// limit held a request, for the hour-by-hour record only.
func (g *floodGuard) waitGateObserved(ctx context.Context, files bool, waited func(time.Duration), waiting func(bool), limited func(time.Duration)) error {
	for {
		g.mu.Lock()
		delay := time.Until(g.until)
		if files {
			delay = max(delay, time.Until(g.filesUntil))
		}
		g.mu.Unlock()
		if delay <= 0 {
			if limited == nil {
				return g.limiter.Wait(ctx)
			}
			started := time.Now()
			err := g.limiter.Wait(ctx)
			limited(time.Since(started))
			return err
		}
		resume := pauseDownloadWait(ctx)
		if waiting != nil {
			waiting(true)
		}
		finish := func() {
			resume()
			if waiting != nil {
				waiting(false)
			}
		}
		started := time.Now()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			finish()
			if waited != nil {
				waited(time.Since(started))
			}
			return ctx.Err()
		case <-timer.C:
		}
		finish()
		if waited != nil {
			waited(time.Since(started))
		}
	}
}

func (g *floodGuard) invoke(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder) error {
	return g.invokeGate(ctx, next, input, output, false, partHooks{})
}

// partHooks hear how Telegram answered a file part: every wait it asked
// for (premium is true for FLOOD_PREMIUM_WAIT: Telegram's speed limit for
// accounts without Premium, which it applies once an account has
// downloaded tens of gigabytes; it is about volume, not about how many
// requests are in flight), and how long an answered request took.
type partHooks struct {
	flood    func(d time.Duration, premium bool)
	answered func(took time.Duration)
	waited   func(time.Duration)
	waiting  func(bool)
	// For the hour-by-hour record only (ix.12.12), all optional: every
	// request's time with Telegram and whether it came back without an
	// error, the request rate limit's hold, and the speed-limit sleeps of a
	// single request (also told to waited).
	invoked func(took time.Duration, ok bool)
	limited func(time.Duration)
	slept   func(time.Duration)
}

// filePartTimeout gives up on one file part that has not been answered in
// this long (8 KB/s for a 1 MB part); the downloaders ask for it again, as
// they do after Telegram's own timeouts. Without it, a part Telegram never
// answers holds its file, and its room in the window, until the whole
// download times out.
var filePartTimeout = 2 * time.Minute

// invokeFiles is invoke for file downloads: their own gate (which also
// waits for the account's), a time limit per part, and hooks the pacer
// learns from.
func (g *floodGuard) invokeFiles(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder, hooks partHooks) error {
	return g.invokeGate(ctx, next, input, output, true, hooks)
}

func (g *floodGuard) invokeGate(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder, files bool, hooks partHooks) error {
	for attempt := 0; ; attempt++ {
		if err := g.waitGateObserved(ctx, files, hooks.waited, hooks.waiting, hooks.limited); err != nil {
			return err
		}
		var err error
		if files {
			reqCtx, cancel := context.WithTimeout(ctx, filePartTimeout)
			started := time.Now()
			err = next.Invoke(reqCtx, input, output)
			timedOut := errors.Is(reqCtx.Err(), context.DeadlineExceeded)
			cancel()
			if hooks.invoked != nil {
				hooks.invoked(time.Since(started), err == nil)
			}
			// A part not answered in time counts as answered that slowly:
			// when every part times out, the window must still shrink.
			if hooks.answered != nil && (err == nil || timedOut && ctx.Err() == nil) {
				hooks.answered(time.Since(started))
			}
			if err != nil && ctx.Err() == nil && timedOut {
				err = fmt.Errorf("Telegram did not answer a file part within %s: %w", filePartTimeout, context.DeadlineExceeded)
			}
		} else {
			err = next.Invoke(ctx, input, output)
		}
		if files && err == nil {
			g.partAnswered(time.Now())
		}
		d, flood := tgerr.AsFloodWait(err)
		if !flood {
			return err
		}
		premium := tgerr.Is(err, tgerr.ErrPremiumFloodWait)
		if hooks.flood != nil {
			hooks.flood(d, premium)
		}
		if files && premium {
			now := time.Now()
			perRequest := g.premiumPerRequest(now)
			g.partPremium(now)
			if perRequest {
				// This request alone waits; the other files, here and on
				// the other servers, go on.
				g.floods.Add(1)
				if err := sleepRequest(ctx, d+floodGateMargin, hooks); err != nil {
					return err
				}
				continue
			}
		}
		if files && !premium {
			g.mu.Lock()
			g.fallBackLocked(time.Now(), FallbackPlainWait)
			g.mu.Unlock()
		}
		g.closeGate(d, files, premium)
		// A regular account may receive speed-limit waits for hours. Keep
		// the same part/transfer alive, without spending failure attempts or
		// throwing away bytes already written. The next loop waits on the
		// shared gate; cancellation remains immediate even for long waits.
		if tgerr.Is(err, tgerr.ErrPremiumFloodWait) {
			continue
		}
		if d > floodSleepThreshold || attempt >= floodInlineRetries {
			return err
		}
	}
}

// Middleware guards the account's main connection.
func (g *floodGuard) Middleware() gotd.Middleware {
	return gotd.MiddlewareFunc(func(next tg.Invoker) gotd.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			return g.invoke(ctx, next, input, output)
		}
	})
}
