package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ErrSiteWorkerNotFound is returned when token lookup finds no row.
var ErrSiteWorkerNotFound = errors.New("site worker not found")

// AmbiguousSiteWorkerError is returned by ResolveSiteWorker when an ident
// that is no row's id (a worker_id or fqdn) matches more than one row —
// e.g. the same worker_id registered under two site slugs, or two workers
// sharing an fqdn. Candidates lists every match so a caller can tell the operator
// which uuid to disambiguate with without re-querying.
type AmbiguousSiteWorkerError struct {
	Ident      string
	Candidates []model.SiteWorker
}

func (e *AmbiguousSiteWorkerError) Error() string {
	rows := make([]string, len(e.Candidates))
	for i, w := range e.Candidates {
		rows[i] = fmt.Sprintf("%s/%s (fqdn=%s, id=%s)", w.SiteSlug, w.WorkerID, w.FQDN, w.ID)
	}
	return fmt.Sprintf("%q matches %d site workers; use the uuid to disambiguate: %s",
		e.Ident, len(e.Candidates), strings.Join(rows, "; "))
}

// RegisterSiteWorker is the admin-path registration: mint (or rotate) a
// bearer token for (site_slug, worker_id). Returns the plaintext token
// exactly once — it is never persisted, only its sha256 hash. Callers
// hand the plaintext to the operator configuring the site worker.
//
// On first call for a (slug, worker_id) pair: creates the row, stores
// TokenHash, returns plaintext.
// On re-register (rotation): updates TokenHash with a fresh value,
// refreshes fqdn + last_seen_at, preserves first_seen_at.
func (s *Store) RegisterSiteWorker(siteSlug, workerID, fqdn string) (*model.SiteWorker, string, error) {
	token, hash, err := newWorkerToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	w := model.SiteWorker{
		ID:          uuid.New(),
		SiteSlug:    siteSlug,
		WorkerID:    workerID,
		FQDN:        fqdn,
		TokenHash:   hash,
		FirstSeenAt: now,
		LastSeenAt:  now,
	}
	err = s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "site_slug"}, {Name: "worker_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"last_seen_at": now,
			"fqdn":         fqdn,
			"token_hash":   hash,
		}),
	}).Create(&w).Error
	if err != nil {
		return nil, "", err
	}
	return &w, token, nil
}

// TouchSiteWorker refreshes last_seen_at for an existing (slug, worker_id)
// row, updating fqdn only when a non-empty one is supplied (heartbeats
// pass "" and must not blank the stored value). Update-only by design:
// the caller has already authenticated via GetSiteWorkerByTokenHash, so
// the row exists; the old upsert could race RegisterSiteWorker and mint
// an unusable empty-token row. A missing row (deleted mid-session)
// returns ErrSiteWorkerNotFound.
//
// serial is a pointer so "don't touch" (heartbeat, serial==nil) is
// distinct from "set to this value" (hello, serial!=nil): the hello path
// records the worker's just-verified hardware serial, which may be ""
// (an unverified worker) and MUST overwrite a previously-verified value so
// the stored serial always reflects the currently-connected worker — the
// outlet-op interlock (P4b) reads it fail-closed.
func (s *Store) TouchSiteWorker(siteSlug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	assign := map[string]any{"last_seen_at": time.Now()}
	if fqdn != "" {
		assign["fqdn"] = fqdn
	}
	if serial != nil {
		assign["serial"] = *serial
	}
	res := s.db.Model(&model.SiteWorker{}).
		Where("site_slug = ? AND worker_id = ?", siteSlug, workerID).
		Updates(assign)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrSiteWorkerNotFound
	}
	var w model.SiteWorker
	if err := s.db.Where("site_slug = ? AND worker_id = ?", siteSlug, workerID).First(&w).Error; err != nil {
		// The row can be deleted between the UPDATE and this readback;
		// map that to the same sentinel the RowsAffected==0 path uses.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSiteWorkerNotFound
		}
		return nil, err
	}
	return &w, nil
}

