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

// Package inline implements the inline JSON patch generator.
package inline

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"text/template"

	sprig "github.com/Masterminds/sprig/v3"
	"github.com/pkg/errors"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	runtimehooksv1 "sigs.k8s.io/cluster-api/exp/runtime/hooks/api/v1alpha1"
	"sigs.k8s.io/cluster-api/internal/contract"
	"sigs.k8s.io/cluster-api/internal/controllers/topology/cluster/patches/api"
	patchvariables "sigs.k8s.io/cluster-api/internal/controllers/topology/cluster/patches/variables"
)

// jsonPatchGenerator generates JSON patches for a GeneratePatchesRequest based on a ClusterClassPatch.
type jsonPatchGenerator struct {
	patch *clusterv1.ClusterClassPatch
}

// New returns a new inline Generator from a given ClusterClassPatch object.
func New(patch *clusterv1.ClusterClassPatch) api.Generator {
	return &jsonPatchGenerator{
		patch: patch,
	}
}

// Generate generates JSON patches for the given GeneratePatchesRequest based on a ClusterClassPatch.
func (j *jsonPatchGenerator) Generate(_ context.Context, req *runtimehooksv1.GeneratePatchesRequest) *runtimehooksv1.GeneratePatchesResponse {
	resp := &runtimehooksv1.GeneratePatchesResponse{}

	globalVariables := toMap(req.Variables)

	// Loop over all templates.
	errs := []error{}
	for i := range req.Items {
		item := &req.Items[i]

		templateVariables := toMap(item.Variables)

		// Calculate the list of patches which match the current template.
		matchingPatches := []clusterv1.PatchDefinition{}
		for _, patch := range j.patch.Definitions {
			// Add the patch to the list, if it matches the template.
			if matchesSelector(item, templateVariables, patch.Selector) {
				matchingPatches = append(matchingPatches, patch)
			}
		}

		// Continue if there are no matching patches.
		if len(matchingPatches) == 0 {
			continue
		}

		// Merge template-specific and global variables.
		variables, err := mergeVariableMaps(globalVariables, templateVariables)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to merge global and template-specific variables for item with uid %q", item.UID))
			continue
		}

		enabled, err := patchIsEnabled(j.patch.EnabledIf, variables)
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to calculate if patch %s is enabled for item with uid %q", j.patch.Name, item.UID))
			continue
		}
		if !enabled {
			// Continue if patch is not enabled.
			continue
		}

		// Loop over all PatchDefinitions.
		for _, patch := range matchingPatches {
			// Generate JSON patches.
			jsonPatches, err := generateJSONPatches(patch.JSONPatches, variables)
			if err != nil {
				errs = append(errs, errors.Wrapf(err, "failed to generate JSON patches for item with uid %q", item.UID))
				continue
			}

			// Add jsonPatches to the response.
			resp.Items = append(resp.Items, runtimehooksv1.GeneratePatchesResponseItem{
				UID:       item.UID,
				Patch:     jsonPatches,
				PatchType: runtimehooksv1.JSONPatchType,
			})
		}
	}

	if err := kerrors.NewAggregate(errs); err != nil {
		return &runtimehooksv1.GeneratePatchesResponse{
			CommonResponse: runtimehooksv1.CommonResponse{
				Status:  runtimehooksv1.ResponseStatusFailure,
				Message: err.Error(),
			},
		}
	}

	return resp
}

// toMap converts a list of Variables to a map of JSON (name is the map key).
func toMap(variables []runtimehooksv1.Variable) map[string]apiextensionsv1.JSON {
	variablesMap := map[string]apiextensionsv1.JSON{}
	for i := range variables {
		variablesMap[variables[i].Name] = variables[i].Value
	}
	return variablesMap
}

