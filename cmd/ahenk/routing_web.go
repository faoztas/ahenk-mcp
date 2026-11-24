// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const (
	routingWebIdle     = 5 * time.Minute
	routingWebLifetime = 15 * time.Minute
)

func runRoutingWeb(args []string) error {
	flags := flag.NewFlagSet("routing web", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "path to Ahenk configuration")
	repoID := flags.String("repo", "", "registered repository ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *config == "" || *repoID == "" {
		return errors.New("usage: ahenk routing web --config FILE --repo ID")
	}
	cfg, err := model.LoadConfig(*config)
	if err != nil {
		return errors.New(routingConfigError)
	}
	if _, ok := cfg.Repositories[*repoID]; !ok {
		return errors.New("routing web requires a registered repository")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return serveRoutingWeb(ctx, cfg, *repoID, routingWebIdle, announceRoutingWeb)
}

func announceRoutingWeb(origin, code string) error {
	// Keep the bootstrap code out of standard streams and URLs.
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return errors.New("routing web requires an interactive terminal for operator sign-in")
	}
	defer tty.Close()
	if _, err := fmt.Fprintf(tty, "Open %s in your browser.\nOne-time sign-in code: %s\n", origin, code); err != nil {
		return errors.New("could not display routing web sign-in details")
	}
	return nil
}

func serveRoutingWeb(ctx context.Context, cfg model.Config, repoID string, idle time.Duration, announce func(string, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, routingWebLifetime)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	host := listener.Addr().String()
	code := rand.Text()
	panel := newRoutingWebPanel(host, cfg, repoID, code, idle)
	if err := announce(panel.origin, code); err != nil {
		return err
	}
	server := &http.Server{
		Handler:           panel,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	timer := time.NewTimer(idle)
	defer timer.Stop()
wait:
	for {
		select {
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			break wait
		case <-timer.C:
			panel.mu.Lock()
			remaining := idle - time.Since(panel.lastActive)
			panel.mu.Unlock()
			if remaining <= 0 {
				break wait
			}
			timer.Reset(remaining)
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return server.Shutdown(shutdown)
}

type routingWebPanel struct {
	host   string
	origin string
	cfg    model.Config
	repoID string
	idle   time.Duration

	mu         sync.Mutex
	bootstrap  string
	session    string
	lastActive time.Time
}

func newRoutingWebPanel(host string, cfg model.Config, repoID string, bootstrap string, idle time.Duration) *routingWebPanel {
	return &routingWebPanel{
		host: host, origin: "http://" + host, cfg: cfg, repoID: repoID, idle: idle,
		bootstrap: bootstrap, lastActive: time.Now(),
	}
}

func (p *routingWebPanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	origins := r.Header.Values("Origin")
	needsOrigin := r.Method == http.MethodPost || len(origins) > 0
	validOrigin := len(origins) == 1 && origins[0] == p.origin
	if r.Host != p.host || needsOrigin && !validOrigin {
		http.Error(w, "Local panel origin required", http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "Query parameters are unsupported", http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/", "/panel.js":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			break
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, routingWebHTML)
		} else {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			io.WriteString(w, routingWebScript)
		}
		return
	case "/session":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			break
		}
		p.startSession(w, r)
		return
	case "/preference":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			break
		}
		if !p.authorized(r) {
			http.Error(w, "Operator session required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			p.readPreference(w)
		} else {
			p.writePreference(w, r)
		}
		return
	default:
		http.NotFound(w, r)
		return
	}
	http.Error(w, "Method is unsupported", http.StatusMethodNotAllowed)
}

func (p *routingWebPanel) startSession(w http.ResponseWriter, r *http.Request) {
	code := routingWebForm(r, w).Get("code")
	p.mu.Lock()
	if r.Context().Err() != nil || time.Since(p.lastActive) >= p.idle ||
		p.bootstrap == "" || subtle.ConstantTimeCompare([]byte(code), []byte(p.bootstrap)) != 1 {
		p.mu.Unlock()
		http.Error(w, "Invalid or expired sign-in code", http.StatusUnauthorized)
		return
	}
	session := rand.Text()
	p.bootstrap = ""
	p.session = session
	p.lastActive = time.Now()
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Session string `json:"session"`
	}{Session: session})
}

func (p *routingWebPanel) authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Context().Err() != nil || time.Since(p.lastActive) >= p.idle || p.session == "" ||
		subtle.ConstantTimeCompare([]byte(values[0]), []byte("Bearer "+p.session)) != 1 {
		return false
	}
	p.lastActive = time.Now()
	return true
}

func routingWebForm(r *http.Request, w http.ResponseWriter) url.Values {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 {
		return nil
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			return nil
		}
	}
	return r.PostForm
}

