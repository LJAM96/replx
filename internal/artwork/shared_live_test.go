package artwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func sharedKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func sharedOpts(name string, max int64) SharedOptions {
	return SharedOptions{Namespace: name, MaxBytes: max, ProtectedBytes: max / 2, FreeBytes: func(string) (uint64, error) { return 1 << 50, nil }}
}

func sharedPut(t *testing.T, s *Store, key string, priority Priority, body []byte) {
	t.Helper()
	r, err := s.Reserve(context.Background(), key, priority)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel(context.Background())
	if err := r.Commit(context.Background(), "image/jpeg", body); err != nil {
		t.Fatal(err)
	}
}

func TestSharedDeduplicationReplacementAndRestart(t *testing.T) {
	ctx, db := testdb.Begin(t)
	dir := t.TempDir()
	opts := sharedOpts(t.Name(), 64<<20)
	s, err := NewShared(db, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user-a-transform", "user-b-transform", "different-source-same-bytes"} {
		sharedPut(t, s, sharedKey(name), High, []byte("identical-image"))
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blobs != 1 || st.Variants != 3 || st.PayloadBytes != int64(len("identical-image")) || st.Reservations != 0 {
		t.Fatalf("deduplication: %+v", st)
	}
	// A new process-level Store preserves committed references and discovers
	// existing payloads; replacement must preserve the other two references.
	restarted, err := NewShared(db, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ct, body, ok := restarted.GetContext(ctx, sharedKey("user-b-transform")); !ok || ct != "image/jpeg" || string(body) != "identical-image" {
		t.Fatalf("restart lookup: %q %q %v", ct, body, ok)
	}
	sharedPut(t, restarted, sharedKey("user-a-transform"), High, []byte("new-image"))
	st, err = restarted.Stats(ctx)
	if err != nil || st.Blobs != 2 || st.Variants != 3 {
		t.Fatalf("replacement: %+v %v", st, err)
	}
	if _, body, ok := restarted.GetContext(ctx, sharedKey("user-b-transform")); !ok || string(body) != "identical-image" {
		t.Fatal("one reference replacement deleted another reference's blob")
	}
	_, err = db.Exec(ctx, `UPDATE artwork_variants SET expires_at=now()-interval '1 second' WHERE store_id=$1 AND variant_key<>$2`, restarted.shared.id, sharedKey("user-a-transform"))
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = restarted.Stats(ctx)
	if err != nil || st.Blobs != 1 || st.Variants != 1 {
		t.Fatalf("expired refs: %+v %v", st, err)
	}
}

func TestSharedRecoveryAndCorruption(t *testing.T) {
	ctx, db := testdb.Begin(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.body"), []byte("legacy-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewShared(db, dir, sharedOpts(t.Name(), 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Reserve(ctx, sharedKey("interrupted"), Normal)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeReservedTemp(s.shared.tmpPath(r.id), []byte("partial")); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `UPDATE artwork_reservations SET expires_at=now()-interval '1 second' WHERE store_id=$1`, s.shared.id)
	if err != nil {
		t.Fatal(err)
	}
	orphanDigest := sharedKey("orphan")
	orphan := s.shared.blobPath(orphanDigest)
	if err = os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if _, err = os.Stat(s.shared.tmpPath(r.id)); !os.IsNotExist(err) {
		t.Fatalf("stale temp remains: %v", err)
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Reservations != 0 || st.UntrackedBytes != int64(len("legacy-data")) {
		t.Fatalf("recovery: %+v %v", st, err)
	}
	key := sharedKey("repair")
	body := []byte("valid-image")
	sharedPut(t, s, key, Normal, body)
	sum := sha256.Sum256(body)
	path := s.shared.blobPath(hex.EncodeToString(sum[:]))
	if err = os.WriteFile(path, []byte(strings.Repeat("!", len(body))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.GetContext(ctx, key); ok {
		t.Fatal("corrupt content was served")
	}
	sharedPut(t, s, key, Normal, body)
	if _, got, ok := s.GetContext(ctx, key); !ok || string(got) != string(body) {
		t.Fatal("refill failed to repair corrupt content")
	}
}

func TestSharedPriorityAndLowDisk(t *testing.T) {
	ctx, db := testdb.Begin(t)
	var free atomic.Uint64
	free.Store(1 << 50)
	opts := sharedOpts(t.Name(), reservationBytes+5<<20)
	opts.MinFreeBytes = 1 << 20
	opts.FreeBytes = func(string) (uint64, error) { return free.Load(), nil }
	s, err := NewShared(db, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	background := sharedKey("background")
	poster := sharedKey("poster")
	sharedPut(t, s, background, Low, []byte(strings.Repeat("b", 3<<20)))
	sharedPut(t, s, poster, High, []byte(strings.Repeat("p", 3<<20)))
	// Another maximum-size transfer crosses the high watermark and reclaims
	// the lower tier while retaining the existing poster.
	incoming, err := s.Reserve(ctx, sharedKey("incoming-poster"), High)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.GetContext(ctx, background); ok {
		t.Fatal("cold background survived capacity eviction")
	}
	if _, _, ok := s.GetContext(ctx, poster); !ok {
		t.Fatal("poster was lost")
	}
	low, err := s.Reserve(ctx, sharedKey("speculative"), Low)
	if err == nil {
		low.Cancel(ctx)
		t.Fatal("lower priority admission exceeded the remaining reservation budget")
	} else if !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	incoming.Cancel(ctx)
	if _, _, ok := s.GetContext(ctx, poster); !ok {
		t.Fatal("lower priority admission evicted poster")
	}
	free.Store(1 << 20)
	if _, err = s.Reserve(ctx, sharedKey("low-disk"), Low); !errors.Is(err, ErrCapacity) {
		t.Fatalf("low disk admission: %v", err)
	}
	if _, _, ok := s.GetContext(ctx, poster); !ok {
		t.Fatal("low disk prevented an existing read")
	}
	st, err := s.Stats(ctx)
	if err != nil || !st.AdmissionPaused || st.Reservations != 0 {
		t.Fatalf("low disk stats: %+v %v", st, err)
	}
}

func TestSharedConcurrentReservationsStayWithinBudget(t *testing.T) {
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	opts := sharedOpts(t.Name()+time.Now().Format(time.RFC3339Nano), reservationBytes*3)
	s, err := NewShared(pool.Raw(), t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	defer pool.Raw().Exec(ctx, `DELETE FROM artwork_stores WHERE store_id=$1`, s.shared.id)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var held []*Reservation
	var unexpected []error
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Reserve(ctx, sharedKey(string(rune('a'+i))), Normal)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				held = append(held, r)
			} else if !errors.Is(err, ErrCapacity) {
				unexpected = append(unexpected, err)
			}
		}(i)
	}
	wg.Wait()
	if len(unexpected) != 0 {
		t.Fatalf("reservation errors: %v", unexpected)
	}
	if len(held) != 3 {
		t.Fatalf("budget should admit exactly three maxima, got %d", len(held))
	}
	st, err := s.Stats(ctx)
	if err != nil || st.TotalBytes > opts.MaxBytes || st.Reservations != 3 {
		t.Fatalf("concurrent budget: %+v %v", st, err)
	}
	for _, r := range held {
		r.Cancel(ctx)
	}
	st, err = s.Stats(ctx)
	if err != nil || st.TotalBytes != 0 || st.Reservations != 0 {
		t.Fatalf("release: %+v %v", st, err)
	}
}

// failArtworkDB fails publication after the filesystem rename, then makes the
// immediate cleanup connection unavailable. This models a database interruption
// at the exact boundary where an orphan must remain charged to its reservation.
type failArtworkDB struct {
	database.DBTX
	publishes atomic.Int32
	cleanup   atomic.Bool
}

func (d *failArtworkDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.cleanup.Swap(false) {
		return nil, errors.New("injected cleanup database outage")
	}
	tx, err := d.DBTX.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &failArtworkTx{Tx: tx, db: d}, nil
}

type failArtworkTx struct {
	pgx.Tx
	db *failArtworkDB
}

func (t *failArtworkTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO artwork_blobs(") && t.db.publishes.Load() > 0 {
		t.db.publishes.Add(-1)
		t.db.cleanup.Store(true)
		return pgconn.CommandTag{}, errors.New("injected publication SQL failure")
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func TestSharedFailedPublicationRemainsReservedUntilCleanup(t *testing.T) {
	ctx, db := testdb.Begin(t)
	faults := &failArtworkDB{DBTX: db}
	s, err := NewShared(faults, t.TempDir(), sharedOpts(t.Name(), reservationBytes*2))
	if err != nil {
		t.Fatal(err)
	}
	if s.Ready() {
		t.Fatal("new shared store must await background recovery")
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Ready() {
		t.Fatal("successful recovery did not mark store ready")
	}
	faults.publishes.Store(2)
	for i, body := range [][]byte{[]byte("orphan-one"), []byte("orphan-two")} {
		r, err := s.Reserve(ctx, sharedKey(string(rune('a'+i))), Normal)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Commit(ctx, "image/jpeg", body); err == nil {
			t.Fatal("publication failure was not surfaced")
		}
		sum := sha256.Sum256(body)
		if _, err = os.Stat(s.shared.blobPath(hex.EncodeToString(sum[:]))); err != nil {
			t.Fatalf("expected charged orphan candidate: %v", err)
		}
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Reservations != 2 || st.Blobs != 0 || st.ReservedBytes != reservationBytes*2 {
		t.Fatalf("failed publication accounting: %+v %v", st, err)
	}
	if _, err = s.Reserve(ctx, sharedKey("third-orphan"), Normal); !errors.Is(err, ErrCapacity) {
		t.Fatalf("orphan bytes escaped quota: %v", err)
	}
	if _, err = db.Exec(ctx, `UPDATE artwork_reservations SET expires_at=now()-interval '1 second' WHERE store_id=$1`, s.shared.id); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = s.Stats(ctx)
	if err != nil || st.TotalBytes != 0 || st.Reservations != 0 {
		t.Fatalf("candidate recovery: %+v %v", st, err)
	}
	var files int
	if err = filepath.WalkDir(s.shared.dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 0 {
		t.Fatalf("candidate files remain after recovery: %d", files)
	}
	// A failed reference publication for existing bytes cannot remove another
	// committed variant's shared blob during cancellation or expiry cleanup.
	sharedPut(t, s, sharedKey("committed"), Normal, []byte("shared-existing"))
	faults.publishes.Store(1)
	r, err := s.Reserve(ctx, sharedKey("failed-reference"), Normal)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Commit(ctx, "image/jpeg", []byte("shared-existing")); err == nil {
		t.Fatal("expected injected failure")
	}
	if _, err = db.Exec(ctx, `UPDATE artwork_reservations SET expires_at=now()-interval '1 second' WHERE store_id=$1`, s.shared.id); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, body, ok := s.GetContext(ctx, sharedKey("committed")); !ok || string(body) != "shared-existing" {
		t.Fatal("cleanup deleted a committed shared blob")
	}
}

func TestSharedExpiredReservationRejectsBeforeFilesystemWrite(t *testing.T) {
	ctx, db := testdb.Begin(t)
	var denyProbe atomic.Bool
	opts := sharedOpts(t.Name(), reservationBytes*2)
	opts.FreeBytes = func(string) (uint64, error) {
		if denyProbe.Load() {
			return 0, errors.New("filesystem probe reached before reservation validation")
		}
		return 1 << 50, nil
	}
	s, err := NewShared(db, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Reserve(ctx, sharedKey("expired"), Normal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `UPDATE artwork_reservations SET expires_at=now()-interval '1 second' WHERE store_id=$1`, s.shared.id); err != nil {
		t.Fatal(err)
	}
	denyProbe.Store(true)
	if err = r.Commit(ctx, "image/jpeg", []byte("must-not-write")); !errors.Is(err, ErrReservation) {
		t.Fatalf("expired transfer reached disk path: %v", err)
	}
	if _, err = os.Stat(s.shared.tmpPath(r.id)); !os.IsNotExist(err) {
		t.Fatalf("expired transfer created a temp file: %v", err)
	}
}

func TestSharedExpiredVariantsDrainInBoundedBatches(t *testing.T) {
	ctx, db := testdb.Begin(t)
	s, err := NewShared(db, t.TempDir(), sharedOpts(t.Name(), 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	key := sharedKey("expired-shared-blob")
	sharedPut(t, s, key, Protected, []byte("expired-artwork"))
	var digest string
	if err := db.QueryRow(ctx, `SELECT blob_digest FROM artwork_variants WHERE store_id=$1 AND variant_key=$2`, s.shared.id, key).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		if _, err := db.Exec(ctx, `INSERT INTO artwork_variants(store_id,variant_key,blob_digest,content_type,priority,expires_at)
			VALUES($1,$2,$3,'image/jpeg',3,now()-interval '1 second')`, s.shared.id, sharedKey(string(rune(i+1))), digest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE artwork_variants SET expires_at=now()-interval '1 second' WHERE store_id=$1`, s.shared.id); err != nil {
		t.Fatal(err)
	}
	removed, _ := s.Sweep()
	if removed != 0 || !s.shared.expiredPending.Load() {
		t.Fatalf("first sweep should leave a bounded backlog: removed=%d pending=%v", removed, s.shared.expiredPending.Load())
	}
	for rounds := 0; s.shared.expiredPending.Load() && rounds < 4; rounds++ {
		s.Sweep()
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Variants != 0 || st.Blobs != 0 || s.shared.expiredPending.Load() {
		t.Fatalf("expired backlog did not drain: %+v pending=%v err=%v", st, s.shared.expiredPending.Load(), err)
	}
}
