package server

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"

	"github.com/tycheg/reasonr-engine/internal/check"
)

// checkCommand is reasonrd check (RFC §30–31): it checks engine.yaml
// against the module's models (the base one and with extensions).
func checkCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reasonrd check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "engine.yaml", "the engine configuration")
	format := fs.String("format", "text", "output format: text or json")
	test := fs.Bool("test", false, "the server's test mode (reasonrd --test): integration fakes, KMS memory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	mod, err := reasonr.Load(dir)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	models := map[string]*reasonr.Model{}
	for _, name := range cfg.Tenants {
		if models[name] != nil {
			continue
		}
		ext := name
		if name == "base" {
			ext = ""
		}
		// --test: with fakes, as in reasonrd --test.
		m, err := mod.Build(reasonr.Options{Extension: ext, Test: *test})
		var errs *reasonr.Errors
		if errors.As(err, &errs) {
			stderr.Write(errs.Text)
			fmt.Fprintf(stderr, "reasonrd: model %s has errors\n", name)
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd: model %s: %v\n", name, err)
			return 2
		}
		models[name] = m
	}
	problems := check.Check(cfg, models, check.Options{Test: *test})
	switch *format {
	case "json":
		type pos struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		}
		type item struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			File     string `json:"file"`
			Range    *struct {
				Start pos `json:"start"`
				End   pos `json:"end"`
			} `json:"range,omitempty"`
			Message string `json:"message"`
		}
		out := struct {
			Version     int    `json:"version"`
			Diagnostics []item `json:"diagnostics"`
		}{Version: 1, Diagnostics: []item{}}
		for _, p := range problems {
			it := item{Code: p.Code, Severity: p.Severity, File: *cfgPath, Message: p.Message}
			if p.Line > 0 {
				it.Range = &struct {
					Start pos `json:"start"`
					End   pos `json:"end"`
				}{pos{p.Line, 1}, pos{p.Line + 1, 1}}
			}
			out.Diagnostics = append(out.Diagnostics, it)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
	case "text":
		for _, p := range problems {
			fmt.Fprintln(stdout, p.Text(*cfgPath))
		}
	default:
		fmt.Fprintf(stderr, "reasonrd: unknown format %q\n", *format)
		return 2
	}
	if check.HasErrors(problems) {
		return 1
	}
	return 0
}

// openapiCommand is reasonrd openapi --integrations [--extension name]
// [path]: the integration service contract of the model's production
// program (RFC §18) — without fakes.
func openapiCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reasonrd openapi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	integrations := fs.Bool("integrations", false, "the integration service contract")
	ext := fs.String("extension", "", "a model with an extension: the base model and extensions/<name>/")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*integrations {
		fmt.Fprintln(stderr, "reasonrd openapi: set --integrations (the model API is GET /openapi.json of the server)")
		return 2
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	m, err := reasonr.Build(dir, reasonr.Options{Extension: *ext})
	var errs *reasonr.Errors
	if errors.As(err, &errs) {
		stderr.Write(errs.Text)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	data, err := json.MarshalIndent(httpapi.IntegrationsOpenAPI(m.Schema()), "", "  ")
	if err == nil {
		_, err = stdout.Write(append(data, '\n'))
	}
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	return 0
}
