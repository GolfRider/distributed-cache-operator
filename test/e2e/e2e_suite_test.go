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

// Package e2e contains end-to-end tests that run against a real Kubernetes
// cluster (typically kind). Unlike unit tests, these exercise the full
// stack: API server, controller-runtime, owned-resource reconciliation,
// and pod scheduling.
//
// Prerequisites:
//   - A kind cluster named dcache-dev (or override with KIND_CLUSTER env var)
//   - The CRD installed (`make install`)
//   - tiny-cache:dev image loaded (`make kind-load-tiny-cache`)
//   - The operator running locally (`make run`) OR deployed in-cluster
//
// These tests are slow (1-2 minutes) and intentionally exclude unit-style
// scenarios. They answer a single question: does the system work end-to-end?
package e2e

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "distributed-cache-operator e2e")
}
