package testrun

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// JSONVersion is the version of the reasonrd test --format json report.
const JSONVersion = 1

var (
	msgErrors    = "errors: %s"
	msgFault     = "runtime failure: %s"
	msgMissError = "expected error %s, got: %s"
	msgUnexpErr  = "unexpected error %s"
	msgMissNote  = "no notification %s; sent: %s"
	msgMissReq   = "no external request %s; sent: %s"
	msgMismatch  = "%s: field %s = %s, expected %s"
	msgNoResult  = "%s: result is %s, expected %s"
	msgFake      = "a fake called %s — errors: %s"
	msgUI        = "the action is not available in the UI: %s"
	msgRun       = "run %d (%s): "
	msgUncovered = "UI places not covered: %d"
	msgNone      = "none"
	msgOK        = "tests: %d"
	msgFailed    = "failed: %d of %d"
)

// Message returns the failure text (without the section).
func Message(f *reasonr.Failure) string {
	list := func(xs []string) string {
		if len(xs) == 0 {
			return msgNone
		}
		return strings.Join(xs, ", ")
	}
	first := func(xs []string) string {
		if len(xs) == 0 {
			return ""
		}
		return xs[0]
	}
	var s string
	switch f.Kind {
	case reasonr.FailStep:
		s = f.Step + " — " + fmt.Sprintf(msgErrors, list(f.Actual))
	case reasonr.FailFault:
		s = fmt.Sprintf(msgFault, f.Message)
		if f.Step != "" {
			s = f.Step + " — " + s
		}
	case reasonr.FailMissingError:
		s = fmt.Sprintf(msgMissError, f.Expected, list(f.Actual))
	case reasonr.FailUnexpectedError:
		s = fmt.Sprintf(msgUnexpErr, first(f.Actual))
	case reasonr.FailMissingNotification:
		s = fmt.Sprintf(msgMissNote, f.Expected, list(f.Actual))
	case reasonr.FailMissingRequest:
		s = fmt.Sprintf(msgMissReq, f.Expected, list(f.Actual))
	case reasonr.FailMismatch:
		s = fmt.Sprintf(msgMismatch, f.Step, f.Field, first(f.Actual), f.Expected)
	case reasonr.FailNoResult:
		s = fmt.Sprintf(msgNoResult, f.Step, first(f.Actual), f.Expected)
	case reasonr.FailFake:
		s = fmt.Sprintf(msgFake, f.Step, list(f.Actual))
	case reasonr.FailUI:
		s = fmt.Sprintf(msgUI, f.Message)
		if f.Step != "" {
			s = f.Step + " — " + s
		}
	}
	if f.Run > 0 {
		s = fmt.Sprintf(msgRun, f.Run, cmp.Or(f.Driver, "memory")) + s
	}
	return s
}

