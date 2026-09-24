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

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel/attribute"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	kjson "sigs.k8s.io/json"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	cbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor/direct"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	"k8s.io/apiserver/pkg/endpoints/handlers/finisher"
	requestmetrics "k8s.io/apiserver/pkg/endpoints/handlers/metrics"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/tracing"
)

const (
	// maximum number of operations a single json patch may contain.
	maxJSONPatchOperations = 10000
)

type patchIfaceWords struct {
	typ  uintptr
	data uintptr
}

func patchIfaceKey(v any) patchIfaceWords {
	return *(*patchIfaceWords)(unsafe.Pointer(&v))
}

type patchHandlerCacheEntry struct {
	rKey       patchIfaceWords
	scope      *RequestScope
	admitKey   patchIfaceWords
	r          rest.Patcher
	admit      admission.Interface
	patchTypes []string
	handler    http.HandlerFunc
}

var (
	patchHandlerCacheMu   sync.RWMutex
	patchHandlerCache     [64]patchHandlerCacheEntry
	patchHandlerCacheNext uint32
)

// PatchResource returns a function that will handle a resource patch.
func PatchResource(r rest.Patcher, scope *RequestScope, admit admission.Interface, patchTypes []string) http.HandlerFunc {
	rKey := patchIfaceKey(r)
	admitKey := patchIfaceKey(admit)
	patchHandlerCacheMu.RLock()
	for i := range patchHandlerCache {
		e := &patchHandlerCache[i]
		if e.handler != nil && e.scope == scope && e.rKey == rKey && e.admitKey == admitKey && len(e.patchTypes) == len(patchTypes) {
			match := true
			for j := range patchTypes {
				if e.patchTypes[j] != patchTypes[j] {
					match = false
					break
				}
			}
			if match {
				h := e.handler
				patchHandlerCacheMu.RUnlock()
				return h
			}
		}
	}
	patchHandlerCacheMu.RUnlock()

	patchTypeSet := sets.NewString(patchTypes...)
	admit = fieldmanager.NewManagedFieldsValidatingAdmissionController(admission.WithAudit(admit))
	mutatingAdmission, _ := admit.(admission.MutationInterface)
	hasUpdateValidation := admission.HasValidationHandler(admit, admission.Update)
	hasCreateValidation := admission.HasValidationHandler(admit, admission.Create)

	var codecCacheMu sync.Mutex
	var jsonStrictCodec, jsonNonStrictCodec, yamlStrictCodec, yamlNonStrictCodec runtime.Codec
	getCachedCodec := func(baseContentType string, strict bool) (runtime.Codec, bool) {
		codecCacheMu.Lock()
		defer codecCacheMu.Unlock()
		switch baseContentType {
		case runtime.ContentTypeJSON:
			if strict && jsonStrictCodec != nil {
				return jsonStrictCodec, true
			}
			if !strict && jsonNonStrictCodec != nil {
				return jsonNonStrictCodec, true
			}
		case runtime.ContentTypeYAML:
			if strict && yamlStrictCodec != nil {
				return yamlStrictCodec, true
			}
			if !strict && yamlNonStrictCodec != nil {
				return yamlNonStrictCodec, true
			}
		}
		s, ok := runtime.SerializerInfoForMediaType(scope.Serializer.SupportedMediaTypes(), baseContentType)
		if !ok {
			return nil, false
		}
		decodeSerializer := s.Serializer
		if strict {
			decodeSerializer = s.StrictSerializer
		}
		c := runtime.NewCodec(
			scope.Serializer.EncoderForVersion(s.Serializer, scope.Kind.GroupVersion()),
			scope.Serializer.DecoderToVersion(decodeSerializer, scope.HubGroupVersion),
		)
		switch baseContentType {
		case runtime.ContentTypeJSON:
			if strict {
				jsonStrictCodec = c
			} else {
				jsonNonStrictCodec = c
			}
		case runtime.ContentTypeYAML:
			if strict {
				yamlStrictCodec = c
			} else {
				yamlNonStrictCodec = c
			}
		}
		return c, true
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		// For performance tracking purposes.
		var span *tracing.Span
		if tracing.IsEnabled(ctx) {
			ctx, span = tracing.Start(ctx, "Patch", traceFields(req)...)
			req = req.WithContext(ctx)
			defer span.End(500 * time.Millisecond)
		} else {
			_, span = tracing.Start(ctx, "Patch")
		}

		// Do this first, otherwise name extraction can fail for unrecognized content types
		// TODO: handle this in negotiation
		contentType := req.Header.Get("Content-Type")
		// Remove "; charset=" if included in header.
		if idx := strings.Index(contentType, ";"); idx > 0 {
			contentType = contentType[:idx]
		}
		patchType := types.PatchType(contentType)

		// Ensure the patchType is one we support
		if !patchTypeSet.Has(contentType) {
			scope.err(negotiation.NewUnsupportedMediaTypeError(patchTypes), w, req)
			return
		}

		namespace, name, err := scope.Namer.Name(req)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		// enforce a timeout of at most requestTimeoutUpperBound (34s) or less if the user-provided
		// timeout inside the parent context is lower than requestTimeoutUpperBound.
		ctx, cancel := context.WithTimeout(ctx, requestTimeoutUpperBound)
		defer cancel()

		if request.NamespaceValue(ctx) != namespace {
			ctx = request.WithNamespace(ctx, namespace)
		}

		outputMediaType, _, err := negotiation.NegotiateOutputMediaType(req, scope.Serializer, scope)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		patchBytes, err := limitedReadBodyWithRecordMetric(ctx, req, scope.MaxRequestBodyBytes, scope.Resource.GroupResource(), requestmetrics.Patch)
		if err != nil {
			span.AddEvent("limitedReadBody failed", attribute.Int("len", len(patchBytes)), attribute.String("err", err.Error()))
			scope.err(err, w, req)
			return
		}
		if tracing.IsEnabled(ctx) {
			span.AddEvent("limitedReadBody succeeded", attribute.Int("len", len(patchBytes)))
		}

		p := patcherPool.Get().(*patcher)
		p.namer = scope.Namer
		p.creater = scope.Creater
		p.defaulter = scope.Defaulter
		p.typer = scope.Typer
		p.unsafeConvertor = scope.UnsafeConvertor
		p.kind = scope.Kind
		p.resource = scope.Resource
		p.subresource = scope.Subresource
		p.objectInterfaces = scope
		p.hubGroupVersion = scope.HubGroupVersion
		p.admissionCheck = mutatingAdmission
		p.restPatcher = r
		p.name = name
		p.patchType = patchType
		p.patchBytes = patchBytes
		p.userAgent = req.UserAgent()
		p.stripManagedFieldsOnRetry = false
		p.forceAllowCreate = false
		p.optionsVal = metav1.PatchOptions{}
		options := &p.optionsVal
		rawQuery := req.URL.RawQuery
		if len(rawQuery) > 0 {
			if strings.HasPrefix(rawQuery, "fieldManager=") && !strings.ContainsAny(rawQuery[len("fieldManager="):], "&+%") {
				options.FieldManager = rawQuery[len("fieldManager="):]
			} else if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), scope.MetaGroupVersion, options); err != nil {
				err = errors.NewBadRequest(err.Error())
				scope.err(err, w, req)
				return
			}
		}
		if errs := validation.ValidatePatchOptions(options, patchType); len(errs) > 0 {
			err := errors.NewInvalid(schema.GroupKind{Group: metav1.GroupName, Kind: "PatchOptions"}, "", errs)
			scope.err(err, w, req)
			return
		}
		options.TypeMeta = metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "PatchOptions",
		}

		// Decode and convert the patch payload to JSON for storage into audit.
		// the decision to reject the request for failure to decode before we record the request for audit.
		//
		// This guarantees that the audit event contains valid JSON, or the request is rejected.
		patchForAudit, err := validateAndTranscodePatch(patchBytes, patchType)
		if err != nil {
			scope.err(err, w, req)
			return
		}

		audit.LogRequestPatch(req.Context(), patchForAudit)
		if tracing.IsEnabled(ctx) {
			span.AddEvent("Recorded the audit event")
		}

		var baseContentType string
		switch patchType {
		case types.ApplyYAMLPatchType:
			baseContentType = runtime.ContentTypeYAML
		case types.ApplyCBORPatchType:
			if !utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
				// This request should have already been rejected by the
				// Content-Type allowlist check. Return 500 because assumptions are
				// already broken and the feature is not GA.
				utilruntime.HandleErrorWithContext(req.Context(), nil, "The patch content-type allowlist check should have made this unreachable.")
				scope.err(errors.NewInternalError(errors.NewInternalError(fmt.Errorf("unexpected patch type: %v", patchType))), w, req)
				return
			}

			baseContentType = runtime.ContentTypeCBOR
		default:
			baseContentType = runtime.ContentTypeJSON
		}

		validationDirective := fieldValidation(options.FieldValidation)
		useStrict := validationDirective == metav1.FieldValidationWarn || validationDirective == metav1.FieldValidationStrict
		codec, ok := getCachedCodec(baseContentType, useStrict)
		if !ok {
			scope.err(fmt.Errorf("no serializer defined for %v", baseContentType), w, req)
			return
		}

		userInfo, _ := request.UserFrom(ctx)
		isDryRun := dryrun.IsDryRun(options.DryRun)

		var updateValidation rest.ValidateObjectUpdateFunc
		if hasUpdateValidation {
			staticUpdateAttributes := admission.NewAttributesRecord(
				nil,
				nil,
				scope.Kind,
				namespace,
				name,
				scope.Resource,
				scope.Subresource,
				admission.Update,
				patchToUpdateOptions(options),
				isDryRun,
				userInfo,
			)
			updateValidation = rest.AdmissionToValidateObjectUpdateFunc(admit, staticUpdateAttributes, scope)
		}

		var createValidation rest.ValidateObjectFunc
		if patchType != types.ApplyYAMLPatchType && patchType != types.ApplyCBORPatchType {
			createValidation = rest.ValidateAllObjectFunc
		} else {
			createAuthorizerAttributes := authorizer.AttributesRecord{
				User:            userInfo,
				ResourceRequest: true,
				Path:            req.URL.Path,
				Verb:            "create",
				APIGroup:        scope.Resource.Group,
				APIVersion:      scope.Resource.Version,
				Resource:        scope.Resource.Resource,
				Subresource:     scope.Subresource,
				Namespace:       namespace,
				Name:            name,
			}
			var staticCreateAttributes admission.Attributes
			if hasCreateValidation {
				staticCreateAttributes = admission.NewAttributesRecord(
					nil,
					nil,
					scope.Kind,
					namespace,
					name,
					scope.Resource,
					scope.Subresource,
					admission.Create,
					patchToCreateOptions(options),
					isDryRun,
					userInfo)
			}
			createValidation = withAuthorization(rest.AdmissionToValidateObjectFunc(admit, staticCreateAttributes, scope), scope.Authorizer, createAuthorizerAttributes)
		}

		p.dryRun = isDryRun
		p.validationDirective = validationDirective
		p.createValidation = createValidation
		p.updateValidation = updateValidation
		p.codec = codec
		p.options = options

		result, wasCreated, err := p.patchResource(ctx, scope)
		if !errors.IsTimeout(err) {
			p.patchBytes = nil
			p.requestCtx = nil
			p.mechanism = nil
			p.smp.schemaReferenceObj = nil
			p.smp.fieldManager = nil
			p.jp.fieldManager = nil
			patcherPool.Put(p)
		}
		if err != nil {
			scope.err(err, w, req)
			return
		}
		if tracing.IsEnabled(ctx) {
			span.AddEvent("Object stored in database")
		}

		status := http.StatusOK
		if wasCreated {
			status = http.StatusCreated
		}

		if tracing.IsEnabled(ctx) {
			span.AddEvent("About to write a response")
			defer span.AddEvent("Writing http response done")
		}
		transformResponseObject(ctx, scope, req, w, status, outputMediaType, result)
	})
	patchHandlerCacheMu.Lock()
	idx := patchHandlerCacheNext & uint32(len(patchHandlerCache)-1)
	patchHandlerCacheNext++
	patchHandlerCache[idx] = patchHandlerCacheEntry{
		rKey:       rKey,
		scope:      scope,
		admitKey:   admitKey,
		r:          r,
		admit:      admit,
		patchTypes: patchTypes,
		handler:    handler,
	}
	patchHandlerCacheMu.Unlock()
	return handler
}

