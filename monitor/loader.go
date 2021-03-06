// Copyright 2022 Block, Inc.

package monitor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/aws"
	"github.com/cashapp/blip/dbconn"
	"github.com/cashapp/blip/event"
	"github.com/cashapp/blip/plan"
	"github.com/cashapp/blip/sink"
	"github.com/cashapp/blip/status"
)

// LoadFunc is a callback that matches blip.Plugin.LoadMonitors.
// It's an arg to NewLoader, if specified by the user.
type LoadFunc func(blip.Config) ([]blip.ConfigMonitor, error)

// StartMonitorFunc is a callback that matches blip.Plugin.StartMonitor.
type StartMonitorFunc func(blip.ConfigMonitor) bool

// Changes are monitors added, removed, and changed. It's the return value
// of Loader.Changes, which the caller might use to clean up or do other things.
// Currently, the only caller is Server.Boot, which ignores the changes because
// there can only be added monitors on startup.
//
// Invalid errors come from blip.ConfigMonitor.Validate, only if not strict.
// If strict, Loader.Load returns on the first error.
type Changes struct {
	Added   []*Monitor
	Removed []*Monitor
	Changed []*Monitor
	Invalid []error
}

type loadedMonitor struct {
	monitor *Monitor
	started bool
}

// Loader is the singleton Monitor loader. It's a combination of factory and
// repository because it makes new monitors and it keeps track of them. The
// Load is created and first called in Server.Boot, and it exists while Blip
// runs because monitors can be reloaded.
//
// Loader is safe for concurrent use, but it's currently only called by the Server.
type Loader struct {
	cfg        blip.Config
	factory    blip.Factories
	plugin     blip.Plugins
	planLoader *plan.Loader
	// --
	dbmon           map[string]*loadedMonitor // keyed on monitorId
	stopLossPercent float64
	stopLossNumber  uint
	*sync.Mutex
	stopChan     chan struct{}
	doneChan     chan struct{}
	rdsLoader    aws.RDSLoader
	startMonitor func(blip.ConfigMonitor) bool
}

type LoaderArgs struct {
	Config     blip.Config
	Factories  blip.Factories
	Plugins    blip.Plugins
	PlanLoader *plan.Loader
	RDSLoader  aws.RDSLoader
}

// NewLoader creates a new Loader singleton. It's called in Server.Boot and Server.Run.
func NewLoader(args LoaderArgs) *Loader {
	startMonitor := args.Plugins.StartMonitor
	if startMonitor == nil {
		startMonitor = func(blip.ConfigMonitor) bool { return true }
	}
	stopLossNumber, stopLossPercent, _ := blip.StopLoss(args.Config.MonitorLoader.StopLoss) // already validated
	return &Loader{
		cfg:        args.Config,
		factory:    args.Factories,
		planLoader: args.PlanLoader,
		rdsLoader:  args.RDSLoader,
		// --
		stopLossPercent: stopLossPercent,
		stopLossNumber:  stopLossNumber,
		dbmon:           map[string]*loadedMonitor{},
		Mutex:           &sync.Mutex{},
		stopChan:        make(chan struct{}),
		doneChan:        make(chan struct{}),
		startMonitor:    startMonitor,
	}
}

func (ml *Loader) Reload(stopChan, doneChan chan struct{}) error {
	if ml.cfg.MonitorLoader.Freq == "" {
		panic("MonitorLoader.Reload called but config.monitor-loader.freq not set")
	}

	defer close(doneChan)

	reloadTime, _ := time.ParseDuration(ml.cfg.MonitorLoader.Freq) // already validated
	reloadTicker := time.NewTicker(reloadTime)
	defer reloadTicker.Stop()

	timeout := time.Duration(reloadTime / 2)

	// Reload monitors every config.monitor-loader.freq
	for {
		status.Blip("monitor-loader", "idle")
		select {
		case <-reloadTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := ml.Load(ctx)
			cancel()
			if err != nil {
				event.Errorf(event.MONITORS_RELOAD_ERROR, "error reloading monitors, will retry: %s", err)
				continue
			}
			ml.StartMonitors() // all new monitors
		case <-stopChan:
			return nil
		}
	}
}

