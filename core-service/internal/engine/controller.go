package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type Account interface {
	telegram.MediaDownloader
	Run(context.Context, func()) error
	Fingerprint() string
	RefreshMessage(context.Context, *tg.Message) (*telegram.RefreshedMessage, error)
}
type historyRecoverable interface{ SupportsHistoryRecovery() bool }

// historyRepairer repairs recovery markers while the account keeps running.
type historyRepairer interface {
	RepairHistoryUntilDone(context.Context)
}

type jobsAccount interface {
	RunJobs(context.Context, func()) error
	StartObserving(context.Context) error
}

type Dialog struct {
	ID, Name, Username, Type string
	Archived                 bool
	Members                  *int
	AccountIDs               []string
	Access                   telegram.DialogAccess
	AccountAccess            map[string]telegram.DialogAccess
}

type dialogSource interface {
	Dialogs(context.Context, int, bool) ([]telegram.Dialog, error)
}
type AccountConfig struct {
	ID, Name string
	Telegram telegram.GotdConfig
}
type Factory func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error)
type Target struct{ ID, Name string }
type Filter func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error)
type Sink func(context.Context, *Work, *tg.Message, telegram.MediaDownloader) error

// UpdateObserver must persist its effects before returning. Any error prevents
// acknowledgement of the Telegram update, just like durable queue ingestion.
type UpdateObserver func(context.Context, string, tg.UpdateClass) error

var ErrEngineNotRunning = errors.New("Engine is not running. Start the monitor first.")

type Controller struct {
	draining       atomic.Bool // no new downloads: an update is waiting (SetDraining)
	writer, reader *sql.DB
	work           *WorkStore
	dataDir        string
	factory        Factory
	filter         Filter
	sink           Sink
	observer       UpdateObserver
	recorder       MessageRecorder
	notify         func(string, error)
	opMu           sync.Mutex
	mu             sync.Mutex
	state          string
	lastError      error
	started        time.Time
	run            *running
	attemptLimit   func(int64) time.Duration
	bandwidth      bandwidthLimiter
	received       atomic.Int64 // media bytes received by all workers
	scaling        Scaling
}

// Scaling adapts how many workers download at once, as 2.x did: a long
// queue adds up to 3 workers per tick until Max, an idle queue shrinks back
// toward Min, and a Telegram FloodWait drops straight to Min. Max <= the
// configured worker count disables scaling.
type Scaling struct {
	Min, Max int
	Interval time.Duration
}

// SetScaling applies to the next Start.
func (c *Controller) SetScaling(s Scaling) {
	c.mu.Lock()
	c.scaling = s
	c.mu.Unlock()
}

// BytesReceived grows while any download is transferring; history
// backpressure uses it to tell slow large files from a stuck queue.
func (c *Controller) BytesReceived() int64 { return c.received.Load() }

func (c *Controller) SetUpdateObserver(observer UpdateObserver) {
	c.mu.Lock()
	c.observer = observer
	c.mu.Unlock()
}

type running struct {
	// claimMu makes the claims that leave out full servers take turns: each
	// sees the files the claims before it took. Without it the workers of a
	// start all counted the same servers as free and one server got four
	// times its slots (ix.12.5).
	claimMu         sync.Mutex
	promotionCancel context.CancelFunc // guarded by controller.opMu
	observing       atomic.Bool
	jobUsers        atomic.Int64
	dialogPhotos    map[string][]RecoveryDialog // guarded by mu
	ctx             context.Context
	cancel          context.CancelCauseFunc
	done            chan struct{}
	wake            chan struct{}
	accounts        map[string]Account
	ids             []string
	workers         int
	target          atomic.Int32 // workers allowed to claim work; <= workers
	maxAttempts     int
	mu              sync.Mutex
	active          map[int64]context.CancelFunc
	activeOrigin    map[int64]string
	progress        map[int64]*transferStats
	// stopAt: a person paused this download while it moved bytes. A file that
	// keeps its progress stops right after its next checkpoint (PauseJob).
	stopAt map[int64]*atomic.Bool
}

func New(writer, reader *sql.DB, dataDir string, factory Factory, filter Filter, sink Sink, notify func(string, error)) *Controller {
	if factory == nil {
		factory = func(cfg AccountConfig, state *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, gap func(int64)) (Account, error) {
			return telegram.NewAccount(cfg.Telegram, state, handler, gap)
		}
	}
	return &Controller{writer: writer, reader: reader, work: NewWorkStore(writer, reader), dataDir: dataDir, factory: factory, filter: filter, sink: sink, notify: notify, state: "stopped", attemptLimit: attemptTimeout}
}
func (c *Controller) setState(state string, cause error) {
	c.mu.Lock()
	c.state = state
	c.lastError = cause
	manual := c.run != nil && !c.run.observing.Load()
	c.mu.Unlock()
	if c.notify != nil && !manual {
		c.notify(state, cause)
	}
}

func (c *Controller) Dialogs(ctx context.Context, limit int) ([]Dialog, error) {
	session, err := c.OpenDialogs(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return session.List(limit)
}

// DialogSession keeps an account run alive while browsing, without enabling
// live downloads. Hard stops still cancel its RPCs, as for manual downloads.
type DialogSession struct {
	run    *running
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	once   sync.Once
}

func (c *Controller) OpenDialogs(ctx context.Context) (*DialogSession, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	run := c.run
	if run == nil || c.state != "running" {
		return nil, ErrEngineNotRunning
	}
	if run.ctx != nil {
		if err := run.ctx.Err(); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	s := &DialogSession{run: run, ctx: ctx, cancel: cancel}
	if run.ctx != nil {
		s.stop = context.AfterFunc(run.ctx, cancel)
	}
	run.jobUsers.Add(1)
	return s, nil
}

func (s *DialogSession) Close() {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
		s.cancel()
		s.run.jobUsers.Add(-1)
	})
}