type mutateObjectUpdateFunc func(ctx context.Context, obj, old runtime.Object) error

// patcher breaks the process of patch application and retries into smaller
// pieces of functionality.
// TODO: Use builder pattern to construct this object?
// TODO: As part of that effort, some aspects of PatchResource above could be
// moved into this type.
type patcher struct {
	// Pieces of RequestScope
	namer               ScopeNamer
	creater             runtime.ObjectCreater
	defaulter           runtime.ObjectDefaulter
	typer               runtime.ObjectTyper
	unsafeConvertor     runtime.ObjectConvertor
	resource            schema.GroupVersionResource
	kind                schema.GroupVersionKind
	subresource         string
	dryRun              bool
	validationDirective string

	objectInterfaces admission.ObjectInterfaces

	hubGroupVersion schema.GroupVersion

	// Validation functions
	createValidation rest.ValidateObjectFunc
	updateValidation rest.ValidateObjectUpdateFunc
	admissionCheck   admission.MutationInterface

	codec runtime.Codec

	options          *metav1.PatchOptions
	optionsVal       metav1.PatchOptions
	updateOptionsVal metav1.UpdateOptions

	// Operation information
	restPatcher rest.Patcher
	name        string
	patchType   types.PatchType
	patchBytes  []byte
	userAgent   string

	// Set at invocation-time (by applyPatch) and immutable thereafter
	namespace                 string
	requestCtx                context.Context
	stripManagedFieldsOnRetry bool
	updatedObjectInfo         rest.UpdatedObjectInfo
	mechanism                 patchMechanism
	jp                        jsonPatcher
	smp                       smpPatcher
	forceAllowCreate          bool
	updateOptionsPtr          *metav1.UpdateOptions
	wasCreated                bool
	runUpdateFn               finisher.ResultFunc
}

var patcherPool = sync.Pool{
	New: func() any {
		p := &patcher{}
		p.runUpdateFn = p.runUpdate
		return p
	},
}

func (p *patcher) runUpdate() (runtime.Object, error) {
	result, created, err := p.restPatcher.Update(p.requestCtx, p.name, p.updatedObjectInfo, p.createValidation, p.updateValidation, p.forceAllowCreate, p.updateOptionsPtr)
	p.wasCreated = created
	if isTooLargeError(err) && p.patchType != types.ApplyYAMLPatchType && p.patchType != types.ApplyCBORPatchType {
		if _, accessorErr := meta.Accessor(p.restPatcher.New()); accessorErr == nil {
			p.stripManagedFieldsOnRetry = true
			result, created, err = p.restPatcher.Update(p.requestCtx, p.name, p.updatedObjectInfo, p.createValidation, p.updateValidation, p.forceAllowCreate, p.updateOptionsPtr)
			p.wasCreated = created
		}
	}
	return result, err
}

func (p *patcher) Preconditions() *metav1.Preconditions {
	return nil
}

func (p *patcher) UpdatedObject(ctx context.Context, oldObj runtime.Object) (runtime.Object, error) {
	newObj, err := p.applyPatch(ctx, nil, oldObj)
	if err != nil {
		return nil, err
	}
	newObj, err = p.applyAdmission(ctx, newObj, oldObj)
	if err != nil {
		return nil, err
	}
	dedupOwnerReferencesAndAddWarning(newObj, p.requestCtx, true)
	if p.stripManagedFieldsOnRetry {
		if accessor, err := meta.Accessor(newObj); err == nil {
			accessor.SetManagedFields(nil)
		}
	}
	return newObj, nil
}

type patchMechanism interface {
	applyPatchToCurrentObject(requextContext context.Context, currentObject runtime.Object) (runtime.Object, error)
	createNewObject(requestContext context.Context) (runtime.Object, error)
}

type jsonPatcher struct {
	*patcher

	fieldManager *managedfields.FieldManager
}

