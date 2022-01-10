package sizedata

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/sqlutil"
)

const (
	DOMAIN = "size.data"

	OPT_INCLUDE = "include"
	OPT_EXCLUDE = "exclude"
	OPT_LIKE    = "like"
	OPT_TOTAL   = "total"
)

// Data collects data sizes for domain size.data.
type Data struct {
	db *sql.DB
	// --
	query map[string]string // keyed on level
	total map[string]bool   // keyed on level
}

func NewData(db *sql.DB) *Data {
	return &Data{
		db:    db,
		query: map[string]string{},
		total: map[string]bool{},
	}
}

func (c *Data) Domain() string {
	return DOMAIN
}

func (c *Data) Help() blip.CollectorHelp {
	return blip.CollectorHelp{
		Domain:      DOMAIN,
		Description: "Collect size of databases (total data size)",
		Options: map[string]blip.CollectorHelpOption{
			OPT_TOTAL: {
				Name:    OPT_TOTAL,
				Desc:    "Return total size of all databases (tag=\"\")",
				Default: "no",
				Values: map[string]string{
					"only": "Only total database size",
					"yes":  "Total and per-database sizes",
					"no":   "Only per-database sizes",
				},
			},
			OPT_INCLUDE: {
				Name: OPT_INCLUDE,
				Desc: "Comma-separate list of database names to include (overrides option " + OPT_INCLUDE + ")",
			},
			OPT_EXCLUDE: {
				Name:    OPT_EXCLUDE,
				Desc:    "Comma-separate list of database names to exclude (ignored if " + OPT_EXCLUDE + " set)",
				Default: "mysql,information_schema,performance_schema,sys",
			},
			OPT_LIKE: {
				Name:    OPT_LIKE,
				Desc:    fmt.Sprintf("Each database in %s or %s is a MySQL LIKE pattern", OPT_INCLUDE, OPT_EXCLUDE),
				Default: "no",
				Values: map[string]string{
					"yes": "Enable, use LIKE pattern matching",
					"no":  "Disable, use literal database names",
				},
			},
		},
		Groups: []blip.CollectorKeyValue{
			{Key: "db", Value: "database name, or empty string for all dbs"},
		},
		Metrics: []blip.CollectorMetric{
			{
				Name: "bytes",
				Type: blip.GAUGE,
				Desc: "Total size of all binary logs in bytes",
			},
		},
	}
}

// Prepares queries for all levels in the plan that contain the "var.global" domain
func (c *Data) Prepare(ctx context.Context, plan blip.Plan) error {
LEVEL:
	for _, level := range plan.Levels {
		dom, ok := level.Collect[DOMAIN]
		if !ok {
			continue LEVEL // not collected in this level
		}
		q, err := DataSizeQuery(dom.Options, c.Help())
		if err != nil {
			return err
		}
		c.query[level.Name] = q

		if dom.Options[OPT_TOTAL] == "yse" {
			c.total[level.Name] = true
		}
	}
	return nil
}

func (c *Data) Collect(ctx context.Context, levelName string) ([]blip.MetricValue, error) {
	q, ok := c.query[levelName]
	if !ok {
		return nil, nil // not collected in this level
	}

	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metrics := []blip.MetricValue{}

	var (
		name string
		val  string
	)
	for rows.Next() {
		if err = rows.Scan(&name, &val); err != nil {
			return nil, err
		}

		m := blip.MetricValue{
			Name: "bytes",
			Type: blip.GAUGE,
			Group: map[string]string{
				"db": name,
			},
		}

		m.Value, ok = sqlutil.Float64(val)
		if !ok {
			continue
		}

		metrics = append(metrics, m)
	}

	if c.total[levelName] {
		total := float64(0)
		for i := range metrics {
			total += metrics[i].Value
		}
		metrics = append(metrics, blip.MetricValue{
			Name:  "bytes",
			Type:  blip.GAUGE,
			Group: map[string]string{"db": ""}, // "" = total
		})
	}

	return metrics, err
}