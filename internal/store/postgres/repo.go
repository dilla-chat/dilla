package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/postgres/pgdb"
)

// Repo adapts the generated pgdb.Querier to store.Repository. Postgres needs no
// writer/reader split — MVCC readers never block the writer — so r.w and r.r are
// bound to the same pool; inside a Tx both point at the transaction, so a
// read-your-writes sequence sees its own uncommitted rows.
type Repo struct {
	db      *sql.DB
	tx      *sql.Tx
	w       *pgdb.Queries // bound to db (or to the tx)
	r       *pgdb.Queries // bound to db (or to the tx)
	inTx    bool
	rawRead *sql.DB
}

// New returns the Postgres repository over an already-migrated database.
func New(db *sql.DB) store.Repository {
	q := pgdb.New(db)
	return &Repo{db: db, w: q, r: q, rawRead: db}
}

func (r *Repo) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Tx runs fn inside one transaction.
func (r *Repo) Tx(ctx context.Context, fn func(store.Repository) error) error {
	if r.inTx {
		return errors.New("store: nested transaction")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	// A panic inside fn must not abandon the transaction: the connection it
	// holds is never returned to the pool and the transaction stays open on the
	// server, keeping its locks and its snapshot. database/sql's awaitDone
	// goroutine rescues only a caller that passed a cancellable context.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	q := pgdb.New(tx)
	sub := &Repo{db: r.db, tx: tx, w: q, r: q, inTx: true, rawRead: r.rawRead}
	if err := fn(sub); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// wrap turns driver errors into the package's three sentinels. Postgres spells
// a unique violation as SQLSTATE 23505 on a *pgconn.PgError, not as a message
// substring.
func wrap(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return store.ErrNotFound
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
		return err
	}
}

// nullInt64 and ptrInt64 convert between the store's nullable timestamps and
// the generated code's sql.NullInt64.
func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func ptrInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// checkedInt64 rejects values that the signed database integer columns cannot represent.
func checkedInt64(value uint64, field string) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("store: %s exceeds int64 range", field)
	}
	return int64(value), nil
}

// ---------------------------------------------------------------- Instance

func (r *Repo) CreateInstance(ctx context.Context, in store.InstanceRow) error {
	return wrap(r.w.CreateInstance(ctx, pgdb.CreateInstanceParams{
		InstanceID:          in.InstanceID,
		ExternalSenderKeyID: in.ExternalSenderKeyID,
		KeyHistory:          in.KeyHistory,
		FrankingKeyID:       in.FrankingKeyID,
		Generation:          int64(in.Generation),
		PolicyVersion:       int64(in.PolicyVersion),
		Created:             in.Created,
	}))
}

func (r *Repo) GetInstance(ctx context.Context) (store.InstanceRow, error) {
	row, err := r.r.GetInstance(ctx)
	if err != nil {
		return store.InstanceRow{}, wrap(err)
	}
	return store.InstanceRow{
		InstanceID:          row.InstanceID,
		ExternalSenderKeyID: row.ExternalSenderKeyID,
		KeyHistory:          row.KeyHistory,
		FrankingKeyID:       row.FrankingKeyID,
		Generation:          uint64(row.Generation),
		PolicyVersion:       uint64(row.PolicyVersion),
		Created:             row.Created,
	}, nil
}

// LockInstanceGeneration takes the shared publication/restore fence. It must be the first
// database row lock in both managed publication and restore transactions.
func (r *Repo) LockInstanceGeneration(ctx context.Context) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	var generation int64
	if err := r.tx.QueryRowContext(ctx, `SELECT generation FROM instances LIMIT 1 FOR UPDATE`).Scan(&generation); err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) BumpGeneration(ctx context.Context) (uint64, error) {
	gen, err := r.w.BumpGeneration(ctx)
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(gen), nil
}

// SetGeneration is monotone in SQL (`GREATEST(generation, $1)`), so a manifest
// older than the instance's own generation is a no-op rather than a step
// backwards.
func (r *Repo) SetGeneration(ctx context.Context, generation uint64) error {
	generationInt, err := checkedInt64(generation, "generation")
	if err != nil {
		return err
	}
	return wrap(r.w.SetGeneration(ctx, pgdb.SetGenerationParams{
		Generation: generationInt,
	}))
}

func (r *Repo) GetSetting(ctx context.Context, key string) ([]byte, error) {
	v, err := r.r.GetSetting(ctx, pgdb.GetSettingParams{Key: key})
	return v, wrap(err)
}

func (r *Repo) PutSetting(ctx context.Context, key string, value []byte, updated int64) error {
	return wrap(r.w.PutSetting(ctx, pgdb.PutSettingParams{Key: key, Value: value, Updated: updated}))
}

// ---------------------------------------------------------------- Accounts

func (r *Repo) CreateUser(ctx context.Context, u store.UserRow) error {
	return wrap(r.w.CreateUser(ctx, pgdb.CreateUserParams{
		ID:         u.ID,
		Username:   u.Username,
		Display:    u.Display,
		Kind:       int64(u.Kind),
		UmkPub:     u.UMKPub,
		SskPub:     u.SSKPub,
		SigUmkSsk:  u.SigUMKSSK,
		Flags:      int64(u.Flags),
		AgeBracket: int64(u.AgeBracket),
		Created:    u.Created,
		DisabledAt: nullInt64(u.DisabledAt),
		DeletedAt:  nullInt64(u.DeletedAt),
		FactorOwed: boolInt(u.FactorOwed),
	}))
}

func (r *Repo) GetUser(ctx context.Context, userID id.ID) (store.UserRow, error) {
	row, err := r.r.GetUser(ctx, pgdb.GetUserParams{ID: userID})
	if err != nil {
		return store.UserRow{}, wrap(err)
	}
	return userRow(row), nil
}

