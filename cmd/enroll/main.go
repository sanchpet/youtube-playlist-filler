// Command enroll obtains the refresh token the service runs on. It is run once, by hand, on a
// machine with a browser — never in the cluster, and never by the service.
//
// The redirect is a loopback listener rather than a copy-and-paste code: Google retired the
// out-of-band flow, and a desktop client is allowed any port on 127.0.0.1, so nothing has to be
// registered ahead of time. PKCE is used even though this client has a secret, because the
// authorization code travels over plain HTTP on the loopback interface where any other process on
// the machine could bind the port first.
//
//	YT_CLIENT_ID=... YT_CLIENT_SECRET=... go run ./cmd/enroll
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/youtube/v3"
)

// consentTimeout bounds the wait for the browser. Long enough to pick an account and read the
// consent screen, short enough that a forgotten terminal does not hold a listener open all day.
const consentTimeout = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(1)
	}
}

func run() error {
	clientID, clientSecret := os.Getenv("YT_CLIENT_ID"), os.Getenv("YT_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errors.New("YT_CLIENT_ID and YT_CLIENT_SECRET are required")
	}

	// Port zero: the kernel picks a free one and the redirect URI is built from what it picked.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("loopback listener: %w", err)
	}
	defer func() { _ = ln.Close() }()

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{youtube.YoutubeScope},
		RedirectURL:  fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port),
	}

	state, err := nonce()
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, consentTimeout)
	defer cancel()

	// AccessTypeOffline is what makes a refresh token be issued at all; ApprovalForce is what makes
	// one be issued *again* on a re-run — Google returns a refresh token only on the first consent
	// for a given client, so without it a second enrolment silently yields an access token and
	// nothing to store.
	url := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier))

	fmt.Fprintln(os.Stderr, "Open this URL, grant access, and come back:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  "+url)
	fmt.Fprintln(os.Stderr, "")

	code, err := await(ctx, ln, state)
	if err != nil {
		return err
	}

	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("no refresh token returned: revoke this app's access at " +
			"https://myaccount.google.com/permissions and run again")
	}

	// stdout carries the token and nothing else, so it can be piped straight into a secret store.
	fmt.Println(tok.RefreshToken)
	fmt.Fprintln(os.Stderr, "Store this as YT_REFRESH_TOKEN.")
	return nil
}

// await serves exactly one callback and returns the authorization code from it.
func await(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case q.Get("error") != "":
				http.Error(w, "authorization refused: "+q.Get("error"), http.StatusBadRequest)
				done <- result{err: fmt.Errorf("authorization refused: %s", q.Get("error"))}
			case q.Get("state") != state:
				// Not pedantry: the listener is on a port any local process can reach, and a
				// mismatched state means this callback was not the one we sent someone to.
				http.Error(w, "state mismatch", http.StatusBadRequest)
				done <- result{err: errors.New("state mismatch")}
			case q.Get("code") == "":
				http.Error(w, "no code", http.StatusBadRequest)
				done <- result{err: errors.New("callback carried no code")}
			default:
				_, _ = fmt.Fprintln(w, "Done. You can close this tab.")
				done <- result{code: q.Get("code")}
			}
		}),
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			done <- result{err: err}
		}
	}()
	defer func() { _ = srv.Close() }()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for the callback: %w", ctx.Err())
	case r := <-done:
		return r.code, r.err
	}
}

func nonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
