// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package pgclient

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/collector/model/pdata"

	"github.com/timescale/promscale/pkg/ha"
	"github.com/timescale/promscale/pkg/log"
	"github.com/timescale/promscale/pkg/pgmodel/cache"
	"github.com/timescale/promscale/pkg/pgmodel/health"
	"github.com/timescale/promscale/pkg/pgmodel/ingestor"
	"github.com/timescale/promscale/pkg/pgmodel/lreader"
	"github.com/timescale/promscale/pkg/pgmodel/querier"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/prompb"
	"github.com/timescale/promscale/pkg/promql"
	"github.com/timescale/promscale/pkg/query"
	"github.com/timescale/promscale/pkg/telemetry"
	"github.com/timescale/promscale/pkg/tenancy"
	"github.com/timescale/promscale/pkg/util"
)

var PromscaleID uuid.UUID

func init() {
	// PromscaleID must always be generated on start, so that it remains constant throughout the lifecycle.
	PromscaleID = uuid.New()
}

// LockFunc does connect validation function, useful for things such as acquiring locks
// that should live the duration of the connection
type LockFunc = func(ctx context.Context, conn *pgx.Conn) error

// Client sends Prometheus samples to TimescaleDB
type Client struct {
	Connection        pgxconn.PgxConn
	QuerierConnection pgxconn.PgxConn
	ingestor          *ingestor.DBIngestor
	querier           querier.Querier
	healthCheck       health.HealthCheckerFn
	queryable         promql.Queryable
	ConnectionStr     string
	metricCache       cache.MetricCache
	labelsCache       cache.LabelsCache
	seriesCache       cache.SeriesCache
	closePool         bool
	sigClose          chan struct{}
	haService         *ha.Service
	TelemetryEngine   telemetry.Engine
	stopHealthChecker context.CancelFunc
}

// NewClient creates a new PostgreSQL client
func NewClient(cfg *Config, mt tenancy.Authorizer, schemaLocker LockFunc, readOnly bool) (*Client, error) {
	pgConfig, numCopiers, err := getPgConfig(cfg)
	if err != nil {
		return nil, err
	}

	pgConfig.AfterConnect = schemaLocker
	connectionPool, err := pgxpool.ConnectConfig(context.Background(), pgConfig)
	if err != nil {
		log.Error("msg", "err creating connection pool for new client", "err", err.Error())
		return nil, err
	}

	client, err := NewClientWithPool(cfg, numCopiers, connectionPool, mt, readOnly)
	if err != nil {
		return client, err
	}
	client.closePool = true
	return client, err
}

func getPgConfig(cfg *Config) (*pgxpool.Config, int, error) {
	minConnections, maxConnections, numCopiers, err := cfg.GetNumConnections()
	if err != nil {
		log.Error("msg", "configuring number of connections", "err", err.Error())
		return nil, numCopiers, err
	}
	connectionStr := cfg.GetConnectionStr()
	pgConfig, err := pgxpool.ParseConfig(connectionStr)
	if err != nil {
		log.Error("msg", "configuring connection", "err", err.Error())
		return nil, numCopiers, err
	}

	// Configure the number of connections and statement cache capacity.
	pgConfig.MinConns = int32(minConnections)
	pgConfig.MaxConns = int32(maxConnections)

	var statementCacheLog string
	if cfg.EnableStatementsCache {
		// Using the PGX default of 512 for statement cache capacity.
		statementCacheCapacity := 512
		pgConfig.AfterRelease = observeStatementCacheState
		statementCacheEnabled.Set(1)
		statementCacheCap.Set(float64(statementCacheCapacity))
		statementCacheLog = fmt.Sprintf("%d statements", statementCacheCapacity)
	} else {
		log.Info("msg", "Statements cached disabled, using simple protocol for database connections.")
		pgConfig.ConnConfig.PreferSimpleProtocol = true
		statementCacheEnabled.Set(0)
		statementCacheCap.Set(0)
		statementCacheLog = "disabled"

	}
	log.Info("msg", getRedactedConnStr(connectionStr),
		"numCopiers", numCopiers,
		"pool_max_conns", maxConnections,
		"pool_min_conns", minConnections,
		"statement_cache", statementCacheLog,
	)
	return pgConfig, numCopiers, nil
}

func getRedactedConnStr(s string) string {
	connURL, err := url.Parse(s)

	// Should never happen because we parsing the URL way before this
	// and error out if this happened.
	if err != nil {
		return "****"
	}

	if _, pwSet := connURL.User.Password(); pwSet {
		connURL.User = url.UserPassword(connURL.User.Username(), "****")
	}

	return connURL.String()
}

