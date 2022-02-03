// Copyright 2022 Block, Inc.

package monitor

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/event"
	"github.com/cashapp/blip/plan"
	"github.com/cashapp/blip/proto"
	"github.com/cashapp/blip/status"
)

// LevelCollector collect metrics according to a plan. It doesn't collect metrics
// directly, as part of a Monitor, it calls the Engine when it's time to collect
// metrics for a certain level--based on the frequency the users specifies for
// each level. After the Engine returns metrics, the collector (or "LPC" for short)
// calls the blip.Plugin.TransformMetrics (if specified), then sends metrics to
// all sinks specififed for the monitor. Then it waits until it's time to collect
// metrics for the next level. Consequently, the LPC drives metrics collection,
// but the Engine does the actual work of collecting metrics.
type LevelCollector interface {
	// Run runs the collector to collect metrics; it's a blocking call.
	Run(stopChan, doneChan chan struct{}) error

	// ChangePlan changes the plan; it's called by an Adjuster.
	ChangePlan(newState, newPlanName string) error

	// Pause pauses metrics collection until ChangePlan is called.
	Pause()

	// Status returns detailed internal status.
	Status() proto.MonitorCollectorStatus
}

var _ LevelCollector = &lpc{}

// lpc is the implementation of LevelCollector.
type lpc struct {
	cfg              blip.ConfigMonitor
	engine           *Engine
	planLoader       *plan.Loader
	sinks            []blip.Sink
	transformMetrics func(*blip.Metrics) error
	// --
	monitorId            string
	state                string
	plan                 blip.Plan
	changing             bool
	changePlanCancelFunc context.CancelFunc
	changePlanDoneChan   chan struct{}
	changeMux            *sync.Mutex
	stateMux             *sync.Mutex
	event                event.MonitorReceiver
	levels               []level
	paused               bool
	stopped              bool
	//
	statsMux           *sync.Mutex
	lastCollectTs      time.Time
	lastCollectError   error
	lastCollectErrorTs time.Time
	sinkErrors         map[string]error
}

type LevelCollectorArgs struct {
	Config           blip.ConfigMonitor
	Engine           *Engine
	PlanLoader       *plan.Loader
	Sinks            []blip.Sink
	TransformMetrics func(*blip.Metrics) error
}

func NewLevelCollector(args LevelCollectorArgs) *lpc {
	return &lpc{
		cfg:              args.Config,
		engine:           args.Engine,
		planLoader:       args.PlanLoader,
		sinks:            args.Sinks,
		transformMetrics: args.TransformMetrics,
		// --
		monitorId: args.Config.MonitorId,
		changeMux: &sync.Mutex{},
		stateMux:  &sync.Mutex{},
		event:     event.MonitorReceiver{MonitorId: args.Config.MonitorId},
		paused:    true,

		statsMux:   &sync.Mutex{},
		sinkErrors: map[string]error{},
	}
}

// TickerDuration sets the internal ticker duration for testing. This is only
// called for testing; do not called outside testing.
func TickerDuration(d time.Duration) {
	tickerDuration = d
}

var tickerDuration = 1 * time.Second // used for testing

const maxCollectors = 2

