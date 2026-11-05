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
	return g.waitGateObserved(ctx, files, nil, nil)
}

func (g *floodGuard) waitGateObserved(ctx context.Context, files bool, waited func(time.Duration), waiting func(bool)) error {
	for {
		g.mu.Lock()
		delay := time.Until(g.until)
		if files {
			delay = max(delay, time.Until(g.filesUntil))
		}
		g.mu.Unlock()
		if delay <= 0 {
			return g.limiter.Wait(ctx)
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
		if err := g.waitGateObserved(ctx, files, hooks.waited, hooks.waiting); err != nil {
			return err
		}
		var err error
		if files {
			reqCtx, cancel := context.WithTimeout(ctx, filePartTimeout)
			started := time.Now()
			err = next.Invoke(reqCtx, input, output)
			timedOut := errors.Is(reqCtx.Err(), context.DeadlineExceeded)
			cancel()
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
		d, flood := tgerr.AsFloodWait(err)
		if !flood {
			return err
		}
		if hooks.flood != nil {
			hooks.flood(d, tgerr.Is(err, tgerr.ErrPremiumFloodWait))
		}
		g.closeGate(d, files, tgerr.Is(err, tgerr.ErrPremiumFloodWait))
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
