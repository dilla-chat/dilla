package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/blob"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/exit"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/ops"
	"github.com/jonasthim/dilla/internal/store"
)

// adminUsage is printed to stderr on a usage error and to stdout on --help.
const adminUsage = `usage: dillad admin <noun> <verb> [flags]

nouns and verbs:
  user      list | show | disable | enable | delete | reset-factors
  invite    create | list | revoke
  community list | show
  device    revoke
  bot-runtime create | rotate | revoke | list
  blob      purge
  audit

every verb accepts --config=PATH; run "dillad admin <noun> --help" for its verbs
and "dillad admin <noun> <verb> --help" for its flags
`

// The limits `invite create` holds itself to are the API's (internal/api/invites.go): the schema's
// own ceiling on max_uses, and thirty days of life.
const (
	maxInviteUses = 1000
	maxInviteTTL  = 30 * 24 * time.Hour
)

// pageSize is how many rows a listing reads at a time when it lists everything.
const pageSize = 500

// nounUsage is what `dillad admin <noun> --help` prints: the verbs and the flags each takes.
var nounUsage = map[string]string{
	"user": `usage: dillad admin user <verb> [flags]

  list     [--limit=N] [--after=ID]
  show     --id=ID | --username=NAME
  disable  --id=ID | --username=NAME   sets disabled_at and deletes every session
  enable   --id=ID | --username=NAME   clears disabled_at
  delete   --id=ID | --username=NAME --yes   tombstones the account; the handle stays reserved
  reset-factors  --id=ID | --username=NAME --yes   removes every second factor; the person sets one up at their next sign-in
`,
	"invite": `usage: dillad admin invite <verb> [flags]

  create  [--max-uses=N] [--ttl=DURATION] [--community=ID] [--grants-admin]
  list    [--community=ID]
  revoke  --id=ID
`,
	"community": `usage: dillad admin community <verb> [flags]

  list  [--limit=N] [--after=ID]
  show  --id=ID
`,
	"device": `usage: dillad admin device <verb> [flags]

  revoke  --id=ID   revokes the device and deletes its sessions
  revoke  --all --id=ID | --username=NAME [--yes]   signs every device of the user out and deletes every
          session; it prints the user and asks for the username typed back unless --yes; audited
`,
	"bot-runtime": `usage: dillad admin bot-runtime <verb> [flags]

  create  --out=PATH       create one runtime credential in a protected new file
  rotate  --id=ID --out=PATH
  revoke  --id=ID
  list
`,
	"blob": `usage: dillad admin blob <verb> [flags]

  purge  --id=HEX64 --reason=TEXT   removes the bytes and every reference, and tombstones the id
`,
}

// runAdmin is `dillad admin <noun> <verb> [flags]`. A usage error prints the block to stderr and
// exits 2; --help prints it to stdout and exits 0. flag.ContinueOnError has already printed a bad
// flag's message and its own usage block, so a handler never prints those a second time.
func runAdmin(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprint(stdout, adminUsage)
		return nil
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, adminUsage)
		return exit.Usage
	}
	noun, rest := args[0], args[1:]
	if noun == "audit" {
		return adminAudit(rest, stdout, stderr)
	}
	verbs := map[string]func(string, []string, io.Writer, io.Writer) error{
		"user": adminUser, "invite": adminInvite, "community": adminCommunity,
		"device": adminDevice, "blob": adminBlob, "bot-runtime": adminBotRuntime,
	}
	dispatch, ok := verbs[noun]
	if !ok {
		fmt.Fprintf(stderr, "dillad admin: unknown noun %q\n\n", noun)
		fmt.Fprint(stderr, adminUsage)
		return exit.Usage
	}
	if len(rest) == 0 {
		fmt.Fprint(stderr, adminUsage)
		return exit.Usage
	}
	if rest[0] == "--help" || rest[0] == "-h" || rest[0] == "help" {
		fmt.Fprint(stdout, nounUsage[noun])
		return nil
	}
	return dispatch(rest[0], rest[1:], stdout, stderr)
}

// unknownVerb is the usage error for a noun asked for a verb it does not have.
func unknownVerb(noun, verb string, stderr io.Writer) error {
	fmt.Fprintf(stderr, "dillad admin %s: unknown verb %q\n\n", noun, verb)
	fmt.Fprint(stderr, adminUsage)
	return exit.Usage
}

// adminAction is one verb's two halves: check validates the flags before anything is opened, so a
// usage error never touches the database, and run does the work against the opened instance.
type adminAction struct {
	check func() error
	run   func(ctx context.Context, e *adminEnv) error
}

// usageError prints "dillad <verb>: <message>" to stderr and is exit 2.
func usageError(stderr io.Writer, verb, format string, args ...any) error {
	fmt.Fprintf(stderr, "dillad %s: %s\n", verb, fmt.Sprintf(format, args...))
	return exit.Usage
}

// runVerb is the shape every verb shares. It builds the verb's own FlagSet (which carries --config,
// as every FlagSet in this binary does), lets setup register the verb's flags, parses, and only
// then opens the instance.
func runVerb(name string, args []string, stdout, stderr io.Writer, setup func(fs *flag.FlagSet) adminAction) error {
	fs, cfgPath := newFlagSet(name, stderr)
	action := setup(fs)
	// flag prints the usage block itself, to the FlagSet's output, before Parse returns ErrHelp. A
	// request for help is answered on stdout and only there, so the output is pointed at stdout
	// up front when one is asked for; every other error keeps stderr.
	if slices.ContainsFunc(args, isHelpFlag) {
		fs.SetOutput(stdout)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// parse() would return nil here too, but its caller goes on to run the verb.
			return nil
		}
		return fmt.Errorf("%s: %w", fs.Name(), exit.Usage)
	}
	if fs.NArg() > 0 {
		return usageError(stderr, name, "unexpected argument %q", fs.Arg(0))
	}
	if action.check != nil {
		if err := action.check(); err != nil {
			return err
		}
	}
	e, err := openAdmin(*cfgPath, stdout, stderr)
	if err != nil {
		return err
	}
	defer e.close()
	return action.run(context.Background(), e)
}