func (c *lpc) Run(stopChan, doneChan chan struct{}) error {
	defer close(doneChan)

	// Metrics are collected async so that this main loop does not block.
	// Normally, collecting metrics should be synchronous: every 1s, take
	// about 100-300 milliseconds get metrics and done--plenty of time
	// before the next whole second tick. But in the real world, there are
	// always blips (yes, that's partly where Blip gets its name from):
	// MySQL takes 1 or 2 seconds--or longer--to return metrics, especially
	// for "big" domains like size.table that might need to iterator over
	// hundreds or thousands of tables. Consequently, we collect metrics
	// asynchronously in multiple goroutines. By default, 2 goroutines
	// should be more than sufficient. If not, there's probably an underlying
	// problem that needs to be fixed.
	sem := make(chan bool, maxCollectors)
	for i := 0; i < maxCollectors; i++ {
		sem <- true
	}

	// -----------------------------------------------------------------------
	// LPC main loop: collect metrics on whole second ticks

	s := -1 // number of whole second ticks
	level := -1
	levelName := ""

	ticker := time.NewTicker(tickerDuration)
	defer ticker.Stop()
	for range ticker.C {
		s = s + 1 // count seconds

		// Was Stop called?
		select {
		case <-stopChan: // yes, return immediately
			// Stop changePlan goroutine (if any) and prevent new ones in the
			// pathological case that the LPA calls ChangePlan while the LPC
			// is terminating
			c.changeMux.Lock()
			c.stopped = true // prevent new changePlan goroutines
			c.changeMux.Unlock()

			c.stateMux.Lock()
			changing := c.changing
			c.stateMux.Unlock()
			if changing {
				c.changePlanCancelFunc() // stop changePlan goroutine
			}

			return nil
		default: // no
		}

		c.stateMux.Lock() // -- LOCK --
		if c.paused {
			s = -1              // reset count on pause
			c.stateMux.Unlock() // -- Unlock
			continue
		}

		// Determine lowest level to collect
		for i := range c.levels {
			if s%c.levels[i].freq == 0 {
				level = i
			}
		}
		if level == -1 {
			c.stateMux.Unlock() // -- Unlock
			continue            // no metrics to collect at this frequency
		}

		// Collect metrics at this level, unlock, and reset
		levelName = c.levels[level].name
		level = -1
		c.stateMux.Unlock() // -- UNLOCK --

		select {
		case <-sem:
			go func() {
				defer func() {
					sem <- true
					if err := recover(); err != nil { // catch panic in collectors, TransformMetrics, and sinks
						b := make([]byte, 4096)
						n := runtime.Stack(b, false)
						errMsg := fmt.Errorf("PANIC: %s: %s\n%s", c.monitorId, err, string(b[0:n]))
						log.Println(errMsg) // extra logging on panic
						c.setErr(errMsg, event.LPC_PANIC)
					}
				}()
				c.collect(levelName)
			}()
		default:
			// all collectors blocked
			errMsg := fmt.Errorf("cannot callect %s/%s: %d of %d collectors still running",
				c.plan.Name, levelName, maxCollectors, maxCollectors)
			c.setErr(errMsg, event.LPC_BLOCKED)
		}
	}
	return nil
}

func (c *lpc) collect(levelName string) {
	lpc := status.MonitorMulti(c.monitorId, "lpc", "%s/%s: collecting", c.plan.Name, levelName)
	defer status.RemoveComponent(c.monitorId, lpc)

	// **************************************************************
	// COLLECT METRICS
	//
	// Collect all metrics at this level. This is where metrics
	// collection begins. Then Engine.Collect does the real work.
	metrics, err := c.engine.Collect(context.Background(), levelName)
	// **************************************************************
	if err != nil {
		errMsg := fmt.Errorf("%s; see monitor status or event log for details", err)
		c.setErr(errMsg, event.ENGINE_COLLECT_ERROR)
	} else {
		c.setErr(nil, "") // clear old error

		// Set last collect ts only on success
		c.statsMux.Lock()
		c.lastCollectTs = time.Now()
		c.statsMux.Unlock()
	}

	// Return early unless there are metrics
	if metrics == nil {
		return
	}

	// Call user-defined TransformMetrics plugin, if set
	if c.transformMetrics != nil {
		status.Monitor(c.monitorId, lpc, "%s/%s: TransformMetrics", c.plan.Name, levelName)
		c.transformMetrics(metrics)
	}

	// Send metrics to all sinks configured for this monitor. This is done
	// sync because sinks are supposed to be fast or async _and_ have their
	// timeout, which is why we pass context.Background() here. Also, this
	// func runs in parallel (up to maxCollectors), so if a sink is slow,
	// that might be ok.
	for i := range c.sinks {
		sinkName := c.sinks[i].Name()
		status.Monitor(c.monitorId, lpc, "%s/%s: sending to %s", c.plan.Name, levelName, sinkName)
		err := c.sinks[i].Send(context.Background(), metrics)
		c.statsMux.Lock()
		c.sinkErrors[sinkName] = fmt.Errorf("[%s] %s", time.Now(), err)
		c.statsMux.Unlock()
	}
}

