// Copyright 2017 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var (
	// cgoEnvVars is the list of all cgo environment variable
	cgoEnvVars = []string{"CGO_CFLAGS", "CGO_CXXFLAGS", "CGO_CPPFLAGS", "CGO_LDFLAGS"}
	// cgoAbsEnvFlags are all the flags that need absolute path in cgoEnvVars
	cgoAbsEnvFlags = []string{"-I", "-L", "-isysroot", "-isystem", "-iquote", "-include", "-gcc-toolchain", "--sysroot"}
)

// env holds a small amount of Go environment and toolchain information
// which is common to multiple builders. Most Bazel-agnostic build information
// is collected in go/build.Default though.
//
// See ./README.rst for more information about handling arguments and
// environment variables.
type env struct {
	// sdk is the path to the Go SDK, which contains tools for the host
	// platform. This may be different than GOROOT.
	sdk string

	// installSuffix is the name of the directory below GOROOT/pkg that contains
	// the .a files for the standard library we should build against.
	// For example, linux_amd64_race.
	installSuffix string

	// verbose indicates whether subprocess command lines should be printed.
	verbose bool

	// workDirPath is a temporary work directory. It is created lazily.
	workDirPath string

	shouldPreserveWorkDir bool
}

// envFlags registers flags common to multiple builders and returns an env
// configured with those flags.
func envFlags(flags *flag.FlagSet) *env {
	env := &env{}
	flags.StringVar(&env.sdk, "sdk", "", "Path to the Go SDK.")
	flags.Var(&tagFlag{}, "tags", "List of build tags considered true.")
	flags.StringVar(&env.installSuffix, "installsuffix", "", "Standard library under GOROOT/pkg")
	flags.BoolVar(&env.verbose, "v", false, "Whether subprocess command lines should be printed")
	flags.BoolVar(&env.shouldPreserveWorkDir, "work", false, "if true, the temporary work directory will be preserved")
	return env
}

// checkFlags checks whether env flags were set to valid values. checkFlags
// should be called after parsing flags.
func (e *env) checkFlags() error {
	if e.sdk == "" {
		return errors.New("-sdk was not set")
	}
	return nil
}

// workDir returns a path to a temporary work directory. The same directory
// is returned on multiple calls. The caller is responsible for cleaning
// up the work directory by calling cleanup.
func (e *env) workDir() (path string, cleanup func(), err error) {
	if e.workDirPath != "" {
		return e.workDirPath, func() {}, nil
	}
	// Keep the stem "rules_go_work" in sync with reproducible_binary_test.go.
	e.workDirPath, err = ioutil.TempDir("", "rules_go_work-")
	if err != nil {
		return "", func() {}, err
	}
	if e.verbose {
		log.Printf("WORK=%s\n", e.workDirPath)
	}
	if e.shouldPreserveWorkDir {
		cleanup = func() {}
	} else {
		cleanup = func() { os.RemoveAll(e.workDirPath) }
	}
	return e.workDirPath, cleanup, nil
}

// goTool returns a slice containing the path to an executable at
// $GOROOT/pkg/$GOOS_$GOARCH/$tool and additional arguments.
func (e *env) goTool(tool string, args ...string) []string {
	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	toolPath := filepath.Join(e.sdk, "pkg", "tool", platform, tool)
	if runtime.GOOS == "windows" {
		toolPath += ".exe"
	}
	return append([]string{toolPath}, args...)
}

// goCmd returns a slice containing the path to the go executable
// and additional arguments.
func (e *env) goCmd(cmd string, args ...string) []string {
	exe := filepath.Join(e.sdk, "bin", "go")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	return append([]string{exe, cmd}, args...)
}

// runCommand executes a subprocess that inherits stdout, stderr, and the
// environment from this process.
func (e *env) runCommand(args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runAndLogCommand(cmd, e.verbose)
}

// runCommandToFile executes a subprocess and writes the output to the given
// writer.
func (e *env) runCommandToFile(w io.Writer, args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return runAndLogCommand(cmd, e.verbose)
}

func absEnv(envNameList []string, argList []string) error {
	for _, envName := range envNameList {
		splitedEnv := strings.Fields(os.Getenv(envName))
		absArgs(splitedEnv, argList)
		if err := os.Setenv(envName, strings.Join(splitedEnv, " ")); err != nil {
			return err
		}
	}
	return nil
}

func runAndLogCommand(cmd *exec.Cmd, verbose bool) error {
	if verbose {
		formatCommand(os.Stderr, cmd)
	}
	cleanup := passLongArgsInResponseFiles(cmd)
	defer cleanup()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error running subcommand: %v", err)
	}
	return nil
}

// readParamsFile looks for arguments in args of the form
// "-param=filename". When it finds these arguments it reads the file "filename"
// and replaces the argument with its content (each argument must be on a
// separate line; blank lines are ignored).
func readParamsFiles(args []string) ([]string, error) {
	var paramsIndices []int
	for i, arg := range args {
		if strings.HasPrefix(arg, "-param=") {
			paramsIndices = append(paramsIndices, i)
		}
	}
	if len(paramsIndices) == 0 {
		return args, nil
	}
	var expandedArgs []string
	last := 0
	for _, pi := range paramsIndices {
		expandedArgs = append(expandedArgs, args[last:pi]...)
		last = pi + 1

		fileName := args[pi][len("-param="):]
		content, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		fileArgs := strings.Split(string(content), "\n")
		if len(fileArgs) >= 0 && fileArgs[len(fileArgs)-1] == "" {
			// Ignore final empty line.
			fileArgs = fileArgs[:len(fileArgs)-1]
		}
		expandedArgs = append(expandedArgs, fileArgs...)
	}
	expandedArgs = append(expandedArgs, args[last:]...)
	return expandedArgs, nil
}

