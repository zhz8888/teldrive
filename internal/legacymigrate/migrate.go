// Package legacymigrate upgrades a v1.x TelDrive database to the v2 layout
// in place.
//
// The legacy database keeps everything in the "teldrive" schema and records its
// migration history in public.goose_db_version. The upgrade builds a complete v2
// schema under a staging name, copies and rewrites every row into it, and only
// then swaps schemas inside one transaction: the legacy schema is renamed to a
// timestamped backup and the staging schema is renamed into place. A failure
// before the swap drops the staging schema and leaves the legacy database
// untouched, so the operation is safe to retry.
package legacymigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tgdrive/teldrive/v2/internal/bots"
	"github.com/tgdrive/teldrive/v2/internal/database"
	"github.com/tgdrive/teldrive/v2/internal/secureblob"
	"github.com/tgdrive/teldrive/v2/internal/treehash"
)

// Config describes one migration run.
//
// SourceURL and Target.URL must point at the same PostgreSQL database: the
// upgrade rewrites the schema in place rather than copying between servers. Both
// LegacySchema and FinalSchema must be database.DefaultSchema, because the legacy
// data hard-codes that name, and the staging schema in Target.Schema must differ
// from the legacy, final, and backup schemas.
type Config struct {
	// SourceURL is the database holding the legacy schema.
	SourceURL string

	// Target describes the staging database and schema that the v2 tables are
	// built in before the swap.
	Target database.Config

	// LegacySchema is the schema the legacy tables are read from.
	LegacySchema string

	// FinalSchema is the schema the staging schema is promoted to.
	FinalSchema string

	// BackupSchema is the schema the legacy data is renamed to, so an operator can
	// still inspect or roll back to it after a successful migration.
	BackupSchema string

	// DataKey is the base64 secureblob key used to encrypt migrated bot tokens.
	DataKey string

	// EncryptionKeyVersion is recorded on every migrated encrypted file. It must
	// be positive.
	EncryptionKeyVersion int

	// Apply selects a real migration. When false, Run only inspects the source and
	// reports what would be migrated without writing anything.
	Apply bool

	// BotVerifier resolves duplicate bot tokens against Telegram. It is required
	// only when the legacy data contains two tokens for the same bot; without it
	// such a duplicate aborts the migration.
	BotVerifier bots.Verifier
}

// Report summarises what a migration found, and for an applied migration what it
// wrote. The counters describe the legacy input, except BackupSchema which names
// the schema the legacy data was moved to.
type Report struct {
	// Users is the number of legacy user rows.
	Users int64

	// Channels is the number of legacy channel rows.
	Channels int64

	// Bots is the number of distinct (user, bot) pairs.
	Bots int64

	// Files is the number of legacy rows that are not folders.
	Files int64

	// Folders is the number of legacy folders, after synthetic drive roots have
	// been flattened away.
	Folders int64

	// FileParts is the number of Telegram parts referenced by migrated files.
	FileParts int64

	// Encrypted is the number of migrated files that carry per-part salts.
	Encrypted int64

	// SkippedZero counts zero-byte files with no parts. They are migrated as empty
	// files rather than rejected, because that is a valid state in the legacy data.
	SkippedZero int64

	// BackupSchema is the schema the legacy data was renamed to. It is set only for
	// an applied migration.
	BackupSchema string
}

// migrationLockID is the advisory lock key that serialises migration detection
// across processes. Its value is the ASCII string "TELDRIVE", chosen only to be
// recognisable in pg_locks.
const migrationLockID int64 = 0x54454c4452495645

