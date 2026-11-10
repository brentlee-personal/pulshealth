package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// `ingest devices …`: the operator's side of per-device tokens (SRV-8). The
// image is distroless, so this runs as
//
//	docker compose run --rm --no-deps ingest devices <subcommand>
//
// (`make devices ARGS='…'` from the repository root), reusing the container's
// DATABASE_URL_FILE. Pairing text on stdout, errors on stderr, exit 2 on a usage
// error and 1 on anything else.
//
// `issue` also prints the pairing code for the token it just minted (qr.go,
// pairing.go) — the one moment that is possible, since only the hash is kept.
// The code needs the URL the phone will use, which a container cannot work
// out for itself (it sees neither the TLS proxy in front of it nor the host's
// LAN address): --url, else PULS_PUBLIC_URL, which Compose hands over from
// server/.env. `make devices` and `scripts/bootstrap.sh --issue-device` fill
// it in from the same rules the pairing block uses.

const devicesUsage = `usage: ingest devices <command>

  list [--all]                        active tokens; --all includes revoked ones
  issue --user <uuid> --name <label>  mint a token for a user (creates the user
        [--url <url>] [--no-qr]       row if needed — a mistyped UUID makes a
                                      new user) and print it once, with the
                                      QR code the app scans. --url is the URL
                                      the phone will use (default:
                                      $PULS_PUBLIC_URL); --no-qr prints the
                                      payload as text only
  revoke <id>                         refuse the token from the next request on
  rename <id> <label>                 change a token's label
`

// devicesCommand is a parsed `devices` invocation.
type devicesCommand struct {
	op     string // list | issue | revoke | rename
	all    bool
	userID string
	name   string
	id     int64
	// issue only. url is the normalized URL for the pairing code, "" when
	// neither --url nor PULS_PUBLIC_URL supplied one.
	url  string
	noQR bool
}

// parseDevicesArgs is pure so it can be unit-tested without a database.
func parseDevicesArgs(args []string) (devicesCommand, error) {
	if len(args) == 0 {
		return devicesCommand{}, errors.New("missing command")
	}
	cmd := devicesCommand{op: args[0]}
	rest := args[1:]
	switch cmd.op {
	case "list":
		for _, a := range rest {
			switch a {
			case "--all", "-a":
				cmd.all = true
			default:
				return devicesCommand{}, fmt.Errorf("list: unknown argument %q", a)
			}
		}
	case "issue":
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--no-qr" {
				cmd.noQR = true
				continue
			}
			key, value, inline := strings.Cut(rest[i], "=")
			if !inline {
				if i+1 >= len(rest) {
					return devicesCommand{}, fmt.Errorf("issue: %s needs a value", key)
				}
				i++
				value = rest[i]
			}
			switch key {
			case "--user":
				// Lower case is how the database hands a uuid back and how
				// the app stores its own, so the pairing code agrees with both.
				cmd.userID = strings.ToLower(value)
			case "--name":
				cmd.name = value
			case "--url":
				normalized, err := normalizePairingURL(value)
				if err != nil {
					return devicesCommand{}, fmt.Errorf("issue: --url: %w", err)
				}
				cmd.url = normalized
			default:
				return devicesCommand{}, fmt.Errorf("issue: unknown argument %q", key)
			}
		}
		if cmd.userID == "" {
			return devicesCommand{}, errors.New("issue: --user <uuid> is required")
		}
		if !isUUID(cmd.userID) {
			return devicesCommand{}, fmt.Errorf("issue: --user %q is not a UUID", cmd.userID)
		}
		if cmd.name == "" {
			return devicesCommand{}, errors.New("issue: --name <label> is required")
		}
		if err := validateDeviceName(cmd.name); err != nil {
			return devicesCommand{}, fmt.Errorf("issue: %w", err)
		}
	case "revoke", "rename":
		want := 1
		if cmd.op == "rename" {
			want = 2
		}
		if len(rest) != want {
			return devicesCommand{}, fmt.Errorf("%s: wrong number of arguments", cmd.op)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || id <= 0 {
			return devicesCommand{}, fmt.Errorf("%s: %q is not a token id", cmd.op, rest[0])
		}
		cmd.id = id
		if cmd.op == "rename" {
			cmd.name = rest[1]
			if err := validateDeviceName(cmd.name); err != nil {
				return devicesCommand{}, fmt.Errorf("rename: %w", err)
			}
		}
	default:
		return devicesCommand{}, fmt.Errorf("unknown command %q", cmd.op)
	}
	return cmd, nil
}

