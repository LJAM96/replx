package artwork

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/jackc/pgx/v5"
)

// Priority is a retention preference, not a permission or permanent pin.
type Priority int

const (
	Low Priority = iota
	Normal
	High
	Protected

	variantOverhead  = int64(1024)
	blobOverhead     = int64(4096)
	reservationBytes = int64(MaxBodyBytes) + variantOverhead + blobOverhead
	reservationTTL   = 2 * time.Minute
)

var (
	ErrCapacity     = errors.New("artwork: admission capacity unavailable")
	ErrReservation  = errors.New("artwork: reservation expired or closed")
	ErrInitializing = errors.New("artwork: recovery is still initializing")
)

// SharedOptions uses explicit bytes. MetadataBytes is a conservative logical
// allowance for index records; PostgreSQL and WAL still need their own budget.
// FreeBytes is an injectable filesystem probe; production leaves it nil.
type SharedOptions struct {
	Namespace         string
	MaxBytes          int64
	MinFreeBytes      int64
	LowWatermarkBytes int64
	ProtectedBytes    int64
	MaxReservations   int
	FreeBytes         func(string) (uint64, error)
}

type sharedStore struct {
	db              database.DBTX
	base, dir, id   string
	opts            SharedOptions
	ready           atomic.Bool
	expiredPending  atomic.Bool
	maintenanceWake chan struct{}
	initialMu       sync.Mutex
}

// Stats reports storage and admission state without exposing variant keys,
// caller identities, Plex tokens, resource URLs or filesystem paths.
type Stats struct {
	Shared          bool   `json:"shared"`
	PayloadBytes    int64  `json:"payloadBytes"`
	MetadataBytes   int64  `json:"metadataBytes"`
	ReservedBytes   int64  `json:"reservedBytes"`
	UntrackedBytes  int64  `json:"untrackedBytes"`
	TotalBytes      int64  `json:"totalBytes"`
	MaxBytes        int64  `json:"maxBytes"`
	MinFreeBytes    int64  `json:"minFreeBytes"`
	FreeBytes       uint64 `json:"freeBytes"`
	ProtectedBytes  int64  `json:"protectedBytes"`
	Blobs           int64  `json:"blobs"`
	Variants        int64  `json:"variants"`
	Reservations    int64  `json:"reservations"`
	AdmissionPaused bool   `json:"admissionPaused"`
}