// MigrateIfNeeded upgrades the database described by cfg when, and only when, it
// still looks like a legacy TelDrive database, and reports whether it did.
//
// Detection is the presence of public.goose_db_version, which the v2 schema does
// not use. The whole check runs under a session advisory lock so two starting
// instances cannot migrate concurrently, and the caller must supply a data key
// because migrated bot tokens are encrypted with it.
//
// A legacy database with a non-default schema is rejected rather than migrated,
// and the staging and backup schemas are named after the current timestamp so a
// failed attempt never collides with its own leftovers.
func MigrateIfNeeded(ctx context.Context, cfg database.Config, dataKey string, verifier bots.Verifier) (Report, bool, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return Report{}, false, errors.New("database URL is required")
	}
	conn, err := pgx.Connect(ctx, cfg.URL)
	if err != nil {
		return Report{}, false, fmt.Errorf("connect for legacy migration detection: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return Report{}, false, fmt.Errorf("acquire legacy migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	var legacy bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.goose_db_version') IS NOT NULL`).Scan(&legacy); err != nil {
		return Report{}, false, fmt.Errorf("inspect legacy migration table: %w", err)
	}
	if !legacy {
		return Report{}, false, nil
	}
	if strings.TrimSpace(dataKey) == "" {
		return Report{}, false, errors.New("legacy database detected but security.data-key is empty; set security.data-key or TELDRIVE_SECURITY_DATA_KEY before starting Teldrive")
	}
	if cfg.Schema != "" && cfg.Schema != database.DefaultSchema {
		return Report{}, false, fmt.Errorf("legacy database migration requires database.schema=%q", database.DefaultSchema)
	}
	// The staging and backup schema names carry the start time. The nanosecond
	// field is appended explicitly because Go only reads a fractional second from
	// a layout that contains a decimal point, so the nine zeros this used to carry
	// were literal text and two runs started in the same second shared one name.
	// The cleanup of a failed attempt is best effort, so a leftover schema would
	// make the retry fail on a name that already exists.
	now := time.Now().UTC()
	suffix := fmt.Sprintf("%s_%09d", now.Format("20060102_150405"), now.Nanosecond())
	report, err := Run(ctx, Config{
		SourceURL: cfg.URL,
		Target: database.Config{
			URL:    cfg.URL,
			Schema: database.DefaultSchema + "_v2_staging_" + suffix,
		},
		LegacySchema:         database.DefaultSchema,
		FinalSchema:          database.DefaultSchema,
		BackupSchema:         database.DefaultSchema + "_legacy_backup_" + suffix,
		DataKey:              dataKey,
		EncryptionKeyVersion: 1,
		BotVerifier:          verifier,
		Apply:                true,
	})
	if err != nil {
		return Report{}, false, err
	}
	report.BackupSchema = database.DefaultSchema + "_legacy_backup_" + suffix
	return report, true, nil
}

// legacyPart is one entry of a legacy file's parts JSON column. Salt is present
// only for encrypted files and is carried over verbatim so the per-part key
// derivation still works after migration.
type legacyPart struct {
	// ID is the Telegram message ID holding the part.
	ID int64 `json:"id"`

	// Salt is the per-part key salt for encrypted files, empty otherwise.
	Salt string `json:"salt,omitempty"`
}

// legacyFile is one row of the legacy teldrive.files table. Its fields keep the
// legacy column names and types; the conversion to the v2 shape happens in
// migrateFiles.
type legacyFile struct {
	// ID is the file UUID and stays the primary key across the migration.
	ID uuid.UUID

	// Name is the display name; it is not normalised during migration.
	Name string

	// Kind is "folder" or "file" in the legacy data, and is copied verbatim.
	Kind string

	// MimeType is the legacy content type. Folders use "drive/folder".
	MimeType string

	// Size is the file size in bytes. It is a pointer because the legacy column is
	// nullable for folders, and it must be non-negative for files.
	Size *int64

	// UserID is the owning Telegram user ID.
	UserID int64

	// ParentID points at the containing folder, or nil for a top-level entry.
	ParentID *uuid.UUID

	// Status is the legacy lifecycle value. Anything other than "active" becomes
	// deletion_pending in v2.
	Status string

	// ChannelID is the Telegram channel holding the file's parts. It may be nil for
	// folders and for zero-byte files.
	ChannelID *int64

	// Parts lists the Telegram messages that make up the file content, in order.
	Parts []legacyPart

	// Encrypted reports whether the content was encrypted by the v1 server, in
	// which case every part must carry a salt.
	Encrypted bool

	// Hash is the legacy content hash, if the v1 server recorded one. It is stored
	// as a blake3-tree hash value in v2.
	Hash *string

	// CreatedAt is the original creation time, preserved so ordering and display
	// stay stable after migration.
	CreatedAt time.Time

	// UpdatedAt is the original modification time. For non-active entries it also
	// becomes the v2 deletion timestamp.
	UpdatedAt time.Time
}

// legacyBot is one row of the legacy teldrive.bots table.
type legacyBot struct {
	// UserID is the owner of the bot token.
	UserID int64

	// Token is the plaintext bot token. It is encrypted with the configured data
	// key before being written to the v2 table.
	Token string

	// BotID is the Telegram bot identity the token belongs to.
	BotID int64
}