func isHelpFlag(a string) bool {
	switch a {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// adminEnv is an opened instance: the configuration, the repository, a logger on stderr and the
// clock the verbs stamp their audit rows with.
type adminEnv struct {
	cfg  *config.Config
	repo store.Repository
	log  *slog.Logger
	out  io.Writer
	errw io.Writer
}

func (e *adminEnv) close() { _ = e.repo.Close() }

func (e *adminEnv) now() int64 { return cliClock.Now().Unix() }

// openAdmin is the one way a verb reaches the database. It refuses a schema that is not exactly the
// one this binary embeds: the CLI writes rows, and a row written through a schema the binary does
// not understand is how a rollback corrupts what the newer binary already migrated. A newer
// schema is the dangerous one (`serve` refuses it the same way, with its own exit code); an older
// one would send the verbs at tables that may not exist, so it is refused too, naming the fix.
func openAdmin(cfgPath string, stdout, stderr io.Writer) (*adminEnv, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	repo, db, err := openRepository(cfg)
	if err != nil {
		return nil, err
	}
	provider, err := migrationProvider(cfg, db)
	if err != nil {
		_ = repo.Close()
		return nil, err
	}
	current, target, err := provider.GetVersions(context.Background())
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("admin: read schema version: %w: %w", err, exit.Data)
	}
	switch {
	case current > target:
		_ = repo.Close()
		return nil, fmt.Errorf("admin: database schema %d is newer than this binary's highest migration %d: "+
			"refusing to write through a schema this binary does not understand: %w", current, target, exit.Config)
	case current < target:
		_ = repo.Close()
		return nil, fmt.Errorf("admin: database schema %d is older than this binary's %d: "+
			"run `dillad migrate up` (or start `dillad serve`, which migrates at start) first: %w", current, target, exit.Config)
	}
	return &adminEnv{cfg: cfg, repo: repo, log: obs.NewLogger(cfg.Log, stderr), out: stdout, errw: stderr}, nil
}

// notFound is the error for an id or name that names no record.
func notFound(what string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s: not found: %w", what, exit.Data)
	}
	return fmt.Errorf("%s: %w: %w", what, err, exit.Software)
}

// ---------------------------------------------------------------------------------------------
// Rendering. Whatever a user typed (a display name, a purge reason) must not reach the operator's
// terminal as an escape sequence or a bidi override, so every free-text column goes through
// printable.

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return '?'
		}
		return r
	}, s)
}

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

func stampPtr(unix *int64) string {
	if unix == nil {
		return "-"
	}
	return stamp(*unix)
}