func (s *DialogSession) Err() error {
	if s.run.ctx != nil && s.run.ctx.Err() != nil {
		return s.run.ctx.Err()
	}
	return s.ctx.Err()
}

func (s *DialogSession) List(limit int) ([]Dialog, error) {
	ctx := s.ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit != -1 && (limit <= 0 || limit > 1000) {
		limit = 500
	}
	accounts, accountIDs := s.run.accounts, s.run.ids
	if len(accounts) == 0 {
		return nil, errors.New("Telegram monitor is not running")
	}
	merged := map[string]Dialog{}
	order := make([]string, 0)
	accountSets := map[string]map[string]bool{}
	accessSets := map[string]map[string]telegram.DialogAccess{}
	var photoCandidates []RecoveryDialog
	for _, id := range accountIDs {
		account := accounts[id]
		source, ok := account.(dialogSource)
		if !ok {
			return nil, fmt.Errorf("account %s cannot list Telegram dialogs", id)
		}
		for _, archived := range []bool{false, true} {
			items, err := source.Dialogs(ctx, limit, archived)
			if err != nil {
				return nil, fmt.Errorf("list account %s dialogs (archived=%t): %w", id, archived, err)
			}
			for _, item := range items {
				photoCandidates = append(photoCandidates, RecoveryDialog{Dialog: item, AccountID: id})
				if accessSets[item.ID] == nil {
					accessSets[item.ID] = map[string]telegram.DialogAccess{}
				}
				if previous, exists := accessSets[item.ID][id]; !exists || (previous.State == "" || previous.State == "unknown") && item.Access.State != "" {
					accessSets[item.ID][id] = item.Access
				}
				current, found := merged[item.ID]
				if !found {
					merged[item.ID] = Dialog{ID: item.ID, Name: item.Name, Username: item.Username, Type: item.Type, Archived: item.Archived, Members: item.Members}
					order = append(order, item.ID)
				} else if current.Archived && !item.Archived {
					merged[item.ID] = Dialog{ID: item.ID, Name: item.Name, Username: item.Username, Type: item.Type, Archived: item.Archived, Members: item.Members}
				}
				if accountSets[item.ID] == nil {
					accountSets[item.ID] = map[string]bool{}
				}
				accountSets[item.ID][id] = true
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Dialog, 0, len(order))
	for _, id := range order {
		item := merged[id]
		for accountID := range accountSets[id] {
			item.AccountIDs = append(item.AccountIDs, accountID)
		}
		sort.Strings(item.AccountIDs)
		item.AccountAccess = accessSets[id]
		for _, accountID := range item.AccountIDs {
			access := item.AccountAccess[accountID]
			if item.Access.State == "" || dialogAccessPriority(access.State) > dialogAccessPriority(item.Access.State) {
				item.Access = access
			}
		}
		out = append(out, item)
	}
	s.rememberDialogPhotos(photoCandidates)
	return out, nil
}

func (s *DialogSession) ForumTopics(ctx context.Context, groupID string) ([]telegram.ForumTopic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, id := range s.run.ids {
		account, ok := s.run.accounts[id].(interface {
			ForumTopics(context.Context, string) ([]telegram.ForumTopic, error)
		})
		if !ok {
			continue
		}
		topics, err := account.ForumTopics(ctx, groupID)
		if err == nil {
			return topics, nil
		}
	}
	return nil, errors.New("no connected account can read topics for this group")
}

func dialogAccessPriority(state string) int {
	switch state {
	case "ok":
		return 3
	case "", "unknown":
		return 2
	default:
		return 1
	}
}

func (c *Controller) queueRun() (*running, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil || c.state != "running" {
		return nil, ErrEngineNotRunning
	}
	return c.run, nil
}

// Running reports the engine's run: the accounts it runs, and whether it
// observes live updates (monitoring) or only works through the queue (a
// manual run, whose workers take no live items). ok is false when no
// engine runs.
func (c *Controller) Running() (accounts []string, observing, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil || c.state != "running" {
		return nil, false, false
	}
	return append([]string(nil), c.run.ids...), c.run.observing.Load(), true
}

// FileSlots is how many files of each size (small: under 20 MB) one server
// downloads at once, from the first account's Telegram configuration; 0, 0
// when no engine runs or the account does not say.
func (c *Controller) FileSlots() (small, large int) {
	c.mu.Lock()
	run := c.run
	ok := run != nil && c.state == "running"
	c.mu.Unlock()
	if !ok || len(run.ids) == 0 {
		return 0, 0
	}
	slots, has := run.accounts[run.ids[0]].(interface{ FileSlots() (small, large int) })
	if !has {
		return 0, 0
	}
	return slots.FileSlots()
}

func (c *Controller) RequireRunning() error {
	_, err := c.queueRun()
	return err
}

// SetDraining stops new downloads starting, for an update (app/ix_update.go), or
// lets them start again. Downloads moving bytes finish. It is deliberately not the
// queue's pause: resuming that clears every item's own pause, which would let go
// files the archive has set aside or held back.
func (c *Controller) SetDraining(on bool) {
	c.draining.Store(on)
	if !on {
		c.mu.Lock()
		run := c.run
		c.mu.Unlock()
		if run != nil {
			c.wake(run)
		}
	}
}

// Draining reports whether new downloads are held back for an update.
func (c *Controller) Draining() bool { return c.draining.Load() }

// DrainStatus is how many downloads are in progress and what they still have to
// fetch.
func (c *Controller) DrainStatus(ctx context.Context) (files int, bytesLeft int64, err error) {
	c.mu.Lock()
	run := c.run
	c.mu.Unlock()
	if run == nil {
		return 0, 0, nil
	}
	select {
	case <-run.done:
		return 0, 0, nil
	default:
	}
	var received int64
	run.mu.Lock()
	files = len(run.active)
	for _, p := range run.progress {
		received += p.received.Load()
	}
	run.mu.Unlock()
	if files == 0 {
		return 0, 0, nil
	}
	var total int64
	if err = c.reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(file_size),0) FROM tgdl_work WHERE status='processing'`).Scan(&total); err != nil {
		return files, 0, err
	}
	return files, max(0, total-received), nil
}

func (c *Controller) wake(run *running) {
	select {
	case run.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) PauseAll(ctx context.Context) error {
	run, err := c.queueRun()
	if err != nil {
		return err
	}
	if err = c.work.SetQueuePaused(ctx, true); err == nil {
		c.wake(run)
	}
	return err
}

func (c *Controller) ResumeAll(ctx context.Context) error {
	run, err := c.queueRun()
	if err != nil {
		return err
	}
	if err = c.work.SetQueuePaused(ctx, false); err == nil {
		c.wake(run)
	}
	return err
}

func (c *Controller) CancelAllQueued(ctx context.Context) (int, error) {
	run, err := c.queueRun()
	if err != nil {
		return 0, err
	}
	removed, err := c.work.CancelAllQueued(ctx)
	if err == nil {
		c.wake(run)
	}
	return removed, err
}

// PauseJob pauses a queued file. One already moving bytes stops right after its
// next checkpoint when it keeps its progress (files from 8 MiB: nothing is lost,
// and it carries on from there when resumed); a smaller one finishes.
func (c *Controller) PauseJob(ctx context.Context, key string) (bool, error) {
	run, err := c.queueRun()
	if err != nil {
		return false, err
	}
	ok, err := c.work.PauseJob(ctx, key)
	if err == nil {
		if ok {
			c.stopAtCheckpoint(ctx, run, key, true)
		}
		c.wake(run)
	}
	return ok, err
}

func (c *Controller) ResumeJob(ctx context.Context, key string) (bool, error) {
	run, err := c.queueRun()
	if err != nil {
		return false, err
	}
	ok, err := c.work.ResumeJob(ctx, key)
	if err == nil {
		if ok {
			c.stopAtCheckpoint(ctx, run, key, false)
		}
		c.wake(run)
	}
	return ok, err
}

// stopAtCheckpoint asks the downloads of key in progress to stop at their next
// checkpoint, or not to after all.
func (c *Controller) stopAtCheckpoint(ctx context.Context, run *running, key string, on bool) {
	rows, err := c.reader.QueryContext(ctx, `SELECT id FROM tgdl_work WHERE group_id||'_'||message_id=? AND status='processing'`, key)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, id := range ids {
		if flag := run.stopAt[id]; flag != nil {
			flag.Store(on)
		}
	}
}

func (c *Controller) CancelJob(ctx context.Context, key string) (bool, error) {
	run, err := c.queueRun()
	if err != nil {
		return false, err
	}
	ok, err := c.work.CancelJob(ctx, key)
	if err == nil {
		if ok {
			// Cancelling a processing row must stop the in-flight media
			// transport as well as marking the durable row skipped. The worker
			// will observe the cancelled context and leave no partial claim.
			var id int64
			if queryErr := c.reader.QueryRowContext(ctx, `SELECT id FROM tgdl_work WHERE group_id||'_'||message_id=? LIMIT 1`, key).Scan(&id); queryErr == nil {
				run.mu.Lock()
				cancel := run.active[id]
				run.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		}
		c.wake(run)
	}
	return ok, err
}

func (c *Controller) RetryJob(ctx context.Context, key string) (bool, error) {
	run, err := c.queueRun()
	if err != nil {
		return false, err
	}
	ok, err := c.work.RetryJob(ctx, key)
	if err == nil {
		c.wake(run)
	}
	return ok, err
}

func (c *Controller) RetryAll(ctx context.Context) (int, error) {
	run, err := c.queueRun()
	if err != nil {
		return 0, err
	}
	count, err := c.work.RetryAll(ctx)
	if err == nil {
		c.wake(run)
	}
	return count, err
}

// RequeueLost: see WorkStore.RequeueLost. It works with the engine stopped
// too; a running engine is woken.
func (c *Controller) RequeueLost(ctx context.Context, accountID string, target Target, message *tg.Message, origin string) (bool, error) {
	ok, err := c.work.RequeueLost(ctx, accountID, target, message, origin)
	if err == nil {
		if run, runErr := c.queueRun(); runErr == nil {
			c.wake(run)
		}
	}
	return ok, err
}

func (c *Controller) DismissJob(ctx context.Context, key string) (bool, error) {
	run, err := c.queueRun()
	if err != nil {
		return false, err
	}
	ok, err := c.work.DismissJob(ctx, key)
	if err == nil {
		c.wake(run)
	}
	return ok, err
}

func (c *Controller) ClearFinished(ctx context.Context) error {
	return c.work.ClearFinished(ctx)
}
func (c *Controller) Start(request, parent context.Context, accounts []AccountConfig, workers, maxAttempts int) error {
	return c.start(request, parent, accounts, workers, maxAttempts, true)
}

func (c *Controller) StartJobs(request, parent context.Context, accounts []AccountConfig, workers, maxAttempts int) error {
	return c.start(request, parent, accounts, workers, maxAttempts, false)
}

func (c *Controller) start(request, parent context.Context, accounts []AccountConfig, workers, maxAttempts int, observing bool) error {
	c.opMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.opMu.Unlock()
		}
	}()
	c.mu.Lock()
	old := c.run
	state := c.state
	c.mu.Unlock()
	if old != nil {
		select {
		case <-old.done:
		default:
			if state == "running" && old.ctx.Err() == nil {
				if !observing {
					return nil
				}
				if !old.observing.Swap(true) {
					c.setState("starting", nil)
					promotion, stop := context.WithTimeout(request, 30*time.Minute)
					old.promotionCancel = stop
					// Recovery may take time. Allow Stop to cancel it and keep the
					// existing connection alive for in-flight manual transfers.
					c.opMu.Unlock()
					locked = false
					stopRun := context.AfterFunc(old.ctx, stop)
					var promoteErr error
					for _, id := range old.ids {
						if account, ok := old.accounts[id].(jobsAccount); ok {
							if promoteErr = account.StartObserving(promotion); promoteErr != nil {
								promoteErr = fmt.Errorf("account %s observation: %w", id, promoteErr)
								break
							}
						}
					}
					stopRun()
					stop()
					c.opMu.Lock()
					locked = true
					old.promotionCancel = nil
					if promoteErr != nil {
						// StopObserving can cancel the promotion wait while keeping
						// explicit manual transfers on this authenticated connection.
						if old.ctx.Err() == nil && !old.observing.Load() && errors.Is(promoteErr, context.Canceled) {
							c.setState("running", nil)
							return promoteErr
						}
						old.cancel(promoteErr)
						c.opMu.Unlock()
						locked = false
						<-old.done
						return promoteErr
					}
					if old.ctx.Err() != nil {
						return context.Cause(old.ctx)
					}
					if !old.observing.Load() {
						c.setState("running", nil)
						return context.Canceled
					}
					c.mu.Lock()
					c.started = time.Now()
					c.mu.Unlock()
					c.setState("running", nil)
					c.wake(old)
					return nil
				}
			}
			return errors.New("Runtime already running")
		}
	}
	if len(accounts) == 0 {
		return errors.New("No Telegram accounts loaded. Add one in Settings → Accounts first.")
	}
	if workers < 1 || workers > 64 || maxAttempts < 1 || maxAttempts > 20 {
		return errors.New("invalid worker or retry count")
	}
	if c.filter == nil || c.sink == nil {
		return errors.New("engine ingestion is not configured")
	}
	release, err := acquireLock(filepath.Join(c.dataDir, ".tgdl-engine.lock"))
	if err != nil {
		return fmt.Errorf("another engine owns this data directory: %w", err)
	}
	ctx, cancel := context.WithCancelCause(parent)
	run := &running{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), accounts: make(map[string]Account), workers: workers, maxAttempts: maxAttempts, active: make(map[int64]context.CancelFunc), progress: make(map[int64]*transferStats)}
	run.observing.Store(observing)
	run.activeOrigin = map[int64]string{}
	run.stopAt = map[int64]*atomic.Bool{}
	failed := true
	defer func() {
		if failed {
			cancel(context.Canceled)
			release()
		}
	}()
	seen := map[string]bool{}
	recoveryPending := false
	for _, config := range accounts {
		if config.ID == "" || run.accounts[config.ID] != nil {
			return errors.New("duplicate or empty account ID")
		}
		config := config
		var gaps int
		if err := c.reader.QueryRowContext(request, `SELECT count(*) FROM tgdl_update_recovery WHERE account_id=?`, config.ID).Scan(&gaps); err != nil {
			return err
		}
		pendingRecovery := gaps > 0
		recoveryPending = recoveryPending || pendingRecovery
		state := &telegram.UpdateState{Writer: c.writer, Reader: c.reader, AccountID: config.ID, OnFailure: func(err error) { cancel(fmt.Errorf("account %s update persistence: %w", config.ID, err)) }}
		handler := func(ctx context.Context, u tg.UpdatesClass) error { return c.accept(ctx, run, config.ID, u) }
		var gapAccount atomic.Value // Account, set once the factory returns
		var repairing, again atomic.Bool
		gap := func(channelID int64) {
			gapCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			err := state.RecordRecovery(gapCtx, 0, channelID, 0)
			if err != nil {
				cancel(fmt.Errorf("account %s recovery marker: %w", config.ID, err))
				return
			}
			// The durable marker is written: repair this channel's missed
			// history in the background while every other chat keeps
			// downloading. Without that ability, stop and repair on restart.
			if repairer, ok := gapAccount.Load().(historyRepairer); ok {
				again.Store(true)
				if repairing.CompareAndSwap(false, true) {
					go func() {
						for again.Swap(false) && ctx.Err() == nil {
							repairer.RepairHistoryUntilDone(ctx)
						}
						repairing.Store(false)
						// A gap reported while finishing must not be stranded.
						if again.Load() && ctx.Err() == nil && repairing.CompareAndSwap(false, true) {
							go func() { defer repairing.Store(false); repairer.RepairHistoryUntilDone(ctx) }()
						}
					}()
				}
				return
			}
			recoveryErr := fmt.Errorf("account %s channel %d requires history recovery", config.ID, channelID)
			_ = state.Latch(gapCtx, recoveryErr)
			cancel(recoveryErr)
		}
		account, err := c.factory(config, state, handler, gap)
		if err != nil {
			return fmt.Errorf("account %s: %w", config.ID, err)
		}
		gapAccount.Store(account)
		if enabler, ok := account.(interface{ EnableBackgroundRepair() }); ok {
			if _, repairs := account.(historyRepairer); repairs {
				enabler.EnableBackgroundRepair()
			}
		}
		if pendingRecovery {
			recoverable, ok := account.(historyRecoverable)
			if !ok || !recoverable.SupportsHistoryRecovery() {
				return fmt.Errorf("account %s has pending Telegram history recovery", config.ID)
			}
		}
		fingerprint := account.Fingerprint()
		if fingerprint == "" || seen[fingerprint] {
			return errors.New("saved accounts contain a missing or duplicated authentication key")
		}
		seen[fingerprint] = true
		run.accounts[config.ID] = account
		run.ids = append(run.ids, config.ID)
	}
	if err = c.work.Recover(request); err != nil {
		return err
	}
	c.mu.Lock()
	c.run = run
	c.started = time.Time{}
	c.mu.Unlock()
	c.setState("starting", nil)
	ready := make(chan struct{}, len(accounts))
	var wg sync.WaitGroup
	wg.Add(len(accounts))
	for _, id := range run.ids {
		id := id
		go func() {
			defer wg.Done()
			var once sync.Once
			startAccount := run.accounts[id].Run
			if !observing {
				if account, ok := run.accounts[id].(jobsAccount); ok {
					startAccount = account.RunJobs
				}
			}
			err := startAccount(ctx, func() { once.Do(func() { ready <- struct{}{} }) })
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("Telegram connection ended")
				}
				cancel(fmt.Errorf("account %s: %w", id, err))
			}
		}()
	}
	// One coordinator owns the worker WaitGroup additions and lifetime. Start
	// never races Wait against Add, and the lock remains held until every user
	// of the account/database has left.
	startWorkers := make(chan bool, 1)
	c.mu.Lock()
	scaling := c.scaling
	c.mu.Unlock()
	run.target.Store(int32(workers))
	wg.Add(1)
	go func() { defer wg.Done(); c.fillServers(ctx) }()
	if scaling.Max > workers {
		scaling.Min = max(1, min(scaling.Min, workers))
		scaling.Max = min(scaling.Max, 64)
		run.workers = scaling.Max
	}
	go func() {
		if <-startWorkers {
			// The files a worker could take know their server before the
			// first claim, or the first claims would go in rank order alone
			// and could all land on one server (ix.12.4).
			c.fillClaimableServers(ctx)
			wg.Add(run.workers)
			for i := 0; i < run.workers; i++ {
				go func() { defer wg.Done(); c.worker(run, i) }()
			}
			if run.workers > workers {
				wg.Add(1)
				go func() { defer wg.Done(); c.scale(run, scaling) }()
			}
		}
		wg.Wait()
		release()
		cause := context.Cause(ctx)
		if cause != nil && !errors.Is(cause, context.Canceled) {
			c.setState("error", cause)
		} else {
			c.setState("stopped", nil)
		}
		c.mu.Lock()
		c.started = time.Time{}
		c.mu.Unlock()
		close(run.done)
	}()
	failed = false
	// Publish the run before waiting on the network so Stop can cancel startup.
	c.opMu.Unlock()
	locked = false
	startupBudget := 30 * time.Second
	if recoveryPending && observing {
		// History repair can legitimately fetch many bounded pages. A normal
		// login startup deadline would cancel it repeatedly before it can
		// advance the durable cursor.
		startupBudget = 30 * time.Minute
	}
	startup := time.NewTimer(startupBudget)
	defer startup.Stop()
	for range accounts {
		select {
		case <-ready:
		case <-ctx.Done():
			startWorkers <- false
			<-run.done
			return context.Cause(ctx)
		case <-request.Done():
			cancel(request.Err())
			startWorkers <- false
			<-run.done
			return request.Err()
		case <-startup.C:
			err = errors.New("Telegram startup timed out")
			cancel(err)
			startWorkers <- false
			<-run.done
			return err
		}
	}
	c.opMu.Lock()
	locked = true
	if ctx.Err() != nil {
		startWorkers <- false
		<-run.done
		return context.Cause(ctx)
	}
	c.mu.Lock()
	c.started = time.Now()
	c.mu.Unlock()
	c.setState("running", nil)
	startWorkers <- true
	return nil
}
func (c *Controller) Stop(ctx context.Context) error {
	c.opMu.Lock()
	c.mu.Lock()
	run := c.run
	c.mu.Unlock()
	if run == nil {
		c.opMu.Unlock()
		return nil
	}
	select {
	case <-run.done:
		c.opMu.Unlock()
		return nil
	default:
	}
	c.setState("stopping", nil)
	run.cancel(context.Canceled)
	c.opMu.Unlock()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Controller) Status(ctx context.Context) (map[string]any, error) {
	c.mu.Lock()
	state, cause, started, run := c.state, c.lastError, c.started, c.run
	c.mu.Unlock()
	if run != nil && !run.observing.Load() {
		state, cause, started = "stopped", nil, time.Time{}
	}
	var errorValue any
	if cause != nil {
		errorValue = cause.Error()
	}
	var startedAt any
	var uptime int64
	if !started.IsZero() {
		startedAt = started.UnixMilli()
		uptime = time.Since(started).Milliseconds()
	}
	workers, accounts := 0, 0
	if run != nil && (state == "running" || state == "starting" || state == "stopping") {
		workers = int(run.target.Load())
		accounts = len(run.accounts)
	}
	counts, err := c.work.Counts(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"state": state, "error": errorValue, "startedAt": startedAt, "uptimeMs": uptime, "workers": workers, "accounts": accounts, "queue": counts["pending"], "active": counts["processing"], "stats": nil}, nil
}

// Pace reports each running account's download pacing.
func (c *Controller) Pace() map[string]telegram.PaceStats {
	c.mu.Lock()
	run := c.run
	c.mu.Unlock()
	out := map[string]telegram.PaceStats{}
	if run == nil {
		return out
	}
	select {
	case <-run.done:
		return out
	default:
	}
	for _, id := range run.ids {
		if paced, ok := run.accounts[id].(interface{ DownloadPace() telegram.PaceStats }); ok {
			out[id] = paced.DownloadPace()
		}
	}
	return out
}

// Gauges are the downloader figures /metrics exports, as 2.x reported
// them: accounts and workers of the engine run in progress (monitoring or a
// backfill-only run), transfers in flight right now, and queued work.
type Gauges struct {
	Accounts, Workers, Active, Queue int
}

func (c *Controller) Gauges(ctx context.Context) (Gauges, error) {
	c.mu.Lock()
	run := c.run
	c.mu.Unlock()
	var g Gauges
	if run != nil {
		select {
		case <-run.done:
		default:
			g.Accounts = len(run.accounts)
			g.Workers = int(run.target.Load())
			run.mu.Lock()
			g.Active = len(run.active)
			run.mu.Unlock()
		}
	}
	counts, err := c.work.Counts(ctx)
	if err != nil {
		return Gauges{}, err
	}
	g.Queue = counts["pending"]
	return g, nil
}

func (c *Controller) accept(ctx context.Context, run *running, accountID string, u tg.UpdatesClass) error {
	observe := run.observing.Load() || telegram.IsHistoryRecovery(ctx)
	c.mu.Lock()
	observer := c.observer
	recorder := c.recorder
	c.mu.Unlock()
	var list []tg.UpdateClass
	switch update := u.(type) {
	case *tg.Updates:
		list = update.Updates
	case *tg.UpdatesCombined:
		list = update.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{update.Update}
	default:
		return nil
	}
	for _, update := range list {
		if recorder != nil {
			switch update.(type) {
			case *tg.UpdateDeleteMessages, *tg.UpdateDeleteChannelMessages:
				recorder(ctx, accountID, update, u)
			case *tg.UpdateNewMessage, *tg.UpdateNewChannelMessage, *tg.UpdateEditMessage, *tg.UpdateEditChannelMessage:
				if observe {
					recorder(ctx, accountID, update, u)
				}
			}
		}
		switch update.(type) {
		case *tg.UpdateDeleteMessages, *tg.UpdateDeleteChannelMessages:
			// Source deletes still protect retained rescue files while an
			// account is connected only for manual/history jobs.
			if observer != nil {
				if err := observer(ctx, accountID, update); err != nil {
					return err
				}
			}
			continue
		}
		if !observe {
			continue
		}
		var raw tg.MessageClass
		var pts int
		switch update := update.(type) {
		case *tg.UpdateNewMessage:
			raw = update.Message
			pts = update.Pts
		case *tg.UpdateNewChannelMessage:
			raw = update.Message
			pts = update.Pts
		case *tg.UpdateEditMessage:
			raw = update.Message
			pts = update.Pts
		case *tg.UpdateEditChannelMessage:
			raw = update.Message
			pts = update.Pts
		default:
			continue
		}
		message, ok := raw.(*tg.Message)
		if !ok {
			continue
		}
		if _, err := telegram.MessageAttachment(message); errors.Is(err, telegram.ErrNoMedia) {
			continue
		} else if err != nil {
			return err
		}
		target, allowed, err := c.filter(ctx, accountID, message, u)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		id, changed, err := c.work.Enqueue(ctx, accountID, target, message, pts)
		if err != nil {
			return err
		}
		if changed {
			run.mu.Lock()
			if cancel := run.active[id]; cancel != nil {
				cancel()
			}
			run.mu.Unlock()
			select {
			case run.wake <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

// An idle worker (its last claim found nothing) looks again after
// workerIdlePoll, then after twice as long each time it again finds
// nothing, up to workerIdlePollMax. A queue whose files all wait (for the
// big-file lane, a retry, a pause) costs the single writer a claim per
// worker every few seconds rather than every second. A wake (new work, a
// resumed item) brings one idle worker back at once and resets its wait;
// a worker that then finds work passes the wake on, so a burst of new
// files reaches every idle worker without waiting for their timers.
var (
	workerIdlePoll    = time.Second
	workerIdlePollMax = 5 * time.Second
)

func (c *Controller) worker(run *running, index int) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	poll, woken := workerIdlePoll, false
	smaller := false // the last file made room for a smaller one: take one
	for run.ctx.Err() == nil {
		if index >= int(run.target.Load()) {
			// Parked by the scaler: claim nothing until the target grows.
			select {
			case <-run.ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		var work *Work
		var err error
		switch {
		case c.draining.Load():
			work, err = c.work.ClaimExpiring(run.ctx, run.unthrottled(), time.Now(), !run.observing.Load())
		case smaller:
			// The large file before gave its place up for a smaller one.
			work, err = run.claimNext(c.work, true)
		default:
			work, err = run.claimNext(c.work, false)
		}
		smaller = false
		if err != nil {
			if run.ctx.Err() == nil {
				run.cancel(fmt.Errorf("claim work: %w", err))
			}
			return
		}
		if work == nil {
			idle := time.NewTimer(poll)
			select {
			case <-run.ctx.Done():
				idle.Stop()
				return
			case <-run.wake:
				idle.Stop()
				poll, woken = workerIdlePoll, true
			case <-idle.C:
				poll, woken = min(2*poll, workerIdlePollMax), false
			}
			continue
		}
		poll = workerIdlePoll
		if woken {
			woken = false
			c.wake(run) // there may be more: let the next idle worker look
		}
		ctx, cancel := context.WithCancel(run.ctx)
		ctx = WithOrigin(ctx, work.Origin)
		progress := &transferStats{started: time.Now()}
		stopAt := &atomic.Bool{}
		run.mu.Lock()
		run.active[work.ID] = cancel
		run.activeOrigin[work.ID] = work.Origin
		if run.stopAt == nil {
			run.stopAt = map[int64]*atomic.Bool{}
		}
		run.stopAt[work.ID] = stopAt
		if work.Origin == "live" && !run.observing.Load() {
			cancel()
		}
		run.progress[work.ID] = progress
		run.mu.Unlock()
		current, err := c.work.IsCurrent(ctx, work)
		if err == nil && !current {
			cancel()
			err = context.Canceled
		}
		var message *tg.Message
		if err == nil {
			message, err = work.Message()
		}
		budget := newAttemptBudget(ctx, c.attemptLimit(work.FileSize))
		attemptCtx := budget.ctx
		// A file already saved under another message takes no bytes, so it
		// needs no fresh reference and costs Telegram no request.
		needsRefresh := work.ForceRefresh || work.Attempts > 1 || (time.Since(time.UnixMilli(work.CreatedAt)) > time.Minute && !c.work.IdentitySaved(ctx, work.Identity))
		if err == nil && needsRefresh {
			var fresh *telegram.RefreshedMessage
			if work.Origin == "stories" {
				if source, ok := run.accounts[work.AccountID].(storySource); ok {
					fresh, err = source.RefreshStory(attemptCtx, message)
				} else {
					err = errors.New("account cannot refresh stories")
				}
			} else {
				fresh, err = run.accounts[work.AccountID].RefreshMessage(attemptCtx, message)
			}
			if err == nil && (fresh == nil || fresh.Message == nil) {
				err = errors.New("Telegram refresh returned no message")
			}
			if err == nil && work.Origin == "stories" {
				oldMedia, e := telegram.MessageAttachment(message)
				newMedia, ne := telegram.MessageAttachment(fresh.Message)
				if e != nil || ne != nil || newMedia.GroupID != oldMedia.GroupID || newMedia.MessageID != oldMedia.MessageID {
					err = errors.New("Telegram refresh returned another story")
				}
			}
			if err == nil {
				message = fresh.Message
				_, allowed, filterErr := c.filter(attemptCtx, work.AccountID, message, fresh.Entities)
				if filterErr != nil {
					err = filterErr
				} else if !allowed {
					err = errFiltered
				}
			}
			if err == nil {
				current, err = c.work.Refresh(attemptCtx, work, message)
				if err == nil && !current {
					cancel()
					err = context.Canceled
				}
			}
		}
		if err == nil {
			// Waits for a file or part slot go by the row's place in the claim
			// order, and a file still waiting for its slot gives way to better
			// work rather than hold this worker.
			key, manual := work.ClaimKey, !run.observing.Load()
			rankedCtx := telegram.WithDownloadRank(attemptCtx, key, func() bool {
				if c.draining.Load() && key >= claimLaneBase {
					return true // an update is waiting, and this file has not started
				}
				better, err := c.work.Outranked(ctx, run.unthrottled(), time.Now(), key, manual)
				return err == nil && better
			})
			// A file that keeps its progress stops right after a checkpoint when an
			// update waits or a person pauses it: nothing is lost (ErrDownloadStopped).
			rankedCtx = telegram.WithCheckpointStop(rankedCtx, func() bool { return c.draining.Load() || stopAt.Load() })
			rankedCtx = telegram.WithDownloadRoom(rankedCtx, func() bool {
				if c.draining.Load() {
					return false
				}
				waiting, err := c.work.SmallerWaiting(ctx, run.unthrottled(), time.Now(), manual, telegram.DownloadLargeFile)
				return err == nil && waiting
			})
			err = c.sink(rankedCtx, work, message, trackedTransport{MediaDownloader: run.accounts[work.AccountID], stats: progress, total: &c.received, bandwidth: &c.bandwidth, budget: budget})
		}
		if err != nil && attemptCtx.Err() != nil {
			err = context.Cause(attemptCtx)
		}
		budget.stop()
		wasCancelled := ctx.Err() != nil
		// A check just before the transfer decided the file waits (ix.12.13):
		// back to the queue paused, no attempt spent, with its reason.
		var deferred *telegram.DownloadDeferred
		if !errors.As(err, &deferred) || wasCancelled {
			deferred = nil
		} else {
			err, wasCancelled = nil, true
		}
		smaller = errors.Is(err, telegram.ErrDownloadMakeRoom)
		if errors.Is(err, telegram.ErrDownloadYield) {
			// Back to the queue as it was: no bytes moved, no attempt spent.
			err, wasCancelled = nil, true
		}
		if errors.Is(err, telegram.ErrDownloadStopped) {
			// Stopped after a checkpoint when asked: back to the queue, no
			// attempt spent, and it carries on from there.
			err, wasCancelled = nil, true
		}
		cancel()
		run.mu.Lock()
		delete(run.active, work.ID)
		delete(run.activeOrigin, work.ID)
		delete(run.stopAt, work.ID)
		delete(run.progress, work.ID)
		run.mu.Unlock()
		retry := time.Time{}
		flooded := false
		if d, ok := tgerr.AsFloodWait(err); ok && !wasCancelled {
			// Telegram asked this account to wait: reschedule after the wait
			// without spending one of the item's retry attempts.
			retry, flooded = time.Now().Add(d+time.Second), true
		} else if err != nil && !errors.Is(err, errFiltered) && !wasCancelled && work.Attempts < run.maxAttempts {
			retry = time.Now().Add(time.Second * time.Duration(1<<min(work.Attempts-1, 8)))
		}
		finishCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		var finishErr error
		held := false
		if deferred != nil {
			d := deferred
			held, finishErr = c.work.FinishDeferred(finishCtx, work, func(ctx context.Context, tx *sql.Tx) error {
				if d.Hold == nil {
					return nil
				}
				e := d.Hold(ctx, tx)
				if e != nil && d.Failed != nil {
					d.Failed(e)
				}
				return e
			})
		}
		if finishErr == nil && !held {
			finishErr = c.work.Finish(finishCtx, work, err, wasCancelled || flooded, retry)
		}
		stop()
		if finishErr != nil {
			run.cancel(fmt.Errorf("finish work: %w", finishErr))
			return
		}
	}
}

// Bound library-internal RPC timeout retries while allowing large files on
// slow links. An attempt deadline consumes a retry; engine cancellation does not.
func attemptTimeout(size int64) time.Duration {
	seconds := min(max(size, 0)/(16*1024), int64((24*time.Hour)/time.Second))
	return min(2*time.Minute+time.Duration(seconds)*time.Second, 24*time.Hour)
}

// fullServers are the servers whose file slots the files downloading now
// fill (WorkStore.FullServers), for a run of one account whose client knows
// its slots; nil otherwise (claims then go in rank order alone, as before
// ix.12.4). Their files are left in the queue for the next claim: a worker
// takes a file on another server instead of waiting for a slot.
func (r *running) fullServers(work *WorkStore) []int {
	if len(r.ids) != 1 {
		return nil
	}
	slots, ok := r.accounts[r.ids[0]].(interface{ FileSlots() (small, large int) })
	if !ok {
		return nil
	}
	small, large := slots.FileSlots()
	full, err := work.FullServers(r.ctx, small, large)
	if err != nil {
		return nil
	}
	return full
}

// claimNext claims the next file, leaving out full servers (smaller: one
// under telegram.DownloadLargeFile first, then any). Claims take turns
// (claimMu).
func (r *running) claimNext(work *WorkStore, smaller bool) (*Work, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	full := r.fullServers(work)
	if smaller {
		w, err := work.ClaimSmallerAvoiding(r.ctx, r.unthrottled(), time.Now(), !r.observing.Load(), telegram.DownloadLargeFile, full)
		if err != nil || w != nil {
			return w, err
		}
	}
	return work.ClaimAvoiding(r.ctx, r.unthrottled(), time.Now(), !r.observing.Load(), full)
}

// fillClaimableServers records the servers of the rows workers could take
// now, before they start: about a second for a few thousand rows; at most
// fillClaimableFor, after which the background fill carries on.
func (c *Controller) fillClaimableServers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, fillClaimableFor)
	defer cancel()
	for ctx.Err() == nil {
		if n, err := c.work.FillServers(ctx, 500, true); err != nil || n == 0 {
			return
		}
	}
}

var fillClaimableFor = 30 * time.Second

// fillServers records the servers of rows queued before ix.12.4, in the
// background, a batch at a time.
func (c *Controller) fillServers(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := c.work.FillServers(ctx, 500)
		if err != nil || n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// speedLimitedOnly: every account waiting now waits only because of
// Telegram's speed limit for accounts without Premium (see
// telegram.GotdClient.SpeedLimitedOnly).
func (r *running) speedLimitedOnly() bool {
	some := false
	for _, id := range r.ids {
		gated, ok := r.accounts[id].(interface{ FloodWaitUntil() time.Time })
		if !ok || gated.FloodWaitUntil().IsZero() {
			continue
		}
		limited, ok := r.accounts[id].(interface{ SpeedLimitedOnly() bool })
		if !ok || !limited.SpeedLimitedOnly() {
			return false
		}
		some = true
	}
	return some
}

// unthrottled lists the run's accounts that Telegram is not currently
// flood-limiting, so workers leave a waiting account's queue alone.
func (r *running) unthrottled() []string {
	ids := make([]string, 0, len(r.ids))
	for _, id := range r.ids {
		if gated, ok := r.accounts[id].(interface{ FloodWaitUntil() time.Time }); ok && !gated.FloodWaitUntil().IsZero() {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// paced reports whether every account of the run paces its downloads.
func (r *running) paced() bool {
	if !telegram.Pacing() || len(r.ids) == 0 {
		return false
	}
	for _, id := range r.ids {
		if _, ok := r.accounts[id].(interface{ SuggestedWorkers() int }); !ok {
			return false
		}
	}
	return true
}

func (c *Controller) scale(run *running, s Scaling) {
	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	gated := false
	for {
		select {
		case <-run.ctx.Done():
			return
		case <-tick.C:
		}
		target := int(run.target.Load())
		if len(run.unthrottled()) < len(run.ids) {
			// Only Telegram's speed limit for accounts without Premium
			// (FLOOD_PREMIUM_WAIT, about volume): fewer files at once do
			// not help against it, and its waits come every few seconds, so
			// halving at each one would end at the fewest files. Hold.
			if run.speedLimitedOnly() {
				continue
			}
			// Telegram asked an account to wait. Without pacing, drop to the
			// fewest files at once. With it, halve once for the whole wait:
			// the accounts' pacers already cut how many parts are in flight,
			// and halving again every tick would end at the fewest anyway.
			switch {
			case !run.paced():
				run.target.Store(int32(s.Min))
			case !gated:
				run.target.Store(int32(max(s.Min, target/2)))
			}
			gated = true
			continue
		}
		gated = false
		// With pacing, the accounts say how many files keep their download
		// windows full: more would only wait for room.
		suggested := 0
		for _, id := range run.ids {
			if paced, ok := run.accounts[id].(interface{ SuggestedWorkers() int }); ok {
				suggested += paced.SuggestedWorkers()
			}
		}
		ceiling := s.Max
		if suggested > 0 {
			ceiling = min(s.Max, max(s.Min, suggested))
			if target > ceiling {
				run.target.Store(int32(ceiling))
				continue
			}
		}
		var pending int
		if err := c.reader.QueryRowContext(run.ctx, `SELECT count(*) FROM tgdl_work WHERE status='pending' AND paused=0`).Scan(&pending); err != nil {
			continue
		}
		run.mu.Lock()
		active := len(run.active)
		run.mu.Unlock()
		switch {
		case pending > target*2 && target < ceiling:
			run.target.Store(int32(min(ceiling, target+3)))
			c.wake(run)
		case pending == 0 && active < s.Min && target > s.Min:
			run.target.Store(int32(max(s.Min, active+1)))
		}
	}
}
