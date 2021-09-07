package server

import (
	"database/sql"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/square/blip"
	"github.com/square/blip/collect"
	"github.com/square/blip/dbconn"
	"github.com/square/blip/event"
	"github.com/square/blip/ha"
	"github.com/square/blip/heartbeat"
	"github.com/square/blip/level"
	"github.com/square/blip/metrics"
	"github.com/square/blip/monitor"
	"github.com/square/blip/prom"
	"github.com/square/blip/sink"
)

type DbMonFactory interface {
	Make(blip.ConfigMonitor) *DbMon
}

var _ DbMonFactory = dbmonFactory{}

type dbmonFactory struct {
	mcMaker    metrics.CollectorFactory
	dbMaker    dbconn.Factory
	planLoader *collect.PlanLoader
}

func (f dbmonFactory) Make(cfg blip.ConfigMonitor) *DbMon {
	return &DbMon{
		monitorId:  dbconn.MonitorId(cfg),
		config:     cfg,
		mcMaker:    f.mcMaker,
		dbMaker:    f.dbMaker,
		planLoader: f.planLoader,
	}
}

type DbMon struct {
	// Factory values
	monitorId  string
	config     blip.ConfigMonitor
	mcMaker    metrics.CollectorFactory
	dbMaker    dbconn.Factory
	planLoader *collect.PlanLoader

	// Monitor and sub-components
	monitor *monitor.Monitor
	db      *sql.DB
	lpc     level.Collector
	lpa     level.Adjuster
	hbw     heartbeat.Writer
	hbr     heartbeat.Reader

	// Control chans and sync
	*sync.Mutex
	stopChan    chan struct{}
	doneChanLPA chan struct{}
	doneChanLPC chan struct{}
	doneChanHBW chan struct{}
	doneChanHBR chan struct{}
	stopped     bool
}

// MonitorId returns the monitor ID. This method implements blip.Monitor.
func (d *DbMon) MonitorId() string {
	return d.monitorId
}

// DB returns the low-level database connection. This method implements blip.Monitor.
func (d *DbMon) DB() *sql.DB {
	return d.db
}

// Config returns the monitor config. This method implements blip.Monitor.
func (d *DbMon) Config() blip.ConfigMonitor {
	return d.config
}

// Start starts monitoring the database if no error is returned.
func (d *DbMon) Start() error {
	var err error

	d.db, err = d.dbMaker.Make(d.config)
	if err != nil {
		return err // @todo
	}

	sinks := []sink.Sink{}
	for sinkName, opts := range d.config.Sinks {
		sink, err := sink.Make(sinkName, d.monitorId, opts)
		if err != nil {
			return err
		}
		sinks = append(sinks, sink)
		blip.Debug("%s sends to %s", d.monitorId, sinkName)
	}
	if len(sinks) == 0 && !blip.Strict {
		blip.Debug("using log sink")
		sink, _ := sink.Make("log", d.monitorId, map[string]string{})
		sinks = append(sinks, sink)
	}

	d.Mutex = &sync.Mutex{}
	d.stopChan = make(chan struct{})
	d.monitor = monitor.NewMonitor(d.monitorId, d.db, metrics.DefaultFactory)
	d.lpc = level.NewCollector(level.CollectorArgs{
		Monitor:    d.monitor,
		PlanLoader: d.planLoader,
		Sinks:      sinks,
	})
	go d.run()
	return nil
}