func (p *jsonPatcher) applyPatchToCurrentObject(requestContext context.Context, currentObject runtime.Object) (runtime.Object, error) {
	var savedManagedFields []metav1.ManagedFieldsEntry
	var currentAccessor metav1.Object
	if p.fieldManager != nil && (p.subresource != "" || !bytes.Contains(p.patchBytes, []byte("managedFields"))) {
		if accessor, err := meta.Accessor(currentObject); err == nil {
			if mf := accessor.GetManagedFields(); len(mf) > 0 {
				currentAccessor = accessor
				savedManagedFields = mf
				currentAccessor.SetManagedFields(nil)
				defer func() {
					if currentAccessor != nil {
						currentAccessor.SetManagedFields(savedManagedFields)
					}
				}()
			}
		}
	}

	var savedHub savedHubFields
	if p.patchType == types.MergePatchType {
		savedHub = saveAndZeroUnpatchedHubFields(currentObject, p.patchBytes)
		defer savedHub.restore(nil)
	}

	// Encode will convert & return a versioned object in JSON.
	currentObjJS, err := runtime.Encode(p.codec, currentObject)
	if err != nil {
		return nil, err
	}

	// Apply the patch.
	patchedObjJS, appliedStrictErrs, err := p.applyJSPatch(currentObjJS)
	if err != nil {
		return nil, err
	}

	// Construct the resulting typed, unversioned object.
	objToUpdate := p.restPatcher.New()
	if err := runtime.DecodeInto(p.codec, patchedObjJS, objToUpdate); err != nil {
		strictError, isStrictError := runtime.AsStrictDecodingError(err)
		switch {
		case !isStrictError:
			// disregard any appliedStrictErrs, because it's an incomplete
			// list of strict errors given that we don't know what fields were
			// unknown because DecodeInto failed. Non-strict errors trump in this case.
			return nil, errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), string(patchedObjJS), err.Error()),
			})
		case p.validationDirective == metav1.FieldValidationWarn:
			addStrictDecodingWarnings(requestContext, append(appliedStrictErrs, strictError.Errors()...))
		default:
			strictDecodingError := runtime.NewStrictDecodingError(append(appliedStrictErrs, strictError.Errors()...))
			return nil, errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), string(patchedObjJS), strictDecodingError.Error()),
			})
		}
	} else if len(appliedStrictErrs) > 0 {
		switch {
		case p.validationDirective == metav1.FieldValidationWarn:
			addStrictDecodingWarnings(requestContext, appliedStrictErrs)
		default:
			return nil, errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), string(patchedObjJS), runtime.NewStrictDecodingError(appliedStrictErrs).Error()),
			})
		}
	}

	savedHub.restore(objToUpdate)
	if currentAccessor != nil {
		currentAccessor.SetManagedFields(savedManagedFields)
		currentAccessor = nil
	}

	if p.options == nil {
		// Provide a more informative error for the crash that would
		// happen on the next line
		panic("PatchOptions required but not provided")
	}
	objToUpdate = p.fieldManager.UpdateNoErrors(currentObject, objToUpdate, managerOrUserAgent(p.options.FieldManager, p.userAgent))
	return objToUpdate, nil
}

func (p *jsonPatcher) createNewObject(_ context.Context) (runtime.Object, error) {
	return nil, errors.NewNotFound(p.resource.GroupResource(), p.name)
}

type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	From  string      `json:"from"`
	Value interface{} `json:"value"`
}

// applyJSPatch applies the patch. Input and output objects must both have
// the external version, since that is what the patch must have been constructed against.
func (p *jsonPatcher) applyJSPatch(versionedJS []byte) (patchedJS []byte, strictErrors []error, retErr error) {
	switch p.patchType {
	case types.JSONPatchType:
		if p.validationDirective == metav1.FieldValidationStrict || p.validationDirective == metav1.FieldValidationWarn {
			var v []jsonPatchOp
			var err error
			if strictErrors, err = kjson.UnmarshalStrict(p.patchBytes, &v); err != nil {
				return nil, nil, errors.NewBadRequest(fmt.Sprintf("error decoding patch: %v", err))
			}
			for i, e := range strictErrors {
				strictErrors[i] = fmt.Errorf("json patch %v", e)
			}
		}

		patchObj, err := jsonpatch.DecodePatch(p.patchBytes)
		if err != nil {
			return nil, nil, errors.NewBadRequest(err.Error())
		}
		if len(patchObj) > maxJSONPatchOperations {
			return nil, nil, errors.NewRequestEntityTooLargeError(
				fmt.Sprintf("The allowed maximum operations in a JSON patch is %d, got %d",
					maxJSONPatchOperations, len(patchObj)))
		}
		patchedJS, err := patchObj.Apply(versionedJS)
		if err != nil {
			return nil, nil, errors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", err.Error(), 0, false)
		}
		return patchedJS, strictErrors, nil
	case types.MergePatchType:
		if p.validationDirective == metav1.FieldValidationStrict || p.validationDirective == metav1.FieldValidationWarn {
			v := map[string]any{}
			var err error
			strictErrors, err = kjson.UnmarshalStrict(p.patchBytes, &v)
			if err != nil {
				return nil, nil, errors.NewBadRequest(fmt.Sprintf("error decoding patch: %v", err))
			}
		}

		patchedJS, retErr = jsonpatch.MergePatch(versionedJS, p.patchBytes)
		if retErr == jsonpatch.ErrBadJSONPatch {
			return nil, nil, errors.NewBadRequest(retErr.Error())
		}
		return patchedJS, strictErrors, retErr
	default:
		// only here as a safety net - go-restful filters content-type
		return nil, nil, fmt.Errorf("unknown Content-Type header for patch: %v", p.patchType)
	}
}

type smpPatcher struct {
	*patcher

	// Schema
	schemaReferenceObj runtime.Object
	fieldManager       *managedfields.FieldManager
}

func (p *smpPatcher) applyPatchToCurrentObject(requestContext context.Context, currentObject runtime.Object) (runtime.Object, error) {
	savedHub := saveAndZeroUnpatchedHubFields(currentObject, p.patchBytes)
	defer savedHub.restore(nil)

	// Since the patch is applied on versioned objects, we need to convert the
	// current object to versioned representation first.
	currentVersionedObject, err := p.unsafeConvertor.ConvertToVersion(currentObject, p.kind.GroupVersion())
	if err != nil {
		return nil, err
	}
	versionedObjToUpdate, err := p.creater.New(p.kind)
	if err != nil {
		return nil, err
	}
	var savedManagedFields []metav1.ManagedFieldsEntry
	var currentAccessor metav1.Object
	if p.fieldManager != nil && (p.subresource != "" || !bytes.Contains(p.patchBytes, []byte("managedFields"))) {
		if accessor, err := meta.Accessor(currentVersionedObject); err == nil {
			if mf := accessor.GetManagedFields(); len(mf) > 0 {
				currentAccessor = accessor
				savedManagedFields = mf
				currentAccessor.SetManagedFields(nil)
				defer func() {
					if currentAccessor != nil {
						currentAccessor.SetManagedFields(savedManagedFields)
					}
				}()
			}
		}
	}
	if err := strategicPatchObject(requestContext, p.defaulter, currentVersionedObject, p.patchBytes, versionedObjToUpdate, p.schemaReferenceObj, p.validationDirective); err != nil {
		return nil, err
	}
	if currentAccessor != nil {
		currentAccessor.SetManagedFields(savedManagedFields)
		currentAccessor = nil
	}
	// Convert the object back to the hub version
	newObj, err := p.unsafeConvertor.ConvertToVersion(versionedObjToUpdate, p.hubGroupVersion)
	if err != nil {
		return nil, err
	}
	savedHub.restore(newObj)

	newObj = p.fieldManager.UpdateNoErrors(currentObject, newObj, managerOrUserAgent(p.options.FieldManager, p.userAgent))
	return newObj, nil
}

func (p *smpPatcher) createNewObject(_ context.Context) (runtime.Object, error) {
	return nil, errors.NewNotFound(p.resource.GroupResource(), p.name)
}

func newApplyPatcher(p *patcher, fieldManager *managedfields.FieldManager, unmarshalFn, unmarshalStrictFn func([]byte, interface{}) error) *applyPatcher {
	return &applyPatcher{
		fieldManager:        fieldManager,
		patch:               p.patchBytes,
		options:             p.options,
		creater:             p.creater,
		kind:                p.kind,
		userAgent:           p.userAgent,
		validationDirective: p.validationDirective,
		unmarshalFn:         unmarshalFn,
		unmarshalStrictFn:   unmarshalStrictFn,
	}
}