// StartMonitors runs all monitors that have been loaded but not started.
// This should be called after Load. If Reload is running (started in Server.Run),
// it calls Load > StartMonitors periodically, else Server.Boot calls Load then
// Server.Run calls StartMonitors once.
func (ml *Loader) StartMonitors() {
	ml.Lock()
	defer ml.Unlock()

	event.Send(event.MONITORS_STARTING)
	defer event.Send(event.MONITORS_STARTED)

	for i := range ml.dbmon {
		if ml.dbmon[i].started {
			continue // skip started monitors
		}

		m := ml.dbmon[i] // m is *loadedMonitor
		status.Blip("monitor-loader", "starting %s", m.monitor.MonitorId())

		// Call StartMonitor callback. Default allows all monitors to start,
		// but user might have provided callback to filter monitors.
		if !ml.startMonitor(m.monitor.Config()) {
			blip.Debug("%s not run", m.monitor.MonitorId())
			continue
		}

		// Start the MySQL monitor, which starts metrics collection
		go m.monitor.Run()
		m.started = true

		// Space out monitors so their clocks don't tick at the same time.
		// We don't want, for example, 25 monitors simultaneously waking up,
		// connecting to MySQL, processing metrics. That'll make Blip
		// CPU/net usage unnecessarily spiky.
		//
		// @improve: 20ms is reasonable, but if there are very few monitors,
		// we can sleep longer to distribute the collection load more evenly.
		time.Sleep(20 * time.Millisecond)
	}
	status.Blip("monitor-loader", "monitors started at "+time.Now().String())
}

// Load loads all monitors specified and auto-detected, for all environments:
// local, remote, cloud, etc. It's safe for concurrent use, but calls are
// serialized. Server.Boot is the first (and primary) caller, which loads
// monitors on startup.
func (ml *Loader) Load(ctx context.Context) error {
	ml.Lock()
	defer ml.Unlock()

	event.Send(event.MONITORS_LOADING)
	defer event.Send(event.MONITORS_LOADED)

	changes, err := ml.Changes(ctx)
	if err != nil {
		return err
	}

	// Check config.monitor-loader.stop-loss: don't change monitors if there's
	// a big drop in the number because it might be a false-positive that will
	// set off alarms when a bunch of metrics fail to report.
	nBefore := float64(len(ml.dbmon))
	nNow := float64(len(changes.Removed))
	if nNow < nBefore {
		var errMsg string
		if ml.stopLossPercent > 0 {
			lost := (nBefore - nNow) / nBefore
			if lost > ml.stopLossPercent {
				errMsg = fmt.Sprintf("before: %d; now: %d; lost %f%% > limit %f%%", int(nBefore), int(nNow), lost, ml.stopLossPercent)
			}
		}
		if ml.stopLossNumber > 0 {
			lost := uint(nBefore - nNow)
			if lost > ml.stopLossNumber {
				errMsg = fmt.Sprintf("before: %d; now: %d; lost %d > limit %d", int(nBefore), int(nNow), lost, ml.stopLossNumber)
			}
		}
		if errMsg != "" {
			event.Errorf(event.MONITORS_STOPLOSS, errMsg)
			return nil // this func didn't fail
		}
	}

	// ----------------------------------------------------------------------
	// Now that all has all loaded monitors (for this call), update ml.dbmon,
	// which is the official internal repo of loaded monitors

	for _, mon := range changes.Removed {
		mon.Stop()
		delete(ml.dbmon, mon.MonitorId())
	}

	for _, mon := range changes.Changed {
		mon.Stop()
		delete(ml.dbmon, mon.MonitorId())
	}

	for _, mon := range changes.Added {
		ml.dbmon[mon.MonitorId()] = &loadedMonitor{
			monitor: mon,
			started: false,
		}
	}

	return nil
}