func (d *DbMon) run() {
	defer func() {
		if err := recover(); err != nil {
			b := make([]byte, 4096)
			n := runtime.Stack(b, false)
			log.Printf("PANIC: %s\n%s", err, string(b[0:n]))
		}
		d.Stop()
	}()

	if d.config.Exporter.Bind != "" {
		exp := prom.NewExporter(
			d.monitorId,
			d.db,
			d.mcMaker,
		)
		if err := exp.Prepare(collect.PromPlan()); err != nil {
			// @todo move to Boot
			blip.Debug(err.Error())
			return
		}
		api := prom.NewAPI(d.config.Exporter.Bind, d.monitorId, exp)
		go api.Run()
		if d.config.Exporter.Legacy {
			blip.Debug("legacy mode")
			<-d.stopChan
			return
		}
	}

	// Run option level plan adjuster (lpa). When enabled, the lpa checks the
	// state of MySQL . If the state changes,
	// it calls lpc.ChangePlan to change the plan as configured by
	// config.monitors.M.plans.adjust.<state>.
	if d.config.Plans.Adjust.Enabled() {
		d.doneChanLPA = make(chan struct{})
		d.lpa = level.NewAdjuster(level.AdjusterArgs{
			MonitorId: d.monitorId,
			Config:    d.config.Plans.Adjust,
			DB:        d.db,
			LPC:       d.lpc,
			HA:        ha.Disabled,
		})
		go d.lpa.Run(d.stopChan, d.doneChanLPA)
	} else {
		// When the lpa is not enabled, we need to get the party started by
		// setting the first (and only) plan: "". When lpc.ChangePlan passes that
		// along to planLoader.Plan, the plan loader will automatically find
		// and return the first plan by precedence: first plan from table, or
		// first plan file, or internal plan--trying monitor plans first, then
		// default plans. So it always finds something: the default internal plan,
		// if nothing else.
		//
		// Also, without an lpa, monitors default to active state.
		if err := d.lpc.ChangePlan(blip.STATE_ACTIVE, ""); err != nil {
			// @todo
		}
	}

	// Run optional heartbeat monitor to monitor replication lag. When enabled,
	// the heartbeat (hb) writes a high-resolution timestamp to a row in a table
	// at the configured frequence: config.monitors.M.heartbeat.freq.
	if !d.config.Heartbeat.Disable {

		if !d.config.Heartbeat.DisableWrite {
			d.hbw = heartbeat.NewWriter(d.monitorId, d.db)
			d.doneChanHBW = make(chan struct{})
			go d.hbw.Write(d.stopChan, d.doneChanHBW)
		}

		if !d.config.Heartbeat.DisableRead &&
			(len(d.config.Heartbeat.Source) > 0 || !d.config.Heartbeat.DisableAutoSource) {
			var sf heartbeat.SourceFinder
			if len(d.config.Heartbeat.Source) > 0 {
				sf = heartbeat.NewStaticSourceList(d.config.Heartbeat.Source, d.db)
			} else if !d.config.Heartbeat.DisableAutoSource {
				sf = heartbeat.NewAutoSourceFinder() // @todo
			} else {
				panic("no repl sources and auto-source disable")
			}
			d.hbr = heartbeat.NewReader(
				d.config,
				d.db,
				heartbeat.NewSlowFastWaiter(),
				sf,
			)
			d.doneChanHBR = make(chan struct{})
			go d.hbr.Read(d.stopChan, d.doneChanHBR)
		} else {
			blip.Debug("heartbeat read disabled: no sources, aut-source dissabled")
		}
	}

	// @todo inconsequential race condition

	// Run level plan collector (lpc). This is the foundation of d.
	d.doneChanLPC = make(chan struct{})
	if err := d.lpc.Run(d.stopChan, d.doneChanLPC); err != nil {
		blip.Debug(err.Error())
		// @todo
	}
}

func (d *DbMon) Stop() {
	d.Lock()
	defer d.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true

	defer event.Sendf(event.MONITOR_STOPPED, d.monitorId)

	close(d.stopChan)
	d.db.Close()

	running := 0
	if d.doneChanLPC != nil {
		running += 1 // lpc
	}
	if d.doneChanLPA != nil {
		running += 1 // lpa
	}
	if d.doneChanHBW != nil {
		running += 1 // + Heartbeat writer
	}
	if d.doneChanHBR != nil {
		running += 1 // + Heartbeat reader
	}

WAIT_LOOP:
	for running > 0 {
		blip.Debug("%s: %d running", d.monitorId, running)
		select {
		case <-d.doneChanLPA:
			blip.Debug("%s: lpa done", d.monitorId)
			d.doneChanLPA = nil
			running -= 1
		case <-d.doneChanLPC:
			blip.Debug("%s: lpc done", d.monitorId)
			d.doneChanLPC = nil
			running -= 1
		case <-d.doneChanHBW:
			blip.Debug("%s: hb writer done", d.monitorId)
			d.doneChanHBW = nil
			running -= 1
		case <-d.doneChanHBR:
			blip.Debug("%s: hb reader done", d.monitorId)
			d.doneChanHBR = nil
			running -= 1
		case <-time.After(2 * time.Second):
			// @todo
			break WAIT_LOOP
		}
	}
}