func idPtr(i *id.ID) string {
	if i == nil {
		return "-"
	}
	return i.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return printable(s)
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// kv prints one "key value" line of a show verb.
func kv(w io.Writer, key string, value any) { fmt.Fprintf(w, "%-22s %v\n", key, value) }

func parseID(stderr io.Writer, verb, flagName, value string) (id.ID, error) {
	if value == "" {
		return id.ID{}, usageError(stderr, verb, "--%s is required", flagName)
	}
	i, err := id.Parse(value)
	if err != nil {
		return id.ID{}, usageError(stderr, verb, "--%s: %v", flagName, err)
	}
	return i, nil
}

// ---------------------------------------------------------------------------------------------
// user

func userStatus(u store.UserRow) string {
	switch {
	case u.DeletedAt != nil:
		return "deleted"
	case u.DisabledAt != nil:
		return "disabled"
	default:
		return "active"
	}
}

func userFlags(u store.UserRow) string {
	var f []string
	if u.Flags&store.UserFlagInstanceAdmin != 0 {
		f = append(f, "instance_admin")
	}
	if u.Flags&store.UserFlagBotOperator != 0 {
		f = append(f, "bot_operator")
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

// factorSummary is what second factors an account holds: a confirmed authenticator app, the passkeys
// registered under the configured rp_id, the unused backup codes, and whether the account owes a factor.
type factorSummary struct {
	TOTP     bool
	Passkeys int
	Codes    int64
	Owed     bool
}

// String is the factors column: totp, passkey×N, codes:N, owed, in that order, or "-" for none.
func (f factorSummary) String() string {
	var parts []string
	if f.TOTP {
		parts = append(parts, "totp")
	}
	if f.Passkeys > 0 {
		parts = append(parts, fmt.Sprintf("passkey×%d", f.Passkeys))
	}
	if f.Codes > 0 {
		parts = append(parts, fmt.Sprintf("codes:%d", f.Codes))
	}
	if f.Owed {
		parts = append(parts, "owed")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// userFactors reads the user's second factors. A TOTP secret that was never confirmed does not count,
// and passkeys count only under rpID, the way sign-in sees them.
func userFactors(ctx context.Context, repo store.Repository, u store.UserRow, rpID string) (factorSummary, error) {
	f := factorSummary{Owed: u.FactorOwed}
	switch totp, err := repo.GetTOTP(ctx, u.ID); {
	case err == nil:
		f.TOTP = totp.ConfirmedAt != nil
	case !errors.Is(err, store.ErrNotFound):
		return f, fmt.Errorf("read totp: %w: %w", err, exit.Software)
	}
	creds, err := repo.ListWebauthnCredentials(ctx, u.ID, rpID)
	if err != nil {
		return f, fmt.Errorf("list passkeys: %w: %w", err, exit.Software)
	}
	f.Passkeys = len(creds)
	if f.Codes, err = repo.CountRecoveryCodes(ctx, u.ID); err != nil {
		return f, fmt.Errorf("count backup codes: %w: %w", err, exit.Software)
	}
	return f, nil
}

// totpConfirmedLine is show's totp value: "confirmed <stamp>", or "-" when there is no confirmed secret.
func totpConfirmedLine(ctx context.Context, repo store.Repository, uid id.ID) (string, error) {
	totp, err := repo.GetTOTP(ctx, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "-", nil
	case err != nil:
		return "", fmt.Errorf("read totp: %w: %w", err, exit.Software)
	case totp.ConfirmedAt == nil:
		return "-", nil
	}
	return "confirmed " + stamp(*totp.ConfirmedAt), nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// userSelector is the pair of flags that name one user: exactly one of --id and --username.
type userSelector struct {
	id       *string
	username *string
}

func (s userSelector) register(fs *flag.FlagSet) userSelector {
	s.id = fs.String("id", "", "the user's id (32 hex)")
	s.username = fs.String("username", "", "the user's handle, in place of --id")
	return s
}

func (s userSelector) check(stderr io.Writer, verb string) error {
	switch {
	case *s.id == "" && *s.username == "":
		return usageError(stderr, verb, "one of --id and --username is required")
	case *s.id != "" && *s.username != "":
		return usageError(stderr, verb, "--id and --username are alternatives; give one")
	case *s.id != "":
		_, err := parseID(stderr, verb, "id", *s.id)
		return err
	}
	return nil
}

func (s userSelector) resolve(ctx context.Context, repo store.Repository) (store.UserRow, error) {
	if *s.id != "" {
		uid, _ := id.Parse(*s.id) // check validated it
		u, err := repo.GetUser(ctx, uid)
		if err != nil {
			return store.UserRow{}, notFound("user "+uid.String(), err)
		}
		return u, nil
	}
	u, err := repo.GetUserByUsername(ctx, *s.username)
	if err != nil {
		return store.UserRow{}, notFound("user "+printable(*s.username), err)
	}
	return u, nil
}

func adminUser(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin user " + verb
	switch verb {
	case "list":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			limit := fs.Int("limit", 0, "list at most this many users (default: all)")
			after := fs.String("after", "", "list the users whose id sorts after this one")
			var cursor id.ID
			return adminAction{
				check: func() error {
					if *limit < 0 {
						return usageError(stderr, name, "--limit must not be negative")
					}
					if *after != "" {
						var err error
						cursor, err = parseID(stderr, name, "after", *after)
						return err
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error {
					tw := newTable(e.out)
					fmt.Fprintln(tw, "id\tusername\tdisplay\tkind\tflags\tfactors\tstatus\tcreated")
					listed := 0
					for *limit == 0 || listed < *limit {
						n := pageSize
						if *limit > 0 {
							n = min(n, *limit-listed)
						}
						rows, err := e.repo.ListUsers(ctx, cursor, int32(n))
						if err != nil {
							return fmt.Errorf("list users: %w: %w", err, exit.Software)
						}
						for _, u := range rows {
							factors, err := userFactors(ctx, e.repo, u, e.cfg.Auth.WebAuthn.RPID)
							if err != nil {
								return err
							}
							fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", u.ID, printable(u.Username),
								orDash(u.Display), u.Kind, userFlags(u), factors, userStatus(u), stamp(u.Created))
							cursor = u.ID
						}
						listed += len(rows)
						if len(rows) < n {
							break
						}
					}
					return tw.Flush()
				},
			}
		})
	case "show":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			sel := userSelector{}.register(fs)
			return adminAction{
				check: func() error { return sel.check(stderr, name) },
				run: func(ctx context.Context, e *adminEnv) error {
					u, err := sel.resolve(ctx, e.repo)
					if err != nil {
						return err
					}
					devices, err := e.repo.ListDevicesByUser(ctx, u.ID)
					if err != nil {
						return fmt.Errorf("list devices: %w: %w", err, exit.Software)
					}
					kv(e.out, "id", u.ID)
					kv(e.out, "username", printable(u.Username))
					kv(e.out, "display", orDash(u.Display))
					kv(e.out, "kind", u.Kind)
					factors, err := userFactors(ctx, e.repo, u, e.cfg.Auth.WebAuthn.RPID)
					if err != nil {
						return err
					}
					totpLine, err := totpConfirmedLine(ctx, e.repo, u.ID)
					if err != nil {
						return err
					}
					kv(e.out, "flags", userFlags(u))
					kv(e.out, "totp", totpLine)
					kv(e.out, "passkeys", factors.Passkeys)
					kv(e.out, "backup_codes", factors.Codes)
					kv(e.out, "factor_owed", yesNo(factors.Owed))
					kv(e.out, "status", userStatus(u))
					kv(e.out, "created", stamp(u.Created))
					kv(e.out, "disabled_at", stampPtr(u.DisabledAt))
					kv(e.out, "deleted_at", stampPtr(u.DeletedAt))
					kv(e.out, "devices", len(devices))
					tw := newTable(e.out)
					for _, d := range devices {
						sessions, err := e.repo.CountSessionsByDevice(ctx, d.ID)
						if err != nil {
							return fmt.Errorf("count sessions: %w: %w", err, exit.Software)
						}
						state := "active"
						if d.RevokedAt != nil {
							state = "revoked"
						}
						fmt.Fprintf(tw, "  %s\ttier %d\t%s\tsessions %d\tlast_seen %s\n", d.ID, d.Tier, state, sessions, stamp(d.LastSeen))
					}
					return tw.Flush()
				},
			}
		})
	case "disable", "enable":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			sel := userSelector{}.register(fs)
			return adminAction{
				check: func() error { return sel.check(stderr, name) },
				run: func(ctx context.Context, e *adminEnv) error {
					return setUserDisabled(ctx, e, sel, verb == "disable")
				},
			}
		})
	case "delete":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			sel := userSelector{}.register(fs)
			yes := fs.Bool("yes", false, "confirm: the account's keys are zeroed and cannot be brought back")
			return adminAction{
				check: func() error {
					if err := sel.check(stderr, name); err != nil {
						return err
					}
					if !*yes {
						return usageError(stderr, name, "deleting an account cannot be undone; pass --yes to confirm")
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error { return deleteUser(ctx, e, sel) },
			}
		})
	case "reset-factors":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			sel := userSelector{}.register(fs)
			yes := fs.Bool("yes", false, "confirm: every second factor and backup code of the account is removed")
			return adminAction{
				check: func() error {
					if err := sel.check(stderr, name); err != nil {
						return err
					}
					if !*yes {
						return usageError(stderr, name, "removing a person's second factors cannot be undone; pass --yes to confirm")
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error { return resetUserFactors(ctx, e, sel) },
			}
		})
	default:
		return unknownVerb("user", verb, stderr)
	}
}

// resetUserFactors is `dillad admin user reset-factors` (web3.6 S3, protocol/09 § Admin): the
// recovery for a person who lost their authenticator and their backup codes. ops.ResetFactors
// removes the TOTP secret, the backup codes and the passkeys, sets users.factor_owed and writes the
// audit row in one transaction under the user lock. The sessions stay (D5): every device of the
// person meets E_FACTOR_REQUIRED and sets a factor up before it is used again. This is another
// process from `dillad serve` and holds no gateway, so no socket is closed here (the closer is nil):
// the serving process's liveness tick finds the owed account and closes it 4003, like a disable
// closes 4004.
func resetUserFactors(ctx context.Context, e *adminEnv, sel userSelector) error {
	u, err := sel.resolve(ctx, e.repo)
	if err != nil {
		return err
	}
	if u.DeletedAt != nil {
		return fmt.Errorf("user %s is deleted: %w", u.ID, exit.Data)
	}
	if u.Kind != 0 {
		// A bot signs in with its token and never with a factor: owing one would lock it out.
		return fmt.Errorf("user %s is a bot account and holds no second factor: %w", u.ID, exit.Data)
	}
	if err := ops.ResetFactors(ctx, e.repo, nil, u.ID, e.now()); err != nil {
		return fmt.Errorf("reset the second factors of %s: %w: %w", u.ID, err, exit.Software)
	}
	e.log.Info("second factors reset", "user", u.ID.String())
	fmt.Fprintf(e.out, "reset the second factors of user %s (%s); sessions are kept, and live connections close within one heartbeat interval; the person sets up a factor at their next sign-in\n",
		u.ID, printable(u.Username))
	return nil
}

// setUserDisabled is protocol/02 §2.2 point 6 from the operator's side: `disable` sets
// users.disabled_at and deletes every session of the user, in ONE transaction, and that is all a
// separate process can do - it holds no handle on the running gateway. `dillad serve` notices: its
// liveness tick reads each connection's session row and closes the socket 4004 when it is gone, so
// a live connection outlives the disable by at most one heartbeat interval.
func setUserDisabled(ctx context.Context, e *adminEnv, sel userSelector, disable bool) error {
	u, err := sel.resolve(ctx, e.repo)
	if err != nil {
		return err
	}
	if u.DeletedAt != nil {
		// A tombstoned account's keys are zeroed; clearing disabled_at would make it look
		// usable and nothing could ever authenticate it.
		return fmt.Errorf("user %s is deleted and cannot be %sd: %w", u.ID, verbOf(disable), exit.Data)
	}
	now := e.now()
	action, past := "user.enable", "enabled"
	var at *int64
	var sessions int64
	if disable {
		action, past, at = "user.disable", "disabled", &now
	}
	if err := e.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.SetUserDisabled(ctx, u.ID, at); err != nil {
			return err
		}
		if disable {
			n, err := tx.DeleteSessionsByUser(ctx, u.ID)
			if err != nil {
				return err
			}
			sessions = n
		}
		return tx.Audit(ctx, store.AuditRow{Action: action, Target: u.ID.String(), At: now})
	}); err != nil {
		return fmt.Errorf("%s %s: %w: %w", verbOf(disable), u.ID, err, exit.Software)
	}
	e.log.Info("user "+past, "user", u.ID.String(), "sessions_deleted", sessions)
	fmt.Fprintf(e.out, "%s user %s (%s)", past, u.ID, printable(u.Username))
	if disable {
		fmt.Fprintf(e.out, "; %d sessions deleted, live connections close within one heartbeat interval", sessions)
	}
	fmt.Fprintln(e.out)
	return nil
}

