package heartbeat

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/sqlutil"
	"github.com/cashapp/blip/status"
)

const BLIP_TABLE_DDL = `CREATE TABLE IF NOT EXISTS heartbeat (
  monitor_id varchar(500)      NOT NULL PRIMARY KEY,  -- source
  ts         timestamp(3)      NOT NULL,              -- heartbeat
  freq       smallint unsigned NOT NULL               -- milliseconds
) ENGINE=InnoDB`

// WriteTimeout is how long to wait for MySQL to execute any heartbeat write.
// This should be much greater than the write frequency (config.hearbeat.freq)
// because it allows for slow network, MySQL, and so on.
var WriteTimeout = 5 * time.Second

// InitErrorWait is how long to wait between retries when initializing the
// heartbeat table (the first INSERT). This should be a relatively long wait.
var InitErrorWait = 10 * time.Second

// ReadOnlyWait is how long to wait when MySQL is read-only (not writable).
// This should be a long wait because it could mean Blip is effectively in
// standby mode on a read-only replica until it's promoted to be the writable
// source, which might not happen for a very long time.
var ReadOnlyWait = 20 * time.Second

type Writer struct {
	monitorId string
	db        *sql.DB
	cfg       blip.ConfigHeartbeat
	freq      time.Duration
	table     string
	timeout   time.Duration
	retryWait time.Duration
}

func NewWriter(monitorId string, db *sql.DB, cfg blip.ConfigHeartbeat) *Writer {
	if cfg.Freq == "" {
		panic("heartbeat.NewWriter called but config.heartbeat.freq not set")
	}
	if cfg.Table == "" {
		panic("heartbeat.NewWriter called but config.heartbeat.table not set")
	}

	freq, _ := time.ParseDuration(cfg.Freq)

	return &Writer{
		monitorId: monitorId,
		db:        db,
		cfg:       cfg,
		freq:      freq,
		table:     sqlutil.SanitizeTable(cfg.Table, blip.DEFAULT_DATABASE),
	}
}

const blip_hb_writer = "heartbeat-writer"

func (w *Writer) Write(stopChan, doneChan chan struct{}) error {
	defer close(doneChan)
	defer status.Monitor(w.monitorId, blip_hb_writer, "stopped")

	var (
		err    error
		ctx    context.Context
		cancel context.CancelFunc
	)

	// First INSERT: either creates row if it doesn't exist for this monitor ID,
	// or it updates an existing row with the current timestamp and frequency.
	// This must be done else the simpler UPDATE statements below, which is the
	// real heartbeat, will fail because there's no match row.
	ping := fmt.Sprintf("INSERT INTO %s (monitor_id, ts, freq) VALUES ('%s', NOW(3), %d) ON DUPLICATE KEY UPDATE ts=NOW(3), freq=%d",
		w.table, w.monitorId, w.freq.Milliseconds(), w.freq.Milliseconds())
	blip.Debug("hb writing: %s", ping)
	for {
		status.Monitor(w.monitorId, blip_hb_writer, "first insert")
		ctx, cancel = context.WithTimeout(context.Background(), WriteTimeout)
		_, err = w.db.ExecContext(ctx, ping)
		cancel()
		if err == nil { // success
			status.Monitor(w.monitorId, blip_hb_writer, "sleep")
			break
		}

		// Error --
		blip.Debug("%s: first insert, failed: %s", w.monitorId, err)
		if sqlutil.ReadOnly(err) {
			status.Monitor(w.monitorId, blip_hb_writer, "init: MySQL is read-only, sleeping %s", ReadOnlyWait)
			time.Sleep(ReadOnlyWait)
		} else {
			status.Monitor(w.monitorId, blip_hb_writer, "init: error: %s (sleeping %s)", err, InitErrorWait)
			time.Sleep(InitErrorWait)
		}

		// Was Stop called?
		select {
		case <-stopChan: // yes, return immediately
			return nil
		default: // no
		}
	}

	// ----------------------------------------------------------------------
	// Write heartbeats

	// This is the critical loop, so we use a query literal, not SQL ? params,
	// to void 2 wasted round trips: prep (waste), exec, close (waste).
	// This risk of SQL injection is miniscule because both table and monitorId
	// are sanitized, and Blip should only have write privs on its heartbeat table.
	ping = fmt.Sprintf("UPDATE %s SET ts=NOW(3) WHERE monitor_id='%s'", w.table, w.monitorId)
	blip.Debug(ping)
	for {
		time.Sleep(w.freq)

		status.Monitor(w.monitorId, blip_hb_writer, "write")
		ctx, cancel = context.WithTimeout(context.Background(), WriteTimeout)
		_, err = w.db.ExecContext(ctx, ping)
		cancel()
		if err != nil {
			if sqlutil.ReadOnly(err) {
				status.Monitor(w.monitorId, blip_hb_writer, "MySQL is read-only, sleeping %s", ReadOnlyWait)
				time.Sleep(ReadOnlyWait)
			} else {
				status.Monitor(w.monitorId, blip_hb_writer, "write error: %s", err)
				// No special sleep on random errors; keep trying to write at freq
			}
		} else {
			// Set status on successful Exec here, not before Sleep, so it
			// doesn't overwrite status set on Exec error; "sleep" = "write OK"
			status.Monitor(w.monitorId, blip_hb_writer, "sleep")
		}

		// Was Stop called?
		select {
		case <-stopChan: // yes, return immediately
			return nil
		default: // no
		}
	}
}