package vectordb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"

	"github.com/mmornati/leanproxy-mcp/pkg/migrate"
)

type sqliteStore struct {
	db     *sql.DB
	vec0   bool
	dim    int
	mu     sync.RWMutex
	logger *slog.Logger
	closed atomic.Bool
}

func newSQLiteStore(cfg *migrate.SQLiteVectorConfig, dim int, logger *slog.Logger) (*sqliteStore, error) {
	path := defaultSQLitePath()
	if cfg != nil && cfg.Path != "" {
		path = cfg.Path
	}

	if strings.ContainsAny(path, "?&") {
		return nil, fmt.Errorf("vectordb sqlite: path must not contain '?' or '&': %q", path)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("vectordb sqlite: create dir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("vectordb sqlite: open: %w", err)
	}

	s := &sqliteStore{
		db:     db,
		dim:    dim,
		logger: logger,
	}

	if err := s.init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("vectordb sqlite: init: %w", err)
	}

	logger.Info("vectordb sqlite initialized", "path", path, "vec0", s.vec0)
	return s, nil
}

func defaultSQLitePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "leanproxy", "cache", "vectors.db")
	}
	return filepath.Join(home, ".leanproxy", "cache", "vectors.db")
}

func (s *sqliteStore) init() error {
	if err := s.tryVec0(); err != nil {
		s.logger.Warn("vectordb sqlite: vec0 extension not available, falling back to manual cosine search", "error", err)
		s.vec0 = false
	}

	if err := s.createTables(); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	return nil
}

func (s *sqliteStore) tryVec0() error {
	var ext sql.NullString
	err := s.db.QueryRow("SELECT load_extension('vec0')").Scan(&ext)
	if err != nil {
		var name string
		err2 := s.db.QueryRow("SELECT name FROM pragma_module_list WHERE name = 'vec0'").Scan(&name)
		if err2 != nil {
			return fmt.Errorf("vec0 not available: %w (lookup: %v)", err, err2)
		}
		return nil
	}
	s.vec0 = true
	return nil
}

func (s *sqliteStore) createTables() error {
	mainTable := `CREATE TABLE IF NOT EXISTS vectors (
		id TEXT PRIMARY KEY,
		vector BLOB NOT NULL,
		metadata TEXT DEFAULT '{}'
	)`
	if _, err := s.db.Exec(mainTable); err != nil {
		return fmt.Errorf("vectors table: %w", err)
	}

	// id is the PRIMARY KEY, which already carries SQLite's automatic unique
	// index; the old explicit index only added write amplification.
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_vectors_id`); err != nil {
		return fmt.Errorf("vectors index cleanup: %w", err)
	}

	if s.vec0 {
		vecTable := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_vectors USING vec0(
			id TEXT PRIMARY KEY,
			vector float[%d]
		)`, s.dim)
		if _, err := s.db.Exec(vecTable); err != nil {
			s.logger.Warn("vectordb sqlite: vec0 table creation failed, using manual search", "error", err)
			s.vec0 = false
		}
	}

	return nil
}

