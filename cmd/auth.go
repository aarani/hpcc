/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/aarani/hpcc/internal/auth"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage credentials for distributed compilation",
	Long: `Hold and refresh the OAuth2 credentials hpcc uses when [remote]
dispatch is enabled. Replaces putting username/password directly into
config.toml: the cached access (and, if issued, refresh) token live in
~/.config/hpcc/token.json with 0600 permissions, and the daemon
refreshes them silently when they expire.`,
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and cache an access token",
	Long: `Prompt for username and password (or take --username), exchange them
with the tenant's IdP via OAuth2 password grant, and store the resulting
access/refresh token in ~/.config/hpcc/token.json with 0600 permissions.

The IdP discovery (token URL, client_id, scope) comes from the scheduler
via GetTenantIdP — same source the daemon uses — so this command only
needs to know your scheduler URL and tenant_id (both in config.toml).`,
	RunE: runAuthLogin,
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove the cached token",
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := tokenPath(cmd)
		if err != nil {
			return err
		}
		if err := auth.Delete(path); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", path)
		return nil
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the cached token's username and expiry",
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := tokenPath(cmd)
		if err != nil {
			return err
		}
		t, err := auth.Load(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no cached token at %s; run `hpcc auth login`", path)
			}
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "path:       %s\n", path)
		fmt.Fprintf(out, "username:   %s\n", t.Username)
		if t.ExpiresAt.IsZero() {
			fmt.Fprintln(out, "expires_at: <unknown>")
		} else {
			fmt.Fprintf(out, "expires_at: %s (%s)\n",
				t.ExpiresAt.Format(time.RFC3339),
				humanizeRemaining(time.Until(t.ExpiresAt)))
		}
		fmt.Fprintf(out, "refresh:    %s\n", yesNo(t.RefreshToken != ""))
		return nil
	},
}

func init() {
	authCmd.PersistentFlags().String("config", "", "path to hpcc config.toml (defaults to $XDG_CONFIG_HOME/hpcc/config.toml)")
	authCmd.PersistentFlags().String("token-file", "", "where to read/write the token (defaults to $XDG_CONFIG_HOME/hpcc/token.json)")

	authLoginCmd.Flags().String("username", "", "username (skip the prompt)")
	authLoginCmd.Flags().String("client-secret", "", "optional OAuth2 client_secret; most public clients leave this empty")

	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authStatusCmd)
	rootCmd.AddCommand(authCmd)
}

func runAuthLogin(cmd *cobra.Command, _ []string) error {
	cfg, err := loadAuthConfig(cmd)
	if err != nil {
		return err
	}
	if !cfg.Remote.Enabled {
		return fmt.Errorf("[remote] is not enabled in config — `hpcc auth login` only matters for distributed dispatch")
	}
	if cfg.Remote.Scheduler.URL == "" {
		return fmt.Errorf("remote.scheduler.url is required in config")
	}
	if cfg.Remote.TenantID == "" {
		return fmt.Errorf("remote.tenant_id is required in config")
	}

	username, _ := cmd.Flags().GetString("username")
	clientSecret, _ := cmd.Flags().GetString("client-secret")

	if username == "" {
		u, err := promptLine(cmd, "Username: ")
		if err != nil {
			return err
		}
		username = strings.TrimSpace(u)
	}
	if username == "" {
		return fmt.Errorf("username is required")
	}
	password, err := promptPassword(cmd, "Password: ")
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	idp, err := discoverIdP(ctx, cfg.Remote)
	if err != nil {
		return err
	}

	resp, err := auth.PasswordGrant(ctx, idp, username, password, clientSecret)
	if err != nil {
		return fmt.Errorf("oauth password grant: %w", err)
	}

	tok := auth.Token{
		Username:     username,
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ClientSecret: clientSecret,
	}
	if resp.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	}

	path, err := tokenPath(cmd)
	if err != nil {
		return err
	}
	if err := auth.Save(path, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "saved token for %s to %s\n", username, path)
	return nil
}

func loadAuthConfig(cmd *cobra.Command) (config.Config, error) {
	path, _ := cmd.Flags().GetString("config")
	if path == "" {
		if env := os.Getenv("HPCC_CONFIG"); env != "" {
			path = env
		} else {
			p, err := config.DefaultConfigPath()
			if err != nil {
				return config.Config{}, err
			}
			path = p
		}
	}
	return config.LoadConfig(path)
}

func tokenPath(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("token-file"); p != "" {
		return p, nil
	}
	return auth.DefaultPath()
}

// discoverIdP dials the scheduler over TLS and runs GetTenantIdP.
// Mirrors the daemon's dialer (same CA handling, no compression
// because GetTenantIdP responses are tiny).
func discoverIdP(ctx context.Context, rc config.RemoteConfig) (auth.IdP, error) {
	tlsCfg := &tls.Config{}
	if rc.Scheduler.CAFile != "" {
		pemBytes, err := os.ReadFile(rc.Scheduler.CAFile)
		if err != nil {
			return auth.IdP{}, fmt.Errorf("read scheduler CA %q: %w", rc.Scheduler.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return auth.IdP{}, fmt.Errorf("scheduler CA %q contains no PEM certs", rc.Scheduler.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	conn, err := grpc.NewClient(rc.Scheduler.URL, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return auth.IdP{}, fmt.Errorf("dial scheduler: %w", err)
	}
	defer conn.Close()

	sched := gen.NewSchedulerServiceClient(conn)
	resp, err := sched.GetTenantIdP(ctx, &gen.GetTenantIdPRequest{TenantId: rc.TenantID})
	if err != nil {
		return auth.IdP{}, fmt.Errorf("scheduler GetTenantIdP for tenant %q: %w", rc.TenantID, err)
	}
	if resp.TokenUrl == "" {
		return auth.IdP{}, fmt.Errorf("scheduler returned empty token_url for tenant %q", rc.TenantID)
	}
	return auth.IdP{
		TokenURL: resp.TokenUrl,
		ClientID: resp.ClientId,
		Scope:    resp.Scope,
	}, nil
}

func promptLine(cmd *cobra.Command, prompt string) (string, error) {
	fmt.Fprint(cmd.ErrOrStderr(), prompt)
	in := bufio.NewReader(cmd.InOrStdin())
	line, err := in.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func promptPassword(cmd *cobra.Command, prompt string) (string, error) {
	fmt.Fprint(cmd.ErrOrStderr(), prompt)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Non-interactive stdin: read a line plain. No-echo isn't
		// available off a pipe; we accept the trade-off so scripted
		// flows (CI, tests) can drive `hpcc auth login` via stdin.
		return promptLine(cmd, "")
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func humanizeRemaining(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	d = d.Truncate(time.Second)
	if d < time.Hour {
		return d.String()
	}
	return d.Truncate(time.Minute).String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