// Changes returns which monitors have been added, changed, or removed since
// the last call to Load. It is not safe for use by multiple goroutines; calls
// are serialized by Load.
func (ml *Loader) Changes(ctx context.Context) (Changes, error) {
	ch := Changes{
		Added:   []*Monitor{},
		Removed: []*Monitor{},
		Changed: []*Monitor{},
		Invalid: []error{},
	}
	defer func() {
		last := fmt.Sprintf("added: %d removed: %d changed: %d",
			len(ch.Added), len(ch.Removed), len(ch.Changed))
		status.Blip("monitor-loader", "%s on %s", last, time.Now())
	}()

	// All tracks all monitors loaded for this call. By contrast, ml.dbmon
	// is currently loaded monitors (from a previous call). Load into all first
	// (which might be slow), then lock and modify ml.dbmon.
	all := map[string]blip.ConfigMonitor{}

	// If the user provided a blip.Plugin.LoadMonitors function, it's entirely
	// responsible for loading monitors. Else, do the normal built-in load sequence.
	// Monitor configs are finalized and validated in merge(); the func calls
	// here just load monitor configs as-is.
	if ml.plugin.LoadMonitors != nil {
		blip.Debug("call plugin.LoadMonitors")
		status.Blip("monitor-loader", "loading from plugin")
		monitors, err := ml.plugin.LoadMonitors(ml.cfg)
		if err != nil {
			return ch, err
		}
		if err := ml.merge(monitors, all, &ch); err != nil {
			return ch, err
		}
	} else {
		// -------------------------------------------------------------------
		// Built-in load sequence: config files, monitors file, AWS, local

		// First, monitors from the config file
		if len(ml.cfg.Monitors) != 0 {
			if err := ml.merge(ml.cfg.Monitors, all, &ch); err != nil {
				return ch, err
			}
			blip.Debug("loaded %d monitors from config file", len(ml.cfg.Monitors))
		}

		// Second, monitors from the monitor files
		monitors, err := ml.loadFiles(ctx)
		if err != nil {
			return ch, err
		}
		if err := ml.merge(monitors, all, &ch); err != nil {
			return ch, err
		}

		// Third, monitors from the AWS RDS API
		if len(ml.cfg.MonitorLoader.AWS.Regions) > 0 {
			monitors, err = ml.rdsLoader.Load(ctx, ml.cfg)
			if err != nil {
				if !ml.cfg.MonitorLoader.AWS.Automatic() {
					return ch, err
				}
				blip.Debug("failed auto-AWS loading, ignoring: %s", err)
			}
			if err := ml.merge(monitors, all, &ch); err != nil {
				return ch, err
			}
		}

		// Last, local monitors auto-detected
		if len(all) == 0 && !ml.cfg.MonitorLoader.Local.DisableAuto {
			monitors, err = ml.loadLocal(ctx)
			if err != nil {
				return ch, err
			}
			if err := ml.merge(monitors, all, &ch); err != nil {
				return ch, err
			}
		}
	}

	// Monitors that have been removed
	for monitorId, loaded := range ml.dbmon {
		if _, ok := all[monitorId]; !ok {
			ch.Removed = append(ch.Removed, loaded.monitor)
		}
	}

	for monitorId, cfg := range all {
		// New monitor? Yes if it doesn't already exist.
		existingMonitor, ok := ml.dbmon[monitorId]
		if !ok {
			newMonitor, err := ml.makeMonitor(cfg)
			if err != nil {
				return ch, err
			}
			ch.Added = append(ch.Added, newMonitor) // note new
			continue
		}

		// Existing monitor, but has it changed?
		// To detect, we hash the entire config and compare the SHAs.
		// Consequently, changing a single character anywhere in the
		// config is a different (new) monitor. It's a dumb but safe
		// approach because a "smart" approach would need a lot of
		// logic to detect what changed and what to do about it.
		newHash := sha256.Sum256([]byte(fmt.Sprintf("%v", cfg)))
		oldHash := sha256.Sum256([]byte(fmt.Sprintf("%v", existingMonitor.monitor.Config())))
		if newHash == oldHash {
			continue // no change
		}
		ch.Changed = append(ch.Changed, existingMonitor.monitor)
		newMonitor, err := ml.makeMonitor(cfg)
		if err != nil {
			return ch, err
		}
		ch.Added = append(ch.Added, newMonitor) // note new
	}

	return ch, nil
}

