package end_to_end_tests

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/timescale/promscale/pkg/internal/testhelpers"
	ingstr "github.com/timescale/promscale/pkg/pgmodel/ingestor"
	"github.com/timescale/promscale/pkg/pgmodel/metrics/database"
	"github.com/timescale/promscale/pkg/pgxconn"
	"github.com/timescale/promscale/pkg/util"
)

func TestDatabaseMetrics(t *testing.T) {
	if !*useTimescaleDB {
		t.Skip("test meaningless without TimescaleDB")
	}
	withDB(t, *testDatabase, func(dbOwner *pgxpool.Pool, t testing.TB) {
		db := testhelpers.PgxPoolWithRole(t, *testDatabase, "prom_writer")
		defer db.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dbMetrics := database.NewEngine(ctx, pgxconn.NewPgxConn(db))

		// Before updating the metrics.
		compressionStatus := getMetricValue(t, "compression_status")
		require.Equal(t, float64(0), compressionStatus)
		numMaintenanceJobs := getMetricValue(t, "worker_maintenance_job")
		require.Equal(t, float64(0), numMaintenanceJobs)
		chunksCreated := getMetricValue(t, "chunks_created")
		require.Equal(t, float64(0), chunksCreated)

		// Update the metrics.
		require.NoError(t, dbMetrics.Update())

		// After updating the metrics.
		compressionStatus = getMetricValue(t, "compression_status")
		require.Equal(t, float64(1), compressionStatus)
		numMaintenanceJobs = getMetricValue(t, "worker_maintenance_job")
		require.Equal(t, float64(2), numMaintenanceJobs)
		chunksCreated = getMetricValue(t, "chunks_created")
		require.Equal(t, float64(0), chunksCreated)

		// Ingest some data and then see check the metrics to ensure proper updating.
		ingestor, err := ingstr.NewPgxIngestorForTests(pgxconn.NewPgxConn(db), nil)
		require.NoError(t, err)
		defer ingestor.Close()

		require.NoError(t, ingestor.IngestTraces(context.Background(), generateTestTrace()))

		// Update the metrics again.
		require.NoError(t, dbMetrics.Update())

		chunksCreated = getMetricValue(t, "chunks_created")
		require.Equal(t, chunksCreated, float64(3))
	})
}

func getMetricValue(t testing.TB, name string) float64 {
	metric, err := database.GetMetric(name)
	require.NoError(t, err)

	val, err := util.ExtractMetricValue(metric)
	require.NoError(t, err)
	return val
}