type applyPatcher struct {
	patch               []byte
	options             *metav1.PatchOptions
	creater             runtime.ObjectCreater
	kind                schema.GroupVersionKind
	fieldManager        *managedfields.FieldManager
	userAgent           string
	validationDirective string
	unmarshalFn         func(data []byte, v any) error
	unmarshalStrictFn   func(data []byte, v any) error
}

func (p *applyPatcher) applyPatchToCurrentObject(requestContext context.Context, obj runtime.Object) (runtime.Object, error) {
	force := false
	if p.options.Force != nil {
		force = *p.options.Force
	}
	if p.fieldManager == nil {
		panic("FieldManager must be installed to run apply")
	}

	patchObj := &unstructured.Unstructured{Object: map[string]any{}}
	if err := p.unmarshalFn(p.patch, &patchObj.Object); err != nil {
		return nil, errors.NewBadRequest(fmt.Sprintf("error decoding patch: %v", err))
	}

	obj, err := p.fieldManager.Apply(obj, patchObj, p.options.FieldManager, force)
	if err != nil {
		return obj, err
	}

	// TODO: spawn something to track deciding whether a fieldValidation=Strict
	// fatal error should return before an error from the apply operation
	if p.validationDirective == metav1.FieldValidationStrict || p.validationDirective == metav1.FieldValidationWarn {
		if err := p.unmarshalStrictFn(p.patch, &map[string]any{}); err != nil {
			if p.validationDirective == metav1.FieldValidationStrict {
				return nil, errors.NewBadRequest(fmt.Sprintf("error strict decoding patch: %v", err))
			}
			addStrictDecodingWarnings(requestContext, []error{err})
		}
	}
	return obj, nil
}

func (p *applyPatcher) createNewObject(requestContext context.Context) (runtime.Object, error) {
	obj, err := p.creater.New(p.kind)
	if err != nil {
		return nil, fmt.Errorf("failed to create new object: %v", err)
	}
	return p.applyPatchToCurrentObject(requestContext, obj)
}

type patchSubFieldInfo struct {
	parentIdx int
	fieldIdx  int
	key       string
	keyBytes  []byte
	pool      sync.Pool
}

type patchStructFieldsInfo struct {
	structType       reflect.Type
	specIdx          int
	specOffset       uintptr
	specSize         uintptr
	specPool         sync.Pool
	zeroSpec         reflect.Value
	statusIdx        int
	statusOffset     uintptr
	statusSize       uintptr
	statusPool       sync.Pool
	statusSubFields  []patchSubFieldInfo
	metaIdx          int
	metaPool         sync.Pool
	metaSubFields    []patchSubFieldInfo
	metaAllSubFields []patchSubFieldInfo
}

type savedSubField struct {
	sub      *patchSubFieldInfo
	curField reflect.Value
	savedVal *reflect.Value
}

type savedHubFields struct {
	info        *patchStructFieldsInfo
	curSpec     reflect.Value
	savedSpec   *reflect.Value
	hasSpec     bool
	curStatus   reflect.Value
	savedStatus *reflect.Value
	hasStatus   bool
	subFields   [24]savedSubField
	numSub      int
}

func saveAndZeroUnpatchedHubFields(currentObject runtime.Object, patchBytes []byte) savedHubFields {
	var s savedHubFields
	if bytes.IndexByte(patchBytes, '\\') >= 0 {
		return s
	}
	trimmed := bytes.TrimLeft(patchBytes, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return s
	}
	objHasUID, err := hasUID(currentObject)
	if err != nil || !objHasUID {
		return s
	}
	rv := reflect.ValueOf(currentObject)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return s
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return s
	}
	info := getPatchStructFieldsInfo(elem.Type())
	s.info = info
	if info.specIdx >= 0 && !bytes.Contains(patchBytes, []byte("spec")) {
		s.curSpec = elem.Field(info.specIdx)
		s.savedSpec = info.specPool.Get().(*reflect.Value)
		s.savedSpec.Set(s.curSpec)
		s.curSpec.Set(info.zeroSpec)
		s.hasSpec = true
	}
	if info.statusIdx >= 0 && !bytes.Contains(patchBytes, []byte("status")) {
		s.curStatus = elem.Field(info.statusIdx)
		s.savedStatus = info.statusPool.Get().(*reflect.Value)
		s.savedStatus.Set(s.curStatus)
		s.curStatus.SetZero()
		s.hasStatus = true
	} else if info.statusIdx >= 0 && len(info.statusSubFields) > 0 {
		statusElem := elem.Field(info.statusIdx)
		for i := range info.statusSubFields {
			if s.numSub >= len(s.subFields) {
				break
			}
			sub := &info.statusSubFields[i]
			f := statusElem.Field(sub.fieldIdx)
			if !f.IsZero() && !bytes.Contains(patchBytes, sub.keyBytes) {
				sv := sub.pool.Get().(*reflect.Value)
				sv.Set(f)
				f.SetZero()
				s.subFields[s.numSub] = savedSubField{sub: sub, curField: f, savedVal: sv}
				s.numSub++
			}
		}
	}
	for i := range info.metaSubFields {
		if s.numSub >= len(s.subFields) {
			break
		}
		sub := &info.metaSubFields[i]
		f := elem.Field(sub.parentIdx).Field(sub.fieldIdx)
		if !f.IsZero() && !bytes.Contains(patchBytes, sub.keyBytes) {
			sv := sub.pool.Get().(*reflect.Value)
			sv.Set(f)
			f.SetZero()
			s.subFields[s.numSub] = savedSubField{sub: sub, curField: f, savedVal: sv}
			s.numSub++
		}
	}
	return s
}

func (s *savedHubFields) restore(newObject runtime.Object) {
	if !s.hasSpec && !s.hasStatus && s.numSub == 0 {
		return
	}
	var newElem reflect.Value
	if newObject != nil {
		if rv := reflect.ValueOf(newObject); rv.Kind() == reflect.Ptr && !rv.IsNil() {
			if e := rv.Elem(); e.Kind() == reflect.Struct && e.Type() == s.info.structType {
				newElem = e
			}
		}
	}
	for i := s.numSub - 1; i >= 0; i-- {
		entry := &s.subFields[i]
		entry.curField.Set(*entry.savedVal)
		if newElem.IsValid() {
			newElem.Field(entry.sub.parentIdx).Field(entry.sub.fieldIdx).Set(*entry.savedVal)
		}
		entry.savedVal.SetZero()
		entry.sub.pool.Put(entry.savedVal)
		s.subFields[i] = savedSubField{}
	}
	s.numSub = 0
	if s.hasStatus {
		s.curStatus.Set(*s.savedStatus)
		if newElem.IsValid() {
			newElem.Field(s.info.statusIdx).Set(*s.savedStatus)
		}
		s.savedStatus.SetZero()
		s.info.statusPool.Put(s.savedStatus)
		s.hasStatus = false
	}
	if s.hasSpec {
		s.curSpec.Set(*s.savedSpec)
		if newElem.IsValid() {
			newElem.Field(s.info.specIdx).Set(*s.savedSpec)
		}
		s.savedSpec.SetZero()
		s.info.specPool.Put(s.savedSpec)
		s.hasSpec = false
	}
}

var patchStructFieldsCache sync.Map // map[reflect.Type]*patchStructFieldsInfo