func verbOf(disable bool) string {
	if disable {
		return "disable"
	}
	return "enable"
}

// deleteUser is the tombstone of DELETE /v1/accounts/me, run by the operator: the row keeps its
// username so the handle can never be registered again (R36), the store zeroes the key columns,
// and in the same transaction the account is disabled, every session is deleted and every device
// is revoked.
func deleteUser(ctx context.Context, e *adminEnv, sel userSelector) error {
	u, err := sel.resolve(ctx, e.repo)
	if err != nil {
		return err
	}
	if u.DeletedAt != nil {
		fmt.Fprintf(e.out, "user %s (%s) is already deleted\n", u.ID, printable(u.Username))
		return nil
	}
	devices, err := e.repo.ListDevicesByUser(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("list devices: %w: %w", err, exit.Software)
	}
	now := e.now()
	revoked := 0
	if err := e.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.TombstoneUser(ctx, u.ID, now); err != nil {
			return err
		}
		if err := tx.SetUserDisabled(ctx, u.ID, &now); err != nil {
			return err
		}
		if _, err := tx.DeleteSessionsByUser(ctx, u.ID); err != nil {
			return err
		}
		for _, d := range devices {
			if d.RevokedAt != nil {
				continue
			}
			if err := tx.RevokeDevice(ctx, d.ID, now); err != nil {
				return err
			}
			revoked++
		}
		return tx.Audit(ctx, store.AuditRow{Action: "user.delete", Target: u.ID.String(), At: now})
	}); err != nil {
		return fmt.Errorf("delete %s: %w: %w", u.ID, err, exit.Software)
	}
	e.log.Info("user deleted", "user", u.ID.String(), "devices_revoked", revoked)
	fmt.Fprintf(e.out, "deleted user %s (%s); the handle stays reserved; %d devices revoked, every session deleted\n",
		u.ID, printable(u.Username), revoked)
	return nil
}

