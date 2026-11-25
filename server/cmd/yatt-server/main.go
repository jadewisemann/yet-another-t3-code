// Command yatt-server is the Go port of the T3 Code server.
//
// Its command line mirrors the parts of `t3` that YATT and the conformance
// runner call:
//
//	yatt-server serve [--base-dir D] [--port P] [--host H] [--no-browser] [cwd]
//	yatt-server auth session issue [--base-dir D] [--token-only] [--label L] [--ttl 365d]
//	yatt-server --version
package main

import (
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
		fmt.Println("yatt-server v" + server.Version)
	case args[0] == "serve":
		serve(args[1:])
	case len(args) >= 3 && args[0] == "auth" && args[1] == "session" && args[2] == "issue":
		issue(args[3:])
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
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		_ = srv.Close()
	}()
	fmt.Printf("yatt-server listening on http://%s (data: %s)\n", addr, *baseDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
