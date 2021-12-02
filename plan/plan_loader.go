// Package plan provides the Loader singleton that loads metric collection plans.
package plan

import (
	"context"
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/square/blip"
	"github.com/square/blip/proto"
	"github.com/square/blip/sqlutil"
)

// planMeta is a blip.Plan plus metadata.
type planMeta struct {
	name   string
	source string
	shared bool
	plan   blip.Plan
}

// PlanLooader is a singleton service and repo for level plans.
type Loader struct {
	plugin       func(blip.ConfigPlans) ([]blip.Plan, error)
	sharedPlans  []planMeta            // keyed on Plan.Name
	monitorPlans map[string][]planMeta // keyed on monitorId, Plan.Name
	needToLoad   map[string]string     // keyed on monitorId => Plan.Table
	*sync.RWMutex
}

func NewLoader(plugin func(blip.ConfigPlans) ([]blip.Plan, error)) *Loader {
	return &Loader{
		plugin:       plugin,
		sharedPlans:  []planMeta{},
		monitorPlans: map[string][]planMeta{},
		needToLoad:   map[string]string{},
		RWMutex:      &sync.RWMutex{},
	}
}

func (pl *Loader) PlansLoaded(monitorId string) []proto.PlanLoaded {
	pl.RLock()
	defer pl.RUnlock()

	var loaded []proto.PlanLoaded

	if monitorId == "" {
		loaded = make([]proto.PlanLoaded, len(pl.sharedPlans))
		for i := range pl.sharedPlans {
			loaded[i] = proto.PlanLoaded{
				Name:   pl.sharedPlans[i].name,
				Source: pl.sharedPlans[i].source,
			}
		}
	} else {
		loaded = make([]proto.PlanLoaded, len(pl.monitorPlans[monitorId]))
		for i := range pl.monitorPlans[monitorId] {
			loaded[i] = proto.PlanLoaded{
				Name:   pl.sharedPlans[i].name,
				Source: pl.sharedPlans[i].source,
			}
		}
	}

	return loaded
}

// LoadShared loads all top-level (shared) plans: config.plans. These plans are
// called "shared" because more than one monitor can use them, which is the normal
// case. For example, the simplest configurate is specifying a single shared plan
// that almost monitors use implicitly (by not specifcying config.monitors.*.plans).
//
// This method is called by Server.Boot(). Plans from a table are deferred until
// the monitor's LPC calls Plan() because the monitor might not be online when Blip
// starts.
func (pl *Loader) LoadShared(cfg blip.ConfigPlans, dbMaker blip.DbFactory) error {

	if pl.plugin != nil {
		plans, err := pl.plugin(cfg)
		if err != nil {
			return err
		}

		pl.Lock()
		pl.sharedPlans = make([]planMeta, len(plans))
		for i, plan := range plans {
			pl.sharedPlans[i] = planMeta{
				name:   plan.Name,
				plan:   plan,
				source: "plugin",
			}
		}
		pl.Unlock()

		return nil
	}

	sharedPlans := []planMeta{}

	// Read default plans from table on pl.cfg.plans.monitor
	if cfg.Table != "" {
		blip.Debug("loading plans from %s", cfg.Table)

		// Connect to db specified by config.plans.monitor, which should have
		// been validated already, but double check. It reuses ConfigMonitor
		// for the DSN info, not because it's an actual db to monitor.
		if cfg.Monitor == nil {
			return fmt.Errorf("Table set but Monitor is nil")
		}

		db, _, err := dbMaker.Make(*cfg.Monitor)
		if err != nil {
			return err
		}
		defer db.Close()

		// Last arg "" = no monitorId, read all rows
		plans, err := ReadPlanTable(cfg.Table, db, "")
		if err != nil {
			return err
		}

		// Save all plans from table by name
		for _, plan := range plans {
			sharedPlans = append(sharedPlans, planMeta{
				name:   plan.Name,
				plan:   plan,
				source: cfg.Table,
			})
		}
	}

	// Read all plans from all files
	if len(cfg.Files) > 0 {
		blip.Debug("loading plans from %v", cfg.Files)
		plans, err := pl.readPlans(cfg.Files)
		if err != nil {
			blip.Debug(err.Error())
			return err
		}

		// Save all plans from table by name
		for _, pm := range plans {
			sharedPlans = append(sharedPlans, pm)
		}
	}

	if len(sharedPlans) == 0 && !blip.Strict {
		// Use built-in internal plan becuase neither config.plans.table
		// nor config.plans.file was specififed
		sharedPlans = append(sharedPlans, planMeta{
			name:   blip.INTERNAL_PLAN_NAME,
			plan:   blip.InternalLevelPlan(),
			source: "blip",
		})
	}

	pl.Lock()
	pl.sharedPlans = sharedPlans
	pl.Unlock()

	return nil
}