// WriteText prints a report like go test: failures with the place and
// messages, with verbose every test and its steps; at the end the model's
// summary line.
func WriteText(w io.Writer, rep *Report, verbose bool) error {
	var b strings.Builder
	for i := range rep.Tests {
		t := &rep.Tests[i]
		if verbose {
			fmt.Fprintf(&b, "=== RUN   %s\n", t.Name)
			for _, st := range t.Steps {
				fmt.Fprintf(&b, "    %-5s %s\n", st.Section, st.Text)
				for _, e := range st.Errors {
					fmt.Fprintf(&b, "          error %s\n", e)
				}
				for _, n := range st.Notifications {
					fmt.Fprintf(&b, "          notification %s\n", n)
				}
				for _, r := range st.Requests {
					fmt.Fprintf(&b, "          request %s\n", r)
				}
			}
		}
		switch {
		case !t.Passed():
			fmt.Fprintf(&b, "--- FAIL: %s\n", t.Name)
			for j := range t.Failures {
				f := &t.Failures[j]
				msg := strings.ReplaceAll(Message(f), "\n", "\n    ")
				fmt.Fprintf(&b, "    %s:%d: %s: %s\n", f.File, f.Line, f.Section, msg)
			}
		case verbose:
			fmt.Fprintf(&b, "--- PASS: %s\n", t.Name)
		}
	}
	if len(rep.Uncovered) > 0 {
		fmt.Fprintf(&b, msgUncovered+"\n", len(rep.Uncovered))
		for _, p := range rep.Uncovered {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	name := rep.Module
	if rep.Extension != "" {
		name += " [" + rep.Extension + "]"
	}
	if n := rep.Failed(); n > 0 {
		fmt.Fprintf(&b, "FAIL\t%s\t"+msgFailed+"\n", name, n, len(rep.Tests))
	} else {
		fmt.Fprintf(&b, "ok\t%s\t"+msgOK+"\n", name, len(rep.Tests))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

type jsonOutput struct {
	Version int         `json:"version"`
	Models  []jsonModel `json:"models"`
}

type jsonModel struct {
	Module    string     `json:"module"`
	Extension string     `json:"extension,omitempty"`
	Passed    int        `json:"passed"`
	Failed    int        `json:"failed"`
	Tests     []jsonTest `json:"tests"`
	// Uncovered are the places without steps (--coverage).
	Uncovered []string `json:"uncovered,omitempty"`
}

type jsonTest struct {
	Name     string        `json:"name"`
	File     string        `json:"file"`
	Line     int           `json:"line"`
	Passed   bool          `json:"passed"`
	Duration float64       `json:"durationMs"` // all runs of the test, ms
	Failures []jsonFailure `json:"failures,omitempty"`
	Steps    []jsonStep    `json:"steps,omitempty"`
}

type jsonFailure struct {
	Kind     string   `json:"kind"`
	Section  string   `json:"section"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Step     string   `json:"step,omitempty"`
	Field    string   `json:"field,omitempty"`
	Expected string   `json:"expected,omitempty"`
	Actual   []string `json:"actual,omitempty"`
	Message  string   `json:"message"`
	Run      int      `json:"run,omitempty"`
	Driver   string   `json:"driver,omitempty"`
}

type jsonStep struct {
	Section       string   `json:"section"`
	Text          string   `json:"text"`
	Errors        []string `json:"errors,omitempty"`
	Notifications []string `json:"notifications,omitempty"`
	Requests      []string `json:"requests,omitempty"`
}

// WriteJSON prints the reports of the models as one object (the format is
// in AGENTS.md); with verbose, the test steps.
func WriteJSON(w io.Writer, reps []*Report, verbose bool) error {
	out := jsonOutput{Version: JSONVersion, Models: []jsonModel{}}
	for _, rep := range reps {
		jm := jsonModel{Module: rep.Module, Extension: rep.Extension, Failed: rep.Failed(), Tests: []jsonTest{}, Uncovered: rep.Uncovered}
		jm.Passed = len(rep.Tests) - jm.Failed
		for i := range rep.Tests {
			t := &rep.Tests[i]
			jt := jsonTest{Name: t.Name, File: t.File, Line: t.Line, Passed: t.Passed(),
				Duration: float64(t.Duration.Microseconds()) / 1000}
			for j := range t.Failures {
				f := &t.Failures[j]
				jt.Failures = append(jt.Failures, jsonFailure{
					Kind: f.Kind.String(), Section: f.Section.String(), File: f.File, Line: f.Line, Step: f.Step, Field: f.Field,
					Expected: f.Expected, Actual: f.Actual, Message: Message(f), Run: f.Run, Driver: f.Driver,
				})
			}
			if verbose {
				for _, st := range t.Steps {
					jt.Steps = append(jt.Steps, jsonStep{
						Section: st.Section.String(), Text: st.Text, Errors: st.Errors,
						Notifications: st.Notifications, Requests: st.Requests,
					})
				}
			}
			jm.Tests = append(jm.Tests, jt)
		}
		out.Models = append(out.Models, jm)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