// legacyReader is the subset of pgx used to read the legacy database. It lets the
// same queries run on a plain connection during detection and inside the locking
// transaction during the migration itself.
type legacyReader interface {
	// Query runs a statement returning rows. The caller owns the returned rows and
	// must close them.
	Query(context.Context, string, ...any) (pgx.Rows, error)

	// QueryRow runs a statement expected to return at most one row, whose error is
	// surfaced by Scan rather than by this call.
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Run performs one migration, or one dry inspection when cfg.Apply is false.
//
// The applied path is deliberately ordered: both schemas are validated, the
// legacy tables are locked in ACCESS EXCLUSIVE mode for the whole run so no v1
// writer can add rows mid-copy, the v2 tables are built in a staging schema, and
// only then are the two schemas swapped and committed together. Until that
// commit the legacy database is untouched; afterwards the original data is still
// available under cfg.BackupSchema.
//
// A copy failure drops the staging schema in a fresh, bounded context so cleanup
// still happens after the caller's context is cancelled.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if strings.TrimSpace(cfg.SourceURL) == "" || strings.TrimSpace(cfg.Target.URL) == "" {
		return Report{}, errors.New("database URL is required")
	}
	if cfg.SourceURL != cfg.Target.URL {
		return Report{}, errors.New("legacy migration must use one PostgreSQL database")
	}
	cipher, err := secureblob.New(cfg.DataKey)
	if err != nil {
		return Report{}, fmt.Errorf("initialize data-key cipher: %w", err)
	}
	if cfg.EncryptionKeyVersion <= 0 {
		return Report{}, errors.New("encryption key version must be greater than zero")
	}

	source, err := pgx.Connect(ctx, cfg.SourceURL)
	if err != nil {
		return Report{}, fmt.Errorf("connect source database: %w", err)
	}
	defer source.Close(ctx)
	if err := verifyLegacy(ctx, source); err != nil {
		return Report{}, err
	}

	if !cfg.Apply {
		report, _, err := inspect(ctx, source)
		if err != nil {
			return Report{}, err
		}
		return report, nil
	}

	cfg = withSchemaDefaults(cfg)
	if cfg.LegacySchema != database.DefaultSchema || cfg.FinalSchema != database.DefaultSchema {
		return Report{}, fmt.Errorf("legacy and final schema must be %q", database.DefaultSchema)
	}
	if cfg.Target.Schema == cfg.LegacySchema || cfg.Target.Schema == cfg.FinalSchema || cfg.BackupSchema == cfg.Target.Schema {
		return Report{}, errors.New("staging, legacy, final, and backup schemas must be distinct")
	}
	promoted := false
	defer func() {
		if promoted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = source.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{cfg.Target.Schema}.Sanitize()+" CASCADE")
	}()
	sourceTx, err := source.Begin(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("begin legacy migration: %w", err)
	}
	defer sourceTx.Rollback(ctx)
	if _, err := sourceTx.Exec(ctx, `LOCK TABLE
teldrive.users,
teldrive.channels,
teldrive.bots,
teldrive.files,
public.goose_db_version
IN ACCESS EXCLUSIVE MODE`); err != nil {
		return Report{}, fmt.Errorf("lock legacy database: %w", err)
	}
	report, files, err := inspect(ctx, sourceTx)
	if err != nil {
		return Report{}, err
	}
	if err := ensureSchemaAbsent(ctx, sourceTx, cfg.Target.Schema); err != nil {
		return Report{}, err
	}
	if err := ensureSchemaAbsent(ctx, sourceTx, cfg.BackupSchema); err != nil {
		return Report{}, err
	}
	cfg.Target.AllowLegacySchema = true
	if err := database.Migrate(ctx, cfg.Target); err != nil {
		return Report{}, fmt.Errorf("prepare target database: %w", err)
	}
	target, err := database.Open(ctx, cfg.Target)
	if err != nil {
		return Report{}, fmt.Errorf("open target database: %w", err)
	}
	defer target.Close()

	tx, err := target.Begin(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("begin target transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := ensureEmpty(ctx, tx, cfg.Target.Schema); err != nil {
		return Report{}, err
	}
	if err := setCopyEventTriggers(ctx, tx, cfg.Target.Schema, false); err != nil {
		return Report{}, err
	}
	if err := migrateUsers(ctx, sourceTx, tx, cfg.Target.Schema); err != nil {
		return Report{}, err
	}
	if err := migrateChannels(ctx, sourceTx, tx, cfg.Target.Schema); err != nil {
		return Report{}, err
	}
	if err := migrateBots(ctx, sourceTx, tx, cipher, cfg.BotVerifier, cfg.Target.Schema); err != nil {
		return Report{}, err
	}
	if err := migrateFiles(ctx, tx, files, cfg); err != nil {
		return Report{}, err
	}
	if err := setCopyEventTriggers(ctx, tx, cfg.Target.Schema, true); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("commit target migration: %w", err)
	}
	target.Close()
	if err := swapSchemas(ctx, sourceTx, cfg); err != nil {
		return Report{}, err
	}
	if err := sourceTx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("commit schema cutover: %w", err)
	}
	promoted = true
	return report, nil
}

