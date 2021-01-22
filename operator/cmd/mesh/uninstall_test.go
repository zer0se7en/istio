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

package mesh

import (
	"bytes"
	"testing"

	"github.com/onsi/gomega"

	"istio.io/pkg/log"
)

var (
	logOpts = log.DefaultOptions()
)

func TestUninstallNonEmptyRevision(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--revision", "rev"}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestUninstallEmptyRevision(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--revision", ""}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.MatchError("at least one of the --revision(or --set revision=<revision>), --filename or --purge flags must be set"))
}

func TestUninstallNonEmptyRevisionFromSetFlags(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--set", "revision=rev"}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestUninstallEmptyRevisionFromSetFlags(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--set", "revision="}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.MatchError("at least one of the --revision(or --set revision=<revision>), --filename or --purge flags must be set"))
}

func TestUninstallEmptyRevisionAndPurge(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--revision", "", "--purge"}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestUninstallEmptyRevisionAndEmptyFile(t *testing.T) {
	t.Skip("https://github.com/istio/istio/issues/30182")
	g := gomega.NewWithT(t)
	args := []string{"--revision", "", "-f", ""}
	uninstallCmd := UninstallCmd(logOpts)
	uninstallCmd.SetArgs(args)
	var out bytes.Buffer
	uninstallCmd.SetOut(&out)
	uninstallCmd.SetErr(&out)

	err := uninstallCmd.Execute()
	g.Expect(err).To(gomega.MatchError("at least one of the --revision(or --set revision=<revision>), --filename or --purge flags must be set"))
}