// ---------------------------------------------------------------------------------------------
// invite

func inviteStatus(i store.InviteRow, now int64) string {
	switch {
	case i.RevokedAt != nil:
		return "revoked"
	case i.ExpiresAt <= now:
		return "expired"
	case i.Exhausted():
		return "exhausted"
	default:
		return "active"
	}
}

// inviteMaxUses and inviteExpiry print an invite made with "∞" uses or "never" (Task 10c).
func inviteMaxUses(i store.InviteRow) string {
	if i.UnlimitedUses == 1 {
		return "unlimited"
	}
	return strconv.FormatUint(i.MaxUses, 10)
}

func inviteExpiry(i store.InviteRow) string {
	if i.NeverExpires() {
		return "never"
	}
	return stamp(i.ExpiresAt)
}

// inviteRef is the 8-hex reference an invite is named by in logs and audit rows: the first four
// bytes of the stored hash, which is itself SHA-256 of the code. It identifies the invite and
// reveals nothing the database does not already hold.
func inviteRef(codeHash []byte) string { return hex.EncodeToString(codeHash[:4]) }

func adminInvite(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin invite " + verb
	switch verb {
	case "create":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			maxUses := fs.Uint64("max-uses", 1, "how many accounts the invite admits (1..1000)")
			ttl := fs.Duration("ttl", 24*time.Hour, "how long the invite lives (at most 720h)")
			community := fs.String("community", "", "a community the invite is for (default: the instance itself)")
			grantsAdmin := fs.Bool("grants-admin", false, "whoever redeems it becomes an instance admin")
			var cid *id.ID
			return adminAction{
				check: func() error {
					if *maxUses < 1 || *maxUses > maxInviteUses {
						return usageError(stderr, name, "--max-uses must be 1..%d", maxInviteUses)
					}
					if *ttl < time.Second || *ttl > maxInviteTTL {
						return usageError(stderr, name, "--ttl must be between 1s and %s", maxInviteTTL)
					}
					if *community != "" {
						c, err := parseID(stderr, name, "community", *community)
						if err != nil {
							return err
						}
						cid = &c
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error {
					return createInvite(ctx, e, *maxUses, *ttl, cid, *grantsAdmin)
				},
			}
		})
	case "list":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			community := fs.String("community", "", "only the invites of this community")
			var cid *id.ID
			return adminAction{
				check: func() error {
					if *community != "" {
						c, err := parseID(stderr, name, "community", *community)
						if err != nil {
							return err
						}
						cid = &c
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error {
					rows, err := e.repo.ListInvites(ctx, cid)
					if err != nil {
						return fmt.Errorf("list invites: %w: %w", err, exit.Software)
					}
					now := e.now()
					tw := newTable(e.out)
					fmt.Fprintln(tw, "id\treference\tcommunity\tgrants_admin\tmax_uses\tused\tcreated\texpires\tstatus")
					for _, i := range rows {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s\n", i.ID, inviteRef(i.CodeHash), idPtr(i.CommunityID),
							i.GrantsAdmin, inviteMaxUses(i), i.UsedCount, stamp(i.Created), inviteExpiry(i), inviteStatus(i, now))
					}
					return tw.Flush()
				},
			}
		})
	case "revoke":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			idFlag := fs.String("id", "", "the invite's id (32 hex), as `invite list` prints it")
			var iid id.ID
			return adminAction{
				check: func() error {
					var err error
					iid, err = parseID(stderr, name, "id", *idFlag)
					return err
				},
				run: func(ctx context.Context, e *adminEnv) error { return revokeInvite(ctx, e, iid) },
			}
		})
	default:
		return unknownVerb("invite", verb, stderr)
	}
}