func (c *lpc) setErr(err error, event string) {
	c.statsMux.Lock()
	c.lastCollectError = err
	if err != nil {
		c.lastCollectErrorTs = time.Now()
	} else {
		c.lastCollectErrorTs = time.Time{}
	}
	c.statsMux.Unlock()

	if event != "" {
		c.event.Errorf(event, err.Error())
	}
}

// ChangePlan changes the metrics collect plan based on database state.
// It loads the plan from the plan.Loader, then it calls Engine.Prepare.
// This is the only time and place that Engine.Prepare is called.
//
// The caller is either LevelAdjuster.CheckState or Monitor.Start. The former
// is the case when config.monitors.plans.adjust is set. In this case,
// the LevelAdjuster (LPA) periodically checks database state and calls this
// function when the database state changes. It trusts that this function
// changes the state, so the LPA does not retry the call. The latter case,
// called from Monitor.Start, happen when the LPA is not enabled, so the
// monitor sets state=active, plan=<default>; then it trusts this function
// to keep retrying.
//
// ChangePlan is safe to call by multiple goroutines because it serializes
// plan changes, and the last plan wins. For example, if plan change 1 is in
// progress, plan change 2 cancels it and is applied. If plan change 3 happens
// while plan change 2 is in progress, then 3 cancels 2 and 3 is applied.
// Since the LPA is the only periodic caller and it has delays (so plans don't
// change too quickly), this shouldn't happen.
//
// Currently, the only way this function fails is if the plan cannot be loaded.
// That shouldn't happen because plans are loaded on startup, but it might
// happen in the future if Blip adds support for reloading plans via the API.
// Then, plans and config.monitors.*.plans.adjust might become out of sync.
// In this hypothetical error case, the plan change fails but the current plan
// continues to work.
func (c *lpc) ChangePlan(newState, newPlanName string) error {
	// Serialize access to this func
	c.changeMux.Lock()
	defer c.changeMux.Unlock()
	if c.stopped {
		return nil
	}

	// Check if changePlan goroutine from previous call is running
	c.stateMux.Lock()
	if c.changing {
		c.stateMux.Unlock()      // let changePlan goroutine return
		c.changePlanCancelFunc() // stop --> changePlan goroutine
		<-c.changePlanDoneChan   // wait for changePlan goroutine
		c.stateMux.Lock()
	}

	c.changing = true // changePlan sets c.changing=false on return, so flip it back

	ctx, cancel := context.WithCancel(context.Background())
	c.changePlanCancelFunc = cancel
	c.changePlanDoneChan = make(chan struct{})
	c.stateMux.Unlock()

	// Don't block caller. If state changes again, LPA will call this
	// func again, in which case the code above will cancel the current
	// changePlan goroutine (if it's still running) and re-change/re-prepare
	// the plan for the latest state.
	go c.changePlan(ctx, newState, newPlanName)

	return nil
}

const (
	cpName = "lpc-change-plan" // only for changePlan
)

