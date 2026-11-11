// Package netlog keeps a live, in-memory record of every connection a
// sandbox makes through the egress proxy: a bounded ring of recent requests
// and a per-host summary (counts, addresses, ports, bytes). The console polls
// it to show network activity in real time and to let an admin pick hosts to
// allow or block.
//
// It is deliberately not persisted: it is an observation window, not an
// audit trail (rule changes are audited). A restart starts it empty.
package netlog

import (
	"sort"
	"sync"
	"time"
)

// Limits. Memory is bounded by maxSandboxes * (maxEvents + maxHosts).
const (
	DefaultMaxEvents    = 500
	DefaultMaxHosts     = 500
	DefaultMaxSandboxes = 1000
	maxIPsPerHost       = 8
	maxPortsPerHost     = 8
)

// Event is one proxied request.
type Event struct {
	Seq     uint64    `json:"seq"`
	Time    time.Time `json:"time"`
	Method  string    `json:"method"` // CONNECT or the HTTP method
	Host    string    `json:"host"`
	Port    string    `json:"port"`
	IP      string    `json:"ip,omitempty"` // the address dialled (allowed requests)
	Allowed bool      `json:"allowed"`
	Reason  string    `json:"reason,omitempty"`
}

// Host summarises every request a sandbox made to one host.
type Host struct {
	Host        string    `json:"host"`
	IPs         []string  `json:"ips"`
	Ports       []string  `json:"ports"`
	Allowed     int64     `json:"allowed"`
	Denied      int64     `json:"denied"`
	BytesUp     int64     `json:"bytes_up"`
	BytesDown   int64     `json:"bytes_down"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	LastAllowed bool      `json:"last_allowed"`
	LastReason  string    `json:"last_reason,omitempty"`
}

type sandboxLog struct {
	events []Event // ring, oldest first after normalisation
	next   int     // ring write index once full
	hosts  map[string]*Host
	seen   time.Time
}

// Recorder is safe for concurrent use. The zero value is not usable; use New.
type Recorder struct {
	mu           sync.Mutex
	seq          uint64
	logs         map[string]*sandboxLog
	MaxEvents    int
	MaxHosts     int
	MaxSandboxes int
	now          func() time.Time
}

// New returns a Recorder with the default limits.
func New() *Recorder {
	return &Recorder{
		logs:         map[string]*sandboxLog{},
		MaxEvents:    DefaultMaxEvents,
		MaxHosts:     DefaultMaxHosts,
		MaxSandboxes: DefaultMaxSandboxes,
		now:          time.Now,
	}
}

func (r *Recorder) logFor(id string) *sandboxLog {
	l := r.logs[id]
	if l == nil {
		if len(r.logs) >= r.MaxSandboxes {
			r.evictOldestLocked()
		}
		l = &sandboxLog{hosts: map[string]*Host{}}
		r.logs[id] = l
	}
	return l
}

func (r *Recorder) evictOldestLocked() {
	var oldID string
	var old time.Time
	for id, l := range r.logs {
		if oldID == "" || l.seen.Before(old) {
			oldID, old = id, l.seen
		}
	}
	delete(r.logs, oldID)
}

// Record appends one request for sandboxID and returns its sequence number.
func (r *Recorder) Record(sandboxID string, e Event) uint64 {
	if sandboxID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if e.Time.IsZero() {
		e.Time = r.now().UTC()
	}
	l := r.logFor(sandboxID)
	l.seen = e.Time
	if len(l.events) < r.MaxEvents {
		l.events = append(l.events, e)
	} else {
		l.events[l.next] = e
		l.next = (l.next + 1) % r.MaxEvents
	}
	h := l.hosts[e.Host]
	if h == nil {
		if len(l.hosts) >= r.MaxHosts {
			evictHost(l.hosts)
		}
		h = &Host{Host: e.Host, IPs: []string{}, Ports: []string{}, FirstSeen: e.Time}
		l.hosts[e.Host] = h
	}
	h.LastSeen = e.Time
	h.LastAllowed = e.Allowed
	h.LastReason = e.Reason
	if e.Allowed {
		h.Allowed++
	} else {
		h.Denied++
	}
	h.IPs = addCapped(h.IPs, e.IP, maxIPsPerHost)
	h.Ports = addCapped(h.Ports, e.Port, maxPortsPerHost)
	return e.Seq
}

// AddBytes adds transferred bytes to a host's totals (called when a
// connection closes).
func (r *Recorder) AddBytes(sandboxID, host string, up, down int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.logs[sandboxID]; l != nil {
		if h := l.hosts[host]; h != nil {
			h.BytesUp += up
			h.BytesDown += down
		}
	}
}

// Forget drops everything recorded for a sandbox.
func (r *Recorder) Forget(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.logs, sandboxID)
}

// Snapshot returns the events after the given sequence number (oldest first,
// at most limit, the newest ones when more are pending), every host summary
// (most recently seen first) and the cursor to pass next time.
func (r *Recorder) Snapshot(sandboxID string, after uint64, limit int) (events []Event, hosts []Host, cursor uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cursor = r.seq
	events = []Event{}
	hosts = []Host{}
	l := r.logs[sandboxID]
	if l == nil {
		return events, hosts, cursor
	}
	ordered := make([]Event, 0, len(l.events))
	ordered = append(ordered, l.events[l.next:]...)
	ordered = append(ordered, l.events[:l.next]...)
	for _, e := range ordered {
		if e.Seq > after {
			events = append(events, e)
		}
	}
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	for _, h := range l.hosts {
		c := *h
		c.IPs = append([]string{}, h.IPs...)
		c.Ports = append([]string{}, h.Ports...)
		hosts = append(hosts, c)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if !hosts[i].LastSeen.Equal(hosts[j].LastSeen) {
			return hosts[i].LastSeen.After(hosts[j].LastSeen)
		}
		return hosts[i].Host < hosts[j].Host
	})
	return events, hosts, cursor
}

func evictHost(hosts map[string]*Host) {
	var oldest *Host
	for _, h := range hosts {
		if oldest == nil || h.LastSeen.Before(oldest.LastSeen) {
			oldest = h
		}
	}
	if oldest != nil {
		delete(hosts, oldest.Host)
	}
}

func addCapped(list []string, v string, max int) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	if len(list) >= max {
		return list
	}
	return append(list, v)
}

// CountSince counts the recorded requests (across every sandbox) at or after
// since, and how many of them were blocked. The rings are bounded, so this is
// a lower bound once a busy sandbox has wrapped its ring.
func (r *Recorder) CountSince(since time.Time) (total, blocked int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		for _, e := range l.events {
			if e.Time.Before(since) {
				continue
			}
			total++
			if !e.Allowed {
				blocked++
			}
		}
	}
	return total, blocked
}