// splitArgs splits a list of command line arguments into two parts: arguments
// that should be interpreted by the builder (before "--"), and arguments
// that should be passed through to the underlying tool (after "--").
func splitArgs(args []string) (builderArgs []string, toolArgs []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// abs returns the absolute representation of path. Some tools/APIs require
// absolute paths to work correctly. Most notably, golang on Windows cannot
// handle relative paths to files whose absolute path is > ~250 chars, while
// it can handle absolute paths. See http://goo.gl/eqeWjm.
//
// Note that strings that begin with "__BAZEL_" are not absolutized. These are
// used on macOS for paths that the compiler wrapper (wrapped_clang) is
// supposed to know about.
func abs(path string) string {
	if strings.HasPrefix(path, "__BAZEL_") {
		return path
	}

	if abs, err := filepath.Abs(path); err != nil {
		return path
	} else {
		return abs
	}
}

// absArgs applies abs to strings that appear in args. Only paths that are
// part of options named by flags are modified.
func absArgs(args []string, flags []string) {
	absNext := false
	for i := range args {
		if absNext {
			args[i] = abs(args[i])
			absNext = false
			continue
		}
		for _, f := range flags {
			if !strings.HasPrefix(args[i], f) {
				continue
			}
			possibleValue := args[i][len(f):]
			if len(possibleValue) == 0 {
				absNext = true
				break
			}
			separator := ""
			if possibleValue[0] == '=' {
				possibleValue = possibleValue[1:]
				separator = "="
			}
			args[i] = fmt.Sprintf("%s%s%s", f, separator, abs(possibleValue))
			break
		}
	}
}

// formatCommand writes cmd to w in a format where it can be pasted into a
// shell. Spaces in environment variables and arguments are escaped as needed.
func formatCommand(w io.Writer, cmd *exec.Cmd) {
	quoteIfNeeded := func(s string) string {
		if strings.IndexByte(s, ' ') < 0 {
			return s
		}
		return strconv.Quote(s)
	}
	quoteEnvIfNeeded := func(s string) string {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return s
		}
		key, value := s[:eq], s[eq+1:]
		if strings.IndexByte(value, ' ') < 0 {
			return s
		}
		return fmt.Sprintf("%s=%s", key, strconv.Quote(value))
	}

	environ := cmd.Env
	if environ == nil {
		environ = os.Environ()
	}
	for _, e := range environ {
		fmt.Fprintf(w, "%s \\\n", quoteEnvIfNeeded(e))
	}

	sep := ""
	for _, arg := range cmd.Args {
		fmt.Fprintf(w, "%s%s", sep, quoteIfNeeded(arg))
		sep = " "
	}
	fmt.Fprint(w, "\n")
}


// passLongArgsInResponseFiles modifies cmd such that, for
// certain programs, long arguments are passed in "response files", a
// file on disk with the arguments, with one arg per line. An actual
// argument starting with '@' means that the rest of the argument is
// a filename of arguments to expand.
//
// See https://github.com/golang/go/issues/18468 (Windows) and
// https://github.com/golang/go/issues/37768 (Darwin).
func passLongArgsInResponseFiles(cmd *exec.Cmd) (cleanup func()) {
	cleanup = func() {} // no cleanup by default
	var argLen int
	for _, arg := range cmd.Args {
		argLen += len(arg)
	}
	// If we're not approaching 32KB of args, just pass args normally.
	// (use 30KB instead to be conservative; not sure how accounting is done)
	if !useResponseFile(cmd.Path, argLen) {
		return
	}
	tf, err := ioutil.TempFile("", "args")
	if err != nil {
		log.Fatalf("error writing long arguments to response file: %v", err)
	}
	cleanup = func() { os.Remove(tf.Name()) }
	var buf bytes.Buffer
	for _, arg := range cmd.Args[1:] {
		fmt.Fprintf(&buf, "%s\n", arg)
	}
	if _, err := tf.Write(buf.Bytes()); err != nil {
		tf.Close()
		cleanup()
		log.Fatalf("error writing long arguments to response file: %v", err)
	}
	if err := tf.Close(); err != nil {
		cleanup()
		log.Fatalf("error writing long arguments to response file: %v", err)
	}
	cmd.Args = []string{cmd.Args[0], "@" + tf.Name()}
	return cleanup
}

func useResponseFile(path string, argLen int) bool {
	// Unless the program uses objabi.Flagparse, which understands
	// response files, don't use response files.
	// TODO: do we need more commands? asm? cgo? For now, no.
	prog := strings.TrimSuffix(filepath.Base(path), ".exe")
	switch prog {
	case "compile", "link":
	default:
		return false
	}
	// Windows has a limit of 32 KB arguments. To be conservative and not
	// worry about whether that includes spaces or not, just use 30 KB.
	// Darwin's limit is less clear. The OS claims 256KB, but we've seen
	// failures with arglen as small as 50KB.
	if argLen > (30 << 10) {
		return true
	}
	return false
}
