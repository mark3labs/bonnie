package schedule

import (
	"fmt"
	"time"
	_ "time/tzdata" // Built binaries need IANA zones on hosts without zone files.

	cron "github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func parseSchedule(expr, zone string) (cron.Schedule, *time.Location, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, nil, fmt.Errorf("schedule: time zone %q: %w", zone, err)
	}
	s, err := cronParser.Parse(expr)
	if err != nil {
		return nil, nil, fmt.Errorf("schedule: cron %q: %w", expr, err)
	}
	return s, loc, nil
}
