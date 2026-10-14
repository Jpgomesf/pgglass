package health

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type viewer struct {
	Role string
	// AllStats reports that the role has the privileges of
	// pg_read_all_stats, so it sees every session in full.
	AllStats bool
}

type connections struct {
	Max    int
	Used   int
	States []stateCount
}

type stateCount struct {
	State string
	Count int
}

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

type list[T any] struct {
	Total int
	Items []T
}

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

type session struct {
	PID      int
	Database string
	State    string
	// Seconds is the time since the current or last query started or, for a
	// long-running session, since its transaction did, when that was earlier.
	Seconds         int64
	FromTransaction bool
	Wait            string
	App             string
	BlockedBy       []int
}

func (s session) line(v viewer) string {
	parts := []string{"pid " + strconv.Itoa(s.PID)}
	if s.Database != "" {
		parts = append(parts, "database "+s.Database)
	}
	switch {
	case s.State == "":
		parts = append(parts, "state not visible to role "+v.Role)
	case s.FromTransaction || s.State == "idle in transaction":
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

type table struct {
	Name                 string
	Dead, Live           int64
	AutovacuumSecondsAgo *int64
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

type index struct {
	Name, Table, Size string
}

func unusedIndexLines(l list[index], _ viewer) []string {
	return l.lines(func(i index) string { return i.Name + " on " + i.Table + ": " + i.Size })
}

type wraparound struct {
	Database          string
	Age, FreezeMaxAge int64
}

func wraparoundLines(w wraparound, _ viewer) []string {
	return []string{fmt.Sprintf("oldest age(datfrozenxid) %d (database %s): %s of autovacuum_freeze_max_age %d",
		w.Age, w.Database, percent(w.Age, w.FreezeMaxAge, 1), w.FreezeMaxAge)}
}

type replication struct {
	Replica            bool
	ReplayedSecondsAgo *int64
	UnreplayedBytes    *int64
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

func duration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

func percent(part, whole int64, decimals int) string {
	return strconv.FormatFloat(100*float64(part)/float64(whole), 'f', decimals, 64) + "%"
}