// withSchemaDefaults fills in the schema names a caller may omit. The legacy and
// final schemas default to the v1 name, and the backup schema is derived from the
// legacy schema rather than from a timestamp so a dry run is reproducible.
func withSchemaDefaults(cfg Config) Config {
	if cfg.LegacySchema == "" {
		cfg.LegacySchema = database.DefaultSchema
	}
	if cfg.FinalSchema == "" {
		cfg.FinalSchema = database.DefaultSchema
	}
	if cfg.BackupSchema == "" {
		cfg.BackupSchema = cfg.LegacySchema + "_legacy_backup"
	}
	return cfg
}

// ensureSchemaAbsent fails when schema already exists. Refusing to reuse a
// schema is what keeps a failed attempt from being silently resumed on top of
// half-migrated data; the operator is expected to inspect and drop it.
func ensureSchemaAbsent(ctx context.Context, conn legacyReader, schema string) error {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil {
		return fmt.Errorf("inspect schema %s: %w", schema, err)
	}
	if exists {
		return fmt.Errorf("schema %s already exists", schema)
	}
	return nil
}

// swapSchemas performs the cutover: the legacy schema is renamed to the backup
// name, the legacy goose history table is moved out of public into that backup,
// and the staging schema is renamed into the final name.
//
// All three steps run in the caller's transaction, so a failure anywhere leaves
// the original names in place. Moving goose_db_version is what makes the
// database stop looking legacy to the next start-up.
func swapSchemas(ctx context.Context, tx pgx.Tx, cfg Config) error {
	legacy := pgx.Identifier{cfg.LegacySchema}.Sanitize()
	backup := pgx.Identifier{cfg.BackupSchema}.Sanitize()
	staging := pgx.Identifier{cfg.Target.Schema}.Sanitize()
	final := pgx.Identifier{cfg.FinalSchema}.Sanitize()
	if _, err := tx.Exec(ctx, "ALTER SCHEMA "+legacy+" RENAME TO "+backup); err != nil {
		return fmt.Errorf("rename legacy schema to backup: %w", err)
	}
	var gooseExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.goose_db_version') IS NOT NULL`).Scan(&gooseExists); err != nil {
		return fmt.Errorf("inspect legacy goose table: %w", err)
	}
	if gooseExists {
		if _, err := tx.Exec(ctx, "ALTER TABLE public.goose_db_version SET SCHEMA "+backup); err != nil {
			return fmt.Errorf("move legacy goose table to backup schema: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "ALTER SCHEMA "+staging+" RENAME TO "+final); err != nil {
		return fmt.Errorf("promote staging schema: %w", err)
	}
	return nil
}

// verifyLegacy rejects a source that does not look like a legacy TelDrive
// production database, so an operator pointing the migrator at the wrong
// database gets an error instead of an empty migration.
func verifyLegacy(ctx context.Context, conn *pgx.Conn) error {
	var legacy bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.goose_db_version') IS NOT NULL`).Scan(&legacy); err != nil {
		return fmt.Errorf("inspect legacy migration table: %w", err)
	}
	if !legacy {
		return errors.New("source is not a legacy TelDrive production database")
	}
	return nil
}