// createInvite mints the code with the same generator the API uses, prints `https://<domain>/i/<code>`
// exactly once on stdout - first, and alone on its line, so a script can take it - stores only the
// SHA-256, and records and logs the 8-hex reference, never the code.
func createInvite(ctx context.Context, e *adminEnv, maxUses uint64, ttl time.Duration, community *id.ID, grantsAdmin bool) error {
	if community != nil {
		if _, err := e.repo.GetCommunity(ctx, *community); err != nil {
			return notFound("community "+community.String(), err)
		}
	}
	code, hash := auth.NewInviteCode()
	now := e.now()
	row := store.InviteRow{
		ID: id.New(), CodeHash: hash, CommunityID: community, MaxUses: maxUses,
		Created: now, ExpiresAt: now + int64(ttl/time.Second),
	}
	if grantsAdmin {
		row.GrantsAdmin = 1
	}
	scope := "instance"
	if community != nil {
		scope = community.String()
	}
	if err := e.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateInvite(ctx, row); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditRow{
			Action: "invite.create", Target: row.ID.String(), Detail: scope + " " + inviteRef(hash), At: now,
		})
	}); err != nil {
		return fmt.Errorf("create invite: %w: %w", err, exit.Software)
	}
	e.log.Info("invite created", "invite", row.ID.String(), "ref", inviteRef(hash), "scope", scope)
	fmt.Fprintf(e.out, "https://%s/i/%s\n", e.cfg.Instance.Domain, code)
	fmt.Fprintf(e.out, "invite %s: %d uses, expires %s, reference %s\n", row.ID, maxUses, stamp(row.ExpiresAt), inviteRef(hash))
	fmt.Fprintln(e.out, "this link is printed once and is not stored anywhere; the database holds only its SHA-256")
	return nil
}

func revokeInvite(ctx context.Context, e *adminEnv, iid id.ID) error {
	rows, err := e.repo.ListInvites(ctx, nil)
	if err != nil {
		return fmt.Errorf("list invites: %w: %w", err, exit.Software)
	}
	var inv *store.InviteRow
	for i := range rows {
		if rows[i].ID == iid {
			inv = &rows[i]
			break
		}
	}
	if inv == nil {
		return notFound("invite "+iid.String(), store.ErrNotFound)
	}
	if inv.RevokedAt != nil {
		// RevokeInvite overwrites revoked_at, so a second revoke would move the first one's time.
		fmt.Fprintf(e.out, "invite %s was already revoked at %s\n", iid, stamp(*inv.RevokedAt))
		return nil
	}
	now := e.now()
	if err := e.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.RevokeInvite(ctx, iid, now); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditRow{
			Action: "invite.revoke", Target: iid.String(), Detail: idPtrScope(inv.CommunityID) + " " + inviteRef(inv.CodeHash), At: now,
		})
	}); err != nil {
		return fmt.Errorf("revoke invite %s: %w: %w", iid, err, exit.Software)
	}
	e.log.Info("invite revoked", "invite", iid.String(), "ref", inviteRef(inv.CodeHash))
	fmt.Fprintf(e.out, "revoked invite %s (reference %s)\n", iid, inviteRef(inv.CodeHash))
	return nil
}

func idPtrScope(c *id.ID) string {
	if c == nil {
		return "instance"
	}
	return c.String()
}

// ---------------------------------------------------------------------------------------------
// community

func adminCommunity(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin community " + verb
	switch verb {
	case "list":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			limit := fs.Int("limit", 0, "list at most this many communities (default: all)")
			after := fs.String("after", "", "list the communities whose id sorts after this one")
			var cursor id.ID
			return adminAction{
				check: func() error {
					if *limit < 0 {
						return usageError(stderr, name, "--limit must not be negative")
					}
					if *after != "" {
						var err error
						cursor, err = parseID(stderr, name, "after", *after)
						return err
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error {
					tw := newTable(e.out)
					fmt.Fprintln(tw, "id\tname\towner\tpolicy_version\tmin_account_age_seconds\trequire_mod_2fa\tcreated")
					listed := 0
					for *limit == 0 || listed < *limit {
						n := pageSize
						if *limit > 0 {
							n = min(n, *limit-listed)
						}
						rows, err := e.repo.ListCommunities(ctx, cursor, int32(n))
						if err != nil {
							return fmt.Errorf("list communities: %w: %w", err, exit.Software)
						}
						for _, c := range rows {
							fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", c.ID, orDash(c.Name), c.Owner,
								c.PolicyVersion, c.MinAccountAgeSeconds, c.RequireMod2FA, stamp(c.Created))
							cursor = c.ID
						}
						listed += len(rows)
						if len(rows) < n {
							break
						}
					}
					return tw.Flush()
				},
			}
		})
	case "show":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			idFlag := fs.String("id", "", "the community's id (32 hex)")
			var cid id.ID
			return adminAction{
				check: func() error {
					var err error
					cid, err = parseID(stderr, name, "id", *idFlag)
					return err
				},
				run: func(ctx context.Context, e *adminEnv) error { return showCommunity(ctx, e, cid) },
			}
		})
	default:
		return unknownVerb("community", verb, stderr)
	}
}