// NewShared creates a fresh storage namespace below dir. Old flat-cache files
// are never imported; recovery counts them against admission until removed by
// the operator. All instances sharing a namespace must use the same directory
// and options. PostgreSQL row locks protect publication, eviction and readers.
// Migrations may still be pending: construction does not access PostgreSQL.
func NewShared(db database.DBTX, dir string, opts SharedOptions) (*Store, error) {
	if db == nil || dir == "" || opts.MaxBytes <= 0 || opts.MinFreeBytes < 0 {
		return nil, errors.New("artwork: invalid shared store configuration")
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.LowWatermarkBytes == 0 {
		opts.LowWatermarkBytes = opts.MaxBytes * 9 / 10
	}
	if opts.ProtectedBytes == 0 {
		opts.ProtectedBytes = opts.MaxBytes / 20
	}
	if opts.MaxReservations == 0 {
		opts.MaxReservations = 32
	}
	if opts.LowWatermarkBytes < 0 || opts.LowWatermarkBytes > opts.MaxBytes ||
		opts.ProtectedBytes < 0 || opts.ProtectedBytes > opts.MaxBytes ||
		opts.MaxReservations < 1 || opts.MaxReservations > 128 {
		return nil, errors.New("artwork: invalid shared store limits")
	}
	if opts.FreeBytes == nil {
		opts.FreeBytes = filesystemFreeBytes
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(opts.Namespace))
	id := hex.EncodeToString(sum[:])
	root := filepath.Join(base, "shared-v1", id)
	for _, path := range []string{root, filepath.Join(root, "blobs"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	ss := &sharedStore{db: db, base: base, dir: root, id: id, opts: opts, maintenanceWake: make(chan struct{}, 1)}
	return &Store{dir: root, maxBytes: opts.MaxBytes, shared: ss}, nil
}

func digestKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func (s *sharedStore) blobPath(digest string) string {
	return filepath.Join(s.dir, "blobs", digest[:2], digest[2:4], digest+".body")
}

func (s *sharedStore) tmpPath(id string) string { return filepath.Join(s.dir, "tmp", id+".tmp") }

func (s *sharedStore) ensure(ctx context.Context) error {
	if s.ready.Load() {
		return nil
	}
	s.initialMu.Lock()
	defer s.initialMu.Unlock()
	if s.ready.Load() {
		return nil
	}
	return s.recover(ctx)
}

func (s *sharedStore) lock(ctx context.Context, exclusive bool) (pgx.Tx, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO artwork_stores(store_id) VALUES($1) ON CONFLICT DO NOTHING`, s.id); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	suffix := " FOR SHARE"
	if exclusive {
		suffix = " FOR UPDATE"
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT store_id FROM artwork_stores WHERE store_id=$1`+suffix, s.id).Scan(&id); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// GetContext looks up a completed variant. Callers must separately prove the
// requesting user's current resource authorization before serving these bytes.
func (s *Store) GetContext(ctx context.Context, key string) (string, []byte, bool) {
	if s == nil {
		return "", nil, false
	}
	if s.shared == nil {
		return s.Get(key)
	}
	ss := s.shared
	if !digestKey(key) || ss.ensure(ctx) != nil {
		return "", nil, false
	}
	tx, err := ss.lock(ctx, false)
	if err != nil {
		return "", nil, false
	}
	defer tx.Rollback(context.Background())
	var digest, ct string
	var size int64
	var accessed time.Time
	err = tx.QueryRow(ctx, `SELECT v.blob_digest,v.content_type,b.byte_size,v.last_accessed_at
		FROM artwork_variants v JOIN artwork_blobs b ON b.store_id=v.store_id AND b.digest=v.blob_digest
		WHERE v.store_id=$1 AND v.variant_key=$2 AND v.expires_at>now()`, ss.id, key).Scan(&digest, &ct, &size, &accessed)
	if err != nil || !digestKey(digest) || size <= 0 || size > MaxBodyBytes {
		return "", nil, false
	}
	body, err := readBlob(ss.blobPath(digest))
	if err != nil || int64(len(body)) != size {
		return "", nil, false
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return "", nil, false
	}
	// At most one index write per variant per minute, regardless of hit rate.
	if time.Since(accessed) >= time.Minute {
		if _, err = tx.Exec(ctx, `UPDATE artwork_variants SET last_accessed_at=now() WHERE store_id=$1 AND variant_key=$2`, ss.id, key); err != nil {
			return "", nil, false
		}
	}
	if tx.Commit(ctx) != nil {
		return "", nil, false
	}
	return ct, body, true
}

// Reservation reserves the maximum stored transfer before its body is read.
// It owns only random temporary filenames and has a fixed, non-sliding expiry.
type Reservation struct {
	mu       sync.Mutex
	s        *sharedStore
	legacy   *Store
	id, key  string
	priority Priority
	closed   bool
}

func (s *Store) Reserve(ctx context.Context, key string, priority Priority) (*Reservation, error) {
	if s == nil || key == "" || priority < Low || priority > Protected {
		return nil, ErrReservation
	}
	if s.shared == nil {
		return &Reservation{legacy: s, key: key, priority: priority}, nil
	}
	ss := s.shared
	if !digestKey(key) {
		return nil, ErrReservation
	}
	if err := ss.ensure(ctx); err != nil {
		return nil, err
	}
	tx, err := ss.lock(ctx, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if err := ss.cleanExpired(ctx, tx); err != nil {
		return nil, err
	}
	pendingExpired, err := ss.pruneExpiredBatch(ctx, tx)
	if err != nil {
		return nil, err
	}
	ss.expiredPending.Store(pendingExpired)
	st, err := ss.stats(ctx, tx)
	if err != nil {
		return nil, err
	}
	if st.Reservations >= int64(ss.opts.MaxReservations) {
		return nil, ErrCapacity
	}
	// Protected storage is bounded; new protected assets replace older protected
	// assets only after lower priorities have been considered.
	protectedNeed := int64(0)
	if priority == Protected {
		protectedNeed = reservationBytes
	}
	if err := ss.makeRoom(ctx, tx, priority, reservationBytes, protectedNeed); err != nil {
		if errors.Is(err, ErrCapacity) {
			// Keep completed evictions even when admission still cannot fit.
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return nil, commitErr
			}
		}
		return nil, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw[:])
	_, err = tx.Exec(ctx, `INSERT INTO artwork_reservations(store_id,reservation_id,reserved_bytes,priority,expires_at)
		VALUES($1,$2,$3,$4,now()+interval '2 minutes')`, ss.id, id, reservationBytes, int(priority))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if pendingExpired {
		select {
		case ss.maintenanceWake <- struct{}{}:
		default:
		}
	}
	return &Reservation{s: ss, id: id, key: key, priority: priority}, nil
}

// Commit publishes a complete image then commits the variant reference. A
// crash between rename and SQL commit leaves an unreferenced blob for recovery.
func (r *Reservation) Commit(ctx context.Context, contentType string, body []byte) (retErr error) {
	if r == nil {
		return ErrReservation
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrReservation
	}
	defer func() {
		if retErr != nil {
			r.cancelLocked()
		}
	}()
	ct, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(strings.ToLower(ct), "image/") || len(contentType) > 255 || len(body) == 0 || len(body) > MaxBodyBytes {
		return errors.New("artwork: invalid or oversized image")
	}
	if r.legacy != nil {
		err := r.legacy.Set(r.key, contentType, body)
		r.closed = true
		return err
	}
	ss := r.s
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	// Record ownership of a possible orphan durably before publishing a file.
	// If the later reference transaction fails, cleanup cannot release these
	// reserved bytes until it has removed or verified the candidate.
	staging, err := ss.lock(ctx, true)
	if err != nil {
		return err
	}
	defer staging.Rollback(context.Background())
	var live bool
	if err := staging.QueryRow(ctx, `SELECT expires_at>now() FROM artwork_reservations WHERE store_id=$1 AND reservation_id=$2`, ss.id, r.id).Scan(&live); err != nil || !live {
		return ErrReservation
	}
	if _, err := staging.Exec(ctx, `UPDATE artwork_reservations SET candidate_digest=$3 WHERE store_id=$1 AND reservation_id=$2`, ss.id, r.id, digest); err != nil {
		return err
	}
	if err := staging.Commit(ctx); err != nil {
		return err
	}
	tx, err := ss.lock(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(ctx, `SELECT expires_at>now() AND candidate_digest=$3 FROM artwork_reservations WHERE store_id=$1 AND reservation_id=$2`, ss.id, r.id, digest).Scan(&live); err != nil || !live {
		return ErrReservation
	}
	// Expiry cleanup and admission use the same exclusive lock. The reservation
	// therefore cannot expire/reassign its capacity while this capped write is
	// in progress, even if the wall-clock deadline passes during fsync.
	free, err := ss.opts.FreeBytes(ss.base)
	if err != nil {
		return err
	}
	if free < uint64(ss.opts.MinFreeBytes+int64(len(body))) {
		return ErrCapacity
	}
	tmp := ss.tmpPath(r.id)
	if err := writeReservedTemp(tmp, body); err != nil {
		return err
	}
	defer os.Remove(tmp)
	var previous string
	if err := tx.QueryRow(ctx, `SELECT blob_digest FROM artwork_variants WHERE store_id=$1 AND variant_key=$2`, ss.id, r.key).Scan(&previous); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	dest := ss.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	// Existing content must really match its digest before deduplication.
	existing, readErr := readBlob(dest)
	existingSum := sha256.Sum256(existing)
	if readErr != nil || existingSum != sum {
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO artwork_blobs(store_id,digest,byte_size) VALUES($1,$2,$3)
		ON CONFLICT(store_id,digest) DO UPDATE SET byte_size=EXCLUDED.byte_size`, ss.id, digest, len(body))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO artwork_variants(store_id,variant_key,blob_digest,content_type,priority,expires_at)
		VALUES($1,$2,$3,$4,$5,now()+interval '7 days') ON CONFLICT(store_id,variant_key) DO UPDATE
		SET blob_digest=EXCLUDED.blob_digest,content_type=EXCLUDED.content_type,
		priority=GREATEST(artwork_variants.priority,EXCLUDED.priority),expires_at=EXCLUDED.expires_at,last_accessed_at=now()`, ss.id, r.key, digest, contentType, int(r.priority))
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM artwork_reservations WHERE store_id=$1 AND reservation_id=$2`, ss.id, r.id); err != nil {
		return err
	}
	if previous != "" && previous != digest {
		var referenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artwork_variants WHERE store_id=$1 AND blob_digest=$2)`, ss.id, previous).Scan(&referenced); err != nil {
			return err
		}
		if !referenced {
			if err := ss.removeBlob(ctx, tx, previous); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	r.closed = true
	return nil
}

func writeReservedTemp(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readBlob(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("artwork: oversized blob")
	}
	return body, nil
}

func (r *Reservation) cancelLocked() {
	if r.closed {
		return
	}
	r.closed = true
	if r.s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A failed release remains reserved until expiry and recovery, failing safe.
	tx, err := r.s.lock(ctx, true)
	if err != nil {
		return
	}
	defer tx.Rollback(context.Background())
	if err := r.s.releaseReservation(ctx, tx, r.id); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

func (s *sharedStore) releaseReservation(ctx context.Context, tx pgx.Tx, id string) error {
	if err := os.Remove(s.tmpPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	var candidate *string
	err := tx.QueryRow(ctx, `SELECT candidate_digest FROM artwork_reservations WHERE store_id=$1 AND reservation_id=$2`, s.id, id).Scan(&candidate)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if candidate != nil {
		if !digestKey(*candidate) {
			return ErrReservation
		}
		var committed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artwork_blobs WHERE store_id=$1 AND digest=$2)`, s.id, *candidate).Scan(&committed); err != nil {
			return err
		}
		if !committed {
			if err := os.Remove(s.blobPath(*candidate)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM artwork_reservations WHERE store_id=$1 AND reservation_id=$2`, s.id, id)
	return err
}

// Cancel releases unused capacity. Cleanup uses its own short deadline so a
// disconnected HTTP caller cannot strand an otherwise healthy reservation.
func (r *Reservation) Cancel(_ context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelLocked()
}

func (s *sharedStore) stats(ctx context.Context, tx pgx.Tx) (Stats, error) {
	st := Stats{Shared: true, MaxBytes: s.opts.MaxBytes, MinFreeBytes: s.opts.MinFreeBytes}
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM artwork_blobs WHERE store_id=$1),
		(SELECT COALESCE(sum(byte_size),0) FROM artwork_blobs WHERE store_id=$1),
		(SELECT count(*) FROM artwork_variants WHERE store_id=$1),
		(SELECT count(*) FROM artwork_reservations WHERE store_id=$1),
		(SELECT COALESCE(sum(reserved_bytes),0) FROM artwork_reservations WHERE store_id=$1),
		untracked_bytes FROM artwork_stores WHERE store_id=$1`, s.id).Scan(&st.Blobs, &st.PayloadBytes, &st.Variants, &st.Reservations, &st.ReservedBytes, &st.UntrackedBytes)
	if err != nil {
		return st, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(sum(byte_size+$2),0) FROM artwork_blobs b WHERE b.store_id=$1
		AND EXISTS(SELECT 1 FROM artwork_variants v WHERE v.store_id=b.store_id AND v.blob_digest=b.digest AND v.priority=3 AND v.expires_at>now())`, s.id, blobOverhead).Scan(&st.ProtectedBytes)
	if err != nil {
		return st, err
	}
	var protectedReserved int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_bytes),0) FROM artwork_reservations WHERE store_id=$1 AND priority=3`, s.id).Scan(&protectedReserved); err != nil {
		return st, err
	}
	st.ProtectedBytes += protectedReserved
	var protectedVariants int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM artwork_variants WHERE store_id=$1 AND priority=3 AND expires_at>now()`, s.id).Scan(&protectedVariants); err != nil {
		return st, err
	}
	st.ProtectedBytes += protectedVariants * variantOverhead
	st.MetadataBytes = st.Blobs*blobOverhead + st.Variants*variantOverhead
	st.TotalBytes = st.PayloadBytes + st.MetadataBytes + st.ReservedBytes + st.UntrackedBytes
	free, err := s.opts.FreeBytes(s.base)
	if err != nil {
		return st, err
	}
	st.FreeBytes = free
	st.AdmissionPaused = st.TotalBytes+reservationBytes > s.opts.MaxBytes ||
		free < uint64(s.opts.MinFreeBytes+st.ReservedBytes+reservationBytes) || st.Reservations >= int64(s.opts.MaxReservations)
	return st, nil
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	if s == nil || s.shared == nil {
		return Stats{}, errors.New("artwork: shared store disabled")
	}
	ss := s.shared
	if !ss.ready.Load() {
		return Stats{}, ErrInitializing
	}
	tx, err := ss.lock(ctx, false)
	if err != nil {
		return Stats{}, err
	}
	defer tx.Rollback(context.Background())
	st, err := ss.stats(ctx, tx)
	if err != nil {
		return st, err
	}
	return st, tx.Commit(ctx)
}