// inspect reads the whole legacy file tree and returns the counters for Report
// together with the files in parent-first order.
//
// It validates as it scans: a non-folder with a missing or negative size, a file
// with no usable parts, or an encrypted file whose part has no salt all abort the
// migration, because silently dropping or guessing such a row would lose data.
// The one tolerated exception is a zero-byte file with no parts, which is counted
// in SkippedZero and migrated as an empty file.
//
// Files are read in created_at order so a rerun produces the same ordering, and
// the result is passed through flattenSyntheticRoots before being topologically
// sorted.
func inspect(ctx context.Context, source legacyReader) (Report, []legacyFile, error) {
	var report Report
	if err := source.QueryRow(ctx, `SELECT
(SELECT count(*) FROM teldrive.users),
(SELECT count(*) FROM teldrive.channels),
(SELECT count(DISTINCT (user_id, bot_id)) FROM teldrive.bots)`).Scan(&report.Users, &report.Channels, &report.Bots); err != nil {
		return Report{}, nil, fmt.Errorf("count legacy rows: %w", err)
	}

	rows, err := source.Query(ctx, `
SELECT id, name, type, mime_type, size, user_id, parent_id, status,
       channel_id, COALESCE(parts, '[]'::jsonb), COALESCE(encrypted, false),
       hash, created_at, updated_at
FROM teldrive.files
ORDER BY created_at, id`)
	if err != nil {
		return Report{}, nil, fmt.Errorf("read legacy files: %w", err)
	}
	defer rows.Close()

	var files []legacyFile
	for rows.Next() {
		var f legacyFile
		var raw []byte
		if err := rows.Scan(&f.ID, &f.Name, &f.Kind, &f.MimeType, &f.Size, &f.UserID, &f.ParentID, &f.Status, &f.ChannelID, &raw, &f.Encrypted, &f.Hash, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return Report{}, nil, fmt.Errorf("scan legacy file: %w", err)
		}
		if err := json.Unmarshal(raw, &f.Parts); err != nil {
			return Report{}, nil, fmt.Errorf("decode parts for file %s: %w", f.ID, err)
		}
		if f.Kind == "folder" {
			report.Folders++
		} else {
			report.Files++
			if f.Size == nil || *f.Size < 0 {
				return Report{}, nil, fmt.Errorf("invalid size for file %s", f.ID)
			}
			if *f.Size == 0 && len(f.Parts) == 0 {
				report.SkippedZero++
			} else if len(f.Parts) == 0 || f.ChannelID == nil {
				return Report{}, nil, fmt.Errorf("file %s has no usable Telegram parts", f.ID)
			}
			report.FileParts += int64(len(f.Parts))
			if f.Encrypted {
				report.Encrypted++
				for _, p := range f.Parts {
					if p.Salt == "" {
						return Report{}, nil, fmt.Errorf("encrypted file %s has a part without salt", f.ID)
					}
				}
			}
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return Report{}, nil, fmt.Errorf("iterate legacy files: %w", err)
	}
	files, removedRoots := flattenSyntheticRoots(files)
	report.Folders -= int64(removedRoots)
	ordered, err := orderFilesParentFirst(files)
	if err != nil {
		return Report{}, nil, err
	}
	return report, ordered, nil
}

// flattenSyntheticRoots removes the per-user "root" folders the v1 server created
// and re-parents their children to nil, which is how v2 represents a top-level
// entry. It returns the rewritten slice and how many folders were removed, so
// Report.Folders does not count rows that no longer exist.
//
// A legacy row only qualifies as synthetic when it is a top-level folder named
// "root" with the drive/folder MIME type, so a user-created folder of that name
// nested elsewhere is left alone.
func flattenSyntheticRoots(files []legacyFile) ([]legacyFile, int) {
	rootIDs := make(map[uuid.UUID]struct{})
	for _, file := range files {
		if file.ParentID == nil && file.Name == "root" && file.Kind == "folder" && file.MimeType == "drive/folder" {
			rootIDs[file.ID] = struct{}{}
		}
	}
	if len(rootIDs) == 0 {
		return files, 0
	}

	flattened := make([]legacyFile, 0, len(files)-len(rootIDs))
	for _, file := range files {
		if _, syntheticRoot := rootIDs[file.ID]; syntheticRoot {
			continue
		}
		if file.ParentID != nil {
			if _, childOfSyntheticRoot := rootIDs[*file.ParentID]; childOfSyntheticRoot {
				file.ParentID = nil
			}
		}
		flattened = append(flattened, file)
	}
	return flattened, len(rootIDs)
}

// orderFilesParentFirst sorts the tree so every folder precedes its children,
// which the v2 foreign key on parent_id requires for a single streaming copy.
//
// The traversal doubles as validation: it rejects a parent that is missing, that
// is not a folder, or that belongs to another user, and it rejects a cycle. The
// third error is what prevents a corrupt legacy tree from being copied into a
// schema whose constraints would fail later, mid-copy.
func orderFilesParentFirst(files []legacyFile) ([]legacyFile, error) {
	byID := make(map[uuid.UUID]legacyFile, len(files))
	for _, file := range files {
		byID[file.ID] = file
	}
	state := make(map[uuid.UUID]uint8, len(files))
	ordered := make([]legacyFile, 0, len(files))
	var visit func(uuid.UUID) error
	visit = func(id uuid.UUID) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("legacy file hierarchy contains a cycle at %s", id)
		case 2:
			return nil
		}
		file, ok := byID[id]
		if !ok {
			return fmt.Errorf("legacy file %s was not found", id)
		}
		state[id] = 1
		if file.ParentID != nil {
			parent, ok := byID[*file.ParentID]
			if !ok || parent.Kind != "folder" || parent.UserID != file.UserID {
				return fmt.Errorf("invalid parent for file %s", file.ID)
			}
			if err := visit(parent.ID); err != nil {
				return err
			}
		}
		state[id] = 2
		ordered = append(ordered, file)
		return nil
	}
	for _, file := range files {
		if err := visit(file.ID); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// ensureEmpty fails unless the users, channels, bots, files, and file_parts
// tables in schema are still empty. Combined with the missing-schema check this
// keeps the copy from merging into rows a previous attempt left behind in those
// tables.
func ensureEmpty(ctx context.Context, tx pgx.Tx, schema string) error {
	var users, channels, bots, files, parts int64
	prefix := pgx.Identifier{schema}.Sanitize()
	query := fmt.Sprintf(`SELECT
(SELECT count(*) FROM %s.users),
(SELECT count(*) FROM %s.channels),
(SELECT count(*) FROM %s.bots),
(SELECT count(*) FROM %s.files),
(SELECT count(*) FROM %s.file_parts)`, prefix, prefix, prefix, prefix, prefix)
	if err := tx.QueryRow(ctx, query).Scan(&users, &channels, &bots, &files, &parts); err != nil {
		return fmt.Errorf("inspect target tables: %w", err)
	}
	for table, count := range map[string]int64{"users": users, "channels": channels, "bots": bots, "files": files, "file_parts": parts} {
		if count != 0 {
			return fmt.Errorf("target table %s is not empty", table)
		}
	}
	return nil
}

// setCopyEventTriggers enables or disables the user-event triggers on the copied
// tables for the duration of the bulk copy.
//
// Without this, every inserted row would publish a notification that no client
// is listening for yet, at a cost proportional to the size of the legacy drive.
// The triggers are re-enabled before the transaction commits, so the promoted
// schema behaves normally once it is live.
func setCopyEventTriggers(ctx context.Context, tx pgx.Tx, schema string, enabled bool) error {
	action := "DISABLE"
	if enabled {
		action = "ENABLE"
	}
	prefix := pgx.Identifier{schema}.Sanitize()
	for _, target := range [][2]string{
		{"channels", "channels_emit_user_event"},
		{"files", "files_emit_user_event"},
	} {
		table, trigger := target[0], target[1]
		query := "ALTER TABLE " + prefix + "." + pgx.Identifier{table}.Sanitize() + " " + action + " TRIGGER " + pgx.Identifier{trigger}.Sanitize()
		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("%s legacy copy event trigger %s: %w", strings.ToLower(action), trigger, err)
		}
	}
	return nil
}

