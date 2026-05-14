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

package testing

import (
	"context"
	"fmt"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	example2v1 "k8s.io/apiserver/pkg/apis/example2/v1"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	"k8s.io/apiserver/pkg/storage/etcd3/testserver"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	"k8s.io/apiserver/pkg/storage/value/encrypt/identity"
	"k8s.io/utils/clock"
)

var (
	Scheme   = runtime.NewScheme()
	Codecs   = serializer.NewCodecFactory(Scheme)
	ErrDummy = fmt.Errorf("dummy error")

	Corev1ProtoCodec    runtime.Codec
	Examplev1ProtoCodec runtime.Codec
)

func init() {
	metav1.AddToGroupVersion(Scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(example.AddToScheme(Scheme))
	utilruntime.Must(examplev1.AddToScheme(Scheme))
	utilruntime.Must(example2v1.AddToScheme(Scheme))
	utilruntime.Must(corev1.AddToScheme(Scheme))
	utilruntime.Must(metav1.AddMetaToScheme(Scheme))
	Scheme.AddUnversionedTypes(corev1.SchemeGroupVersion, &metav1.Status{})
	pb := protobuf.NewSerializer(Scheme, Scheme)
	Corev1ProtoCodec = Codecs.CodecForVersions(pb, pb, schema.GroupVersions{corev1.SchemeGroupVersion}, nil)
	Examplev1ProtoCodec = Codecs.CodecForVersions(pb, pb, schema.GroupVersions{examplev1.SchemeGroupVersion}, nil)
}

func NewPod() runtime.Object     { return &example.Pod{} }
func NewPodList() runtime.Object { return &example.PodList{} }

func ComputePodKey(obj *example.Pod) string {
	return fmt.Sprintf("/pods/%s/%s", obj.Namespace, obj.Name)
}

func NewEtcdTestStorage(t testing.TB, prefix string) (*etcd3testing.EtcdTestServer, storage.Interface) {
	return NewEtcdTestStorageWithCodec(t, prefix, Examplev1ProtoCodec)
}

func NewEtcdTestStorageWithCodec(t testing.TB, prefix string, codec runtime.Codec) (*etcd3testing.EtcdTestServer, storage.Interface) {
	server, _ := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	versioner := storage.APIObjectVersioner{}
	compactor := etcd3.NewCompactor(server.V3Client.Client, 0, clock.RealClock{}, nil)
	t.Cleanup(compactor.Stop)
	s, err := etcd3.New(
		server.V3Client,
		compactor,
		codec,
		NewPod,
		NewPodList,
		prefix,
		"/pods/",
		schema.GroupResource{Resource: "pods"},
		identity.NewEncryptCheckTransformer(),
		etcd3.NewDefaultLeaseManagerConfig(),
		etcd3.NewDefaultDecoder(codec, versioner),
		versioner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return server, s
}

func NewCorev1EtcdTestStorage(t testing.TB) (*etcd3testing.EtcdTestServer, storage.Interface) {
	cfg := testserver.NewTestConfig(t)
	cfg.QuotaBackendBytes = 4 << 30 // 4 GiB (default 2 GiB is too small for 150k pods)
	server := &etcd3testing.EtcdTestServer{V3Client: testserver.RunEtcd(t, cfg)}
	versioner := storage.APIObjectVersioner{}
	compactor := etcd3.NewCompactor(server.V3Client.Client, 0, clock.RealClock{}, nil)
	t.Cleanup(compactor.Stop)
	s, err := etcd3.New(
		server.V3Client,
		compactor,
		Corev1ProtoCodec,
		func() runtime.Object { return &corev1.Pod{} },
		func() runtime.Object { return &corev1.PodList{} },
		etcd3testing.PathPrefix(),
		"/pods/",
		schema.GroupResource{Resource: "pods"},
		identity.NewEncryptCheckTransformer(),
		etcd3.NewDefaultLeaseManagerConfig(),
		etcd3.NewDefaultDecoder(Corev1ProtoCodec, versioner),
		versioner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return server, s
}

// GetPodAttrs returns labels and fields of a given object for filtering purposes.
func GetPodAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod, ok := obj.(*example.Pod)
	if !ok {
		return nil, nil, fmt.Errorf("not a pod")
	}
	return labels.Set(pod.ObjectMeta.Labels), PodToSelectableFields(pod), nil
}

// PodToSelectableFields returns a field set that represents the object
// TODO: fields are not labels, and the validation rules for them do not apply.
func PodToSelectableFields(pod *example.Pod) fields.Set {
	// The purpose of allocation with a given number of elements is to reduce
	// amount of allocations needed to create the fields.Set. If you add any
	// field here or the number of object-meta related fields changes, this should
	// be adjusted.
	podSpecificFieldsSet := make(fields.Set, 5)
	podSpecificFieldsSet["spec.nodeName"] = pod.Spec.NodeName
	podSpecificFieldsSet["spec.restartPolicy"] = string(pod.Spec.RestartPolicy)
	podSpecificFieldsSet["status.phase"] = string(pod.Status.Phase)
	return AddObjectMetaFieldsSet(podSpecificFieldsSet, &pod.ObjectMeta, true)
}

func AddObjectMetaFieldsSet(source fields.Set, objectMeta *metav1.ObjectMeta, hasNamespaceField bool) fields.Set {
	source["metadata.name"] = objectMeta.Name
	if hasNamespaceField {
		source["metadata.namespace"] = objectMeta.Namespace
	}
	return source
}

func GetCorev1PodAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil, fmt.Errorf("not a pod")
	}
	fs := fields.Set{
		"metadata.name":      pod.Name,
		"metadata.namespace": pod.Namespace,
		"spec.nodeName":      pod.Spec.NodeName,
		"spec.restartPolicy": string(pod.Spec.RestartPolicy),
		"status.phase":       string(pod.Status.Phase),
	}
	return labels.Set(pod.Labels), fs, nil
}

func CheckStorageInvariants(ctx context.Context, t *testing.T, key string) {
	// No-op function since cacher simply passes object creation to the underlying storage.
}

func IncreaseRVFunc(client *clientv3.Client) storagetesting.IncreaseRVFunc {
	return func(ctx context.Context, t *testing.T) int64 {
		resp, err := client.KV.Put(ctx, "increaseRV", "ok")
		if err != nil {
			t.Fatalf("Could not update increaseRV: %v", err)
		}
		return resp.Header.Revision
	}
}
