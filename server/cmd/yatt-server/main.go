// Command yatt-server is the Go port of the T3 Code server.
//
// Its command line mirrors the parts of `t3` that YATT and the conformance
// runner call:
//
//	yatt-server serve [--base-dir D] [--port P] [--host H] [--no-browser] [cwd]
//	yatt-server auth session issue [--base-dir D] [--token-only] [--label L] [--ttl 365d]
//	yatt-server auth pairing create [--base-dir D] [--json] [--label L] [--ttl 5m] [--scope S ...]
//	yatt-server --version
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/server"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	switch {
	case args[0] == "--version" || args[0] == "-v":
		fmt.Printf("yatt-server v%s (T3 Code %s)\n", server.BuildVersion, server.Version)
	case args[0] == "serve":
		serve(args[1:])
	case len(args) >= 3 && args[0] == "auth" && args[1] == "session" && args[2] == "issue":
		issue(args[3:])
	case len(args) >= 3 && args[0] == "auth" && args[1] == "pairing" && args[2] == "create":
		createPairing(args[3:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: yatt-server serve [flags] [cwd] | auth session issue [flags] | --version")
	os.Exit(2)
}

// defaultBaseDir is separate from upstream's ~/.t3 on purpose: the two
// servers must never write the same database (see docs/go-port/PLAN.md).
func defaultBaseDir() string {
	if v := os.Getenv("YATT_SERVER_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".yatt", "server")
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	baseDir := fs.String("base-dir", defaultBaseDir(), "data directory")
	host := fs.String("host", "127.0.0.1", "interface to bind")
	port := fs.Int("port", 3773, "port")
	_ = fs.Bool("no-browser", false, "accepted for compatibility with `t3 serve`")
	_ = fs.Parse(args)
	cwd, _ := os.Getwd()
	if fs.NArg() > 0 {
		cwd = fs.Arg(0)
	}
	s, err := server.New(server.Config{BaseDir: *baseDir, Host: *host, Port: *port, Cwd: cwd})
	if err != nil {
		fatal(err)
	}
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal(err)
	}
	removeState, err := s.WriteRuntimeState(ln.Addr())
	if err != nil {
		fatal(err)
	}
	defer removeState()
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		_ = srv.Close()
	}()
	fmt.Printf("yatt-server listening on http://%s (data: %s)\n", ln.Addr(), *baseDir)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		removeState()
		fatal(err)
	}
}

func issue(args []string) {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	baseDir := fs.String("base-dir", defaultBaseDir(), "data directory")
	tokenOnly := fs.Bool("token-only", false, "print only the token")
	label := fs.String("label", "cli", "label for the session")
	ttl := fs.String("ttl", "30d", "lifetime, such as 12h or 365d")
	_ = fs.Parse(args)
	d, err := parseTTL(*ttl)
	if err != nil {
		fatal(err)
	}
	token, err := server.NewSessions(server.UserData(*baseDir)).Issue(*label, d)
	if err != nil {
		fatal(err)
	}
	if *tokenOnly {
		fmt.Println(token)
		return
	}
	fmt.Printf("Issued a session token for %q, valid for %s:\n%s\n", *label, *ttl, token)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func createPairing(args []string) {
	fs := flag.NewFlagSet("pairing create", flag.ExitOnError)
	baseDir := fs.String("base-dir", defaultBaseDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	label := fs.String("label", "", "label for the paired client")
	ttl := fs.String("ttl", "5m", "lifetime of the credential")
	var scopes multiFlag
	fs.Var(&scopes, "scope", "scope to grant (repeatable)")
	_ = fs.Parse(args)
	d, err := parseTTL(*ttl)
	if err != nil {
		fatal(err)
	}
	p, credential, err := server.NewSessions(server.UserData(*baseDir)).CreatePairing(*label, scopes, d)
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		out := map[string]any{"id": p.ID, "credential": credential, "scopes": p.Scopes,
			"expiresAt": p.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")}
		if p.Label != "" {
			out["label"] = p.Label
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return
	}
	fmt.Printf("Pairing credential (expires %s):\n%s\n", p.ExpiresAt.Format(time.RFC3339), credential)
}

func parseTTL(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("invalid --ttl %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