func (r *Repo) GetPresenceChoice(ctx context.Context, userID id.ID) (uint8, error) {
	var choice uint8
	var err error
	if r.tx != nil {
		err = r.tx.QueryRowContext(ctx, "SELECT choice FROM user_presence WHERE user_id = $1", userID).Scan(&choice)
	} else {
		err = r.db.QueryRowContext(ctx, "SELECT choice FROM user_presence WHERE user_id = $1", userID).Scan(&choice)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	return choice, wrap(err)
}

func (r *Repo) ListPresenceChoices(ctx context.Context, users []id.ID) (map[id.ID]uint8, error) {
	choices := make(map[id.ID]uint8, len(users))
	if len(users) == 0 {
		return choices, nil
	}
	args := make([]any, len(users))
	var placeholders strings.Builder
	for i, user := range users {
		if i > 0 {
			placeholders.WriteByte(',')
		}
		fmt.Fprintf(&placeholders, "$%d", i+1)
		args[i] = user
	}
	query := "SELECT user_id, choice FROM user_presence WHERE user_id IN (" + placeholders.String() + ")" //nolint:gosec // G202: placeholders contain only generated parameter numbers
	var rows *sql.Rows
	var err error
	if r.tx != nil {
		rows, err = r.tx.QueryContext(ctx, query, args...)
	} else {
		rows, err = r.db.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var user id.ID
		var choice uint8
		if err := rows.Scan(&user, &choice); err != nil {
			return nil, wrap(err)
		}
		choices[user] = choice
	}
	return choices, wrap(rows.Err())
}

func (r *Repo) SetPresenceChoice(ctx context.Context, userID id.ID, choice uint8) error {
	if choice < 1 || choice > 4 {
		return fmt.Errorf("invalid presence choice %d", choice)
	}
	var err error
	if r.tx != nil {
		_, err = r.tx.ExecContext(ctx, "INSERT INTO user_presence(user_id, choice) VALUES ($1, $2) ON CONFLICT(user_id) DO UPDATE SET choice = excluded.choice", userID, choice)
	} else {
		_, err = r.db.ExecContext(ctx, "INSERT INTO user_presence(user_id, choice) VALUES ($1, $2) ON CONFLICT(user_id) DO UPDATE SET choice = excluded.choice", userID, choice)
	}
	return wrap(err)
}

func (r *Repo) GetUserByUsername(ctx context.Context, username string) (store.UserRow, error) {
	row, err := r.r.GetUserByUsername(ctx, pgdb.GetUserByUsernameParams{Username: username})
	if err != nil {
		return store.UserRow{}, wrap(err)
	}
	return userRow(row), nil
}

func (r *Repo) ListUsers(ctx context.Context, after id.ID, limit int32) ([]store.UserRow, error) {
	rows, err := r.r.ListUsers(ctx, pgdb.ListUsersParams{ID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.UserRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, userRow(row))
	}
	return out, nil
}

func (r *Repo) SetUserDisabled(ctx context.Context, userID id.ID, at *int64) error {
	if r.inTx {
		if err := r.LockUserForDeviceRegistration(ctx, userID); err != nil {
			return err
		}
	}
	return wrap(r.w.SetUserDisabled(ctx, pgdb.SetUserDisabledParams{
		DisabledAt: nullInt64(at),
		ID:         userID,
	}))
}

func (r *Repo) TombstoneUser(ctx context.Context, userID id.ID, at int64) error {
	if r.inTx {
		if err := r.LockUserForDeviceRegistration(ctx, userID); err != nil {
			return err
		}
	}
	return wrap(r.w.TombstoneUser(ctx, pgdb.TombstoneUserParams{
		DeletedAt: nullInt64(&at),
		ID:        userID,
	}))
}

// UpdateProfile is ErrNotFound unless the UPDATE matched a live user's row.
func (r *Repo) UpdateProfile(ctx context.Context, userID id.ID, display, statusMsg, bio *string) error {
	n, err := r.w.UpdateProfile(ctx, pgdb.UpdateProfileParams{Display: nullString(display), StatusMsg: nullString(statusMsg), Bio: nullString(bio), ID: userID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

// SetFactorOwed is ErrNotFound unless the UPDATE matched the user's row.
func (r *Repo) SetFactorOwed(ctx context.Context, userID id.ID, owed bool) error {
	n, err := r.w.SetFactorOwed(ctx, pgdb.SetFactorOwedParams{FactorOwed: boolInt(owed), ID: userID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) MarkFactorOwedWithoutFactor(ctx context.Context, rpID string) (int64, error) {
	n, err := r.w.MarkFactorOwedWithoutFactor(ctx, pgdb.MarkFactorOwedWithoutFactorParams{RpID: rpID})
	return n, wrap(err)
}

// ClearFactorOwedWithFactor locks the owed accounts' rows before the UPDATE, in one transaction.
// Under READ COMMITTED the UPDATE's subqueries read the factor tables at its own snapshot and are
// not re-read when it waits on a row: without the lock, an operator reset that commits between
// that snapshot and the row lock would have its just-deleted factors count, and the mark it set
// would be cleared. The reset takes the same row lock first (LockUserForDeviceRegistration), so
// with it a reset is wholly before the UPDATE's snapshot or waits for this commit.
func (r *Repo) ClearFactorOwedWithFactor(ctx context.Context, rpID string) (int64, error) {
	var n int64
	settle := func(q *Repo) error {
		if _, err := q.w.LockOwedHumans(ctx); err != nil {
			return wrap(err)
		}
		var err error
		n, err = q.w.ClearFactorOwedWithFactor(ctx, pgdb.ClearFactorOwedWithFactorParams{RpID: rpID})
		return wrap(err)
	}
	if r.inTx {
		return n, settle(r)
	}
	err := r.Tx(ctx, func(tx store.Repository) error {
		q, ok := tx.(*Repo)
		if !ok {
			return errors.New("store: ClearFactorOwedWithFactor: the transaction is not a postgres Repo")
		}
		return settle(q)
	})
	return n, err
}

// boolInt is a 0/1 flag column's value.
// nullString is a nullable text argument: nil is SQL NULL.
func nullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func userRow(row pgdb.Users) store.UserRow {
	return store.UserRow{
		ID:         row.ID,
		Username:   row.Username,
		Display:    row.Display,
		Kind:       uint8(row.Kind),
		UMKPub:     row.UmkPub,
		SSKPub:     row.SskPub,
		SigUMKSSK:  row.SigUmkSsk,
		Flags:      uint64(row.Flags),
		AgeBracket: uint64(row.AgeBracket),
		Created:    row.Created,
		DisabledAt: ptrInt64(row.DisabledAt),
		DeletedAt:  ptrInt64(row.DeletedAt),
		FactorOwed: row.FactorOwed == 1,
		StatusMsg:  row.StatusMsg,
		Bio:        row.Bio,
	}
}

// ---------------------------------------------------------------- Devices

func (r *Repo) CreateDevice(ctx context.Context, d store.DeviceRow) error {
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error { return tx.CreateDevice(ctx, d) })
	}
	return r.createDeviceLocked(ctx, d, false)
}

func (r *Repo) CreateBotDevice(ctx context.Context, d store.DeviceRow) error {
	if !r.inTx {
		return errors.New("store: CreateBotDevice requires a transaction")
	}
	return r.createDeviceLocked(ctx, d, true)
}

func (r *Repo) createDeviceLocked(ctx context.Context, d store.DeviceRow, newIsBot bool) error {
	if !r.inTx {
		return errors.New("store: device DSK locking requires a transaction")
	}
	sum := sha256.Sum256(d.DSKPub)
	if err := r.w.LockDeviceDSK(ctx, pgdb.LockDeviceDSKParams{LockKey: int64(binary.BigEndian.Uint64(sum[:8]))}); err != nil {
		return wrap(err)
	}
	kind, err := r.r.GetUserKind(ctx, pgdb.GetUserKindParams{ID: d.UserID})
	if err != nil {
		return wrap(err)
	}
	if newIsBot && kind != 1 {
		return errors.New("store: CreateBotDevice requires a Bot account")
	}
	newIsBot = newIsBot || kind == 1
	count, err := r.r.DeviceDSKConflictCount(ctx, pgdb.DeviceDSKConflictCountParams{DskPub: d.DSKPub, NewIsBot: newIsBot})
	if err != nil {
		return wrap(err)
	}
	if count != 0 {
		return store.ErrConflict
	}
	return wrap(r.w.CreateDevice(ctx, pgdb.CreateDeviceParams{
		ID:               d.ID,
		UserID:           d.UserID,
		DskPub:           d.DSKPub,
		Tier:             int64(d.Tier),
		SignerTier:       int64(d.SignerTier),
		CredentialBlob:   d.CredentialBlob,
		VerifiedAt:       nullInt64(d.VerifiedAt),
		RevokedAt:        nullInt64(d.RevokedAt),
		QuarantinedAt:    nullInt64(d.QuarantinedAt),
		QuarantineReason: d.QuarantineReason,
		LastSeen:         d.LastSeen,
		Created:          d.Created,
	}))
}

func (r *Repo) GetDevice(ctx context.Context, deviceID id.ID) (store.DeviceRow, error) {
	row, err := r.r.GetDevice(ctx, pgdb.GetDeviceParams{ID: deviceID})
	if err != nil {
		return store.DeviceRow{}, wrap(err)
	}
	return deviceRow(row), nil
}

func (r *Repo) ListDevicesByUser(ctx context.Context, userID id.ID) ([]store.DeviceRow, error) {
	rows, err := r.r.ListDevicesByUser(ctx, pgdb.ListDevicesByUserParams{UserID: userID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.DeviceRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, deviceRow(row))
	}
	return out, nil
}

// SetDeviceLabel names (or, with nil, unnames) a device of userID. It is ErrNotFound unless the
// device exists and is the user's: the owner is part of the UPDATE's WHERE, not a second read.
func (r *Repo) SetDeviceLabel(ctx context.Context, userID, deviceID id.ID, label *string) error {
	var v sql.NullString
	if label != nil {
		v = sql.NullString{String: *label, Valid: true}
	}
	n, err := r.w.SetDeviceLabel(ctx, pgdb.SetDeviceLabelParams{Label: v, ID: deviceID, UserID: userID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) TouchDevice(ctx context.Context, deviceID id.ID, lastSeen int64) error {
	return wrap(r.w.TouchDevice(ctx, pgdb.TouchDeviceParams{LastSeen: lastSeen, ID: deviceID}))
}

func (r *Repo) RevokeDevice(ctx context.Context, deviceID id.ID, at int64) error {
	if r.inTx {
		if err := r.LockKeyPackageDevice(ctx, deviceID); err != nil {
			return err
		}
	}
	return wrap(r.w.RevokeDevice(ctx, pgdb.RevokeDeviceParams{
		RevokedAt: nullInt64(&at),
		ID:        deviceID,
	}))
}

func (r *Repo) PutDeviceList(ctx context.Context, l store.DeviceListRow) error {
	return wrap(r.w.PutDeviceList(ctx, pgdb.PutDeviceListParams{
		UserID:       l.UserID,
		Version:      int64(l.Version),
		Blob:         l.Blob,
		SskSignature: l.SSKSignature,
		PrevHash:     l.PrevHash,
		Created:      l.Created,
	}))
}

func (r *Repo) GetDeviceList(ctx context.Context, userID id.ID) (store.DeviceListRow, error) {
	row, err := r.r.GetDeviceList(ctx, pgdb.GetDeviceListParams{UserID: userID})
	if err != nil {
		return store.DeviceListRow{}, wrap(err)
	}
	return store.DeviceListRow{
		UserID:       row.UserID,
		Version:      uint64(row.Version),
		Blob:         row.Blob,
		SSKSignature: row.SskSignature,
		PrevHash:     row.PrevHash,
		Created:      row.Created,
	}, nil
}

func (r *Repo) ListDeviceListsAfter(ctx context.Context, userID id.ID, after uint64, limit int32) ([]store.DeviceListRow, error) {
	if limit <= 0 || after > math.MaxInt64 {
		return []store.DeviceListRow{}, nil
	}
	rows, err := r.r.ListDeviceListsAfter(ctx, pgdb.ListDeviceListsAfterParams{UserID: userID, After: int64(after), MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.DeviceListRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.DeviceListRow{UserID: row.UserID, Version: uint64(row.Version), Blob: row.Blob, SSKSignature: row.SskSignature, PrevHash: row.PrevHash, Created: row.Created})
	}
	return out, nil
}

func (r *Repo) CountLiveDevicesByUser(ctx context.Context, userID id.ID) (int64, error) {
	n, err := r.r.CountLiveDevicesByUser(ctx, pgdb.CountLiveDevicesByUserParams{UserID: userID})
	return n, wrap(err)
}

func (r *Repo) ListDeviceCreationsSince(ctx context.Context, userID id.ID, since int64) ([]int64, error) {
	rows, err := r.r.ListDeviceCreationsSince(ctx, pgdb.ListDeviceCreationsSinceParams{UserID: userID, Since: since})
	if err != nil {
		return nil, wrap(err)
	}
	return rows, nil
}

func (r *Repo) LockUserForDeviceRegistration(ctx context.Context, userID id.ID) error {
	if !r.inTx {
		return errors.New("store: device registration lock requires Tx")
	}
	_, err := r.w.LockUserForDeviceRegistration(ctx, pgdb.LockUserForDeviceRegistrationParams{ID: userID})
	return wrap(err)
}

func (r *Repo) CountLiveUnlistedDevicesByUser(ctx context.Context, userID id.ID, listed []id.ID, unlistedCutoff int64) (int64, error) {
	rows, err := r.liveUnlistedDevices(ctx, userID, listed, unlistedCutoff)
	if err != nil {
		return 0, err
	}
	return int64(len(rows)), nil
}

func (r *Repo) ListLiveDeviceCreationsSince(ctx context.Context, userID id.ID, listed []id.ID, since, unlistedCutoff int64) ([]int64, error) {
	rows, err := r.ListDevicesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.RevokedAt == nil && row.Created >= since && (slices.Contains(listed, row.ID) || row.Created >= unlistedCutoff) {
			out = append(out, row.Created)
		}
	}
	return out, nil
}

func (r *Repo) liveUnlistedDevices(ctx context.Context, userID id.ID, listed []id.ID, unlistedCutoff int64) ([]store.DeviceRow, error) {
	rows, err := r.ListDevicesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]store.DeviceRow, 0, len(rows))
	for _, row := range rows {
		if row.RevokedAt == nil && row.Created >= unlistedCutoff && !slices.Contains(listed, row.ID) {
			out = append(out, row)
		}
	}
	return out, nil
}

func deviceRow(row pgdb.Devices) store.DeviceRow {
	return store.DeviceRow{
		ID:               row.ID,
		UserID:           row.UserID,
		DSKPub:           row.DskPub,
		Tier:             uint8(row.Tier),
		SignerTier:       uint8(row.SignerTier),
		CredentialBlob:   row.CredentialBlob,
		VerifiedAt:       ptrInt64(row.VerifiedAt),
		RevokedAt:        ptrInt64(row.RevokedAt),
		QuarantinedAt:    ptrInt64(row.QuarantinedAt),
		QuarantineReason: row.QuarantineReason,
		LastSeen:         row.LastSeen,
		Created:          row.Created,
		Label:            ptrString(row.Label),
	}
}

// ptrString is a nullable TEXT column as a pointer.
func ptrString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// ---------------------------------------------------------------- Sessions

func (r *Repo) CreateSession(ctx context.Context, s store.SessionRow) error {
	return wrap(r.w.CreateSession(ctx, pgdb.CreateSessionParams{
		TokenHash:   s.TokenHash,
		DeviceID:    s.DeviceID,
		UserID:      s.UserID,
		Scope:       int64(s.Scope),
		Tier:        int64(s.Tier),
		Created:     s.Created,
		Expires:     s.Expires,
		IdleExpires: s.IdleExpires,
	}))
}

func (r *Repo) GetSessionByHash(ctx context.Context, tokenHash []byte, now int64) (store.SessionRow, error) {
	row, err := r.r.GetSessionByHash(ctx, pgdb.GetSessionByHashParams{
		TokenHash:   tokenHash,
		Expires:     now,
		IdleExpires: now,
	})
	if err != nil {
		return store.SessionRow{}, wrap(err)
	}
	return store.SessionRow{
		TokenHash:    row.TokenHash,
		DeviceID:     row.DeviceID,
		UserID:       row.UserID,
		Scope:        uint8(row.Scope),
		Tier:         uint8(row.Tier),
		Created:      row.Created,
		Expires:      row.Expires,
		IdleExpires:  row.IdleExpires,
		WireVersion:  uint64(row.WireVersion),
		E2EEVersion:  uint64(row.E2eeVersion),
		MediaVersion: uint64(row.MediaVersion),
		FactorOwed:   row.FactorOwed == 1,
	}, nil
}

func (r *Repo) SetSessionVersions(ctx context.Context, tokenHash []byte, wire, e2ee, media uint64) error {
	n, err := r.w.SetSessionVersions(ctx, pgdb.SetSessionVersionsParams{
		WireVersion: int64(wire), E2eeVersion: int64(e2ee), MediaVersion: int64(media), TokenHash: tokenHash,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) TouchSession(ctx context.Context, tokenHash []byte, idleExpires int64) error {
	return wrap(r.w.TouchSession(ctx, pgdb.TouchSessionParams{
		IdleExpires: idleExpires,
		TokenHash:   tokenHash,
	}))
}

func (r *Repo) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return wrap(r.w.DeleteSession(ctx, pgdb.DeleteSessionParams{TokenHash: tokenHash}))
}

func (r *Repo) DeleteSessionsByDevice(ctx context.Context, deviceID id.ID) (int64, error) {
	n, err := r.w.DeleteSessionsByDevice(ctx, pgdb.DeleteSessionsByDeviceParams{DeviceID: deviceID})
	return n, wrap(err)
}

func (r *Repo) DeleteSessionsByUser(ctx context.Context, userID id.ID) (int64, error) {
	n, err := r.w.DeleteSessionsByUser(ctx, pgdb.DeleteSessionsByUserParams{UserID: userID})
	return n, wrap(err)
}

func (r *Repo) CountSessionsByDevice(ctx context.Context, deviceID id.ID) (int64, error) {
	n, err := r.r.CountSessionsByDevice(ctx, pgdb.CountSessionsByDeviceParams{DeviceID: deviceID})
	return n, wrap(err)
}

func (r *Repo) DeleteOldestSessionForDevice(ctx context.Context, deviceID id.ID) error {
	return wrap(r.w.DeleteOldestSessionForDevice(ctx, pgdb.DeleteOldestSessionForDeviceParams{
		DeviceID: deviceID,
	}))
}

func (r *Repo) PruneSessions(ctx context.Context, now int64) (int64, error) {
	n, err := r.w.PruneSessions(ctx, pgdb.PruneSessionsParams{Expires: now, IdleExpires: now})
	return n, wrap(err)
}

// ---------------------------------------------------------------- Auth

func (r *Repo) PutPasswordCredential(ctx context.Context, userID id.ID, phc string, updated int64) error {
	return wrap(r.w.PutPasswordCredential(ctx, pgdb.PutPasswordCredentialParams{
		UserID:  userID,
		Phc:     phc,
		Updated: updated,
	}))
}

func (r *Repo) ChangePasswordCredential(ctx context.Context, userID id.ID, phc string, at int64) error {
	return wrap(r.w.ChangePasswordCredential(ctx, pgdb.ChangePasswordCredentialParams{
		UserID:    userID,
		Phc:       phc,
		Updated:   at,
		ChangedAt: sql.NullInt64{Int64: at, Valid: true},
	}))
}

func (r *Repo) GetPasswordChangedAt(ctx context.Context, userID id.ID) (*int64, error) {
	at, err := r.r.GetPasswordChangedAt(ctx, pgdb.GetPasswordChangedAtParams{UserID: userID})
	if err != nil {
		return nil, wrap(err)
	}
	if !at.Valid {
		return nil, nil
	}
	return &at.Int64, nil
}

func (r *Repo) GetPasswordCredential(ctx context.Context, userID id.ID) (string, error) {
	phc, err := r.r.GetPasswordCredential(ctx, pgdb.GetPasswordCredentialParams{UserID: userID})
	return phc, wrap(err)
}

func (r *Repo) PutTOTP(ctx context.Context, t store.TOTPRow) error {
	return wrap(r.w.PutTOTP(ctx, pgdb.PutTOTPParams{
		UserID:      t.UserID,
		Secret:      t.Secret,
		Digits:      int64(t.Digits),
		Period:      int64(t.Period),
		Algorithm:   t.Algorithm,
		ConfirmedAt: nullInt64(t.ConfirmedAt),
		LastCounter: t.LastCounter,
		Created:     t.Created,
	}))
}

func (r *Repo) GetTOTP(ctx context.Context, userID id.ID) (store.TOTPRow, error) {
	row, err := r.r.GetTOTP(ctx, pgdb.GetTOTPParams{UserID: userID})
	if err != nil {
		return store.TOTPRow{}, wrap(err)
	}
	return store.TOTPRow{
		UserID:      row.UserID,
		Secret:      row.Secret,
		Digits:      uint64(row.Digits),
		Period:      uint64(row.Period),
		Algorithm:   row.Algorithm,
		ConfirmedAt: ptrInt64(row.ConfirmedAt),
		LastCounter: row.LastCounter,
		Created:     row.Created,
	}, nil
}

// ConsumeTOTPCounter is the replay guard: the UPDATE only fires while the
// presented counter is strictly greater than the stored one, so a code accepted
// once cannot be presented again.
func (r *Repo) ConsumeTOTPCounter(ctx context.Context, userID id.ID, counter int64) error {
	n, err := r.w.ConsumeTOTPCounter(ctx, pgdb.ConsumeTOTPCounterParams{
		LastCounter:   counter,
		UserID:        userID,
		LastCounter_2: counter,
	})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrConflict
	}
	return nil
}

// PutRecoveryCodes replaces the whole set in one transaction: regenerating
// invalidates every unconsumed code (facts-auth §2.5).
//
// Every multi-statement method is split in two: an inner form that assumes an
// open transaction, and an exported form that opens one only when it is not
// already inside one. Without the split, a caller that legitimately needs
// atomicity across two of these — task 9's TOTP-confirm writes the TOTP row and
// its recovery codes together — gets `store: nested transaction` from Tx.
func (r *Repo) PutRecoveryCodes(ctx context.Context, userID id.ID, hashes [][]byte, created int64) error {
	if r.inTx {
		return r.putRecoveryCodesTx(ctx, r.w, userID, hashes, created)
	}
	return r.Tx(ctx, func(tx store.Repository) error {
		sub, ok := tx.(*Repo)
		if !ok {
			return fmt.Errorf("store: Tx handed a %T, not this adapter's *Repo", tx)
		}
		return sub.putRecoveryCodesTx(ctx, sub.w, userID, hashes, created)
	})
}

func (r *Repo) putRecoveryCodesTx(ctx context.Context, q *pgdb.Queries, userID id.ID, hashes [][]byte, created int64) error {
	if err := q.DeleteRecoveryCodes(ctx, pgdb.DeleteRecoveryCodesParams{UserID: userID}); err != nil {
		return wrap(err)
	}
	for _, h := range hashes {
		if err := q.PutRecoveryCode(ctx, pgdb.PutRecoveryCodeParams{
			UserID: userID, CodeHash: h, Created: created,
		}); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// ConsumeRecoveryCode marks one unused code used. A code that does not exist and
// a code already spent are the same answer to the caller: ErrNotFound.
func (r *Repo) ConsumeRecoveryCode(ctx context.Context, userID id.ID, hash []byte, at int64) error {
	n, err := r.w.ConsumeRecoveryCode(ctx, pgdb.ConsumeRecoveryCodeParams{
		UsedAt:   nullInt64(&at),
		UserID:   userID,
		CodeHash: hash,
	})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) DeleteRecoveryCodes(ctx context.Context, userID id.ID) error {
	return wrap(r.w.DeleteRecoveryCodes(ctx, pgdb.DeleteRecoveryCodesParams{UserID: userID}))
}

func (r *Repo) DeleteTOTP(ctx context.Context, userID id.ID) error {
	return wrap(r.w.DeleteTOTP(ctx, pgdb.DeleteTOTPParams{UserID: userID}))
}

func (r *Repo) DeleteWebauthnCredential(ctx context.Context, userID id.ID, credID []byte) (int64, error) {
	n, err := r.w.DeleteWebauthnCredential(ctx, pgdb.DeleteWebauthnCredentialParams{UserID: userID, CredID: credID})
	return n, wrap(err)
}

func (r *Repo) DeleteWebauthnCredentialsByUser(ctx context.Context, userID id.ID) error {
	return wrap(r.w.DeleteWebauthnCredentialsByUser(ctx, pgdb.DeleteWebauthnCredentialsByUserParams{UserID: userID}))
}

func (r *Repo) CountRecoveryCodes(ctx context.Context, userID id.ID) (int64, error) {
	n, err := r.r.CountRecoveryCodes(ctx, pgdb.CountRecoveryCodesParams{UserID: userID})
	return n, wrap(err)
}

func (r *Repo) PutWebauthnUser(ctx context.Context, userID id.ID, rpID string, handle []byte, created int64) error {
	return wrap(r.w.PutWebauthnUser(ctx, pgdb.PutWebauthnUserParams{
		RpID:       rpID,
		UserID:     userID,
		UserHandle: handle,
		Created:    created,
	}))
}

func (r *Repo) GetWebauthnUserHandle(ctx context.Context, rpID string, userID id.ID) ([]byte, error) {
	h, err := r.r.GetWebauthnUserHandle(ctx, pgdb.GetWebauthnUserHandleParams{
		RpID:   rpID,
		UserID: userID,
	})
	return h, wrap(err)
}

func (r *Repo) GetWebauthnUserByHandle(ctx context.Context, rpID string, handle []byte) (id.ID, error) {
	userID, err := r.r.GetWebauthnUserByHandle(ctx, pgdb.GetWebauthnUserByHandleParams{
		RpID:       rpID,
		UserHandle: handle,
	})
	if err != nil {
		return id.Zero, wrap(err)
	}
	return userID, nil
}

func (r *Repo) PutWebauthnCredential(ctx context.Context, c store.WebauthnCredentialRow) error {
	return wrap(r.w.PutWebauthnCredential(ctx, pgdb.PutWebauthnCredentialParams{
		CredID:            c.CredID,
		RpID:              c.RPID,
		UserID:            c.UserID,
		PublicKey:         c.PublicKey,
		SignCount:         c.SignCount,
		AttestationType:   c.AttestationType,
		AttestationFormat: c.AttestationFormat,
		Transports:        c.Transports,
		Flags:             c.Flags,
		ExtensionsJson:    c.ExtensionsJSON,
		Name:              c.Name,
		Created:           c.Created,
		LastUsed:          nullInt64(c.LastUsed),
	}))
}

func (r *Repo) ListWebauthnCredentials(ctx context.Context, userID id.ID, rpID string) ([]store.WebauthnCredentialRow, error) {
	rows, err := r.r.ListWebauthnCredentials(ctx, pgdb.ListWebauthnCredentialsParams{
		RpID:   rpID,
		UserID: userID,
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.WebauthnCredentialRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, webauthnCredentialRow(row))
	}
	return out, nil
}

func (r *Repo) GetWebauthnCredential(ctx context.Context, credID []byte) (store.WebauthnCredentialRow, error) {
	row, err := r.r.GetWebauthnCredential(ctx, pgdb.GetWebauthnCredentialParams{CredID: credID})
	if err != nil {
		return store.WebauthnCredentialRow{}, wrap(err)
	}
	return webauthnCredentialRow(row), nil
}

func (r *Repo) UpdateWebauthnCredential(ctx context.Context, credID []byte, signCount int64, flags []byte, lastUsed int64) error {
	return wrap(r.w.UpdateWebauthnCredential(ctx, pgdb.UpdateWebauthnCredentialParams{
		SignCount: signCount,
		Flags:     flags,
		LastUsed:  nullInt64(&lastUsed),
		CredID:    credID,
	}))
}

func webauthnCredentialRow(row pgdb.WebauthnCredentials) store.WebauthnCredentialRow {
	return store.WebauthnCredentialRow{
		CredID:            row.CredID,
		RPID:              row.RpID,
		UserID:            row.UserID,
		PublicKey:         row.PublicKey,
		SignCount:         row.SignCount,
		AttestationType:   row.AttestationType,
		AttestationFormat: row.AttestationFormat,
		Transports:        row.Transports,
		Flags:             row.Flags,
		ExtensionsJSON:    row.ExtensionsJson,
		Name:              row.Name,
		Created:           row.Created,
		LastUsed:          ptrInt64(row.LastUsed),
	}
}

func (r *Repo) PutCeremony(ctx context.Context, c store.CeremonyRow) error {
	return wrap(r.w.PutCeremony(ctx, pgdb.PutCeremonyParams{
		ID:          c.ID,
		Kind:        int64(c.Kind),
		UserID:      c.UserID,
		SessionJson: c.SessionJSON,
		Created:     c.Created,
		Expires:     c.Expires,
	}))
}

// TakeCeremony is single use: read and delete in one transaction, and treat a
// zero-row delete as "someone else took it" rather than as success. Same split
// as PutRecoveryCodes.
func (r *Repo) TakeCeremony(ctx context.Context, ceremonyID id.ID, now int64) (store.CeremonyRow, error) {
	if r.inTx {
		return r.takeCeremonyTx(ctx, r.w, ceremonyID, now)
	}
	var out store.CeremonyRow
	err := r.Tx(ctx, func(tx store.Repository) error {
		sub, ok := tx.(*Repo)
		if !ok {
			return fmt.Errorf("store: Tx handed a %T, not this adapter's *Repo", tx)
		}
		var err error
		out, err = sub.takeCeremonyTx(ctx, sub.w, ceremonyID, now)
		return err
	})
	return out, err
}

func (r *Repo) takeCeremonyTx(ctx context.Context, q *pgdb.Queries, ceremonyID id.ID, now int64) (store.CeremonyRow, error) {
	row, err := q.GetCeremony(ctx, pgdb.GetCeremonyParams{ID: ceremonyID, Expires: now})
	if err != nil {
		return store.CeremonyRow{}, wrap(err)
	}
	n, err := q.DeleteCeremony(ctx, pgdb.DeleteCeremonyParams{ID: ceremonyID})
	if err != nil {
		return store.CeremonyRow{}, wrap(err)
	}
	if n != 1 {
		return store.CeremonyRow{}, store.ErrNotFound
	}
	// SessionJson, not SessionJSON: that is sqlc's spelling of the column, and
	// this conversion is the only place in the tree that uses it.
	return store.CeremonyRow{
		ID: row.ID, Kind: uint8(row.Kind), UserID: row.UserID,
		SessionJSON: row.SessionJson, Created: row.Created, Expires: row.Expires,
	}, nil
}

func (r *Repo) PruneCeremonies(ctx context.Context, now int64) (int64, error) {
	n, err := r.w.PruneCeremonies(ctx, pgdb.PruneCeremoniesParams{Expires: now})
	return n, wrap(err)
}

func (r *Repo) PutOIDCIdentity(ctx context.Context, issuer, subject string, userID id.ID, created int64) error {
	return wrap(r.w.PutOIDCIdentity(ctx, pgdb.PutOIDCIdentityParams{
		Issuer:  issuer,
		Subject: subject,
		UserID:  userID,
		Created: created,
	}))
}

func (r *Repo) GetOIDCIdentity(ctx context.Context, issuer, subject string) (id.ID, error) {
	userID, err := r.r.GetOIDCIdentity(ctx, pgdb.GetOIDCIdentityParams{
		Issuer:  issuer,
		Subject: subject,
	})
	if err != nil {
		return id.Zero, wrap(err)
	}
	return userID, nil
}

func (r *Repo) RecordLoginAttempt(ctx context.Context, a store.LoginAttemptRow) error {
	ok := int64(0)
	if a.OK {
		ok = 1
	}
	return wrap(r.w.RecordLoginAttempt(ctx, pgdb.RecordLoginAttemptParams{
		UserID: a.UserID,
		Ip:     a.IP,
		Method: int64(a.Method),
		Ok:     ok,
		At:     a.At,
	}))
}

func (r *Repo) CountLoginFailures(ctx context.Context, userID id.ID, since int64) (int64, error) {
	n, err := r.r.CountLoginFailures(ctx, pgdb.CountLoginFailuresParams{
		UserID: &userID,
		At:     since,
	})
	return n, wrap(err)
}

func (r *Repo) ClearLoginFailures(ctx context.Context, userID id.ID) error {
	return wrap(r.w.ClearLoginFailures(ctx, pgdb.ClearLoginFailuresParams{UserID: &userID}))
}

// ---------------------------------------------------------------- Invites

func (r *Repo) CreateInvite(ctx context.Context, i store.InviteRow) error {
	return wrap(r.w.CreateInvite(ctx, pgdb.CreateInviteParams{
		ID:            i.ID,
		CodeHash:      i.CodeHash,
		CommunityID:   i.CommunityID,
		CreatedBy:     i.CreatedBy,
		GrantsAdmin:   int64(i.GrantsAdmin),
		MaxUses:       int64(i.MaxUses),
		UsedCount:     int64(i.UsedCount),
		Created:       i.Created,
		ExpiresAt:     i.ExpiresAt,
		RevokedAt:     nullInt64(i.RevokedAt),
		UnlimitedUses: int64(i.UnlimitedUses),
	}))
}

func (r *Repo) GetInviteByHash(ctx context.Context, codeHash []byte) (store.InviteRow, error) {
	row, err := r.r.GetInviteByHash(ctx, pgdb.GetInviteByHashParams{CodeHash: codeHash})
	if err != nil {
		return store.InviteRow{}, wrap(err)
	}
	return inviteRow(row), nil
}

// RedeemInvite is the one method whose SQL is the concurrency control: the
// UPDATE's own WHERE clause is the guard, so 64 racing callers produce exactly
// one RETURNING row and 63 sql.ErrNoRows.
func (r *Repo) RedeemInvite(ctx context.Context, codeHash []byte, now int64) (store.InviteRow, error) {
	row, err := r.w.RedeemInvite(ctx, pgdb.RedeemInviteParams{CodeHash: codeHash, ExpiresAt: now})
	if errors.Is(err, sql.ErrNoRows) {
		return store.InviteRow{}, store.ErrExhausted
	}
	if err != nil {
		return store.InviteRow{}, wrap(err)
	}
	return inviteRow(row), nil
}

func (r *Repo) RevokeInvite(ctx context.Context, inviteID id.ID, at int64) error {
	return wrap(r.w.RevokeInvite(ctx, pgdb.RevokeInviteParams{
		RevokedAt: nullInt64(&at),
		ID:        inviteID,
	}))
}

func (r *Repo) ListInvites(ctx context.Context, communityID *id.ID) ([]store.InviteRow, error) {
	var rows []pgdb.Invites
	var err error
	if communityID == nil {
		rows, err = r.r.ListInvites(ctx)
	} else {
		rows, err = r.r.ListInvitesByCommunity(ctx, pgdb.ListInvitesByCommunityParams{
			CommunityID: communityID,
		})
	}
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.InviteRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, inviteRow(row))
	}
	return out, nil
}

func (r *Repo) PutInviteSignup(ctx context.Context, userID, inviteID id.ID, at int64) error {
	return wrap(r.w.PutInviteSignup(ctx, pgdb.PutInviteSignupParams{
		UserID: userID, InviteID: inviteID, Created: at,
	}))
}

// TakeInviteSignup is a DELETE whose row count is the guard, as RedeemInvite's
// UPDATE is: of two racing takes exactly one sees the row.
func (r *Repo) TakeInviteSignup(ctx context.Context, userID, inviteID id.ID) error {
	n, err := r.w.TakeInviteSignup(ctx, pgdb.TakeInviteSignupParams{UserID: userID, InviteID: inviteID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func inviteRow(row pgdb.Invites) store.InviteRow {
	return store.InviteRow{
		ID: row.ID, CodeHash: row.CodeHash, CommunityID: row.CommunityID, CreatedBy: row.CreatedBy,
		GrantsAdmin: uint8(row.GrantsAdmin), MaxUses: uint64(row.MaxUses), UsedCount: uint64(row.UsedCount),
		Created: row.Created, ExpiresAt: row.ExpiresAt, RevokedAt: ptrInt64(row.RevokedAt),
		UnlimitedUses: uint8(row.UnlimitedUses),
	}
}

// ---------------------------------------------------------------- Ops

func (r *Repo) PutReport(ctx context.Context, rep store.ReportRow) error {
	return wrap(r.w.PutReport(ctx, pgdb.PutReportParams{
		ID:                 rep.ID,
		Reporter:           rep.Reporter,
		GroupID:            rep.GroupID,
		Seq:                int64(rep.Seq),
		RevealedEnvelope:   rep.RevealedEnvelope,
		KF:                 rep.KF,
		FrankingKeyID:      rep.FrankingKeyID,
		VerificationResult: rep.VerificationResult,
		Status:             int64(rep.Status),
		Created:            rep.Created,
	}))
}

func (r *Repo) GetReport(ctx context.Context, reportID id.ID) (store.ReportRow, error) {
	row, err := r.r.GetReport(ctx, pgdb.GetReportParams{ID: reportID})
	if err != nil {
		return store.ReportRow{}, wrap(err)
	}
	return reportRow(row), nil
}

// ListReports is the report queue, newest first (Plan 2 task 17).
func (r *Repo) ListReports(ctx context.Context, limit int32) ([]store.ReportRow, error) {
	rows, err := r.r.ListReports(ctx, pgdb.ListReportsParams{MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ReportRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, reportRow(row))
	}
	return out, nil
}

// reportRow is the one place `reports` becomes store.ReportRow.
func reportRow(row pgdb.Reports) store.ReportRow {
	return store.ReportRow{
		ID:                 row.ID,
		Reporter:           row.Reporter,
		GroupID:            row.GroupID,
		Seq:                uint64(row.Seq),
		RevealedEnvelope:   row.RevealedEnvelope,
		KF:                 row.KF,
		FrankingKeyID:      row.FrankingKeyID,
		VerificationResult: row.VerificationResult,
		Status:             int32(row.Status),
		Created:            row.Created,
	}
}

func (r *Repo) UpdateReportStatus(ctx context.Context, reportID id.ID, status int32, result string) error {
	return wrap(r.w.UpdateReportStatus(ctx, pgdb.UpdateReportStatusParams{
		Status:             int64(status),
		VerificationResult: result,
		ID:                 reportID,
	}))
}

func (r *Repo) Audit(ctx context.Context, a store.AuditRow) error {
	return wrap(r.w.InsertAudit(ctx, pgdb.InsertAuditParams{
		Actor:  a.Actor,
		Action: a.Action,
		Target: a.Target,
		Detail: a.Detail,
		At:     a.At,
	}))
}

func (r *Repo) ListAudit(ctx context.Context, since int64, limit int32) ([]store.AuditRow, error) {
	rows, err := r.r.ListAudit(ctx, pgdb.ListAuditParams{At: since, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.AuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.AuditRow{
			Actor:  row.Actor,
			Action: row.Action,
			Target: row.Target,
			Detail: row.Detail,
			At:     row.At,
		})
	}
	return out, nil
}

// SchemaVersion reads goose's table with a plain SELECT: every Provider read
// path calls ensureVersionTable and would create the table as a side effect
// (gap-67 claim 9), which a read-only doctor must not do.
func (r *Repo) SchemaVersion(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := r.rawRead.QueryRowContext(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&v)
	if err != nil {
		return 0, wrap(err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// ------------------------------------------------------------------- MLS
//
// 004_mls.sql's table set. Deviation ID1: `store.Repository` embeds `MLS` from
// this task onward, so these methods and the embed land in one commit.

// mlsGroupRow maps `mls_groups` onto store.GroupRow. `epoch_unknown` is the
// column's 0/1 integer and the Row's bool (deviation B13).
func mlsGroupRow(m pgdb.MlsGroups) store.GroupRow {
	return store.GroupRow{
		GroupID:             m.GroupID,
		Binding:             m.Binding,
		Kind:                uint8(m.Kind),
		CommunityID:         m.CommunityID,
		TargetID:            m.TargetID,
		CallID:              m.CallID,
		Ciphersuite:         uint64(m.Ciphersuite),
		Epoch:               uint64(m.Epoch),
		Seq:                 uint64(m.Seq),
		GroupInfoBlob:       m.GroupInfoBlob,
		TreeHash:            m.TreeHash,
		PublicGroupState:    m.PublicGroupState,
		ExternalSenderKeyID: m.ExternalSenderKeyID,
		E2EEVersion:         uint64(m.E2eeVersion),
		MediaVersion:        uint64(m.MediaVersion),
		PolicyVersion:       uint64(m.PolicyVersion),
		EpochUnknown:        m.EpochUnknown != 0,
		HealDeadline:        ptrInt64(m.HealDeadline),
		Created:             m.Created,
		ClosedAt:            ptrInt64(m.ClosedAt),
		PrunedBelow:         uint64(m.PrunedBelow),
		HandshakesPruned:    uint64(m.HandshakesPrunedThrough),
	}
}

func (r *Repo) CreateGroup(ctx context.Context, g store.GroupRow) error {
	return wrap(r.w.CreateGroup(ctx, pgdb.CreateGroupParams{
		GroupID:             g.GroupID,
		Binding:             g.Binding,
		Kind:                int64(g.Kind),
		CommunityID:         g.CommunityID,
		TargetID:            g.TargetID,
		CallID:              g.CallID,
		Ciphersuite:         int64(g.Ciphersuite),
		Epoch:               int64(g.Epoch),
		Seq:                 int64(g.Seq),
		GroupInfoBlob:       g.GroupInfoBlob,
		TreeHash:            g.TreeHash,
		PublicGroupState:    g.PublicGroupState,
		ExternalSenderKeyID: g.ExternalSenderKeyID,
		E2eeVersion:         int64(g.E2EEVersion),
		MediaVersion:        int64(g.MediaVersion),
		PolicyVersion:       int64(g.PolicyVersion),
		EpochUnknown:        boolInt64(g.EpochUnknown),
		HealDeadline:        nullInt64(g.HealDeadline),
		Created:             g.Created,
		ClosedAt:            nullInt64(g.ClosedAt),
	}))
}

func (r *Repo) GetGroup(ctx context.Context, groupID id.ID) (store.GroupRow, error) {
	row, err := r.r.GetGroup(ctx, pgdb.GetGroupParams{GroupID: groupID})
	if err != nil {
		return store.GroupRow{}, wrap(err)
	}
	return mlsGroupRow(row), nil
}

// GroupsForTarget is P2-D3 (Plan 2 task 4): the open groups of one kind bound
// to one target, oldest first.
func (r *Repo) GroupsForTarget(ctx context.Context, targetID id.ID, kind uint8) ([]store.GroupRow, error) {
	rows, err := r.r.GroupsForTarget(ctx, pgdb.GroupsForTargetParams{TargetID: targetID, Kind: int64(kind)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.GroupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mlsGroupRow(row))
	}
	return out, nil
}

func (r *Repo) ListIdleCallGroups(ctx context.Context, cutoff int64, limit int32) ([]store.GroupRow, error) {
	rows, err := r.r.ListIdleCallGroups(ctx, pgdb.ListIdleCallGroupsParams{Created: cutoff, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.GroupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mlsGroupRow(row))
	}
	return out, nil
}

func (r *Repo) ListOpenGroups(ctx context.Context, after id.ID, limit int32) ([]store.GroupRow, error) {
	rows, err := r.r.ListOpenGroups(ctx, pgdb.ListOpenGroupsParams{GroupID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.GroupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mlsGroupRow(row))
	}
	return out, nil
}

// ListGroupsForRetention is ListOpenGroups without the `closed_at IS NULL`
// filter: retention applies to every group, closed ones included, because a
// closed group's ciphertext is still ciphertext.
func (r *Repo) ListGroupsForRetention(ctx context.Context, after id.ID, limit int32) ([]store.GroupRow, error) {
	rows, err := r.r.ListGroupsForRetention(ctx, pgdb.ListGroupsForRetentionParams{GroupID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.GroupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mlsGroupRow(row))
	}
	return out, nil
}

func (r *Repo) CloseGroup(ctx context.Context, groupID id.ID, at int64) error {
	return wrap(r.w.CloseGroup(ctx, pgdb.CloseGroupParams{
		ClosedAt: sql.NullInt64{Int64: at, Valid: true},
		GroupID:  groupID,
	}))
}

// MarkAllGroupsEpochUnknown is the first of invariant 11's three statements (deviation B13);
// ClearEpochUnknown and EndAllVoiceSessions are the other two.
func (r *Repo) MarkAllGroupsEpochUnknown(ctx context.Context, healDeadline int64) error {
	return wrap(r.w.MarkAllGroupsEpochUnknown(ctx, pgdb.MarkAllGroupsEpochUnknownParams{
		HealDeadline: healDeadline,
	}))
}

func (r *Repo) ClearEpochUnknown(ctx context.Context, groupID id.ID) error {
	return wrap(r.w.ClearEpochUnknown(ctx, pgdb.ClearEpochUnknownParams{GroupID: groupID}))
}

// EndAllVoiceSessions is invariant 11's "Live calls end", in both of its halves and in one
// transaction: every open call group is closed (Plan 1) and every live voice_sessions row is
// ended (Plan 2 task 16, P2-D19). Inside a caller's transaction it runs in that one.
func (r *Repo) EndAllVoiceSessions(ctx context.Context, at int64) error {
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error { return tx.EndAllVoiceSessions(ctx, at) })
	}
	if err := r.w.EndAllVoiceSessions(ctx, pgdb.EndAllVoiceSessionsParams{At: at}); err != nil {
		return wrap(err)
	}
	return wrap(r.w.EndAllVoiceSessionRows(ctx, pgdb.EndAllVoiceSessionRowsParams{At: at}))
}

// NextSeq allocates the next number in the group's ONE sequence space, shared by
// the handshake and application streams. The allocation is the UPDATE itself, so
// two concurrent callers never see the same value.
func (r *Repo) NextSeq(ctx context.Context, groupID id.ID) (uint64, error) {
	seq, err := r.w.BumpGroupSeq(ctx, pgdb.BumpGroupSeqParams{GroupID: groupID})
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(seq), nil
}

// PutGroupState writes the PublicGroup blob and the three columns derived from
// it. It clears `epoch_unknown`: a state blob is by definition a known epoch.
func (r *Repo) PutGroupState(ctx context.Context, groupID id.ID, epoch uint64, state, groupInfo, treeHash []byte) error {
	return wrap(r.w.PutGroupState(ctx, pgdb.PutGroupStateParams{
		Epoch:            int64(epoch),
		PublicGroupState: state,
		GroupInfoBlob:    groupInfo,
		TreeHash:         treeHash,
		GroupID:          groupID,
	}))
}

func (r *Repo) AppendHandshake(ctx context.Context, h store.HandshakeRow) error {
	return wrap(r.w.AppendHandshake(ctx, pgdb.AppendHandshakeParams{
		GroupID:      h.GroupID,
		Seq:          int64(h.Seq),
		Epoch:        int64(h.Epoch),
		Kind:         int64(h.Kind),
		SenderLeaf:   nullUint32(h.SenderLeaf),
		SenderDevice: idBytes(h.SenderDevice),
		Blob:         h.Blob,
		Created:      h.Created,
	}))
}

func (r *Repo) ListHandshakes(ctx context.Context, groupID id.ID, fromSeq uint64, limit int32) ([]store.HandshakeRow, error) {
	fromSeqInt, err := checkedInt64(fromSeq, "handshake sequence")
	if err != nil {
		return nil, err
	}

	rows, err := r.r.ListHandshakes(ctx, pgdb.ListHandshakesParams{
		GroupID: groupID, Seq: fromSeqInt, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.HandshakeRow, 0, len(rows))
	for _, m := range rows {
		device, err := idPtr(m.SenderDevice)
		if err != nil {
			return nil, err
		}
		out = append(out, store.HandshakeRow{
			GroupID:      m.GroupID,
			Seq:          uint64(m.Seq),
			Epoch:        uint64(m.Epoch),
			Kind:         uint8(m.Kind),
			SenderLeaf:   ptrUint32(m.SenderLeaf),
			SenderDevice: device,
			Blob:         m.Blob,
			Created:      m.Created,
		})
	}
	return out, nil
}

// GetCommitAtEpoch is the handshake that carried the group into `epoch`: the
// lowest-seq commit or external commit recorded at it, read over
// `mls_handshakes_by_epoch`. `E_COMMIT_CONFLICT` names that blob, and a scan of
// the log from seq 0 would miss it on any group with more than one page of live
// handshakes (deviation B13).
func (r *Repo) GetCommitAtEpoch(ctx context.Context, groupID id.ID, epoch uint64) (store.HandshakeRow, error) {
	m, err := r.r.GetCommitAtEpoch(ctx, pgdb.GetCommitAtEpochParams{
		GroupID: groupID, Epoch: int64(epoch),
	})
	if err != nil {
		return store.HandshakeRow{}, wrap(err)
	}
	device, err := idPtr(m.SenderDevice)
	if err != nil {
		return store.HandshakeRow{}, err
	}
	return store.HandshakeRow{
		GroupID:      m.GroupID,
		Seq:          uint64(m.Seq),
		Epoch:        uint64(m.Epoch),
		Kind:         uint8(m.Kind),
		SenderLeaf:   ptrUint32(m.SenderLeaf),
		SenderDevice: device,
		Blob:         m.Blob,
		Created:      m.Created,
	}, nil
}

// OldestHandshakeSeq is the retention floor a resync is refused below. An empty
// log has no floor, which is 0 and not ErrNotFound: a group with nothing to
// replay refuses nothing.
func (r *Repo) OldestHandshakeSeq(ctx context.Context, groupID id.ID) (uint64, error) {
	seq, err := r.r.OldestHandshakeSeq(ctx, pgdb.OldestHandshakeSeqParams{GroupID: groupID})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(seq), nil
}

// PruneHandshakes deletes every handshake older than `before` and, in the same transaction and
// first, raises each affected group's handshakes_pruned_through to the highest seq it loses: the
// catch-up's E_PRUNED is decided against that mark, so a deletion the mark does not record would
// serve a log with a silent hole.
func (r *Repo) PruneHandshakes(ctx context.Context, before int64) (int64, error) {
	var n int64
	err := r.atomically(ctx, func(q *pgdb.Queries) error {
		if err := q.RaiseHandshakesPrunedThrough(ctx, pgdb.RaiseHandshakesPrunedThroughParams{Created: before}); err != nil {
			return err
		}
		var err error
		n, err = q.PruneHandshakes(ctx, pgdb.PruneHandshakesParams{Created: before})
		return err
	})
	return n, wrap(err)
}

// atomically runs fn on the transaction this repository is already in, or on a new one.
func (r *Repo) atomically(ctx context.Context, fn func(q *pgdb.Queries) error) error {
	if r.inTx {
		return fn(r.w)
	}
	return r.Tx(ctx, func(tx store.Repository) error {
		sub, ok := tx.(*Repo)
		if !ok {
			return errors.New("store: a transaction that is not this engine's")
		}
		return fn(sub.w)
	})
}

func (r *Repo) PutProposal(ctx context.Context, p store.ProposalRow) error {
	return wrap(r.w.PutProposal(ctx, proposalParams(p)))
}

func proposalParams(p store.ProposalRow) pgdb.PutProposalParams {
	return pgdb.PutProposalParams{
		GroupID:      p.GroupID,
		Ref:          p.Ref,
		Epoch:        int64(p.Epoch),
		Kind:         int64(p.Kind),
		TargetLeaf:   nullUint32(p.TargetLeaf),
		TargetDevice: idBytes(p.TargetDevice),
		KeyPackage:   p.KeyPackage,
		Origin:       int64(p.Origin),
		ActionID:     p.ActionID,
		IssuedAt:     p.IssuedAt,
		Ttl:          int64(p.TTL),
		VoidAt:       nullInt64(p.VoidAt),
	}
}

func mlsProposalRow(m pgdb.MlsPendingProposals) (store.ProposalRow, error) {
	device, err := idPtr(m.TargetDevice)
	if err != nil {
		return store.ProposalRow{}, err
	}
	return store.ProposalRow{
		GroupID:      m.GroupID,
		Ref:          m.Ref,
		Epoch:        uint64(m.Epoch),
		Kind:         uint8(m.Kind),
		TargetLeaf:   ptrUint32(m.TargetLeaf),
		TargetDevice: device,
		KeyPackage:   m.KeyPackage,
		Origin:       uint8(m.Origin),
		ActionID:     m.ActionID,
		IssuedAt:     m.IssuedAt,
		TTL:          uint64(m.Ttl),
		VoidAt:       ptrInt64(m.VoidAt),
	}, nil
}

func (r *Repo) ListProposals(ctx context.Context, groupID id.ID, epoch uint64, includeVoid bool) ([]store.ProposalRow, error) {
	var rows []pgdb.MlsPendingProposals
	var err error
	if includeVoid {
		rows, err = r.r.ListAllProposals(ctx, pgdb.ListAllProposalsParams{GroupID: groupID, Epoch: int64(epoch)})
	} else {
		rows, err = r.r.ListLiveProposals(ctx, pgdb.ListLiveProposalsParams{GroupID: groupID, Epoch: int64(epoch)})
	}
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ProposalRow, 0, len(rows))
	for _, m := range rows {
		p, err := mlsProposalRow(m)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *Repo) ListClosedGroupLiveProposals(ctx context.Context, limit int32) ([]store.ProposalRow, error) {
	rows, err := r.r.ListClosedGroupLiveProposals(ctx, pgdb.ListClosedGroupLiveProposalsParams{MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ProposalRow, 0, len(rows))
	for _, m := range rows {
		p, err := mlsProposalRow(m)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *Repo) CountLiveInstanceProposals(ctx context.Context) (map[uint8]int64, error) {
	rows, err := r.r.CountLiveInstanceProposals(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	out := make(map[uint8]int64, len(rows))
	for _, row := range rows {
		out[uint8(row.Kind)] = row.N // kind is CHECKed to 0..3 by the schema
	}
	return out, nil
}

func (r *Repo) VoidProposal(ctx context.Context, groupID id.ID, ref []byte, at int64) error {
	return wrap(r.w.VoidProposal(ctx, pgdb.VoidProposalParams{
		VoidAt: sql.NullInt64{Int64: at, Valid: true}, GroupID: groupID, Ref: ref,
	}))
}

func (r *Repo) DeleteProposals(ctx context.Context, groupID id.ID, refs [][]byte) error {
	for _, ref := range refs {
		if err := r.w.DeleteProposal(ctx, pgdb.DeleteProposalParams{GroupID: groupID, Ref: ref}); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// ReissueProposal replaces one outstanding row with a fresh one and KEEPS the
// old row's action_id: the logical action survives a re-issue with a new
// KeyPackage, which is what lets the DS tell a retry from a second request.
func (r *Repo) ReissueProposal(ctx context.Context, oldRef []byte, p store.ProposalRow) error {
	old, err := r.w.GetProposal(ctx, pgdb.GetProposalParams{GroupID: p.GroupID, Ref: oldRef})
	switch {
	case err == nil:
		p.ActionID = old.ActionID
		if err := r.w.DeleteProposal(ctx, pgdb.DeleteProposalParams{GroupID: p.GroupID, Ref: oldRef}); err != nil {
			return wrap(err)
		}
	case errors.Is(err, sql.ErrNoRows):
		// Nothing to supersede; the re-issue is an ordinary insert.
	default:
		return wrap(err)
	}
	return wrap(r.w.PutProposal(ctx, proposalParams(p)))
}

// ReplaceMembers rewrites one group's leaves. Its two statements belong to the
// caller's transaction: every delivery-service caller runs it inside the same Tx
// as the handshake row that changed the tree (R12).
func (r *Repo) ReplaceMembers(ctx context.Context, groupID id.ID, epoch uint64, m []store.MemberRow) error {
	if err := r.w.DeleteMembers(ctx, pgdb.DeleteMembersParams{GroupID: groupID}); err != nil {
		return wrap(err)
	}
	for _, row := range m {
		if err := r.w.PutMemberLeaf(ctx, pgdb.PutMemberLeafParams{
			GroupID:      groupID,
			LeafIndex:    int64(row.LeafIndex),
			UserID:       row.UserID,
			DeviceID:     row.DeviceID,
			SignatureKey: row.SignatureKey,
			AddedEpoch:   int64(row.AddedEpoch),
			RemovedEpoch: nullUint64(row.RemovedEpoch),
		}); err != nil {
			return wrap(err)
		}
	}
	return nil
}

func (r *Repo) ListMembers(ctx context.Context, groupID id.ID) ([]store.MemberRow, error) {
	rows, err := r.r.ListMembers(ctx, pgdb.ListMembersParams{GroupID: groupID})
	if err != nil {
		return nil, wrap(err)
	}
	return memberRows(rows), nil
}

// ListBarredMembers is the group's live leaves whose device is quarantined or revoked.
func (r *Repo) ListBarredMembers(ctx context.Context, groupID id.ID) ([]store.MemberRow, error) {
	rows, err := r.r.ListBarredMembers(ctx, pgdb.ListBarredMembersParams{GroupID: groupID})
	if err != nil {
		return nil, wrap(err)
	}
	return memberRows(rows), nil
}

func memberRows(rows []pgdb.MlsMembers) []store.MemberRow {
	out := make([]store.MemberRow, 0, len(rows))
	for _, m := range rows {
		out = append(out, store.MemberRow{
			GroupID:      m.GroupID,
			LeafIndex:    uint32(m.LeafIndex),
			UserID:       m.UserID,
			DeviceID:     m.DeviceID,
			SignatureKey: m.SignatureKey,
			AddedEpoch:   uint64(m.AddedEpoch),
			RemovedEpoch: ptrUint64(m.RemovedEpoch),
		})
	}
	return out
}

func (r *Repo) GroupsForDevice(ctx context.Context, deviceID id.ID) ([]id.ID, error) {
	rows, err := r.r.GroupsForDevice(ctx, pgdb.GroupsForDeviceParams{DeviceID: deviceID})
	return rows, wrap(err)
}

// PutKeyPackages writes a device's published packages, and holds the directory's one structural
// invariant: a device has AT MOST ONE last-resort package, so a new one REPLACES the old. That is
// protocol/01 § Joining in SQL — "each device keeps 32 ordinary KeyPackages plus 1 last-resort
// KeyPackage on the DS" — and nothing else in the stack enforces the second half of that sentence.
//
// The rule is here rather than in the delivery service because nothing above can see the rows it
// bounds. `key_packages` is unique only on `(device_id, kp_ref)`, and `CountKeyPackages` — the
// count the publish cap is taken against — filters `last_resort = 0` by contract, because the
// number it reports is what a client refills against and the fallback package is not one a client
// refills. Without this, a device that mints a fresh valid last-resort package and publishes it in
// a loop adds one unbounded row per call, each a full KeyPackage blob held for its 90-day
// lifetime. Dropping the older row is also what the package MEANS: one reusable fallback per
// device, and RFC 9420 gives a joiner no way to choose between two.
//
// Insert first, then delete the others, and both inside ONE transaction: the order keeps the
// device addressable at every instant (a delete-then-insert leaves a window with no fallback at
// all), and the transaction keeps two concurrent republishes from each deleting the other's row —
// here it does the work MVCC will not, since two autocommitted deletes both read the snapshot
// before the other's insert.
func (r *Repo) PutKeyPackages(ctx context.Context, deviceID id.ID, kps []store.KeyPackageRow) error {
	// Only a publish that carries a last-resort package needs the transaction; the ordinary path,
	// which is the hot one, pays nothing.
	if !r.inTx && holdsLastResort(kps) {
		return r.Tx(ctx, func(s store.Repository) error {
			return s.PutKeyPackages(ctx, deviceID, kps)
		})
	}
	for _, kp := range kps {
		if err := r.w.PutKeyPackage(ctx, pgdb.PutKeyPackageParams{
			DeviceID:   deviceID,
			KpRef:      kp.KPRef,
			Blob:       kp.Blob,
			LastResort: int64(kp.LastResort),
			Expires:    kp.Expires,
			Created:    kp.Created,
			ConsumedAt: nullInt64(kp.ConsumedAt),
		}); err != nil {
			return wrap(err)
		}
		if kp.LastResort == 0 {
			continue
		}
		if err := r.w.DeleteOtherLastResortKeyPackages(ctx, pgdb.DeleteOtherLastResortKeyPackagesParams{
			DeviceID: deviceID,
			KpRef:    kp.KPRef,
		}); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// holdsLastResort reports whether the batch carries a last-resort package. A batch with more than
// one is not rejected: each insert drops the ones before it, so the last one written is the one
// that survives, and the invariant holds however the caller batched its rows.
func holdsLastResort(kps []store.KeyPackageRow) bool {
	for _, kp := range kps {
		if kp.LastResort != 0 {
			return true
		}
	}
	return false
}

// TakeKeyPackage consumes one ordinary KeyPackage, and falls back to the
// device's last-resort package WITHOUT consuming it: a last-resort package is
// reusable by construction, and consuming it would leave the device unaddable.
func (r *Repo) TakeKeyPackage(ctx context.Context, deviceID id.ID, now int64) (store.KeyPackageRow, error) {
	kp, err := r.w.TakeKeyPackage(ctx, pgdb.TakeKeyPackageParams{
		Now: sql.NullInt64{Int64: now, Valid: true}, DeviceID: deviceID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		kp, err = r.r.GetLastResortKeyPackage(ctx, pgdb.GetLastResortKeyPackageParams{
			DeviceID: deviceID, Expires: now,
		})
	}
	if err != nil {
		return store.KeyPackageRow{}, wrap(err)
	}
	return store.KeyPackageRow{
		DeviceID:   kp.DeviceID,
		KPRef:      kp.KpRef,
		Blob:       kp.Blob,
		LastResort: uint8(kp.LastResort),
		Expires:    kp.Expires,
		Created:    kp.Created,
		ConsumedAt: ptrInt64(kp.ConsumedAt),
	}, nil
}

// CountKeyPackages counts the ORDINARY packages still available: the last-resort
// one is never the answer to "is this device running low".
func (r *Repo) CountKeyPackages(ctx context.Context, deviceID id.ID, now int64) (int64, error) {
	n, err := r.r.CountKeyPackages(ctx, pgdb.CountKeyPackagesParams{DeviceID: deviceID, Expires: now})
	return n, wrap(err)
}

// HasCurrentLastResortKeyPackage reports whether a live reusable fallback exists without
// consuming it. It is used by the managed directory inventory snapshot.
func (r *Repo) HasCurrentLastResortKeyPackage(ctx context.Context, deviceID id.ID, now int64) (bool, error) {
	_, err := r.r.GetLastResortKeyPackage(ctx, pgdb.GetLastResortKeyPackageParams{DeviceID: deviceID, Expires: now})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrap(err)
	}
	return true, nil
}

func (r *Repo) DeleteKeyPackage(ctx context.Context, deviceID id.ID, kpRef []byte) error {
	return wrap(r.w.DeleteKeyPackage(ctx, pgdb.DeleteKeyPackageParams{DeviceID: deviceID, KpRef: kpRef}))
}

func (r *Repo) PurgeKeyPackages(ctx context.Context, keepLastResort bool) (int64, error) {
	if keepLastResort {
		n, err := r.w.PurgeKeyPackagesKeepingLastResort(ctx)
		return n, wrap(err)
	}
	n, err := r.w.PurgeAllKeyPackages(ctx)
	return n, wrap(err)
}

func (r *Repo) LockKeyPackageDevice(ctx context.Context, deviceID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	// Lock the actual device row so this fence also serializes with plain UPDATE writers such as
	// QuarantineDevice and non-transactional RevokeDevice. A repository-only advisory lock would
	// coordinate publishers while allowing those authority changes to pass it unnoticed.
	var locked id.ID
	if err := r.tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE id=$1 FOR UPDATE`, deviceID).Scan(&locked); err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) GetBotKeyPackagePublicationReceipt(ctx context.Context, installation, operation id.ID) (store.KeyPackagePublicationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.rawRead)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.KeyPackagePublicationReceipt
	var generation, count int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,user_id,device_id,operation_id,generation,upload_sha256,package_count,accepted_at FROM bot_keypackage_publication_receipts WHERE installation_id=$1 AND operation_id=$2`, installation, operation).Scan(&out.InstallationID, &out.UserID, &out.DeviceID, &out.OperationID, &generation, &out.UploadSHA256, &count, &out.AcceptedAt)
	if err != nil {
		return store.KeyPackagePublicationReceipt{}, wrap(err)
	}
	out.Generation, out.Count = uint64(generation), uint64(count)
	return out, nil
}

func (r *Repo) CreateBotKeyPackagePublicationReceipt(ctx context.Context, v store.KeyPackagePublicationReceipt) error {
	if !r.inTx || len(v.UploadSHA256) != 32 || v.Count < 1 || v.Count > 33 {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_keypackage_publication_receipts(installation_id,user_id,device_id,operation_id,generation,upload_sha256,package_count,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, v.InstallationID, v.UserID, v.DeviceID, v.OperationID, int64(v.Generation), v.UploadSHA256, int64(v.Count), v.AcceptedAt)
	return wrap(err)
}

func (r *Repo) DeleteBotKeyPackagePublicationReceipts(ctx context.Context, installation id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.tx.ExecContext(ctx, `DELETE FROM bot_keypackage_publication_receipts WHERE installation_id=$1`, installation)
	return wrap(err)
}

func (r *Repo) GetKeyPackage(ctx context.Context, deviceID id.ID, ref []byte) (store.KeyPackageRow, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.rawRead)
	if r.tx != nil {
		queryer = r.tx
	}
	var row store.KeyPackageRow
	var last int64
	err := queryer.QueryRowContext(ctx, `SELECT device_id,kp_ref,blob,last_resort,expires,created,consumed_at FROM key_packages WHERE device_id=$1 AND kp_ref=$2`, deviceID, ref).Scan(&row.DeviceID, &row.KPRef, &row.Blob, &last, &row.Expires, &row.Created, &row.ConsumedAt)
	if err != nil {
		return store.KeyPackageRow{}, wrap(err)
	}
	row.LastResort = uint8(last)
	return row, nil
}

func (r *Repo) PutWelcomePayload(ctx context.Context, w store.WelcomePayloadRow) error {
	return wrap(r.w.PutWelcomePayload(ctx, pgdb.PutWelcomePayloadParams{
		BlobSha256: w.BlobSHA256,
		GroupID:    w.GroupID,
		Epoch:      int64(w.Epoch),
		Blob:       w.Blob,
		Created:    w.Created,
	}))
}

func (r *Repo) PutEpochTree(ctx context.Context, t store.EpochTreeRow) error {
	return wrap(r.w.PutEpochTree(ctx, pgdb.PutEpochTreeParams{
		GroupID:     t.GroupID,
		Epoch:       int64(t.Epoch),
		RatchetTree: t.RatchetTree,
		TreeHash:    t.TreeHash,
		Created:     t.Created,
	}))
}

func (r *Repo) PutWelcomes(ctx context.Context, ws []store.WelcomeRow) error {
	for _, w := range ws {
		if err := r.w.PutWelcome(ctx, pgdb.PutWelcomeParams{
			DeviceID:    w.DeviceID,
			GroupID:     w.GroupID,
			Epoch:       int64(w.Epoch),
			CommitSeq:   int64(w.CommitSeq),
			BlobSha256:  w.BlobSHA256,
			Created:     w.Created,
			Expires:     w.Expires,
			DeliveredAt: nullInt64(w.DeliveredAt),
		}); err != nil {
			return wrap(err)
		}
	}
	return nil
}

func (r *Repo) ListWelcomes(ctx context.Context, deviceID id.ID, afterID int64, limit int32) ([]store.WelcomeFull, error) {
	rows, err := r.r.ListWelcomes(ctx, pgdb.ListWelcomesParams{
		DeviceID: deviceID, WelcomeID: afterID, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.WelcomeFull, 0, len(rows))
	for _, m := range rows {
		out = append(out, store.WelcomeFull{
			WelcomeRow: store.WelcomeRow{
				WelcomeID:   m.WelcomeID,
				DeviceID:    m.DeviceID,
				GroupID:     m.GroupID,
				Epoch:       uint64(m.Epoch),
				CommitSeq:   uint64(m.CommitSeq),
				BlobSHA256:  m.BlobSha256,
				Created:     m.Created,
				Expires:     m.Expires,
				DeliveredAt: ptrInt64(m.DeliveredAt),
			},
			Blob: m.Blob,
			// The tree of the WELCOMING epoch, from the LEFT JOIN on
			// `mls_epoch_trees`: a dilla Welcome carries no ratchet tree and
			// the live one has moved on (protocol/02 row 15). The join is
			// LEFT so a Welcome whose epoch tree has been pruned still
			// reaches its device — with an empty tree it can act on rather
			// than vanishing from the queue.
			RatchetTree: m.RatchetTree,
			TreeHash:    m.TreeHash,
		})
	}
	return out, nil
}

// DeleteWelcome marks one queued Welcome delivered. The row survives so the
// unique index keeps the same payload from being queued to the same device
// twice.
func (r *Repo) DeleteWelcome(ctx context.Context, deviceID id.ID, welcomeID int64, at int64) error {
	return wrap(r.w.DeleteWelcome(ctx, pgdb.DeleteWelcomeParams{
		DeliveredAt: sql.NullInt64{Int64: at, Valid: true},
		DeviceID:    deviceID,
		WelcomeID:   welcomeID,
	}))
}

func (r *Repo) PruneWelcomes(ctx context.Context, before int64) (int64, error) {
	n, err := r.w.PruneWelcomes(ctx, pgdb.PruneWelcomesParams{Expires: before})
	return n, wrap(err)
}

func (r *Repo) PutForkReport(ctx context.Context, f store.ForkReportRow) error {
	return wrap(r.w.PutForkReport(ctx, pgdb.PutForkReportParams{
		GroupID:        f.GroupID,
		Seq:            int64(f.Seq),
		ReporterDevice: f.ReporterDevice,
		Epoch:          int64(f.Epoch),
		Reason:         f.Reason,
		Created:        f.Created,
	}))
}

// CountForkReporters counts DISTINCT reporters without saying so: the primary
// key is (group_id, seq, reporter_device), so one device contributes one row.
func (r *Repo) CountForkReporters(ctx context.Context, groupID id.ID, seq uint64) (int64, error) {
	n, err := r.r.CountForkReporters(ctx, pgdb.CountForkReportersParams{GroupID: groupID, Seq: int64(seq)})
	return n, wrap(err)
}

func (r *Repo) QuarantineDevice(ctx context.Context, deviceID id.ID, at int64, reason string) error {
	return wrap(r.w.QuarantineDevice(ctx, pgdb.QuarantineDeviceParams{
		QuarantinedAt:    sql.NullInt64{Int64: at, Valid: true},
		QuarantineReason: reason,
		ID:               deviceID,
	}))
}

// ---------------------------------------------------------------- Messages

// PutAppMessage appends one application ciphertext at the seq NextSeq allocated.
// It runs inside the delivery service's own transaction, beside that allocation.
func (r *Repo) PutAppMessage(ctx context.Context, m store.AppMessageRow) error {
	return wrap(r.w.PutAppMessage(ctx, pgdb.PutAppMessageParams{
		GroupID:        m.GroupID,
		Seq:            int64(m.Seq),
		Epoch:          int64(m.Epoch),
		UploaderDevice: m.UploaderDevice,
		Blob:           m.Blob,
		CommitmentC:    m.CommitmentC,
		FrankingTag:    m.FrankingTag,
		Size:           int64(m.Size),
		Created:        m.Created,
		Expires:        nullInt64(m.Expires),
		DeletedAt:      nullInt64(m.DeletedAt),
		FrankingKeyID:  m.FrankingKeyID,
		Class:          nullClass(m.Class),
	}))
}

func (r *Repo) ListAppMessages(ctx context.Context, groupID id.ID, fromSeq uint64, limit int32) ([]store.AppMessageRow, error) {
	fromSeqInt, err := checkedInt64(fromSeq, "app-message sequence")
	if err != nil {
		return nil, err
	}

	rows, err := r.r.ListAppMessages(ctx, pgdb.ListAppMessagesParams{
		GroupID: groupID, Seq: fromSeqInt, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.AppMessageRow, 0, len(rows))
	for _, m := range rows {
		out = append(out, appMessageRow(m))
	}
	return out, nil
}

func (r *Repo) GetAppMessage(ctx context.Context, groupID id.ID, seq uint64) (store.AppMessageRow, error) {
	seqInt, err := checkedInt64(seq, "app-message sequence")
	if err != nil {
		return store.AppMessageRow{}, err
	}

	row, err := r.r.GetAppMessage(ctx, pgdb.GetAppMessageParams{GroupID: groupID, Seq: seqInt})
	if err != nil {
		return store.AppMessageRow{}, wrap(err)
	}
	return appMessageRow(row), nil
}

// appMessageRow is the one place `mls_app_messages` becomes store.AppMessageRow.
// A tombstoned row keeps every column but `blob`, which the UPDATE nulls.
func appMessageRow(m pgdb.MlsAppMessages) store.AppMessageRow {
	return store.AppMessageRow{
		GroupID:        m.GroupID,
		Seq:            uint64(m.Seq),
		Epoch:          uint64(m.Epoch),
		UploaderDevice: m.UploaderDevice,
		Blob:           m.Blob,
		CommitmentC:    m.CommitmentC,
		FrankingTag:    m.FrankingTag,
		Size:           uint64(m.Size),
		Created:        m.Created,
		Expires:        ptrInt64(m.Expires),
		DeletedAt:      ptrInt64(m.DeletedAt),
		FrankingKeyID:  m.FrankingKeyID,
		Class:          ptrClass(m.Class),
	}
}

// nullClass and ptrClass convert the upload class (web3.7 SB1) between the
// store's *uint8 and the generated sql.NullInt64; the column's CHECK keeps it
// to NULL, 0 or 1.
func nullClass(v *uint8) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func ptrClass(v sql.NullInt64) *uint8 {
	if !v.Valid {
		return nil
	}
	c := uint8(v.Int64)
	return &c
}

// TombstoneAppMessage drops the ciphertext and records when. The row itself
// stays: seq, epoch, uploader_device, commitment_c, franking_tag and recv_ts are
// what a franking report is checked against, and a deleted message must still be
// reportable (R29). The `deleted_at IS NULL` guard keeps a second delete from
// moving the timestamp.
func (r *Repo) TombstoneAppMessage(ctx context.Context, groupID id.ID, seq uint64, at int64) error {
	return wrap(r.w.TombstoneAppMessage(ctx, pgdb.TombstoneAppMessageParams{
		DeletedAt: sql.NullInt64{Int64: at, Valid: true},
		GroupID:   groupID,
		Seq:       int64(seq),
	}))
}

func (r *Repo) PruneAppMessages(ctx context.Context, groupID id.ID,
	cursorFloor uint64, deliveryFloor, now int64) (int64, error) {
	var n int64
	err := r.atomically(ctx, func(q *pgdb.Queries) error {
		// The high-water first, from the DELIVERY half of the predicate the DELETE applies:
		// pruned_below is then exactly the highest seq delivery retention takes. An archival
		// expiry deletes without moving it, so the mark never stands above a surviving message
		// (Plan 2 task 8's retention ruling; see MaxPrunableAppMessageSeq).
		top, err := q.MaxPrunableAppMessageSeq(ctx, pgdb.MaxPrunableAppMessageSeqParams{
			GroupID:       groupID,
			CursorFloor:   int64(cursorFloor),
			DeliveryFloor: deliveryFloor,
		})
		if err != nil {
			return err
		}
		if top > 0 {
			if err := q.RaisePrunedBelow(ctx, pgdb.RaisePrunedBelowParams{GroupID: groupID, PrunedBelow: top}); err != nil {
				return err
			}
		}
		n, err = q.PruneAppMessages(ctx, pgdb.PruneAppMessagesParams{
			GroupID:       groupID,
			CursorFloor:   int64(cursorFloor),
			DeliveryFloor: deliveryFloor,
			Now:           now,
		})
		return err
	})
	return n, wrap(err)
}

func (r *Repo) RaisePrunedBelow(ctx context.Context, groupID id.ID, below uint64) error {
	return wrap(r.w.RaisePrunedBelow(ctx, pgdb.RaisePrunedBelowParams{
		GroupID:     groupID,
		PrunedBelow: int64(below),
	}))
}

// ---------------------------------------------------------------- Cursors

func (r *Repo) PutCursor(ctx context.Context, deviceID, groupID id.ID, lastSeq, lastEpoch uint64, at int64) error {
	return wrap(r.w.PutCursor(ctx, pgdb.PutCursorParams{
		DeviceID:  deviceID,
		GroupID:   groupID,
		LastSeq:   int64(lastSeq),
		LastEpoch: int64(lastEpoch),
		Updated:   at,
	}))
}

// GetCursor answers a device that has acknowledged nothing in this group with a
// ZERO CursorRow and a nil error, not store.ErrNotFound. A cursor row is created
// by the first POST /cursor, while the gateway reads one per group of every
// device that connects (internal/gateway/session.go, sendReady) and returns the
// first error it gets: an ErrNotFound here would close every new connection
// before `ready`, and `Gateway.Online` — the predicate invariants 5, 6 and 7 are
// all defined over — would then be false for every device forever. "No row" and
// "acknowledged nothing" are the same state, so the absent row is not an error.
func (r *Repo) GetCursor(ctx context.Context, deviceID, groupID id.ID) (store.CursorRow, error) {
	row, err := r.r.GetCursor(ctx, pgdb.GetCursorParams{DeviceID: deviceID, GroupID: groupID})
	if errors.Is(err, sql.ErrNoRows) {
		return store.CursorRow{DeviceID: deviceID, GroupID: groupID}, nil
	}
	if err != nil {
		return store.CursorRow{}, wrap(err)
	}
	return store.CursorRow{
		DeviceID:  row.DeviceID,
		GroupID:   row.GroupID,
		LastSeq:   uint64(row.LastSeq),
		LastEpoch: uint64(row.LastEpoch),
		Updated:   row.Updated,
	}, nil
}

// MinCursor is the retention floor: the lowest seq acknowledged by any device
// that is still eligible, which is any device whose cursor moved at or after
// `activeSince`. A group no eligible device has acknowledged anything in
// answers 0, which retains everything.
func (r *Repo) MinCursor(ctx context.Context, groupID id.ID, activeSince int64) (uint64, error) {
	n, err := r.r.MinCursor(ctx, pgdb.MinCursorParams{GroupID: groupID, Updated: activeSince})
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(n), nil
}

// boolInt64, nullUint32/ptrUint32, nullUint64/ptrUint64 and idBytes/idPtr are
// 004_mls.sql's conversions: a 0/1 integer column, a uint32 leaf index in a
// nullable INTEGER, a uint64 epoch in one, and a nullable 16-byte identifier
// column that no sqlc override types as id.ID because its name does not end in
// `_id`.
func boolInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullUint32(v *uint32) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func ptrUint32(v sql.NullInt64) *uint32 {
	if !v.Valid {
		return nil
	}
	n := uint32(v.Int64)
	return &n
}

func nullUint64(v *uint64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func ptrUint64(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	n := uint64(v.Int64)
	return &n
}

func idBytes(v *id.ID) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v[:]...)
}

func idPtr(b []byte) (*id.ID, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if len(b) != id.Size {
		return nil, fmt.Errorf("store: identifier column is %d bytes, want %d", len(b), id.Size)
	}
	var out id.ID
	copy(out[:], b)
	return &out, nil
}

// ---------------------------------------------------------------- Communities
//
// Plan 2 task 1: the slice of store.Structure whose tables 00004_structure.sql
// ships. The fifteen methods are internal/store/sqlite/repo.go's with the
// package name changed: policy_json is TEXT on both engines (the plan's JSONB
// would re-serialise the policy, so the bytes read back would not be the bytes
// written) and sqlc.yaml pulls the three SMALLINT flags back to int64, so the
// generated parameter types are identical.

func (r *Repo) CreateCommunity(ctx context.Context, c store.CommunityRow) error {
	return wrap(r.w.CreateCommunity(ctx, pgdb.CreateCommunityParams{
		ID:                   c.ID,
		Owner:                c.Owner,
		Name:                 c.Name,
		IconBlob:             c.IconBlob,
		PolicyJson:           string(c.PolicyJSON),
		PolicyVersion:        int64(c.PolicyVersion),
		MinAccountAgeSeconds: int64(c.MinAccountAgeSeconds),
		RequireMod2fa:        int64(c.RequireMod2FA),
		Created:              c.Created,
		DeletedAt:            nullInt64(c.DeletedAt),
	}))
}

func (r *Repo) GetCommunity(ctx context.Context, communityID id.ID) (store.CommunityRow, error) {
	row, err := r.r.GetCommunity(ctx, pgdb.GetCommunityParams{ID: communityID})
	if err != nil {
		return store.CommunityRow{}, wrap(err)
	}
	return store.CommunityRow{
		ID:                   row.ID,
		Owner:                row.Owner,
		Name:                 row.Name,
		IconBlob:             row.IconBlob,
		PolicyJSON:           []byte(row.PolicyJson),
		PolicyVersion:        uint64(row.PolicyVersion),
		MinAccountAgeSeconds: uint64(row.MinAccountAgeSeconds),
		RequireMod2FA:        uint8(row.RequireMod2fa),
		Created:              row.Created,
		DeletedAt:            ptrInt64(row.DeletedAt),
	}, nil
}

func (r *Repo) GetCommunityAFK(ctx context.Context, communityID id.ID) (store.CommunityAFK, error) {
	row, err := r.r.GetCommunityAFK(ctx, pgdb.GetCommunityAFKParams{CommunityID: communityID})
	if errors.Is(err, sql.ErrNoRows) {
		return store.CommunityAFK{TimeoutSeconds: 300}, nil
	}
	if err != nil {
		return store.CommunityAFK{}, wrap(err)
	}
	return store.CommunityAFK{ChannelID: row.ChannelID, TimeoutSeconds: uint64(row.TimeoutSeconds)}, nil
}

func (r *Repo) SetAFKAway(ctx context.Context, communityID, userID id.ID, since int64) (bool, error) {
	n, err := r.w.SetAFKAway(ctx, pgdb.SetAFKAwayParams{CommunityID: communityID, UserID: userID, Since: since})
	// The only foreign key is onto members: a user who is no member (a kick that landed between a
	// caller's check and this insert) is ErrNotFound, as the store answers a missing member.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return false, fmt.Errorf("%w: %w", store.ErrNotFound, err)
	}
	return n > 0, wrap(err)
}

func (r *Repo) ClearAFKAway(ctx context.Context, communityID, userID id.ID) (bool, error) {
	n, err := r.w.ClearAFKAway(ctx, pgdb.ClearAFKAwayParams{CommunityID: communityID, UserID: userID})
	return n > 0, wrap(err)
}

func (r *Repo) ClearAFKAwayUser(ctx context.Context, userID id.ID) ([]id.ID, error) {
	cids, err := r.w.ClearAFKAwayUser(ctx, pgdb.ClearAFKAwayUserParams{UserID: userID})
	if err != nil {
		return nil, wrap(err)
	}
	// DELETE ... RETURNING has no ORDER BY on either engine; sort so both answer the same.
	slices.SortFunc(cids, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	return cids, nil
}

func (r *Repo) ListAFKAway(ctx context.Context, communityID id.ID) ([]store.AFKAway, error) {
	rows, err := r.r.ListAFKAway(ctx, pgdb.ListAFKAwayParams{CommunityID: communityID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.AFKAway, len(rows))
	for i, row := range rows {
		out[i] = store.AFKAway{UserID: row.UserID, Since: row.Since}
	}
	return out, nil
}

func (r *Repo) AFKAwayExists(ctx context.Context, communityID, userID id.ID) (bool, error) {
	n, err := r.r.AFKAwayExists(ctx, pgdb.AFKAwayExistsParams{CommunityID: communityID, UserID: userID})
	return n > 0, wrap(err)
}

func (r *Repo) AFKAwayExistsUser(ctx context.Context, userID id.ID) (bool, error) {
	n, err := r.r.AFKAwayExistsUser(ctx, pgdb.AFKAwayExistsUserParams{UserID: userID})
	return n > 0, wrap(err)
}

func (r *Repo) ListAFKAwayFirst(ctx context.Context, communityID id.ID, limit int32) ([]store.AFKAway, error) {
	rows, err := r.r.ListAFKAwayFirst(ctx, pgdb.ListAFKAwayFirstParams{CommunityID: communityID, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.AFKAway, len(rows))
	for i, row := range rows {
		out[i] = store.AFKAway{UserID: row.UserID, Since: row.Since}
	}
	return out, nil
}

func (r *Repo) SetCommunityAFK(ctx context.Context, communityID id.ID, config store.CommunityAFK) error {
	return wrap(r.w.SetCommunityAFK(ctx, pgdb.SetCommunityAFKParams{CommunityID: communityID, ChannelID: config.ChannelID, TimeoutSeconds: int64(config.TimeoutSeconds)}))
}

func (r *Repo) ListCommunities(ctx context.Context, after id.ID, limit int32) ([]store.CommunityRow, error) {
	rows, err := r.r.ListCommunities(ctx, pgdb.ListCommunitiesParams{ID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.CommunityRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.CommunityRow{
			ID:                   row.ID,
			Owner:                row.Owner,
			Name:                 row.Name,
			IconBlob:             row.IconBlob,
			PolicyJSON:           []byte(row.PolicyJson),
			PolicyVersion:        uint64(row.PolicyVersion),
			MinAccountAgeSeconds: uint64(row.MinAccountAgeSeconds),
			RequireMod2FA:        uint8(row.RequireMod2fa),
			Created:              row.Created,
			DeletedAt:            ptrInt64(row.DeletedAt),
		})
	}
	return out, nil
}

// ListCommunitiesForUser is GET /v1/communities (L-SQL-02): the live communities of userID.
func (r *Repo) ListCommunitiesForUser(ctx context.Context, userID id.ID) ([]store.CommunityRow, error) {
	rows, err := r.r.ListCommunitiesForUser(ctx, pgdb.ListCommunitiesForUserParams{UserID: userID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.CommunityRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.CommunityRow{
			ID:                   row.ID,
			Owner:                row.Owner,
			Name:                 row.Name,
			IconBlob:             row.IconBlob,
			PolicyJSON:           []byte(row.PolicyJson),
			PolicyVersion:        uint64(row.PolicyVersion),
			MinAccountAgeSeconds: uint64(row.MinAccountAgeSeconds),
			RequireMod2FA:        uint8(row.RequireMod2fa),
			Created:              row.Created,
			DeletedAt:            ptrInt64(row.DeletedAt),
		})
	}
	return out, nil
}

func (r *Repo) UpdateCommunityPolicy(ctx context.Context, communityID id.ID, policy []byte, version int64) error {
	n, err := r.w.UpdateCommunityPolicy(ctx, pgdb.UpdateCommunityPolicyParams{
		PolicyJson:    string(policy),
		PolicyVersion: version,
		ID:            communityID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 1 {
		return nil
	}
	// Zero rows: an unknown or deleted community, or a stored version at or
	// above the one offered. Under READ COMMITTED a concurrent writer's UPDATE
	// re-evaluates the guard against the committed row, so the loser lands here.
	if _, err := r.w.GetCommunity(ctx, pgdb.GetCommunityParams{ID: communityID}); err != nil {
		return wrap(err)
	}
	return fmt.Errorf("%w: community %s is already at or past policy version %d", store.ErrConflict, communityID, version)
}

func (r *Repo) UpdateCommunityMeta(ctx context.Context, communityID id.ID, name string, minAge uint64, requireMod2FA uint8) error {
	n, err := r.w.UpdateCommunityMeta(ctx, pgdb.UpdateCommunityMetaParams{
		Name:                 name,
		MinAccountAgeSeconds: int64(minAge),
		RequireMod2fa:        int64(requireMod2FA),
		ID:                   communityID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) SoftDeleteCommunity(ctx context.Context, communityID id.ID, at int64) error {
	n, err := r.w.SoftDeleteCommunity(ctx, pgdb.SoftDeleteCommunityParams{
		DeletedAt: sql.NullInt64{Int64: at, Valid: true},
		ID:        communityID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) SetCommunityOwner(ctx context.Context, communityID, owner id.ID) error {
	return wrap(r.w.SetCommunityOwner(ctx, pgdb.SetCommunityOwnerParams{Owner: owner, ID: communityID}))
}

// LockCommunity takes the community row FOR NO KEY UPDATE until the
// transaction ends. Outside a transaction the lock would be released with the
// statement, so it is refused there.
func (r *Repo) LockCommunity(ctx context.Context, communityID id.ID) error {
	if !r.inTx {
		return errors.New("store: LockCommunity outside a transaction")
	}
	_, err := r.w.LockCommunity(ctx, pgdb.LockCommunityParams{ID: communityID})
	return wrap(err)
}

func (r *Repo) PutMember(ctx context.Context, m store.MemberOfCommunityRow) error {
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error { return tx.PutMember(ctx, m) })
	}
	if m.EpisodeID.IsZero() {
		m.EpisodeID = id.New()
	}
	if err := wrap(r.w.PutMember(ctx, pgdb.PutMemberParams{
		CommunityID: m.CommunityID, UserID: m.UserID, Joined: m.Joined, Nick: m.Nick,
	})); err != nil {
		return err
	}
	_, err := r.writeQuery().ExecContext(ctx, `INSERT INTO community_member_episodes(community_id,user_id,episode_id) VALUES($1,$2,$3) ON CONFLICT(community_id,user_id) DO NOTHING`, m.CommunityID, m.UserID, m.EpisodeID)
	return wrap(err)
}

func (r *Repo) writeQuery() interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
} {
	if r.tx != nil {
		return r.tx
	}
	return r.db
}

func (r *Repo) GetMember(ctx context.Context, communityID, userID id.ID) (store.MemberOfCommunityRow, error) {
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = r.db
	if r.tx != nil {
		q = r.tx
	}
	var member store.MemberOfCommunityRow
	err := q.QueryRowContext(ctx, `SELECT m.community_id,m.user_id,e.episode_id,m.joined,m.nick FROM members m JOIN community_member_episodes e USING(community_id,user_id) WHERE m.community_id=$1 AND m.user_id=$2`, communityID, userID).Scan(&member.CommunityID, &member.UserID, &member.EpisodeID, &member.Joined, &member.Nick)
	if err != nil {
		// wrap turns sql.ErrNoRows into store.ErrNotFound, which membership gates use.
		return store.MemberOfCommunityRow{}, wrap(err)
	}
	return member, nil
}

func (r *Repo) DeleteMember(ctx context.Context, communityID, userID id.ID) error {
	n, err := r.w.DeleteMember(ctx, pgdb.DeleteMemberParams{CommunityID: communityID, UserID: userID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetBotInteractionEligibilityRevision returns and row-locks the durable
// revision so a later registration check in the same transaction is fenced
// against concurrent membership, ban, and device-state changes.
func (r *Repo) GetBotInteractionEligibilityRevision(ctx context.Context, communityID, userID id.ID) (uint64, error) {
	if !r.inTx {
		return 0, errors.New("store: GetBotInteractionEligibilityRevision outside a transaction")
	}
	var revision int64
	err := r.tx.QueryRowContext(ctx, `SELECT revision FROM bot_interaction_eligibility_revisions WHERE community_id=$1 AND user_id=$2 FOR UPDATE`, communityID, userID).Scan(&revision)
	if err != nil {
		return 0, wrap(err)
	}
	if revision < 0 {
		return 0, errors.New("store: negative bot interaction eligibility revision")
	}
	return uint64(revision), nil
}

func (r *Repo) ListMembersOfCommunity(ctx context.Context, communityID, after id.ID, limit int32) ([]store.MemberOfCommunityRow, error) {
	rows, err := r.r.ListMembersOfCommunity(ctx, pgdb.ListMembersOfCommunityParams{
		CommunityID: communityID, UserID: after, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.MemberOfCommunityRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.MemberOfCommunityRow{
			CommunityID: row.CommunityID, UserID: row.UserID, Joined: row.Joined, Nick: row.Nick,
		})
	}
	return out, nil
}

func (r *Repo) PutRole(ctx context.Context, role store.RoleRow) error {
	return wrap(r.w.PutRole(ctx, pgdb.PutRoleParams{
		ID: role.ID, CommunityID: role.CommunityID, Name: role.Name,
		Color: int64(role.Color), Position: int64(role.Position),
		Allow: int64(role.Allow), Deny: int64(role.Deny),
		Hoist: int64(role.Hoist), Mentionable: int64(role.Mentionable),
		Created: role.Created,
	}))
}

func roleRow(row pgdb.Roles) store.RoleRow {
	return store.RoleRow{
		ID: row.ID, CommunityID: row.CommunityID, Name: row.Name,
		Color: uint64(row.Color), Position: uint64(row.Position),
		Allow: uint64(row.Allow), Deny: uint64(row.Deny),
		Hoist: uint8(row.Hoist), Mentionable: uint8(row.Mentionable),
		Created: row.Created,
	}
}

func (r *Repo) GetRole(ctx context.Context, roleID id.ID) (store.RoleRow, error) {
	row, err := r.r.GetRole(ctx, pgdb.GetRoleParams{ID: roleID})
	if err != nil {
		return store.RoleRow{}, wrap(err)
	}
	return roleRow(row), nil
}

func (r *Repo) ListRoles(ctx context.Context, communityID id.ID) ([]store.RoleRow, error) {
	rows, err := r.r.ListRoles(ctx, pgdb.ListRolesParams{CommunityID: communityID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.RoleRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, roleRow(row))
	}
	return out, nil
}

func (r *Repo) PutMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error {
	return r.AddMemberRoleGrant(ctx, communityID, userID, roleID, store.RoleGrantManual)
}

func (r *Repo) DeleteMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error {
	return r.RemoveMemberRoleGrant(ctx, communityID, userID, roleID, store.RoleGrantManual)
}

func validRoleGrantSource(source store.RoleGrantSource) bool {
	return source == store.RoleGrantManual || source == store.RoleGrantSelf || source == store.RoleGrantReaction
}

func (r *Repo) AddMemberRoleGrant(ctx context.Context, communityID, userID, roleID id.ID, source store.RoleGrantSource) error {
	if !validRoleGrantSource(source) {
		return fmt.Errorf("store: invalid role grant source %q", source)
	}
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error {
			return tx.AddMemberRoleGrant(ctx, communityID, userID, roleID, source)
		})
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO member_roles(community_id,user_id,role_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, communityID, userID, roleID); err != nil {
		return wrap(err)
	}
	var locked int
	if err := r.tx.QueryRowContext(ctx, `SELECT 1 FROM member_roles WHERE community_id=$1 AND user_id=$2 AND role_id=$3 FOR UPDATE`, communityID, userID, roleID).Scan(&locked); err != nil {
		return wrap(err)
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO member_role_grant_sources(community_id,user_id,role_id,source) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, communityID, userID, roleID, string(source)); err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) RemoveMemberRoleGrant(ctx context.Context, communityID, userID, roleID id.ID, source store.RoleGrantSource) error {
	if !validRoleGrantSource(source) {
		return fmt.Errorf("store: invalid role grant source %q", source)
	}
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error {
			return tx.RemoveMemberRoleGrant(ctx, communityID, userID, roleID, source)
		})
	}
	var locked int
	if err := r.tx.QueryRowContext(ctx, `SELECT 1 FROM member_roles WHERE community_id=$1 AND user_id=$2 AND role_id=$3 FOR UPDATE`, communityID, userID, roleID).Scan(&locked); err != nil {
		return wrap(err)
	}
	res, err := r.tx.ExecContext(ctx, `DELETE FROM member_role_grant_sources WHERE community_id=$1 AND user_id=$2 AND role_id=$3 AND source=$4`, communityID, userID, roleID, string(source))
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	var remaining int64
	if err := r.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM member_role_grant_sources WHERE community_id=$1 AND user_id=$2 AND role_id=$3) + (SELECT COUNT(*) FROM reaction_role_grant_sources WHERE community_id=$1 AND user_id=$2 AND role_id=$3) + (SELECT COUNT(*) FROM bot_join_role_grant_sources WHERE community_id=$1 AND user_id=$2 AND role_id=$3)`, communityID, userID, roleID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		_, err = r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=$1 AND user_id=$2 AND role_id=$3`, communityID, userID, roleID)
		return wrap(err)
	}
	return nil
}

func (r *Repo) RevokeMemberRole(ctx context.Context, communityID, userID, roleID id.ID) error {
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error {
			return tx.RevokeMemberRole(ctx, communityID, userID, roleID)
		})
	}
	var locked int
	if err := r.tx.QueryRowContext(ctx, `SELECT 1 FROM member_roles WHERE community_id=$1 AND user_id=$2 AND role_id=$3 FOR UPDATE`, communityID, userID, roleID).Scan(&locked); err != nil {
		return wrap(err)
	}
	res, err := r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=$1 AND user_id=$2 AND role_id=$3`, communityID, userID, roleID)
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) ListMemberRoles(ctx context.Context, communityID, userID id.ID) ([]id.ID, error) {
	rows, err := r.r.ListMemberRoles(ctx, pgdb.ListMemberRolesParams{
		CommunityID: communityID, UserID: userID,
	})
	if err != nil {
		return nil, wrap(err)
	}
	return rows, nil
}

// ---------------------------------------------------------------- Channels
//
// Plan 2 task 2: the slice of store.Structure whose table 00005_channels.sql
// ships. The other adapter carries the same seven methods with the package name
// changed: settings_json is TEXT on both engines and the three SMALLINT enums
// are pulled back to int64 by sqlc.yaml, so nothing else differs.

func (r *Repo) CreateChannel(ctx context.Context, c store.ChannelRow) error {
	return wrap(r.w.CreateChannel(ctx, pgdb.CreateChannelParams{
		ID:                c.ID,
		CommunityID:       c.CommunityID,
		Kind:              int64(c.Kind),
		Mode:              int64(c.Mode),
		Visibility:        int64(c.Visibility),
		ParentID:          c.ParentID,
		Name:              c.Name,
		Topic:             c.Topic,
		Position:          int64(c.Position),
		SettingsJson:      settingsJSON(c.SettingsJSON),
		HostPolicyVersion: int64(c.HostPolicyVersion),
		SlowmodeSeconds:   int64(c.SlowmodeSeconds),
		Seq:               int64(c.Seq),
		Created:           c.Created,
		DeletedAt:         nullInt64(c.DeletedAt),
	}))
}

// settingsJSON is the stored form of a channel's settings document: `{}` when
// the row carries none, because the column is NOT NULL.
func settingsJSON(b []byte) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func channelRow(row pgdb.Channels) store.ChannelRow {
	return store.ChannelRow{
		ID:                row.ID,
		CommunityID:       row.CommunityID,
		Kind:              uint8(row.Kind),
		Mode:              uint8(row.Mode),
		Visibility:        uint8(row.Visibility),
		ParentID:          row.ParentID,
		Name:              row.Name,
		Topic:             row.Topic,
		Position:          uint64(row.Position),
		SettingsJSON:      []byte(row.SettingsJson),
		HostPolicyVersion: uint64(row.HostPolicyVersion),
		SlowmodeSeconds:   uint64(row.SlowmodeSeconds),
		Seq:               uint64(row.Seq),
		Created:           row.Created,
		DeletedAt:         ptrInt64(row.DeletedAt),
	}
}

func (r *Repo) GetChannel(ctx context.Context, channelID id.ID) (store.ChannelRow, error) {
	row, err := r.r.GetChannel(ctx, pgdb.GetChannelParams{ID: channelID})
	if err != nil {
		return store.ChannelRow{}, wrap(err)
	}
	return channelRow(row), nil
}

func (r *Repo) ListChannels(ctx context.Context, communityID id.ID) ([]store.ChannelRow, error) {
	rows, err := r.r.ListChannels(ctx, pgdb.ListChannelsParams{CommunityID: &communityID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ChannelRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, channelRow(row))
	}
	return out, nil
}

// UpdateChannel deliberately does not write kind, community_id, seq or created:
// a channel's kind and home are immutable, and seq moves only through
// NextChannelSeq.
func (r *Repo) UpdateChannel(ctx context.Context, c store.ChannelRow) error {
	n, err := r.w.UpdateChannel(ctx, pgdb.UpdateChannelParams{
		Mode:              int64(c.Mode),
		Visibility:        int64(c.Visibility),
		ParentID:          c.ParentID,
		Name:              c.Name,
		Topic:             c.Topic,
		Position:          int64(c.Position),
		SettingsJson:      settingsJSON(c.SettingsJSON),
		HostPolicyVersion: int64(c.HostPolicyVersion),
		SlowmodeSeconds:   int64(c.SlowmodeSeconds),
		ID:                c.ID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) DeleteChannel(ctx context.Context, channelID id.ID, at int64) error {
	n, err := r.w.DeleteChannel(ctx, pgdb.DeleteChannelParams{
		DeletedAt: sql.NullInt64{Int64: at, Valid: true},
		ID:        channelID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) DeleteChannelsOfCommunity(ctx context.Context, communityID id.ID, at int64) (int64, error) {
	n, err := r.w.DeleteChannelsOfCommunity(ctx, pgdb.DeleteChannelsOfCommunityParams{
		DeletedAt:   sql.NullInt64{Int64: at, Valid: true},
		CommunityID: &communityID,
	})
	return n, wrap(err)
}

func (r *Repo) NextChannelSeq(ctx context.Context, channelID id.ID) (uint64, error) {
	seq, err := r.w.NextChannelSeq(ctx, pgdb.NextChannelSeqParams{ID: channelID})
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(seq), nil
}

func (r *Repo) DeleteRole(ctx context.Context, communityID, roleID id.ID) error {
	n, err := r.w.DeleteRole(ctx, pgdb.DeleteRoleParams{ID: roleID, CommunityID: communityID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- Overwrites
//
// Plan 2 task 3: the slice of store.Structure whose table 00006_overwrites.sql
// ships. target_kind is SMALLINT on Postgres and pulled back to int64 by
// sqlc.yaml, so both adapters carry the same three methods.

func (r *Repo) PutOverwrite(ctx context.Context, o store.OverwriteRow) error {
	return wrap(r.w.PutOverwrite(ctx, pgdb.PutOverwriteParams{
		ChannelID:  o.ChannelID,
		TargetKind: int64(o.TargetKind),
		TargetID:   o.TargetID,
		Allow:      int64(o.Allow),
		Deny:       int64(o.Deny),
	}))
}

func (r *Repo) ListOverwrites(ctx context.Context, channelID id.ID) ([]store.OverwriteRow, error) {
	rows, err := r.r.ListOverwrites(ctx, pgdb.ListOverwritesParams{ChannelID: channelID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.OverwriteRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.OverwriteRow{
			ChannelID:  row.ChannelID,
			TargetKind: uint8(row.TargetKind),
			TargetID:   row.TargetID,
			Allow:      uint64(row.Allow),
			Deny:       uint64(row.Deny),
		})
	}
	return out, nil
}

func (r *Repo) DeleteOverwrite(ctx context.Context, channelID id.ID, targetKind uint8, targetID id.ID) error {
	n, err := r.w.DeleteOverwrite(ctx, pgdb.DeleteOverwriteParams{
		ChannelID: channelID, TargetKind: int64(targetKind), TargetID: targetID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- Bans
//
// Plan 2 task 4: the slice of store.Structure whose table 00007_bans.sql ships,
// with P2-D10's listing.

func (r *Repo) PutBan(ctx context.Context, b store.BanRow) error {
	return wrap(r.w.PutBan(ctx, pgdb.PutBanParams{
		CommunityID: b.CommunityID,
		UserID:      b.UserID,
		Reason:      b.Reason,
		ByUser:      b.ByUser,
		Created:     b.Created,
		Expires:     nullInt64(b.Expires),
	}))
}

func banRow(row pgdb.Bans) store.BanRow {
	return store.BanRow{
		CommunityID: row.CommunityID,
		UserID:      row.UserID,
		Reason:      row.Reason,
		ByUser:      row.ByUser,
		Created:     row.Created,
		Expires:     ptrInt64(row.Expires),
	}
}

func (r *Repo) GetBan(ctx context.Context, communityID, userID id.ID) (store.BanRow, error) {
	row, err := r.r.GetBan(ctx, pgdb.GetBanParams{CommunityID: communityID, UserID: userID})
	if err != nil {
		return store.BanRow{}, wrap(err)
	}
	return banRow(row), nil
}

func (r *Repo) ListBans(ctx context.Context, communityID id.ID) ([]store.BanRow, error) {
	rows, err := r.r.ListBans(ctx, pgdb.ListBansParams{CommunityID: communityID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BanRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, banRow(row))
	}
	return out, nil
}

func (r *Repo) DeleteBan(ctx context.Context, communityID, userID id.ID) error {
	n, err := r.w.DeleteBan(ctx, pgdb.DeleteBanParams{CommunityID: communityID, UserID: userID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// PutChannelMember keeps the first row of a pair: a second call is a no-op.
func (r *Repo) PutChannelMember(ctx context.Context, channelID, userID id.ID, at int64) error {
	return wrap(r.w.PutChannelMember(ctx, pgdb.PutChannelMemberParams{
		ChannelID: channelID, UserID: userID, Added: at,
	}))
}

func (r *Repo) DeleteUserOverwrites(ctx context.Context, communityID, userID id.ID) (int64, error) {
	n, err := r.w.DeleteUserOverwrites(ctx, pgdb.DeleteUserOverwritesParams{
		UserID: userID, CommunityID: &communityID,
	})
	return n, wrap(err)
}

func (r *Repo) DeleteCommunityChannelMembers(ctx context.Context, communityID, userID id.ID) (int64, error) {
	n, err := r.w.DeleteCommunityChannelMembers(ctx, pgdb.DeleteCommunityChannelMembersParams{
		UserID: userID, CommunityID: &communityID,
	})
	return n, wrap(err)
}

func (r *Repo) DeleteChannelMember(ctx context.Context, channelID, userID id.ID) error {
	n, err := r.w.DeleteChannelMember(ctx, pgdb.DeleteChannelMemberParams{
		ChannelID: channelID, UserID: userID,
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) ListChannelMembers(ctx context.Context, channelID id.ID) ([]id.ID, error) {
	ids, err := r.r.ListChannelMembers(ctx, pgdb.ListChannelMembersParams{ChannelID: channelID})
	if err != nil {
		return nil, wrap(err)
	}
	return ids, nil
}

// ListChannelsForUser is P2-D11: the live DMs and group DMs of userID.
func (r *Repo) ListChannelsForUser(ctx context.Context, userID id.ID) ([]store.ChannelRow, error) {
	rows, err := r.r.ListChannelsForUser(ctx, pgdb.ListChannelsForUserParams{UserID: userID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ChannelRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, channelRow(row))
	}
	return out, nil
}

// QueuePendingJoins is the pending-join queue's write (Plan 1 follow-up card 8): every device of
// the batch in one transaction, so a storm is queued whole or not at all.
func (r *Repo) QueuePendingJoins(ctx context.Context, groupID id.ID, devices []id.ID, at int64) error {
	if len(devices) == 0 {
		return nil
	}
	return wrap(r.atomically(ctx, func(q *pgdb.Queries) error {
		for _, d := range devices {
			if err := q.QueuePendingJoin(ctx, pgdb.QueuePendingJoinParams{
				GroupID: groupID, DeviceID: d, Queued: at,
			}); err != nil {
				return err
			}
		}
		return nil
	}))
}

// ListPendingJoins reads the oldest `limit` devices and removes nothing: a device leaves the queue
// through DeletePendingJoins once the drain has resolved it, never when it is read.
func (r *Repo) ListPendingJoins(ctx context.Context, groupID id.ID, limit int32) ([]id.ID, error) {
	devices, err := r.r.ListPendingJoins(ctx, pgdb.ListPendingJoinsParams{GroupID: groupID, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	return devices, nil
}

// DeletePendingJoins removes the named devices from the group's queue, in one transaction. A
// device that is not queued is not an error.
func (r *Repo) DeletePendingJoins(ctx context.Context, groupID id.ID, devices []id.ID) error {
	if len(devices) == 0 {
		return nil
	}
	return wrap(r.atomically(ctx, func(q *pgdb.Queries) error {
		for _, d := range devices {
			if err := q.DeletePendingJoin(ctx, pgdb.DeletePendingJoinParams{GroupID: groupID, DeviceID: d}); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (r *Repo) CountPendingJoins(ctx context.Context, groupID id.ID) (int64, error) {
	n, err := r.r.CountPendingJoins(ctx, pgdb.CountPendingJoinsParams{GroupID: groupID})
	return n, wrap(err)
}

func (r *Repo) ListPendingJoinGroups(ctx context.Context, after id.ID, limit int32) ([]id.ID, error) {
	ids, err := r.r.ListPendingJoinGroups(ctx, pgdb.ListPendingJoinGroupsParams{GroupID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	return ids, nil
}

// ---------------------------------------------------------------- Readable
//
// 007_readable.sql (Plan 2 task 8). SearchReadable is hand-written in search.go.

// PutReadableMessage ignores m.ID: the identity column is the store's to assign.
func (r *Repo) PutReadableMessage(ctx context.Context, m store.ReadableMessageRow) (int64, error) {
	rowID, err := r.w.PutReadableMessage(ctx, pgdb.PutReadableMessageParams{
		ChannelID:      m.ChannelID,
		ChannelHex:     m.ChannelHex,
		Seq:            int64(m.Seq),
		Sender:         m.Sender,
		Envelope:       m.Envelope,
		Body:           m.Body,
		FrankingTag:    m.FrankingTag,
		FrankingKeyID:  m.FrankingKeyID,
		MentionCount:   int64(m.MentionCount),
		Created:        m.Created,
		Edited:         nullInt64(m.Edited),
		Deleted:        nullInt64(m.Deleted),
		UploaderDevice: m.UploaderDevice,
		CommitmentC:    m.CommitmentC,
	})
	if err != nil {
		return 0, wrap(err)
	}
	return rowID, nil
}

func (r *Repo) ListReadableMessages(ctx context.Context, channelID id.ID, fromSeq uint64, limit int32) ([]store.ReadableMessageRow, error) {
	fromSeqInt, err := checkedInt64(fromSeq, "readable-message sequence")
	if err != nil {
		return nil, err
	}

	rows, err := r.r.ListReadableMessages(ctx, pgdb.ListReadableMessagesParams{
		ChannelID: channelID,
		Seq:       fromSeqInt,
		MaxRows:   int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.ReadableMessageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.ReadableMessageRow{
			ID:             row.ID,
			ChannelID:      row.ChannelID,
			ChannelHex:     row.ChannelHex,
			Seq:            uint64(row.Seq),
			Sender:         row.Sender,
			Envelope:       row.Envelope,
			Body:           row.Body,
			FrankingTag:    row.FrankingTag,
			FrankingKeyID:  row.FrankingKeyID,
			MentionCount:   uint64(row.MentionCount),
			Created:        row.Created,
			Edited:         ptrInt64(row.Edited),
			Deleted:        ptrInt64(row.Deleted),
			UploaderDevice: row.UploaderDevice,
			CommitmentC:    row.CommitmentC,
		})
	}
	return out, nil
}

func (r *Repo) EditReadableMessage(ctx context.Context, channelID id.ID, seq uint64,
	envelope []byte, body string, f store.ReadableFranking, at int64,
) error {
	n, err := r.w.EditReadableMessage(ctx, pgdb.EditReadableMessageParams{
		Envelope:       envelope,
		Body:           body,
		FrankingTag:    f.Tag,
		FrankingKeyID:  f.KeyID,
		UploaderDevice: f.UploaderDevice,
		CommitmentC:    f.CommitmentC,
		Edited:         sql.NullInt64{Int64: at, Valid: true},
		ChannelID:      channelID,
		Seq:            int64(seq),
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) DeleteReadableMessage(ctx context.Context, channelID id.ID, seq uint64, at int64) error {
	n, err := r.w.DeleteReadableMessage(ctx, pgdb.DeleteReadableMessageParams{
		Deleted:   sql.NullInt64{Int64: at, Valid: true},
		ChannelID: channelID,
		Seq:       int64(seq),
	})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// PutReadState is monotone in SQL (`GREATEST(stored, new)`).
func (r *Repo) PutReadState(ctx context.Context, userID, channelID id.ID, lastReadSeq uint64) error {
	return wrap(r.w.PutReadState(ctx, pgdb.PutReadStateParams{
		UserID:      userID,
		ChannelID:   channelID,
		LastReadSeq: int64(lastReadSeq),
	}))
}

func (r *Repo) GetReadState(ctx context.Context, userID, channelID id.ID) (uint64, error) {
	seq, err := r.r.GetReadState(ctx, pgdb.GetReadStateParams{UserID: userID, ChannelID: channelID})
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(seq), nil
}

func (r *Repo) LastReadableMessageAt(ctx context.Context, channelID, userID id.ID) (int64, error) {
	at, err := r.r.LastReadableMessageAt(ctx, pgdb.LastReadableMessageAtParams{ChannelID: channelID, Sender: userID})
	if err != nil {
		return 0, wrap(err)
	}
	return at, nil
}

func (r *Repo) LastClassZeroUploadAt(ctx context.Context, channelID, userID id.ID) (int64, error) {
	at, err := r.r.LastClassZeroUploadAt(ctx, pgdb.LastClassZeroUploadAtParams{ChannelID: channelID, UserID: userID})
	if err != nil {
		return 0, wrap(err)
	}
	return at, nil
}

func (r *Repo) PutSlowmodeMark(ctx context.Context, channelID, userID id.ID, at int64) error {
	return wrap(r.w.PutSlowmodeMark(ctx, pgdb.PutSlowmodeMarkParams{
		ChannelID: channelID, UserID: userID, LastPostAt: at,
	}))
}

func (r *Repo) ListReadableAudience(ctx context.Context, channelID id.ID) ([]id.ID, error) {
	ids, err := r.r.ListReadableAudience(ctx, pgdb.ListReadableAudienceParams{ChannelID: channelID})
	if err != nil {
		return nil, wrap(err)
	}
	return ids, nil
}

// ---------------------------------------------------------------- Blobs
//
// Plan 2 task 10: 00010_blobs.sql's blobs, blob_refs and blob_tombstones, with
// P2-D16's ClearBlobUnreferenced and P2-D17's GetBlobRef.

func blobRow(row pgdb.Blobs) store.BlobRow {
	return store.BlobRow{
		BlobID:     row.BlobID,
		Size:       uint64(row.Size),
		StorageRef: row.StorageRef,
		Created:    row.Created,
		UnrefSince: ptrInt64(row.UnrefSince),
	}
}

func (r *Repo) PutBlob(ctx context.Context, b store.BlobRow) error {
	return wrap(r.w.PutBlob(ctx, pgdb.PutBlobParams{
		BlobID:     b.BlobID,
		Size:       int64(b.Size),
		StorageRef: b.StorageRef,
		Created:    b.Created,
		UnrefSince: nullInt64(b.UnrefSince),
	}))
}

func (r *Repo) GetBlob(ctx context.Context, blobID []byte) (store.BlobRow, error) {
	row, err := r.r.GetBlob(ctx, pgdb.GetBlobParams{BlobID: blobID})
	if err != nil {
		return store.BlobRow{}, wrap(err)
	}
	return blobRow(row), nil
}

func (r *Repo) PutBlobRef(ctx context.Context, blobID []byte, channelID, uploaderDevice id.ID, mime string, created int64) error {
	return wrap(r.w.PutBlobRef(ctx, pgdb.PutBlobRefParams{
		BlobID:         blobID,
		ChannelID:      channelID,
		UploaderDevice: uploaderDevice,
		Mime:           mime,
		Created:        created,
	}))
}

func (r *Repo) PutPendingBlobRef(ctx context.Context, blobID []byte, channelID, uploaderDevice id.ID, mime string, created int64) error {
	return wrap(r.w.PutPendingBlobRef(ctx, pgdb.PutPendingBlobRefParams{
		BlobID: blobID, ChannelID: channelID, UploaderDevice: uploaderDevice,
		Mime: mime, Created: created,
	}))
}

func (r *Repo) ConfirmBlobRef(ctx context.Context, blobID []byte, channelID id.ID) error {
	n, err := r.w.ConfirmBlobRef(ctx, pgdb.ConfirmBlobRefParams{BlobID: blobID, ChannelID: channelID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) GetBlobRef(ctx context.Context, blobID []byte, channelID id.ID) (store.BlobRefRow, error) {
	row, err := r.r.GetBlobRef(ctx, pgdb.GetBlobRefParams{BlobID: blobID, ChannelID: channelID})
	if err != nil {
		return store.BlobRefRow{}, wrap(err)
	}
	return store.BlobRefRow{
		BlobID:         row.BlobID,
		ChannelID:      row.ChannelID,
		UploaderDevice: row.UploaderDevice,
		Mime:           row.Mime,
		Created:        row.Created,
		Confirmed:      row.Confirmed == 1,
	}, nil
}

func (r *Repo) DeleteBlobRef(ctx context.Context, blobID []byte, channelID id.ID) error {
	return wrap(r.w.DeleteBlobRef(ctx, pgdb.DeleteBlobRefParams{BlobID: blobID, ChannelID: channelID}))
}

func (r *Repo) CountBlobRefs(ctx context.Context, blobID []byte) (int64, error) {
	n, err := r.r.CountBlobRefs(ctx, pgdb.CountBlobRefsParams{BlobID: blobID})
	return n, wrap(err)
}

func (r *Repo) MarkBlobUnreferenced(ctx context.Context, blobID []byte, at int64) error {
	return wrap(r.w.MarkBlobUnreferenced(ctx, pgdb.MarkBlobUnreferencedParams{
		At:     sql.NullInt64{Int64: at, Valid: true},
		BlobID: blobID,
	}))
}

func (r *Repo) ClearBlobUnreferenced(ctx context.Context, blobID []byte) error {
	return wrap(r.w.ClearBlobUnreferenced(ctx, pgdb.ClearBlobUnreferencedParams{BlobID: blobID}))
}

func (r *Repo) ListCollectableBlobs(ctx context.Context, before int64, limit int32) ([]store.BlobRow, error) {
	rows, err := r.r.ListCollectableBlobs(ctx, pgdb.ListCollectableBlobsParams{Before: before, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BlobRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, blobRow(row))
	}
	return out, nil
}

// ListBlobs passes an empty bytea for a nil after: database/sql sends a nil
// []byte as NULL, and `blob_id > NULL` selects nothing.
func (r *Repo) ListBlobs(ctx context.Context, after []byte, limit int32) ([]store.BlobRow, error) {
	if after == nil {
		after = []byte{}
	}
	rows, err := r.r.ListBlobs(ctx, pgdb.ListBlobsParams{After: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BlobRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, blobRow(row))
	}
	return out, nil
}

func (r *Repo) DeleteBlob(ctx context.Context, blobID []byte) error {
	n, err := r.w.DeleteBlob(ctx, pgdb.DeleteBlobParams{BlobID: blobID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) PutBlobTombstone(ctx context.Context, blobID []byte, reason string, by id.ID, at int64) error {
	return wrap(r.w.PutBlobTombstone(ctx, pgdb.PutBlobTombstoneParams{
		BlobID:  blobID,
		Reason:  reason,
		ByUser:  by,
		Created: at,
	}))
}

func (r *Repo) GetBlobTombstone(ctx context.Context, blobID []byte) (bool, error) {
	n, err := r.r.GetBlobTombstone(ctx, pgdb.GetBlobTombstoneParams{BlobID: blobID})
	if err != nil {
		return false, wrap(err)
	}
	return n > 0, nil
}

func (r *Repo) UserBlobBytes(ctx context.Context, userID id.ID) (int64, error) {
	n, err := r.r.UserBlobBytes(ctx, pgdb.UserBlobBytesParams{UserID: userID})
	return n, wrap(err)
}

func (r *Repo) UserReferencesBlob(ctx context.Context, userID id.ID, blobID []byte) (bool, error) {
	n, err := r.r.UserReferencesBlob(ctx, pgdb.UserReferencesBlobParams{BlobID: blobID, UserID: userID})
	if err != nil {
		return false, wrap(err)
	}
	return n > 0, nil
}

func (r *Repo) InstanceBlobBytes(ctx context.Context) (int64, error) {
	n, err := r.r.InstanceBlobBytes(ctx)
	return n, wrap(err)
}

func (r *Repo) DeleteAllBlobRefs(ctx context.Context, blobID []byte) (int64, error) {
	n, err := r.w.DeleteAllBlobRefs(ctx, pgdb.DeleteAllBlobRefsParams{BlobID: blobID})
	return n, wrap(err)
}

func (r *Repo) ListBlobRetentionPolicies(ctx context.Context) ([]store.BlobRetentionRow, error) {
	rows, err := r.r.ListBlobRetentionPolicies(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BlobRetentionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.BlobRetentionRow{CommunityID: row.ID, PolicyJSON: []byte(row.PolicyJson)})
	}
	return out, nil
}

func (r *Repo) ListExpiredBlobRefs(ctx context.Context, communityID id.ID, before int64, limit int32) ([]store.BlobRefRow, error) {
	rows, err := r.r.ListExpiredBlobRefs(ctx, pgdb.ListExpiredBlobRefsParams{
		CommunityID: communityID, Before: before, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	return blobRefRows(rows), nil
}

func (r *Repo) ListBlobRefsOfDeletedChannels(ctx context.Context, limit int32) ([]store.BlobRefRow, error) {
	rows, err := r.r.ListBlobRefsOfDeletedChannels(ctx, pgdb.ListBlobRefsOfDeletedChannelsParams{MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	return blobRefRows(rows), nil
}

func (r *Repo) ListPendingBlobRefs(ctx context.Context, before int64, limit int32) ([]store.BlobRefRow, error) {
	rows, err := r.r.ListPendingBlobRefs(ctx, pgdb.ListPendingBlobRefsParams{Before: before, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	return blobRefRows(rows), nil
}

func blobRefRows(rows []pgdb.BlobRefs) []store.BlobRefRow {
	out := make([]store.BlobRefRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.BlobRefRow{
			BlobID:         row.BlobID,
			ChannelID:      row.ChannelID,
			UploaderDevice: row.UploaderDevice,
			Mime:           row.Mime,
			Created:        row.Created,
			Confirmed:      row.Confirmed == 1,
		})
	}
	return out
}

// ---------------------------------------------------------------- OpsBackups
//
// Plan 2 task 10 (P2-D5): the backups table ships in 00010_blobs.sql, so its
// two methods land here rather than with task 12's instance archive.

func (r *Repo) PutBackup(ctx context.Context, b store.BackupRow) error {
	return wrap(r.w.PutBackup(ctx, pgdb.PutBackupParams{
		UserID:      b.UserID,
		Kind:        int64(b.Kind),
		DeviceID:    b.DeviceID,
		ChunkSeq:    int64(b.ChunkSeq),
		BlobID:      b.BlobID,
		ManifestSig: b.ManifestSig,
		Created:     b.Created,
	}))
}

func (r *Repo) ListBackups(ctx context.Context, userID id.ID, kind int32) ([]store.BackupRow, error) {
	rows, err := r.r.ListBackups(ctx, pgdb.ListBackupsParams{UserID: userID, Kind: int64(kind)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BackupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.BackupRow{
			UserID:      row.UserID,
			Kind:        uint64(row.Kind),
			DeviceID:    row.DeviceID,
			ChunkSeq:    uint64(row.ChunkSeq),
			BlobID:      row.BlobID,
			ManifestSig: row.ManifestSig,
			Created:     row.Created,
		})
	}
	return out, nil
}

func (r *Repo) InsertBackup(ctx context.Context, b store.BackupRow) error {
	return wrap(r.w.InsertBackup(ctx, pgdb.InsertBackupParams{UserID: b.UserID, Kind: int64(b.Kind), DeviceID: b.DeviceID, ChunkSeq: int64(b.ChunkSeq), BlobID: b.BlobID, ManifestSig: b.ManifestSig, Created: b.Created}))
}

func (r *Repo) GetBackup(ctx context.Context, userID id.ID, kind int32, deviceID id.ID, chunkSeq int64) (store.BackupRow, error) {
	row, err := r.r.GetBackup(ctx, pgdb.GetBackupParams{UserID: userID, Kind: int64(kind), DeviceID: deviceID, ChunkSeq: chunkSeq})
	if err != nil {
		return store.BackupRow{}, wrap(err)
	}
	return store.BackupRow{UserID: row.UserID, Kind: uint64(row.Kind), DeviceID: row.DeviceID, ChunkSeq: uint64(row.ChunkSeq), BlobID: row.BlobID, ManifestSig: row.ManifestSig, Created: row.Created}, nil
}

func (r *Repo) LockBlob(ctx context.Context, blobID []byte) error {
	if !r.inTx {
		return errors.New("store: the blob lock requires Tx")
	}
	return wrap(r.w.LockBlob(ctx, pgdb.LockBlobParams{BlobID: blobID}))
}

func (r *Repo) BackupRefersToBlob(ctx context.Context, blobID []byte) (bool, error) {
	n, err := r.r.BackupRefersToBlob(ctx, pgdb.BackupRefersToBlobParams{BlobID: blobID})
	if err != nil {
		return false, wrap(err)
	}
	return n > 0, nil
}

// Voice sessions (Plan 2 task 16, P2-D22, 00011_voice.sql).

func voiceSessionRow(row pgdb.VoiceSessions) store.VoiceSessionRow {
	return store.VoiceSessionRow{
		CallID:      row.CallID,
		ChannelID:   row.ChannelID,
		GroupID:     row.GroupID,
		LivekitRoom: row.LivekitRoom,
		Started:     row.Started,
		Ended:       ptrInt64(row.Ended),
	}
}

// PutVoiceSession records a call, or reopens an ended one: a call is keyed by its call group's
// call id (R9), so the next call of the same group rewrites the row and clears ended. A live row
// is left as it is.
func (r *Repo) PutVoiceSession(ctx context.Context, v store.VoiceSessionRow) error {
	return wrap(r.w.PutVoiceSession(ctx, pgdb.PutVoiceSessionParams{
		CallID:      v.CallID,
		ChannelID:   v.ChannelID,
		GroupID:     v.GroupID,
		LivekitRoom: v.LivekitRoom,
		Started:     v.Started,
	}))
}

func (r *Repo) GetVoiceSession(ctx context.Context, callID id.ID) (store.VoiceSessionRow, error) {
	row, err := r.r.GetVoiceSession(ctx, pgdb.GetVoiceSessionParams{CallID: callID})
	if err != nil {
		return store.VoiceSessionRow{}, wrap(err)
	}
	return voiceSessionRow(row), nil
}

// EndVoiceSession ends a live call; ErrNotFound when there is no such call or it has ended.
func (r *Repo) EndVoiceSession(ctx context.Context, callID id.ID, at int64) error {
	n, err := r.w.EndVoiceSession(ctx, pgdb.EndVoiceSessionParams{At: at, CallID: callID})
	if err != nil {
		return wrap(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) ListLiveVoiceSessions(ctx context.Context, channelID id.ID) ([]store.VoiceSessionRow, error) {
	rows, err := r.r.ListLiveVoiceSessions(ctx, pgdb.ListLiveVoiceSessionsParams{ChannelID: channelID})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.VoiceSessionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, voiceSessionRow(row))
	}
	return out, nil
}

var _ store.Repository = (*Repo)(nil)

// PutMemberTimeout persists a member timeout.
func (r *Repo) PutMemberTimeout(ctx context.Context, v store.MemberTimeoutRow) error {
	_, err := r.writeQuery().ExecContext(ctx, `INSERT INTO community_member_timeouts(community_id,user_id,by_user,created,expires) VALUES($1,$2,$3,$4,$5) ON CONFLICT(community_id,user_id) DO UPDATE SET by_user=excluded.by_user,created=excluded.created,expires=excluded.expires`, v.CommunityID, v.UserID, v.ByUser, v.Created, v.Expires)
	return wrap(err)
}
func (r *Repo) GetMemberTimeout(ctx context.Context, cid, uid id.ID, now int64) (store.MemberTimeoutRow, error) {
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = r.db
	if r.tx != nil {
		q = r.tx
	}
	var v store.MemberTimeoutRow
	err := q.QueryRowContext(ctx, `SELECT community_id,user_id,by_user,created,expires FROM community_member_timeouts WHERE community_id=$1 AND user_id=$2 AND expires>$3`, cid, uid, now).Scan(&v.CommunityID, &v.UserID, &v.ByUser, &v.Created, &v.Expires)
	if err != nil {
		return v, wrap(err)
	}
	return v, nil
}
func (r *Repo) DeleteMemberTimeout(ctx context.Context, cid, uid id.ID) error {
	res, err := r.writeQuery().ExecContext(ctx, `DELETE FROM community_member_timeouts WHERE community_id=$1 AND user_id=$2`, cid, uid)
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