// NewClientWithPool creates a new PostgreSQL client with an existing connection pool.
func NewClientWithPool(cfg *Config, numCopiers int, connPool *pgxpool.Pool, mt tenancy.Authorizer, readOnly bool) (*Client, error) {
	dbConn := pgxconn.NewPgxConn(connPool)
	sigClose := make(chan struct{})
	metricsCache := cache.NewMetricCache(cfg.CacheConfig)
	labelsCache := cache.NewLabelsCache(cfg.CacheConfig)
	seriesCache := cache.NewSeriesCache(cfg.CacheConfig, sigClose)
	c := ingestor.Cfg{
		NumCopiers:             numCopiers,
		IgnoreCompressedChunks: cfg.IgnoreCompressedChunks,
		AsyncAcks:              cfg.AsyncAcks,
	}

	labelsReader := lreader.NewLabelsReader(dbConn, labelsCache)
	exemplarKeyPosCache := cache.NewExemplarLabelsPosCache(cfg.CacheConfig)

	dbQuerierConn := pgxconn.NewQueryLoggingPgxConn(connPool)
	dbQuerier := querier.NewQuerier(dbQuerierConn, metricsCache, labelsReader, exemplarKeyPosCache, mt.ReadAuthorizer())
	queryable := query.NewQueryable(dbQuerier, labelsReader)

	var telemetryEngine telemetry.Engine
	engine, err := telemetry.NewEngine(dbConn, PromscaleID, queryable)
	if err != nil {
		log.Debug("msg", "err creating telemetry engine", "err", err.Error())
	}
	if engine == nil {
		telemetryEngine = telemetry.NewNoopEngine()
	} else {
		telemetryEngine = engine
	}
	telemetryEngine.Start() // We stop the engine at client.Close().

	var dbIngestor *ingestor.DBIngestor
	if !readOnly {
		var err error
		dbIngestor, err = ingestor.NewPgxIngestor(dbConn, metricsCache, seriesCache, exemplarKeyPosCache, &c, telemetryEngine)
		if err != nil {
			log.Error("msg", "err starting the ingestor", "err", err)
			return nil, err
		}
	}

	healthCheckerCtx, stopHealthChecker := context.WithCancel(context.Background())
	healthCheckRoutine(healthCheckerCtx, dbConn)

	client := &Client{
		Connection:        dbConn,
		QuerierConnection: dbQuerierConn,
		ingestor:          dbIngestor,
		querier:           dbQuerier,
		healthCheck:       health.NewHealthChecker(dbConn),
		queryable:         queryable,
		metricCache:       metricsCache,
		labelsCache:       labelsCache,
		seriesCache:       seriesCache,
		sigClose:          sigClose,
		TelemetryEngine:   telemetryEngine,
		stopHealthChecker: stopHealthChecker,
	}

	InitClientMetrics(client)
	return client, nil
}

// Close closes the client and performs cleanup
func (c *Client) Close() {
	log.Info("msg", "Shutting down Client")
	if c.stopHealthChecker != nil {
		c.stopHealthChecker()
	}
	if c.TelemetryEngine != nil {
		c.TelemetryEngine.Stop()
	}
	if c.ingestor != nil {
		c.ingestor.Close()
	}
	close(c.sigClose)
	if c.closePool {
		c.Connection.Close()
	}
	if c.haService != nil {
		c.haService.Close()
	}
}

func (c *Client) Ingestor() *ingestor.DBIngestor {
	return c.ingestor
}

// Ingest writes the timeseries object into the DB
func (c *Client) Ingest(ctx context.Context, r *prompb.WriteRequest) (uint64, uint64, error) {
	return c.ingestor.Ingest(ctx, r)
}

// IngestTraces writes the traces object into the DB.
func (c *Client) IngestTraces(ctx context.Context, tr pdata.Traces) error {
	return c.ingestor.IngestTraces(ctx, tr)
}

// Read returns the promQL query results
func (c *Client) Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error) {
	if req == nil {
		return nil, nil
	}

	resp := prompb.ReadResponse{
		Results: make([]*prompb.QueryResult, len(req.Queries)),
	}

	qr := c.querier.RemoteReadQuerier()

	for i, q := range req.Queries {
		tts, err := qr.Query(q)
		if err != nil {
			return nil, err
		}
		resp.Results[i] = &prompb.QueryResult{
			Timeseries: tts,
		}
	}

	return &resp, nil
}

func (c *Client) NumCachedMetricNames() int {
	return c.metricCache.Len()
}

func (c *Client) MetricNamesCacheCapacity() int {
	return c.metricCache.Cap()
}

func (c *Client) NumCachedLabels() int {
	return c.labelsCache.Len()
}

func (c *Client) LabelsCacheCapacity() int {
	return c.labelsCache.Cap()
}

// HealthCheck checks that the client is properly connected
func (c *Client) HealthCheck() error {
	return c.healthCheck()
}

// Queryable returns the Prometheus promql.Queryable interface that's running
// with the same underlying Querier as the Client.
func (c *Client) Queryable() promql.Queryable {
	return c.queryable
}

func observeStatementCacheState(conn *pgx.Conn) bool {
	// connections have been opened and are released already
	// but the Client metrics have not been initialized yet
	if statementCacheLen == nil {
		return true
	}
	statementCache := conn.StatementCache()
	if statementCache == nil {
		return true
	}

	statementCacheSize := statementCache.Len()
	statementCacheLen.Observe(float64(statementCacheSize))
	return true
}

func healthCheckRoutine(ctx context.Context, conn pgxconn.PgxConn) {
	r := prometheus.DefaultRegisterer
	if env := os.Getenv("IS_TEST"); env == "true" {
		r = prometheus.NewRegistry()
	}
	dbHealthChecks := prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: util.PromNamespace,
			Subsystem: "database",
			Name:      "health_checks_total",
			Help:      "Total number of database health checks performed.",
		},
	)
	dbHealthErrors := prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: util.PromNamespace,
			Subsystem: "database",
			Name:      "health_check_errors_total",
			Help:      "Total number of database health check errors.",
		},
	)
	r.MustRegister(dbHealthChecks, dbHealthErrors)
	go func() {
		check := time.NewTicker(time.Minute)
		defer check.Stop()
		connection := health.NewHealthChecker(conn)
		for {
			select {
			case <-ctx.Done():
				return
			case <-check.C:
			}
			dbHealthChecks.Inc()
			if err := connection(); err != nil {
				dbHealthErrors.Inc()
			}
		}
	}()
}