func showCommunity(ctx context.Context, e *adminEnv, cid id.ID) error {
	c, err := e.repo.GetCommunity(ctx, cid)
	if err != nil {
		return notFound("community "+cid.String(), err)
	}
	members := 0
	var after id.ID
	for {
		page, err := e.repo.ListMembersOfCommunity(ctx, cid, after, pageSize)
		if err != nil {
			return fmt.Errorf("list members: %w: %w", err, exit.Software)
		}
		members += len(page)
		if len(page) < pageSize {
			break
		}
		after = page[len(page)-1].UserID
	}
	channels, err := e.repo.ListChannels(ctx, cid)
	if err != nil {
		return fmt.Errorf("list channels: %w: %w", err, exit.Software)
	}
	invites, err := e.repo.ListInvites(ctx, &cid)
	if err != nil {
		return fmt.Errorf("list invites: %w: %w", err, exit.Software)
	}
	kv(e.out, "id", c.ID)
	kv(e.out, "name", orDash(c.Name))
	kv(e.out, "owner", c.Owner)
	kv(e.out, "created", stamp(c.Created))
	kv(e.out, "policy_version", c.PolicyVersion)
	kv(e.out, "min_account_age_seconds", c.MinAccountAgeSeconds)
	kv(e.out, "require_mod_2fa", c.RequireMod2FA)
	kv(e.out, "members", members)
	kv(e.out, "channels", len(channels))
	kv(e.out, "invites", len(invites))
	kv(e.out, "policy", orDash(string(c.PolicyJSON)))
	return nil
}

// ---------------------------------------------------------------------------------------------
// device

func adminDevice(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin device " + verb
	switch verb {
	case "revoke":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			// --id is the device's id without --all, and the user's id with it; --username
			// names the user and goes with --all only. Both selectors come from userSelector,
			// the flags the other user verbs take.
			sel := userSelector{}.register(fs)
			all := fs.Bool("all", false, "sign every device of the user named by --id or --username out (the recovery reset)")
			yes := fs.Bool("yes", false, "with --all: revoke without typing the username back")
			var did id.ID
			return adminAction{
				check: func() error {
					if *all {
						return sel.check(stderr, name)
					}
					if *sel.username != "" {
						return usageError(stderr, name, "--username goes with --all; a single device is named with --id")
					}
					if *yes {
						return usageError(stderr, name, "--yes goes with --all")
					}
					var err error
					did, err = parseID(stderr, name, "id", *sel.id)
					return err
				},
				run: func(ctx context.Context, e *adminEnv) error {
					if *all {
						return revokeAllDevices(ctx, e, sel, *yes)
					}
					return revokeDevice(ctx, e, did)
				},
			}
		})
	default:
		return unknownVerb("device", verb, stderr)
	}
}

// revokeDevice revokes the device and deletes its sessions in one transaction. The running
// gateway closes the device's sockets on its next liveness tick (see setUserDisabled). What it does
// not do is publish a new signed device list: that needs the user's own key, so the list clients
// read still names the device until its owner (or another of their devices) removes it.
func revokeDevice(ctx context.Context, e *adminEnv, did id.ID) error {
	d, err := e.repo.GetDevice(ctx, did)
	if err != nil {
		return notFound("device "+did.String(), err)
	}
	if d.RevokedAt != nil {
		// RevokeDevice overwrites revoked_at, so a second revoke would move the first one's time.
		fmt.Fprintf(e.out, "device %s was already revoked at %s\n", did, stamp(*d.RevokedAt))
		return nil
	}
	now := e.now()
	var sessions int64
	if err := e.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.RevokeDevice(ctx, did, now); err != nil {
			return err
		}
		n, err := tx.DeleteSessionsByDevice(ctx, did)
		if err != nil {
			return err
		}
		sessions = n
		return tx.Audit(ctx, store.AuditRow{Action: "device.revoke", Target: did.String(), Detail: d.UserID.String(), At: now})
	}); err != nil {
		return fmt.Errorf("revoke device %s: %w: %w", did, err, exit.Software)
	}
	e.log.Info("device revoked", "device", did.String(), "user", d.UserID.String(), "sessions_deleted", sessions)
	fmt.Fprintf(e.out, "revoked device %s of user %s; %d sessions deleted, live connections close within one heartbeat interval\n",
		did, d.UserID, sessions)
	return nil
}

// adminIn is where `device revoke --all` reads the typed confirmation, and adminInTerminal says
// whether it is a terminal the operator can answer on. Tests replace both.
var (
	adminIn         io.Reader = os.Stdin
	adminInTerminal           = func() bool { return stdinIsTerminal(os.Stdin) }
)

