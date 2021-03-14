package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"golang.org/x/mod/modfile"
)

func main() {
	r := Runner{}
	flag.StringVar(&r.TestCommand, "test", "go test ./...", "The command that evaluates if an update works")
	flag.StringVar(&r.RootDir, "c", ".", "The root directory of the module to update")
	flag.BoolVar(&r.DoCommit, "commit", false, "Commit changes")
	flag.BoolVar(&r.Verbose, "v", false, "Show output of test runs")
	flag.Parse()

	if err := r.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

// Runner holds the state for an update run
type Runner struct {
	RootDir     string
	TestCommand string
	DoCommit    bool
	Verbose     bool
	OriginalMod *modfile.File
}

func (r *Runner) Run() error {
	var err error
	r.OriginalMod, err = r.readModFile()
	if err != nil {
		return err
	}

	initialTestPassed, err := r.test()
	if err != nil {
		return err
	}
	if !initialTestPassed {
		fmt.Printf("%s\n", color.RedString("test failed before upgrading anything, aborting."))
		return nil
	}

	if err := r.updateAll(); err != nil {
		_ = r.writeModFile(r.OriginalMod)
		return err
	}

	allUpdatesMod, err := r.readModFile()
	if err != nil {
		_ = r.writeModFile(r.OriginalMod)
		return err
	}

	// build the original list of updates from the changed made to the mod file
	updates := []*modfile.Require{}
	for _, req := range allUpdatesMod.Require {
		if req.Indirect {
			continue
		}
		if requiredVersion(r.OriginalMod, req.Mod.Path) == req.Mod.Version {
			continue
		}
		updates = append(updates, req)
	}

	if len(updates) == 0 {
		fmt.Printf("%s\n", color.GreenString("all packages are up to date"))
		return nil
	}

	goodUpdates, err := r.try(updates, "")
	if err != nil {
		_ = r.writeModFile(r.OriginalMod)
		return err
	}

	// rewrite the mod file with the updated packages
	mod := copyMod(r.OriginalMod)
	setVersions(mod, goodUpdates)
	if err := r.writeModFile(mod); err != nil {
		_ = r.writeModFile(r.OriginalMod)
		return err
	}

	finalTestPassed, err := r.test()
	if err != nil {
		return err
	}
	if !finalTestPassed {
		fmt.Printf("%s\n", color.RedString("test failed after applying upgrades, aborting."))
		return nil
	}

	for _, req := range updates {
		if requiredVersion(&modfile.File{Require: goodUpdates}, req.Mod.Path) != "" {
			fmt.Printf("%s: %s %s\n", color.GreenString("package upgraded"), req.Mod.Path, req.Mod.Version)
		}
	}
	for _, req := range updates {
		if requiredVersion(&modfile.File{Require: goodUpdates}, req.Mod.Path) == "" {
			fmt.Printf("%s: %s %s\n", color.RedString("package upgrade failed"), req.Mod.Path, req.Mod.Version)
		}
	}

	{
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = r.RootDir
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy failed: %v", err)
		}
	}

	if r.DoCommit {
		goodUpdateCount := 0
		message := []string{"Update go.mod", ""}
		for _, req := range updates {
			if requiredVersion(&modfile.File{Require: goodUpdates}, req.Mod.Path) != "" {
				message = append(message, fmt.Sprintf("* upgrade %s from %s to %s",
					req.Mod.Path, requiredVersion(r.OriginalMod, req.Mod.Path), req.Mod.Version))
				goodUpdateCount++
			} else {
				message = append(message, fmt.Sprintf("* FAILED upgrade %s from %s to %s",
					req.Mod.Path, requiredVersion(r.OriginalMod, req.Mod.Path), req.Mod.Version))
			}
		}

		if goodUpdateCount > 0 {
			cmd := exec.Command("git", "-C", r.RootDir, "add", "-A")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Dir = r.RootDir
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("git add failed: %v", err)
			}
			cmd = exec.Command("git", "commit", "-m", strings.Join(message, "\n"))
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Dir = r.RootDir
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("git commit failed: %v", err)
			}
		}
	}

	return nil
}

func (r Runner) updateAll() error {
	fmt.Printf("running go get -u ./...\n")
	cmd := exec.Command("go", "get", "-u", "./...")
	outputReader, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	cmd.Dir = r.RootDir
	if err := cmd.Run(); err != nil {
		_, _ = io.Copy(os.Stdout, outputReader)
		return fmt.Errorf("go get -u ./...: %s", err)
	}
	return nil
}

