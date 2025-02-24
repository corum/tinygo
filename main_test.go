package main

// This file tests the compiler by running Go files in testdata/*.go and
// comparing their output with the expected output in testdata/*.txt.

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinygo-org/tinygo/builder"
	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/goenv"
)

const TESTDATA = "testdata"

var testTarget = flag.String("target", "", "override test target")

var supportedLinuxArches = map[string]string{
	"AMD64Linux": "linux/amd64",
	"X86Linux":   "linux/386",
	"ARMLinux":   "linux/arm/6",
	"ARM64Linux": "linux/arm64",
}

var sema = make(chan struct{}, runtime.NumCPU())

func TestBuild(t *testing.T) {
	t.Parallel()

	tests := []string{
		"alias.go",
		"atomic.go",
		"binop.go",
		"calls.go",
		"cgo/",
		"channel.go",
		"embed/",
		"float.go",
		"gc.go",
		"generics.go",
		"goroutines.go",
		"init.go",
		"init_multi.go",
		"interface.go",
		"json.go",
		"map.go",
		"math.go",
		"print.go",
		"reflect.go",
		"slice.go",
		"sort.go",
		"stdlib.go",
		"string.go",
		"structs.go",
		"testing.go",
		"timers.go",
		"zeroalloc.go",
	}

	if *testTarget != "" {
		// This makes it possible to run one specific test (instead of all),
		// which is especially useful to quickly check whether some changes
		// affect a particular target architecture.
		runPlatTests(optionsFromTarget(*testTarget, sema), tests, t)
		return
	}

	t.Run("Host", func(t *testing.T) {
		t.Parallel()
		runPlatTests(optionsFromTarget("", sema), tests, t)
	})

	// Test a few build options.
	t.Run("build-options", func(t *testing.T) {
		t.Parallel()

		// Test with few optimizations enabled (no inlining, etc).
		t.Run("opt=1", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.Opt = "1"
			runTestWithConfig("stdlib.go", t, opts, nil, nil)
		})

		// Test with only the bare minimum of optimizations enabled.
		// TODO: fix this for stdlib.go, which currently fails.
		t.Run("opt=0", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.Opt = "0"
			runTestWithConfig("print.go", t, opts, nil, nil)
		})

		t.Run("ldflags", func(t *testing.T) {
			t.Parallel()
			opts := optionsFromTarget("", sema)
			opts.GlobalValues = map[string]map[string]string{
				"main": {
					"someGlobal": "foobar",
				},
			}
			runTestWithConfig("ldflags.go", t, opts, nil, nil)
		})
	})

	if testing.Short() {
		// Don't test other targets when the -short flag is used. Only test the
		// host system.
		return
	}

	t.Run("EmulatedCortexM3", func(t *testing.T) {
		t.Parallel()
		runPlatTests(optionsFromTarget("cortex-m-qemu", sema), tests, t)
	})

	t.Run("EmulatedRISCV", func(t *testing.T) {
		t.Parallel()
		runPlatTests(optionsFromTarget("riscv-qemu", sema), tests, t)
	})

	t.Run("AVR", func(t *testing.T) {
		t.Parallel()
		runPlatTests(optionsFromTarget("simavr", sema), tests, t)
	})

	if runtime.GOOS == "linux" {
		for name, osArch := range supportedLinuxArches {
			options := optionsFromOSARCH(osArch, sema)
			if options.GOARCH != runtime.GOARCH { // Native architecture already run above.
				t.Run(name, func(t *testing.T) {
					runPlatTests(options, tests, t)
				})
			}
		}
		t.Run("WebAssembly", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("wasm", sema), tests, t)
		})
		t.Run("WASI", func(t *testing.T) {
			t.Parallel()
			runPlatTests(optionsFromTarget("wasi", sema), tests, t)
		})
	}
}