// changePlan is a gorountine run by ChangePlan It's potentially long-running
// because it waits for Engine.Prepare. If that function returns an error
// (e.g. MySQL is offline), then this function retires forever, or until canceled
// by either another call to ChangePlan or Run is stopped (LPC is terminated).
//
// Never all this function directly; it's only called via ChangePlan, which
// serializes access and guarantees only one changePlan goroutine at a time.
func (c *lpc) changePlan(ctx context.Context, newState, newPlanName string) {
	defer func() {
		c.stateMux.Lock()
		c.changing = false
		c.stateMux.Unlock()
		close(c.changePlanDoneChan) // signal that ChangePlan can lock stateMux
	}()

	oldState := c.state
	oldPlanName := c.plan.Name
	change := fmt.Sprintf("state:%s plan:%s -> state:%s plan:%s", oldState, oldPlanName, newState, newPlanName)
	c.event.Sendf(event.CHANGE_PLAN_BEGIN, change)

	// Load new plan from plan loader, which contains all plans. Try forever because
	// that's what this func/gouroutine does: try forever (caller's expect that).
	// This shouldn't fail given that plans were already loaded and validated on startup,
	// but maybe plans reloaded after startup and something broke. User can fix by
	// reloading plans again.
	var newPlan blip.Plan
	var err error
	for {
		status.Monitor(c.monitorId, cpName, "loading new plan %s (state %s)", newPlanName, newState)
		newPlan, err = c.planLoader.Plan(c.engine.MonitorId(), newPlanName, c.engine.DB())
		if err == nil {
			break // success
		}

		status.Monitor(c.monitorId, cpName, "%s: error loading new plan: %s (retry in 2s)", change, err)
		c.event.Sendf(event.CHANGE_PLAN_ERROR, "%s: error loading new plan: %s (retry in 2s)", change, err)
		time.Sleep(2 * time.Second)
	}

	newPlan.MonitorId = c.monitorId
	newPlan.InterpolateEnvVars()
	newPlan.InterpolateMonitor(&c.cfg)

	// Convert plan levels to sorted levels for efficient level calculation in Run;
	// see code comments on sortedLevels.
	levels := sortedLevels(newPlan)

	// ----------------------------------------------------------------------
	// Prepare the (new) plan
	//
	// This is two-phase commit:
	//   0. LPC: pause Run loop
	//   1. Engine: commit new plan
	//   2. LPC: commit new plan
	//   3. LPC: resume Run loop
	// Below in call c.engine.Prepare(ctx, newPlan, c.Pause, after), Prepare
	// does its work and, if successful, calls c.Pause, which is step 0;
	// then Prepare does step 1, which won't be collected yet because it
	// just paused LPC.Run which drives metrics collection; then Prepare calls
	// the after func/callback defined below, which is step 2 and signals to
	// this func that we commit the new plan and resume Run (step 3) to begin
	// collecting that plan.

	after := func() {
		c.stateMux.Lock() // -- X lock --
		c.state = newState
		c.plan = newPlan
		c.levels = levels

		// Changing state/plan always resumes (if paused); in fact, it's the
		// only way to resume after Pause is called
		c.paused = false
		blip.Debug("%s: resume", c.monitorId)

		c.stateMux.Unlock() // -- X unlock --
	}

	// Try forever, or until context is cancelled, because it could be that MySQL is
	// temporarily offline. In the real world, this is not uncommon: Blip might be
	// started before MySQL, for example. We're running in a goroutine from ChangePlan
	// that already returned to its caller, so we're not blocking anything here.
	// More importantly, as documented in several place: this is _the code_ that
	// all other code relies on to try "forever" because a plan must be prepared
	// before anything can be collected.
	status.Monitor(c.monitorId, cpName, "preparing new plan %s (state %s)", newPlanName, newState)
	retry := backoff.NewExponentialBackOff()
	retry.MaxElapsedTime = 0
	for {
		// ctx controls the goroutine, which might run "forever" if plans don't
		// change. ctxPrep is a timeout for Prepare to ensure that it does not
		// run try "forever". If preparing takes too long, there's probably some
		// issue, so we need to sleep and retry.
		ctxPrep, cancelPrep := context.WithTimeout(ctx, 10*time.Second)
		err := c.engine.Prepare(ctxPrep, newPlan, c.Pause, after)
		if err == nil {
			break // success
		}
		if ctx.Err() != nil {
			return // changePlan goroutine has been cancelled
		}
		cancelPrep()
		status.Monitor(c.monitorId, cpName, "%s: error preparing new plan: %s", change, err)
		c.event.Sendf(event.CHANGE_PLAN_ERROR, "%s: error preparing new plan: %s", change, err)
		time.Sleep(retry.NextBackOff())
	}

	status.RemoveComponent(c.monitorId, cpName)
	c.event.Sendf(event.CHANGE_PLAN_SUCCESS, change)
}

