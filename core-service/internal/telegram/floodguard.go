package telegram

import (
	"context"
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
	return g.closeGate(d, false)
}

func (g *floodGuard) closeGate(d time.Duration, files bool) time.Time {
	g.floods.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	gate := &g.until
	if files {
		gate = &g.filesUntil
	}
	if until := time.Now().Add(d + floodGateMargin); until.After(*gate) {
		*gate = until
	}
	return *gate
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
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (g *floodGuard) invoke(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder) error {
	return g.invokeGate(ctx, next, input, output, false, nil)
}

// invokeFiles is invoke for file downloads: their own gate (which also
// waits for the account's), and onFlood hears of every wait Telegram asks
// for, so the pacer can slow down.
//
// premium is true for FLOOD_PREMIUM_WAIT: Telegram's speed limit for
// accounts without Premium, which it applies once an account has
// downloaded tens of gigabytes. It is about volume, not about how many
// requests are in flight.
func (g *floodGuard) invokeFiles(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder, onFlood func(d time.Duration, premium bool)) error {
	return g.invokeGate(ctx, next, input, output, true, onFlood)
}

func (g *floodGuard) invokeGate(ctx context.Context, next tg.Invoker, input bin.Encoder, output bin.Decoder, files bool, onFlood func(time.Duration, bool)) error {
	for attempt := 0; ; attempt++ {
		if err := g.waitGate(ctx, files); err != nil {
			return err
		}
		err := next.Invoke(ctx, input, output)
		d, flood := tgerr.AsFloodWait(err)
		if !flood {
			return err
		}
		if onFlood != nil {
			onFlood(d, tgerr.Is(err, tgerr.ErrPremiumFloodWait))
		}
		g.closeGate(d, files)
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
