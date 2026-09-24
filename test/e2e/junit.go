/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/reporters"
)

// RegisterJUnitReporter arranges for a JUnit XML report to be written under
// ${ARTIFACT_DIR}/junit after the suite finishes. That is where OpenShift CI's
// junit lens looks, and where hack/lib/spyglass-report.sh reads the failures it
// summarises. It is a no-op when ARTIFACT_DIR is unset, so `make test-e2e-*` on
// a developer's own cluster writes nothing and behaves exactly as before.
//
// Call it once at suite scope, alongside RunSpecs, with a filename unique to the
// suite so the aws, azure and (future) gcp reports do not overwrite each other:
//
//	var _ = e2e.RegisterJUnitReporter("junit_aws_e2e.xml")
//
// The bool return exists only so it reads naturally in a file-scope `var _ =`,
// the same shape ginkgo's own ReportAfterSuite uses. Registering a second
// ReportAfterSuite is fine: ginkgo runs them in order, so this coexists with the
// diagnostics hook the azure suite already has.
func RegisterJUnitReporter(filename string) bool {
	return ginkgo.ReportAfterSuite("junit report", func(report ginkgo.Report) {
		dir := os.Getenv("ARTIFACT_DIR")
		if dir == "" {
			return
		}
		path := filepath.Join(dir, "junit", filename)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			ginkgo.GinkgoWriter.Printf("junit: cannot create %s: %v\n", filepath.Dir(path), err)
			return
		}
		if err := reporters.GenerateJUnitReport(report, path); err != nil {
			ginkgo.GinkgoWriter.Printf("junit: cannot write %s: %v\n", path, err)
			return
		}
		ginkgo.GinkgoWriter.Printf("junit: wrote %s\n", path)
	})
}