func runPlatTests(options compileopts.Options, tests []string, t *testing.T) {
	emuCheck(t, options)

	spec, err := compileopts.LoadTarget(&options)
	if err != nil {
		t.Fatal("failed to load target spec:", err)
	}

	for _, name := range tests {
		if options.GOOS == "linux" && (options.GOARCH == "arm" || options.GOARCH == "386") {
			switch name {
			case "timers.go":
				// Timer tests do not work because syscall.seek is implemented
				// as Assembly in mainline Go and causes linker failure
				continue
			}
		}
		if options.Target == "simavr" {
			// Not all tests are currently supported on AVR.
			// Skip the ones that aren't.
			switch name {
			case "reflect.go":
				// Reflect tests do not work due to type code issues.
				continue

			case "gc.go":
				// Does not pass due to high mark false positive rate.
				continue

			case "json.go", "stdlib.go", "testing.go":
				// Breaks interp.
				continue

			case "math.go":
				// Stuck somewhere, not sure what's happening.
				continue

			case "cgo/":
				// CGo does not work on AVR.
				continue

			case "timers.go":
				// Doesn't compile:
				//   panic: compiler: could not store type code number inside interface type code
				continue

			default:
			}
		}
		name := name // redefine to avoid race condition
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runTest(name, options, t, nil, nil)
		})
	}
	if !strings.HasPrefix(spec.Emulator, "simavr ") {
		t.Run("env.go", func(t *testing.T) {
			t.Parallel()
			runTest("env.go", options, t, []string{"first", "second"}, []string{"ENV1=VALUE1", "ENV2=VALUE2"})
		})
	}
	if options.Target == "wasi" || options.Target == "wasm" {
		t.Run("alias.go-scheduler-none", func(t *testing.T) {
			t.Parallel()
			options := compileopts.Options(options)
			options.Scheduler = "none"
			runTest("alias.go", options, t, nil, nil)
		})
	}
	if options.Target == "" || options.Target == "wasi" {
		t.Run("filesystem.go", func(t *testing.T) {
			t.Parallel()
			runTest("filesystem.go", options, t, nil, nil)
		})
	}
	if options.Target == "" || options.Target == "wasi" || options.Target == "wasm" {
		t.Run("rand.go", func(t *testing.T) {
			t.Parallel()
			runTest("rand.go", options, t, nil, nil)
		})
	}
	if options.Target != "wasi" && options.Target != "wasm" {
		// The recover() builtin isn't supported yet on WebAssembly and Windows.
		t.Run("recover.go", func(t *testing.T) {
			t.Parallel()
			runTest("recover.go", options, t, nil, nil)
		})
	}
}

func emuCheck(t *testing.T, options compileopts.Options) {
	// Check if the emulator is installed.
	spec, err := compileopts.LoadTarget(&options)
	if err != nil {
		t.Fatal("failed to load target spec:", err)
	}
	if spec.Emulator != "" {
		emulatorCommand := strings.SplitN(spec.Emulator, " ", 2)[0]
		_, err := exec.LookPath(emulatorCommand)
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				t.Skipf("emulator not installed: %q", emulatorCommand)
			}

			t.Errorf("searching for emulator: %v", err)
			return
		}
	}
}

func optionsFromTarget(target string, sema chan struct{}) compileopts.Options {
	return compileopts.Options{
		// GOOS/GOARCH are only used if target == ""
		GOOS:          goenv.Get("GOOS"),
		GOARCH:        goenv.Get("GOARCH"),
		GOARM:         goenv.Get("GOARM"),
		Target:        target,
		Semaphore:     sema,
		InterpTimeout: 180 * time.Second,
		Debug:         true,
		VerifyIR:      true,
		Opt:           "z",
	}
}

