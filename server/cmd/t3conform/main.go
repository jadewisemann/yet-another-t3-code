// Command t3conform runs the conformance scenarios in conformance/scenarios
// against a T3 Code server binary.
//
//	# record golden transcripts from upstream
//	t3conform -server $(which t3) -record
//	# check our server against them
//	t3conform -server ./yatt-server
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/conform"
	"github.com/jadewisemann/yet-another-t3-code/server/internal/spec"
)

func main() {
	root := repoRoot()
	server := flag.String("server", "", "server binary that speaks the t3 CLI (required)")
	scenarios := flag.String("scenarios", filepath.Join(root, "conformance/scenarios"), "scenario directory")
	golden := flag.String("golden", filepath.Join(root, "conformance/golden"), "golden transcript directory")
	specPath := flag.String("spec", filepath.Join(root, "spec/generated/rpc.json"), "rpc.json")
	record := flag.Bool("record", false, "write golden transcripts instead of comparing")
	only := flag.String("only", "", "comma-separated scenario names")
	tier := flag.String("tier", "", "only scenarios of this tier")
	keep := flag.Bool("keep", false, "keep each run's data directory")
	report := flag.String("report", "", "write a JSON report here")
	expectFail := flag.String("expect-fail", "", "file listing scenarios that are known to fail; "+
		"the run fails if any other scenario fails or a listed one passes")
	flag.Parse()
	if *server == "" {
		fmt.Fprintln(os.Stderr, "-server is required")
		os.Exit(2)
	}
	bin, err := exec.LookPath(*server)
	if err != nil {
		fatal(err)
	}
	bin, _ = filepath.Abs(bin)
	sp, err := spec.Load(*specPath)
	if err != nil {
		fatal(err)
	}
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}
	list, err := conform.LoadScenarios(*scenarios, names)
	if err != nil {
		fatal(err)
	}
	version := serverVersion(bin)
	type outcome struct {
		Scenario       string   `json:"scenario"`
		Tier           string   `json:"tier"`
		Status         string   `json:"status"` // pass, fail, error, recorded
		Detail         string   `json:"detail,omitempty"`
		SpecViolations []string `json:"specViolations,omitempty"`
		Seconds        float64  `json:"seconds"`
	}
	var outcomes []outcome
	failed := 0
	for _, sc := range list {
		if *tier != "" && sc.Tier != *tier {
			continue
		}
		start := time.Now()
		o := outcome{Scenario: sc.Name, Tier: sc.Tier}
		dir, _ := os.MkdirTemp("", "t3conform-"+sc.Name+"-")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		t, err := conform.Start(ctx, bin, dir)
		if err != nil {
			o.Status, o.Detail = "error", err.Error()
		} else {
			res, err := conform.Run(ctx, sc, t, sp)
			t.Stop()
			if res != nil {
				o.SpecViolations = res.SpecViolations
			}
			switch {
			case err != nil:
				o.Status, o.Detail = "error", err.Error()+"\n--- server log (tail) ---\n"+tail(t.Log.String(), 40)
			case *record:
				g := conform.Golden{Scenario: sc.Name, RecordedOn: version, Transcript: res.Transcript}
				if err := os.WriteFile(filepath.Join(*golden, sc.Name+".json"), conform.Marshal(g), 0o644); err != nil {
					fatal(err)
				}
				o.Status = "recorded"
			default:
				o.Status = "pass"
				data, err := os.ReadFile(filepath.Join(*golden, sc.Name+".json"))
				if err != nil {
					o.Status, o.Detail = "error", "no golden transcript; record one from upstream first"
					break
				}
				var g conform.Golden
				if err := json.Unmarshal(data, &g); err != nil {
					fatal(err)
				}
				// Compare through JSON so both sides have the same types.
				var got []conform.Entry
				_ = json.Unmarshal(conform.Marshal(res.Transcript), &got)
				if d := conform.Diff(g.Transcript, got); d != "" {
					o.Status, o.Detail = "fail", d
				}
			}
		}
		cancel()
		if !*keep {
			os.RemoveAll(dir)
		}
		if len(o.SpecViolations) > 0 && o.Status == "pass" {
			o.Status = "fail"
		}
		if o.Status == "fail" || o.Status == "error" {
			failed++
		}
		o.Seconds = time.Since(start).Seconds()
		fmt.Printf("%-8s %-40s %5.1fs\n", strings.ToUpper(o.Status), sc.Name, o.Seconds)
		if o.Detail != "" {
			fmt.Println(indent(o.Detail))
		}
		for _, v := range o.SpecViolations {
			fmt.Println(indent("spec: " + v))
		}
		outcomes = append(outcomes, o)
	}
	if *expectFail != "" {
		known := map[string]bool{}
		data, err := os.ReadFile(*expectFail)
		if err != nil {
			fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				known[strings.Fields(line)[0]] = true
			}
		}
		failed = 0
		for _, o := range outcomes {
			bad := o.Status == "fail" || o.Status == "error"
			switch {
			case bad && !known[o.Scenario]:
				fmt.Printf("REGRESSION %s\n", o.Scenario)
				failed++
			case !bad && known[o.Scenario]:
				fmt.Printf("NOW PASSES %s: remove it from %s\n", o.Scenario, *expectFail)
				failed++
			}
		}
	}
	if *report != "" {
		_ = os.WriteFile(*report, conform.Marshal(map[string]any{
			"server": bin, "serverVersion": version, "spec": sp.Upstream, "results": outcomes,
		}), 0o644)
	}
	fmt.Printf("\n%d scenarios, %d failed\n", len(outcomes), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func serverVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// repoRoot finds the repository from the working directory.
func repoRoot() string {
	dir, _ := os.Getwd()
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "spec/generated/rpc.json")); err == nil {
			return d
		}
	}
	return dir
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