// Monitor plans: config.monitors.*.plans
func (pl *Loader) LoadMonitor(mon blip.ConfigMonitor, dbMaker blip.DbFactory) error {
	monitorPlans := []planMeta{}

	if mon.Plans.Table != "" {
		// Monitor plans from table, but defer until monitor's LPC calls Plan()
		table := mon.Plans.Table
		blip.Debug("%s: loading plans from table %s", mon.MonitorId, table)

		db, _, err := dbMaker.Make(mon)
		if err != nil {
			return err
		}
		defer db.Close()

		plans, err := ReadPlanTable(table, db, mon.MonitorId)
		if err != nil {
			return nil
		}

		pl.RUnlock() // -- R unlock
		pl.Lock()    // -- X lock

		for _, plan := range plans {
			monitorPlans = append(monitorPlans, planMeta{
				name:   plan.Name,
				plan:   plan,
				source: table,
			})
		}

		pl.Lock()  // -- X unlock
		pl.RLock() // -- R relock
	}

	if len(mon.Plans.Files) > 0 {
		// Monitor plans from files, load all
		blip.Debug("monitor %s plans from %s", mon.MonitorId, mon.Plans.Files)
		plans, err := pl.readPlans(mon.Plans.Files)
		if err != nil {
			return err
		}
		for _, pm := range plans {
			monitorPlans = append(monitorPlans, pm)
		}
	}

	if len(monitorPlans) == 0 && !blip.Strict {
		// Use built-in internal plan becuase neither config.plans.table
		// nor config.plans.file was specififed
		monitorPlans = append(monitorPlans, planMeta{
			name:   blip.INTERNAL_PLAN_NAME,
			shared: true, // copy from sharedPlans
			source: "blip",
		})
	}

	pl.Lock()
	pl.monitorPlans[mon.MonitorId] = monitorPlans
	pl.Unlock()
	blip.Debug("loaded plans for monitor %s", mon.MonitorId)

	return nil
}

// Plan returns the plan for the given monitor.
func (pl *Loader) Plan(monitorId string, planName string, db *sql.DB) (blip.Plan, error) {
	pl.RLock()
	defer pl.RUnlock()

	plans := pl.monitorPlans[monitorId]
	if len(plans) == 0 {
		return blip.Plan{}, fmt.Errorf("no plans loaded for monitor %s", monitorId)
	}

	var pm *planMeta
	if planName == "" {
		pm = &plans[0]
		planName = pm.name
		blip.Debug("%s: loading first plan: %s", monitorId, planName)
	} else {
		for i := range plans {
			if plans[i].name == planName {
				pm = &plans[i]
			}
		}
		if pm == nil {
			return blip.Plan{}, fmt.Errorf("monitor %s has no plan named %s", monitorId, planName)
		}
	}

	if pm.shared {
		blip.Debug("%s: loading plan %s (shared)", monitorId, pm.name)
		pm = nil
		for i := range pl.sharedPlans {
			if pl.sharedPlans[i].name == planName {
				pm = &pl.sharedPlans[i]
			}
		}
		if pm == nil {
			return blip.Plan{}, fmt.Errorf("monitor %s uses shared plan %s but it was not loaded", monitorId, planName)
		}
	}

	blip.Debug("%s: loading plan %s from %s", monitorId, planName, pm.source)
	return pm.plan, nil
}

