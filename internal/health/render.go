package health

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// viewer is the role the queries run as.
type viewer struct {
	Role string
	// AllStats reports that the role has the privileges of
	// pg_read_all_stats, so it sees every session in full.
	AllStats bool
}

// connections is the result of connectionsSQL.
type connections struct {
	Max    int
	Used   int
	States []stateCount
}

type stateCount struct {
	State string // empty for NULL: not visible to the role, or never reported
	Count int
}

// connectionLines renders one line: the connections used, then by state.
// When the role may not see the type of some backends, which need not be
// client backends, the count is an upper bound.
func connectionLines(c connections, v viewer) []string {
	var states []string
	hidden := 0
	for _, s := range c.States {
		switch {
		case s.State != "":
			states = append(states, fmt.Sprintf("%s %d", s.State, s.Count))
		case v.AllStats:
			states = append(states, fmt.Sprintf("no state %d", s.Count))
		default:
			hidden = s.Count
		}
	}
	line := fmt.Sprintf("%d of max_connections %d", c.Used, c.Max)
	if hidden > 0 {
		line = "up to " + line
	}
	if len(states) > 0 {
		line += ": " + strings.Join(states, ", ")
	}
	if hidden > 0 {
		line += fmt.Sprintf("; the type and state of %d are not visible to role %s", hidden, v.Role)
	}
	return []string{line}
}

// list holds the first items of a list, and how many there are in all.
type list[T any] struct {
	Total int
	Items []T
}

// lines renders one line per item, then one saying how many were left out;
// "none" when there are no items.
func (l list[T]) lines(line func(T) string) []string {
	if len(l.Items) == 0 {
		return []string{"none"}
	}
	lines := make([]string, 0, len(l.Items)+1)
	for _, item := range l.Items {
		lines = append(lines, line(item))
	}
	if more := l.Total - len(l.Items); more > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more", more))
	}
	return lines
}

// session is a row of pg_stat_activity. A role that may not see a session
// sees its pid, database and application name only.
type session struct {
	PID      int
	Database string
	State    string // empty when not visible to the role
	// Seconds is the time since the current or last query started or, for a
	// long-running session idle in transaction, since its transaction did.
	Seconds   int64
	Wait      string // wait_event_type:wait_event; empty when not waiting
	App       string
	BlockedBy []int // the pids that block it, for blocked sessions
}

// line renders s, e.g. "pid 76, database app, active for 1m5s, waiting on
// Lock:transactionid, blocked by 75", or for a session idle in transaction
// "pid 75, database app, idle in transaction, transaction open for 2h0m0s".
func (s session) line(v viewer) string {
	parts := []string{"pid " + strconv.Itoa(s.PID)}
	if s.Database != "" {
		parts = append(parts, "database "+s.Database)
	}
	switch s.State {
	case "":
		parts = append(parts, "state not visible to role "+v.Role)
	case "idle in transaction":
		parts = append(parts, s.State+", transaction open for "+duration(s.Seconds))
	default:
		parts = append(parts, s.State+" for "+duration(s.Seconds))
	}
	if s.Wait != "" {
		parts = append(parts, "waiting on "+s.Wait)
	}
	if s.App != "" {
		parts = append(parts, "application "+strconv.Quote(s.App))
	}
	if len(s.BlockedBy) > 0 {
		pids := make([]string, len(s.BlockedBy))
		for i, pid := range s.BlockedBy {
			pids[i] = strconv.Itoa(pid)
		}
		parts = append(parts, "blocked by "+strings.Join(pids, " "))
	}
	return strings.Join(parts, ", ")
}

// longRunningLines renders the long-running sessions. A role without the
// privileges of pg_read_all_stats cannot tell other roles' idle sessions from
// running ones, so the last line says so.
func longRunningLines(l list[session], v viewer) []string {
	lines := l.lines(func(s session) string { return s.line(v) })
	if !v.AllStats {
		lines = append(lines, "other roles' sessions are not visible to role "+v.Role)
	}
	return lines
}

func blockedLines(l list[session], v viewer) []string {
	return l.lines(func(s session) string { return s.line(v) })
}

// cacheHit is the result of cacheHitSQL.
type cacheHit struct {
	Database  string
	Hit, Read int64
}

func cacheHitLines(c cacheHit, _ viewer) []string {
	if c.Hit+c.Read == 0 {
		return []string{"no blocks read yet in database " + c.Database}
	}
	return []string{percent(c.Hit, c.Hit+c.Read, 2) + " in database " + c.Database}
}

// table is a row of deadTuplesSQL.
type table struct {
	Name                 string
	Dead, Live           int64
	AutovacuumSecondsAgo *int64 // nil when never autovacuumed
}

func deadTupleLines(tables []table, _ viewer) []string {
	if len(tables) == 0 {
		return []string{"none"}
	}
	lines := make([]string, len(tables))
	for i, t := range tables {
		autovacuum := "never autovacuumed"
		if t.AutovacuumSecondsAgo != nil {
			autovacuum = "autovacuumed " + duration(*t.AutovacuumSecondsAgo) + " ago"
		}
		lines[i] = fmt.Sprintf("%s: %s dead (%d dead, %d live), %s", t.Name, percent(t.Dead, t.Dead+t.Live, 1), t.Dead, t.Live, autovacuum)
	}
	return lines
}

// index is an item of unusedIndexesSQL.
type index struct {
	Name, Table, Size string
}

func unusedIndexLines(l list[index], _ viewer) []string {
	return l.lines(func(i index) string { return i.Name + " on " + i.Table + ": " + i.Size })
}

// wraparound is the result of wraparoundSQL.
type wraparound struct {
	Database          string
	Age, FreezeMaxAge int64
}

func wraparoundLines(w wraparound, _ viewer) []string {
	return []string{fmt.Sprintf("oldest age(datfrozenxid) %d (database %s): %s of autovacuum_freeze_max_age %d",
		w.Age, w.Database, percent(w.Age, w.FreezeMaxAge, 1), w.FreezeMaxAge)}
}

// replication is the result of replicationSQL.
type replication struct {
	Replica            bool
	ReplayedSecondsAgo *int64 // nil when no transaction was replayed yet
	UnreplayedBytes    *int64 // nil when no WAL was received by streaming
}

func replicationLines(r replication, _ viewer) []string {
	if !r.Replica {
		return []string{"not a replica"}
	}
	line := "replica: no transaction replayed yet"
	if r.ReplayedSecondsAgo != nil {
		line = "replica: last transaction replayed " + duration(*r.ReplayedSecondsAgo) + " ago"
	}
	if r.UnreplayedBytes != nil {
		line += fmt.Sprintf(", %d bytes of received WAL not replayed yet", *r.UnreplayedBytes)
	}
	return []string{line}
}

// duration renders seconds as Go does, e.g. "1m5s".
func duration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

// percent renders part of whole as a percentage with decimals digits after
// the point, e.g. "99.25%".
func percent(part, whole int64, decimals int) string {
	return strconv.FormatFloat(100*float64(part)/float64(whole), 'f', decimals, 64) + "%"
}