func (p *routingWebPanel) readPreference(w http.ResponseWriter) {
	stateDir, err := store.RoutingPreferenceStateDir(p.cfg)
	if err != nil {
		http.Error(w, "Repository preference storage is unavailable", http.StatusInternalServerError)
		return
	}
	repo := p.cfg.Repositories[p.repoID]
	profile, source, err := orchestration.ResolveDefaultRoutingProfile(stateDir, p.repoID, repo)
	warning := ""
	if errors.Is(err, store.ErrInvalidRoutingPreference) {
		warning = "INVALID_ROUTING_PREFERENCE"
	} else if err != nil {
		http.Error(w, "Could not read the repository preference", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Project string               `json:"project"`
		Profile model.RoutingProfile `json:"profile"`
		Source  string               `json:"source"`
		Warning string               `json:"warning,omitempty"`
	}{Project: p.repoID, Profile: profile, Source: source, Warning: warning})
}

func (p *routingWebPanel) writePreference(w http.ResponseWriter, r *http.Request) {
	form := routingWebForm(r, w)
	var value *string
	switch profile := form.Get("profile"); {
	case model.RoutingProfile(profile).Valid():
		value = &profile
	case form.Get("reset") == "1":
		// A nil value restores the repository default.
	default:
		http.Error(w, "Select a supported profile or reset", http.StatusBadRequest)
		return
	}
	stateDir, err := store.RoutingPreferenceStateDir(p.cfg)
	if err != nil {
		http.Error(w, "Repository preference storage is unavailable", http.StatusInternalServerError)
		return
	}
	repo := p.cfg.Repositories[p.repoID]
	if err := store.SaveRoutingPreference(stateDir, p.repoID, repo.Root, value); err != nil {
		http.Error(w, "Could not save the repository preference", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const routingWebHTML = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ahenk routing preferences</title>
<script src="/panel.js" defer></script>
<main>
  <h1>Routing preferences</h1>
  <form id="sign-in" method="post" action="/session">
    <label>One-time sign-in code <input name="code" type="password" required autocomplete="off" autocapitalize="off" spellcheck="false"></label>
    <button>Sign in</button>
  </form>
  <form id="preference" method="post" action="/preference" hidden>
    <dl>
      <dt>Project</dt><dd id="project"></dd>
      <dt>Profile</dt><dd id="profile"></dd>
      <dt>Source</dt><dd id="source"></dd>
    </dl>
    <fieldset>
      <legend>Routing mode</legend>
      <button name="profile" value="price_performance">Price/performance</button>
      <button name="profile" value="quality">Quality</button>
      <button name="profile" value="price">Price</button>
      <button name="reset" value="1">Reset</button>
    </fieldset>
  </form>
  <p id="status" role="status"></p>
</main>
</html>
`

// The session stays in the panel's origin, since cookies span loopback ports.
const routingWebScript = `
const login = document.getElementById("sign-in");
const preference = document.getElementById("preference");
const statusMessage = document.getElementById("status");
let session = sessionStorage.getItem("ahenk-routing-session");
let busy = false;

async function loadPreference() {
  const response = await fetch("/preference", {
    headers: {Authorization: "Bearer " + session}
  });
  if (response.status === 401) {
    sessionStorage.removeItem("ahenk-routing-session");
    session = null;
    preference.hidden = true;
    login.hidden = true;
    statusMessage.textContent = "Session expired. Restart the panel to sign in.";
    return;
  }
  if (!response.ok) throw new Error();
  const {project, profile, source, warning} = await response.json();
  document.getElementById("project").textContent = project;
  document.getElementById("profile").textContent = profile || "Legacy";
  document.getElementById("source").textContent = source;
  statusMessage.textContent = warning === "INVALID_ROUTING_PREFERENCE"
    ? "The saved preference is invalid. The default profile is active."
    : "";
  login.hidden = true;
  preference.hidden = false;
}

login.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (busy) return;
  busy = true;
  statusMessage.textContent = "";
  try {
    const body = new URLSearchParams(new FormData(login));
    login.reset();
    const response = await fetch("/session", {method: "POST", body});
    if (!response.ok) {
      statusMessage.textContent = "Sign-in failed. Check the one-time code.";
      return;
    }
    ({session} = await response.json());
    sessionStorage.setItem("ahenk-routing-session", session);
    login.hidden = true;
    preference.hidden = false;
    await loadPreference();
  } catch {
    statusMessage.textContent = "Could not load the routing preference.";
  } finally {
    busy = false;
  }
});

preference.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (busy) return;
  busy = true;
  statusMessage.textContent = "";
  try {
    const body = new URLSearchParams(new FormData(preference, event.submitter));
    const response = await fetch("/preference", {
      method: "POST", body, headers: {Authorization: "Bearer " + session}
    });
    if (response.status !== 401 && !response.ok) throw new Error();
    await loadPreference();
  } catch {
    statusMessage.textContent = "Could not update the routing preference.";
  } finally {
    busy = false;
  }
});

if (session) {
  login.hidden = true;
  preference.hidden = false;
  busy = true;
  loadPreference().catch(() => {
    statusMessage.textContent = "Could not load the routing preference.";
  }).finally(() => { busy = false; });
}
`
