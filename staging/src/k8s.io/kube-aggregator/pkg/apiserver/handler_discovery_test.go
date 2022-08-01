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

package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	restful "github.com/emicklei/go-restful/v3"
	fuzz "github.com/google/gofuzz"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/v2"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
)

// Test that the discovery manager starts and aggregates from two local API services
func TestBasic(t *testing.T) {
	service1 := discoveryendpoint.NewResourceManager(legacyscheme.Codecs)
	service2 := discoveryendpoint.NewResourceManager(legacyscheme.Codecs)
	apiGroup1 := fuzzAPIGroups(2, 5, 25)
	apiGroup2 := fuzzAPIGroups(2, 5, 50)
	service1.SetGroups(apiGroup1.Groups)
	service2.SetGroups(apiGroup2.Groups)
	aggregatedManager := NewDiscoveryManager(legacyscheme.Codecs)
	aggregatedManager.AddLocalAPIService("service1", service1)
	aggregatedManager.AddLocalAPIService("service2", service2)
	container := restful.NewContainer()
	container.Add(aggregatedManager.WebService())
	testCtx, _ := context.WithCancel(context.Background())
	err := aggregatedManager.Run(testCtx.Done())
	if err != nil {
		t.Fatalf("unexpected error while starting ResourceDiscoveryManager: %e", err)
	}
	response, _, parsed := fetchPath(container, "/discovery/v2", "")
	if response.StatusCode != 200 {
		t.Fatalf("unexpected status code %d", response.StatusCode)
	}
	checkAPIGroups(t, apiGroup1, parsed)
	checkAPIGroups(t, apiGroup2, parsed)
}

func checkAPIGroups(t *testing.T, api metav1.DiscoveryAPIGroupList, response *metav1.DiscoveryAPIGroupList) {
	if len(response.Groups) < len(api.Groups) {
		t.Errorf("expected to check for at least %d groups, only have %d groups in response", len(api.Groups), len(response.Groups))
	}
	for _, knownGroup := range api.Groups {
		found := false
		for _, possibleGroup := range response.Groups {
			if knownGroup.Name == possibleGroup.Name {
				t.Logf("found %s", knownGroup.Name)
				found = true
			}
		}
		if found == false {
			t.Errorf("could not find %s", knownGroup.Name)
		}
	}
}

// Test that a handler associated with an APIService gets pinged after the
// APIService has been marked as dirty
func TestDirty(t *testing.T) {
	pinged := false
	service := discoveryendpoint.NewResourceManager(legacyscheme.Codecs)
	aggregatedManager := NewDiscoveryManager(legacyscheme.Codecs)
	aggregatedManager.AddLocalAPIService("service", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pinged = true
		service.ServeHTTP(w, r)
	}))
	container := restful.NewContainer()
	container.Add(aggregatedManager.WebService())
	testCtx, _ := context.WithCancel(context.Background())
	err := aggregatedManager.Run(testCtx.Done())
	if err != nil {
		t.Fatalf("unexpected error while starting ResourceDiscoveryManager: %e", err)
	}
	// immediately check for ping, since Run() should block for local services
	if !pinged {
		t.Errorf("service handler never pinged")
	}
}

// Show that an APIService can be removed and that its group no longer remains
// if there are no versions
func TestRemoveAPIService(t *testing.T) {
	service := discoveryendpoint.NewResourceManager(legacyscheme.Codecs)
	apiGroup := fuzzAPIGroups(2, 3, 10)
	service.SetGroups(apiGroup.Groups)
	aggregatedManager := NewDiscoveryManager(legacyscheme.Codecs)
	aggregatedManager.AddAPIService(&apiregistrationv1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "serviceName",
		},
		Spec: apiregistrationv1.APIServiceSpec{
			Service: &apiregistrationv1.ServiceReference{
				Namespace: "serviceNamespace",
				Name:      "serviceName",
			},
		},
	}, service)
	container := restful.NewContainer()
	container.Add(aggregatedManager.WebService())
	testCtx, _ := context.WithCancel(context.Background())
	err := aggregatedManager.Run(testCtx.Done())
	if err != nil {
		t.Fatalf("unexpected error while starting ResourceDiscoveryManager: %e", err)
	}
	aggregatedManager.RemoveAPIService("serviceName")
	err = aggregatedManager.Run(testCtx.Done())
	if err != nil {
		t.Fatalf("unexpected error while starting ResourceDiscoveryManager: %e", err)
	}
	response, _, parsed := fetchPath(container, "/discovery/v2", "")
	if response.StatusCode != 200 {
		t.Fatalf("unexpected status code %d", response.StatusCode)
	}
	if len(parsed.Groups) > 0 {
		t.Errorf("expected to find no groups after service deletion (got %d groups)", len(parsed.Groups))
	}
}

// copied from staging/src/k8s.io/apiserver/pkg/endpoints/discovery/v2/handler_test.go
func fuzzAPIGroups(atLeastNumGroups, maxNumGroups int, seed int64) metav1.DiscoveryAPIGroupList {
	fuzzer := fuzz.NewWithSeed(seed)
	fuzzer.NumElements(atLeastNumGroups, maxNumGroups)
	fuzzer.NilChance(0)
	fuzzer.Funcs(func(o *metav1.DiscoveryAPIGroup, c fuzz.Continue) {
		c.FuzzNoCustom(o)

		// The ResourceManager will just not serve the grouop if its versions
		// list is empty
		atLeastOne := metav1.DiscoveryGroupVersion{}
		c.Fuzz(&atLeastOne)
		o.Versions = append(o.Versions, atLeastOne)

		o.TypeMeta = metav1.TypeMeta{
			Kind:       "DiscoveryAPIGroup",
			APIVersion: "v1",
		}
	})

	var apis []metav1.DiscoveryAPIGroup
	fuzzer.Fuzz(&apis)

	return metav1.DiscoveryAPIGroupList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DiscoveryAPIGroupList",
			APIVersion: "v1",
		},
		Groups: apis,
	}

}

// copied from staging/src/k8s.io/apiserver/pkg/endpoints/discovery/v2/handler_test.go
func fetchPath(handler http.Handler, path string, etag string) (*http.Response, []byte, *metav1.DiscoveryAPIGroupList) {
	// Expect json-formatted apis group list
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/discovery/v2", nil)

	// Ask for JSON response
	req.Header.Set("Accept", runtime.ContentTypeProtobuf)

	if etag != "" {
		// Quote provided etag if unquoted
		quoted := etag
		if !strings.HasPrefix(etag, "\"") {
			quoted = strconv.Quote(etag)
		}
		req.Header.Set("If-None-Match", quoted)
	}

	handler.ServeHTTP(w, req)

	bytes := w.Body.Bytes()
	var decoded *metav1.DiscoveryAPIGroupList
	if len(bytes) > 0 {
		decoded = &metav1.DiscoveryAPIGroupList{}
		runtime.DecodeInto(legacyscheme.Codecs.UniversalDecoder(), bytes, decoded)
	}

	return w.Result(), bytes, decoded
}