type evictionCandidate struct {
	digest   string
	size     int64
	priority Priority
}

// makeRoom evicts lower tiers first, then oldest use and larger payloads. It
// never evicts a higher tier for a lower-tier incoming image. Low watermarks
// amortize eviction batches when the high watermark is crossed.
func (s *sharedStore) makeRoom(ctx context.Context, tx pgx.Tx, priority Priority, need, protectedNeed int64) error {
	st, err := s.stats(ctx, tx)
	if err != nil {
		return err
	}
	pressure := st.TotalBytes+need > s.opts.MaxBytes
	target := s.opts.MaxBytes
	if pressure {
		target = s.opts.LowWatermarkBytes
		if target+need > s.opts.MaxBytes {
			target = s.opts.MaxBytes - need
		}
	}
	room := func(st Stats) bool {
		return st.TotalBytes+need <= s.opts.MaxBytes && (!pressure || st.TotalBytes <= target) &&
			st.FreeBytes >= uint64(s.opts.MinFreeBytes+st.ReservedBytes+need) &&
			(protectedNeed == 0 || st.ProtectedBytes+protectedNeed <= s.opts.ProtectedBytes)
	}
	if room(st) {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT b.digest,b.byte_size,COALESCE(max(v.priority),0) AS priority
		FROM artwork_blobs b LEFT JOIN artwork_variants v ON v.store_id=b.store_id AND v.blob_digest=b.digest AND v.expires_at>now()
		WHERE b.store_id=$1 GROUP BY b.digest,b.byte_size HAVING COALESCE(max(v.priority),0)<=$2
		ORDER BY priority ASC,max(v.last_accessed_at) ASC NULLS FIRST,b.byte_size DESC LIMIT 128`, s.id, int(priority))
	if err != nil {
		return err
	}
	var candidates []evictionCandidate
	for rows.Next() {
		var c evictionCandidate
		if err = rows.Scan(&c.digest, &c.size, &c.priority); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err := s.removeBlob(ctx, tx, c.digest); err != nil {
			continue
		}
		st, err = s.stats(ctx, tx)
		if err != nil {
			return err
		}
		if room(st) {
			return nil
		}
	}
	return ErrCapacity
}

func (s *sharedStore) removeBlob(ctx context.Context, tx pgx.Tx, digest string) error {
	if !digestKey(digest) {
		return errors.New("artwork: invalid blob digest")
	}
	if err := os.Remove(s.blobPath(digest)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM artwork_blobs WHERE store_id=$1 AND digest=$2`, s.id, digest)
	return err
}