// merge merges new monitors into the map of all monitors. The merge is necessary
// because, in Load above, we load monitors from 4 places: config file, monitors file,
// AWS, and local (if the first 3 lodad nothing). Latter silently replaces former.
// For example, if a monitor is loaded from config file then loaded again from
// AWS, the AWS instance silently overwrites (takes precedent) the config file
// instance.
//
// Since monitors are identified by blip.ConfigMonitor.MonitorId, this func also
// finalizes the monitor config by applying defaults, interpolating env vars, and
// interpolating monitor field vars. These must be done before finalizing MonitorId
// in case the user specifies something like:
//
//   monitors:
//     - socket: ${TMPDIR}/mysql.sock
//
// In this case, env var ${TMPDIR} needs to be replaced first so MonitorId works
// out to "/tmp/mysql.sock", for example.
func (ml *Loader) merge(new []blip.ConfigMonitor, all map[string]blip.ConfigMonitor, changes *Changes) error {
	for _, newcfg := range new {
		newcfg.ApplyDefaults(ml.cfg)              // apply defaults to monitor values
		newcfg.InterpolateEnvVars()               // replace ${ENV_VAR} in monitor values
		newcfg.InterpolateMonitor()               // replace %{monitor.X} in monitor values
		newcfg.MonitorId = blip.MonitorId(newcfg) // finalize MonitorId

		// Validate monitor config. If invalid and strict, return the error which
		// makes Loader return the error to the caller trying to load monitors,
		// which is probably Server.Boot. If not strict, then same the error and
		// continue loading other monitors. In this case, the user might be ok
		// ignore the broken monitor, or maybe they'll fix it and reload while
		// Blip is running, which is another reason we might see duplicate monitors
		// on load.
		if err := newcfg.Validate(); err != nil {
			return err
		}

		// Monitor config is valid; merge it. The does NOT create or run the
		// monitor. That's done at the end of Load.
		all[newcfg.MonitorId] = newcfg
	}
	return nil
}

// makeMonitor makes a new Monitor. Normally, there'd be a factory for this,
// but Monitor are concrete, not abstract, so there's only one way to make them.
// Testing mocks the abstract parts of a Monitor, like the LPC and LPA.
func (ml *Loader) makeMonitor(cfg blip.ConfigMonitor) (*Monitor, error) {
	// Make sinks for this monitor. Each monitor has its own sinks.
	sinks := []blip.Sink{}
	for sinkName, opts := range cfg.Sinks {
		sink, err := sink.Make(blip.SinkFactoryArgs{
			SinkName:  sinkName,
			MonitorId: cfg.MonitorId,
			Options:   opts,
			Tags:      cfg.Tags,
		})
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, sink)
		blip.Debug("%s sends to %s", cfg.MonitorId, sinkName)
	}

	// If no sinks, default to printing metrics to stdout
	if len(sinks) == 0 && !blip.Strict {
		blip.Debug("using log sink")
		sink, _ := sink.Make(blip.SinkFactoryArgs{SinkName: "log", MonitorId: cfg.MonitorId})
		sinks = append(sinks, sink)
	}

	mon := NewMonitor(MonitorArgs{
		Config:     cfg,
		DbMaker:    ml.factory.DbConn,
		PlanLoader: ml.planLoader,
		Sinks:      sinks,
	})
	return mon, nil
}

// loadFiles loads monitors from config.monitor-loader.files, if any. It only
// loads the files; it doesn't validate--that's done in merge, which is called
// in Load.
func (ml *Loader) loadFiles(ctx context.Context) ([]blip.ConfigMonitor, error) {
	if len(ml.cfg.MonitorLoader.Files) == 0 {
		return nil, nil
	}
	status.Blip("monitor-loader", "loading from files")

	mons := []blip.ConfigMonitor{}
FILES:
	for _, file := range ml.cfg.MonitorLoader.Files {
		bytes, err := ioutil.ReadFile(file)
		if err != nil {
			if blip.Strict {
				return nil, err
			}
			continue FILES
		}
		var cfg blip.ConfigMonitor
		if err := yaml.Unmarshal(bytes, &cfg); err != nil {
			return nil, fmt.Errorf("%s: invalid YAML: %s", file, err)
		}
		mons = append(mons, cfg)
		blip.Debug("loaded %s", file)
	}
	return mons, nil
}