func (s *sqliteStore) Upsert(ctx context.Context, records ...VectorRecord) error {
	if len(records) == 0 {
		return nil
	}

	if s.closed.Load() {
		return fmt.Errorf("vectordb sqlite: store closed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO vectors (id, vector, metadata) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, rec := range records {
		vecBytes := float32SliceToBytes(rec.Vector)
		metaBytes := marshalMetadata(rec.Metadata)
		if _, err := stmt.ExecContext(ctx, rec.ID, vecBytes, string(metaBytes)); err != nil {
			return fmt.Errorf("upsert %q: %w", rec.ID, err)
		}
	}

	if s.vec0 {
		vecStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO vec_vectors (id, vector) VALUES (?, ?)`)
		if err != nil {
			return fmt.Errorf("vec0 prepare: %w", err)
		}
		defer vecStmt.Close()

		for _, rec := range records {
			floatStr := float32SliceToString(rec.Vector)
			if _, err := vecStmt.ExecContext(ctx, rec.ID, floatStr); err != nil {
				return fmt.Errorf("vec0 upsert %q: %w", rec.ID, err)
			}
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) Search(ctx context.Context, vector []float32, k int) ([]SearchResult, error) {
	if k <= 0 {
		k = 10
	}

	if s.closed.Load() {
		return nil, fmt.Errorf("vectordb sqlite: store closed")
	}

	if s.vec0 {
		return s.searchVec0(ctx, vector, k)
	}
	return s.searchManual(ctx, vector, k)
}

func (s *sqliteStore) searchVec0(ctx context.Context, vector []float32, k int) ([]SearchResult, error) {
	floatStr := float32SliceToString(vector)
	// One round trip: the inner query keeps the exact vec0 KNN shape, the
	// join fetches the k winning records instead of a per-row lookup.
	query := `SELECT m.id, m.distance, v.vector, v.metadata
		FROM (SELECT id, distance FROM vec_vectors WHERE vector MATCH ? ORDER BY distance LIMIT ?) m
		JOIN vectors v ON v.id = m.id
		ORDER BY m.distance`

	rows, err := s.db.QueryContext(ctx, query, floatStr, k)
	if err != nil {
		return nil, fmt.Errorf("vec0 search: %w", err)
	}
	defer rows.Close()

	results := make([]SearchResult, 0, k)
	for rows.Next() {
		var id string
		var distance float64
		var vecBytes []byte
		var metaStr sql.NullString
		if err := rows.Scan(&id, &distance, &vecBytes, &metaStr); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		results = append(results, SearchResult{
			Record: VectorRecord{
				ID:       id,
				Vector:   bytesToFloat32Slice(vecBytes),
				Metadata: unmarshalMetadata([]byte(metaStr.String)),
			},
			Score: 1.0 - distance,
		})
	}

	return results, rows.Err()
}

func (s *sqliteStore) searchManual(ctx context.Context, vector []float32, k int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, vector, metadata FROM vectors`)
	if err != nil {
		return nil, fmt.Errorf("manual search query: %w", err)
	}
	defer rows.Close()

	// The query vector's squared norm is loop-invariant: computing it inside
	// the per-row cosine used to waste a third of the math on every row.
	var qna float64
	for _, f := range vector {
		qna += float64(f) * float64(f)
	}

	// Keep only the k best rows while scanning: for the default k of 5 a
	// sorted insert beats sorting (and JSON-decoding metadata for) every row.
	// Rows are scanned into sql.RawBytes (no database/sql copy) and scored
	// straight off the blob bytes; id/vector/metadata are only materialized
	// for the handful of rows that actually enter the top k, which takes the
	// scan from ~12KB of garbage per row to near-zero.
	type manualCand struct {
		id      string
		vec     []float32
		metaStr string
		score   float64
	}
	best := make([]manualCand, 0, k)
	var idB, vecBytes, metaB sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&idB, &vecBytes, &metaB); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if len(vecBytes)%4 != 0 {
			continue
		}

		// Fused decode + dot/norm pass. Accumulation order matches the old
		// cosineSimilarity exactly, so scores are bit-identical; a stored
		// vector of a different dimension scores 0, as before.
		var score float64
		if len(vecBytes) == len(vector)*4 && qna != 0 {
			var dot, nb float64
			for i := range vector {
				fb := float64(math.Float32frombits(binary.LittleEndian.Uint32(vecBytes[i*4:])))
				dot += float64(vector[i]) * fb
				nb += fb * fb
			}
			if nb != 0 {
				score = dot / (math.Sqrt(qna) * math.Sqrt(nb))
			}
		}
		if len(best) == k && score <= best[k-1].score {
			continue
		}

		// Insert in descending-score order, dropping the current worst.
		// Materialize copies now: RawBytes memory is reused on the next Scan.
		pos := len(best)
		for pos > 0 && best[pos-1].score < score {
			pos--
		}
		if len(best) < k {
			best = append(best, manualCand{})
		}
		copy(best[pos+1:], best[pos:len(best)-1])
		best[pos] = manualCand{id: string(idB), vec: bytesToFloat32Slice(vecBytes), metaStr: string(metaB), score: score}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	results := make([]SearchResult, 0, len(best))
	for _, c := range best {
		results = append(results, SearchResult{
			Record: VectorRecord{
				ID:       c.id,
				Vector:   c.vec,
				Metadata: unmarshalMetadata([]byte(c.metaStr)),
			},
			Score: c.score,
		})
	}
	return results, nil
}

func (s *sqliteStore) Delete(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}

	if s.closed.Load() {
		return fmt.Errorf("vectordb sqlite: store closed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM vectors WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("delete %q: %w", id, err)
		}
	}

	if s.vec0 {
		vecStmt, err := tx.PrepareContext(ctx, `DELETE FROM vec_vectors WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("vec0 prepare delete: %w", err)
		}
		defer vecStmt.Close()

		for _, id := range ids {
			if _, err := vecStmt.ExecContext(ctx, id); err != nil {
				s.logger.Warn("vectordb sqlite: vec0 delete failed", "id", id, "error", err)
			}
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.db.Close()
}

func float32SliceToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func bytesToFloat32Slice(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func float32SliceToString(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	// strconv.AppendFloat into one buffer: ~20-50x cheaper than a Sprintf
	// per element, and this runs once per search and per upserted record.
	buf := make([]byte, 0, len(v)*12+2)
	buf = append(buf, '[')
	for i, f := range v {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(f), 'f', -1, 32)
	}
	buf = append(buf, ']')
	return string(buf)
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa := float64(a[i])
		fb := float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func sortResults(results []SearchResult) {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
}

func marshalMetadata(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func unmarshalMetadata(data []byte) map[string]string {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return make(map[string]string)
	}
	return m
}
