/*
Copyright The Kubernetes Authors.

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

package storage

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	api "k8s.io/kubernetes/pkg/apis/core"
	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
)

func BenchmarkPatchPod(b *testing.B) {
	benchmarkPatchPod(b, false)
}

func BenchmarkPatchPodNoMF(b *testing.B) {
	benchmarkPatchPod(b, true)
}

func benchmarkPatchPod(b *testing.B, noMF bool) {
	podStorage, _, scope, _, admit, store, pod := setupBenchmarkPatch(b, noMF)
	patchTypes := []string{string(types.StrategicMergePatchType)}
	ctx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "patch",
		APIVersion:        "v1",
		Resource:          "pods",
		Namespace:         pod.Namespace,
		Name:              pod.Name,
	}), pod.Namespace))
	target := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s?fieldManager=patch-manager", pod.Namespace, pod.Name)

	patch := func(i int) {
		body := bytes.NewReader(fmt.Appendf(nil, `{"metadata":{"labels":{"bench-updated":"%d"}}}`, i))
		req := httptest.NewRequestWithContext(ctx, http.MethodPatch, target, body)
		req.Header.Set("Content-Type", string(types.StrategicMergePatchType))
		req.Header.Set("Accept", runtime.ContentTypeProtobuf)
		w := httptest.NewRecorder()
		handlers.PatchResource(podStorage, scope, admit, patchTypes)(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}
	patch(0)
	if noMF && len(store.slots[0].cachedObj.(*api.Pod).ManagedFields) != 0 {
		b.Fatalf("expected 0 managedFields in NoMF benchmark, got %d", len(store.slots[0].cachedObj.(*api.Pod).ManagedFields))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		patch(i + 1)
	}
}

func BenchmarkPatchPodStatus(b *testing.B) {
	benchmarkPatchPodStatus(b, false)
}

func BenchmarkPatchPodStatusNoMF(b *testing.B) {
	benchmarkPatchPodStatus(b, true)
}

func benchmarkPatchPodStatus(b *testing.B, noMF bool) {
	_, statusStorage, _, statusScope, admit, store, pod := setupBenchmarkPatch(b, noMF)
	patchTypes := []string{string(types.StrategicMergePatchType)}
	ctx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "patch",
		APIVersion:        "v1",
		Resource:          "pods",
		Subresource:       "status",
		Namespace:         pod.Namespace,
		Name:              pod.Name,
	}), pod.Namespace))
	target := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/status?fieldManager=kubelet", pod.Namespace, pod.Name)

	patch := func(i int) {
		readyStatus := "True"
		if i%2 == 0 {
			readyStatus = "False"
		}
		body := bytes.NewReader(fmt.Appendf(nil,
			`{"status":{"conditions":[{"type":"Ready","status":%q,"message":"kubelet-probe-%d"},{"type":"ContainersReady","status":%q}]}}`,
			readyStatus, i, readyStatus))
		req := httptest.NewRequestWithContext(ctx, http.MethodPatch, target, body)
		req.Header.Set("Content-Type", string(types.StrategicMergePatchType))
		req.Header.Set("Accept", runtime.ContentTypeProtobuf)
		w := httptest.NewRecorder()
		handlers.PatchResource(statusStorage, statusScope, admit, patchTypes)(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}
	patch(0)
	if noMF && len(store.slots[0].cachedObj.(*api.Pod).ManagedFields) != 0 {
		b.Fatalf("expected 0 managedFields in NoMF benchmark, got %d", len(store.slots[0].cachedObj.(*api.Pod).ManagedFields))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		patch(i + 1)
	}
}

func newBenchmarkRequestScope(b *testing.B, gvk schema.GroupVersionKind, subresource string, resetFields map[fieldpath.APIVersion]*fieldpath.Set) *handlers.RequestScope {
	b.Helper()
	scheme := legacyscheme.Scheme
	fm, err := managedfields.NewDefaultFieldManager(
		applyconfigurations.NewTypeConverter(scheme),
		runtime.UnsafeObjectConvertor(scheme),
		scheme,
		scheme,
		gvk,
		api.SchemeGroupVersion,
		subresource,
		fieldpath.NewExcludeFilterSetMap(resetFields),
	)
	if err != nil {
		b.Fatalf("failed to create field manager: %v", err)
	}
	return &handlers.RequestScope{
		Namer:               handlers.ContextBasedNaming{Namer: meta.NewAccessor()},
		Serializer:          legacyscheme.Codecs,
		ParameterCodec:      legacyscheme.ParameterCodec,
		Creater:             scheme,
		Convertor:           scheme,
		Defaulter:           scheme,
		Typer:               scheme,
		UnsafeConvertor:     runtime.UnsafeObjectConvertor(scheme),
		MaxRequestBodyBytes: 3 * 1024 * 1024,
		Kind:                gvk,
		Resource:            v1.SchemeGroupVersion.WithResource("pods"),
		Subresource:         subresource,
		MetaGroupVersion:    metav1.SchemeGroupVersion,
		HubGroupVersion:     api.SchemeGroupVersion,
		FieldManager:        fm,
	}
}

func setupBenchmarkPatch(b *testing.B, noMF bool) (*REST, *StatusREST, *handlers.RequestScope, *handlers.RequestScope, admission.Interface, *codecStorage, *api.Pod) {
	b.Helper()
	protobufInfo, ok := runtime.SerializerInfoForMediaType(legacyscheme.Codecs.SupportedMediaTypes(), runtime.ContentTypeProtobuf)
	if !ok {
		b.Fatal("protobuf serializer not found")
	}
	storageCodec := legacyscheme.Codecs.CodecForVersions(
		protobufInfo.Serializer,
		legacyscheme.Codecs.UniversalDeserializer(),
		v1.SchemeGroupVersion,
		runtime.InternalGroupVersioner,
	)
	clientProtoCodec := legacyscheme.Codecs.CodecForVersions(
		protobufInfo.Serializer,
		legacyscheme.Codecs.UniversalDeserializer(),
		v1.SchemeGroupVersion,
		v1.SchemeGroupVersion,
	)
	store := &codecStorage{
		codec:     storageCodec,
		versioner: storage.APIObjectVersioner{},
	}
	store.slots[0].rev = 1
	restOptions := generic.RESTOptions{
		StorageConfig: &storagebackend.ConfigForResource{
			Config:        storagebackend.Config{Prefix: "/registry", Codec: storageCodec},
			GroupResource: schema.GroupResource{Resource: "pods"},
		},
		Decorator: func(*storagebackend.ConfigForResource, string, generic.CacheKeyFunc, storage.ReverseKeyFunc, func() runtime.Object, func() runtime.Object, storage.AttrFunc, storage.IndexerFuncs, *cache.Indexers) (storage.Interface, factory.DestroyFunc, error) {
			return store, func() {}, nil
		},
		DeleteCollectionWorkers: 1,
		ResourcePrefix:          "pods",
	}
	podStorage, err := NewStorage(restOptions, nil, nil, nil, nil)
	if err != nil {
		b.Fatalf("unexpected error from REST storage: %v", err)
	}

	gvk := v1.SchemeGroupVersion.WithKind("Pod")
	podScope := newBenchmarkRequestScope(b, gvk, "", podStorage.Pod.GetResetFields())
	statusScope := newBenchmarkRequestScope(b, gvk, "status", podStorage.Status.GetResetFields())
	admit := admission.NewChainHandler(admission.NewHandler(admission.Create, admission.Update))

	pod := seedBenchmarkPod(b, podStorage.Pod, podStorage.Status, podScope, statusScope, admit, clientProtoCodec, store, noMF)
	return podStorage.Pod, podStorage.Status, podScope, statusScope, admit, store, pod
}

func seedBenchmarkPod(b *testing.B, podREST *REST, statusREST *StatusREST, podScope, statusScope *handlers.RequestScope, admit admission.Interface, clientProtoCodec runtime.Codec, store *codecStorage, noMF bool) *api.Pod {
	b.Helper()
	const name = "pod-00000"
	const padBytes = 3000
	template := makeRealisticMultiContainerPod(name, padBytes)
	desiredStatus := template.Status
	template.Status = v1.PodStatus{}

	createBytes, err := runtime.Encode(clientProtoCodec, template)
	if err != nil {
		b.Fatalf("encode create: %v", err)
	}
	createCtx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "create",
		APIVersion:        "v1",
		Resource:          "pods",
		Namespace:         "default",
	}), "default"))
	reqCreate := httptest.NewRequestWithContext(createCtx, http.MethodPost, "/api/v1/namespaces/default/pods?fieldManager=kube-controller-manager", bytes.NewReader(createBytes))
	reqCreate.Header.Set("Content-Type", runtime.ContentTypeProtobuf)
	reqCreate.Header.Set("Accept", runtime.ContentTypeProtobuf)
	wCreate := httptest.NewRecorder()
	handlers.CreateResource(podREST, podScope, admit)(wCreate, reqCreate)
	if wCreate.Code != http.StatusCreated {
		b.Fatalf("create pod failed %d: %s", wCreate.Code, wCreate.Body.String())
	}

	if noMF {
		// Match the CREATE-only MutatingAdmissionPolicy that clears metadata.managedFields on Pod creation.
		slot := &store.slots[0]
		cachedPod := slot.cachedObj.(*api.Pod).DeepCopy()
		cachedPod.ManagedFields = nil
		data, err := runtime.Encode(store.codec, cachedPod)
		if err != nil {
			b.Fatalf("re-encode stripped pod: %v", err)
		}
		slot.data = data
		slot.cachedObj = cachedPod
	}

	var createdPod v1.Pod
	if err := runtime.DecodeInto(clientProtoCodec, wCreate.Body.Bytes(), &createdPod); err != nil {
		b.Fatalf("decode created pod: %v", err)
	}
	if noMF {
		createdPod.ManagedFields = nil
	}
	createdPod.Status = desiredStatus
	statusBytes, err := runtime.Encode(clientProtoCodec, &createdPod)
	if err != nil {
		b.Fatalf("encode status: %v", err)
	}

	statusCtx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "update",
		APIVersion:        "v1",
		Resource:          "pods",
		Subresource:       "status",
		Namespace:         "default",
		Name:              name,
	}), "default"))
	reqStatus := httptest.NewRequestWithContext(statusCtx, http.MethodPut, fmt.Sprintf("/api/v1/namespaces/default/pods/%s/status?fieldManager=kubelet", name), bytes.NewReader(statusBytes))
	reqStatus.Header.Set("Content-Type", runtime.ContentTypeProtobuf)
	reqStatus.Header.Set("Accept", runtime.ContentTypeProtobuf)
	wStatus := httptest.NewRecorder()
	handlers.UpdateResource(statusREST, statusScope, admit)(wStatus, reqStatus)
	if wStatus.Code != http.StatusOK {
		b.Fatalf("put status failed %d: %s", wStatus.Code, wStatus.Body.String())
	}

	patchTypes := []string{string(types.StrategicMergePatchType)}
	patchPodCtx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "patch",
		APIVersion:        "v1",
		Resource:          "pods",
		Namespace:         "default",
		Name:              name,
	}), "default"))
	reqPatch1 := httptest.NewRequestWithContext(patchPodCtx, http.MethodPatch, fmt.Sprintf("/api/v1/namespaces/default/pods/%s?fieldManager=patch-manager", name), bytes.NewReader([]byte(`{"metadata":{"labels":{"bench-updated":"0"}}}`)))
	reqPatch1.Header.Set("Content-Type", string(types.StrategicMergePatchType))
	reqPatch1.Header.Set("Accept", runtime.ContentTypeProtobuf)
	wPatch1 := httptest.NewRecorder()
	handlers.PatchResource(podREST, podScope, admit, patchTypes)(wPatch1, reqPatch1)
	if wPatch1.Code != http.StatusOK {
		b.Fatalf("patch pod failed %d: %s", wPatch1.Code, wPatch1.Body.String())
	}

	patchStatusCtx := audit.WithAuditContext(request.WithNamespace(request.WithRequestInfo(context.Background(), &request.RequestInfo{
		IsResourceRequest: true,
		Verb:              "patch",
		APIVersion:        "v1",
		Resource:          "pods",
		Subresource:       "status",
		Namespace:         "default",
		Name:              name,
	}), "default"))
	reqPatch2 := httptest.NewRequestWithContext(patchStatusCtx, http.MethodPatch, fmt.Sprintf("/api/v1/namespaces/default/pods/%s/status?fieldManager=kubelet", name), bytes.NewReader(fmt.Appendf(nil, `{"metadata":{"uid":%q},"status":{"conditions":[{"type":"Ready","status":"False","message":"kubelet-probe-0"},{"type":"ContainersReady","status":"False"}]}}`, createdPod.UID)))
	reqPatch2.Header.Set("Content-Type", string(types.StrategicMergePatchType))
	reqPatch2.Header.Set("Accept", runtime.ContentTypeProtobuf)
	wPatch2 := httptest.NewRecorder()
	handlers.PatchResource(statusREST, statusScope, admit, patchTypes)(wPatch2, reqPatch2)
	if wPatch2.Code != http.StatusOK {
		b.Fatalf("patch status failed %d: %s", wPatch2.Code, wPatch2.Body.String())
	}

	return store.slots[0].cachedObj.(*api.Pod)
}

func makeRealisticMultiContainerPod(name string, padBytes int) *v1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/component": "backend",
		"app.kubernetes.io/instance":  "payment-service-primary",
		"app.kubernetes.io/name":      "payment-service",
		"app.kubernetes.io/part-of":   "ecommerce-platform",
		"app.kubernetes.io/version":   "v2.1.4",
		"env":                         "production",
		"group":                       "load",
		"name":                        "large-deployment-30kb",
		"pod-template-hash":           "67cc84df8d",
		"svc":                         "large-service-30kb",
		"track":                       "canary",
		"topology.kubernetes.io/zone": "us-west1-b",
		"tier":                        "control-plane-bench",
		"bench-shard":                 name,
	}
	annotations := map[string]string{
		"argocd.argoproj.io/hook":                         "PreSync",
		"argocd.argoproj.io/hook-delete-policy":           "HookSucceeded",
		"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
		"fluentbit.io/parser":                             "json",
		"prometheus.io/path":                              "/metrics",
		"prometheus.io/port":                              "8080",
		"prometheus.io/scrape":                            "false",
		"sidecar.istio.io/inject":                         "false",
		"vault.hashicorp.com/agent-inject":                "false",
		"vault.hashicorp.com/role":                        "my-app-db-role",
	}
	if padBytes > 0 {
		chunkSize := 512
		idx := 0
		rem := padBytes
		for rem > 0 {
			n := chunkSize
			if rem < n {
				n = rem
			}
			annotations[fmt.Sprintf("config.kubernetes.io/data-shard-%02d", idx)] = strings.Repeat("x", n)
			rem -= n
			idx++
		}
	}

	makeEnvVars := func(cName string) []v1.EnvVar {
		return []v1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
				},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
				},
			},
			{
				Name: "NODE_NAME",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "spec.nodeName"},
				},
			},
			{
				Name: "POD_IP",
				ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"},
				},
			},
			{
				Name: "GOMAXPROCS",
				ValueFrom: &v1.EnvVarSource{
					ResourceFieldRef: &v1.ResourceFieldSelector{
						ContainerName: cName,
						Divisor:       apiresource.MustParse("0"),
						Resource:      "limits.cpu",
					},
				},
			},
			{
				Name: "MEM_LIMIT_BYTES",
				ValueFrom: &v1.EnvVarSource{
					ResourceFieldRef: &v1.ResourceFieldSelector{
						ContainerName: cName,
						Divisor:       apiresource.MustParse("0"),
						Resource:      "limits.memory",
					},
				},
			},
			{Name: "JAVA_TOOL_OPTIONS", Value: "-XX:MaxRAMPercentage=75.0 -XX:+CrashOnOutOfMemoryError -XX:+UseG1GC"},
			{Name: "REDIS_URL", Value: "redis://redis-master.cache.svc.cluster.local:6379/0"},
			{Name: "DATABASE_URL", Value: "postgresql://app_rw@postgres-primary.db.svc.cluster.local:5432/payments?sslmode=require"},
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel-collector.monitoring.svc.cluster.local:4317"},
			{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: "service.namespace=ecommerce,service.version=v2.1.4,deployment.environment=production"},
			{Name: "PYTHONUNBUFFERED", Value: "1"},
			{Name: "NODE_ENV", Value: "production"},
			{Name: "LOG_FORMAT", Value: "json-structured-v2"},
		}
	}

	mounts := []v1.VolumeMount{
		{Name: "configmap", MountPath: "/var/configmap"},
		{Name: "secret", MountPath: "/var/secret"},
		{Name: "cache-volume", MountPath: "/var/cache/app"},
		{Name: "kube-api-access-sjcs6", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
	}

	containerNames := []string{"main", "istio-proxy", "fluentbit-logger", "otel-agent", "vault-agent"}
	containers := make([]v1.Container, len(containerNames))
	containerStatuses := make([]v1.ContainerStatus, len(containerNames))
	recursiveDisabled := v1.RecursiveReadOnlyDisabled
	statusMounts := []v1.VolumeMountStatus{
		{Name: "configmap", MountPath: "/var/configmap"},
		{Name: "secret", MountPath: "/var/secret"},
		{Name: "cache-volume", MountPath: "/var/cache/app"},
		{Name: "kube-api-access-sjcs6", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true, RecursiveReadOnly: &recursiveDisabled},
	}

	for i, cName := range containerNames {
		containers[i] = v1.Container{
			Name:                     cName,
			Image:                    fmt.Sprintf("registry.k8s.io/ enterprise/%s:v2.1.%d", cName, i),
			ImagePullPolicy:          v1.PullIfNotPresent,
			Env:                      makeEnvVars(cName),
			VolumeMounts:             mounts,
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: v1.TerminationMessageReadFile,
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    apiresource.MustParse("10m"),
					v1.ResourceMemory: apiresource.MustParse("64Mi"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    apiresource.MustParse("100m"),
					v1.ResourceMemory: apiresource.MustParse("256Mi"),
				},
			},
		}
		started := true
		containerStatuses[i] = v1.ContainerStatus{
			Name:         cName,
			Image:        containers[i].Image,
			ImageID:      fmt.Sprintf("registry.k8s.io/enterprise/%s@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a%02d", cName, i),
			ContainerID:  fmt.Sprintf("containerd://bbb710b46090518dfc404bf82b59ee1852230fab410c44863c421332d64a5e%02d", i),
			Ready:        true,
			Started:      &started,
			RestartCount: 0,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{
					StartedAt: metav1.Date(2026, 4, 23, 12, 23, 21, 0, time.UTC),
				},
			},
			VolumeMounts: statusMounts,
			User: &v1.ContainerUser{
				Linux: &v1.LinuxContainerUser{
					UID:                1000,
					GID:                1000,
					SupplementalGroups: []int64{0, 1, 2, 3, 4, 6, 10, 11, 20, 26, 27},
				},
			},
		}
	}

	initNames := []string{"init-schema-check", "init-certs"}
	initContainers := make([]v1.Container, len(initNames))
	initStatuses := make([]v1.ContainerStatus, len(initNames))
	for i, iName := range initNames {
		initContainers[i] = v1.Container{
			Name:                     iName,
			Image:                    "registry.k8s.io/e2e-test-images/agnhost:2.53",
			Command:                  []string{"/bin/sh", "-c", "sleep 1"},
			ImagePullPolicy:          v1.PullIfNotPresent,
			Env:                      makeEnvVars(iName)[:8],
			VolumeMounts:             mounts,
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: v1.TerminationMessageReadFile,
		}
		started := false
		initStatuses[i] = v1.ContainerStatus{
			Name:         iName,
			Image:        "registry.k8s.io/e2e-test-images/agnhost:2.53",
			ImageID:      "registry.k8s.io/e2e-test-images/agnhost@sha256:99c6b4bb4a1e1df3f0b3752168c89358794d02258ebebc26bf21c29399011a85",
			ContainerID:  fmt.Sprintf("containerd://91e8a3c8b793f5563ab9885872c2f7ae53a351a748b92fe152b9583d3e8529%02d", i),
			Ready:        true,
			Started:      &started,
			RestartCount: 0,
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					ExitCode:    0,
					Reason:      "Completed",
					StartedAt:   metav1.Date(2026, 4, 23, 12, 23, 14, 0, time.UTC),
					FinishedAt:  metav1.Date(2026, 4, 23, 12, 23, 15, 0, time.UTC),
					ContainerID: fmt.Sprintf("containerd://91e8a3c8b793f5563ab9885872c2f7ae53a351a748b92fe152b9583d3e8529%02d", i),
				},
			},
			VolumeMounts: statusMounts,
			User: &v1.ContainerUser{
				Linux: &v1.LinuxContainerUser{
					UID:                0,
					GID:                0,
					SupplementalGroups: []int64{0, 1, 2, 3, 4, 6, 10, 11, 20, 26, 27},
				},
			},
		}
	}

	defaultMode := int32(420)
	expSeconds := int64(3607)
	graceSeconds := int64(1)
	tolSeconds := int64(900)
	enableLinks := true
	blockOwner := true
	isController := true
	preemptPolicy := v1.PreemptLowerPriority
	prio := int32(0)

	tTransition := metav1.Date(2026, 4, 23, 12, 23, 8, 0, time.UTC)

	return &v1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               "large-deployment-30kb-67cc84df8d",
					UID:                types.UID("55338d45-98ff-4b84-949d-1619bff3c641"),
					Controller:         &isController,
					BlockOwnerDeletion: &blockOwner,
				},
			},
		},
		Spec: v1.PodSpec{
			InitContainers:                initContainers,
			Containers:                    containers,
			DNSPolicy:                     v1.DNSClusterFirst,
			EnableServiceLinks:            &enableLinks,
			NodeName:                      "worker-node-01",
			PreemptionPolicy:              &preemptPolicy,
			Priority:                      &prio,
			RestartPolicy:                 v1.RestartPolicyAlways,
			SchedulerName:                 "default-scheduler",
			ServiceAccountName:            "default",
			TerminationGracePeriodSeconds: &graceSeconds,
			Tolerations: []v1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &tolSeconds},
				{Key: "node.kubernetes.io/unreachable", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute, TolerationSeconds: &tolSeconds},
			},
			Volumes: []v1.Volume{
				{
					Name: "configmap",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: v1.LocalObjectReference{Name: "large-deployment-30kb"},
							DefaultMode:          &defaultMode,
						},
					},
				},
				{
					Name: "secret",
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName:  "large-deployment-30kb",
							DefaultMode: &defaultMode,
						},
					},
				},
				{
					Name: "cache-volume",
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "kube-api-access-sjcs6",
					VolumeSource: v1.VolumeSource{
						Projected: &v1.ProjectedVolumeSource{
							DefaultMode: &defaultMode,
							Sources: []v1.VolumeProjection{
								{
									ServiceAccountToken: &v1.ServiceAccountTokenProjection{
										ExpirationSeconds: &expSeconds,
										Path:              "token",
									},
								},
								{
									ConfigMap: &v1.ConfigMapProjection{
										LocalObjectReference: v1.LocalObjectReference{Name: "kube-root-ca.crt"},
										Items:                []v1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
									},
								},
								{
									DownwardAPI: &v1.DownwardAPIProjection{
										Items: []v1.DownwardAPIVolumeFile{
											{
												Path:     "namespace",
												FieldRef: &v1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase:     v1.PodRunning,
			HostIP:    "192.168.8.8",
			HostIPs:   []v1.HostIP{{IP: "192.168.8.8"}},
			PodIP:     "10.244.7.83",
			PodIPs:    []v1.PodIP{{IP: "10.244.7.83"}},
			QOSClass:  v1.PodQOSBurstable,
			StartTime: &tTransition,
			Conditions: []v1.PodCondition{
				{Type: v1.PodReadyToStartContainers, Status: v1.ConditionTrue, LastTransitionTime: tTransition, ObservedGeneration: 1},
				{Type: v1.PodInitialized, Status: v1.ConditionTrue, LastTransitionTime: tTransition, ObservedGeneration: 1, Reason: "ContainersInitialized", Message: "all init containers completed"},
				{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: tTransition, ObservedGeneration: 1, Reason: "ContainersReady", Message: "all containers ready"},
				{Type: v1.ContainersReady, Status: v1.ConditionTrue, LastTransitionTime: tTransition, ObservedGeneration: 1, Reason: "ContainersReady", Message: "all containers ready"},
				{Type: v1.PodScheduled, Status: v1.ConditionTrue, LastTransitionTime: tTransition, ObservedGeneration: 1},
			},
			InitContainerStatuses: initStatuses,
			ContainerStatuses:     containerStatuses,
		},
	}
}

type workerCtxKey struct{}

const numCodecSlots = 256

type codecSlot struct {
	rev       uint64
	data      []byte
	cachedObj runtime.Object
	_         [128]byte // pad to avoid false sharing across cores
}

// codecStorage implements storage.Interface using the Protobuf storage codec
// (matching etcd3.store's Encode/Decode round-trip) without an in-process embedded
// etcd server polluting b.ReportAllocs().
type codecStorage struct {
	codec     runtime.Codec
	versioner storage.Versioner
	slots     [numCodecSlots]codecSlot
}

func (s *codecStorage) slotFor(ctx context.Context) *codecSlot {
	if wid, ok := ctx.Value(workerCtxKey{}).(int); ok {
		return &s.slots[wid%numCodecSlots]
	}
	return &s.slots[0]
}

func (s *codecStorage) Versioner() storage.Versioner { return s.versioner }
func (s *codecStorage) CompactRevision() int64       { return 0 }
func (s *codecStorage) ReadinessCheck() error        { return nil }
func (s *codecStorage) EnableResourceSizeEstimation(storage.KeysFunc) error {
	return nil
}
func (s *codecStorage) RequestWatchProgress(context.Context) error { return nil }
func (s *codecStorage) GetCurrentResourceVersion(ctx context.Context) (uint64, error) {
	return s.slotFor(ctx).rev, nil
}
func (s *codecStorage) Stats(context.Context) (storage.Stats, error) {
	return storage.Stats{}, nil
}
func (s *codecStorage) Watch(context.Context, string, storage.ListOptions) (watch.Interface, error) {
	return nil, nil
}
func (s *codecStorage) GetList(context.Context, string, storage.ListOptions, runtime.Object) error {
	return nil
}
func (s *codecStorage) Delete(context.Context, string, runtime.Object, *storage.Preconditions, storage.ValidateObjectFunc, runtime.Object, storage.DeleteOptions) error {
	return nil
}
func (s *codecStorage) Get(ctx context.Context, _ string, _ storage.GetOptions, objPtr runtime.Object) error {
	slot := s.slotFor(ctx)
	if err := runtime.DecodeInto(s.codec, slot.data, objPtr); err != nil {
		return err
	}
	return s.versioner.UpdateObject(objPtr, slot.rev)
}
func (s *codecStorage) Create(ctx context.Context, _ string, obj, out runtime.Object, _ uint64) error {
	slot := s.slotFor(ctx)
	if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
		return err
	}
	data, err := runtime.Encode(s.codec, obj)
	if err != nil {
		return err
	}
	slot.rev++
	slot.data = data
	slot.cachedObj = obj.DeepCopyObject()
	_ = s.versioner.UpdateObject(slot.cachedObj, slot.rev)
	if out != nil {
		if err := runtime.DecodeInto(s.codec, data, out); err != nil {
			return err
		}
		return s.versioner.UpdateObject(out, slot.rev)
	}
	return nil
}
func (s *codecStorage) GuaranteedUpdate(ctx context.Context, key string, destination runtime.Object, _ bool, preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, _ runtime.Object) error {
	slot := s.slotFor(ctx)
	curr := slot.cachedObj.DeepCopyObject()
	if preconditions != nil {
		if err := preconditions.Check(key, curr); err != nil {
			return err
		}
	}
	ret, _, err := tryUpdate(curr, storage.ResponseMeta{ResourceVersion: slot.rev})
	if err != nil {
		return err
	}
	if err := s.versioner.PrepareObjectForStorage(ret); err != nil {
		return err
	}
	data, err := runtime.Encode(s.codec, ret)
	if err != nil {
		return err
	}
	slot.rev++
	slot.data = data
	if err := runtime.DecodeInto(s.codec, data, destination); err != nil {
		return err
	}
	if err := s.versioner.UpdateObject(destination, slot.rev); err != nil {
		return err
	}
	slot.cachedObj = destination.DeepCopyObject()
	return nil
}
