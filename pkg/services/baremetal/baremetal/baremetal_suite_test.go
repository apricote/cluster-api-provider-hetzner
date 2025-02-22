/*
Copyright 2022 The Kubernetes Authors.

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

package baremetal

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/klog/v2/klogr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"github.com/syself/cluster-api-provider-hetzner/pkg/scope"
)

var log = klogr.New()

func TestBaremetal(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Baremetal Suite")
}

func newTestService(
	bmMachine *infrav1.HetznerBareMetalMachine,
	client client.Client,
) *Service {
	return &Service{
		&scope.BareMetalMachineScope{
			Logger:           log,
			Client:           client,
			BareMetalMachine: bmMachine,
		},
	}
}