// runDevicesCLI is main's branch for `ingest devices …`; it returns the
// process exit code.
func runDevicesCLI(args []string, stdout, stderr io.Writer) int {
	cmd, err := parseDevicesArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ingest devices: %v\n\n%s", err, devicesUsage)
		return 2
	}
	// Settled before the database is touched: a URL the app would refuse
	// must stop the command while there is still no token to throw away.
	if cmd.op == "issue" && cmd.url == "" {
		if raw := strings.TrimSpace(os.Getenv("PULS_PUBLIC_URL")); raw != "" {
			normalized, err := normalizePairingURL(raw)
			if err != nil {
				fmt.Fprintf(stderr, "ingest devices: PULS_PUBLIC_URL: %v\nFix it in server/.env, or pass --url. No token was issued.\n", err)
				return 1
			}
			cmd.url = normalized
		}
	}
	dbURL, err := readDatabaseURL(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "ingest devices:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Connection chatter belongs on stderr: stdout is the answer.
	pool, err := connectWithRetry(ctx, dbURL, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "ingest devices: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := runDevicesCommand(ctx, NewStore(pool), cmd, stdout); err != nil {
		fmt.Fprintf(stderr, "ingest devices: %v\n", err)
		return 1
	}
	return 0
}

func runDevicesCommand(ctx context.Context, store *Store, cmd devicesCommand, out io.Writer) error {
	switch cmd.op {
	case "list":
		tokens, err := store.ListDeviceTokens(ctx, cmd.all)
		if err != nil {
			return err
		}
		printDeviceTokens(out, tokens, cmd.all)
	case "issue":
		plaintext, d, err := store.IssueDeviceToken(ctx, cmd.userID, cmd.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Issued device token %d (%s…) for user %s.\n\n", d.ID, d.TokenPrefix, d.UserID)
		printIssuedPairing(out, cmd, d.ID, plaintext, d.UserID)
	case "revoke":
		if err := store.RevokeDeviceToken(ctx, cmd.id); err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked device token %d; the next request that presents it gets 401.\n", cmd.id)
	case "rename":
		if err := store.RenameDeviceToken(ctx, cmd.id, cmd.name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Renamed device token %d to %q.\n", cmd.id, cmd.name)
	}
	return nil
}

// printIssuedPairing is the part of `issue` a person acts on: the three values
// the app needs and, when the URL is known, the QR code that carries them.
func printIssuedPairing(out io.Writer, cmd devicesCommand, id int64, token, userID string) {
	if cmd.url != "" {
		fmt.Fprintf(out, "  Server URL  %s\n", cmd.url)
	} else {
		fmt.Fprintln(out, "  Server URL  (not known to this command — see below)")
	}
	fmt.Fprintf(out, "  Token       %s\n  User ID     %s\n\n", token, userID)

	if cmd.url == "" {
		// The token is already minted and must not be lost to a missing
		// URL, so this is advice, not an error.
		fmt.Fprintln(out, "No QR code: it has to carry the URL the phone will use, and none was given.")
		fmt.Fprintln(out, "This token works as it is — in the app, Settings > Server, enter the server URL")
		fmt.Fprintln(out, "with the token and user ID above, then Test Connection. For a code to scan")
		fmt.Fprintf(out, "instead, revoke it (devices revoke %d) and issue again one of these ways:\n", id)
		fmt.Fprintln(out, "  scripts/bootstrap.sh --issue-device <label>   works the URL out for you")
		fmt.Fprintln(out, "  devices issue … --url <URL>                   https://<your proxy>, or")
		fmt.Fprintln(out, "                                                http://<LAN IP>:8080 on your Wi-Fi")
		fmt.Fprintln(out, "  PULS_PUBLIC_URL=<URL> in server/.env          picked up by every later issue")
	} else {
		payload := pairingPayload(cmd.url, token, userID)
		if !cmd.noQR {
			// A payload that cannot be drawn is not worth failing over
			// either: the values above pair the phone just as well.
			if err := writeQR(out, payload); err != nil {
				fmt.Fprintf(out, "(No QR code: %v.)\n", err)
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "  Pairing payload (what the QR code encodes):\n  %s\n\n", payload)
		fmt.Fprintln(out, "In the app: Settings > Server, scan the QR code or enter the three values, then")
		fmt.Fprintln(out, "Test Connection.")
	}
	fmt.Fprintln(out, "This is the only time the token is shown: only its hash is stored, so a lost")
	fmt.Fprintln(out, "token is revoked and reissued, never recovered. Keep this output private — with")
	fmt.Fprintln(out, "the token, anyone who can reach the URL can upload and delete this user's data.")
}

func printDeviceTokens(out io.Writer, tokens []deviceToken, all bool) {
	if len(tokens) == 0 {
		if all {
			fmt.Fprintln(out, "No device tokens. Issue one: ingest devices issue --user <uuid> --name <label>")
		} else {
			fmt.Fprintln(out, "No active device tokens (--all shows revoked ones). Issue one: ingest devices issue --user <uuid> --name <label>")
		}
		return
	}
	stamp := func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04")
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPREFIX\tSTATUS\tUSER\tNAME\tCREATED\tLAST SEEN")
	for _, d := range tokens {
		fmt.Fprintf(w, "%d\t%s…\t%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.TokenPrefix, d.Status, d.UserID, d.Name, stamp(&d.CreatedAt), stamp(d.LastSeenAt))
	}
	w.Flush()
}
