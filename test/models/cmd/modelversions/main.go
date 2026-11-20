package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/openshift/lightspeed-agentic-operator/test/models"
)

const catalogPath = "test/models/model-versions.yaml"

func main() {
	if err := run(os.Args[1:], "test/models", os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, dir string, out, diagnostics io.Writer) error {
	usage := errors.New("usage: modelversions validate | primary <route> | matrix | swap --slot S --old M --new N --summary FILE [--date YYYY-MM-DD] | check-history [--base REF]")
	if len(args) == 0 {
		return usage
	}
	versions := filepath.Join(dir, "model-versions.yaml")
	history := filepath.Join(dir, "model-history.yaml")
	today := time.Now().UTC().Format(time.DateOnly)
	warn := func(msg string) { fmt.Fprintln(diagnostics, "WARNING:", msg) }
	switch args[0] {
	case "validate", "primary", "matrix":
		n := 1
		if args[0] == "primary" {
			n++
		}
		if len(args) != n {
			return usage
		}
		catalog, err := models.Load(versions)
		if err != nil {
			return err
		}
		switch args[0] {
		case "primary":
			id, err := catalog.Primary(args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, id)
			return err
		case "matrix":
			return json.NewEncoder(out).Encode(catalog.SmokeMatrix())
		default:
			_, err := loadHistory(history)
			return err
		}
	case "check-history":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		base := flags.String("base", "origin/main", "base git ref")
		if err := flags.Parse(args[1:]); err != nil {
			warn(err.Error())
			return nil
		}
		if flags.NArg() != 0 {
			warn(usage.Error())
			return nil
		}
		records, err := loadHistory(history)
		if err != nil {
			warn(err.Error())
		}
		head, err := models.Load(versions)
		if err != nil {
			warn(err.Error())
		}
		var prior *models.Catalog
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, err := readBaseCatalog(ctx, *base)
		cancel()
		if err != nil {
			warn(fmt.Sprintf("cannot read %s at %s: %v", catalogPath, *base, err))
		} else {
			prior, err = models.Parse(data)
			if err != nil {
				warn(fmt.Sprintf("cannot parse base catalog: %v", err))
			}
		}
		for _, msg := range models.CheckHistory(prior, head, records, today) {
			warn(msg)
		}
		return nil
	case "swap":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(diagnostics)
		slot := flags.String("slot", "", "catalog slot")
		old := flags.String("old", "", "previous model ID")
		next := flags.String("new", "", "replacement model ID")
		summaryFile := flags.String("summary", "", "smoke summary JSON file")
		date := flags.String("date", today, "swap date (UTC)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *slot == "" || *old == "" || *next == "" || *summaryFile == "" {
			return usage
		}
		data, err := os.ReadFile(*summaryFile)
		if err != nil {
			return err
		}
		var summary models.Summary
		if err := json.Unmarshal(data, &summary); err != nil {
			return fmt.Errorf("parse smoke summary: %w", err)
		}
		before, err := os.ReadFile(versions)
		if err != nil {
			return err
		}
		records, err := os.ReadFile(history)
		if err != nil {
			return err
		}
		after, updated, warnings, err := models.Swap(before, records, *slot, *old, *next, *date, summary)
		if err != nil {
			return err
		}
		if err := replace(history, updated); err != nil {
			return err
		}
		if err := replace(versions, after); err != nil {
			return errors.Join(err, replace(history, records))
		}
		for _, msg := range warnings {
			warn(msg)
		}
		return nil
	default:
		return usage
	}
}

func readBaseCatalog(ctx context.Context, ref string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "show", "--end-of-options", ref+":"+catalogPath)
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

func loadHistory(path string) ([]models.Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	records, err := models.ParseHistory(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return records, nil
}

func replace(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".modelversions-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	_, err = tmp.Write(data)
	if err := errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
