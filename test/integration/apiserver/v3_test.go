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
	"reflect"
	"testing"

	"github.com/golang/protobuf/proto"
	openapi_v3 "github.com/googleapis/gnostic/openapiv3"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	restclient "k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kubernetes/test/integration/framework"
	"sigs.k8s.io/yaml"
)

func TestOpenAPIV3Spec(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.OpenAPIV3, true)()
	controlPlaneConfig := framework.NewIntegrationTestControlPlaneConfigWithOptions(&framework.ControlPlaneConfigOptions{})
	controlPlaneConfig.GenericConfig.OpenAPIConfig = framework.DefaultOpenAPIConfig()
	instanceConfig, _, closeFn := framework.RunAnAPIServer(controlPlaneConfig)
	defer closeFn()
	paths := []string{
		"/apis/apps/v1",
		"/apis/authentication.k8s.io/v1",
		"/apis/policy/v1",
		"/apis/batch/v1",
		"/version",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rt, err := restclient.TransportFor(instanceConfig.GenericAPIServer.LoopbackClientConfig)
			if err != nil {
				t.Fatal(err)
			}
			// attempt to fetch and unmarshal
			url := instanceConfig.GenericAPIServer.LoopbackClientConfig.Host + "/openapi/v3" + path
			req, err := http.NewRequest("GET", url, nil)
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
			var firstSpec spec3.OpenAPI
			err = json.Unmarshal(bs, &firstSpec)
			if err != nil {
				t.Fatal(err)
			}
			specBytes, err := json.Marshal(&firstSpec)
			if err != nil {
				t.Fatal(err)
			}
			var secondSpec spec3.OpenAPI
			err = json.Unmarshal(specBytes, &secondSpec)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(firstSpec, secondSpec) {
				t.Fatal("spec mismatch")
			}

			protoReq, err := http.NewRequest("GET", instanceConfig.GenericAPIServer.LoopbackClientConfig.Host+"/openapi/v3/apis/apps/v1", nil)
			protoReq.Header.Set("Accept", "application/com.github.proto-openapi.spec.v3@v1.0+protobuf")
			protoResp, err := rt.RoundTrip(protoReq)
			if err != nil {
				t.Fatal(err)
			}
			defer protoResp.Body.Close()
			bs, err = ioutil.ReadAll(protoResp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var protoDoc openapi_v3.Document
			err = proto.Unmarshal(bs, &protoDoc)
			if err != nil {
				t.Fatal(err)
			}
			yamlBytes, err := protoDoc.YAMLValue("")
			if err != nil {
				t.Fatal(err)
			}
			jsonBytes, err := yaml.YAMLToJSON(yamlBytes)
			if err != nil {
				t.Fatal(err)
			}
			var specFromProto spec3.OpenAPI
			err = json.Unmarshal(jsonBytes, &specFromProto)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(specFromProto, firstSpec) {
				t.Fatalf("spec mismatch - specFromProto: %s\n", jsonBytes)
			}
		})
	}
}
