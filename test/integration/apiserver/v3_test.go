/*
Copyright 2021 The Kubernetes Authors.

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

package apiserver

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"testing"

	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	restclient "k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestOpenAPIV3Spec(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.OpenAPIV3, true)()
	controlPlaneConfig := framework.NewIntegrationTestControlPlaneConfigWithOptions(&framework.ControlPlaneConfigOptions{})
	controlPlaneConfig.GenericConfig.OpenAPIConfig = framework.DefaultOpenAPIConfig()
	instanceConfig, _, closeFn := framework.RunAnAPIServer(controlPlaneConfig)
	defer closeFn()
	rt, err := restclient.TransportFor(instanceConfig.GenericAPIServer.LoopbackClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	// attempt to fetch and unmarshal
	req, err := http.NewRequest("GET", instanceConfig.GenericAPIServer.LoopbackClientConfig.Host+"/openapi/v3/apis/apps/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	spec := &spec.Swagger{}
	err = json.Unmarshal(bs, spec)
	if err != nil {
		t.Fatal(err)
	}
}
