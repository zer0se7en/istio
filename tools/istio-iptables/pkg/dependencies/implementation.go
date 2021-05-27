// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dependencies

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/tools/istio-iptables/pkg/constants"
)

// XTablesExittype is the exit type of xtables commands.
type XTablesExittype int

// Learn from `xtables_exittype` of iptables.
// `XTF_ONLY_ONCE`, `XTF_NO_INVERT`, `XTF_BAD_VALUE`, `XTF_ONE_ACTION` will eventually turned out to be a
// parameter problem with explicit error message. Thus, we do not need to support them here.
const (
	// XTablesOtherProblem indicates a problem of other type in xtables
	XTablesOtherProblem XTablesExittype = iota + 1
	// XTablesParameterProblem indicates a parameter problem in xtables
	XTablesParameterProblem
	// XTablesVersionProblem indicates a version problem in xtables
	XTablesVersionProblem
	// XTablesResourceProblem indicates a resource problem in xtables
	XTablesResourceProblem
)

var exittypeToString = map[XTablesExittype]string{
	XTablesOtherProblem:     "xtables other problem",
	XTablesParameterProblem: "xtables parameter problem",
	XTablesVersionProblem:   "xtables version problem",
	XTablesResourceProblem:  "xtables resource problem",
}

// XTablesCmds is the set of all the xtables-related commands currently supported.
var XTablesCmds = sets.NewSet(
	constants.IPTABLES,
	constants.IP6TABLES,
	constants.IPTABLESRESTORE,
	constants.IP6TABLESRESTORE,
	constants.IPTABLESSAVE,
	constants.IP6TABLESSAVE,
)

// RealDependencies implementation of interface Dependencies, which is used in production
type RealDependencies struct{}

func (r *RealDependencies) execute(cmd string, redirectStdout bool, args ...string) error {
	fmt.Printf("%s %s\n", cmd, strings.Join(args, " "))
	externalCommand := exec.Command(cmd, args...)
	externalCommand.Stdout = os.Stdout
	// TODO Check naming and redirection logic
	if !redirectStdout {
		externalCommand.Stderr = os.Stderr
	}
	return externalCommand.Run()
}

func (r *RealDependencies) executeXTables(cmd string, redirectStdout bool, args ...string) error {
	fmt.Printf("%s %s\n", cmd, strings.Join(args, " "))
	externalCommand := exec.Command(cmd, args...)
	externalCommand.Stdout = os.Stdout

	var stderr bytes.Buffer
	// TODO Check naming and redirection logic
	if !redirectStdout {
		externalCommand.Stderr = &stderr
	}

	err := externalCommand.Run()
	// TODO Check naming and redirection logic
	if err != nil && !redirectStdout {
		stderrStr := stderr.String()

		// Transform to xtables-specific error messages with more useful and actionable hints.
		stderrStr = transformToXTablesErrorMessage(stderrStr, err)

		// Print stderr to os.Stderr by default.
		fmt.Fprintln(os.Stderr, stderrStr)
	}

	return err
}

// transformToXTablesErrorMessage returns an updated error message with explicit xtables error hints, if applicable.
func transformToXTablesErrorMessage(stderr string, err error) string {
	exitcode := err.(*exec.ExitError).ExitCode()

	if errtypeStr, ok := exittypeToString[XTablesExittype(exitcode)]; ok {
		// The original stderr is something like:
		// `prog_name + prog_vers: error hints`
		// `(optional) try help information`.
		// e.g.,
		// `iptables 1.8.4 (legacy): Couldn't load target 'ISTIO_OUTPUT':No such file or directory`
		// `Try 'iptables -h' or 'iptables --help' for more information.`
		// Reusing the `error hints` and optional `try help information` parts of the original stderr to form
		// an error message with explicit xtables error information.
		return fmt.Sprintf("%v: %v", errtypeStr, strings.Trim(strings.SplitN(stderr, ":", 2)[1], " "))
	}

	return stderr
}

// RunOrFail runs a command and exits with an error message, if it fails
func (r *RealDependencies) RunOrFail(cmd string, args ...string) {
	var err error
	if XTablesCmds.Contains(cmd) {
		err = r.executeXTables(cmd, false, args...)
	} else {
		err = r.execute(cmd, false, args...)
	}
	if err != nil {
		fmt.Printf("Failed to execute: %s %s, %v\n", cmd, strings.Join(args, " "), err)
		os.Exit(-1)
	}
}

// Run runs a command
func (r *RealDependencies) Run(cmd string, args ...string) (err error) {
	if XTablesCmds.Contains(cmd) {
		err = r.executeXTables(cmd, false, args...)
	} else {
		err = r.execute(cmd, false, args...)
	}
	return err
}

// RunQuietlyAndIgnore runs a command quietly and ignores errors
func (r *RealDependencies) RunQuietlyAndIgnore(cmd string, args ...string) {
	if XTablesCmds.Contains(cmd) {
		_ = r.executeXTables(cmd, true, args...)
	} else {
		_ = r.execute(cmd, true, args...)
	}
}
