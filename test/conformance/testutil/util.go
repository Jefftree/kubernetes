package testutil

import (
	"fmt"
	"io/ioutil"
	"gopkg.in/yaml.v2"

	"k8s.io/kubernetes/test/conformance/behaviors"
)

func LoadBehaviorsMapping(root string) (map[string]behaviors.ConformanceData, error) {
	behaviorsLinkFile := fmt.Sprintf("%s/test/conformance/kubeconform/link.yaml", root)
	conformanceData := make(map[string]behaviors.ConformanceData)
	yamlFile, err := ioutil.ReadFile(behaviorsLinkFile)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(yamlFile, &conformanceData)
	if err != nil {
		return nil, err
	}
	return conformanceData, nil
}