func getPatchStructFieldsInfo(t reflect.Type) *patchStructFieldsInfo {
	if v, ok := patchStructFieldsCache.Load(t); ok {
		return v.(*patchStructFieldsInfo)
	}
	info := &patchStructFieldsInfo{
		structType: t,
		specIdx:    -1,
		statusIdx:  -1,
		metaIdx:    -1,
	}
	if sf, ok := t.FieldByName("Spec"); ok && !sf.Anonymous && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		info.specIdx = sf.Index[0]
		info.specOffset = sf.Offset
		info.specSize = sf.Type.Size()
		specType := sf.Type
		info.specPool.New = func() any {
			v := reflect.New(specType).Elem()
			return &v
		}
		zeroSpec := reflect.New(specType).Elem()
		if esl := zeroSpec.FieldByName("EnableServiceLinks"); esl.IsValid() && esl.Kind() == reflect.Ptr && esl.Type().Elem().Kind() == reflect.Bool {
			b := reflect.New(esl.Type().Elem())
			b.Elem().SetBool(true)
			esl.Set(b)
		}
		if sc := zeroSpec.FieldByName("SecurityContext"); sc.IsValid() && sc.Kind() == reflect.Ptr && sc.Type().Elem().Kind() == reflect.Struct {
			sc.Set(reflect.New(sc.Type().Elem()))
		}
		if tgp := zeroSpec.FieldByName("TerminationGracePeriodSeconds"); tgp.IsValid() && tgp.Kind() == reflect.Ptr && tgp.Type().Elem().Kind() == reflect.Int64 {
			p := reflect.New(tgp.Type().Elem())
			p.Elem().SetInt(30)
			tgp.Set(p)
		}
		info.zeroSpec = zeroSpec
	}
	if sf, ok := t.FieldByName("Status"); ok && !sf.Anonymous && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		info.statusIdx = sf.Index[0]
		info.statusOffset = sf.Offset
		info.statusSize = sf.Type.Size()
		statusType := sf.Type
		info.statusPool.New = func() any {
			v := reflect.New(statusType).Elem()
			return &v
		}
		for i := 0; i < statusType.NumField(); i++ {
			f := statusType.Field(i)
			if !f.IsExported() || f.Anonymous {
				continue
			}
			k := f.Type.Kind()
			var key, jsonKey string
			switch f.Name {
			case "PodIP":
				key = "podIP"
				jsonKey = "podIP"
			case "PodIPs":
				key = "podIP"
				jsonKey = "podIPs"
			case "HostIP":
				key = "hostIP"
				jsonKey = "hostIP"
			case "HostIPs":
				key = "hostIP"
				jsonKey = "hostIPs"
			case "QOSClass":
				key = "qosClass"
				jsonKey = "qosClass"
			default:
				if k == reflect.Slice || k == reflect.Map || k == reflect.Ptr || k == reflect.String {
					key = strings.ToLower(f.Name[:1]) + f.Name[1:]
					jsonKey = key
				}
			}
			if key != "" {
				ft := f.Type
				sub := patchSubFieldInfo{
					parentIdx: info.statusIdx,
					fieldIdx:  i,
					key:       jsonKey,
					keyBytes:  []byte(key),
				}
				sub.pool.New = func() any {
					v := reflect.New(ft).Elem()
					return &v
				}
				info.statusSubFields = append(info.statusSubFields, sub)
			}
		}
	}
	if sf, ok := t.FieldByName("ObjectMeta"); ok && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		metaIdx := sf.Index[0]
		metaType := sf.Type
		if t.Name() != "ReplicationController" {
			info.metaIdx = metaIdx
			info.metaPool.New = func() any {
				v := reflect.New(metaType).Elem()
				return &v
			}
		}
		metaFields := []struct {
			name string
			key  string
			hub  bool
		}{
			{"Annotations", "annotations", true},
			{"OwnerReferences", "ownerReferences", true},
			{"Finalizers", "finalizers", true},
			{"Name", "name", false},
			{"Namespace", "namespace", false},
			{"UID", "uid", false},
			{"ResourceVersion", "resourceVersion", false},
			{"Generation", "generation", false},
			{"CreationTimestamp", "creationTimestamp", false},
		}
		if t.Name() != "ReplicationController" {
			metaFields = append(metaFields, struct {
				name string
				key  string
				hub  bool
			}{"Labels", "labels", true})
		}
		for _, mf := range metaFields {
			if f, ok := metaType.FieldByName(mf.name); ok && len(f.Index) == 1 {
				ft := f.Type
				sub := patchSubFieldInfo{
					parentIdx: metaIdx,
					fieldIdx:  f.Index[0],
					key:       mf.key,
					keyBytes:  []byte(mf.key),
				}
				sub.pool.New = func() any {
					v := reflect.New(ft).Elem()
					return &v
				}
				if mf.hub {
					info.metaSubFields = append(info.metaSubFields, sub)
				}
				info.metaAllSubFields = append(info.metaAllSubFields, sub)
			}
		}
	}
	actual, _ := patchStructFieldsCache.LoadOrStore(t, info)
	return actual.(*patchStructFieldsInfo)
}

func patchShallowFieldEqual(a, b reflect.Value, offset, size uintptr) bool {
	if size == 0 {
		return false
	}
	ptrA := unsafe.Add(a.Addr().UnsafePointer(), offset)
	ptrB := unsafe.Add(b.Addr().UnsafePointer(), offset)
	return bytes.Equal(unsafe.Slice((*byte)(ptrA), size), unsafe.Slice((*byte)(ptrB), size))
}

type strategicZeroState struct {
	info            *patchStructFieldsInfo
	skipSpec        bool
	skipStatus      bool
	skipMeta        bool
	origSpecField   reflect.Value
	origStatusField reflect.Value
	origMetaField   reflect.Value
	savedSpec       *reflect.Value
	savedStatus     *reflect.Value
	savedMeta       *reflect.Value
	subFields       [24]savedSubField
	numSub          int
	restoredSub     int
}

func (s *strategicZeroState) restore() {
	for i := s.numSub - 1; i >= 0; i-- {
		entry := &s.subFields[i]
		if entry.savedVal != nil {
			entry.curField.Set(*entry.savedVal)
			entry.savedVal.SetZero()
			entry.sub.pool.Put(entry.savedVal)
			entry.savedVal = nil
		}
	}
	s.restoredSub = s.numSub
	s.numSub = 0
	if s.skipMeta {
		s.origMetaField.Set(*s.savedMeta)
		s.savedMeta.SetZero()
		s.info.metaPool.Put(s.savedMeta)
		s.skipMeta = false
	}
	if s.skipStatus {
		s.origStatusField.Set(*s.savedStatus)
		s.savedStatus.SetZero()
		s.info.statusPool.Put(s.savedStatus)
		s.skipStatus = false
	}
	if s.skipSpec {
		s.origSpecField.Set(*s.savedSpec)
		s.savedSpec.SetZero()
		s.info.specPool.Put(s.savedSpec)
		s.skipSpec = false
	}
}

func (s *strategicZeroState) copyRestoredToUpdate(updateElem reflect.Value) {
	if s.origStatusField.IsValid() {
		updateElem.Field(s.info.statusIdx).Set(s.origStatusField)
	}
	if s.origSpecField.IsValid() {
		updateElem.Field(s.info.specIdx).Set(s.origSpecField)
	}
	if s.origMetaField.IsValid() {
		updateElem.Field(s.info.metaIdx).Set(s.origMetaField)
	}
	for i := 0; i < s.restoredSub; i++ {
		entry := &s.subFields[i]
		updateElem.Field(entry.sub.parentIdx).Field(entry.sub.fieldIdx).Set(entry.curField)
	}
}

var (
	patchBoolTrueAny  interface{} = true
	patchBoolFalseAny interface{} = false

	patchKnownValueAnys = map[string]interface{}{
		"":                interface{}(""),
		"True":            interface{}("True"),
		"False":           interface{}("False"),
		"Unknown":         interface{}("Unknown"),
		"Ready":           interface{}("Ready"),
		"ContainersReady": interface{}("ContainersReady"),
		"PodScheduled":    interface{}("PodScheduled"),
		"Initialized":     interface{}("Initialized"),
		"Running":         interface{}("Running"),
		"Pending":         interface{}("Pending"),
		"Succeeded":       interface{}("Succeeded"),
		"Failed":          interface{}("Failed"),
		"Always":          interface{}("Always"),
		"IfNotPresent":    interface{}("IfNotPresent"),
		"Never":           interface{}("Never"),
	}
)

type patchValueCacheEntry struct {
	str string
	val interface{}
}

var patchValueCache [64]struct {
	mu      sync.Mutex
	entries [64]patchValueCacheEntry
}

func fastPatchKeyString(raw []byte) string {
	switch len(raw) {
	case 4:
		switch string(raw) {
		case "type":
			return "type"
		case "spec":
			return "spec"
		case "name":
			return "name"
		}
	case 5:
		switch string(raw) {
		case "phase":
			return "phase"
		case "podIP":
			return "podIP"
		case "image":
			return "image"
		}
	case 6:
		switch string(raw) {
		case "status":
			return "status"
		case "labels":
			return "labels"
		case "reason":
			return "reason"
		case "podIPs":
			return "podIPs"
		case "hostIP":
			return "hostIP"
		}
	case 7:
		switch string(raw) {
		case "message":
			return "message"
		case "hostIPs":
			return "hostIPs"
		}
	case 8:
		switch string(raw) {
		case "metadata":
			return "metadata"
		case "qosClass":
			return "qosClass"
		}
	case 10:
		switch string(raw) {
		case "conditions":
			return "conditions"
		case "containers":
			return "containers"
		case "finalizers":
			return "finalizers"
		}
	case 11:
		if string(raw) == "annotations" {
			return "annotations"
		}
	case 13:
		if string(raw) == "bench-updated" {
			return "bench-updated"
		}
	case 17:
		if string(raw) == "containerStatuses" {
			return "containerStatuses"
		}
	case 18:
		if string(raw) == "lastTransitionTime" {
			return "lastTransitionTime"
		}
	}
	return string(raw)
}

