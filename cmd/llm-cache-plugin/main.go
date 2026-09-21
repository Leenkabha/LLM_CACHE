// Command llm-cache-plugin is the developer tool for LLM Cache plugins.
//
//	llm-cache-plugin verify [DIR] [--endpoint URL] [--token T] [--config k=v ...]
//	llm-cache-plugin manifest FILE
//
// verify validates plugin.yaml, then runs the contract suite for the plugin's
// type against either a running endpoint (--endpoint) or the plugin built and
// started in an isolated local container (needs Docker). The same suites gate
// activation in the platform, so a plugin that passes here passes there.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/controller"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/safehttp"
)

const usage = `usage:
  llm-cache-plugin verify [DIR] [flags]   validate plugin.yaml and run the contract tests
  llm-cache-plugin manifest FILE          validate a manifest only

verify flags:
  --endpoint URL     test a plugin that is already running (no Docker needed)
  --type TYPE        plugin type when there is no plugin.yaml (endpoint mode)
  --token TOKEN      bearer token to send to --endpoint
  --model NAME       model name to send (llm)
  --config k=v       configuration passed to the plugin (repeatable), e.g. --config dim=384
  --runner-dir DIR   LLM_CACHE checkout (Python plugins; default: search upwards)
  --keep             leave the local container and image for debugging
  --json             print the report as JSON
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "verify":
		return verify(args[1:])
	case "manifest":
		return manifestCmd(args[1:])
	case "version", "--version":
		fmt.Println("llm-cache-plugin (contract v1, manifest " + manifest.APIVersion + ")")
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprint(os.Stderr, usage)
	return 2
}

func manifestCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	m, err := manifest.Parse(data)
	if err != nil {
		printIssues(err)
		return 1
	}
	fmt.Printf("ok: %s %s (%s)\n", m.Metadata.Name, m.Metadata.Version, m.Spec.Type)
	return 0
}

func printIssues(err error) {
	var ve *manifest.ValidationError
	if errors.As(err, &ve) {
		fmt.Fprintln(os.Stderr, "plugin.yaml is not valid:")
		for _, is := range ve.Issues {
			if is.Path != "" {
				fmt.Fprintf(os.Stderr, "  - %s: %s\n", is.Path, is.Message)
			} else {
				fmt.Fprintf(os.Stderr, "  - %s\n", is.Message)
			}
		}
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func verify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "")
	typeName := fs.String("type", "", "")
	token := fs.String("token", os.Getenv("PLUGIN_ENDPOINT_TOKEN"), "")
	model := fs.String("model", "verify", "")
	runner := fs.String("runner-dir", "", "")
	keep := fs.Bool("keep", false, "")
	asJSON := fs.Bool("json", false, "")
	var cfgs multi
	fs.Var(&cfgs, "config", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	// The directory may come before or after the flags.
	dir, flagArgs := ".", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, flagArgs = args[0], args[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	env := map[string]string{}
	for _, kv := range cfgs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			fmt.Fprintf(os.Stderr, "--config expects key=value, got %q\n", kv)
			return 2
		}
		env["CONFIG_"+strings.ToUpper(k)] = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	progress := func(s string) {
		if !*asJSON {
			fmt.Fprintln(os.Stderr, s)
		}
	}

	// 1. The manifest.
	var mf *manifest.Manifest
	if data, err := os.ReadFile(dir + "/plugin.yaml"); err == nil {
		if mf, err = manifest.Parse(data); err != nil {
			printIssues(err)
			return 1
		}
		progress(fmt.Sprintf("plugin.yaml ok: %s %s (%s)", mf.Metadata.Name, mf.Metadata.Version, mf.Spec.Type))
	} else if *endpoint == "" || *typeName == "" {
		fmt.Fprintf(os.Stderr, "no plugin.yaml in %s (with --endpoint you can pass --type instead)\n", dir)
		return 1
	}

	var rep *contract.Report
	var err error
	if *endpoint != "" {
		typ, terr := plugins.ParseType(firstNonEmpty(*typeName, typeOf(mf)))
		if terr != nil {
			fmt.Fprintln(os.Stderr, terr)
			return 2
		}
		u, uerr := safehttp.ValidateURL(*endpoint, safehttp.Policy{AllowInsecure: true}) // a developer tool: local endpoints are the point
		if uerr != nil {
			fmt.Fprintln(os.Stderr, "endpoint:", uerr)
			return 2
		}
		client, cerr := protocol.NewClient(u.String(), safehttp.NewClient(safehttp.Policy{AllowInsecure: true}, 60*time.Second), *token)
		if cerr != nil {
			fmt.Fprintln(os.Stderr, cerr)
			return 2
		}
		t := contract.Target{Type: typ, Client: client, Model: *model, RequireEmpty: true}
		if mf != nil {
			t.VectorDim = mf.Spec.Verify.VectorDim
			if mf.Spec.Python != nil {
				t.ExpectedBackend = mf.Spec.Python.Backend
			}
		}
		progress("running the " + string(typ) + " contract suite against " + u.Host + " ...")
		r := contract.Run(ctx, t)
		rep = &r
	} else {
		_, rep, err = controller.LocalVerify(ctx, controller.LocalOptions{Dir: dir, RunnerDir: firstNonEmpty(*runner, ""), Env: env, Keep: *keep}, progress)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify failed:", err)
			return 1
		}
	}

	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, r := range rep.Results {
			switch r.Status {
			case "pass":
				fmt.Printf("  PASS  %s\n", r.Name)
			case "skip":
				fmt.Printf("  skip  %s  (%s)\n", r.Name, r.Detail)
			default:
				fmt.Printf("  FAIL  %s\n        %s\n", r.Name, r.Detail)
			}
		}
		fmt.Println(rep.Summary())
	}
	if !rep.Passed() {
		return 1
	}
	return 0
}

func typeOf(m *manifest.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Spec.Type
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