// matchesSelector returns true if the GeneratePatchesRequestItem matches the selector.
func matchesSelector(req *runtimehooksv1.GeneratePatchesRequestItem, templateVariables map[string]apiextensionsv1.JSON, selector clusterv1.PatchSelector) bool {
	gvk := req.Object.Object.GetObjectKind().GroupVersionKind()

	// Check if the apiVersion and kind are matching.
	if gvk.GroupVersion().String() != selector.APIVersion {
		return false
	}
	if gvk.Kind != selector.Kind {
		return false
	}

	// Check if the request is for an InfrastructureCluster.
	if selector.MatchResources.InfrastructureCluster {
		// Cluster.spec.infrastructureRef holds the InfrastructureCluster.
		if req.HolderReference.Kind == "Cluster" && req.HolderReference.FieldPath == "spec.infrastructureRef" {
			return true
		}
	}

	// Check if the request is for a ControlPlane or the InfrastructureMachineTemplate of a ControlPlane.
	if selector.MatchResources.ControlPlane {
		// Cluster.spec.controlPlaneRef holds the ControlPlane.
		if req.HolderReference.Kind == "Cluster" && req.HolderReference.FieldPath == "spec.controlPlaneRef" {
			return true
		}
		// *.spec.machineTemplate.infrastructureRef holds the InfrastructureMachineTemplate of a ControlPlane.
		// Note: this field path is only used in this context.
		if req.HolderReference.FieldPath == strings.Join(contract.ControlPlane().MachineTemplate().InfrastructureRef().Path(), ".") {
			return true
		}
	}

	// Check if the request is for a BootstrapConfigTemplate or an InfrastructureMachineTemplate
	// of one of the configured MachineDeploymentClasses.
	if selector.MatchResources.MachineDeploymentClass != nil {
		// MachineDeployment.spec.template.spec.bootstrap.configRef or
		// MachineDeployment.spec.template.spec.infrastructureRef holds the BootstrapConfigTemplate or
		// InfrastructureMachineTemplate.
		if req.HolderReference.Kind == "MachineDeployment" &&
			(req.HolderReference.FieldPath == "spec.template.spec.bootstrap.configRef" ||
				req.HolderReference.FieldPath == "spec.template.spec.infrastructureRef") {
			// Read the builtin.machineDeployment.class variable.
			templateMDClassJSON, err := patchvariables.GetVariableValue(templateVariables, "builtin.machineDeployment.class")

			// If the builtin variable could be read.
			if err == nil {
				// If templateMDClass matches one of the configured MachineDeploymentClasses.
				for _, mdClass := range selector.MatchResources.MachineDeploymentClass.Names {
					// We have to quote mdClass as templateMDClassJSON is a JSON string (e.g. "default-worker").
					if string(templateMDClassJSON.Raw) == strconv.Quote(mdClass) {
						return true
					}
				}
			}
		}
	}

	return false
}

func patchIsEnabled(enabledIf *string, variables map[string]apiextensionsv1.JSON) (bool, error) {
	// If enabledIf is not set, patch is enabled.
	if enabledIf == nil {
		return true, nil
	}

	// Rendered template.
	value, err := renderValueTemplate(*enabledIf, variables)
	if err != nil {
		return false, errors.Wrapf(err, "failed to calculate value for enabledIf")
	}

	// Patch is enabled if the rendered template value is `true`.
	return bytes.Equal(value.Raw, []byte(`true`)), nil
}

// jsonPatchRFC6902 is used to render the generated JSONPatches.
type jsonPatchRFC6902 struct {
	Op    string                `json:"op"`
	Path  string                `json:"path"`
	Value *apiextensionsv1.JSON `json:"value,omitempty"`
}

// generateJSONPatches generates JSON patches based on the given JSONPatches and variables.
func generateJSONPatches(jsonPatches []clusterv1.JSONPatch, variables map[string]apiextensionsv1.JSON) ([]byte, error) {
	res := []jsonPatchRFC6902{}

	for _, jsonPatch := range jsonPatches {
		var value *apiextensionsv1.JSON
		if jsonPatch.Op == "add" || jsonPatch.Op == "replace" {
			var err error
			value, err = calculateValue(jsonPatch, variables)
			if err != nil {
				return nil, err
			}
		}

		res = append(res, jsonPatchRFC6902{
			Op:    jsonPatch.Op,
			Path:  jsonPatch.Path,
			Value: value,
		})
	}

	// Render JSON Patches.
	resJSON, err := json.Marshal(res)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal JSON Patch %v", jsonPatches)
	}

	return resJSON, nil
}

// calculateValue calculates a value for a JSON patch.
func calculateValue(patch clusterv1.JSONPatch, variables map[string]apiextensionsv1.JSON) (*apiextensionsv1.JSON, error) {
	// Return if values are set incorrectly.
	if patch.Value == nil && patch.ValueFrom == nil {
		return nil, errors.Errorf("failed to calculate value: neither .value nor .valueFrom are set")
	}
	if patch.Value != nil && patch.ValueFrom != nil {
		return nil, errors.Errorf("failed to calculate value: both .value and .valueFrom are set")
	}
	if patch.ValueFrom != nil && patch.ValueFrom.Variable == nil && patch.ValueFrom.Template == nil {
		return nil, errors.Errorf("failed to calculate value: .valueFrom is set, but neither .valueFrom.variable nor .valueFrom.template are set")
	}
	if patch.ValueFrom != nil && patch.ValueFrom.Variable != nil && patch.ValueFrom.Template != nil {
		return nil, errors.Errorf("failed to calculate value: .valueFrom is set, but both .valueFrom.variable and .valueFrom.template are set")
	}

	// Return raw value.
	if patch.Value != nil {
		return patch.Value, nil
	}

	// Return variable.
	if patch.ValueFrom.Variable != nil {
		value, err := patchvariables.GetVariableValue(variables, *patch.ValueFrom.Variable)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to calculate value")
		}
		return value, nil
	}

	// Return rendered value template.
	value, err := renderValueTemplate(*patch.ValueFrom.Template, variables)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to calculate value for template")
	}
	return value, nil
}

