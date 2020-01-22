/*
Copyright 2019 The Kubernetes Authors.

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

package egressselector

import (
	"fmt"
	"io/ioutil"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/apis/apiserver/install"
	"k8s.io/apiserver/pkg/apis/apiserver/v1alpha1"
	"k8s.io/utils/path"
	"sigs.k8s.io/yaml"
)

var cfgScheme = runtime.NewScheme()

func init() {
	install.Install(cfgScheme)
}

// ReadEgressSelectorConfiguration reads the egress selector configuration at the specified path.
// It returns the loaded egress selector configuration if the input file aligns with the required syntax.
// If it does not align with the provided syntax, it returns a default configuration which should function as a no-op.
// It does this by returning a nil configuration, which preserves backward compatibility.
// This works because prior to this there was no egress selector configuration.
// It returns an error if the file did not exist.
func ReadEgressSelectorConfiguration(configFilePath string) (*apiserver.EgressSelectorConfiguration, error) {
	if configFilePath == "" {
		return nil, nil
	}
	// a file was provided, so we just read it.
	data, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to read egress selector configuration from %q [%v]", configFilePath, err)
	}
	var decodedConfig v1alpha1.EgressSelectorConfiguration
	err = yaml.Unmarshal(data, &decodedConfig)
	if err != nil {
		// we got an error where the decode wasn't related to a missing type
		return nil, err
	}
	if decodedConfig.Kind != "EgressSelectorConfiguration" {
		return nil, fmt.Errorf("invalid service configuration object %q", decodedConfig.Kind)
	}
	internalConfig := &apiserver.EgressSelectorConfiguration{}
	if err := cfgScheme.Convert(&decodedConfig, internalConfig, nil); err != nil {
		// we got an error where the decode wasn't related to a missing type
		return nil, err
	}
	return internalConfig, nil
}

// ValidateEgressSelectorConfiguration checks the apiserver.EgressSelectorConfiguration for
// common configuration errors. It will return error for problems such as configuring mtls/cert
// settings for protocol which do not support security. It will also try to catch errors such as
// incorrect file paths. It will return nil if it does not find anything wrong.
func ValidateEgressSelectorConfiguration(config *apiserver.EgressSelectorConfiguration) field.ErrorList {
	allErrs := field.ErrorList{}
	if config == nil {
		return allErrs // Treating a nil configuration as valid
	}
	for _, service := range config.EgressSelections {
		base := field.NewPath("service", "connection")
		switch service.Connection.Protocol {
		case "direct":
			allErrs = append(allErrs, validateDirectConnection(service.Connection, base)...)
		case "http-connect":
			switch service.Connection.Transport {
			case "uds":
				allErrs = append(allErrs, validateUDSConnection(service.Connection, base)...)
			case "tcp":
				allErrs = append(allErrs, validateHTTPConnection(service.Connection, base)...)
			default:
				allErrs = append(allErrs, field.NotSupported(
					base.Child("protocol"),
					service.Connection.Protocol,
					[]string{"uds", "tcp"}))
			}
		case "grpc":
			switch service.Connection.Transport {
			case "uds":
				allErrs = append(allErrs, validateUDSConnection(service.Connection, base)...)
			default:
				allErrs = append(allErrs, field.NotSupported(
					base.Child("protocol"),
					service.Connection.Protocol,
					[]string{"uds"}))
			}
		default:
			allErrs = append(allErrs, field.NotSupported(
				base.Child("protocol"),
				service.Connection.Protocol,
				[]string{"direct", "http-connect", "grpc"}))
		}
	}

	return allErrs
}

func validateDirectConnection(connection apiserver.Connection, fldPath *field.Path) field.ErrorList {
	if connection.TLSConfig != nil {
		return field.ErrorList{field.Invalid(
			fldPath.Child("tlsConfig"),
			"direct",
			"TLSConfig config should be absent for direct connect"),
		}
	}
	return nil
}

func validateUDSConnection(connection apiserver.Connection, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if connection.TLSConfig != nil {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig"),
			"direct",
			"tlsConfig config should be absent for connect via UDS"))
	}
	if connection.UDSName == "" {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("udsName"),
			"nil",
			"UDSName should be present for grpc uds or http-connect uds"))
	}
	return allErrs
}

func validateHTTPConnection(connection apiserver.Connection, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if connection.TLSConfig == nil {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("tlsConfig"),
			"nil",
			"TLSConfig config should be present for http-connect via tcp"))
	} else if strings.HasPrefix(connection.URL, "https://") {
		if connection.TLSConfig.CABundle == "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "caBundle"),
				"nil",
				"http-connect via https requires caBundle"))
		} else if exists, err := path.Exists(path.CheckFollowSymlink, connection.TLSConfig.CABundle); exists == false || err != nil {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "caBundle"),
				connection.TLSConfig.CABundle,
				"http-connect ca bundle does not exist"))
		}
		if connection.TLSConfig.ClientCert == "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientCert"),
				"nil",
				"http-connect via https requires clientCert"))
		} else if exists, err := path.Exists(path.CheckFollowSymlink, connection.TLSConfig.ClientCert); exists == false || err != nil {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientCert"),
				connection.TLSConfig.ClientCert,
				"http-connect client cert does not exist"))
		}
		if connection.TLSConfig.ClientKey == "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientKey"),
				"nil",
				"http-connect via https requires clientKey"))
		} else if exists, err := path.Exists(path.CheckFollowSymlink, connection.TLSConfig.ClientKey); exists == false || err != nil {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientKey"),
				connection.TLSConfig.ClientKey,
				"http-connect client key does not exist"))
		}
	} else if strings.HasPrefix(connection.URL, "http://") {
		if connection.TLSConfig.CABundle != "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "caBundle"),
				connection.TLSConfig.CABundle,
				"http-connect via http does not support caBundle"))
		}
		if connection.TLSConfig.ClientCert != "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientCert"),
				connection.TLSConfig.ClientCert,
				"http-connect via http does not support clientCert"))
		}
		if connection.TLSConfig.ClientKey != "" {
			allErrs = append(allErrs, field.Invalid(
				fldPath.Child("tlsConfig", "clientKey"),
				connection.TLSConfig.ClientKey,
				"http-connect via http does not support clientKey"))
		}
	} else {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("url"),
			connection.URL,
			"supported connection protocols are http:// and https://"))
	}
	return allErrs
}
