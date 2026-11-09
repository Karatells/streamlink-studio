package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Counters is a tiny in-memory counter set rendered in Prometheus format.
type Counters struct {
	mu sync.Mutex
	m  map[string]float64 // "name\x00labels" -> value
}

func NewCounters() *Counters { return &Counters{m: map[string]float64{}} }

// promLabels renders k1,v1,k2,v2 as {k1="v1",k2="v2"} (escaped, in the given order).
func promLabels(kv ...string) string {
	if len(kv) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		fmt.Fprintf(&b, `%s="%s"`, kv[i], v)
	}
	b.WriteByte('}')
	return b.String()
}

func (c *Counters) Inc(name string, kv ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m[name+"\x00"+promLabels(kv...)]++
	c.mu.Unlock()
}

func (c *Counters) snapshot() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]float64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// promWriter emits HELP/TYPE once per metric name.
type promWriter struct {
	b    strings.Builder
	seen map[string]bool
}

func (p *promWriter) sample(name, typ, help, labels string, v float64) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	if !p.seen[name] {
		p.seen[name] = true
		fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	fmt.Fprintf(&p.b, "%s%s %g\n", name, labels, v)
}

// counters writes every counter, grouped by name so TYPE lines stay contiguous.
func (p *promWriter) counters(c *Counters, help map[string]string) {
	snap := c.snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, labels, _ := strings.Cut(k, "\x00")
		p.sample(name, "counter", help[name], labels, snap[k])
	}
}

// Sum adds up every series of a counter whose label string contains sub ("" = all).
func (c *Counters) Sum(name, sub string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t float64
	for k, v := range c.m {
		n, labels, _ := strings.Cut(k, "\x00")
		if n == name && strings.Contains(labels, sub) {
			t += v
		}
	}
	return t
}