func fastPatchValueAny(raw []byte) interface{} {
	if len(raw) <= 16 {
		if v, ok := patchKnownValueAnys[string(raw)]; ok {
			return v
		}
	}
	return runtime.BytesToUnstructuredStringAny(raw)
}

func skipFastJSONWS(b []byte, pos int) int {
	for pos < len(b) {
		switch b[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
}

func parseFastJSONStringRaw(b []byte, pos int) ([]byte, int, bool) {
	if pos >= len(b) || b[pos] != '"' {
		return nil, 0, false
	}
	start := pos + 1
	for i := start; i < len(b); i++ {
		c := b[i]
		if c == '"' {
			return b[start:i], i + 1, true
		}
		if c < 0x20 || c == '\\' {
			return nil, 0, false
		}
	}
	return nil, 0, false
}

func parseFastJSONValue(b []byte, pos int) (interface{}, int, bool) {
	pos = skipFastJSONWS(b, pos)
	if pos >= len(b) {
		return nil, 0, false
	}
	switch b[pos] {
	case '{':
		return parseFastJSONObject(b, pos)
	case '[':
		pos = skipFastJSONWS(b, pos+1)
		if pos < len(b) && b[pos] == ']' {
			return []interface{}{}, pos + 1, true
		}
		arr := make([]interface{}, 0, 2)
		for {
			elem, nextPos, ok := parseFastJSONValue(b, pos)
			if !ok {
				return nil, 0, false
			}
			arr = append(arr, elem)
			pos = skipFastJSONWS(b, nextPos)
			if pos >= len(b) {
				return nil, 0, false
			}
			if b[pos] == ']' {
				return arr, pos + 1, true
			}
			if b[pos] != ',' {
				return nil, 0, false
			}
			pos = skipFastJSONWS(b, pos+1)
			if pos < len(b) && b[pos] == ']' {
				return nil, 0, false
			}
		}
	case '"':
		raw, nextPos, ok := parseFastJSONStringRaw(b, pos)
		if !ok {
			return nil, 0, false
		}
		return fastPatchValueAny(raw), nextPos, true
	case 't':
		if pos+4 <= len(b) && string(b[pos:pos+4]) == "true" {
			return patchBoolTrueAny, pos + 4, true
		}
	case 'f':
		if pos+5 <= len(b) && string(b[pos:pos+5]) == "false" {
			return patchBoolFalseAny, pos + 5, true
		}
	case 'n':
		if pos+4 <= len(b) && string(b[pos:pos+4]) == "null" {
			return nil, pos + 4, true
		}
	}
	return nil, 0, false
}

func parseFastJSONObject(b []byte, pos int) (map[string]interface{}, int, bool) {
	pos = skipFastJSONWS(b, pos)
	if pos >= len(b) || b[pos] != '{' {
		return nil, 0, false
	}
	pos = skipFastJSONWS(b, pos+1)
	if pos < len(b) && b[pos] == '}' {
		return make(map[string]interface{}), pos + 1, true
	}
	m := make(map[string]interface{}, 2)
	for {
		rawKey, nextPos, ok := parseFastJSONStringRaw(b, pos)
		if !ok {
			return nil, 0, false
		}
		key := fastPatchKeyString(rawKey)
		if _, dup := m[key]; dup {
			return nil, 0, false
		}
		pos = skipFastJSONWS(b, nextPos)
		if pos >= len(b) || b[pos] != ':' {
			return nil, 0, false
		}
		val, valEnd, ok := parseFastJSONValue(b, pos+1)
		if !ok {
			return nil, 0, false
		}
		m[key] = val
		pos = skipFastJSONWS(b, valEnd)
		if pos >= len(b) {
			return nil, 0, false
		}
		if b[pos] == '}' {
			return m, pos + 1, true
		}
		if b[pos] != ',' {
			return nil, 0, false
		}
		pos = skipFastJSONWS(b, pos+1)
		if pos >= len(b) || b[pos] != '"' {
			return nil, 0, false
		}
	}
}

func fastUnmarshalPatchMap(b []byte) (map[string]interface{}, bool) {
	if bytes.IndexByte(b, '\\') >= 0 {
		return nil, false
	}
	m, endPos, ok := parseFastJSONObject(b, 0)
	if !ok {
		return nil, false
	}
	if skipFastJSONWS(b, endPos) != len(b) {
		return nil, false
	}
	return m, true
}

// strategicPatchObject applies a strategic merge patch of `patchBytes` to
// `originalObject` and stores the result in `objToUpdate`.
// It additionally returns the map[string]interface{} representation of the
// `originalObject` and `patchBytes`.
// NOTE: Both `originalObject` and `objToUpdate` are supposed to be versioned.
func strategicPatchObject(
	requestContext context.Context,
	defaulter runtime.ObjectDefaulter,
	originalObject runtime.Object,
	patchBytes []byte,
	objToUpdate runtime.Object,
	schemaReferenceObj runtime.Object,
	validationDirective string,
) error {
	var patchMap map[string]interface{}
	var strictErrs []error
	var err error
	if fm, ok := fastUnmarshalPatchMap(patchBytes); ok {
		patchMap = fm
	} else {
		patchMap = make(map[string]interface{})
		if validationDirective == metav1.FieldValidationWarn || validationDirective == metav1.FieldValidationStrict {
			strictErrs, err = kjson.UnmarshalStrict(patchBytes, &patchMap)
			if err != nil {
				return errors.NewBadRequest(err.Error())
			}
		} else {
			if err = kjson.UnmarshalCaseSensitivePreserveInts(patchBytes, &patchMap); err != nil {
				return errors.NewBadRequest(err.Error())
			}
		}
	}

	var (
		s                    strategicZeroState
		origElem, updateElem reflect.Value
	)
	if objHasUID, uidErr := hasUID(originalObject); uidErr == nil && objHasUID {
		origRV := reflect.ValueOf(originalObject)
		updateRV := reflect.ValueOf(objToUpdate)
		if origRV.Kind() == reflect.Ptr && updateRV.Kind() == reflect.Ptr && origRV.Type() == updateRV.Type() && !origRV.IsNil() && !updateRV.IsNil() {
			origElem = origRV.Elem()
			updateElem = updateRV.Elem()
			if origElem.Kind() == reflect.Struct {
				s.info = getPatchStructFieldsInfo(origElem.Type())
				if _, hasSpec := patchMap["spec"]; !hasSpec && s.info.specIdx >= 0 {
					s.origSpecField = origElem.Field(s.info.specIdx)
					s.savedSpec = s.info.specPool.Get().(*reflect.Value)
					s.savedSpec.Set(s.origSpecField)
					s.origSpecField.SetZero()
					s.skipSpec = true
				}
				if statusVal, hasStatus := patchMap["status"]; !hasStatus && s.info.statusIdx >= 0 {
					s.origStatusField = origElem.Field(s.info.statusIdx)
					s.savedStatus = s.info.statusPool.Get().(*reflect.Value)
					s.savedStatus.Set(s.origStatusField)
					s.origStatusField.SetZero()
					s.skipStatus = true
				} else if statusPatch, ok := statusVal.(map[string]interface{}); ok && s.info.statusIdx >= 0 && len(s.info.statusSubFields) > 0 {
					statusElem := origElem.Field(s.info.statusIdx)
					for i := range s.info.statusSubFields {
						if s.numSub >= len(s.subFields) {
							break
						}
						sub := &s.info.statusSubFields[i]
						f := statusElem.Field(sub.fieldIdx)
						if f.IsZero() {
							continue
						}
						_, hasKey := statusPatch[sub.key]
						if !hasKey && (sub.key == "podIP" || sub.key == "podIPs") {
							_, has1 := statusPatch["podIP"]
							_, has2 := statusPatch["podIPs"]
							hasKey = has1 || has2
						} else if !hasKey && (sub.key == "hostIP" || sub.key == "hostIPs") {
							_, has1 := statusPatch["hostIP"]
							_, has2 := statusPatch["hostIPs"]
							hasKey = has1 || has2
						}
						if !hasKey {
							sv := sub.pool.Get().(*reflect.Value)
							sv.Set(f)
							f.SetZero()
							s.subFields[s.numSub] = savedSubField{sub: sub, curField: f, savedVal: sv}
							s.numSub++
						}
					}
				}
				hasTopDirectives := false
				for k := range patchMap {
					if len(k) > 0 && k[0] == '$' {
						hasTopDirectives = true
						break
					}
				}
				if !hasTopDirectives {
					if metaVal, hasMeta := patchMap["metadata"]; !hasMeta && s.info.metaIdx >= 0 {
						s.origMetaField = origElem.Field(s.info.metaIdx)
						s.savedMeta = s.info.metaPool.Get().(*reflect.Value)
						s.savedMeta.Set(s.origMetaField)
						s.origMetaField.SetZero()
						s.skipMeta = true
					} else {
						metaPatch, _ := metaVal.(map[string]interface{})
						hasMetaDirectives := false
						for k := range metaPatch {
							if len(k) > 0 && k[0] == '$' {
								hasMetaDirectives = true
								break
							}
						}
						metaSubList := s.info.metaSubFields
						if !hasMetaDirectives {
							metaSubList = s.info.metaAllSubFields
						}
						for i := range metaSubList {
							if s.numSub >= len(s.subFields) {
								break
							}
							sub := &metaSubList[i]
							f := origElem.Field(sub.parentIdx).Field(sub.fieldIdx)
							if f.IsZero() {
								continue
							}
							if metaPatch != nil {
								if _, hasKey := metaPatch[sub.key]; hasKey {
									continue
								}
							}
							sv := sub.pool.Get().(*reflect.Value)
							sv.Set(f)
							f.SetZero()
							s.subFields[s.numSub] = savedSubField{sub: sub, curField: f, savedVal: sv}
							s.numSub++
						}
					}
				}
				if s.skipSpec || s.skipStatus || s.skipMeta || s.numSub > 0 {
					defer s.restore()
				}
			}
		}
	}

	originalObjMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(originalObject)
	if s.skipStatus {
		if originalObjMap != nil {
			delete(originalObjMap, "status")
		}
	}
	if s.skipSpec {
		if originalObjMap != nil {
			delete(originalObjMap, "spec")
		}
	}
	if s.skipMeta {
		if originalObjMap != nil {
			delete(originalObjMap, "metadata")
		}
	}
	if s.numSub > 0 && originalObjMap != nil {
		statusMap, _ := originalObjMap["status"].(map[string]interface{})
		metaMap, _ := originalObjMap["metadata"].(map[string]interface{})
		for i := 0; i < s.numSub; i++ {
			sub := s.subFields[i].sub
			if sub.parentIdx == s.info.statusIdx {
				if statusMap != nil {
					delete(statusMap, sub.key)
				}
			} else if metaMap != nil {
				delete(metaMap, sub.key)
			}
		}
	}
	s.restore()
	if err != nil {
		return err
	}

	s.copyRestoredToUpdate(updateElem)

	if err := applyPatchToObject(requestContext, defaulter, originalObjMap, patchMap, objToUpdate, schemaReferenceObj, strictErrs, validationDirective); err != nil {
		return err
	}
	s.copyRestoredToUpdate(updateElem)
	return nil
}

// applyPatch is called every time GuaranteedUpdate asks for the updated object,
// and is given the currently persisted object as input.
// TODO: rename this function because the name implies it is related to applyPatcher
func (p *patcher) applyPatch(ctx context.Context, _, currentObject runtime.Object) (objToUpdate runtime.Object, patchErr error) {
	// Make sure we actually have a persisted currentObject
	tracing.SpanFromContext(ctx).AddEvent("About to apply patch")
	currentObjectHasUID, err := hasUID(currentObject)
	if err != nil {
		return nil, err
	} else if !currentObjectHasUID {
		objToUpdate, patchErr = p.mechanism.createNewObject(ctx)
	} else {
		objToUpdate, patchErr = p.mechanism.applyPatchToCurrentObject(ctx, currentObject)
	}

	if patchErr != nil {
		return nil, patchErr
	}

	objToUpdateHasUID, err := hasUID(objToUpdate)
	if err != nil {
		return nil, err
	}
	if objToUpdateHasUID && !currentObjectHasUID {
		accessor, err := meta.Accessor(objToUpdate)
		if err != nil {
			return nil, err
		}
		return nil, errors.NewConflict(p.resource.GroupResource(), p.name, fmt.Errorf("uid mismatch: the provided object specified uid %s, and no existing object was found", accessor.GetUID()))
	}

	// if this object supports namespace info
	if objectMeta, err := meta.Accessor(objToUpdate); err == nil {
		// ensure namespace on the object is correct, or error if a conflicting namespace was set in the object
		if err := rest.EnsureObjectNamespaceMatchesRequestNamespace(rest.ExpectedNamespaceForResource(p.namespace, p.resource), objectMeta); err != nil {
			return nil, err
		}
	}

	if err := checkName(objToUpdate, p.name, p.namespace, p.namer); err != nil {
		return nil, err
	}
	return objToUpdate, nil
}

func (p *patcher) admissionAttributes(ctx context.Context, updatedObject runtime.Object, currentObject runtime.Object, operation admission.Operation, operationOptions runtime.Object) admission.Attributes {
	userInfo, _ := request.UserFrom(ctx)
	return admission.NewAttributesRecord(updatedObject, currentObject, p.kind, p.namespace, p.name, p.resource, p.subresource, operation, operationOptions, p.dryRun, userInfo)
}

// applyAdmission is called every time GuaranteedUpdate asks for the updated object,
// and is given the currently persisted object and the patched object as input.
// TODO: rename this function because the name implies it is related to applyPatcher
func (p *patcher) applyAdmission(ctx context.Context, patchedObject runtime.Object, currentObject runtime.Object) (runtime.Object, error) {
	tracing.SpanFromContext(ctx).AddEvent("About to check admission control")
	if p.admissionCheck == nil {
		return patchedObject, nil
	}
	var operation admission.Operation
	if hasUID, err := hasUID(currentObject); err != nil {
		return nil, err
	} else if !hasUID {
		operation = admission.Create
		currentObject = nil
	} else {
		operation = admission.Update
	}
	if admission.HasMutationHandler(p.admissionCheck, operation) {
		var options runtime.Object
		if operation == admission.Create {
			options = patchToCreateOptions(p.options)
		} else {
			options = patchToUpdateOptions(p.options)
		}
		if currentObject != nil {
			patchedRV := reflect.ValueOf(patchedObject)
			currentRV := reflect.ValueOf(currentObject)
			if patchedRV.Kind() == reflect.Ptr && currentRV.Kind() == reflect.Ptr && patchedRV.Type() == currentRV.Type() && !patchedRV.IsNil() && !currentRV.IsNil() {
				patchedElem := patchedRV.Elem()
				currentElem := currentRV.Elem()
				if patchedElem.Kind() == reflect.Struct {
					info := getPatchStructFieldsInfo(patchedElem.Type())
					if (info.specIdx >= 0 && patchShallowFieldEqual(patchedElem, currentElem, info.specOffset, info.specSize)) ||
						(info.statusIdx >= 0 && patchShallowFieldEqual(patchedElem, currentElem, info.statusOffset, info.statusSize)) {
						patchedObject = patchedObject.DeepCopyObject()
					}
				}
			}
		}
		attributes := p.admissionAttributes(ctx, patchedObject, currentObject, operation, options)
		return patchedObject, p.admissionCheck.Admit(ctx, attributes, p.objectInterfaces)
	}
	return patchedObject, nil
}

var (
	schemaReferenceObjMu    sync.RWMutex
	schemaReferenceObjCache = make(map[schema.GroupVersionKind]runtime.Object, 16)
)

// patchResource divides PatchResource for easier unit testing
func (p *patcher) patchResource(ctx context.Context, scope *RequestScope) (runtime.Object, bool, error) {
	p.namespace = request.NamespaceValue(ctx)
	p.requestCtx = ctx
	switch p.patchType {
	case types.JSONPatchType, types.MergePatchType:
		p.jp = jsonPatcher{
			patcher:      p,
			fieldManager: scope.FieldManager,
		}
		p.mechanism = &p.jp
	case types.StrategicMergePatchType:
		schemaReferenceObjMu.RLock()
		schemaReferenceObj, ok := schemaReferenceObjCache[p.kind]
		schemaReferenceObjMu.RUnlock()
		if !ok {
			var err error
			schemaReferenceObj, err = p.unsafeConvertor.ConvertToVersion(p.restPatcher.New(), p.kind.GroupVersion())
			if err != nil {
				return nil, false, err
			}
			schemaReferenceObjMu.Lock()
			schemaReferenceObjCache[p.kind] = schemaReferenceObj
			schemaReferenceObjMu.Unlock()
		}
		p.smp = smpPatcher{
			patcher:            p,
			schemaReferenceObj: schemaReferenceObj,
			fieldManager:       scope.FieldManager,
		}
		p.mechanism = &p.smp
	// this case is unreachable if ServerSideApply is not enabled because we will have already rejected the content type
	case types.ApplyYAMLPatchType:
		p.mechanism = newApplyPatcher(p, scope.FieldManager, yaml.Unmarshal, yaml.UnmarshalStrict)
		p.forceAllowCreate = true
	case types.ApplyCBORPatchType:
		if !utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			utilruntime.HandleErrorWithContext(context.TODO(), nil, "CBOR apply requests should be rejected before reaching this point unless the feature gate is enabled.")
			return nil, false, fmt.Errorf("%v: unimplemented patch type", p.patchType)
		}

		// The strict and non-strict funcs are the same here because any CBOR map with
		// duplicate keys is invalid and always rejected outright regardless of strictness
		// mode, and unknown field errors can't occur in practice because the type of the
		// destination value for unmarshaling an apply configuration is always
		// "unstructured".
		p.mechanism = newApplyPatcher(p, scope.FieldManager, cbor.Unmarshal, cbor.Unmarshal)
		p.forceAllowCreate = true
	default:
		return nil, false, fmt.Errorf("%v: unimplemented patch type", p.patchType)
	}

	p.wasCreated = false
	p.updatedObjectInfo = p
	p.updateOptionsPtr = nil
	if p.options != nil {
		p.updateOptionsVal = metav1.UpdateOptions{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "meta.k8s.io/v1",
				Kind:       "UpdateOptions",
			},
			DryRun:          p.options.DryRun,
			FieldManager:    p.options.FieldManager,
			FieldValidation: p.options.FieldValidation,
		}
		p.updateOptionsPtr = &p.updateOptionsVal
	}
	if p.runUpdateFn == nil {
		p.runUpdateFn = p.runUpdate
	}
	result, err := finisher.FinishRequest(ctx, p.runUpdateFn)

	// In case of a timeout error, the goroutine handling the request is still running.
	// https://github.com/kubernetes/kubernetes/blob/d2c12afa4593e50a187075157d38748292b02733/staging/src/k8s.io/apiserver/pkg/endpoints/handlers/finisher/finisher.go#L127-L146
	// We cannot reliably read the variable (data race!) and have to assume that
	// the object was not created.
	if errors.IsTimeout(err) {
		return result, false, err
	}
	return result, p.wasCreated, err
}