// renderValueTemplate renders a template with the given variables as data.
func renderValueTemplate(valueTemplate string, variables map[string]apiextensionsv1.JSON) (*apiextensionsv1.JSON, error) {
	// Parse the template.
	tpl, err := template.New("tpl").Funcs(sprig.HermeticTxtFuncMap()).Parse(valueTemplate)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse template: %q", valueTemplate)
	}

	// Convert the flat variables map in a nested map, so that variables can be
	// consumed in templates like this: `{{ .builtin.cluster.name }}`
	// NOTE: Variable values are also converted to their Go types as
	// they cannot be directly consumed as byte arrays.
	data, err := calculateTemplateData(variables)
	if err != nil {
		return nil, errors.Wrap(err, "failed to calculate template data")
	}

	// Render the template.
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, errors.Wrapf(err, "failed to render template: %q", valueTemplate)
	}

	// Unmarshal the rendered template.
	// NOTE: The YAML library is used for unmarshalling, to be able to handle YAML and JSON.
	value := apiextensionsv1.JSON{}
	if err := yaml.Unmarshal(buf.Bytes(), &value); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal rendered template: %q", buf.String())
	}

	return &value, nil
}

// calculateTemplateData calculates data for the template, by converting
// the variables to their Go types.
// Example:
// * Input:
//   map[string]apiextensionsv1.JSON{
//     "builtin": {Raw: []byte(`{"cluster":{"name":"cluster-name"}}`},
//     "integerVariable": {Raw: []byte("4")},
//     "numberVariable": {Raw: []byte("2.5")},
//     "booleanVariable": {Raw: []byte("true")},
//   }
// * Output:
//   map[string]interface{}{
//     "builtin": map[string]interface{}{
//       "cluster": map[string]interface{}{
//         "name": <string>"cluster-name"
//       }
//     },
//     "integerVariable": <float64>4,
//     "numberVariable": <float64>2.5,
//     "booleanVariable": <bool>true,
//   }
func calculateTemplateData(variables map[string]apiextensionsv1.JSON) (map[string]interface{}, error) {
	res := make(map[string]interface{}, len(variables))

	// Marshal the variables into a byte array.
	tmp, err := json.Marshal(variables)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to convert variables: failed to marshal variables")
	}

	// Unmarshal the byte array back.
	// NOTE: This converts the "leaf nodes" of the nested map
	// from apiextensionsv1.JSON to their Go types.
	if err := json.Unmarshal(tmp, &res); err != nil {
		return nil, errors.Wrapf(err, "failed to convert variables: failed to unmarshal variables")
	}

	return res, nil
}

// mergeVariableMaps merges variables.
// NOTE: In case a variable exists in multiple maps, the variable from the latter map is preserved.
// NOTE: The builtin variable object is merged instead of simply overwritten.
func mergeVariableMaps(variableMaps ...map[string]apiextensionsv1.JSON) (map[string]apiextensionsv1.JSON, error) {
	res := make(map[string]apiextensionsv1.JSON)

	for _, variableMap := range variableMaps {
		for variableName, variableValue := range variableMap {
			// If the variable already exits and is the builtin variable, merge it.
			if _, ok := res[variableName]; ok && variableName == patchvariables.BuiltinsName {
				mergedV, err := mergeBuiltinVariables(res[variableName], variableValue)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to merge builtin variables")
				}
				res[variableName] = *mergedV
				continue
			}
			res[variableName] = variableValue
		}
	}

	return res, nil
}

// mergeBuiltinVariables merges builtin variable objects.
// NOTE: In case a variable exists in multiple builtin variables, the variable from the latter map is preserved.
func mergeBuiltinVariables(variableList ...apiextensionsv1.JSON) (*apiextensionsv1.JSON, error) {
	builtins := &patchvariables.Builtins{}

	// Unmarshal all variables into builtins.
	// NOTE: This accumulates the fields on the builtins.
	// Fields will be overwritten by later Unmarshals if fields are
	// set on multiple variables.
	for _, variable := range variableList {
		if err := json.Unmarshal(variable.Raw, builtins); err != nil {
			return nil, errors.Wrapf(err, "failed to unmarshal builtin variable")
		}
	}

	// Marshal builtins to JSON.
	builtinVariableJSON, err := json.Marshal(builtins)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal builtin variable")
	}

	return &apiextensionsv1.JSON{
		Raw: builtinVariableJSON,
	}, nil
}