// migrateUsers copies the legacy users and assigns the v2 roles.
//
// The v1 schema had no roles, so the migration promotes exactly one account: the
// oldest user, by created_at then user_id, becomes "owner" and everyone else
// becomes "user". The ordering is total, so a rerun on the same data always
// picks the same owner.
//
// Rows are streamed straight from the source cursor into a COPY, so no table is
// materialised in memory.
func migrateUsers(ctx context.Context, source legacyReader, tx pgx.Tx, schema string) error {
	rows, err := source.Query(ctx, `
SELECT user_id, name, user_name, is_premium, created_at, updated_at,
       CASE WHEN row_number() OVER (ORDER BY created_at ASC, user_id ASC) = 1 THEN 'owner' ELSE 'user' END
FROM teldrive.users
ORDER BY user_id`)
	if err != nil {
		return fmt.Errorf("read users: %w", err)
	}
	defer rows.Close()
	_, err = tx.CopyFrom(ctx, pgx.Identifier{schema, "users"}, []string{"user_id", "display_name", "username", "premium", "created_at", "updated_at", "role"}, pgx.CopyFromFunc(func() ([]any, error) {
		if !rows.Next() {
			return nil, rows.Err()
		}
		var id int64
		var name, username *string
		var premium bool
		var created, updated time.Time
		var role string
		if err := rows.Scan(&id, &name, &username, &premium, &created, &updated, &role); err != nil {
			return nil, err
		}
		return []any{id, name, username, premium, created, updated, role}, nil
	}))
	if err != nil {
		return fmt.Errorf("copy users: %w", err)
	}
	return nil
}

// migrateChannels copies the legacy channels.
//
// A legacy channel may have no created_at, in which case the current time is
// substituted; v2 also has an updated_at column the legacy schema lacked, so it
// is initialised to the same value as created_at. The "selected" flag is carried
// over as-is, which preserves each user's active channel.
func migrateChannels(ctx context.Context, source legacyReader, tx pgx.Tx, schema string) error {
	rows, err := source.Query(ctx, `SELECT channel_id,user_id,channel_name,COALESCE(selected,false),COALESCE(created_at,now()) FROM teldrive.channels ORDER BY user_id,channel_id`)
	if err != nil {
		return fmt.Errorf("read channels: %w", err)
	}
	defer rows.Close()
	_, err = tx.CopyFrom(ctx, pgx.Identifier{schema, "channels"}, []string{"channel_id", "user_id", "name", "selected", "created_at", "updated_at"}, pgx.CopyFromFunc(func() ([]any, error) {
		if !rows.Next() {
			return nil, rows.Err()
		}
		var channelID, userID int64
		var name string
		var selected bool
		var created time.Time
		if err := rows.Scan(&channelID, &userID, &name, &selected, &created); err != nil {
			return nil, err
		}
		return []any{channelID, userID, name, selected, created, created}, nil
	}))
	if err != nil {
		return fmt.Errorf("copy channels: %w", err)
	}
	return nil
}