// loadLocal auto-detects local MySQL instances.
func (ml *Loader) loadLocal(ctx context.Context) ([]blip.ConfigMonitor, error) {
	status.Blip("monitor-loader", "auto-detect local")

	// Auto-detect using default MySQL username (config.mysql.username),
	// which is probably "blip". Also try "root" if not explicitly disabled.
	users := []string{ml.cfg.MySQL.Username}
	if !ml.cfg.MonitorLoader.Local.DisableAutoRoot {
		users = append(users, "root")
	}

	sockets := dbconn.Sockets()

	// For every user, try every socket, then 127.0.0.1.
USERS:
	for _, user := range users {

		cfg := blip.DefaultConfigMonitor()
		cfg.ApplyDefaults(ml.cfg)
		cfg.InterpolateEnvVars()
		moncfg := cfg
		moncfg.MonitorId = "localhost"
		moncfg.Username = user

	SOCKETS:
		for _, socket := range sockets {
			moncfg.Socket = socket
			cfg.InterpolateMonitor()

			if err := ml.testLocal(ctx, moncfg); err != nil {
				// Failed to connect
				blip.Debug("auto-detect socket %s user %s: fail: %s",
					moncfg.Socket, moncfg.Username, err)
				continue SOCKETS
			}

			// Connected via socket
			return []blip.ConfigMonitor{moncfg}, nil
		}

		// -------------------------------------------------------------------
		// TCP
		moncfg.Socket = ""
		moncfg.Hostname = "127.0.0.1:3306"
		cfg.InterpolateMonitor()

		if err := ml.testLocal(ctx, moncfg); err != nil {
			blip.Debug("local auto-detect tcp %s user %s: fail: %s",
				moncfg.Hostname, moncfg.Username, err)
			continue USERS
		}

		return []blip.ConfigMonitor{moncfg}, nil
	}

	return nil, nil
}

func (ml *Loader) testLocal(bg context.Context, moncfg blip.ConfigMonitor) error {
	db, _, err := ml.factory.DbConn.Make(moncfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()
	return db.PingContext(ctx)
}

// Monitor returns one monitor by ID.  It's used by the API to get single
// monitor status.
func (ml *Loader) Monitor(monitorId string) *Monitor {
	ml.Lock()
	defer ml.Unlock()
	return ml.dbmon[monitorId].monitor
}

// Monitors returns a list of all currently loaded monitors.
func (ml *Loader) Monitors() []*Monitor {
	ml.Lock()
	defer ml.Unlock()
	monitors := make([]*Monitor, len(ml.dbmon))
	i := 0
	for _, loaded := range ml.dbmon {
		monitors[i] = loaded.monitor
		i++
	}
	return monitors
}

// Count returns the number of loaded monitors. It's used by the API for status.
func (ml *Loader) Count() uint {
	ml.Lock()
	defer ml.Unlock()
	return uint(len(ml.dbmon))
}

func (ml *Loader) Unload(monitorId string) error {
	ml.Lock()
	defer ml.Unlock()

	m, ok := ml.dbmon[monitorId]
	if !ok {
		return nil
	}
	m.monitor.Stop()
	delete(ml.dbmon, monitorId)
	status.RemoveMonitor(monitorId)
	return nil
}

// Print prints all loaded monitors in blip.ConfigMonitor YAML format.
// It's used for --print-monitors.
func (ml *Loader) Print() string {
	ml.Lock()
	defer ml.Unlock()
	m := make([]blip.ConfigMonitor, len(ml.dbmon))
	i := 0
	for monitorId := range ml.dbmon {
		m[i] = ml.dbmon[monitorId].monitor.Config()
		i++
	}
	p := printMonitors{Monitors: m}
	bytes, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Sprintf("# yaml.Marshal error: %s", err) // shouldn't happen
	}
	return string(bytes)
}

// printMonitors is used by Print to output monitors in the correct YAML format.
type printMonitors struct {
	Monitors []blip.ConfigMonitor `yaml:"monitors"`
}
