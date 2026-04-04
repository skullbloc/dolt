package membench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"testing"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	dsqle "github.com/dolthub/dolt/go/libraries/doltcore/sqle"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/writer"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/types"
)

type memReport struct {
	Phase        string `json:"phase"`
	HeapAllocMB  float64 `json:"heap_alloc_mb"`
	HeapSysMB    float64 `json:"heap_sys_mb"`
	TotalAllocMB float64 `json:"total_alloc_mb"`
	NumGC        uint32  `json:"num_gc"`
	VmRSSKB      int64   `json:"vm_rss_kb,omitempty"`
}

func readMemStats(phase string) memReport {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	r := memReport{
		Phase:        phase,
		HeapAllocMB:  float64(m.HeapAlloc) / (1024 * 1024),
		HeapSysMB:    float64(m.HeapSys) / (1024 * 1024),
		TotalAllocMB: float64(m.TotalAlloc) / (1024 * 1024),
		NumGC:        m.NumGC,
	}

	// Try reading VmRSS from /proc/self/status
	data, err := os.ReadFile("/proc/self/status")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				var kb int64
				fmt.Sscanf(strings.TrimPrefix(line, "VmRSS:"), "%d", &kb)
				r.VmRSSKB = kb
				break
			}
		}
	}

	return r
}

func emitJSON(t *testing.T, r memReport) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s\n", b)
}

func TestGasTownMemory(t *testing.T) {
	ctx := context.Background()

	// --- Phase: baseline ---
	baseline := readMemStats("baseline")
	emitJSON(t, baseline)

	// --- Set up Dolt environment on disk ---
	tmp := t.TempDir()
	dbDir := path.Join(tmp, "testdb")
	if err := os.Mkdir(dbDir, os.ModePerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dbDir); err != nil {
		t.Fatal(err)
	}

	fs, err := filesys.LocalFilesysWithWorkingDir(dbDir)
	if err != nil {
		t.Fatal(err)
	}

	const userName = "test"
	const userEmail = "test@test.com"

	dEnv := env.Load(ctx, os.UserHomeDir, fs, doltdb.LocalDirDoltDB, "membench")
	err = dEnv.InitRepo(ctx, types.Format_Default, userName, userEmail, env.DefaultInitBranch)
	if err != nil {
		t.Fatal(err)
	}

	afterInit := readMemStats("after_init")
	emitJSON(t, afterInit)

	// --- Create engine ---
	db, err := dsqle.NewDatabase(ctx, "testdb", dEnv.DbData(ctx), editor.Options{})
	if err != nil {
		t.Fatal(err)
	}

	b := env.GetDefaultInitBranch(dEnv.Config)
	pro, err := dsqle.NewDoltDatabaseProviderWithDatabase(b, dEnv.FS, db, dEnv.FS, sql.EngineOverrides{})
	if err != nil {
		t.Fatal(err)
	}

	gcSafepointController := gcctx.NewGCSafepointController()
	engine := sqle.NewDefault(pro)

	globalCfg, _ := dEnv.Config.GetConfig(env.GlobalConfig)
	sqlCtx := newSQLCtx(ctx, pro, globalCfg, gcSafepointController)
	sqlCtx.SetCurrentDatabase(db.Name())

	afterEngine := readMemStats("after_engine_create")
	emitJSON(t, afterEngine)

	// --- Create table ---
	execMust(t, engine, sqlCtx, "CREATE TABLE issues (id INT PRIMARY KEY, title TEXT, status TEXT)")

	// --- Insert 100 rows ---
	var sb strings.Builder
	sb.WriteString("INSERT INTO issues VALUES ")
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("(%d, 'Issue title number %d', 'open')", i, i))
	}
	execMust(t, engine, sqlCtx, sb.String())

	// Commit the transaction
	trx := sqlCtx.GetTransaction()
	if trx != nil {
		err = dsess.DSessFromSess(sqlCtx.Session).CommitTransaction(sqlCtx, trx)
		if err != nil {
			t.Fatal(err)
		}
	}

	afterInsert := readMemStats("after_insert_100_rows")
	emitJSON(t, afterInsert)

	// --- Read rows back ---
	_, rowIter, _, err := engine.Query(sqlCtx, "SELECT * FROM issues ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}

	rowCount := 0
	for {
		_, err := rowIter.Next(sqlCtx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rowCount++
	}
	rowIter.Close(sqlCtx)

	if rowCount != 100 {
		t.Fatalf("expected 100 rows, got %d", rowCount)
	}

	afterSelect := readMemStats("after_select_all")
	emitJSON(t, afterSelect)

	// --- Summary ---
	fmt.Println("--- MEMORY SUMMARY ---")
	summary := map[string]interface{}{
		"baseline_heap_mb":     baseline.HeapAllocMB,
		"after_insert_heap_mb": afterInsert.HeapAllocMB,
		"after_select_heap_mb": afterSelect.HeapAllocMB,
		"delta_heap_mb":        afterSelect.HeapAllocMB - baseline.HeapAllocMB,
		"rows_read":            rowCount,
		"baseline_rss_kb":      baseline.VmRSSKB,
		"final_rss_kb":         afterSelect.VmRSSKB,
	}
	b2, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Printf("%s\n", b2)
}

func newSQLCtx(ctx context.Context, pro dsess.DoltDatabaseProvider, cfg config.ReadWriteConfig, gcSafepointController *gcctx.GCSafepointController) *sql.Context {
	s, err := dsess.NewDoltSession(sql.NewBaseSession(), pro, cfg, branch_control.CreateDefaultController(ctx), nil, writer.NewWriteSession, gcSafepointController, nil)
	if err != nil {
		panic(err)
	}
	s.SetCurrentDatabase("testdb")
	return sql.NewContext(ctx, sql.WithSession(s))
}

func execMust(t *testing.T, engine *sqle.Engine, sqlCtx *sql.Context, query string) {
	_, rowIter, _, err := engine.Query(sqlCtx, query)
	if err != nil {
		t.Fatalf("query %q failed: %v", query, err)
	}
	for {
		_, err := rowIter.Next(sqlCtx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("draining query %q: %v", query, err)
		}
	}
	rowIter.Close(sqlCtx)
}