func (s *sharedStore) removeUnreferenced(ctx context.Context, tx pgx.Tx) error {
	for {
		rows, err := tx.Query(ctx, `SELECT digest FROM artwork_blobs b WHERE store_id=$1
		AND NOT EXISTS(SELECT 1 FROM artwork_variants v WHERE v.store_id=b.store_id AND v.blob_digest=b.digest) LIMIT 128`, s.id)
		if err != nil {
			return err
		}
		var digests []string
		for rows.Next() {
			var digest string
			if err = rows.Scan(&digest); err != nil {
				rows.Close()
				return err
			}
			digests = append(digests, digest)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, digest := range digests {
			if err := s.removeBlob(ctx, tx, digest); err != nil {
				return err
			}
		}
		if len(digests) < 128 {
			return nil
		}
	}
}

func (s *sharedStore) cleanExpired(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT reservation_id FROM artwork_reservations WHERE store_id=$1 AND expires_at<=now()`, s.id)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if len(id) != 32 {
			return ErrReservation
		}
		if err := s.releaseReservation(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *sharedStore) pruneExpiredBatch(ctx context.Context, tx pgx.Tx) (bool, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM artwork_variants WHERE store_id=$1 AND variant_key IN
		(SELECT variant_key FROM artwork_variants WHERE store_id=$1 AND expires_at<=now() LIMIT 128)`, s.id); err != nil {
		return false, err
	}
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artwork_variants WHERE store_id=$1 AND expires_at<=now())`, s.id).Scan(&pending)
	return pending, err
}

// Recover reconciles crash remnants while holding the publication/eviction
// lock. Live reservations keep their temporary files; stale transfers expire
// after two minutes. Missing blobs retire every reference atomically.
func (s *Store) Recover(ctx context.Context) error {
	if s == nil || s.shared == nil {
		return nil
	}
	s.shared.initialMu.Lock()
	defer s.shared.initialMu.Unlock()
	return s.shared.recover(ctx)
}

// Ready lets request handlers bypass caching until background recovery has
// completed. Direct storage APIs retain synchronous initialization for tools.
func (s *Store) Ready() bool {
	return s != nil && (s.shared == nil || s.shared.ready.Load())
}

func (s *sharedStore) recover(ctx context.Context) error {
	s.ready.Store(false)
	tx, err := s.lock(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = s.cleanExpired(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM artwork_variants WHERE store_id=$1 AND expires_at<=now()`, s.id); err != nil {
		return err
	}
	if err = s.removeUnreferenced(ctx, tx); err != nil {
		return err
	}
	active := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT reservation_id,candidate_digest FROM artwork_reservations WHERE store_id=$1`, s.id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var candidate *string
		if err = rows.Scan(&id, &candidate); err != nil {
			rows.Close()
			return err
		}
		active[s.tmpPath(id)] = true
		if candidate != nil {
			active[s.blobPath(*candidate)] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var untracked int64
	err = filepath.WalkDir(s.base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("artwork: nonregular file in cache directory")
		}
		if active[path] {
			return nil
		}
		if strings.HasPrefix(path, s.dir+string(filepath.Separator)) {
			digest := strings.TrimSuffix(filepath.Base(path), ".body")
			if digestKey(digest) && path == s.blobPath(digest) {
				var size int64
				err := tx.QueryRow(ctx, `SELECT byte_size FROM artwork_blobs WHERE store_id=$1 AND digest=$2`, s.id, digest).Scan(&size)
				if err == nil && size == info.Size() {
					return nil
				}
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			if err := os.Remove(path); err == nil {
				return nil
			}
		}
		untracked += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	// Reconcile missing files in bounded metadata batches rather than loading
	// the complete library's blob index into application memory.
	cursor := ""
	for {
		rows, err := tx.Query(ctx, `SELECT digest FROM artwork_blobs WHERE store_id=$1 AND digest>$2 ORDER BY digest LIMIT 512`, s.id, cursor)
		if err != nil {
			return err
		}
		var digests []string
		for rows.Next() {
			var digest string
			if err = rows.Scan(&digest); err != nil {
				rows.Close()
				return err
			}
			digests = append(digests, digest)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, digest := range digests {
			if _, statErr := os.Stat(s.blobPath(digest)); os.IsNotExist(statErr) {
				if _, err = tx.Exec(ctx, `DELETE FROM artwork_blobs WHERE store_id=$1 AND digest=$2`, s.id, digest); err != nil {
					return err
				}
			} else if statErr != nil {
				return statErr
			}
		}
		if len(digests) < 512 {
			break
		}
		cursor = digests[len(digests)-1]
	}
	if _, err = tx.Exec(ctx, `UPDATE artwork_stores SET untracked_bytes=$2,recovered_at=now() WHERE store_id=$1`, s.id, untracked); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	s.ready.Store(true)
	return nil
}

func (s *sharedStore) sweep(ctx context.Context) (int, int64) {
	if err := s.ensure(ctx); err != nil {
		return 0, 0
	}
	tx, err := s.lock(ctx, true)
	if err != nil {
		return 0, 0
	}
	defer tx.Rollback(context.Background())
	if err := s.cleanExpired(ctx, tx); err != nil {
		return 0, 0
	}
	// Routine maintenance is bounded and does not walk the filesystem. Full
	// reconciliation occurs at startup or explicit operator recovery.
	pending, err := s.pruneExpiredBatch(ctx, tx)
	if err != nil {
		return 0, 0
	}
	if err := s.removeUnreferenced(ctx, tx); err != nil {
		return 0, 0
	}
	before, err := s.stats(ctx, tx)
	if err != nil {
		return 0, 0
	}
	if before.TotalBytes > s.opts.MaxBytes || before.FreeBytes < uint64(s.opts.MinFreeBytes+before.ReservedBytes) {
		// Under global pressure even protected objects are reclaimable.
		_ = s.makeRoom(ctx, tx, Protected, 0, 0)
	}
	after, err := s.stats(ctx, tx)
	if err != nil {
		return 0, 0
	}
	if err = tx.Commit(ctx); err != nil {
		s.expiredPending.Store(true)
		return 0, 0
	}
	s.expiredPending.Store(pending)
	return int(before.Blobs - after.Blobs), before.PayloadBytes - after.PayloadBytes
}