func (pl *Loader) Print() {
	pl.RLock()
	defer pl.RUnlock()
	var bytes []byte

	for i := range pl.sharedPlans {
		bytes, _ = yaml.Marshal(pl.sharedPlans[i].plan.Levels)
		fmt.Printf("---\n# %s\n%s\n\n", pl.sharedPlans[i].plan.Name, string(bytes))
	}
	/*
		if len(pl.monitorPlans) > 0 {
			bytes, _ = yaml.Marshal(pl.monitorPlans)
			fmt.Printf("---\n%s\n\n", string(bytes))
		} else {
			fmt.Printf("---\n# No monitor plans\n\n")
		}
	*/
}

type planFile map[string]*blip.Level

func (pl *Loader) readPlans(filePaths []string) ([]planMeta, error) {
	plans := []planMeta{}

PATHS:
	for _, filePattern := range filePaths {

		files, err := filepath.Glob(filePattern)
		if err != nil {
			if blip.Strict {
				return nil, err
			}
			// @todo log bad glob
			blip.Debug("invalid glob, skipping: %s: %s", filePattern, err)
			continue PATHS
		}
		blip.Debug("files in %s: %v", filePattern, files)

	FILES:
		for _, file := range files {
			if pl.fileLoaded(file) {
				blip.Debug("already read %s", file)
				pm := planMeta{
					name:   file,
					shared: true,
				}
				plans = append(plans, pm)
				continue FILES
			}

			fileabs, err := filepath.Abs(file)
			if err != nil {
				blip.Debug("%s does not exist (abs), skipping")
				return nil, err
			}

			if _, err := os.Stat(file); err != nil {
				if blip.Strict {
					return nil, fmt.Errorf("config file %s (%s) does not exist", file, fileabs)
				}
				blip.Debug("%s does not exist, skipping")
				continue FILES
			}

			plan, err := ReadPlanFile(file)
			if err != nil {
				blip.Debug("cannot read %s (%s), skipping: %s", file, fileabs, err)
				continue FILES
			}

			pm := planMeta{
				name:   file,
				plan:   plan,
				source: fileabs,
			}
			plans = append(plans, pm)
			blip.Debug("loaded file %s (%s) as plan %s", file, fileabs, plan.Name)
		}
	}

	return plans, nil
}

func (pl *Loader) fileLoaded(file string) bool {
	for i := range pl.sharedPlans {
		if pl.sharedPlans[i].name == file {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------

func ReadPlanFile(file string) (blip.Plan, error) {
	bytes, err := ioutil.ReadFile(file)
	if err != nil {
		return blip.Plan{}, err
	}

	var pf planFile
	if err := yaml.Unmarshal(bytes, &pf); err != nil {
		return blip.Plan{}, fmt.Errorf("cannot decode YAML in %s: %s", file, err)
	}

	levels := make(map[string]blip.Level, len(pf))
	for k := range pf {
		levels[k] = blip.Level{
			Name:    k, // must have, levels are collected by name
			Freq:    pf[k].Freq,
			Collect: pf[k].Collect,
		}
	}

	plan := blip.Plan{
		Name:   file,
		Levels: levels,
	}
	return plan, nil
}

func ReadPlanTable(table string, db *sql.DB, monitorId string) ([]blip.Plan, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := fmt.Sprintf("SELECT name, plan, COALESCE(monitorId, '') FROM %s", sqlutil.SanitizeTable(table, blip.DEFAULT_DATABASE))
	if monitorId != "" {
		q += " WHERE monitorId = '" + monitorId + "' ORDER BY name ASC" // @todo sanitize
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans := []blip.Plan{}
	for rows.Next() {
		var plan blip.Plan
		var levels string
		err := rows.Scan(&plan.Name, &levels, &plan.MonitorId)
		if err != nil {
			return nil, err
		}
		err = yaml.Unmarshal([]byte(levels), &plan.Levels)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	return plans, nil
}