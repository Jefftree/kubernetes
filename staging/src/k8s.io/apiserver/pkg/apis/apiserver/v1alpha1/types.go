/*
Copyright 2017 The Kubernetes Authors.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// AdmissionConfiguration provides versioned configuration for admission controllers.
type AdmissionConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// Plugins allows specifying a configuration per admission control plugin.
	// +optional
	Plugins []AdmissionPluginConfiguration `json:"plugins"`
}

// AdmissionPluginConfiguration provides the configuration for a single plug-in.
type AdmissionPluginConfiguration struct {
	// Name is the name of the admission controller.
	// It must match the registered admission plugin name.
	Name string `json:"name"`

	// Path is the path to a configuration file that contains the plugin's
	// configuration
	// +optional
	Path string `json:"path"`

	// Configuration is an embedded configuration object to be used as the plugin's
	// configuration. If present, it will be used instead of the path to the configuration file.
	// +optional
	Configuration *runtime.Unknown `json:"configuration"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EgressSelectorConfiguration provides versioned configuration for egress selector clients.
type EgressSelectorConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// connectionServices contains a list of egress selection client configurations
	EgressSelections []EgressSelection `json:"egressSelections"`
}

// EgressSelection provides the configuration for a single egress selection client.
type EgressSelection struct {
	// name is the name of the egress selection.
	// Currently supported values are "Master", "Etcd" and "Cluster"
	Name string `json:"name"`

	// connection is the exact information used to configure the egress selection
	Connection Connection `json:"connection"`
}

// Connection provides the configuration for a single egress selection client.
type Connection struct {
	// Protocol is the protocol used to connect from client to the konnectivity server.
	// Supported values are "http-connect," "grpc," and "direct"
	Protocol string `json:"protocol"`

	// Transport is the transport socket used to connect to konnectivity server.
	// Supported values are "uds" and "tcp." TCP will only work with http-connect protocol
	// Optional only for "direct" protocol
	// +optional
	Transport string `json:"transport"`

	// url is the location of the proxy server to connect to.
	// As an example it might be "https://127.0.0.1:8131"
	// +optional
	URL string `json:"url"`

	// UDSName is the name of the unix domain socket to connect to konnectivity server.
	// Only required if communication with konnectivity server is via UDS.
	// +optional
	UDSName string `json:"udsName,omitempty"`

	// TLSConfig is the config needed to use TLS when connecting to konnectivity server
	// Absence when the type is "http-connect" will cause an error
	// Presence when the type is "direct" will also cause an error
	// +optional
	TLSConfig *TLSConfig `json:"tlsConfig,omitempty"`
}

type TLSConfig struct {
	// caBundle is the file location of the CA to be used to determine trust with the konnectivity server.
	// Must be absent/empty http-connect using the plain http
	// Must be configured for http-connect using the https protocol
	// Misconfiguration will cause an error
	// +optional
	CABundle string `json:"caBundle,omitempty"`

	// clientKey is the file location of the client key to be used in mtls handshakes with the konnectivity server.
	// Must be absent/empty http-connect using the plain http
	// Must be configured for http-connect using the https protocol
	// Misconfiguration will cause an error
	// +optional
	ClientKey string `json:"clientKey,omitempty"`

	// clientCert is the file location of the client certificate to be used in mtls handshakes with the konnectivity server.
	// Must be absent/empty http-connect using the plain http
	// Must be configured for http-connect using the https protocol
	// Misconfiguration will cause an error
	// +optional
	ClientCert string `json:"clientCert,omitempty"`
}