// optionsFromOSARCH returns a set of options based on the "osarch" string. This
// string is in the form of "os/arch/subarch", with the subarch only sometimes
// being necessary. Examples are "darwin/amd64" or "linux/arm/7".
func optionsFromOSARCH(osarch string, sema chan struct{}) compileopts.Options {
	parts := strings.Split(osarch, "/")
	options := compileopts.Options{
		GOOS:          parts[0],
		GOARCH:        parts[1],
		Semaphore:     sema,
		InterpTimeout: 180 * time.Second,
		Debug:         true,
		VerifyIR:      true,
		Opt:           "z",
	}
	if options.GOARCH == "arm" {
		options.GOARM = parts[2]
	}
	return options
}

func runTest(name string, options compileopts.Options, t *testing.T, cmdArgs, environmentVars []string) {
	runTestWithConfig(name, t, options, cmdArgs, environmentVars)
}

func runTestWithConfig(name string, t *testing.T, options compileopts.Options, cmdArgs, environmentVars []string) {
	// Get the expected output for this test.
	// Note: not using filepath.Join as it strips the path separator at the end
	// of the path.
	path := TESTDATA + "/" + name
	// Get the expected output for this test.
	txtpath := path[:len(path)-3] + ".txt"
	if path[len(path)-1] == '/' {
		txtpath = path + "out.txt"
	}
	expected, err := os.ReadFile(txtpath)
	if err != nil {
		t.Fatal("could not read expected output file:", err)
	}

	config, err := builder.NewConfig(&options)
	if err != nil {
		t.Fatal(err)
	}

	// Build the test binary.
	stdout := &bytes.Buffer{}
	_, err = buildAndRun("./"+path, config, stdout, cmdArgs, environmentVars, time.Minute, func(cmd *exec.Cmd, result builder.BuildResult) error {
		return cmd.Run()
	})
	if err != nil {
		printCompilerError(t.Log, err)
		t.Fail()
		return
	}

	// putchar() prints CRLF, convert it to LF.
	actual := bytes.Replace(stdout.Bytes(), []byte{'\r', '\n'}, []byte{'\n'}, -1)
	expected = bytes.Replace(expected, []byte{'\r', '\n'}, []byte{'\n'}, -1) // for Windows

	if config.EmulatorName() == "simavr" {
		// Strip simavr log formatting.
		actual = bytes.Replace(actual, []byte{0x1b, '[', '3', '2', 'm'}, nil, -1)
		actual = bytes.Replace(actual, []byte{0x1b, '[', '0', 'm'}, nil, -1)
		actual = bytes.Replace(actual, []byte{'.', '.', '\n'}, []byte{'\n'}, -1)
		actual = bytes.Replace(actual, []byte{'\n', '.', '\n'}, []byte{'\n', '\n'}, -1)
	}
	if name == "testing.go" {
		// Strip actual time.
		re := regexp.MustCompile(`\([0-9]\.[0-9][0-9]s\)`)
		actual = re.ReplaceAllLiteral(actual, []byte{'(', '0', '.', '0', '0', 's', ')'})
	}

	// Check whether the command ran successfully.
	fail := false
	if err != nil {
		t.Log("failed to run:", err)
		fail = true
	} else if !bytes.Equal(expected, actual) {
		t.Logf("output did not match (expected %d bytes, got %d bytes):", len(expected), len(actual))
		fail = true
	}

	if fail {
		r := bufio.NewReader(bytes.NewReader(actual))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			t.Log("stdout:", line[:len(line)-1])
		}
		t.Fail()
	}
}

