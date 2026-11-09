// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Observable (pull) metrics, collected once per export interval.

type namedPool struct {
	attrs metric.MeasurementOption
	pool  *pgxpool.Pool
}

// snapshotPools copies the live pool set so the callback doesn't hold locks
// while reading stats.
func (g *GlobalContext) snapshotPools() []namedPool {
	var ret []namedPool
	g.RLock()
	for typ, p := range g.dbpools {
		ret = append(ret, namedPool{
			attrs: metric.WithAttributes(attribute.String("db.type", typ.ToString())),
			pool:  p,
		})
	}
	kv := g.kvShardMgr
	g.RUnlock()

	if kv != nil {
		kv.dbsMu.Lock()
		for id, p := range kv.dbs {
			ret = append(ret, namedPool{
				attrs: metric.WithAttributes(
					attribute.String("db.type", DbTypeKVStore.ToString()),
					attribute.String("db.kv_shard", strconv.Itoa(int(id))),
				),
				pool: p,
			})
		}
		kv.dbsMu.Unlock()
	}
	return ret
}

func registerObservables(g *GlobalContext, t *Telemetry, typ string) error {
	mt := t.meter
	var errs []error
	gauge := func(name, desc string) metric.Int64ObservableGauge {
		o, err := mt.Int64ObservableGauge(name, metric.WithDescription(desc))
		errs = append(errs, err)
		return o
	}
	counter := func(name, unit, desc string) metric.Int64ObservableCounter {
		o, err := mt.Int64ObservableCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return o
	}

	acquired := gauge("foks.db.pool.acquired", "Pooled connections checked out")
	idle := gauge("foks.db.pool.idle", "Idle pooled connections")
	total := gauge("foks.db.pool.total", "Total pooled connections")
	maxConns := gauge("foks.db.pool.max", "Pool size limit")
	emptyAcq := counter("foks.db.pool.empty_acquires", "{acquire}", "Acquires that had to wait for a connection")
	acqWait := counter("foks.db.pool.acquire_wait", "ms", "Cumulative time spent waiting on acquires")
	if err := errors.Join(errs...); err != nil {
		return err
	}
	_, err := mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, np := range g.snapshotPools() {
			s := np.pool.Stat()
			o.ObserveInt64(acquired, int64(s.AcquiredConns()), np.attrs)
			o.ObserveInt64(idle, int64(s.IdleConns()), np.attrs)
			o.ObserveInt64(total, int64(s.TotalConns()), np.attrs)
			o.ObserveInt64(maxConns, int64(s.MaxConns()), np.attrs)
			o.ObserveInt64(emptyAcq, s.EmptyAcquireCount(), np.attrs)
			o.ObserveInt64(acqWait, s.EmptyAcquireWaitTime().Milliseconds(), np.attrs)
		}
		return nil
	}, acquired, idle, total, maxConns, emptyAcq, acqWait)
	if err != nil {
		return err
	}

	if typ == "realtime" {
		return registerRealtimeObservables(g, t)
	}
	return nil
}

func registerRealtimeObservables(g *GlobalContext, t *Telemetry) error {
	mt := t.meter
	parked, err := mt.Int64ObservableGauge("foks.rt.pollers.parked",
		metric.WithDescription("rtPollInbox long-polls currently parked in this process"))
	if err != nil {
		return err
	}
	backlog, err := mt.Int64ObservableGauge("foks.rt.push_outbox.backlog",
		metric.WithDescription("push_outbox rows waiting to be sent, by status"))
	if err != nil {
		return err
	}
	oldest, err := mt.Float64ObservableGauge("foks.rt.push_outbox.oldest_age",
		metric.WithUnit("s"), metric.WithDescription("Age of the oldest queued/sending push_outbox row"))
	if err != nil {
		return err
	}
	pb := &pushBacklogProbe{g: g, every: 30 * time.Second}
	_, err = mt.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		o.ObserveInt64(parked, int64(g.RTInboxHub().NumParked()))
		counts, age, ok := pb.read(ctx)
		if ok {
			for status, n := range counts {
				o.ObserveInt64(backlog, n, metric.WithAttributes(attribute.String("status", status)))
			}
			o.ObserveFloat64(oldest, age)
		}
		return nil
	}, parked, backlog, oldest)
	return err
}

// pushBacklogProbe queries push_outbox at most once per `every`, reusing the
// last result in between, so the export interval can stay short.
type pushBacklogProbe struct {
	g     *GlobalContext
	every time.Duration

	mu     sync.Mutex
	last   time.Time
	counts map[string]int64
	age    float64
	ok     bool
}

func (p *pushBacklogProbe) read(ctx context.Context) (map[string]int64, float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.last) < p.every {
		return p.counts, p.age, p.ok
	}
	p.last = time.Now()
	counts, age, err := p.query(ctx)
	if err != nil {
		p.g.Log().Debugw("telemetry", "stage", "push_outbox backlog", "err", err)
		p.ok = false
		return nil, 0, false
	}
	p.counts, p.age, p.ok = counts, age, true
	return counts, age, true
}

func (p *pushBacklogProbe) query(ctx context.Context) (map[string]int64, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	db, err := p.g.Db(ctx, DbTypeRealTime)
	if err != nil {
		return nil, 0, err
	}
	defer db.Release()
	// Served by push_outbox_queue_idx (status, ctime) WHERE status IN (...).
	rows, err := db.Query(ctx,
		`SELECT status::text, COUNT(*),
		        COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(ctime)), 0)::float8
		 FROM push_outbox
		 WHERE status IN ('queued', 'sending')
		 GROUP BY status`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[string]int64{"queued": 0, "sending": 0}
	var oldest float64
	for rows.Next() {
		var status string
		var n int64
		var age float64
		if err := rows.Scan(&status, &n, &age); err != nil {
			return nil, 0, err
		}
		counts[status] = n
		if age > oldest {
			oldest = age
		}
	}
	return counts, oldest, rows.Err()
}