// stdinIsTerminal reports whether f is a character device, which is what a terminal is for this.
func stdinIsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// revokeAllDevices is `dillad admin device revoke --all` (ruling R1, the recovery reset). The user
// is named by --id or --username (sel). It resolves the user first and prints who it is and how
// many devices are active; a deleted user is refused and a user with no active device has nothing
// to revoke. Otherwise it prints the history warning and asks for the username typed back, unless
// yes is given. Without yes and without a terminal to answer on, it refuses. Then ops.RevokeAll
// Devices signs every device out and deletes every session, in one audited transaction. Like
// revokeDevice it publishes no new signed device list (that needs the person's own key).
func revokeAllDevices(ctx context.Context, e *adminEnv, sel userSelector, yes bool) error {
	u, err := sel.resolve(ctx, e.repo)
	if err != nil {
		return err
	}
	if u.DeletedAt != nil {
		return fmt.Errorf("user %s is deleted: %w", u.ID, exit.Data)
	}
	devices, err := e.repo.ListDevicesByUser(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("list devices: %w: %w", err, exit.Software)
	}
	active := 0
	for _, d := range devices {
		if d.RevokedAt == nil {
			active++
		}
	}
	fmt.Fprintf(e.out, "user %s (%s): %d active devices\n", printable(u.Username), u.ID, active)
	if active == 0 {
		fmt.Fprintln(e.out, "nothing to revoke: no active devices")
		return nil
	}
	fmt.Fprintln(e.out, "Encrypted history is lost unless another device still holds it, and after this none of the person's devices is active.")
	if !yes {
		if !adminInTerminal() {
			return usageError(e.errw, "admin device revoke", "refusing to revoke without a terminal to confirm on; pass --yes")
		}
		fmt.Fprintf(e.out, "type %s to sign every device out: ", printable(u.Username))
		line, _ := bufio.NewReader(adminIn).ReadString('\n')
		if strings.TrimRight(line, "\r\n") != u.Username {
			return fmt.Errorf("the typed name did not match %s; nothing revoked: %w", printable(u.Username), exit.Fail)
		}
	}
	res, err := ops.RevokeAllDevices(ctx, e.repo, u.ID, e.now())
	if err != nil {
		return fmt.Errorf("revoke every device of %s: %w: %w", u.ID, err, exit.Software)
	}
	e.log.Info("all devices revoked", "user", u.ID.String(), "devices_revoked", len(res.Revoked), "sessions_deleted", res.Sessions)
	fmt.Fprintf(e.out, "revoked %d devices of user %s (%s); %d sessions deleted, live connections close within one heartbeat interval\n",
		len(res.Revoked), u.ID, printable(u.Username), res.Sessions)
	for _, d := range res.Revoked {
		fmt.Fprintf(e.out, "  device %s\n", d)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// blob

func adminBlob(verb string, args []string, stdout, stderr io.Writer) error {
	name := "admin blob " + verb
	switch verb {
	case "purge":
		return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
			idFlag := fs.String("id", "", "the blob id: 64 hex, the SHA-256 of the ciphertext")
			reason := fs.String("reason", "", "why; recorded in the audit log and the tombstone")
			var blobID []byte
			return adminAction{
				check: func() error {
					raw, err := hex.DecodeString(*idFlag)
					if err != nil || len(raw) != sha256Size {
						return usageError(stderr, name, "--id must be 64 hex digits (the blob's SHA-256)")
					}
					blobID = raw
					if !blob.ValidPurgeReason(*reason) {
						return usageError(stderr, name, "--reason is required: 1..%d bytes with no NUL", blob.MaxPurgeReasonBytes)
					}
					return nil
				},
				run: func(ctx context.Context, e *adminEnv) error {
					bs, err := blob.Open(e.cfg.Blobs.Dir, e.cfg.Blobs.Backend)
					if err != nil {
						return fmt.Errorf("open blob store %s: %w: %w", e.cfg.Blobs.Dir, err, exit.CantCreate)
					}
					defer func() { _ = bs.Close() }()
					// The CLI is the operator, not a users row: By is the zero id, so the audit row
					// names no actor, exactly as every other verb's does.
					res, err := blob.Purge(ctx, e.repo, bs, blob.PurgeRequest{BlobID: blobID, Reason: *reason, At: e.now()})
					if err != nil {
						return fmt.Errorf("purge %s: %w: %w", *idFlag, err, exit.Software)
					}
					if res.BackupKept {
						// A user's sealed backup object, not an attachment: the root is written once,
						// so unlinking it would make that account's recovery impossible. Every channel
						// reference is still gone and the id tombstoned (BACKUPS-RECOVERY-05; this was
						// exit 65 before).
						e.log.Info("blob references purged; a backup object keeps the bytes", "blob_id", *idFlag)
						fmt.Fprintf(e.out, "removed every channel reference to blob %s and tombstoned its id; a backup object names these bytes, so the file is kept until that backup is replaced\n", *idFlag)
						return nil
					}
					if res.UnlinkErr != nil {
						fmt.Fprintf(e.errw, "dillad admin blob purge: the row and every reference are gone, but unlinking the file failed: %v\n"+
							"  the tombstone refuses the bytes on every route; `dillad doctor` reports the file as an orphan\n", res.UnlinkErr)
					}
					e.log.Info("blob purged", "blob_id", *idFlag)
					fmt.Fprintf(e.out, "purged blob %s; its id is tombstoned, so the same bytes cannot be uploaded again\n", *idFlag)
					fmt.Fprintln(e.out, "note: attachments are encrypted under a random key each, so this removes these bytes, not the content")
					return nil
				},
			}
		})
	default:
		return unknownVerb("blob", verb, stderr)
	}
}

// sha256Size is the length of a blob id.
const sha256Size = 32

// ---------------------------------------------------------------------------------------------
// audit

// adminAudit is `dillad admin audit [--since=UNIX] [--limit=N]`: the audit log, newest first. It is
// read-only, and it is the one noun with no verb.
func adminAudit(args []string, stdout, stderr io.Writer) error {
	const name = "admin audit"
	return runVerb(name, args, stdout, stderr, func(fs *flag.FlagSet) adminAction {
		since := fs.Int64("since", 0, "only rows at or after this unix time")
		limit := fs.Int("limit", 100, "at most this many rows, newest first")
		return adminAction{
			check: func() error {
				if *since < 0 {
					return usageError(stderr, name, "--since must not be negative")
				}
				if *limit < 1 {
					return usageError(stderr, name, "--limit must be at least 1")
				}
				return nil
			},
			run: func(ctx context.Context, e *adminEnv) error {
				rows, err := e.repo.ListAudit(ctx, *since, int32(min(*limit, 1<<20))) //nolint:gosec // G115: bounded by min
				if err != nil {
					return fmt.Errorf("list audit: %w: %w", err, exit.Software)
				}
				tw := newTable(e.out)
				fmt.Fprintln(tw, "at\tactor\taction\ttarget\tdetail")
				for _, r := range rows {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", stamp(r.At), idPtr(r.Actor), printable(r.Action), printable(r.Target), orDash(r.Detail))
				}
				return tw.Flush()
			},
		}
	})
}