// applyPatchToObject applies a strategic merge patch of <patchMap> to
// <originalMap> and stores the result in <objToUpdate>.
// NOTE: <objToUpdate> must be a versioned object.
func applyPatchToObject(
	requestContext context.Context,
	defaulter runtime.ObjectDefaulter,
	originalMap map[string]interface{},
	patchMap map[string]interface{},
	objToUpdate runtime.Object,
	schemaReferenceObj runtime.Object,
	strictErrs []error,
	validationDirective string,
) error {
	patchedObjMap, err := strategicpatch.StrategicMergeMapPatch(originalMap, patchMap, schemaReferenceObj)
	if err != nil {
		return interpretStrategicMergePatchError(err)
	}

	// Rather than serialize the patched map to JSON, then decode it to an object, we go directly from a map to an object
	converter := runtime.DefaultUnstructuredConverter
	returnUnknownFields := validationDirective == metav1.FieldValidationWarn || validationDirective == metav1.FieldValidationStrict
	if err := converter.FromUnstructuredWithValidation(patchedObjMap, objToUpdate, returnUnknownFields); err != nil {
		strictError, isStrictError := runtime.AsStrictDecodingError(err)
		switch {
		case !isStrictError:
			// disregard any sttrictErrs, because it's an incomplete
			// list of strict errors given that we don't know what fields were
			// unknown because StrategicMergeMapPatch failed.
			// Non-strict errors trump in this case.
			return errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), fmt.Sprintf("%+v", patchMap), err.Error()),
			})
		case validationDirective == metav1.FieldValidationWarn:
			addStrictDecodingWarnings(requestContext, append(strictErrs, strictError.Errors()...))
		default:
			strictDecodingError := runtime.NewStrictDecodingError(append(strictErrs, strictError.Errors()...))
			return errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), fmt.Sprintf("%+v", patchMap), strictDecodingError.Error()),
			})
		}
	} else if len(strictErrs) > 0 {
		switch {
		case validationDirective == metav1.FieldValidationWarn:
			addStrictDecodingWarnings(requestContext, strictErrs)
		default:
			return errors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{
				field.Invalid(field.NewPath("patch"), fmt.Sprintf("%+v", patchMap), runtime.NewStrictDecodingError(strictErrs).Error()),
			})
		}
	}

	// Decoding from JSON to a versioned object would apply defaults, so we do the same here
	defaulter.Default(objToUpdate)

	return nil
}

