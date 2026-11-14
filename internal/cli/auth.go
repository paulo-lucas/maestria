package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/maestria-dev/maestria/internal/audit"
	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/store"
)

func cmdAuthSetup(args []string, env *Env) error {
	fs := newFlagSet("auth setup", env)
	force := fs.Bool("force", false, "replace existing credentials")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("Usage: maestria auth setup [--force]")
	}
	return runAuthSetup(bufio.NewReader(env.Stdin), *force, env)
}

func runAuthSetup(in *bufio.Reader, force bool, env *Env) error {
	dir := configDir(env)
	existing, err := auth.LoadSecrets(dir)
	if err != nil {
		return err
	}
	if existing != nil && !force {
		return fmt.Errorf("credentials already exist. Re-run with --force to replace them (this signs out every session)")
	}
	out := env.Stdout

	var password string
	for {
		pw, err := env.readSecret(in, fmt.Sprintf("New password (at least %d characters): ", auth.MinPasswordLength))
		if err != nil {
			return err
		}
		if err := auth.ValidateNewPassword(pw); err != nil {
			fmt.Fprintf(out, "%s.\n", capitalize(err.Error()))
			continue
		}
		again, err := env.readSecret(in, "Repeat password: ")
		if err != nil {
			return err
		}
		if again != pw {
			fmt.Fprintln(out, "The passwords do not match. Please try again.")
			continue
		}
		password = pw
		break
	}

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		return err
	}
	qrText, err := RenderQR(auth.OTPAuthURI(secret, "panel", "Maestria"))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nScan this QR code with your authenticator app:\n\n%s\nOr enter the key manually: %s\n\n", qrText, secret)

	for {
		code, err := env.readLine(in, "Enter the 6-digit code shown by the app to confirm: ")
		if err != nil {
			return err
		}
		if _, ok := auth.VerifyTOTP(secret, strings.TrimSpace(code), env.now(), 1); ok {
			break
		}
		fmt.Fprintln(out, "That code is not valid. Check the 6-digit code in your app and try again.")
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	codes, err := auth.GenerateRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		return err
	}
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := auth.SaveSecrets(dir, &auth.Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: env.now().UTC().Format("2006-01-02T15:04:05Z")}); err != nil {
		return err
	}
	if err := auth.ReplaceRecoveryCodes(db, codes, env.now()); err != nil {
		return err
	}
	cfg, err := config.Load(dir, env.Config)
	if err != nil {
		return err
	}
	sessions := auth.NewSessionStore(db, 0, 0, env.Now)
	dropped, err := sessions.DestroyAll()
	if err != nil {
		return err
	}
	// A fresh TOTP secret restarts replay protection.
	if err := auth.ResetTOTPReplay(db); err != nil {
		return err
	}
	_ = audit.New(db, env.Now).Write("auth_setup", "", map[string]any{"replaced": existing != nil, "sessionsDropped": dropped, "via": "cli"})

	fmt.Fprintf(out, "\nRecovery codes (each works once; they are shown only now, keep them in a password manager):\n\n")
	for _, c := range codes {
		fmt.Fprintf(out, "  %s\n", c)
	}
	fmt.Fprintf(out, "\nDone. Credentials saved with 0600 permissions. Start the panel with: maestria serve\n")
	if cfg.CookieSecure {
		fmt.Fprintf(out, "Note: cookieSecure is on (the default), which needs https. For plain http://127.0.0.1 in Safari, set \"cookieSecure\": false in %s.\n", config.FilePath(dir))
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func cmdAuthResetSessions(args []string, env *Env) error {
	fs := newFlagSet("auth reset-sessions", env)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("Usage: maestria auth reset-sessions")
	}
	dir := configDir(env)
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := auth.NewSessionStore(db, 0, 0, env.Now).DestroyAll()
	if err != nil {
		return err
	}
	_ = audit.New(db, env.Now).Write("auth_reset_sessions", "", map[string]any{"sessions": n, "via": "cli"})
	plural := "s"
	if n == 1 {
		plural = ""
	}
	fmt.Fprintf(env.Stdout, "Invalidated %d session%s.\n", n, plural)
	return nil
}