// Pause pauses metrics collection until ChangePlan is called. Run still runs,
// but it doesn't collect when paused. The only way to resume after pausing is
// to call ChangePlan again.
func (c *lpc) Pause() {
	c.stateMux.Lock()
	blip.Debug("%s: pause", c.monitorId)
	c.paused = true
	c.stateMux.Unlock()
}

// Status returns the current state and plan name.
func (c *lpc) Status() proto.MonitorCollectorStatus {
	c.stateMux.Lock()
	defer c.stateMux.Unlock()
	c.statsMux.Lock()
	defer c.statsMux.Unlock()

	s := proto.MonitorCollectorStatus{
		State:         c.state,
		Plan:          c.plan.Name,
		Paused:        c.paused,
		LastCollectTs: c.lastCollectTs,
		SinkErrors:    map[string]string{},
	}
	if c.lastCollectError != nil {
		s.LastCollectError = c.lastCollectError.Error()
		lastCollectErrorTs := c.lastCollectErrorTs // copy because we use pointer
		s.LastCollectErrorTs = &lastCollectErrorTs
	}
	sinkErrors := map[string]string{}
	for sinkName, err := range c.sinkErrors {
		if err == nil {
			continue
		}
		sinkErrors[sinkName] = err.Error()
	}
	if len(sinkErrors) > 0 {
		s.SinkErrors = sinkErrors
	}
	return s
}

// ---------------------------------------------------------------------------
// Plan vs. sorted level
// ---------------------------------------------------------------------------

// level represents a sorted level created by sortedLevels below.
type level struct {
	freq int
	name string
}

// Sort levels ascending by frequency.
type byFreq []level

func (a byFreq) Len() int           { return len(a) }
func (a byFreq) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byFreq) Less(i, j int) bool { return a[i].freq < a[j].freq }

// sortedLevels returns a list of levels sorted (asc) by frequency. Sorted levels
// are used in the main Run loop: for i := range c.levels. Sorted levels are
// required because plan levels are unorded because the plan is a map. We could
// check every level in the plan, but that's wasteful. With sorted levels, we
// can precisely check which levels to collect at every 1s tick.
//
// Also, plan levels are abbreviated whereas sorted levels are complete.
// For example, a plan says "collect X every 5s, and collect Y every 10s".
// But the complete version of that is "collect X every 5s, and collect X + Y
// every 10s." See "metric inheritance" in the docs.
//
// Also, we convert duration strings from the plan level to integers for sorted
// levels in order to do modulo (%) in the main Run loop.
func sortedLevels(plan blip.Plan) []level {
	// Make a sorted level for each plan level
	levels := make([]level, len(plan.Levels))
	i := 0
	for _, l := range plan.Levels {
		d, _ := time.ParseDuration(l.Freq) // "5s" -> 5 (for freq below)
		levels[i] = level{
			name: l.Name,
			freq: int(d.Seconds()),
		}
		i++
	}

	// Sort levels by ascending frequency
	sort.Sort(byFreq(levels))
	blip.Debug("%s levels: %v", plan.Name, levels)

	// Metric inheritence: level N applies to N+(N+1)
	for i := 0; i < len(levels); i++ {
		// At level N
		rootLevel := levels[i].name
		root := plan.Levels[rootLevel]

		// Add metrics from N to all N+1
		for j := i + 1; j < len(levels); j++ {
			leafLevel := levels[j].name
			leaf := plan.Levels[leafLevel]

			for domain := range root.Collect {
				dom, ok := leaf.Collect[domain]
				if !ok {
					leaf.Collect[domain] = root.Collect[domain]
				} else {
					dom.Metrics = append(dom.Metrics, root.Collect[domain].Metrics...)
					leaf.Collect[domain] = dom
				}
			}
		}
	}

	return levels
}