// migrateBots copies the legacy bot tokens, encrypting each one with the
// configured data key before it reaches the target table.
//
// The legacy data allows several tokens for the same (user, bot) pair, while v2
// allows one row. Duplicates are therefore resolved against Telegram: the
// candidates are verified in order and the first token whose Telegram identity
// matches the stored bot ID wins. If none matches, or if no verifier was
// supplied, the migration aborts rather than guessing, because picking the wrong
// token would silently break uploads for that user.
//
// The plaintext token never leaves this function: it is only ever passed to the
// verifier and to the cipher.
func migrateBots(ctx context.Context, source legacyReader, tx pgx.Tx, cipher *secureblob.Cipher, verifier bots.Verifier, schema string) error {
	rows, err := source.Query(ctx, `SELECT user_id,token,bot_id FROM teldrive.bots ORDER BY user_id,bot_id,token`)
	if err != nil {
		return fmt.Errorf("read bots: %w", err)
	}

	legacyBots := make([]legacyBot, 0)
	for rows.Next() {
		var bot legacyBot
		if err := rows.Scan(&bot.UserID, &bot.Token, &bot.BotID); err != nil {
			rows.Close()
			return fmt.Errorf("scan bot: %w", err)
		}
		legacyBots = append(legacyBots, bot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read bots: %w", err)
	}
	rows.Close()

	selected := make([]legacyBot, 0, len(legacyBots))
	for start := 0; start < len(legacyBots); {
		end := start + 1
		for end < len(legacyBots) && legacyBots[end].UserID == legacyBots[start].UserID && legacyBots[end].BotID == legacyBots[start].BotID {
			end++
		}

		chosen := legacyBots[start]
		if end-start > 1 {
			if verifier == nil {
				return fmt.Errorf("resolve duplicate tokens for user %d bot %d: Telegram bot verifier is unavailable", chosen.UserID, chosen.BotID)
			}
			found := false
			var lastVerifyErr error
			for _, candidate := range legacyBots[start:end] {
				identity, verifyErr := verifier.Verify(ctx, candidate.Token)
				if verifyErr != nil {
					lastVerifyErr = verifyErr
					continue
				}
				if identity.ID != candidate.BotID {
					continue
				}
				chosen = candidate
				found = true
				break
			}
			if !found {
				if lastVerifyErr != nil {
					return fmt.Errorf("resolve duplicate tokens for user %d bot %d: no candidate token was accepted by Telegram: %w", chosen.UserID, chosen.BotID, lastVerifyErr)
				}
				return fmt.Errorf("resolve duplicate tokens for user %d bot %d: no candidate token matches the Telegram bot identity", chosen.UserID, chosen.BotID)
			}
		}
		selected = append(selected, chosen)
		start = end
	}

	_, err = tx.CopyFrom(ctx, pgx.Identifier{schema, "bots"}, []string{"bot_id", "user_id", "token_ciphertext", "enabled"}, pgx.CopyFromSlice(len(selected), func(i int) ([]any, error) {
		bot := selected[i]
		sealed, err := cipher.Seal("bot-token", []byte(bot.Token))
		if err != nil {
			return nil, fmt.Errorf("encrypt bot %d: %w", bot.BotID, err)
		}
		return []any{bot.BotID, bot.UserID, sealed, true}, nil
	}))
	if err != nil {
		return fmt.Errorf("copy bots: %w", err)
	}
	return nil
}

// migrateFiles rewrites the legacy tree into the v2 files and file_parts tables.
//
// The mapping is deliberately lossless where it can be. Status is narrowed to v2's
// vocabulary: anything other than "active" becomes "deletion_pending" with the
// legacy updated_at reused as deleted_at, so a v1 trash entry stays in the trash.
// Size is recorded only for files, leaving folders NULL. A legacy hash becomes a
// blake3-tree hash value, and encrypted files record the configured key version
// so their parts remain decryptable. New rows start at generation 1.
//
// Parts are numbered from 1 in slice order, which is the order the v1 server
// wrote them in and therefore the order the file must be reassembled in.
// Plain-text and stored sizes are left NULL: the legacy schema never recorded
// them, and the v2 code treats NULL as "not yet measured".
//
// Both tables are written with COPY in the caller's transaction, so a failure
// rolls back the whole copy rather than leaving a partially migrated tree.
// migrateFiles copies the legacy files and their parts into the target schema.
//
// Both copies stream rather than materializing a row slice: the files are
// transformed one at a time, and the parts cursor releases each file's parsed
// parts once they have been written. A legacy library therefore costs the metadata
// it was read into once, instead of that plus a second copy of every row, which for
// a library of a million parts is the difference between a migration and an
// out-of-memory kill.
func migrateFiles(ctx context.Context, tx pgx.Tx, files []legacyFile, cfg Config) error {
	fileIndex := 0
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{cfg.Target.Schema, "files"}, legacyFileColumns, pgx.CopyFromFunc(func() ([]any, error) {
		// pgx ends a stream when the source returns no row and no error; an error
		// aborts the copy, so the end of the slice is signalled with nil row and
		// nil error.
		if fileIndex >= len(files) {
			return nil, nil
		}
		row := legacyFileRow(files[fileIndex], cfg)
		fileIndex++
		return row, nil
	})); err != nil {
		return fmt.Errorf("copy files: %w", err)
	}
	parts := legacyPartCursor{files: files}
	if !parts.hasRows() {
		return nil
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{cfg.Target.Schema, "file_parts"}, legacyPartColumns, pgx.CopyFromFunc(parts.next)); err != nil {
		return fmt.Errorf("copy file parts: %w", err)
	}
	return nil
}