// interpretStrategicMergePatchError interprets the error type and returns an error with appropriate HTTP code.
func interpretStrategicMergePatchError(err error) error {
	switch err {
	case mergepatch.ErrBadJSONDoc, mergepatch.ErrBadPatchFormatForPrimitiveList, mergepatch.ErrBadPatchFormatForRetainKeys, mergepatch.ErrBadPatchFormatForSetElementOrderList, mergepatch.ErrUnsupportedStrategicMergePatchFormat:
		return errors.NewBadRequest(err.Error())
	case mergepatch.ErrNoListOfLists, mergepatch.ErrPatchContentNotMatchRetainKeys:
		return errors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", err.Error(), 0, false)
	default:
		return err
	}
}

// patchToUpdateOptions creates an UpdateOptions with the same field values as the provided PatchOptions.
func patchToUpdateOptions(po *metav1.PatchOptions) *metav1.UpdateOptions {
	if po == nil {
		return nil
	}
	uo := &metav1.UpdateOptions{
		DryRun:          po.DryRun,
		FieldManager:    po.FieldManager,
		FieldValidation: po.FieldValidation,
	}
	uo.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("UpdateOptions"))
	return uo
}

// patchToCreateOptions creates an CreateOptions with the same field values as the provided PatchOptions.
func patchToCreateOptions(po *metav1.PatchOptions) *metav1.CreateOptions {
	if po == nil {
		return nil
	}
	co := &metav1.CreateOptions{
		DryRun:          po.DryRun,
		FieldManager:    po.FieldManager,
		FieldValidation: po.FieldValidation,
	}
	co.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("CreateOptions"))
	return co
}

func validateAndTranscodePatch(patchBytes []byte, patchType types.PatchType) ([]byte, error) {
	switch patchType {
	case types.ApplyCBORPatchType:
		var obj any
		if err := cbor.Unmarshal(patchBytes, &obj); err != nil {
			return nil, errors.NewBadRequest(fmt.Sprintf("error decoding patch: %v", err))
		}
		jsonBytes, err := json.Marshal(obj)
		if err != nil {
			return nil, errors.NewBadRequest(fmt.Sprintf("error encoding patch: %v", err))
		}
		return jsonBytes, nil
	case types.ApplyYAMLPatchType:
		jsonBytes, err := yaml.ToJSON(patchBytes)
		if err != nil {
			return nil, errors.NewBadRequest(fmt.Sprintf("error encoding patch: %v", err))
		}
		return jsonBytes, nil
	default:
		if !json.Valid(patchBytes) {
			return nil, errors.NewBadRequest("invalid JSON patch")
		}
		return patchBytes, nil
	}
}