// GetSiteWorkerByTokenHash looks up a worker row by sha256(token). Used
// at WS upgrade time to authenticate the connection. Returns
// ErrSiteWorkerNotFound when no match — callers should treat that as a
// 401 and not leak whether the slug or token was the problem.
func (s *Store) GetSiteWorkerByTokenHash(tokenHash string) (*model.SiteWorker, error) {
	if tokenHash == "" {
		return nil, ErrSiteWorkerNotFound
	}
	var w model.SiteWorker
	err := s.db.Where("token_hash = ?", tokenHash).First(&w).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSiteWorkerNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ListSiteWorkers returns workers for a given slug (empty slug -> all).
func (s *Store) ListSiteWorkers(siteSlug string) ([]model.SiteWorker, error) {
	var out []model.SiteWorker
	q := s.db.Order("last_seen_at DESC")
	if siteSlug != "" {
		q = q.Where("site_slug = ?", siteSlug)
	}
	return out, q.Find(&out).Error
}

// ListSiteWorkersBySerial returns registered workers whose verified hardware
// serial equals the given value ignoring case (across all slugs). The P4b
// PDU-outlet POST uses it as the creation-time analog of the claim-time INV-1 check: "is this
// outlet's downstream device a serving worker?" A non-empty result means the
// downstream IS a registered worker (so `off` is refused, and a non-AllowSelf
// cycle/on needs an eligible partner). An empty serial never matches a real
// worker, so the caller must not call this with "". The column holds the
// worker's SMBIOS spelling and the caller passes NetBox's; the hello gate
// matched those case-insensitively (model.SameHardwareSerial), so this
// lookup does too.
func (s *Store) ListSiteWorkersBySerial(serial string) ([]model.SiteWorker, error) {
	if serial == "" {
		return nil, nil
	}
	var out []model.SiteWorker
	return out, s.db.Where("LOWER(serial) = LOWER(?)", serial).Find(&out).Error
}

// ResolveSiteWorker resolves ident to exactly one site worker row.
// ident may be the row's uuid (id), or its worker_id, or its fqdn.
// Returns ErrSiteWorkerNotFound if nothing matches, *AmbiguousSiteWorkerError
// if more than one row matches (e.g. the same worker_id registered under
// two different site slugs, or two different workers sharing an fqdn).
//
// An ident that parses as a uuid is tried as an id first, so the id wins
// when it is one row's id and another row's worker_id: the uuid is what
// slagctl tells an operator to use to disambiguate. When no row has that
// id, the ident is tried as a worker_id or fqdn, since slagworksd generates
// a uuid for its worker_id unless SLAGWORKS_WORKER_ID overrides it.
func (s *Store) ResolveSiteWorker(ident string) (*model.SiteWorker, error) {
	if id, err := uuid.Parse(ident); err == nil {
		var w model.SiteWorker
		err := s.db.Where("id = ?", id).First(&w).Error
		if err == nil {
			return &w, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	var matches []model.SiteWorker
	if err := s.db.Where("worker_id = ? OR fqdn = ?", ident, ident).Find(&matches).Error; err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, ErrSiteWorkerNotFound
	case 1:
		return &matches[0], nil
	default:
		return nil, &AmbiguousSiteWorkerError{Ident: ident, Candidates: matches}
	}
}

// DeleteSiteWorker deletes the site worker row by id. Callers resolve an
// ident to an id via ResolveSiteWorker first; delete is by-id only so a
// caller that already resolved once (e.g. after the connected/in-flight-op
// checks) can't have the identifier re-resolve to a different row.
func (s *Store) DeleteSiteWorker(id uuid.UUID) error {
	res := s.db.Delete(&model.SiteWorker{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrSiteWorkerNotFound
	}
	return nil
}

// HashWorkerToken returns the canonical lowercase-hex sha256 of a
// plaintext token. Callers that need to compare an incoming ?token=
// query-string value against stored hashes use this.
func HashWorkerToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// newWorkerToken generates 32 bytes of crypto/rand entropy, returning
// (plaintext-hex, sha256(plaintext-hex) hex). The hex-encoded plaintext
// is what operators paste into site worker config; the hash is what
// the DB stores.
func newWorkerToken() (plaintext, hash string, err error) {
	var buf [32]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", "", fmt.Errorf("generating worker token: %w", err)
	}
	plaintext = hex.EncodeToString(buf[:])
	hash = HashWorkerToken(plaintext)
	return plaintext, hash, nil
}
