package blip

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path"
	"runtime"
	"time"
)

const VERSION = "0.0.0"

var SHA = ""

// Metrics are metrics collected for one plan level, from one database instance.
type Metrics struct {
	Begin     time.Time                     // when collection started
	End       time.Time                     // when collection completed
	MonitorId string                        // ID of monitor (MySQL)
	Plan      string                        // plan name
	Level     string                        // level name
	State     string                        // state of monitor
	Values    map[string]map[string]float64 // keyed on domain => metric
}

// Monitor provides information about a MySQL instance that Blip monitors.
type Monitor interface {
	MonitorId() string
	Config() ConfigMonitor
	DB() *sql.DB
}

const (
	STATE_NONE      = ""
	STATE_OFFLINE   = "offline"
	STATE_STANDBY   = "standby"
	STATE_READ_ONLY = "read-only"
	STATE_ACTIVE    = "active"
)

var (
	Strict    = false
	Debugging = false
	debugLog  = log.New(os.Stderr, "DEBUG ", log.LstdFlags|log.Lmicroseconds)
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func Debug(msg string, v ...interface{}) {
	if !Debugging {
		return
	}
	_, file, line, _ := runtime.Caller(1)
	msg = fmt.Sprintf("%s:%d %s", path.Base(file), line, msg)
	debugLog.Printf(msg, v...)
}