func TestTest(t *testing.T) {
	t.Parallel()

	type targ struct {
		name string
		opts compileopts.Options
	}
	targs := []targ{
		// Host
		{"Host", optionsFromTarget("", sema)},
	}
	if !testing.Short() {
		if runtime.GOOS == "linux" {
			for name, osArch := range supportedLinuxArches {
				options := optionsFromOSARCH(osArch, sema)
				if options.GOARCH != runtime.GOARCH { // Native architecture already run above.
					targs = append(targs, targ{name, options})
				}
			}
		}

		targs = append(targs,
			// QEMU microcontrollers
			targ{"EmulatedCortexM3", optionsFromTarget("cortex-m-qemu", sema)},
			targ{"EmulatedRISCV", optionsFromTarget("riscv-qemu", sema)},

			// Node/Wasmtime
			targ{"WASM", optionsFromTarget("wasm", sema)},
			targ{"WASI", optionsFromTarget("wasi", sema)},
		)
	}
	for _, targ := range targs {
		targ := targ
		t.Run(targ.name, func(t *testing.T) {
			t.Parallel()

			emuCheck(t, targ.opts)

			t.Run("Pass", func(t *testing.T) {
				t.Parallel()

				// Test a package which builds and passes normally.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/pass", out, out, &opts, false, false, false, "", "", "", false, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if !passed {
					t.Error("test failed")
				}
			})

			t.Run("Fail", func(t *testing.T) {
				t.Parallel()

				// Test a package which builds fine but fails.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/fail", out, out, &opts, false, false, false, "", "", "", false, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if passed {
					t.Error("test passed")
				}
			})

			if targ.name != "Host" {
				// Emulated tests are somewhat slow, and these do not need to be run across every platform.
				return
			}

			t.Run("Nothing", func(t *testing.T) {
				t.Parallel()

				// Test a package with no test files.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				var output bytes.Buffer
				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/nothing", io.MultiWriter(&output, out), out, &opts, false, false, false, "", "", "", false, "")
				if err != nil {
					t.Errorf("test error: %v", err)
				}
				if !passed {
					t.Error("test failed")
				}
				if !strings.Contains(output.String(), "[no test files]") {
					t.Error("missing [no test files] in output")
				}
			})

			t.Run("BuildErr", func(t *testing.T) {
				t.Parallel()

				// Test a package which fails to build.

				var wg sync.WaitGroup
				defer wg.Wait()

				out := ioLogger(t, &wg)
				defer out.Close()

				opts := targ.opts
				passed, err := Test("github.com/tinygo-org/tinygo/tests/testing/builderr", out, out, &opts, false, false, false, "", "", "", false, "")
				if err == nil {
					t.Error("test did not error")
				}
				if passed {
					t.Error("test passed")
				}
			})
		})
	}
}

func ioLogger(t *testing.T, wg *sync.WaitGroup) io.WriteCloser {
	r, w := io.Pipe()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.Close()

		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			t.Log(scanner.Text())
		}
	}()

	return w
}

func TestGetListOfPackages(t *testing.T) {
	opts := optionsFromTarget("", sema)
	tests := []struct {
		pkgs          []string
		expectedPkgs  []string
		expectesError bool
	}{
		{
			pkgs: []string{"./tests/testing/recurse/..."},
			expectedPkgs: []string{
				"github.com/tinygo-org/tinygo/tests/testing/recurse",
				"github.com/tinygo-org/tinygo/tests/testing/recurse/subdir",
			},
		},
		{
			pkgs: []string{"./tests/testing/pass"},
			expectedPkgs: []string{
				"github.com/tinygo-org/tinygo/tests/testing/pass",
			},
		},
		{
			pkgs:          []string{"./tests/testing"},
			expectesError: true,
		},
	}

	for _, test := range tests {
		actualPkgs, err := getListOfPackages(test.pkgs, &opts)
		if err != nil && !test.expectesError {
			t.Errorf("unexpected error: %v", err)
		} else if err == nil && test.expectesError {
			t.Error("expected error, but got none")
		}

		if !reflect.DeepEqual(test.expectedPkgs, actualPkgs) {
			t.Errorf("expected two slices to be equal, expected %v got %v", test.expectedPkgs, actualPkgs)
		}
	}
}

// This TestMain is necessary because TinyGo may also be invoked to run certain
// LLVM tools in a separate process. Not capturing these invocations would lead
// to recursive tests.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "clang", "ld.lld", "wasm-ld":
			// Invoke a specific tool.
			err := builder.RunTool(os.Args[1], os.Args[2:]...)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	// Run normal tests.
	os.Exit(m.Run())
}
