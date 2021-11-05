// This is a generated file. Do not edit directly.

module k8s.io/component-base

go 1.16

require (
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/blang/semver v3.5.1+incompatible
	github.com/go-logr/logr v1.2.0
	github.com/go-logr/zapr v1.2.0
	github.com/google/go-cmp v0.5.5
	github.com/matttproud/golang_protobuf_extensions v1.0.2-0.20181231171920-c182affec369 // indirect
	github.com/moby/term v0.0.0-20210610120745-9d4ed1856297
	github.com/prometheus/client_golang v1.11.0
	github.com/prometheus/client_model v0.2.0
	github.com/prometheus/common v0.28.0
	github.com/prometheus/procfs v0.6.0
	github.com/spf13/cobra v1.2.1
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.7.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.20.0
	go.opentelemetry.io/otel v0.20.0
	go.opentelemetry.io/otel/exporters/otlp v0.20.0
	go.opentelemetry.io/otel/sdk v0.20.0
	go.opentelemetry.io/otel/trace v0.20.0
	go.uber.org/zap v1.19.0
	golang.org/x/sys v0.0.0-20210831042530-f4d43177bf5e
	golang.org/x/tools v0.1.6-0.20210820212750-d4cc65f0b2ff // indirect
	gotest.tools/v3 v3.0.3 // indirect
	k8s.io/apimachinery v0.0.0
	k8s.io/client-go v0.0.0
	k8s.io/klog/v2 v2.30.0
	k8s.io/utils v0.0.0-20210930125809-cb0fa318a74b
)

replace (
	github.com/golang/glog => github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b
	golang.org/x/net => golang.org/x/net v0.0.0-20210813160813-60bc85c4be6d
	golang.org/x/sys => golang.org/x/sys v0.0.0-20210820121016-41cdb8703e55
	golang.org/x/text => golang.org/x/text v0.3.6
	google.golang.org/genproto => google.golang.org/genproto v0.0.0-20210602131652-f16073e35f0c
	google.golang.org/grpc => google.golang.org/grpc v1.38.0
	google.golang.org/protobuf => google.golang.org/protobuf v1.26.0
	k8s.io/api => ../api
	k8s.io/apimachinery => ../apimachinery
	k8s.io/client-go => ../client-go
	k8s.io/component-base => ../component-base
	k8s.io/kube-openapi => github.com/jefftree/kube-openapi v0.0.1
)