// try tries to apply `updates` by performing the update and running the test. If the
// tests fail, it invokes itself recursively with a smaller set of updates. Returns a list of
// the updates that passed the test.
func (r Runner) try(updates []*modfile.Require, indent string) ([]*modfile.Require, error) {
	fmt.Printf("%strying %d updates\n", indent, len(updates))
	for _, req := range updates {
		fmt.Printf("%s  %s: %s -> %s\n", indent, req.Mod.Path, requiredVersion(r.OriginalMod, req.Mod.Path), req.Mod.Version)
	}

	if len(updates) == 0 {
		return nil, nil
	}

	mod := copyMod(r.OriginalMod)
	setVersions(mod, updates)
	err := r.writeModFile(mod)
	if err != nil {
		return nil, err
	}

	ok, err := r.test()
	if err != nil {
		return nil, err
	}
	if ok {
		fmt.Printf("%s  test passed\n", indent)
		return updates, nil
	}

	fmt.Printf("%s  test failed\n", indent)

	// if we are testing only one package, and it fails, then this package
	// is bad, and we shouldn't include it in the update
	if len(updates) == 1 {
		return []*modfile.Require{}, nil
	}

	// more than one package was being updated, so we split the updates in half
	// and try them separately, to see if we can figure out which ones are actually
	// broken
	requireA, requireB := bisect(updates)

	successA, err := r.try(requireA, indent+"  ")
	if err != nil {
		return nil, err
	}
	successB, err := r.try(requireB, indent+"  ")
	if err != nil {
		return nil, err
	}

	goodUpdates := append(successA, successB...)
	fmt.Printf("%skeeping %d of %d updates:\n", indent, len(goodUpdates), len(updates))
	for _, req := range goodUpdates {
		fmt.Printf("%s  %s: %s -> %s\n", indent, req.Mod.Path,
			requiredVersion(r.OriginalMod, req.Mod.Path), req.Mod.Version)
	}

	return goodUpdates, nil
}

// test runs the tests to determine if an upgrade was successful
func (r Runner) test() (bool, error) {
	cmd := exec.Command("/bin/sh", "-c", r.TestCommand)
	cmd.Dir = r.RootDir
	if r.Verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot run test program: %s", err)
	}
	return true, nil
}

// readModFile reads and parses go.mod
func (r Runner) readModFile() (*modfile.File, error) {
	buf, err := ioutil.ReadFile(filepath.Join(r.RootDir, "go.mod"))
	if err != nil {
		return nil, err
	}

	return modfile.Parse(filepath.Join(r.RootDir, "go.mod"), buf, nil)
}

// writeModFile writes `mf` to go.mod.
func (r Runner) writeModFile(mf *modfile.File) error {
	buf, err := mf.Format()
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(r.RootDir, "go.mod"), buf, 0644)
}

// bisect returns two require lists, each containing approximately half of the
// items in `updates`
func bisect(updates []*modfile.Require) ([]*modfile.Require, []*modfile.Require) {
	a, b := []*modfile.Require{}, []*modfile.Require{}
	for i := range updates {
		if i%2 == 0 {
			a = append(a, updates[i])
		} else {
			b = append(b, updates[i])
		}
	}
	return a, b
}

// setVersions updates the requirements in `mf` with the updates described
// by `updates`.
func setVersions(mf *modfile.File, updates []*modfile.Require) {
	for _, req := range updates {
		_ = mf.AddRequire(req.Mod.Path, req.Mod.Version) // AddRequire cannot fail
	}
}

// requiredVersion returns the version of the package `path` that is required by `file` or an
// empty string if none is required.
func requiredVersion(file *modfile.File, path string) string {
	for _, req := range file.Require {
		if req.Mod.Path == path {
			return req.Mod.Version
		}
	}
	return ""
}

// copyMod returns a copy of `mf` by serializing and re-parsing it.
func copyMod(mf *modfile.File) *modfile.File {
	buf, err := mf.Format()
	if err != nil {
		panic(err)
	}
	copy, err := modfile.Parse("go.mod", buf, nil)
	if err != nil {
		panic(err)
	}
	return copy
}