// legacyFileColumns and legacyPartColumns are the target columns of the two copies
// above, in the order the row builders produce their values.
var (
	legacyFileColumns = []string{"id", "user_id", "parent_id", "name", "kind", "mime_type", "size", "hash_algorithm", "hash_value", "encryption", "encryption_key_version", "status", "mod_time", "generation", "created_at", "updated_at", "deleted_at"}
	legacyPartColumns = []string{"file_id", "part_no", "channel_id", "message_id", "plain_size", "stored_size", "salt", "created_at"}
)

// legacyFileRow maps one legacy file onto the columns of the target files table.
// A non-active legacy status becomes a pending deletion, an encrypted file carries
// the configured key version, and the recorded digest is described with the
// algorithm the API contract declares, so a migrated row looks like a freshly
// completed upload.
func legacyFileRow(f legacyFile, cfg Config) []any {
	status := "active"
	var deletedAt *time.Time
	if f.Status != "active" {
		status = "deletion_pending"
		t := f.UpdatedAt
		deletedAt = &t
	}
	enc := false
	var keyVersion *int
	if f.Encrypted {
		enc = true
		v := cfg.EncryptionKeyVersion
		keyVersion = &v
	}
	var size *int64
	if f.Kind == "file" {
		size = f.Size
	}
	var hashAlg, hashValue *string
	if f.Hash != nil && *f.Hash != "" {
		alg := string(treehash.TypeBlake3)
		hashAlg, hashValue = &alg, f.Hash
	}
	return []any{f.ID, f.UserID, f.ParentID, f.Name, f.Kind, f.MimeType, size, hashAlg, hashValue, enc, keyVersion, status, f.UpdatedAt, int64(1), f.CreatedAt, f.UpdatedAt, deletedAt}
}

// legacyPartCursor walks the parts of the legacy files in file order. It clears a
// file's parts as soon as the last of them is handed out, so the memory the
// migration holds shrinks while it writes.
type legacyPartCursor struct {
	files []legacyFile
	file  int
	part  int
}

// hasRows reports whether any legacy file contributes a part, which is what the
// caller checks before starting a copy that would otherwise write nothing.
func (c *legacyPartCursor) hasRows() bool {
	for _, f := range c.files {
		if f.Kind == "file" && len(f.Parts) > 0 {
			return true
		}
	}
	return false
}

// next returns the next part row, or no row at all once every part has been
// written, which is how pgx ends a copy stream. It is called by pgx while the copy
// runs, so it must be safe to call after the end.
func (c *legacyPartCursor) next() ([]any, error) {
	for c.file < len(c.files) {
		f := &c.files[c.file]
		if f.Kind != "file" || c.part >= len(f.Parts) {
			c.file++
			c.part = 0
			continue
		}
		p := f.Parts[c.part]
		c.part++
		if c.part == len(f.Parts) {
			f.Parts = nil
		}
		var salt *string
		if p.Salt != "" {
			salt = &p.Salt
		}
		return []any{f.ID, int32(c.part), *f.ChannelID, p.ID, nil, nil, salt, f.CreatedAt}, nil
	}
	return nil, nil
}
