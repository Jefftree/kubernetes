/*
Copyright 2016 The Kubernetes Authors.

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

package routes

import (
	restful "github.com/emicklei/go-restful"
	"k8s.io/klog/v2"

	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/kube-openapi/pkg/builder"
	"k8s.io/kube-openapi/pkg/builder3"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/handler"
	"k8s.io/kube-openapi/pkg/handler3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// OpenAPI installs spec endpoints for each web service.
type OpenAPI struct {
	Config *common.Config
}

// Install adds the SwaggerUI webservice to the given mux.
func (oa OpenAPI) Install(c *restful.Container, mux *mux.PathRecorderMux) (*handler.OpenAPIService, *spec.Swagger) {
	w := c.RegisteredWebServices()

	spec, err := builder.BuildOpenAPISpec(w, oa.Config)
	if err != nil {
		klog.Fatalf("Failed to build open api spec for root: %v", err)
	}
	spec.Definitions = handler.PruneDefaults(spec.Definitions)
	openAPIVersionedService, err := handler.NewOpenAPIService(spec)
	if err != nil {
		klog.Fatalf("Failed to create OpenAPIService: %v", err)
	}

	err = openAPIVersionedService.RegisterOpenAPIVersionedService("/openapi/v2", mux)
	if err != nil {
		klog.Fatalf("Failed to register versioned open api spec for root: %v", err)
	}

	return openAPIVersionedService, spec
}

func (oa OpenAPI) InstallV3(c *restful.Container, mux *mux.PathRecorderMux) (*handler3.OpenAPIService) {
	w := c.RegisteredWebServices()

	spec, err := builder.BuildOpenAPISpec(w, oa.Config)
	if err != nil {
		klog.Fatalf("Failed to build open api spec for root: %v", err)
	}
	// spec.Definitions = handler.PruneDefaults(spec.Definitions)
	openAPIVersionedService, err := handler3.NewOpenAPIService(spec)
	if err != nil {
		klog.Fatalf("Failed to create OpenAPIService: %v", err)
	}

	err = openAPIVersionedService.RegisterOpenAPIV3VersionedService("/openapi/v3", mux)
	if err != nil {
		klog.Fatalf("Failed to register versioned open api spec for root: %v", err)
	}

	grouped := make(map[string][]*restful.WebService)

	for _, t := range w {
		r := t.RootPath()[1:]
		grouped[r] = []*restful.WebService{t}
	}

	for x, y := range grouped {
		sc, _ := builder3.BuildOpenAPISpec(y, oa.Config)
		openAPIVersionedService.UpdateGroupVersion(x, sc)
	}

	return openAPIVersionedService
}
